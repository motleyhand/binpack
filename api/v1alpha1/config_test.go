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
