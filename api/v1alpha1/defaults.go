package v1alpha1

import "time"

// Defaults. These are chosen so that a completely empty configuration is a
// working, safe one: pools and their bounds are discovered rather than
// declared, and dry-run means an unconfigured binpack observes and reports
// without touching anything.
const (
	DefaultInterval             = time.Minute
	DefaultDryRun               = true
	DefaultNodeGroupIDLabel     = "doks.digitalocean.com/node-pool-id"
	DefaultPoolNameLabel        = "doks.digitalocean.com/node-pool"
	DefaultEnabled              = true
	DefaultExpendableCutoff     = int32(-10)
	DefaultReserveForLargestPod = true
	DefaultMaxPodsPerDrain      = 0 // unlimited
	DefaultStallTimeout         = 10 * time.Minute
	DefaultRemovalTimeout       = 15 * time.Minute
	DefaultBackoffInitial       = 30 * time.Minute
	DefaultBackoffMax           = 24 * time.Hour
	DefaultCooldownAfterScaleUp = 10 * time.Minute
	DefaultCooldownAfterDrain   = 15 * time.Minute
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
		c.DryRun = boolPtr(DefaultDryRun)
	}
	if c.Discovery.NodeGroupIDLabel == "" {
		c.Discovery.NodeGroupIDLabel = DefaultNodeGroupIDLabel
	}
	if c.Discovery.PoolNameLabel == "" {
		c.Discovery.PoolNameLabel = DefaultPoolNameLabel
	}
}

// PolicyFor resolves the policy for a pool, layering the built-in defaults,
// the global policy, and any override naming this pool — in that order, field
// by field. Names are matched against the pool name label value or the node
// group identifier, whichever the operator wrote.
//
// Unknown pool names are a validation error rather than a silent no-op, so a
// name reaching here has already been checked against discovery.
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

func (c *Config) PolicyFor(names ...string) PoolPolicy {
	p := PoolPolicy{
		Enabled:                  DefaultEnabled,
		ExpendablePriorityCutoff: DefaultExpendableCutoff,
		ReserveForLargestPod:     DefaultReserveForLargestPod,
		MaxPodsPerDrain:          DefaultMaxPodsPerDrain,
		StallTimeout:             DefaultStallTimeout,
		RemovalTimeout:           DefaultRemovalTimeout,
		BackoffInitial:           DefaultBackoffInitial,
		BackoffMax:               DefaultBackoffMax,
		CooldownAfterScaleUp:     DefaultCooldownAfterScaleUp,
		CooldownAfterDrain:       DefaultCooldownAfterDrain,
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

func boolPtr(b bool) *bool { return &b }
