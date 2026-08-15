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
}

// Policy is the set of tunables that can be set globally or per pool.
// Every field is optional; unset fields inherit.
type Policy struct {
	// Enabled false leaves a pool's nodes alone entirely. Useful for
	// rolling binpack out to one pool at a time.
	Enabled *bool `json:"enabled,omitempty"`

	Feasibility Feasibility `json:"feasibility,omitempty"`
	Drain       Drain       `json:"drain,omitempty"`
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
	// node still has room for a pod the size of the largest relocatable pod
	// running in the cluster.
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

	// VerifyRemovalTimeout is how long to wait for the autoscaler to delete
	// a drained node before uncordoning it and recording a failure. A
	// cordoned node nothing removes is lost capacity, so this is a
	// correctness bound rather than a convenience.
	VerifyRemovalTimeout *Duration `json:"verifyRemovalTimeout,omitempty"`
}

// Cooldown suppresses action after recent cluster activity.
type Cooldown struct {
	// AfterScaleUp mirrors the autoscaler's own scale-down-delay-after-add.
	AfterScaleUp *Duration `json:"afterScaleUp,omitempty"`

	// AfterDrain prevents a drain and an immediate re-drain oscillating.
	AfterDrain *Duration `json:"afterDrain,omitempty"`
}

// Exclusions protects workloads from being moved.
type Exclusions struct {
	// Namespaces whose pods binpack will not evict. A node hosting such a
	// pod is not a drain candidate — the pods are protected, not ignored.
	// Ignoring them would remove them from the feasibility arithmetic while
	// leaving them on the node, which is unsound.
	//
	// A pool override replaces this list rather than adding to it.
	Namespaces []string `json:"namespaces,omitempty"`
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
	VerifyRemovalTimeout     time.Duration
	CooldownAfterScaleUp     time.Duration
	CooldownAfterDrain       time.Duration
	ExcludedNamespaces       []string
}
