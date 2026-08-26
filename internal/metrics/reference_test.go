package metrics

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/motleyhand/binpack/internal/drain"
	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/vocab"
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
	partial bool
} {
	return []struct {
		anchor  string
		allowed []string
		partial bool
	}{
		{"`code` is one of:", engine.DecisionCodes(), false},
		// The codes selection produces, not the whole vocabulary. This series
		// is fed from a Decision's assessments, so the three codes only
		// Revalidate reaches can never label it — and marking the table
		// partial while allowing all sixteen accepted a row for one of them,
		// which is the same wrong selector the guard beside it refuses.
		{"`code` on `binpack_nodes_skipped` is one of:", engine.SelectionSkipCodes(), false},
		// The abandonment table lists the drain's own codes; the skip codes
		// and the two verdicts reach the counter through revalidation and are
		// described in the prose beneath it rather than repeated as rows.
		// The drain's own codes, exactly. The skip codes and the two verdicts
		// reach this counter too and are described in the prose beneath the
		// table rather than repeated as rows — so widening the allowed set to
		// the union and marking the table partial let a row disappear: with
		// `stuck` removed and the token still backticked in prose, the
		// forward guard found it, every surviving row was in the broad
		// allowance, and the reverse comparison was skipped. The prose half is
		// held by TestEveryLabelValueBinpackCanProduceIsDocumented, which
		// searches the page.
		{"`reason` is one of:", drain.AbandonCodes(), false},
		{"| `cause` | What binpack could not do |", unmodelledCauses, false},
	}
}

// TestTheAbandonmentSectionNamesNoReasonTheCounterCannotCarry closes the
// prose half of binpack_drains_abandoned_total.
//
// The table beneath that metric lists the drain's own codes and is compared in
// both directions. Everything else the counter can carry — every skip code,
// and the two verdicts that end a drain without one — is described in the
// prose below it instead of repeated as rows, and prose had only ever been
// checked forwards: each value binpack produces appears somewhere on the page.
//
// That direction cannot see a stale claim. Rename a reason and document the
// new spelling anywhere, and the paragraph explaining the old one survives,
// describing a series that will never appear again — in the one document an
// operator consults when a value shows up on a graph they do not recognise.
// Drop a verdict from [AbandonmentVerdicts] and the sentence promising it
// survives the same way, hidden by that word's other appearances elsewhere on
// the page.
//
// So the section is read as a closed claim: every backticked token in it
// shaped like a label value has to be one the counter can carry. Not every
// value has to appear here — the section says skip codes can appear and then
// names only the exceptions, which is the right way to write it — so the
// forward direction stays page-wide, in the test above.
func TestTheAbandonmentSectionNamesNoReasonTheCounterCannotCarry(t *testing.T) {
	section := abandonmentSection(t)

	// Everything the counter can label a series with, from the same three
	// enumerators its zero series are pre-initialised from.
	var vocabulary []string
	vocabulary = append(vocabulary, drain.AbandonCodes()...)
	vocabulary = append(vocabulary, engine.SkipCodes()...)
	vocabulary = append(vocabulary, AbandonmentVerdicts()...)

	// The words in this section that are shaped like a code and are not one: a
	// label's name rather than its value, a Kubernetes field, a noun the prose
	// happens to set in code font.
	//
	// Each is asserted to still be here, so a paragraph that stops using one
	// fails until it is pruned. An exception list nobody prunes is how a real
	// code later takes one of these spellings and inherits its exemption.
	notValues := []string{"reason", "generation"}
	for _, word := range notValues {
		if !strings.Contains(section, "`"+word+"`") {
			t.Errorf("`%s` is listed as a word this section uses that is not a reason "+
				"value, and the section no longer uses it; remove it from the list, or "+
				"a reason code spelled that way will be exempt from this check", word)
		}
	}

	for _, match := range codeShaped.FindAllStringSubmatch(section, -1) {
		token := match[1]
		if slices.Contains(vocabulary, token) || slices.Contains(notValues, token) {
			continue
		}
		t.Errorf("the abandonment section presents `%s` as a reason and no enumerator "+
			"produces it, so binpack_drains_abandoned_total can carry no such series: "+
			"either a code was renamed and this paragraph was left behind, or it is a "+
			"word that needs adding to the list of non-values here", token)
	}
}

// codeShaped matches a backticked token written the way a label value is:
// lower case, words joined by hyphens.
//
// Deliberately not every backticked token. This section names metrics,
// selectors and configuration settings too, and those are spelled with
// underscores, braces or capitals — so the shape excludes them without a list
// to maintain.
var codeShaped = regexp.MustCompile("`([a-z][a-z0-9]*(?:-[a-z0-9]+)*)`")

// nextHeading matches the start of the next Markdown heading, at any level.
var nextHeading = regexp.MustCompile(`(?m)^#{1,6} `)

