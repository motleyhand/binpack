package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	cmd := NewRootCommand(&buf)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return buf.String(), err
}

func TestVersionText(t *testing.T) {
	out, err := run(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}

	// Every field must be populated. An unstamped build still reports "dev"
	// and falls back to the VCS metadata the toolchain embeds, so an empty
	// value means the fallback in version.Get broke.
	for _, want := range []string{"binpack ", "commit:", "built:", "go:", "platform:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if strings.HasSuffix(line, ":") {
			t.Errorf("field has empty value: %q", line)
		}
	}
}

func TestVersionJSON(t *testing.T) {
	out, err := run(t, "version", "--output", "json")
	if err != nil {
		t.Fatalf("version --output json: %v", err)
	}

	var got map[string]string
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}

	for _, key := range []string{"version", "commit", "date", "goVersion", "platform"} {
		if got[key] == "" {
			t.Errorf("field %q is empty in %v", key, got)
		}
	}
}

func TestInvalidOutputFormat(t *testing.T) {
	_, err := run(t, "version", "--output", "yaml")
	if err == nil {
		t.Fatal("expected an error for --output yaml, got none")
	}
	if !strings.Contains(err.Error(), "invalid --output") {
		t.Errorf("error should name the offending flag, got: %v", err)
	}
}

func TestVersionRejectsArgs(t *testing.T) {
	if _, err := run(t, "version", "extra"); err == nil {
		t.Fatal("expected an error for an unexpected argument, got none")
	}
}
