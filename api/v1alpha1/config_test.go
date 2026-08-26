package v1alpha1

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func mustLoad(t *testing.T, y string) *Config {
	t.Helper()
	cfg, err := Load([]byte(y))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestEmptyConfigIsValidAndSafe(t *testing.T) {
	// An operator who installs binpack without writing configuration must
	// get something that works and touches nothing.
	cfg := mustLoad(t, "")

	if cfg.APIVersion != GroupVersion || cfg.Kind != Kind {
		t.Errorf("type meta not defaulted: %q %q", cfg.APIVersion, cfg.Kind)
	}
	if cfg.DryRun == nil || !*cfg.DryRun {
		t.Error("dryRun must default to true: acting is opt-in")
	}
	if cfg.Interval.Duration != DefaultInterval {
		t.Errorf("interval = %s, want %s", cfg.Interval.Duration, DefaultInterval)
	}
	if cfg.Discovery.NodeGroupIDLabel != DefaultNodeGroupIDLabel {
		t.Errorf("nodeGroupIDLabel = %q", cfg.Discovery.NodeGroupIDLabel)
	}
	// kube-system is where most clusters run the autoscaler, and an empty
	// value is not a namespace anything can be read from — so the default has
	// to be a name, not the absence of one.
	if cfg.Discovery.AutoscalerNamespace != DefaultAutoscalerNamespace {
		t.Errorf("autoscalerNamespace = %q, want %q",
			cfg.Discovery.AutoscalerNamespace, DefaultAutoscalerNamespace)
	}
	// And the other half of the location, for the same reason and with the
	// same consequence: binpack reads one object, and a Get for a name
	// nothing published finds nothing and reports a healthy autoscaler as
	// absent. This constant is the only spelling of that name in the tree —
	// the fixtures every autoscaler test builds are constructed from it — so
	// what it is set to is what binpack reads on a cluster that configured
	// nothing.
	if cfg.Discovery.AutoscalerStatusName != DefaultAutoscalerStatusName {
		t.Errorf("autoscalerStatusName = %q, want %q",
			cfg.Discovery.AutoscalerStatusName, DefaultAutoscalerStatusName)
	}

	p := cfg.PolicyFor("any-pool")
	if !p.Enabled {
		t.Error("pools should be enabled by default")
	}
	if p.ExpendablePriorityCutoff != DefaultExpendableCutoff {
		t.Errorf("cutoff = %d, want %d", p.ExpendablePriorityCutoff, DefaultExpendableCutoff)
	}
	if p.StallTimeout != DefaultStallTimeout {
		t.Errorf("stallTimeout = %s, want %s", p.StallTimeout, DefaultStallTimeout)
	}
	if p.RemovalTimeout != DefaultRemovalTimeout {
		t.Errorf("removalTimeout = %s, want %s", p.RemovalTimeout, DefaultRemovalTimeout)
	}
	if p.BackoffInitial != DefaultBackoffInitial || p.BackoffMax != DefaultBackoffMax {
		t.Errorf("backoff = %s..%s", p.BackoffInitial, p.BackoffMax)
	}
}

func TestPolicyResolution(t *testing.T) {
	cfg := mustLoad(t, `
apiVersion: binpack.motleyhand.com/v1alpha1
kind: BinpackConfig
policy:
  drain:
    maxPodsPerDrain: 10
  cooldown:
    afterDrain: 30m
  exclusions:
    namespaces: [kube-system, monitoring]
pools:
  - name: pool-8g
    enabled: false
  - name: pool-4g
    drain:
      maxPodsPerDrain: 3
    exclusions:
      namespaces: [kube-system]
`)

	t.Run("pool without an override inherits the global policy", func(t *testing.T) {
		p := cfg.PolicyFor("pool-other")
		if p.MaxPodsPerDrain != 10 {
			t.Errorf("maxPodsPerDrain = %d, want 10", p.MaxPodsPerDrain)
		}
		if got, want := len(p.ExcludedNamespaces), 2; got != want {
			t.Errorf("excluded namespaces = %v, want %d entries", p.ExcludedNamespaces, want)
		}
		if !p.Enabled {
			t.Error("should be enabled")
		}
	})

	t.Run("override wins field by field", func(t *testing.T) {
		p := cfg.PolicyFor("pool-4g")
		if p.MaxPodsPerDrain != 3 {
			t.Errorf("maxPodsPerDrain = %d, want 3 from the override", p.MaxPodsPerDrain)
		}
		// Not overridden, so still inherited.
		if p.CooldownAfterDrain != 30*time.Minute {
			t.Errorf("cooldownAfterDrain = %s, want the global 30m", p.CooldownAfterDrain)
		}
		// Untouched by either, so still the built-in default.
		if p.CooldownAfterScaleUp != DefaultCooldownAfterScaleUp {
			t.Errorf("cooldownAfterScaleUp = %s, want the default", p.CooldownAfterScaleUp)
		}
	})

	t.Run("namespace lists replace rather than merge", func(t *testing.T) {
		p := cfg.PolicyFor("pool-4g")
		if len(p.ExcludedNamespaces) != 1 || p.ExcludedNamespaces[0] != "kube-system" {
			t.Errorf("namespaces = %v, want exactly [kube-system]", p.ExcludedNamespaces)
		}
	})

	t.Run("explicit false is honoured, not treated as unset", func(t *testing.T) {
		if cfg.PolicyFor("pool-8g").Enabled {
			t.Error("pool-8g sets enabled: false and must resolve to disabled")
		}
	})

	t.Run("a pool matches on either of its identifiers", func(t *testing.T) {
		// Callers pass both the pool name label value and the node group ID,
		// so an operator may write whichever they know.
		if cfg.PolicyFor("some-uuid", "pool-4g").MaxPodsPerDrain != 3 {
			t.Error("should match on the second identifier too")
		}
	})
}

func TestValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "wrong apiVersion",
			yaml:    "apiVersion: v1\nkind: BinpackConfig",
			wantErr: "apiVersion",
		},
		{
			name:    "wrong kind",
			yaml:    "kind: ConfigMap",
			wantErr: "kind",
		},
		{
			name:    "unknown field is rejected rather than ignored",
			yaml:    "dryRunn: false",
			wantErr: "parsing configuration",
		},
		{
			name:    "misspelled nested field is rejected too",
			yaml:    "policy:\n  drain:\n    maxPodsPerDrian: 5",
			wantErr: "parsing configuration",
		},
		{
			name:    "interval below the floor",
			yaml:    "interval: 1s",
			wantErr: "below the minimum",
		},
		{
			name:    "unparseable duration",
			yaml:    "interval: soon",
			wantErr: "invalid duration",
		},
		{
			name:    "positive expendable cutoff would treat real pods as expendable",
			yaml:    "policy:\n  feasibility:\n    expendablePriorityCutoff: 100",
			wantErr: "above 0",
		},
		{
			name:    "negative blocking system pod timeout",
			yaml:    "policy:\n  autoscaler:\n    blockingSystemPodDistruptionTimeout: -5m",
			wantErr: "must not be negative",
		},
		{
			name:    "negative maxPodsPerDrain",
			yaml:    "policy:\n  drain:\n    maxPodsPerDrain: -1",
			wantErr: "negative",
		},
		{
			name:    "zero removal timeout would leave nodes cordoned forever",
			yaml:    "policy:\n  drain:\n    removalTimeout: 0s",
			wantErr: "drain.removalTimeout: must be positive",
		},
		{
			name:    "zero stall timeout would let a wedged drain hold a node forever",
			yaml:    "policy:\n  drain:\n    stallTimeout: 0s",
			wantErr: "drain.stallTimeout: must be positive",
		},
		{
			name:    "backoff max below initial would shorten the first retry",
			yaml:    "policy:\n  backoff:\n    initial: 1h\n    max: 10m",
			wantErr: "shorter than backoff.initial",
		},
		{
			// Legal where written, illegal once the 30m default initial
			// delay is applied. Only a resolved-policy check catches this.
			name:    "backoff max below the default initial",
			yaml:    "policy:\n  backoff:\n    max: 10m",
			wantErr: "shorter than backoff.initial",
		},
		{
			// Each layer is individually fine; the combination is not.
			name:    "pool inherits a long initial and overrides only max",
			yaml:    "policy:\n  backoff:\n    initial: 1h\npools:\n  - name: pool-4g\n    backoff:\n      max: 10m",
			wantErr: `pools[0] "pool-4g" (resolved)`,
		},
		{
			name:    "duplicate pool names",
			yaml:    "pools:\n  - name: a\n  - name: a",
			wantErr: "already configured",
		},
		{
			name:    "empty pool name",
			yaml:    "pools:\n  - name: \"\"",
			wantErr: "must not be empty",
		},
		{
			name:    "invalid namespace",
			yaml:    "policy:\n  exclusions:\n    namespaces: [Not_A_Namespace]",
			wantErr: "not a valid namespace",
		},
		{
			name:    "invalid label key",
			yaml:    "discovery:\n  poolNameLabel: \"has spaces\"",
			wantErr: "not a valid label key",
		},
		{
			// The other of the two keys. Both are checked and only one was
			// ever exercised, so the guard on this one could have been
			// deleted with nothing going red.
			name:    "invalid node group ID label",
			yaml:    "discovery:\n  nodeGroupIDLabel: \"has spaces\"",
			wantErr: "discovery.nodeGroupIDLabel",
		},
		{
			// Zero is not "no backoff", it is a retry loop with no delay in
			// it: the doubling starts from the initial value, so a zero
			// initial can never grow. Rejected rather than clamped, and the
			// exact value is what separates that from the legal 1h below.
			name:    "zero backoff initial would never grow",
			yaml:    "policy:\n  backoff:\n    initial: 0s",
			wantErr: "backoff.initial: must be positive",
		},
		{
			name:    "zero backoff max would cap every retry at nothing",
			yaml:    "policy:\n  backoff:\n    max: 0s",
			wantErr: "backoff.max: must be positive",
		},
		{
			// A namespace binpack cannot read is worth rejecting where
			// somebody is still watching: the runtime symptom is a report
			// that no cluster-autoscaler is running, which reads as a fact
			// about the cluster rather than about a typo.
			name:    "invalid autoscaler namespace",
			yaml:    "discovery:\n  autoscalerNamespace: Kube_System",
			wantErr: "not a valid namespace",
		},
		{
			// A namespace name is a DNS-1123 label, and the length rule is
			// half of that spec. Kubernetes has no such namespace to find, so
			// the Get returns NotFound and binpack reports a cluster with no
			// cluster-autoscaler — the same confident falsehood the field
			// exists to end, reached by a different route.
			name:    "autoscaler namespace longer than a DNS label",
			yaml:    "discovery:\n  autoscalerNamespace: " + strings.Repeat("a", 64),
			wantErr: "not a valid namespace",
		},
		{
			// The same helper, and the same gap: this list has always been
			// checked by a regexp that matched the characters and not the
			// length.
			name:    "excluded namespace longer than a DNS label",
			yaml:    "policy:\n  exclusions:\n    namespaces: [" + strings.Repeat("a", 64) + "]",
			wantErr: "not a valid namespace",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected an error mentioning %q, got none", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error should mention %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestLabelKeysKubernetesAcceptsAreAccepted(t *testing.T) {
	// The rule is the API server's, not binpack's: a label key is an optional
	// DNS-subdomain prefix and a name, and the name may start with an
	// uppercase letter — "MyName" is the first example in upstream's own
	// error message (k8s.io/apimachinery pkg/api/validate/content, IsLabelKey,
	// which is what ValidateLabelName calls). Refusing one is not a
	// consolidation binpack declines; it is binpack refusing to start, on a
	// field every operator not on DOKS has to set.
	for _, key := range []string{
		"NodeGroup", // a hand-rolled labelling scheme
		"Pool_Name", // underscores are legal in the name part
		"eks.amazonaws.com/nodegroup",
		"example.com/MyName", // uppercase behind a prefix
		"123-abc",
	} {
		t.Run(key, func(t *testing.T) {
			if _, err := Load([]byte("discovery:\n  poolNameLabel: " + key)); err != nil {
				t.Errorf("Kubernetes accepts %q as a label key: %v", key, err)
			}
		})
	}
}

