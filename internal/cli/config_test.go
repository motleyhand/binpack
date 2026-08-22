package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
}
