package metrics

import (
	"os"
	"slices"
	"strings"
	"testing"

	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/motleyhand/binpack/internal/drain"
	"github.com/motleyhand/binpack/internal/engine"
)

const reference = "../../docs/reference/metrics.md"

func referenceText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(reference)
	if err != nil {
		t.Fatalf("reading the metrics reference: %v", err)
	}
	return string(data)
}

func TestEveryPublishedMetricIsDocumented(t *testing.T) {
	// These names are public API. A hand-written reference drifts from the
	// code it describes; this is the check that stops it.
	Observe(snapshot(engine.NodeGroup{ID: "da8977ba-244f", MinSize: 1, MaxSize: 10, Ready: 1}),
		engine.Decision{
			Code:        engine.CodeNoneFeasible,
			Assessments: []engine.NodeAssessment{assess(engine.VerdictSkipped, engine.SkipCordoned)},
		}, config(), 0.01)

	doc := referenceText(t)
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}

	for _, f := range families {
		name := f.GetName()
		if !strings.HasPrefix(name, "binpack_") {
			continue
		}
		// Histograms are exposed as _bucket, _sum and _count; the family
		// carries the documented name.
		if !strings.Contains(doc, "`"+name+"`") {
			t.Errorf("%s is published but not documented", name)
		}
	}
}

func TestTheReferenceDocumentsNoMetricThatDoesNotExist(t *testing.T) {
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	published := map[string]bool{}
	for _, f := range families {
		published[f.GetName()] = true
	}

	doc := referenceText(t)
	for field := range strings.SplitSeq(doc, "`") {
		// The bare prefix appears in prose; it is not a metric name.
		if !strings.HasPrefix(field, "binpack_") || field == "binpack_" {
			continue
		}
		// PromQL examples wrap a name in functions and comparisons; take the
		// bare identifier.
		name := strings.FieldsFunc(field, func(r rune) bool {
			return r != '_' && (r < 'a' || r > 'z')
		})[0]
		if !published[name] {
			t.Errorf("the reference documents %q, which binpack does not publish", name)
		}
	}
}

func TestEveryLabelValueBinpackCanProduceIsDocumented(t *testing.T) {
	// The codes are the vocabulary shared between a dashboard and an
	// investigation. One missing from the reference is one nobody can look up
	// when it appears on a graph at three in the morning.
	//
	// Named for binpack rather than the engine, because the engine is not the
	// only package that produces one: the reason values on
	// binpack_drains_abandoned_total are drain constants.
	doc := referenceText(t)

	// Every one of these comes from an enumerator, and that is the whole
	// point. This list was hand-written, and a hand-written copy of a set is
	// a copy that drifts: three values binpack published — `being-removed`,
	// `gone` and `uncordoned` — were absent from the reference while this
	// test was green, because they were absent from the list too. The one
	// enumerator it did use, drain.AbandonCodes(), is the half that never
	// drifted.
	var codes []string
	codes = append(codes, engine.DecisionCodes()...)
	codes = append(codes, engine.Verdicts()...)
	codes = append(codes, engine.SkipCodes()...)
	codes = append(codes, drain.AbandonCodes()...)
	codes = append(codes, unmodelledCauses...)

	for _, code := range codes {
		if !strings.Contains(doc, "`"+code+"`") {
			t.Errorf("label value %q is not documented", code)
		}
	}
}

// codeTables are the reference's closed-set tables, each named by the line
// that introduces it, and the enumerator each one is a rendering of.
//
// Keyed on the introducing line rather than on position so that inserting a
// section moves nothing here, and so that a table which loses its preamble
// fails loudly instead of being skipped.
func codeTables() []struct {
	anchor  string
	allowed []string
} {
	return []struct {
		anchor  string
		allowed []string
	}{
		{"`code` is one of:", engine.DecisionCodes()},
		{"`code` on `binpack_nodes_skipped` is one of:", engine.SkipCodes()},
		// The abandonment table lists the drain's own codes; the skip codes
		// and the two verdicts reach the counter through revalidation and are
		// described in the prose beneath it rather than repeated as rows.
		{"`reason` is one of:", append(append(append([]string{},
			drain.AbandonCodes()...), engine.SkipCodes()...), engine.Verdicts()...)},
		{"| `cause` | What binpack could not do |", unmodelledCauses},
	}
}

// TestTheReferenceDocumentsNoLabelValueBinpackCannotProduce is the direction
// the metrics reference never had, and it is the one that catches a rename.
//
// Adding a code and forgetting the doc fails the test above. Renaming one
// fails neither, because the new spelling gets documented and the old row is
// simply left behind — describing a series that will never appear again, in
// the one document an operator consults when a value shows up on a graph they
// do not recognise. internal/cli/diagnostics_doc_test.go has had both
// directions since the catalogue existed; this is the metric vocabulary
// catching up.
func TestTheReferenceDocumentsNoLabelValueBinpackCannotProduce(t *testing.T) {
	doc := referenceText(t)

	for _, table := range codeTables() {
		rows := tableTokens(t, doc, table.anchor)
		if len(rows) == 0 {
			t.Errorf("no table found under %q: the reference's shape moved and this "+
				"check silently stopped reading it", table.anchor)
			continue
		}
		for _, code := range rows {
			if !slices.Contains(table.allowed, code) {
				t.Errorf("the reference documents %q under %q, and binpack cannot produce it",
					code, table.anchor)
			}
		}
	}
}

