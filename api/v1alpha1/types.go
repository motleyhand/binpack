// Package v1alpha1 defines binpack's configuration format.
//
// The shape is deliberately that of a Kubernetes API type — an API group, a
// version, a kind, JSON tags, defaulting and validation — even though it is
// currently loaded from a ConfigMap rather than served by the API server.
// Promoting it to a CustomResourceDefinition later is then additive rather
// than a breaking migration for anyone who has written a config file. See
// ADR-0002.
//
// Two representations live here. Config is the wire format: everything is
// optional, and pointer fields distinguish "unset" from "explicitly false".
// PoolPolicy is what the rest of binpack consumes: every field has a value,
// with defaults and per-pool overrides already resolved. Keeping optionality
// confined to this package means no other package has to reason about nil.
package v1alpha1

import "time"

// GroupVersion and Kind identify the configuration document.
const (
	GroupVersion = "binpack.motleyhand.com/v1alpha1"
	Kind         = "BinpackConfig"
)

// Config is the on-disk configuration document.
type Config struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`

	// Interval between evaluations.
	Interval *Duration `json:"interval,omitempty"`

	// DryRun decides everything and acts on nothing. Defaults to true:
	// a tool that drains nodes must be opted into acting.
	DryRun *bool `json:"dryRun,omitempty"`

	// Discovery locates node pools and their bounds. binpack reads these
	// from the cluster-autoscaler's own status rather than a cloud API, so
	// only the label keys that map nodes to pools are configurable.
	Discovery Discovery `json:"discovery"`

	// Policy applies to every discovered pool that has no override.
	Policy Policy `json:"policy"`

	// Pools overrides Policy for individual discovered pools. Pools are
	// never declared here — they are discovered — so an entry naming a pool
	// that does not exist is a configuration error rather than a definition.
	Pools []PoolOverride `json:"pools,omitempty"`
}

// Discovery configures how nodes are mapped to autoscaler node groups, and
// where to find the object that publishes them.
//
// That object — the cluster-autoscaler's status ConfigMap — is the reason
// binpack needs no cloud credentials. It is present and populated even on a
// managed control plane whose autoscaler pods and logs are invisible, and it
// reports which pools autoscale, their bounds, and when the cluster last grew:
// everything binpack would otherwise have to ask a cloud API for. See
// ADR-0004.
type Discovery struct {
	// NodeGroupIDLabel holds, on each node, the identifier the
	// cluster-autoscaler uses for that node's group. This is what makes
	// credential-free discovery of pool bounds possible; on DOKS the values
	// are pool UUIDs matching the status ConfigMap exactly.
	NodeGroupIDLabel string `json:"nodeGroupIDLabel,omitempty"`

	// PoolNameLabel holds a human-readable pool name, used only so that
	// Pools entries can be written as "pool-4g" rather than a UUID.
	PoolNameLabel string `json:"poolNameLabel,omitempty"`

	// AutoscalerNamespace is where the cluster-autoscaler publishes its
	// status ConfigMap, which is the namespace it was started with
	// --namespace and therefore the one it runs in. The upstream Helm chart
	// sets that flag to whatever namespace you install it into, so
	// kube-system is the common answer rather than the only one.
	//
	// binpack reads this namespace and no other. Searching for the name
	// across the cluster was the alternative and is worse: a status
	// ConfigMap outlives the autoscaler that wrote it, so a cluster can hold
	// a stale one beside the live one, and nothing about either says which
	// is which.
	//
	// Whatever is set here must also be the namespace binpack's Role for
	// reading that ConfigMap is bound in — see docs/reference/rbac.md.
	AutoscalerNamespace string `json:"autoscalerNamespace,omitempty"`

	// AutoscalerStatusName is what that ConfigMap is called, which is the
	// value the autoscaler was started with --status-config-map-name and
	// defaults to cluster-autoscaler-status.
	//
	// A sibling of AutoscalerNamespace rather than a field nested under it,
	// because upstream keeps them apart: --namespace is the namespace the
	// autoscaler runs in, and --status-config-map-name names one object
	// inside it. Nesting the two would imply a single setting that upstream
	// can move as a unit, and it cannot.
	//
	// Configurable for the same reason the namespace is. binpack looking in
	// one place, finding nothing and reporting "no cluster-autoscaler is
	// running" is a claim about the operator's cluster it has not established
	// — and on a cluster that renamed the object, a false one.
	//
	// DefaultAutoscalerStatusName is the one spelling of that default in Go,
	// and binpack's tests build their fixtures from it rather than from a
	// constant of their own. A second Go copy is self-consistent with whatever
	// the tests assert and silent about what binpack ships, so changing this
	// value would leave an unconfigured binpack reading an object that is not
	// there while the suite stayed green — exactly the false claim the
	// paragraph above says the configurability exists to avoid.
	//
	// In Go, and no further. The chart hard-codes the literal in
	// templates/NOTES.txt, where it renders the *namespace* half through the
	// binpack.autoscalerNamespace helper and the name half not at all — so an
	// operator who sets this field is told after install that binpack reads an
	// object it will not read. Nothing in this package can hold that, and the
	// claim above should not be read as covering it.
	AutoscalerStatusName string `json:"autoscalerStatusName,omitempty"`

	// NodeGroups states outright which node group a NodeGroupIDLabel value
	// belongs to, for the clusters where neither join binpack can make works:
	// the identifier is not a legal label value, and it was not generated
	// from anything a label carries. A self-managed Auto Scaling group named
	// by hand is the case, and so is a GKE node pool whose name the instance
	// group truncated.
	//
	// Membership only. Pool bounds still come from the status ConfigMap,
	// because a minimum stated here and enforced by the autoscaler is two
	// numbers that can disagree — and the one binpack would act on is the one
	// nobody updates when a pool is resized in a console. That is what
	// ADR-0004's withdrawn second resolution mode got wrong, and this is the
	// half of it that is safe.
	//
	// Additive: a value nothing here names still joins by equality, so
	// stating one pool never takes the others out of scope.
	NodeGroups []NodeGroupJoin `json:"nodeGroups,omitempty"`
}

// NodeGroupJoin states that nodes carrying one value of
// Discovery.NodeGroupIDLabel are in one autoscaler node group.
type NodeGroupJoin struct {
	// LabelValue is what the nodes carry under NodeGroupIDLabel.
	LabelValue string `json:"labelValue"`

	// Group is the identifier the cluster-autoscaler publishes for that pool
	// — `nodeGroups[].name` in the status ConfigMap, which is the cloud
	// provider's own identifier and not in general the pool name anyone typed
	// into a console.
	Group string `json:"group"`
}

// Policy is the set of tunables that can be set globally or per pool.
// Every field is optional; unset fields inherit.
type Policy struct {
	// Enabled false leaves a pool's nodes alone entirely. Useful for
	// rolling binpack out to one pool at a time.
	Enabled *bool `json:"enabled,omitempty"`

	Feasibility Feasibility `json:"feasibility"`
	Autoscaler  Autoscaler  `json:"autoscaler"`
	Drain       Drain       `json:"drain"`
	Backoff     Backoff     `json:"backoff"`
	Cooldown    Cooldown    `json:"cooldown"`
	Exclusions  Exclusions  `json:"exclusions"`
}

// PoolOverride adjusts Policy for one discovered pool.
type PoolOverride struct {
	// Name matches the value of Discovery.PoolNameLabel on a node, or the
	// node group identifier itself.
	Name string `json:"name"`

	Policy `json:",inline"`
}

// Feasibility governs which pods must be shown to fit elsewhere.
type Feasibility struct {
	// ExpendablePriorityCutoff mirrors the cluster-autoscaler's
	// --expendable-pods-priority-cutoff. Pods strictly below it are excluded
	// from the placement simulation, because the autoscaler will terminate a
	// node running them without ceremony.
	//
	// Raising this above 0 would treat ordinary workloads as expendable, so
	// validation rejects it. Note that overprovisioning pause pods sit at or
	// above the cutoff by necessity and are therefore NOT expendable: see
	// docs/explanation/overprovisioning-and-expendable-pods.md.
	ExpendablePriorityCutoff *int32 `json:"expendablePriorityCutoff,omitempty"`

	// ReserveForLargestPod requires that, after a drain, some schedulable
	// node still has room for a pod of every maximal shape among the
	// relocatable pods running in the cluster.
	//
	// Not "the largest pod": across more than one resource a cluster does not
	// have one, only maximal ones. A 7-core pod and a 24Gi pod are each the
	// largest in their own dimension, and room for either says nothing about
	// the other, so the check asks about every shape no other relocatable pod
	// is at least as large as in every resource.
	//
	// This replaces a percentage of headroom deliberately. A percentage is
	// blind to absolute capacity — the same objection binpack raises against
	// the descheduler — and the risk being guarded against is not "the
	// cluster is full" but "the next pod that restarts cannot be placed",
	// which is a question about bytes.
	ReserveForLargestPod *bool `json:"reserveForLargestPod,omitempty"`
}

// Autoscaler states what the cluster-autoscaler in this cluster is configured
// to do, where that differs from upstream's defaults.
//
// Not settings for binpack: nothing here changes what binpack is willing to
// do, only what it predicts the autoscaler will do once a node is empty.
// Getting one wrong is not unsafe, it is wrong in one of two directions —
// too strict and binpack refuses nodes the autoscaler would have removed, too
// loose and it drains a node the autoscaler then declines to remove, which the
// drain verification catches and backs off from.
//
// These are per-pool for the same reason
// [Feasibility.ExpendablePriorityCutoff] is, which mirrors a process-global
// flag too: the resolved policy is the unit every other setting travels in,
// and one section that could not be overridden would be a shape nothing else
// here has.
//
// Nested under policy rather than discovery because discovery is about finding
// the autoscaler's status document, and this is about the flags it was started
// with — which that document does not report, and which nothing binpack can
// read will tell it.
type Autoscaler struct {
	// SkipNodesWithLocalStorage mirrors --skip-nodes-with-local-storage,
	// default true. Under it a pod with a hostPath or disk-backed emptyDir
	// volume blocks the node's removal.
	//
	// Worth setting where the platform ships something else: AKS's
	// cluster-autoscaler profile exposes this flag and defaults it to false.
	SkipNodesWithLocalStorage *bool `json:"skipNodesWithLocalStorage,omitempty"`

	// SkipNodesWithSystemPods mirrors --skip-nodes-with-system-pods,
	// default true. Under it a kube-system pod with no PodDisruptionBudget
	// blocks the node's removal — for BlockingSystemPodDistruptionTimeout
	// after that pod was created.
	SkipNodesWithSystemPods *bool `json:"skipNodesWithSystemPods,omitempty"`

	// BlockingSystemPodDistruptionTimeout mirrors
	// --blocking-system-pod-distruption-timeout, default one hour. The
	// misspelling is upstream's, kept so the two names can be grepped
	// together; correcting it here would hide the next divergence between
	// them behind a difference that is only spelling.
	//
	// It has meaning only while SkipNodesWithSystemPods is true, and it is
	// the one field here whose default is not true of every autoscaler
	// binpack supports: the grace arrived in cluster-autoscaler 1.33, and
	// 1.30 to 1.32 block on such a pod for as long as it is there. Zero says
	// exactly that — the blocker never expires — which is deliberately *not*
	// what zero means upstream, where it would expire the blocker
	// immediately. Upstream needs no such value: expiring immediately is
	// already spelled skipNodesWithSystemPods: false, in both vocabularies,
	// while "no grace at all" is a supported autoscaler that the flag cannot
	// describe. Reading zero upstream's way would also make an unset field
	// switch the whole rule off, which is the direction that accepts.
	BlockingSystemPodDistruptionTimeout *Duration `json:"blockingSystemPodDistruptionTimeout,omitempty"`
}

// Drain governs how a node is emptied.
type Drain struct {
	// MaxPodsPerDrain caps how many pods a single drain may relocate.
	// Zero means unlimited. This is a blast-radius guard, expressed
	// directly rather than as a utilisation threshold standing in for one.
	MaxPodsPerDrain *int `json:"maxPodsPerDrain,omitempty"`

	// StallTimeout abandons a drain that has stopped making progress.
	//
	// It bounds the absence of progress, not elapsed time. A pod that is
	// terminating within its terminationGracePeriodSeconds counts as
	// progress, so a workload with an hour-long grace period needs no
	// special configuration — see ADR-0007.
	StallTimeout *Duration `json:"stallTimeout,omitempty"`

	// RemovalTimeout is how long to wait, once the node is empty, for the
	// autoscaler to delete it before uncordoning and recording a failure. A
	// cordoned node nothing removes is lost capacity, so this is a
	// correctness bound rather than a convenience.
	//
	// Separate from StallTimeout because it asks a different question: that
	// one is about the workload, this one is about the autoscaler.
	RemovalTimeout *Duration `json:"removalTimeout,omitempty"`
}

// Backoff governs how long a node is left alone after a failed drain.
//
// This is not politeness. Abandoning a drain uncordons a node that now has
// fewer pods than before, and candidates are ordered least-loaded-first — so
// without backoff, binpack would preferentially retry the node that just
// failed, evicting a few more pods each time.
type Backoff struct {
	// Initial delay after the first failed drain. Doubles with each
	// consecutive failure.
	Initial *Duration `json:"initial,omitempty"`

	// Max caps the doubling. Deliberately a long retry rather than a
	// permanent skip: a permanent skip would need a human to clear it, and
	// a node blocked by something transient would stay skipped forever.
	Max *Duration `json:"max,omitempty"`
}

// Cooldown suppresses action after recent cluster activity.
type Cooldown struct {
	// AfterScaleUp mirrors the autoscaler's own scale-down-delay-after-add.
	AfterScaleUp *Duration `json:"afterScaleUp,omitempty"`

	// AfterDrain pauses cluster-wide after a *successful* drain, so binpack
	// does not immediately start another while the cluster is still settling.
	// Distinct from Backoff, which is per-node and follows a failure.
	AfterDrain *Duration `json:"afterDrain,omitempty"`
}

// Exclusions protects workloads from being moved.
type Exclusions struct {
	// Namespaces whose pods binpack will not evict. A node hosting such a
	// pod is not a drain candidate — the pods are protected, not ignored.
	// Ignoring them would remove them from the feasibility arithmetic while
	// leaving them on the node, which is unsound.
	//
	// A pool override replaces this list rather than adding to it, so a pool
	// can narrow the global set as well as widen it.
	//
	// A pointer, like every other optional field here, because an explicitly
	// empty list means something different from an absent one: `namespaces:
	// []` on a pool clears the global exclusions for that pool. A plain slice
	// cannot express that across a round trip, since `omitempty` drops an
	// empty slice and the reloaded document would silently inherit again.
	Namespaces *[]string `json:"namespaces,omitempty"`
}

