package cli

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The surfaces a stranger meets: the page GitHub renders, the two references
// and the how-to they reach from it, and the notes Helm prints at the end of
// every install.
//
// They live here rather than beside the chart or the docs because this is the
// package that already reads both — see chart_test.go and
// diagnostics_doc_test.go — and because a doc test needs a package that
// compiles without a cluster.
var userFacingSurfaces = []string{
	"../../README.md",
	"../../docs/how-to/install-binpack.md",
	"../../docs/reference/configuration.md",
	"../../docs/reference/rbac.md",
	"../../charts/binpack/templates/NOTES.txt",
}

// Sentences that were true before the executor shipped and have been false in
// every tagged release since.
//
// A phrase list rather than a pattern for "status banner", deliberately. The
// broad version of this test matches docs/reference/versioning.md, whose 0.x
// policy — the version stays at 0.x until binpack has acted on somebody
// else's cluster — is a policy that still holds, and the quick-wins page,
// which tells a reader they might not need binpack at all. Both should keep
// saying what they say. What must not come back is a claim that this build
// cannot do what it does.
var deniesActing = []string{
	"nothing is installable",
	"not yet used",
	"nothing in this build cordons",
	"until a build that can act",
	"this build never writes those",
	"until the controller lands",
}

func TestNoDocClaimsBinpackCannotAct(t *testing.T) {
	for _, path := range userFacingSurfaces {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		doc := flatten(string(data))

		for _, claim := range deniesActing {
			if strings.Contains(doc, claim) {
				t.Errorf("%s says %q, and binpack cordons nodes and evicts pods "+
					"whenever dryRun is false", path, claim)
			}
		}
	}
}

// TestTheRBACReferenceMatchesWhatTheExecutorDoes checks the third side of an
// agreement the chart and the code already keep between them.
//
// An operator managing RBAC outside the chart — rbac.create: false, which the
// chart supports on purpose — writes their role from the reference, and the
// chart's render-time refusal cannot see what they granted. So a verb the
// executor needs that the page files under "not needed" is a role missing it,
// and a drain that 403s on its first node patch: binpack holds its lease,
// serves its metrics, publishes decisions, and never drains anything.
func TestTheRBACReferenceMatchesWhatTheExecutorDoes(t *testing.T) {
	chart, err := os.ReadFile("../../charts/binpack/templates/rbac.yaml")
	if err != nil {
		t.Fatalf("reading the chart's RBAC: %v", err)
	}

	// The rules gated on rbac.allowDraining: what internal/executor needs, as
	// the chart grants it. Taken from the chart rather than listed here, so
	// the day a third verb is added the reference has to say so too.
	_, rest, ok := strings.Cut(string(chart), "{{- if .Values.rbac.allowDraining }}")
	if !ok {
		t.Fatal("the chart no longer gates the act rules on rbac.allowDraining")
	}
	act, _, ok := strings.Cut(rest, "{{- end }}")
	if !ok {
		t.Fatal("the chart's act rules are not a closed block")
	}

	granted := rulePairs(act)
	if len(granted) == 0 {
		t.Fatal("no act rules found in the chart; this test would assert nothing")
	}

	data, err := os.ReadFile("../../docs/reference/rbac.md")
	if err != nil {
		t.Fatalf("reading the RBAC reference: %v", err)
	}
	documented := rulePairs(grantedSections(string(data)))

	for pair := range granted {
		if !documented[pair] {
			t.Errorf("the chart grants %q to let binpack act, and the RBAC reference "+
				"does not list it outside a section marked unused", pair)
		}
	}
}

// grantedSections drops the sections of a document whose heading says the
// permissions in them are not granted or not needed, so what remains is what
// the page tells an operator to grant.
func grantedSections(doc string) string {
	var kept []string
	for i, part := range strings.Split(doc, "\n## ") {
		heading, _, _ := strings.Cut(part, "\n")
		if i > 0 && notGranted.MatchString(heading) {
			continue
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, "\n")
}

var notGranted = regexp.MustCompile(`(?i)not yet|unused|not needed|never granted`)

// rulePairs pulls `resource: verb` pairs out of the RBAC rule blocks in a
// document. Both the chart and the reference write a rule as adjacent lines:
//
//	resources: [nodes]
//	verbs: [patch]
func rulePairs(doc string) map[string]bool {
	pairs := map[string]bool{}
	var resources []string
	for _, line := range strings.Split(doc, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "resources:"):
			resources = bracketed(line)
		case strings.HasPrefix(line, "verbs:"):
			for _, resource := range resources {
				for _, verb := range bracketed(line) {
					pairs[resource+": "+verb] = true
				}
			}
			resources = nil
		}
	}
	return pairs
}

// bracketed reads the inline sequence `key: [a, b]`.
func bracketed(line string) []string {
	open := strings.Index(line, "[")
	shut := strings.LastIndex(line, "]")
	if open < 0 || shut < open {
		return nil
	}
	var out []string
	for _, item := range strings.Split(line[open+1:shut], ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

var whitespace = regexp.MustCompile(`\s+`)

// flatten lower-cases a document and collapses every run of whitespace to one
// space.
//
// Both halves are load-bearing rather than tidiness. These pages wrap at 95
// columns, so "and this build\nnever writes those" is one sentence split
// across two lines and a search for the unwrapped form finds nothing —
// a test that passes against the exact page it was written to catch. The
// claim about cordoning likewise appears capitalised in one place on the
// RBAC page and lower-cased in another.
func flatten(doc string) string {
	return whitespace.ReplaceAllString(strings.ToLower(doc), " ")
}
