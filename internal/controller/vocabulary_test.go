package controller

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/motleyhand/binpack/internal/mother"
)

const referenceDir = "../../docs/reference"

// TestEveryEventReasonIsDocumented is the third closed vocabulary binpack
// publishes and the last one nothing held to a reference page.
//
// The metric label values have had a doc guard since the reference existed and
// the diagnosis codes have had one in both directions. Event reasons had
// neither, and the set has grown from five to eight in one release — all of it
// documented, and all of it in task-shaped how-to pages that describe one
// scenario each, so that no single place listed the set. An operator writing
// `kubectl get events --field-selector reason=…` was reading a list assembled
// from two guides written for other purposes.
//
// Scoped to docs/reference/ rather than to one file on purpose: what this
// asserts is that the vocabulary has a look-up home, not which page it is on.
func TestEveryEventReasonIsDocumented(t *testing.T) {
	reference := referenceCorpus(t)

	for _, name := range append(EventReasons(), ActionConsolidate) {
		if !strings.Contains(reference, "`"+name+"`") {
			t.Errorf("the Event %q is written onto nodes and no reference page names it: "+
				"an operator filtering events has nowhere to look the set up", name)
		}
	}
}

// referenceCorpus is every reference page, concatenated.
func referenceCorpus(t *testing.T) string {
	t.Helper()

	entries, err := os.ReadDir(referenceDir)
	if err != nil {
		t.Fatalf("reading %s: %v", referenceDir, err)
	}

	var b strings.Builder
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(referenceDir, entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}
		b.Write(data)
	}
	if b.Len() == 0 {
		t.Fatalf("no reference pages found under %s", referenceDir)
	}
	return b.String()
}

// TestTheDrainLogLineNamesThePoolWhenOnlyTheIdentifierIsPresent is one half of
// SSOT-02, and it is the half nobody would have noticed on the platform the
// defaults were written for.
//
// "readable pool name, else the identifier" was implemented three times — in
// engine.displayPool, in engine.poolLabel, and inline in metrics.observePools
// — and omitted twice, here and in explain's node report. On DOKS both labels
// are present and everything agrees. On EKS, AKS and most GKE installs only
// the identifier is, so within one evaluation the metric named the pool by its
// identifier and this line logged the empty string, leaving nothing to join a
// dashboard series to the log entry that recorded the drain.
func TestTheDrainLogLineNamesThePoolWhenOnlyTheIdentifierIsPresent(t *testing.T) {
	var log captured
	rec := &fakeRecorder{}

	// Only the identifier label, which is the ordinary case everywhere the
	// provider does not also publish a readable pool name.
	unnamed := func(name string) *corev1.Node {
		return mother.LargeNode(name, mother.NodeLabels(map[string]string{
			"doks.digitalocean.com/node-pool-id": poolID,
		}))
	}

	ev := newEvaluator(t, &log, rec,
		unnamed("a"), unnamed("b"), unnamed("c"),
		mother.Pod("default", "web", mother.OnNode("a")),
		statusConfigMap(),
	)

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluating: %v", err)
	}

	if !log.contains(`"pool"="` + poolID + `"`) {
		t.Errorf("the drain line does not name the pool by its identifier: %v", log.lines)
	}
}

// TestEveryEventReasonIsDistinct is internal/metrics'
// TestEveryPublishedVocabularyIsASet for the one published vocabulary that
// file cannot reach.
//
// Same hazard, same cost: the guard above iterates EventReasons() and compares
// it with the reference, so two constants sharing a value collapse to one
// entry that both sides agree about, and an operator filtering events on the
// reason cannot tell the two apart. Separate because internal/metrics may not
// import this package.
func TestEveryEventReasonIsDistinct(t *testing.T) {
	reasons := EventReasons()
	if len(reasons) == 0 {
		t.Fatal("EventReasons() enumerates nothing, so this asserts nothing about it")
	}

	seen := map[string]bool{}
	for _, reason := range reasons {
		// An empty reason is an Event nobody can filter on, and the guard
		// above would look for `` in the reference — which every Markdown code
		// fence satisfies. Unlike the metric label values, nothing downstream
		// refuses this one: an Event carries whatever reason it is given.
		if reason == "" {
			t.Error("EventReasons() enumerates the empty string; the Events written with " +
				"it carry no reason, and `kubectl get events --field-selector reason=` " +
				"has nothing to match")
		}
		if seen[reason] {
			t.Errorf("EventReasons() enumerates %q twice, so two constants share it and "+
				"the events they were meant to distinguish read as one", reason)
		}
		seen[reason] = true
	}
}

// reasonSources is where the Event reason constants are declared. A glob
// rather than a file, for the reason internal/cli's conventions guard uses
// one: a split moves the declarations and a named file stops seeing them.
const reasonSources = "*.go"

// TestEveryEventReasonDeclaredIsEnumerated is the direction the guard above
// cannot give.
//
// That one proves EventReasons() ⊆ the reference, so it visits whatever the
// enumerator holds and has no opinion about what it does not. Dropping
// ReasonWouldDrain from the enumerator and from the pages leaves report.go
// emitting it and leaves its fixtures green — they compare against the same
// constant — while the loop simply stops visiting it. The reason is then
// written onto nodes and documented nowhere, with the suite passing.
//
// Read from the package's own declarations rather than by driving every
// reporting path. The constants are what the emitting code names, so a
// constant that exists and is not enumerated is the whole defect; driving the
// paths would test the same thing through more machinery, and would go quiet
// for a branch no fixture reaches — which is exactly the state this is meant
// to catch.
func TestEveryEventReasonDeclaredIsEnumerated(t *testing.T) {
	enumerated := map[string]bool{}
	for _, reason := range EventReasons() {
		enumerated[reason] = true
	}

	sources, err := filepath.Glob(reasonSources)
	if err != nil {
		t.Fatalf("globbing %s: %v", reasonSources, err)
	}

	fset := token.NewFileSet()
	var declared int
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, source, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", source, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok || len(spec.Names) != len(spec.Values) {
				return true
			}
			for i, name := range spec.Names {
				if !strings.HasPrefix(name.Name, "Reason") {
					continue
				}
				lit, ok := spec.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				value, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				declared++
				if !enumerated[value] {
					t.Errorf("%s = %q is declared here and EventReasons() does not enumerate "+
						"it; the Events carrying it are written onto nodes, and nothing "+
						"holds the reference to a vocabulary it is not in", name.Name, value)
				}
			}
			return true
		})
	}

	// A glob no file answers returns an empty list and a nil error, so a
	// package that moves reads as one declaring nothing.
	if declared != len(EventReasons()) {
		t.Errorf("found %d Reason constants and EventReasons() holds %d; either the parse "+
			"has stopped seeing the declarations or the enumerator holds a value no "+
			"constant does", declared, len(EventReasons()))
	}
}
