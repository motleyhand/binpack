package controller

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/motleyhand/binpack/internal/mother"
	"github.com/motleyhand/binpack/internal/vocab"
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

	for _, name := range eventVocabulary() {
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
	// Each field on its own. An Event carries a reason and an action, and
	// Kubernetes stores and filters them separately — so `reason=Consolidate`
	// and `action=Consolidate` are two selectors rather than one collision,
	// and merging the vocabularies before deduplicating them would refuse a
	// legitimate reason for sharing a name with an unrelated action.
	//
	// The action is checked at all because it was appended to the
	// documentation loop above and left out of this one, and an Event whose
	// action is empty has lost the field that groups every consolidation event
	// together.
	for _, vocabulary := range []struct {
		field  string
		values []string
	}{
		{"reason", EventReasons()},
		{"action", eventActions()},
	} {
		requireDistinct(t, vocabulary.field, vocabulary.values)
	}
}

// requireDistinct holds one Event field's vocabulary to being a set of
// non-empty values.
func requireDistinct(t *testing.T, field string, values []string) {
	t.Helper()

	if len(values) == 0 {
		t.Fatalf("the Event %s vocabulary enumerates nothing, so this asserts nothing "+
			"about it", field)
	}

	seen := map[string]bool{}
	for _, value := range values {
		// An empty value is an Event nobody can filter on, and the guard above
		// would look for `` in the reference — which every Markdown code fence
		// satisfies. Unlike the metric label values, nothing downstream
		// refuses this one: an Event carries whatever it is given.
		if value == "" {
			t.Errorf("the Event %s vocabulary holds the empty string; an Event written "+
				"with it carries no %s, and `kubectl get events --field-selector %s=` has "+
				"nothing to match", field, field, field)
		}
		if seen[value] {
			t.Errorf("the Event %s vocabulary enumerates %q twice, so two constants share "+
				"it and the events they were meant to distinguish read as one",
				field, value)
		}
		seen[value] = true
	}
}

// reasonSources is where the Event reason constants are declared. A glob
// rather than a file, for the reason internal/cli's conventions guard uses
// one: a split moves the declarations and a named file stops seeing them.
const reasonSources = "."

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
	for _, reason := range eventVocabulary() {
		enumerated[reason] = true
	}

	// Reasons and actions. An Event carries both and an operator filters on
	// both, so a check written for one leaves the other unheld — and asked for
	// every constant instead, this would refuse the package's durations, which
	// it is right to refuse and wrong to be asked about.
	declared := map[string]string{}
	for _, prefix := range []string{"Reason", "Action"} {
		found, err := vocab.StringConstants(reasonSources, prefix)
		if err != nil {
			t.Fatalf("reading this package's %s constants: %v", prefix, err)
		}
		maps.Copy(declared, found)
	}

	var counted int
	for name, value := range declared {
		counted++
		if !enumerated[value] {
			t.Errorf("%s = %q is declared here and the Event vocabulary does not enumerate "+
				"it; the Events carrying it are written onto nodes, and nothing holds the "+
				"reference to a vocabulary it is not in", name, value)
		}
	}

	if counted != len(eventVocabulary()) {
		t.Errorf("found %d Reason and Action constants and the Event vocabulary holds %d; "+
			"either the parse has stopped seeing the declarations or the vocabulary holds "+
			"a value no constant does", counted, len(eventVocabulary()))
	}
}

// reasonTable is the catalogue an operator reads the Event vocabulary from,
// and the line that introduces it.
//
// Keyed on the introducing line rather than on position, the way
// internal/metrics keys its code tables: inserting a section moves nothing
// here, and a table that loses its preamble fails loudly instead of being
// skipped.
const (
	reasonTablePage   = "../../docs/reference/cli.md"
	reasonTableAnchor = "| Reason | When |"
)

