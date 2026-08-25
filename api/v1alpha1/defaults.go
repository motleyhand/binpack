package v1alpha1

import "time"

// Defaults. These are chosen so that a completely empty configuration is a
// working, safe one: pools and their bounds are discovered rather than
// declared, and dry-run means an unconfigured binpack observes and reports
// without touching anything.
//
// One of them is not binpack's to choose. DefaultRemovalTimeout bounds a wait
// on the cluster-autoscaler, so its floor is that component's own arithmetic,
// mirrored below: ten minutes of scale-down-unneeded-time, ten more of
// scale-down-delay-after-add if anything in the cluster grows during the wait,
// and up to five during which a node already found unremovable is not looked
// at again. Twenty-five, and TestRemovalTimeoutCoversTheAutoscalersOwnDelays
// holds it at or above the first two.
const (
	DefaultInterval             = time.Minute
	DefaultDryRun               = true
	DefaultNodeGroupIDLabel     = "doks.digitalocean.com/node-pool-id"
	DefaultPoolNameLabel        = "doks.digitalocean.com/node-pool"
	DefaultAutoscalerNamespace  = "kube-system"
	DefaultAutoscalerStatusName = "cluster-autoscaler-status"
	DefaultEnabled              = true
	DefaultExpendableCutoff     = int32(-10)
	DefaultReserveForLargestPod = true
	DefaultMaxPodsPerDrain      = 0 // unlimited
	DefaultStallTimeout         = 10 * time.Minute
	DefaultRemovalTimeout       = 25 * time.Minute
	DefaultBackoffInitial       = 30 * time.Minute
	DefaultBackoffMax           = 24 * time.Hour
	DefaultCooldownAfterScaleUp = 10 * time.Minute
	DefaultCooldownAfterDrain   = 15 * time.Minute
)

// Defaults for the policy.autoscaler section, named for the cluster-autoscaler
// flags they mirror rather than for the binpack settings they back, because
// the whole point of the section is that each value is a claim about somebody
// else's component and the next drift between the two should be greppable.
// cluster-autoscaler config/flags/flags.go.
//
// DefaultBlockingSystemPodDistruptionTimeout is the one that is not true of
// every autoscaler binpack supports: the grace it names arrived in
// cluster-autoscaler 1.33, and before it a blocking system pod blocked for as
// long as it was there. An hour is upstream's default at the version binpack
// pins, and an operator running an older one says so with a zero — see
// [Autoscaler.BlockingSystemPodDistruptionTimeout].
const (
	DefaultSkipNodesWithLocalStorage           = true
	DefaultSkipNodesWithSystemPods             = true
	DefaultBlockingSystemPodDistruptionTimeout = time.Hour
)

// Upstream defaults, mirrored so the derivation of a binpack default can be
// stated as arithmetic rather than as a sentence. Not settings: nothing reads
// these to decide anything, and changing one here changes nothing about the
// cluster-autoscaler.
const (
	// DefaultScaleDownUnneededTime is the cluster-autoscaler's own
	// --scale-down-unneeded-time: how long a node must have been unneeded
	// before it will remove it. cluster-autoscaler config/const.go,
	// DefaultScaleDownUnneededTime.
	//
	// It is the floor under [DefaultRemovalTimeout], and it restarts when the
	// autoscaler process does: the unneeded-since map is built fresh by
	// unneeded.NewNodes on every start, so a node the previous process had
	// been watching for nine minutes starts again from zero.
	DefaultScaleDownUnneededTime = 10 * time.Minute

	// DefaultUnremovableNodeRecheck is the cluster-autoscaler's own
	// --unremovable-node-recheck-timeout: having found a node unremovable it
	// caches that answer and does not ask again for this long.
	// cluster-autoscaler config/flags/flags.go.
	//
	// So the delays do not merely add up — the autoscaler can also stop
	// looking part-way through one and come back after it has passed.
	DefaultUnremovableNodeRecheck = 5 * time.Minute
)

// SetDefaults fills in unset top-level fields. Policy fields are left alone:
// they stay nil so that PolicyFor can tell "inherit" from "explicitly set",
// and the defaults are applied there instead.
func (c *Config) SetDefaults() {
	if c.APIVersion == "" {
		c.APIVersion = GroupVersion
	}
	if c.Kind == "" {
		c.Kind = Kind
	}
	if c.Interval == nil {
		c.Interval = NewDuration(DefaultInterval)
	}
	if c.DryRun == nil {
		c.DryRun = new(DefaultDryRun)
	}
	if c.Discovery.NodeGroupIDLabel == "" {
		c.Discovery.NodeGroupIDLabel = DefaultNodeGroupIDLabel
	}
	if c.Discovery.PoolNameLabel == "" {
		c.Discovery.PoolNameLabel = DefaultPoolNameLabel
	}
	// Unlike the label keys, an unset value here cannot be tolerated further
	// down: a Get for a namespaced object with no namespace finds nothing, and
	// binpack would report a healthy cluster-autoscaler as absent. collect
	// refuses an empty namespace rather than reading one, so this default is
	// what keeps a configuration that says nothing about it working.
	if c.Discovery.AutoscalerNamespace == "" {
		c.Discovery.AutoscalerNamespace = DefaultAutoscalerNamespace
	}
	if c.Discovery.AutoscalerStatusName == "" {
		c.Discovery.AutoscalerStatusName = DefaultAutoscalerStatusName
	}
}

