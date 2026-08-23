package v1alpha1

import (
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
)

// minInterval bounds how often binpack may evaluate. Below this the cost of
// re-reading cluster state outweighs any benefit, and a mistake in
// configuration turns into API pressure.
const minInterval = 10 * time.Second

// namespaceErrors reports why a string is not a namespace name, if it is not.
//
// A namespace name is a DNS-1123 label, and this asks the API server's own
// helper rather than restating the rule. What used to be here was a regexp
// matching the character rule alone, which is the half of the spec a
// hand-rolled copy keeps; the 63-character limit is the half it loses, and
// nothing goes wrong until a namespace binpack cannot read looks exactly like
// a namespace that is empty.
func namespaceErrors(path, ns string) []error {
	var errs []error
	for _, why := range validation.IsDNS1123Label(ns) {
		errs = append(errs, fmt.Errorf("%s: %q is not a valid namespace name: %s", path, ns, why))
	}
	return errs
}

// labelKeyErrors reports why a string is not a label key, if it is not.
//
// The same borrowing as above, and the one that had gone wrong. What used to
// be here was a regexp applying the *prefix* rule to a key with no prefix, so
// it required a bare key to begin lowercase — and Kubernetes does not:
// upstream splits on '/', validates any prefix as a DNS subdomain and the name
// part against a rule whose own error message offers "MyName" as its first
// example (k8s.io/apimachinery pkg/api/validate/content, IsLabelKey, which is
// what the API server's ValidateLabelName calls).
//
// Getting this wrong is not a missed consolidation. Validate runs at load, so
// a key binpack refuses is binpack refusing to start — on a field every
// operator whose cluster is not DOKS has to set, since the defaults are
// DigitalOcean's.
func labelKeyErrors(path, key string) []error {
	var errs []error
	for _, why := range validation.IsQualifiedName(key) {
		errs = append(errs, fmt.Errorf("%s: %q is not a valid label key: %s", path, key, why))
	}
	return errs
}

// labelValueErrors reports why a string is not a label value, if it is not.
//
// The same borrowing as above and for the same reason: the rule is 63
// characters of alphanumerics, `-`, `_` and `.`, beginning and ending
// alphanumeric, and a hand-rolled copy keeps the character half and loses the
// length. That length is not incidental here — it is exactly why the
// identifier a cloud provider publishes cannot always be carried by a label
// at all, which is the case discovery.nodeGroups exists for.
func labelValueErrors(path, value string) []error {
	var errs []error
	for _, why := range validation.IsValidLabelValue(value) {
		errs = append(errs, fmt.Errorf("%s: %q is not a valid label value: %s", path, value, why))
	}
	return errs
}

// Validate reports every problem it finds rather than the first, so a
// misconfigured file can be fixed in one pass instead of several.
func (c *Config) Validate() error {
	var errs []error

	if c.APIVersion != GroupVersion {
		errs = append(errs, fmt.Errorf("apiVersion: got %q, want %q", c.APIVersion, GroupVersion))
	}
	if c.Kind != Kind {
		errs = append(errs, fmt.Errorf("kind: got %q, want %q", c.Kind, Kind))
	}

	if c.Interval != nil && c.Interval.Duration < minInterval {
		errs = append(errs, fmt.Errorf("interval: %s is below the minimum of %s",
			c.Interval.Duration, minInterval))
	}

	if key := c.Discovery.NodeGroupIDLabel; key != "" {
		errs = append(errs, labelKeyErrors("discovery.nodeGroupIDLabel", key)...)
	}
	if key := c.Discovery.PoolNameLabel; key != "" {
		errs = append(errs, labelKeyErrors("discovery.poolNameLabel", key)...)
	}
	// A namespace no object can live in is worth refusing where somebody is
	// still watching. Left to runtime it surfaces as "no cluster-autoscaler is
	// running", which reads as a fact about the cluster rather than a typo in
	// the document that produced it.
	if ns := c.Discovery.AutoscalerNamespace; ns != "" {
		errs = append(errs, namespaceErrors("discovery.autoscalerNamespace", ns)...)
	}
	// And the object's name, for the same reason and by the same rule: a
	// ConfigMap name is a DNS subdomain, so anything that is not one names an
	// object that cannot exist.
	if name := c.Discovery.AutoscalerStatusName; name != "" {
		for _, why := range validation.IsDNS1123Subdomain(name) {
			errs = append(errs, fmt.Errorf(
				"discovery.autoscalerStatusName: %q is not a valid ConfigMap name: %s", name, why))
		}
	}
	errs = append(errs, validateNodeGroups(c.Discovery.NodeGroups)...)

	errs = append(errs, c.Policy.validate("policy")...)

	seen := make(map[string]int, len(c.Pools))
	for i, p := range c.Pools {
		path := fmt.Sprintf("pools[%d]", i)
		if p.Name == "" {
			errs = append(errs, fmt.Errorf("%s.name: must not be empty", path))
		} else if first, dup := seen[p.Name]; dup {
			errs = append(errs, fmt.Errorf("%s.name: %q already configured at pools[%d]",
				path, p.Name, first))
		} else {
			seen[p.Name] = i
		}
		errs = append(errs, p.validate(path)...)
	}

	// Some invariants are properties of the *resolved* policy rather than of
	// any single layer: a value can be legal where it is written and illegal
	// once defaults and inheritance are applied. Checking the raw document
	// alone would accept `backoff.max: 10m` against a 30-minute default
	// initial delay, which is exactly the state the check exists to reject.
	errs = append(errs, c.PolicyFor().validate("policy (resolved)")...)
	for i, pool := range c.Pools {
		if pool.Name == "" {
			continue // already reported; PolicyFor would match nothing useful
		}
		errs = append(errs, c.PolicyFor(pool.Name).validate(
			fmt.Sprintf("pools[%d] %q (resolved)", i, pool.Name))...)
	}

	return errors.Join(errs...)
}

