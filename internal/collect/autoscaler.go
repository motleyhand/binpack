// Package collect reads a cluster into an engine.Snapshot.
//
// It is the only package that talks to a Kubernetes API server, and it lists
// objects rather than transforming them: the engine works on API types
// directly, so there is almost nothing here to get wrong. The exception is the
// cluster-autoscaler's status, which is a YAML document embedded in a
// ConfigMap and has to be parsed.
package collect

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/motleyhand/binpack/internal/engine"
)

// StatusConfigMapName is what the cluster-autoscaler calls the object it
// publishes its status into, unless it was told otherwise.
//
// This object is the reason binpack needs no cloud credentials. It is present
// and populated even on a managed control plane whose autoscaler pods and logs
// are invisible, and it reports which pools autoscale, their bounds, and when
// the cluster last grew — everything binpack would otherwise have to ask a
// cloud API for. See ADR-0004.
//
// Neither half of where it lives is fixed. The autoscaler writes into the
// namespace it was given with --namespace — which the upstream chart sets to
// whatever namespace you install it into, so kube-system is a common answer
// and not the only one — under the name it was given with
// --status-config-map-name. Both are configuration here for the same reason:
// binpack reporting "no cluster-autoscaler is running" because it looked
// somewhere else is a claim about the operator's cluster that it has not
// established. See discovery.autoscalerNamespace and
// discovery.autoscalerStatusName.
//
// binpack reads the one location it is told to read and no other: a cluster
// can hold a stale status ConfigMap alongside the live one, and a search
// across namespaces would have to pick between two documents it cannot tell
// apart.
const (
	StatusConfigMapName = "cluster-autoscaler-status"
	statusKey           = "status"
)

// StatusRef is where to look for that object.
//
// A type rather than two string parameters. They are adjacent, both strings,
// and a caller that swapped them would compile, run, find nothing and report a
// cluster with no autoscaler — which is precisely the confident, wrong answer
// making the location configurable exists to end.
type StatusRef struct {
	Namespace string
	Name      string
}

func (r StatusRef) String() string { return r.Namespace + "/" + r.Name }

// legacyPrefix is what cluster-autoscaler 1.29 and earlier open their status
// with. Before 1.30 the status was ClusterAutoscalerStatus.GetReadableString()
// wrapped in a header line — indented free text whose continuation lines carry
// colons, so it is not YAML and the parser fails on line 4 of a document the
// operator did not write, naming neither the component nor the version.
//
// Checked ahead of the unmarshal rather than after it, because one shape of
// that document does parse: a 1.29 autoscaler with no conditions yet yields a
// zero struct, which reads downstream as "no autoscaler is running" — the same
// wrong answer, arrived at silently.
const legacyPrefix = "Cluster-autoscaler status at "

// ErrPre130Status is returned for a status document in the pre-1.30 format.
//
// binpack's whole no-cloud-credentials design rests on the structured status
// document, which arrived in cluster-autoscaler 1.30 (ADR-0004). Below that
// version binpack cannot work at all, and saying so is the only useful thing
// it can do.
var ErrPre130Status = errors.New(
	"the cluster-autoscaler is publishing the free-text status format used before " +
		"cluster-autoscaler 1.30; binpack reads the structured status document that " +
		"version introduced, and cannot work with an older autoscaler")

// status mirrors the parts of the autoscaler's status document binpack reads.
// Unknown fields are ignored: this is somebody else's schema, and it grows.
type status struct {
	AutoscalerStatus string `json:"autoscalerStatus"`

	// Time is the document's own publication time, written in Go's default
	// time format rather than RFC3339. A fallback for freshness when the
	// health probe time is absent.
	Time string `json:"time"`

	ClusterWide struct {
		Health    transition `json:"health"`
		ScaleUp   transition `json:"scaleUp"`
		ScaleDown transition `json:"scaleDown"`
	} `json:"clusterWide"`

	NodeGroups []struct {
		Name   string `json:"name"`
		Health struct {
			MinSize int `json:"minSize"`
			MaxSize int `json:"maxSize"`
			// A pointer so a reported zero is distinguishable from an absent
			// field: zero is a legitimate target for a pool removing its last
			// node.
			CloudProviderTarget *int `json:"cloudProviderTarget"`
			NodeCounts          struct {
				Registered struct {
					Ready int `json:"ready"`
				} `json:"registered"`
			} `json:"nodeCounts"`
		} `json:"health"`
	} `json:"nodeGroups"`
}

