package v1alpha1

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// minInterval bounds how often binpack may evaluate. Below this the cost of
// re-reading cluster state outweighs any benefit, and a mistake in
// configuration turns into API pressure.
const minInterval = 10 * time.Second

// Label keys are a name, optionally prefixed by a DNS subdomain. This is the
// subset of the Kubernetes rule that matters here; a wrong key produces a
// clear "no nodes matched" failure at preflight rather than silent damage.
var labelKeyRE = regexp.MustCompile(
	`^([a-z0-9]([-a-z0-9]*[a-z0-9])?\.)*[a-z0-9]([-a-z0-9]*[a-z0-9])?/?[A-Za-z0-9]([-A-Za-z0-9_.]*[A-Za-z0-9])?$`)

var namespaceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

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

	if c.Discovery.NodeGroupIDLabel != "" && !labelKeyRE.MatchString(c.Discovery.NodeGroupIDLabel) {
		errs = append(errs, fmt.Errorf("discovery.nodeGroupIDLabel: %q is not a valid label key",
			c.Discovery.NodeGroupIDLabel))
	}
	if c.Discovery.PoolNameLabel != "" && !labelKeyRE.MatchString(c.Discovery.PoolNameLabel) {
		errs = append(errs, fmt.Errorf("discovery.poolNameLabel: %q is not a valid label key",
			c.Discovery.PoolNameLabel))
	}
	// A namespace no object can live in is worth refusing where somebody is
	// still watching. Left to runtime it surfaces as "no cluster-autoscaler is
	// running", which reads as a fact about the cluster rather than a typo in
	// the document that produced it.
	if ns := c.Discovery.AutoscalerNamespace; ns != "" && !namespaceRE.MatchString(ns) {
		errs = append(errs, fmt.Errorf("discovery.autoscalerNamespace: %q is not a valid namespace name", ns))
	}

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
			if !namespaceRE.MatchString(ns) {
				errs = append(errs, fmt.Errorf("%s.exclusions.namespaces[%d]: %q is not a valid namespace name",
					path, i, ns))
			}
		}
	}

	return errs
}
