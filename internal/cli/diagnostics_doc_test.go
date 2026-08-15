package cli

import (
	"os"
	"strings"
	"testing"

	"github.com/motleyhand/binpack/internal/engine"
)

// The reference lives here rather than beside the engine because the engine
// may not touch the filesystem — see ADR-0008 — and because the CLI is what
// prints these codes at people and points them at the document.
const diagnosticsReference = "../../docs/reference/diagnostics.md"

func TestEveryDiagnosticCodeIsDocumented(t *testing.T) {
	// A hand-written reference drifts from the code it describes; this is the
	// check that stops it. Adding a diagnosis without documenting it fails
	// here rather than shipping a report pointing at a document that does not
	// mention what it just printed.
	data, err := os.ReadFile(diagnosticsReference)
	if err != nil {
		t.Fatalf("reading the diagnostics reference: %v", err)
	}
	doc := string(data)

	for _, d := range engine.Diagnoses() {
		heading := "### `" + d.Code + "` — " + d.Severity.String()
		if !strings.Contains(doc, heading) {
			t.Errorf("%s is not documented, or its severity there disagrees: want a heading %q",
				d.Code, heading)
		}
	}
}

func TestTheReferenceDocumentsNothingThatDoesNotExist(t *testing.T) {
	data, err := os.ReadFile(diagnosticsReference)
	if err != nil {
		t.Fatalf("reading the diagnostics reference: %v", err)
	}

	known := map[string]bool{}
	for _, d := range engine.Diagnoses() {
		known[d.Code] = true
	}

	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "### `") {
			continue
		}
		code := strings.SplitN(strings.TrimPrefix(line, "### `"), "`", 2)[0]
		if !known[code] {
			t.Errorf("the reference documents %q, which binpack cannot report — "+
				"renamed or removed without updating the document", code)
		}
	}
}
