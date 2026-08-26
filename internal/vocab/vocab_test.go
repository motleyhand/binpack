package vocab_test

import (
	"maps"
	"slices"
	"testing"

	"github.com/motleyhand/binpack/internal/vocab"
)

// TestStringConstantsReadsEveryShapeAVocabularyIsWrittenIn holds the property
// the package exists for, against the spellings that were found missing one at
// a time.
//
// Six review rounds each found another way to declare a constant that the
// hand-written evaluator could not read, and every one of them was silent: a
// member skipped here is a member missing from both sides of the count that
// compares declarations with an enumerator, so the guard passes while the
// value is published and documented nowhere.
//
// The fixture is a package rather than a table of source strings, so each
// shape has to compile to be in the test at all.
func TestStringConstantsReadsEveryShapeAVocabularyIsWrittenIn(t *testing.T) {
	got, err := vocab.StringConstants("testdata/shapes", "Code")
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}

	want := map[string]string{
		"CodePlain":       "plain",
		"CodeJoined":      "two-words",
		"CodeAlias":       "plain",
		"CodeFirst":       "first",
		"CodeRepeated":    "first",
		"CodeFromLiteral": "x",
		"CodeFromRune":    "A",
		"CodeFromArith":   "B",
		"CodeConverted":   "converted",
		"CodeIotaA":       "A",
		"CodeIotaB":       "B",
		"CodeIotaC":       "C",
	}

	for name, value := range want {
		switch found, ok := got[name]; {
		case !ok:
			t.Errorf("%s is declared in the fixture and was not read; a member missing "+
				"here is missing from both sides of every count that compares "+
				"declarations with an enumerator", name)
		case found != value:
			t.Errorf("%s reads as %q, and Go compiles it to %q", name, found, value)
		}
	}

	// And the names that carry the prefix without being members. Reported as
	// unevaluable rather than ignored, each of these failed the guard over
	// code that compiles.
	for _, name := range []string{"CodeRadix", "CodeMask", "CodeCompared", "CodeRuneOnly"} {
		if value, ok := got[name]; ok {
			t.Errorf("%s is not a string and was read as one (%q); a vocabulary is the "+
				"values a label can take, and this is not one of them", name, value)
		}
	}

	// A constant inside a function is not part of anything published.
	if value, ok := got["CodeLocal"]; ok {
		t.Errorf("a function-local constant was read as a package vocabulary member (%q)",
			value)
	}

	// A floor, because every check above iterates a list: a reader that found
	// nothing would satisfy the absence checks and report nothing about the
	// rest.
	if len(got) != len(want) {
		t.Errorf("read %d constants and the fixture declares %d members: %v",
			len(got), len(want), slices.Sorted(maps.Keys(got)))
	}
}

// TestStringConstantsRefusesAPackageThatDoesNotCompile holds the other half of
// "never skip what you cannot read".
//
// The evaluator this replaces reported an expression it could not handle,
// which was the right instinct and the wrong mechanism — it kept finding
// expressions Go handles perfectly well. Here the only way to have no value is
// to have no package, and that is an error rather than a shorter answer.
func TestStringConstantsRefusesAPackageThatDoesNotCompile(t *testing.T) {
	if _, err := vocab.StringConstants("testdata/broken", "Code"); err == nil {
		t.Error("a package that does not type-check produced constants rather than an " +
			"error, so a vocabulary read from it would be whatever survived")
	}
}
