package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/motleyhand/binpack/api/v1alpha1"
)

func runWithStdin(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	cmd := NewRootCommand(&buf)
	cmd.SetArgs(args)
	cmd.SetIn(strings.NewReader(stdin))
	err := cmd.Execute()
	return buf.String(), err
}

func TestConfigValidateEmptyStdin(t *testing.T) {
	out, err := runWithStdin(t, "", "config", "validate")
	if err != nil {
		t.Fatalf("empty configuration should be valid: %v", err)
	}

	for _, want := range []string{"configuration is valid", "dry run:   true", "interval:  1m0s"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}
}

func TestConfigValidateShowsResolvedOverrides(t *testing.T) {
	out, err := runWithStdin(t, `
policy:
  drain:
    maxPodsPerDrain: 10
pools:
  - name: pool-8g
    enabled: false
`, "config", "validate")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}

	if !strings.Contains(out, `override for pool "pool-8g"`) {
		t.Errorf("overrides should be shown:\n%s", out)
	}
	// The override must be shown resolved, so an operator sees the effect
	// rather than the sparse document that produced it.
	if !strings.Contains(out, "enabled:                 false") {
		t.Errorf("override should resolve to disabled:\n%s", out)
	}
	if !strings.Contains(out, "max pods per drain:      10") {
		t.Errorf("override should inherit the global maxPodsPerDrain:\n%s", out)
	}
}

func TestConfigValidateFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "binpack.yaml")
	if err := os.WriteFile(path, []byte("interval: 45s\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := runWithStdin(t, "", "config", "validate", "-f", path)
	if err != nil {
		t.Fatalf("validate -f: %v", err)
	}
	if !strings.Contains(out, "interval:  45s") {
		t.Errorf("file contents should be used:\n%s", out)
	}
}

func TestConfigValidateJSON(t *testing.T) {
	out, err := runWithStdin(t, "interval: 45s", "config", "validate", "--output", "json")
	if err != nil {
		t.Fatalf("validate --output json: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if got["interval"] != "45s" {
		t.Errorf("interval = %v, want \"45s\" as a string", got["interval"])
	}
	// Defaults must be materialised, so the JSON shows what will actually
	// be used rather than echoing a sparse input.
	if got["dryRun"] != true {
		t.Errorf("defaults should appear in the output, got %v", got)
	}
}

func TestConfigValidateReportsErrors(t *testing.T) {
	_, err := runWithStdin(t, "interval: 1s\npools:\n  - name: \"\"", "config", "validate")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"below the minimum", "must not be empty"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestConfigValidateMissingFile(t *testing.T) {
	_, err := runWithStdin(t, "", "config", "validate", "-f", "/nonexistent/binpack.yaml")
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if !strings.Contains(err.Error(), "/nonexistent/binpack.yaml") {
		t.Errorf("error should name the file, got: %v", err)
	}
}

// TestConfigValidateSaysWhereTheAutoscalerStatusIsRead is the read-only
// command that answers "where will binpack look?" before anything has looked.
//
// It matters more than the two label keys beside it. A wrong label key fails
// preflight loudly; a wrong namespace produces a confident report that no
// cluster-autoscaler is running, which reads as a fact about the cluster. The
// summary prints resolved values, so this also shows an operator who set
// nothing which namespace the default picked for them.
func TestConfigValidateSaysWhereTheAutoscalerStatusIsRead(t *testing.T) {
	out, err := runWithStdin(t, "", "config", "validate")
	if err != nil {
		t.Fatalf("empty configuration should be valid: %v", err)
	}
	if !strings.Contains(out, "kube-system") {
		t.Errorf("the summary does not say where the autoscaler's status is read from:\n%s", out)
	}

	out, err = runWithStdin(t, "discovery:\n  autoscalerNamespace: autoscaler\n",
		"config", "validate")
	if err != nil {
		t.Fatalf("config validate: %v", err)
	}
	if !strings.Contains(out, "autoscaler") || strings.Contains(out, "kube-system") {
		t.Errorf("the summary does not report the configured namespace:\n%s", out)
	}

	// And the other half of the location, which is configuration too. A
	// summary that prints the default name whatever was set sends an operator
	// to `kubectl describe` an object binpack is not reading — the same defect
	// as the namespace, on the field beside it.
	out, err = runWithStdin(t, "discovery:\n  autoscalerStatusName: my-ca-status\n",
		"config", "validate")
	if err != nil {
		t.Fatalf("config validate: %v", err)
	}
	if !strings.Contains(out, "my-ca-status") {
		t.Errorf("the summary does not report the configured status object name:\n%s", out)
	}
	if strings.Contains(out, "cluster-autoscaler-status") {
		t.Errorf("the summary prints the default name over the configured one:\n%s", out)
	}
}

// TestConfigValidateRendersEveryResolvedField is the reporting half of the seam
// TestEnginePolicyCarriesEveryResolvedPolicyField holds for the acting half.
//
// `binpack config validate` exists to answer "what will actually be used", and
// the configuration reference sends operators to it for exactly that. A field
// it omits is one the schema accepts, the defaults fill in and the engine acts
// on — which an operator then cannot confirm, and which reads identically
// whether they set it or not. Both renderings are asserted, because JSON and
// text are written separately and either can be forgotten alone.
//
// Distinct values throughout, so a field rendered from its neighbour fails as
// loudly as one rendered from nothing.
func TestConfigValidateRendersEveryResolvedField(t *testing.T) {
	resolved := v1alpha1.PoolPolicy{
		Enabled:                             true,
		ExpendablePriorityCutoff:            -7,
		ReserveForLargestPod:                true,
		SkipNodesWithLocalStorage:           false,
		SkipNodesWithSystemPods:             true,
		BlockingSystemPodDistruptionTimeout: 90 * time.Minute,
		MaxPodsPerDrain:                     3,
		StallTimeout:                        11 * time.Minute,
		RemovalTimeout:                      17 * time.Minute,
		BackoffInitial:                      5 * time.Minute,
		BackoffMax:                          72 * time.Hour,
		CooldownAfterScaleUp:                13 * time.Minute,
		CooldownAfterDrain:                  19 * time.Minute,
		ExcludedNamespaces:                  []string{"kube-system"},
	}

	// Counted from the struct rather than taken on trust, the same guard the
	// engine seam carries — and the one this renderer did not have, which is
	// how three fields reached the engine while `config validate` stayed
	// silent about them.
	const rendered = 14
	if n := reflect.TypeFor[v1alpha1.PoolPolicy]().NumField(); n != rendered {
		t.Fatalf("PoolPolicy has %d fields and this test asserts %d: render the new one "+
			"in both viewOf and writePolicy and assert it here, or say here why it is not "+
			"rendered", n, rendered)
	}

	t.Run("json", func(t *testing.T) {
		encoded, err := json.Marshal(viewOf(resolved))
		if err != nil {
			t.Fatalf("marshalling the view: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatalf("decoding the view: %v", err)
		}

		for _, tc := range []struct {
			key  string
			want any
		}{
			{"enabled", true},
			{"expendablePriorityCutoff", float64(-7)},
			{"reserveForLargestPod", true},
			{"skipNodesWithLocalStorage", false},
			{"skipNodesWithSystemPods", true},
			{"blockingSystemPodDistruptionTimeout", "1h30m0s"},
			{"maxPodsPerDrain", float64(3)},
			{"stallTimeout", "11m0s"},
			{"removalTimeout", "17m0s"},
			{"backoffInitial", "5m0s"},
			{"backoffMax", "72h0m0s"},
			{"cooldownAfterScaleUp", "13m0s"},
			{"cooldownAfterDrain", "19m0s"},
			{"excludedNamespaces", []any{"kube-system"}},
		} {
			v, ok := got[tc.key]
			if !ok {
				t.Errorf("%s is missing from the JSON rendering", tc.key)
				continue
			}
			if !reflect.DeepEqual(v, tc.want) {
				t.Errorf("%s = %v, want the configured %v", tc.key, v, tc.want)
			}
		}
	})

	t.Run("text", func(t *testing.T) {
		var buf bytes.Buffer
		writePolicy(func(format string, args ...any) {
			fmt.Fprintf(&buf, format, args...)
		}, resolved)
		out := buf.String()

		// The values, not the labels: prose is not public and a label may be
		// reworded, but a number an operator cannot find is the defect.
		for _, want := range []string{
			"priority -7", "3", "11m0s", "17m0s", "5m0s", "72h0m0s", "13m0s", "19m0s",
			"kube-system", "1h30m0s",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%q does not appear in the text rendering:\n%s", want, out)
			}
		}
		// The two skip flags differ, so a crossed pair cannot read as correct.
		if !strings.Contains(out, "local storage") || !strings.Contains(out, "system pods") {
			t.Errorf("the autoscaler's two skip flags are not rendered:\n%s", out)
		}
	})
}