func TestValuesAtTheBoundaryAreValid(t *testing.T) {
	// TestValidation is a rejection-only table, and a validator is two claims:
	// this value is refused, and the one next to it is not. Only the first had
	// a test, so every accepting side of every bound could be tightened by one
	// with the suite green — and a validator that refuses a legal value is not
	// a worse decision, it is binpack failing to start.
	//
	// Each row is a value the documentation, an error message or the reference
	// tells an operator they may write. The resolved figure is asserted too,
	// because "loaded without error" does not say the value survived
	// defaulting: a zero that is silently replaced by a default reads as
	// accepted and is not.
	for _, tc := range []struct {
		name  string
		yaml  string
		check func(*testing.T, *Config)
	}{
		{
			// The documented minimum, stated as the minimum. Rejecting it
			// leaves the reference describing a value the loader refuses.
			name: "interval at the documented minimum",
			yaml: "interval: 10s",
			check: func(t *testing.T, c *Config) {
				if got := c.Settings().Interval; got != 10*time.Second {
					t.Errorf("interval = %s, want 10s", got)
				}
			},
		},
		{
			// The validator's own error message documents this value: "use 0
			// for unlimited".
			name: "maxPodsPerDrain zero means unlimited",
			yaml: "policy:\n  drain:\n    maxPodsPerDrain: 0",
			check: func(t *testing.T, c *Config) {
				if got := c.PolicyFor().MaxPodsPerDrain; got != 0 {
					t.Errorf("maxPodsPerDrain = %d, want 0", got)
				}
			},
		},
		{
			// Zero is the boundary of "above 0 would treat ordinary pods as
			// expendable", and it is the value that excludes only pods of
			// negative priority.
			name: "expendable cutoff at zero",
			yaml: "policy:\n  feasibility:\n    expendablePriorityCutoff: 0",
			check: func(t *testing.T, c *Config) {
				if got := c.PolicyFor().ExpendablePriorityCutoff; got != 0 {
					t.Errorf("expendablePriorityCutoff = %d, want 0", got)
				}
			},
		},
		{
			// binpack prints this one at the operator: refusing to start
			// under --once, it says "set cooldown.afterDrain: 0 to say that
			// consecutive drains are acceptable". Nothing asserted that the
			// value binpack instructs them to write survives its own loader.
			name: "cooldowns at zero",
			yaml: "policy:\n  cooldown:\n    afterDrain: 0s\n    afterScaleUp: 0s",
			check: func(t *testing.T, c *Config) {
				p := c.PolicyFor()
				if p.CooldownAfterDrain != 0 || p.CooldownAfterScaleUp != 0 {
					t.Errorf("cooldowns = %s/%s, want 0s/0s",
						p.CooldownAfterDrain, p.CooldownAfterScaleUp)
				}
			},
		},
		{
			// max equal to initial is a backoff that does not double, which is
			// a legitimate thing to ask for. The rejection is for a max
			// *below* initial, where the cap would shorten the first retry.
			name: "backoff max equal to initial",
			yaml: "policy:\n  backoff:\n    initial: 1h\n    max: 1h",
			check: func(t *testing.T, c *Config) {
				p := c.PolicyFor()
				if p.BackoffInitial != time.Hour || p.BackoffMax != time.Hour {
					t.Errorf("backoff = %s/%s, want 1h/1h", p.BackoffInitial, p.BackoffMax)
				}
			},
		},
		{
			name: "a prefixed node group ID label",
			yaml: "discovery:\n  nodeGroupIDLabel: example.com/pool",
			check: func(t *testing.T, c *Config) {
				if got := c.Discovery.NodeGroupIDLabel; got != "example.com/pool" {
					t.Errorf("nodeGroupIDLabel = %q", got)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("a documented value was refused: %v", err)
			}
			tc.check(t, cfg)
		})
	}
}

