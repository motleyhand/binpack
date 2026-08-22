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
	Discovery Discovery `json:"discovery,omitempty"`

	// Policy applies to every discovered pool that has no override.
	Policy Policy `json:"policy,omitempty"`

	// Pools overrides Policy for individual discovered pools. Pools are
	// never declared here — they are discovered — so an entry naming a pool
	// that does not exist is a configuration error rather than a definition.
	Pools []PoolOverride `json:"pools,omitempty"`
}

// Discovery configures how nodes are mapped to autoscaler node groups.
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

	Feasibility Feasibility `json:"feasibility,omitempty"`
	Drain       Drain       `json:"drain,omitempty"`
	Backoff     Backoff     `json:"backoff,omitempty"`
	Cooldown    Cooldown    `json:"cooldown,omitempty"`
	Exclusions  Exclusions  `json:"exclusions,omitempty"`
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
	Enabled                  bool
	ExpendablePriorityCutoff int32
	ReserveForLargestPod     bool
	MaxPodsPerDrain          int
	StallTimeout             time.Duration
	RemovalTimeout           time.Duration
	BackoffInitial           time.Duration
	BackoffMax               time.Duration
	CooldownAfterScaleUp     time.Duration
	CooldownAfterDrain       time.Duration
	ExcludedNamespaces       []string
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