// Settings is the resolved top-level configuration: what a document means
// once defaults have been applied, with nothing left to dereference.
type Settings struct {
	Interval time.Duration
	DryRun   bool
}

// Settings resolves the top-level fields, the same way [Config.PolicyFor]
// resolves the per-pool ones.
//
// The wire types use pointers so that an omitted field is distinguishable from
// one explicitly set to its zero value, which round-tripping requires. That is
// a property of the format, not something every caller should have to handle:
// a caller that dereferences directly is one nil away from a panic, and gets
// the wrong answer rather than the default if it guards by testing for nil
// itself.
func (c *Config) Settings() Settings {
	s := Settings{Interval: DefaultInterval, DryRun: DefaultDryRun}
	if c == nil {
		return s
	}
	if c.Interval != nil {
		s.Interval = c.Interval.Duration
	}
	if c.DryRun != nil {
		s.DryRun = *c.DryRun
	}
	return s
}

// PolicyFor resolves the policy for a pool, layering the built-in defaults,
// the global policy, and any override naming this pool — in that order, field
// by field. Names are matched against the pool name label value or the node
// group identifier, whichever the operator wrote.
//
// It resolves what the document says and checks nothing. A name matching no
// pool in the cluster yields exactly the policy an absent override would, and
// what refuses such a configuration is the engine's pool preflight — which
// runs *downstream* of every caller of this rather than upstream of any:
// `explain` passes the configuration this resolved into it, and
// `binpack config validate` has no cluster to run it against at all. That is
// why validate discloses the name as unchecked rather than letting a fully
// resolved override read as a name it verified.
func (c *Config) PolicyFor(names ...string) PoolPolicy {
	p := PoolPolicy{
		Enabled:                  DefaultEnabled,
		ExpendablePriorityCutoff: DefaultExpendableCutoff,
		ReserveForLargestPod:     DefaultReserveForLargestPod,

		SkipNodesWithLocalStorage:           DefaultSkipNodesWithLocalStorage,
		SkipNodesWithSystemPods:             DefaultSkipNodesWithSystemPods,
		BlockingSystemPodDistruptionTimeout: DefaultBlockingSystemPodDistruptionTimeout,

		MaxPodsPerDrain:      DefaultMaxPodsPerDrain,
		StallTimeout:         DefaultStallTimeout,
		RemovalTimeout:       DefaultRemovalTimeout,
		BackoffInitial:       DefaultBackoffInitial,
		BackoffMax:           DefaultBackoffMax,
		CooldownAfterScaleUp: DefaultCooldownAfterScaleUp,
		CooldownAfterDrain:   DefaultCooldownAfterDrain,
	}

	p.apply(c.Policy)

	for i := range c.Pools {
		if matchesAny(c.Pools[i].Name, names) {
			p.apply(c.Pools[i].Policy)
		}
	}

	return p
}

// apply overlays the set fields of a Policy onto a resolved policy.
func (p *PoolPolicy) apply(o Policy) {
	if o.Enabled != nil {
		p.Enabled = *o.Enabled
	}
	if o.Feasibility.ExpendablePriorityCutoff != nil {
		p.ExpendablePriorityCutoff = *o.Feasibility.ExpendablePriorityCutoff
	}
	if o.Feasibility.ReserveForLargestPod != nil {
		p.ReserveForLargestPod = *o.Feasibility.ReserveForLargestPod
	}
	if o.Autoscaler.SkipNodesWithLocalStorage != nil {
		p.SkipNodesWithLocalStorage = *o.Autoscaler.SkipNodesWithLocalStorage
	}
	if o.Autoscaler.SkipNodesWithSystemPods != nil {
		p.SkipNodesWithSystemPods = *o.Autoscaler.SkipNodesWithSystemPods
	}
	if o.Autoscaler.BlockingSystemPodDistruptionTimeout != nil {
		p.BlockingSystemPodDistruptionTimeout = o.Autoscaler.BlockingSystemPodDistruptionTimeout.Duration
	}
	if o.Drain.MaxPodsPerDrain != nil {
		p.MaxPodsPerDrain = *o.Drain.MaxPodsPerDrain
	}
	if o.Drain.StallTimeout != nil {
		p.StallTimeout = o.Drain.StallTimeout.Duration
	}
	if o.Drain.RemovalTimeout != nil {
		p.RemovalTimeout = o.Drain.RemovalTimeout.Duration
	}
	if o.Backoff.Initial != nil {
		p.BackoffInitial = o.Backoff.Initial.Duration
	}
	if o.Backoff.Max != nil {
		p.BackoffMax = o.Backoff.Max.Duration
	}
	if o.Cooldown.AfterScaleUp != nil {
		p.CooldownAfterScaleUp = o.Cooldown.AfterScaleUp.Duration
	}
	if o.Cooldown.AfterDrain != nil {
		p.CooldownAfterDrain = o.Cooldown.AfterDrain.Duration
	}
	// An override replaces the namespace list rather than appending to it,
	// so a pool can narrow the global set as well as widen it. A non-nil
	// pointer to an empty list clears it entirely.
	if o.Exclusions.Namespaces != nil {
		p.ExcludedNamespaces = append([]string(nil), *o.Exclusions.Namespaces...)
	}
}

func matchesAny(name string, candidates []string) bool {
	for _, c := range candidates {
		if c != "" && name == c {
			return true
		}
	}
	return false
}