func TestFieldNamesAreCaseInsensitive(t *testing.T) {
	// Not a deliberate feature, but a consequence of parsing YAML through
	// encoding/json — which Kubernetes itself does, so the behaviour matches
	// what operators are used to. Pinned here because it is surprising and
	// because it interacts with strict unknown-field rejection: "dryrun" is
	// accepted as dryRun, while "dryRunn" is an error.
	cfg := mustLoad(t, "dryrun: false")
	if cfg.DryRun == nil || *cfg.DryRun {
		t.Error("dryrun should have matched the dryRun field")
	}
}

func TestValidationReportsEveryProblem(t *testing.T) {
	// One pass should surface everything, so a bad file can be fixed once.
	_, err := Load([]byte("interval: 1s\npools:\n  - name: \"\"\n  - name: \"\""))
	if err == nil {
		t.Fatal("expected errors")
	}
	msg := err.Error()
	for _, want := range []string{"below the minimum", "must not be empty"} {
		if !strings.Contains(msg, want) {
			t.Errorf("combined error should mention %q, got:\n%s", want, msg)
		}
	}
}

func TestRoundTrip(t *testing.T) {
	original := `apiVersion: binpack.motleyhand.com/v1alpha1
kind: BinpackConfig
interval: 30s
dryRun: false
policy:
  feasibility:
    expendablePriorityCutoff: -10
    reserveForLargestPod: false
  drain:
    maxPodsPerDrain: 5
    stallTimeout: 20m
    removalTimeout: 25m
pools:
  - name: pool-4g
    enabled: true
`
	cfg := mustLoad(t, original)

	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	reloaded, err := Load(out)
	if err != nil {
		t.Fatalf("reloading marshalled config: %v\n%s", err, out)
	}

	// Compare through the resolved view, which is what actually governs
	// behaviour.
	before, after := cfg.PolicyFor("pool-4g"), reloaded.PolicyFor("pool-4g")
	if !reflect.DeepEqual(before, after) {
		t.Errorf("policy changed across a round trip:\nbefore %+v\nafter  %+v", before, after)
	}
	if cfg.Interval.Duration != reloaded.Interval.Duration {
		t.Errorf("interval changed: %s -> %s", cfg.Interval.Duration, reloaded.Interval.Duration)
	}
	if *cfg.DryRun != *reloaded.DryRun {
		t.Error("dryRun changed across a round trip")
	}
}

