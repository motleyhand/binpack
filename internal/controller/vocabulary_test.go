package controller

import (
	"context"
	"os"
	"path/filepath"
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
		if seen[reason] {
			t.Errorf("EventReasons() enumerates %q twice, so two constants share it and "+
				"the events they were meant to distinguish read as one", reason)
		}
		seen[reason] = true
	}
}