// validate checks invariants that only exist once defaults and per-pool
// overrides have been applied.
func (p PoolPolicy) validate(path string) []error {
	var errs []error

	if p.BackoffMax < p.BackoffInitial {
		// Otherwise the cap would shorten the first retry rather than bound
		// the doubling, which is the opposite of what it reads as.
		errs = append(errs, fmt.Errorf(
			"%s.backoff.max: %s is shorter than backoff.initial %s (values may come from defaults or inheritance)",
			path, p.BackoffMax, p.BackoffInitial))
	}

	return errs
}

func (p Policy) validate(path string) []error {
	var errs []error

	if cutoff := p.Feasibility.ExpendablePriorityCutoff; cutoff != nil && *cutoff > 0 {
		// Above zero this would classify ordinary workloads as expendable and
		// exclude them from the feasibility simulation, which is exactly how
		// binpack would drain a node whose pods have nowhere to go.
		errs = append(errs, fmt.Errorf(
			"%s.feasibility.expendablePriorityCutoff: %d is above 0, which would treat ordinary pods as expendable",
			path, *cutoff))
	}

	if t := p.Autoscaler.BlockingSystemPodDistruptionTimeout; t != nil && t.Duration < 0 {
		// Zero is meaningful here — it is how an operator says their
		// autoscaler predates the grace entirely — so only a negative is
		// refused, and it is refused rather than clamped because it says
		// nothing anybody meant.
		errs = append(errs, fmt.Errorf(
			"%s.autoscaler.blockingSystemPodDistruptionTimeout: must not be negative, got %s "+
				"(use 0 for an autoscaler that has no grace at all)",
			path, t.Duration))
	}

	if n := p.Drain.MaxPodsPerDrain; n != nil && *n < 0 {
		errs = append(errs, fmt.Errorf("%s.drain.maxPodsPerDrain: %d is negative (use 0 for unlimited)",
			path, *n))
	}

	if t := p.Drain.StallTimeout; t != nil && t.Duration <= 0 {
		// Without a positive bound, a drain wedged on an unevictable pod
		// would hold the node cordoned indefinitely.
		errs = append(errs, fmt.Errorf("%s.drain.stallTimeout: must be positive, got %s",
			path, t.Duration))
	}

	if t := p.Drain.RemovalTimeout; t != nil && t.Duration <= 0 {
		// Without a positive timeout, a node the autoscaler never removes
		// would stay cordoned forever.
		errs = append(errs, fmt.Errorf("%s.drain.removalTimeout: must be positive, got %s",
			path, t.Duration))
	}

	if t := p.Backoff.Initial; t != nil && t.Duration <= 0 {
		errs = append(errs, fmt.Errorf("%s.backoff.initial: must be positive, got %s",
			path, t.Duration))
	}
	if t := p.Backoff.Max; t != nil && t.Duration <= 0 {
		errs = append(errs, fmt.Errorf("%s.backoff.max: must be positive, got %s",
			path, t.Duration))
	}

	if t := p.Cooldown.AfterScaleUp; t != nil && t.Duration < 0 {
		errs = append(errs, fmt.Errorf("%s.cooldown.afterScaleUp: must not be negative, got %s",
			path, t.Duration))
	}
	if t := p.Cooldown.AfterDrain; t != nil && t.Duration < 0 {
		errs = append(errs, fmt.Errorf("%s.cooldown.afterDrain: must not be negative, got %s",
			path, t.Duration))
	}

	if p.Exclusions.Namespaces != nil {
		for i, ns := range *p.Exclusions.Namespaces {
			errs = append(errs, namespaceErrors(
				fmt.Sprintf("%s.exclusions.namespaces[%d]", path, i), ns)...)
		}
	}

	return errs
}

// validateNodeGroups rejects a stated join binpack could not act on.
//
// Every one of these is an entry that would sit in the document looking
// applied while doing nothing, or worse. An empty label value claims every
// node that carries no label at all; an empty group names the pool the status
// document writes for a group with no name; a duplicated value would resolve
// to whichever entry happened to be last; and a value no label may hold can
// never match a node.
func validateNodeGroups(joins []NodeGroupJoin) []error {
	var errs []error
	seen := make(map[string]int, len(joins))
	for i, j := range joins {
		path := fmt.Sprintf("discovery.nodeGroups[%d]", i)
		malformed := labelValueErrors(path+".labelValue", j.LabelValue)
		switch first, dup := seen[j.LabelValue]; {
		case j.LabelValue == "":
			errs = append(errs, fmt.Errorf("%s.labelValue: must not be empty", path))
		case dup:
			errs = append(errs, fmt.Errorf("%s.labelValue: %q already joined at "+
				"discovery.nodeGroups[%d]", path, j.LabelValue, first))
		case len(malformed) > 0:
			errs = append(errs, malformed...)
		default:
			seen[j.LabelValue] = i
		}
		if j.Group == "" {
			errs = append(errs, fmt.Errorf("%s.group: must not be empty", path))
		}
	}
	return errs
}