func TestExplicitEmptyNamespaceOverrideSurvivesRoundTrip(t *testing.T) {
	// `namespaces: []` on a pool means "clear the global exclusions for this
	// pool", which is different from omitting the field. A plain slice cannot
	// carry that across Marshal: omitempty drops an empty slice, and the
	// reloaded document silently inherits the global list again — changing the
	// pool's resolved policy without anyone touching it.
	cfg := mustLoad(t, `
policy:
  exclusions:
    namespaces: [kube-system, monitoring]
pools:
  - name: pool-4g
    exclusions:
      namespaces: []
`)

	if got := cfg.PolicyFor("pool-4g").ExcludedNamespaces; len(got) != 0 {
		t.Fatalf("explicit empty list should clear exclusions, got %v", got)
	}

	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "namespaces: []") {
		t.Errorf("marshalled document must keep the explicit empty list:\n%s", out)
	}

	reloaded, err := Load(out)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got := reloaded.PolicyFor("pool-4g").ExcludedNamespaces; len(got) != 0 {
		t.Errorf("round trip restored the global exclusions: %v", got)
	}
}

func TestOmittedNamespacesStillInherit(t *testing.T) {
	// The other half of the distinction: omitting the field entirely must
	// still inherit, or the pointer change would have broken inheritance.
	cfg := mustLoad(t, `
policy:
  exclusions:
    namespaces: [kube-system]
pools:
  - name: pool-4g
    drain:
      maxPodsPerDrain: 3
`)
	if got := cfg.PolicyFor("pool-4g").ExcludedNamespaces; len(got) != 1 || got[0] != "kube-system" {
		t.Errorf("omitted namespaces should inherit, got %v", got)
	}
}