// abandonmentSection is the reference's prose about
// binpack_drains_abandoned_total: its reason table and everything said about
// the values that are not in it, up to the next heading.
func abandonmentSection(t *testing.T) string {
	t.Helper()

	const opening = "`reason` is one of:"

	doc := referenceText(t)
	start := strings.Index(doc, opening)
	if start < 0 {
		t.Fatalf("the metrics reference no longer says %q, so this cannot find the "+
			"abandonment prose and would check an empty string", opening)
	}

	section := doc[start:]

	// The next heading at any level, not the next `## `. Cut at the section
	// level alone, this ran on through `### Pools` and swallowed another
	// metric's table — so `pool` had to be excused as a word that is not a
	// reason, when the truth was that it is a different metric's label and
	// this had no business reading it. An exception list is where an
	// over-broad reader hides.
	if end := nextHeading.FindStringIndex(section); end != nil {
		section = section[:end[0]]
	}
	// A floor, because every assertion above iterates what it finds: a section
	// that shrank to its heading would satisfy them all in silence.
	if lines := strings.Count(section, "\n"); lines < 20 {
		t.Fatalf("the abandonment section is %d lines, which is shorter than it has ever "+
			"been; this test would be checking almost nothing", lines)
	}
	return section
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

		// And every allowed code needs a row. Checking only that each row is
		// allowed leaves the table free to lose one: removing the
		// `none-feasible` row stayed green because the value is still
		// backticked in the prose below, which satisfies the
		// every-value-is-documented guard — and that guard searches the whole
		// page while this table is the catalogue somebody reads the closed set
		// out of. A series missing from it is a series an operator has no way
		// to know exists.
		if table.partial {
			continue
		}
		for _, code := range table.allowed {
			if !slices.Contains(rows, code) {
				t.Errorf("binpack publishes %q and the table under %q does not list it; "+
					"that table is the closed set an operator reads, and a value absent "+
					"from it is one they cannot look up", code, table.anchor)
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

	// Every such claim, not merely one of them. Containment proves the current
	// sentence exists and says nothing about the others: a verdict renamed
	// with the new sentence added leaves the old one standing somewhere else
	// on the page, the forward guard finds the new spelling, and the reverse
	// label checks never look — they read tables, and this is prose. The page
	// then advertises a `verdict` selector that matches nothing.
	claims := verdictClaim.FindAllStringSubmatch(doc, -1)
	if len(claims) == 0 {
		t.Fatal("no verdict sentence found at all, so the pattern has stopped matching " +
			"and the check above passed on a page it did not read")
	}
	for _, claim := range claims {
		listed := backticked.FindAllStringSubmatch(claim[1], -1)
		var named []string
		for _, m := range listed {
			named = append(named, m[1])
		}
		if !slices.Equal(named, engine.Verdicts()) {
			t.Errorf("the reference says the verdicts are %v and the engine's are %v; one "+
				"of these sentences has outlived the vocabulary, and it tells an operator "+
				"to filter on a value nothing carries", named, engine.Verdicts())
		}
	}
}

// verdictClaim matches a sentence stating the verdict vocabulary, capturing
// the list it gives.
var verdictClaim = regexp.MustCompile("`verdict` is one of ([^.]*)\\.")

// backticked matches one backticked token.
var backticked = regexp.MustCompile("`([^`]+)`")

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
	reasons = append(reasons, abandonmentVerdicts...)

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

// TestEveryVocabularyConstantIsEnumerated is the direction
// TestEveryPublishedVocabularyIsASet cannot give.
//
// That one visits what an enumerator holds, so it has no opinion about a value
// removed from it. Dropping AbandonNotRemoved from drain.AbandonCodes() and
// its row from the reference leaves the drain code emitting it and its
// fixtures green — they compare against the same constant — while the loop
// stops visiting it. The series is then published, undocumented, and no longer
// pre-initialised, so it appears only once something has already gone wrong,
// which is the case a pre-initialised zero exists to prevent.
//
// Read from the packages' own declarations rather than by driving every path
// that emits one. The constants are what the emitting code names, so a
// constant that exists and is not enumerated is the whole defect; driving
// would test the same thing through more machinery and would go quiet for a
// branch no fixture reaches — which is the state this is meant to catch.
// internal/controller does the same for its Event reasons, in its own package
// because the purity rule keeps it out of this one.
func TestEveryVocabularyConstantIsEnumerated(t *testing.T) {
	for _, vocabulary := range []struct {
		name   string
		dir    string
		prefix string
		values []string
	}{
		{"engine.SkipCodes", "../engine", "Skip", engine.SkipCodes()},
		{"engine.Verdicts", "../engine", "Verdict", engine.Verdicts()},
		{"engine.DecisionCodes", "../engine", "Code", engine.DecisionCodes()},
		{"drain.AbandonCodes", "../drain", "Abandon", drain.AbandonCodes()},
	} {
		enumerated := map[string]bool{}
		for _, value := range vocabulary.values {
			enumerated[value] = true
		}

		declared := constantsWithPrefix(t, vocabulary.dir, vocabulary.prefix)
		for name, value := range declared {
			if !enumerated[value] {
				t.Errorf("%s = %q is declared in %s and %s() does not enumerate it; it is "+
					"published as a label value, documented by nothing that reads the "+
					"enumerator, and never pre-initialised — so the series appears for the "+
					"first time on the day it fires", name, value, vocabulary.dir,
					vocabulary.name)
			}
		}
		if len(declared) != len(vocabulary.values) {
			t.Errorf("%s declares %d %s* constants and %s() holds %d; either the parse has "+
				"stopped seeing them or the enumerator holds a value no constant does",
				vocabulary.dir, len(declared), vocabulary.prefix, vocabulary.name,
				len(vocabulary.values))
		}
	}
}

// constantsWithPrefix is every string constant a package declares whose name
// starts with the given prefix, by name.
func constantsWithPrefix(t *testing.T, dir, prefix string) map[string]string {
	t.Helper()

	out, err := vocab.StringConstants(dir, prefix)
	if err != nil {
		t.Fatalf("reading %s's constants: %v", dir, err)
	}
	return out
}
