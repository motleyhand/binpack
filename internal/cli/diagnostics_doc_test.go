package cli

import (
	"os"
	"path/filepath"
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

// jobNeedsAPolicy is the caveat that has to travel with any recommendation of
// a Job, in the words both the printed fix and the reference use.
//
// The rule and not merely its name: "podFailurePolicy" alone would be
// satisfied by a document that mentions the feature and shows nothing, and
// what makes a Job drain-safe is this specific pair — ignore the condition an
// eviction stamps, so the disruption does not spend the failure budget.
//
// Spelled out as literals rather than interpolated from the fix, because the
// point is that two texts say the same thing: a guard that read the fix and
// then looked for the fix in the document would agree with any rewording of
// both.
var jobNeedsAPolicy = []string{
	"podFailurePolicy",
	"action: Ignore",
	"DisruptionTarget",
	// Upstream permits podFailurePolicy only here (batch validation), so a
	// recommendation omitting it is one the reader cannot apply.
	"restartPolicy: Never",
}

// TestTheBarePodFixDoesNotRecommendAnUnprotectedJob settles a limitation
// binpack cannot check for.
//
// `bare-pod` is the guard against a pod nothing recreates, and its premise is
// that a controlling owner means a replacement is coming. A Job is the one
// controller kind that stops recreating: an API eviction deletes the pod, the
// Job controller counts a deleted pod as a failure, and once `.status.failed`
// passes `.spec.backoffLimit` — 0 for a migration or a Helm hook — the Job
// goes terminally Failed and creates nothing. So the remedy printed for
// `bare-pod` sends the next operator to the very controller kind with this
// hazard, and binpack will relocate a Job pod as readily as a Deployment's.
//
// The remedy that holds is a `podFailurePolicy` ignoring the
// `DisruptionTarget` condition the eviction subresource stamps, which upstream
// documents as the way to make batch work drain-safe.
func TestTheBarePodFixDoesNotRecommendAnUnprotectedJob(t *testing.T) {
	var fix string
	for _, d := range engine.Diagnoses() {
		if d.Code == engine.BlockedBarePod {
			fix = d.Fix
		}
	}
	if fix == "" {
		t.Fatal("bare-pod has no fix text, so this asserts nothing")
	}

	data, err := os.ReadFile(diagnosticsReference)
	if err != nil {
		t.Fatalf("reading the diagnostics reference: %v", err)
	}
	section := sectionOf(t, string(data), engine.BlockedBarePod)

	// Both surfaces, because they are one recommendation printed twice: the
	// fix is what `binpack diagnose` says and the section is what it sends
	// the reader to.
	for surface, text := range map[string]string{
		"the printed fix":                  fix,
		"the reference's bare-pod section": section,
	} {
		if !strings.Contains(text, "Job") {
			continue
		}
		for _, want := range jobNeedsAPolicy {
			if strings.Contains(text, want) {
				continue
			}
			t.Errorf("%s recommends a Job as the way to make a pod safe to evict without "+
				"%q, so it is not the policy that makes one safe: an eviction spends the "+
				"Job's failure budget, and a Job at its limit creates no replacement",
				surface, want)
		}
	}
}

// sectionOf returns one `### `code“ section of the reference, so an assertion
// about one diagnosis is not answered by a sentence about another.
func sectionOf(t *testing.T, doc, code string) string {
	t.Helper()
	_, after, found := strings.Cut(doc, "### `"+code+"`")
	if !found {
		t.Fatalf("the reference has no section for %s", code)
	}
	section, _, _ := strings.Cut(after, "\n### ")
	if !strings.Contains(section, "**Fix.**") {
		t.Fatalf("the %s section carries no fix, so this asserts nothing:\n%s", code, section)
	}
	return section
}

// zeroDisruptionsCaveat is what every document recommending `kubectl get pdb
// -A` has to say about the rows it turns up, in the same words in all three.
//
// A budget whose selector matches nothing also reports zero: upstream forces
// it, so that a budget which currently matches no pods is in a safe state when
// its first pods appear. Such a row pins nothing, and binpack's own
// `pdb-selects-nothing` finding says so — three documents telling the reader
// otherwise is the tool contradicting itself.
const zeroDisruptionsCaveat = "selects no pods"

// unqualifiedAnchorClaims are the sentences that made the zero row sufficient
// on its own. Each came from one of the three documents and all three are
// checked in every one of them, because the failure mode is fixing two.
var unqualifiedAnchorClaims = []string{
	"is a node anchor",
	"prevents its node from ever being drained",
	"is pinning its node",
}

// zeroDisruptionsSources are the documents that recommend the command. All
// three present it as a complete test, so all three have to carry the caveat.
var zeroDisruptionsSources = []string{
	"../../docs/explanation/the-poddisruptionbudget-that-costs-money.md",
	"../../docs/how-to/quick-wins-before-installing-binpack.md",
	"../../docs/how-to/diagnose-scale-down-blockers.md",
}

func TestTheZeroDisruptionsCheckIsQualifiedWhereverItIsRecommended(t *testing.T) {
	for _, source := range zeroDisruptionsSources {
		t.Run(filepath.Base(source), func(t *testing.T) {
			data, err := os.ReadFile(source)
			if err != nil {
				t.Fatalf("reading %s: %v", source, err)
			}
			doc := string(data)

			// Guards the guard: a document that stopped recommending the
			// command would pass everything below by saying nothing.
			if !strings.Contains(doc, "ALLOWED DISRUPTIONS: 0") {
				t.Fatalf("%s no longer shows a zero row, so this asserts nothing", source)
			}
			if !strings.Contains(doc, zeroDisruptionsCaveat) {
				t.Errorf("%s presents every zero row as an anchor: a budget matching no pods "+
					"reports zero too and pins nothing, which is a different problem and a "+
					"real one", source)
			}
			for _, claim := range unqualifiedAnchorClaims {
				if strings.Contains(doc, claim) {
					t.Errorf("%s still says a zero row %q with no qualification", source, claim)
				}
			}
		})
	}
}