func TestDurationMarshalsAsString(t *testing.T) {
	cfg := mustLoad(t, "interval: 90s")
	out, err := Marshal(cfg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(out), "interval: 1m30s") {
		t.Errorf("duration should marshal as a string, got:\n%s", out)
	}
}

// The `discovery.nodeGroups` tests below cover the one escape hatch binpack
// has where neither join works: the identifier is not a legal label value and
// is not built from anything a label carries. Membership only — a pool
// minimum stated here and enforced by the autoscaler is two numbers that can
// disagree, which is why ADR-0004's second resolution mode was withdrawn
// rather than built.

func TestAStatedJoinIsLoadedAsWritten(t *testing.T) {
	cfg := mustLoad(t, `
apiVersion: binpack.motleyhand.com/v1alpha1
kind: BinpackConfig
discovery:
  nodeGroupIDLabel: eks.amazonaws.com/nodegroup
  nodeGroups:
    - labelValue: workers
      group: eks-workers-a2c1d3e4-1111
    - labelValue: system
      group: eks-system-b7f10c2d-4444
`)

	want := map[string]string{
		"workers": "eks-workers-a2c1d3e4-1111",
		"system":  "eks-system-b7f10c2d-4444",
	}
	if got := cfg.NodeGroupJoin(); !reflect.DeepEqual(got, want) {
		t.Errorf("NodeGroupJoin() = %v, want %v", got, want)
	}
}