// tableTokens returns the first backticked cell of every row of the Markdown
// table that follows anchor.
func tableTokens(t *testing.T, doc, anchor string) []string {
	t.Helper()

	lines := strings.Split(doc, "\n")
	at := slices.Index(lines, anchor)
	if at < 0 {
		return nil
	}

	var out []string
	for _, line := range lines[at+1:] {
		if !strings.HasPrefix(line, "|") {
			// Blank lines separate the anchor from its table; anything else
			// ends it.
			if strings.TrimSpace(line) == "" && len(out) == 0 {
				continue
			}
			break
		}
		cell := strings.SplitN(line, "|", 3)
		if len(cell) < 3 {
			continue
		}
		// The header separator and the header row itself carry no backticks.
		if code := strings.Trim(strings.TrimSpace(cell[1]), "`"); strings.Contains(cell[1], "`") {
			out = append(out, code)
		}
	}
	return out
}

// TestTheVerdictSentenceListsEveryVerdict pins the one closed set the
// reference states inline rather than as a table.
//
// Four values fit in a sentence and a four-row table would be worse to read,
// so the sentence stays — but it is still the vocabulary of a public label,
// and a fifth verdict added without touching it would leave the reference
// asserting a set that is no longer complete.
func TestTheVerdictSentenceListsEveryVerdict(t *testing.T) {
	doc := referenceText(t)

	var quoted []string
	for _, v := range engine.Verdicts() {
		quoted = append(quoted, "`"+v+"`")
	}
	sentence := "`verdict` is one of " + strings.Join(quoted, ", ") + "."

	if !strings.Contains(doc, sentence) {
		t.Errorf("the reference does not say %q, so its verdict vocabulary is not the engine's",
			sentence)
	}
}

// TestEveryPublishedVocabularyIsASet holds the property every guard in this
// file assumes without checking: that an enumerator's values are distinct.
//
// These lists are turned into sets — by the reference comparisons above, by
// the zero-series pre-initialisation, by the reachability loop in
// internal/engine — and a set silently absorbs a duplicate. Two constants
// sharing a value therefore collapse into one enumerated code, every fixture
// goes on asserting its own constant and passing, and a reference updated to
// the resulting vocabulary agrees with it. What is lost is the distinction:
// two operational causes an alert cannot tell apart, published under one label
// value, with nothing anywhere saying so.
//
// Here rather than beside each vocabulary because this is the file that
// already gathers them, and because they are one namespace to whoever reads
// the metrics — the reference lists them together and the guards above compare
// them together.
//
// engine.Diagnoses is deliberately absent: it is built from a map keyed by the
// code, so a duplicate there is not something that can be written. That is the
// shape to prefer, and the reason these need a guard is that they are
// hand-written slices. controller.EventReasons is the same hazard and cannot
// be reached from here — internal/engine is in the depguard purity set and
// this file imports it — so it carries its own guard in that package.
//
// The names cannot be recovered from the values, which is why this reports the
// value and leaves finding the pair to a grep. Constants are the only place
// the names exist.
func TestEveryPublishedVocabularyIsASet(t *testing.T) {
	for _, vocabulary := range []struct {
		name   string
		values []string
	}{
		{"engine.SkipCodes", engine.SkipCodes()},
		{"engine.Verdicts", engine.Verdicts()},
		{"engine.DecisionCodes", engine.DecisionCodes()},
		{"drain.AbandonCodes", drain.AbandonCodes()},
	} {
		if len(vocabulary.values) == 0 {
			t.Errorf("%s() enumerates nothing, so this asserts nothing about it",
				vocabulary.name)
		}

		seen := map[string]bool{}
		for _, value := range vocabulary.values {
			// Already refused downstream — TestNoLabelCarriesProse gathers the
			// pre-initialised series and rejects `=""`, and every one of these
			// four is pre-initialised. Refused here too because that test is
			// about a rendered series and this one is about the vocabulary,
			// and a reader of the enumerator should not have to know which
			// other file holds it well-formed.
			if value == "" {
				t.Errorf("%s() enumerates the empty string; observeNodes suppresses an "+
					"empty code, so the value is documented and pre-initialised and no "+
					"series ever carries it", vocabulary.name)
			}
			if seen[value] {
				t.Errorf("%s() enumerates %q twice, so two constants share it: the pair is "+
					"one label value now, and every check that turns this list into a set "+
					"reports them as one thing an operator cannot tell apart",
					vocabulary.name, value)
			}
			seen[value] = true
		}
	}
}

// TestTheAbandonmentReasonsAreOneSet is the cross-enumerator half of
// TestEveryPublishedVocabularyIsASet, and it is needed because one label
// namespace is fed by three lists.
//
// binpack_drains_abandoned_total{reason} carries a drain's own codes, the skip
// codes a revalidation can end a drain with, and the verdicts that can. Each
// list is distinct within itself and nothing checked them against each other:
// AbandonStuck taking SkipBackoff's value leaves every per-list guard green,
// and the reference updated to the resulting vocabulary agrees with it. The
// series then means both "this drain is wedged on a finalizer" and "this node
// is in backoff", and an alert on it cannot say which — which is exactly the
// distinction PUBLIC-01 split these codes apart to make.
//
// The same three lists the reference's abandonment table is checked against,
// so a value added to that namespace is covered here without being named.
func TestTheAbandonmentReasonsAreOneSet(t *testing.T) {
	var reasons []string
	reasons = append(reasons, drain.AbandonCodes()...)
	reasons = append(reasons, engine.SkipCodes()...)
	reasons = append(reasons, engine.Verdicts()...)

	if len(reasons) == 0 {
		t.Fatal("the abandonment reasons enumerate nothing, so this asserts nothing")
	}

	seen := map[string]bool{}
	for _, reason := range reasons {
		if seen[reason] {
			t.Errorf("%q reaches binpack_drains_abandoned_total{reason} from two of "+
				"drain.AbandonCodes, engine.SkipCodes and engine.Verdicts; the two causes "+
				"are one series, and nothing reading the metric can tell them apart",
				reason)
		}
		seen[reason] = true
	}
}