type transition struct {
	Status             string     `json:"status"`
	LastProbeTime      *time.Time `json:"lastProbeTime"`
	LastTransitionTime *time.Time `json:"lastTransitionTime"`
}

// ParseAutoscalerStatus turns the status document into what the engine needs.
//
// A pool absent from nodeGroups is one the autoscaler does not manage, which
// is how binpack learns not to touch it — no configuration required, and no
// way for that configuration to drift.
func ParseAutoscalerStatus(document string) (engine.Autoscaler, error) {
	if strings.HasPrefix(document, legacyPrefix) {
		return engine.Autoscaler{}, ErrPre130Status
	}

	var s status
	if err := yaml.Unmarshal([]byte(document), &s); err != nil {
		return engine.Autoscaler{}, fmt.Errorf("parsing the cluster-autoscaler status: %w", err)
	}

	out := engine.Autoscaler{
		// Anything other than Running means the autoscaler is not in a state
		// to reap a drained node, so binpack should not create one.
		Running: s.AutoscalerStatus == "Running",
		// Kept alongside the verdict, because the refusal has to be able to
		// say what was read. An autoscaler reporting Initializing and one that
		// is not there are different facts about the operator's cluster, and
		// binpack asserted the second about both.
		StatusFound:     true,
		ObservedStatus:  s.AutoscalerStatus,
		HealthStatus:    s.ClusterWide.Health.Status,
		ScaleDownStatus: s.ClusterWide.ScaleDown.Status,
	}

	// The autoscaler updates lastProbeTime on every scan. Without it a
	// ConfigMap left behind by a dead autoscaler would report Running
	// forever, and binpack would cheerfully drain nodes nothing will reap —
	// defeating the one check that exists to prevent exactly that.
	//
	// Compared against the clock in the engine, not here: parsing stays pure.
	if t := s.ClusterWide.Health.LastProbeTime; t != nil {
		out.LastProbe = *t
	} else if published, ok := parseStatusTime(s.Time); ok {
		// The document's own timestamp, for the one window where the health
		// probe time is genuinely absent: a process that has published a
		// status but not yet completed a scan, which reports Initializing and
		// is refused above on that account anyway. It is not, as this comment
		// once said, a concession to older autoscalers — every release that
		// publishes the structured document sets the health probe time on
		// every scan, and the ones that do not are refused outright as
		// pre-1.30. Kept because a freshness reading binpack has is better
		// than one it invents, not because anything depends on it.
		out.LastProbe = published
	}

	// The autoscaler records when it last changed scale-up state. Using it
	// rather than inferring from node creation timestamps means the cooldown
	// needs no persistence and cannot disagree with the autoscaler's own view.
	//
	// A scale-up still in progress is reported as such rather than being
	// turned into a timestamp here: parsing must not consult the clock, or
	// the same document would parse differently on each read.
	out.ScaleUpInProgress = s.ClusterWide.ScaleUp.Status == "InProgress"
	if t := s.ClusterWide.ScaleUp.LastTransitionTime; t != nil {
		out.LastScaleUp = *t
	}

	for _, g := range s.NodeGroups {
		// A group with no name has no identifier, and the identifier is what
		// a node's join label is matched against. Since a node in no pool at
		// all resolves to the empty string too, an unnamed group is one that
		// claims every static node in the cluster — turning off the check
		// that says "nothing will ever remove this node" rather than failing
		// it. `name` is omitempty upstream, so the value is representable.
		//
		// Dropped here rather than guarded at each consumer, because here
		// there is one of them.
		if g.Name == "" {
			continue
		}
		group := engine.NodeGroup{
			ID:      g.Name,
			MinSize: g.Health.MinSize,
			MaxSize: g.Health.MaxSize,
			Ready:   g.Health.NodeCounts.Registered.Ready,
		}
		// What the autoscaler has asked the provider for: its intent, rather
		// than the cluster's lagging reality.
		if g.Health.CloudProviderTarget != nil {
			group.Target, group.HasTarget = *g.Health.CloudProviderTarget, true
		}
		out.Groups = append(out.Groups, group)
	}

	return out, nil
}

// statusTimeLayout is Go's default time rendering, which is what the
// autoscaler writes for the document's top-level time field — not RFC3339,
// unlike every other timestamp in the same document.
const statusTimeLayout = "2006-01-02 15:04:05.999999999 -0700 MST"

func parseStatusTime(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(statusTimeLayout, value)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