func TestAStatedJoinIsRejectedWhenItCannotBeActedOn(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{
			// Nothing to join from. An entry with no label value silently
			// claims every unlabelled node, which is the mapping being wrong
			// in the direction that costs a node.
			name: "no label value",
			yaml: "  nodeGroups:\n    - group: eks-workers-a2c1\n",
			want: "discovery.nodeGroups[0].labelValue: must not be empty",
		},
		{
			// And nothing to join to. An empty group is what the status
			// document writes for a group with no name, so this would map a
			// node onto a pool that is not there.
			name: "no group",
			yaml: "  nodeGroups:\n    - labelValue: workers\n",
			want: "discovery.nodeGroups[0].group: must not be empty",
		},
		{
			// A value can only be in one pool. Loading both and keeping
			// whichever came last would put the nodes somewhere the document
			// does not obviously say.
			name: "the same label value twice",
			yaml: "  nodeGroups:\n    - labelValue: workers\n      group: eks-a-1111\n" +
				"    - labelValue: workers\n      group: eks-b-2222\n",
			want: "discovery.nodeGroups[1].labelValue: \"workers\" already joined at " +
				"discovery.nodeGroups[0]",
		},
		{
			// Not a label value at all, so no node can ever carry it: this
			// entry would sit there doing nothing.
			name: "a label value no node could carry",
			yaml: "  nodeGroups:\n    - labelValue: \"a/b\"\n      group: eks-a-1111\n",
			want: "discovery.nodeGroups[0].labelValue: \"a/b\" is not a valid label value",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load([]byte("apiVersion: binpack.motleyhand.com/v1alpha1\n" +
				"kind: BinpackConfig\ndiscovery:\n" + tc.yaml))
			if err == nil {
				t.Fatal("a join binpack cannot act on was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not say %q:\n%v", tc.want, err)
			}
		})
	}
}

func TestNoStatedJoinIsNoJoinRatherThanAnEmptyOne(t *testing.T) {
	// Distinguished because engine.Config.NodeGroups being non-empty is what
	// makes a mapping "configured", and an empty map arriving from a document
	// that says nothing would be a claim binpack never made.
	if got := mustLoad(t, "").NodeGroupJoin(); got != nil {
		t.Errorf("NodeGroupJoin() = %v on a document that states none", got)
	}
}