// PoolPolicy is a fully resolved policy for one pool. Every field has a
// value; nothing here is optional.
// It uses plain time.Duration rather than the wire Duration type: this is a
// Go-facing value, and how it renders is the caller's concern.
type PoolPolicy struct {
	Enabled                             bool
	ExpendablePriorityCutoff            int32
	ReserveForLargestPod                bool
	SkipNodesWithLocalStorage           bool
	SkipNodesWithSystemPods             bool
	BlockingSystemPodDistruptionTimeout time.Duration
	MaxPodsPerDrain                     int
	StallTimeout                        time.Duration
	RemovalTimeout                      time.Duration
	BackoffInitial                      time.Duration
	BackoffMax                          time.Duration
	CooldownAfterScaleUp                time.Duration
	CooldownAfterDrain                  time.Duration
	ExcludedNamespaces                  []string
}

// NodeGroupJoin resolves Discovery.NodeGroups into the lookup the engine
// consumes: a value of the node-group label, to the identifier it belongs to.
//
// Nil rather than an empty map when the document states none, because "no
// join was stated" and "a join covering nothing was stated" reach the engine
// as different things — the first leaves every value matching by equality,
// and the second would be a claim nobody made.
func (c *Config) NodeGroupJoin() map[string]string {
	if len(c.Discovery.NodeGroups) == 0 {
		return nil
	}
	out := make(map[string]string, len(c.Discovery.NodeGroups))
	for _, j := range c.Discovery.NodeGroups {
		out[j.LabelValue] = j.Group
	}
	return out
}