// TestTheEventReasonTableIsExactlyTheVocabulary is the direction the
// containment check above cannot give.
//
// That one proves each current reason appears *somewhere* in the reference
// corpus, which a mention in any page satisfies and which says nothing about
// what else the catalogue lists. Renaming a reason, putting the new spelling
// on some other page and leaving the old row here keeps every vocabulary test
// green while this table goes on advertising a `--field-selector` no Event can
// match — and this table is the one an operator copies the selector out of.
//
// Compared exactly, in both directions, because the table is the catalogue
// rather than a mention: a row with no constant is a promise nothing keeps,
// and a constant with no row is the gap the containment check was added for.
func TestTheEventReasonTableIsExactlyTheVocabulary(t *testing.T) {
	page, err := os.ReadFile(reasonTablePage)
	if err != nil {
		t.Fatalf("reading %s: %v", reasonTablePage, err)
	}

	_, rest, ok := strings.Cut(string(page), reasonTableAnchor)
	if !ok {
		t.Fatalf("%s no longer carries the %q table, so the Event vocabulary has no "+
			"catalogue and this asserts nothing", reasonTablePage, reasonTableAnchor)
	}

	listed := map[string]bool{}
	for _, line := range strings.Split(rest, "\n") {
		// The table ends at the first line that is not one of its rows.
		if !strings.HasPrefix(line, "| `") {
			if len(listed) > 0 {
				break
			}
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(line, "| `"), "`")
		listed[name] = true
	}
	if len(listed) == 0 {
		t.Fatalf("the %q table in %s parsed to no rows", reasonTableAnchor, reasonTablePage)
	}

	enumerated := map[string]bool{}
	for _, reason := range EventReasons() {
		enumerated[reason] = true
		if !listed[reason] {
			t.Errorf("EventReasons() holds %q and the catalogue in %s does not list it; an "+
				"operator reading that table for a field selector has no way to know the "+
				"Event exists", reason, reasonTablePage)
		}
	}
	for name := range listed {
		if !enumerated[name] {
			t.Errorf("the catalogue in %s lists %q and no Event carries it; the page "+
				"advertises `kubectl get events --field-selector reason=%s`, which matches "+
				"nothing", reasonTablePage, name, name)
		}
	}
}

// eventVocabulary is every value an Event binpack writes can carry in the two
// fields an operator filters on.
//
// Reasons and actions together, because they are one vocabulary from outside:
// `kubectl get events --field-selector reason=…,action=…` reads both, and a
// check written for one of them leaves the other unheld. EventReasons() is the
// published enumerator of the first half; the second was one constant written
// into each check by hand, so a second action would have been public,
// undocumented and unaudited.
func eventVocabulary() []string {
	return append(EventReasons(), eventActions()...)
}

// eventActions is every action binpack writes. One today, enumerated all the
// same, for the reason [EventReasons] is enumerated.
func eventActions() []string {
	return []string{ActionConsolidate}
}

// actionClaim matches the reference's statement of an Event's action, taking
// the whole token.
//
// Bounded by the closing backtick rather than by an alphabetic run: an Event
// action may hold anything the field permits, and `Consolidate-Node` is a
// legal value the declaration audit and the documentation guard both accept
// while this pattern could not see the sentence documenting it — so a valid
// rename failed here for being spelled with a dash.
var actionClaim = regexp.MustCompile("`action: ([^`]+)`")

// TestTheDocumentedActionIsExactlyTheVocabulary is the actions' half of
// TestTheEventReasonTableIsExactlyTheVocabulary.
//
// The reasons have a catalogue and are compared with it in both directions.
// The action has a sentence — "Every Event carries `action: Consolidate`, so
// they filter as a group" — and was only ever checked forward, by the
// containment guard that asks whether each current value appears *somewhere*
// in the reference corpus. Renaming the action and mentioning the new spelling
// anywhere satisfies that, while the sentence goes on telling an operator to
// filter on `action=Consolidate`, which no Event carries.
//
// One claim today and compared as a set anyway, because the failure this
// catches is a value that stops being emitted rather than one that is added,
// and a check written for exactly one value cannot see that.
func TestTheDocumentedActionIsExactlyTheVocabulary(t *testing.T) {
	claimed := map[string]bool{}
	for _, match := range actionClaim.FindAllStringSubmatch(referenceCorpus(t), -1) {
		claimed[match[1]] = true
	}
	if len(claimed) == 0 {
		t.Fatal("no reference page states the action an Event carries, so an operator " +
			"filtering on it has nowhere to look and this asserts nothing")
	}

	enumerated := map[string]bool{}
	for _, action := range eventActions() {
		enumerated[action] = true
		if !claimed[action] {
			t.Errorf("binpack writes Events with action %q and no reference page says so; "+
				"the pages document the reasons and leave the field they are grouped by "+
				"unstated", action)
		}
	}
	for action := range claimed {
		if !enumerated[action] {
			t.Errorf("a reference page says Events carry action %q and none does; it tells "+
				"an operator to filter on `action=%s`, which matches nothing", action, action)
		}
	}
}