// TestRemovalTimeoutCoversTheAutoscalersOwnDelays pins the one bound whose
// deadline is set by a component binpack does not control.
//
// removalTimeout is how long binpack waits for the cluster-autoscaler to
// delete a node it has emptied, and the autoscaler's own arithmetic decides
// when that can happen: the node must have been unneeded for
// scale-down-unneeded-time, and a scale-up anywhere in the cluster suppresses
// all scale-down for scale-down-delay-after-add on top of that — one
// cluster-wide gate, since scale-down-delay-type-local defaults to false. A
// default shorter than the sum abandons a drain the autoscaler was going to
// finish: the pods moved for nothing, the consolidation is lost, and a node
// that did nothing wrong collects backoff.
//
// The floor, not the value. The default carries headroom over it for the
// recheck timeout, and the assertion deliberately does not pin that — what
// must not happen is the number drifting back under the sum.
func TestRemovalTimeoutCoversTheAutoscalersOwnDelays(t *testing.T) {
	floor := DefaultScaleDownUnneededTime + DefaultCooldownAfterScaleUp
	if DefaultRemovalTimeout < floor {
		t.Errorf("DefaultRemovalTimeout = %s, want at least %s "+
			"(scale-down-unneeded-time %s + scale-down-delay-after-add %s)",
			DefaultRemovalTimeout, floor,
			DefaultScaleDownUnneededTime, DefaultCooldownAfterScaleUp)
	}
}

// TestAutoscalerPolicyIsWhatADocumentSays is the half of TestDrainableEvictConfig
// that failed: the engine has always read EvictConfig, and until now no
// configuration document could produce one that differed from the built-in
// literal. An operator whose autoscaler runs --skip-nodes-with-local-storage=false
// — the AKS default — had no way to say so.
func TestAutoscalerPolicyIsWhatADocumentSays(t *testing.T) {
	t.Run("an empty document mirrors the autoscaler's own defaults", func(t *testing.T) {
		p := mustLoad(t, "").PolicyFor("any-pool")

		if !p.SkipNodesWithLocalStorage || !p.SkipNodesWithSystemPods {
			t.Errorf("skip flags = %v/%v, want both true: upstream defaults both to true",
				p.SkipNodesWithLocalStorage, p.SkipNodesWithSystemPods)
		}
		if p.BlockingSystemPodDistruptionTimeout != DefaultBlockingSystemPodDistruptionTimeout {
			t.Errorf("blockingSystemPodDistruptionTimeout = %s, want %s",
				p.BlockingSystemPodDistruptionTimeout, DefaultBlockingSystemPodDistruptionTimeout)
		}
	})

	cfg := mustLoad(t, `
apiVersion: binpack.motleyhand.com/v1alpha1
kind: BinpackConfig
policy:
  autoscaler:
    skipNodesWithLocalStorage: false
    blockingSystemPodDistruptionTimeout: 0s
pools:
  - name: pool-4g
    autoscaler:
      skipNodesWithSystemPods: false
`)

	t.Run("explicit false is honoured, not read as unset", func(t *testing.T) {
		// The reason every field on the wire is a pointer: false is the value
		// an operator writes here, and a plain bool cannot tell it from a
		// field nobody mentioned.
		p := cfg.PolicyFor("pool-other")
		if p.SkipNodesWithLocalStorage {
			t.Error("skipNodesWithLocalStorage: false must resolve to false")
		}
		if !p.SkipNodesWithSystemPods {
			t.Error("skipNodesWithSystemPods was not set globally and must still be true")
		}
	})

	t.Run("zero is a grace of none rather than an absent field", func(t *testing.T) {
		// How an operator says their autoscaler is older than 1.33, where a
		// blocking system pod blocks for as long as it is there. Unset would
		// give them the one-hour grace their autoscaler does not have.
		//
		// That the document above loaded at all is half the assertion: Load
		// validates, and the neighbouring durations all refuse a zero. This
		// one must not, because here zero is a supported autoscaler rather
		// than an unbounded wait.
		if got := cfg.PolicyFor("pool-other").BlockingSystemPodDistruptionTimeout; got != 0 {
			t.Errorf("blockingSystemPodDistruptionTimeout = %s, want 0", got)
		}
	})

	t.Run("a pool override wins field by field", func(t *testing.T) {
		p := cfg.PolicyFor("pool-4g")
		if p.SkipNodesWithSystemPods {
			t.Error("the pool sets skipNodesWithSystemPods: false and must resolve to false")
		}
		if p.SkipNodesWithLocalStorage {
			t.Error("the pool overrides one field; the global false must still stand for the other")
		}
	})
}
