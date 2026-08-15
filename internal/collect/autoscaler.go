// Package collect reads a cluster into an engine.Snapshot.
//
// It is the only package that talks to a Kubernetes API server, and it lists
// objects rather than transforming them: the engine works on API types
// directly, so there is almost nothing here to get wrong. The exception is the
// cluster-autoscaler's status, which is a YAML document embedded in a
// ConfigMap and has to be parsed.
package collect

import (
	"fmt"
	"time"

	"sigs.k8s.io/yaml"

	"github.com/motleyhand/binpack/internal/engine"
)

// StatusConfigMap is where the cluster-autoscaler publishes what it is doing.
//
// This object is the reason binpack needs no cloud credentials. It is present
// and populated even on a managed control plane whose autoscaler pods and logs
// are invisible, and it reports which pools autoscale, their bounds, and when
// the cluster last grew — everything binpack would otherwise have to ask a
// cloud API for. See ADR-0004.
const (
	StatusConfigMapNamespace = "kube-system"
	StatusConfigMapName      = "cluster-autoscaler-status"
	statusKey                = "status"
)

// status mirrors the parts of the autoscaler's status document binpack reads.
// Unknown fields are ignored: this is somebody else's schema, and it grows.
type status struct {
	AutoscalerStatus string `json:"autoscalerStatus"`

	ClusterWide struct {
		ScaleUp   transition `json:"scaleUp"`
		ScaleDown transition `json:"scaleDown"`
	} `json:"clusterWide"`

	NodeGroups []struct {
		Name   string `json:"name"`
		Health struct {
			MinSize    int `json:"minSize"`
			MaxSize    int `json:"maxSize"`
			NodeCounts struct {
				Registered struct {
					Ready int `json:"ready"`
				} `json:"registered"`
			} `json:"nodeCounts"`
		} `json:"health"`
	} `json:"nodeGroups"`
}

type transition struct {
	Status             string     `json:"status"`
	LastTransitionTime *time.Time `json:"lastTransitionTime"`
}

// ParseAutoscalerStatus turns the status document into what the engine needs.
//
// A pool absent from nodeGroups is one the autoscaler does not manage, which
// is how binpack learns not to touch it — no configuration required, and no
// way for that configuration to drift.
func ParseAutoscalerStatus(document string) (engine.Autoscaler, error) {
	var s status
	if err := yaml.Unmarshal([]byte(document), &s); err != nil {
		return engine.Autoscaler{}, fmt.Errorf("parsing the cluster-autoscaler status: %w", err)
	}

	out := engine.Autoscaler{
		// Anything other than Running means the autoscaler is not in a state
		// to reap a drained node, so binpack should not create one.
		Running:         s.AutoscalerStatus == "Running",
		ScaleDownStatus: s.ClusterWide.ScaleDown.Status,
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
		out.Groups = append(out.Groups, engine.NodeGroup{
			ID:      g.Name,
			MinSize: g.Health.MinSize,
			MaxSize: g.Health.MaxSize,
			Ready:   g.Health.NodeCounts.Registered.Ready,
		})
	}

	return out, nil
}
