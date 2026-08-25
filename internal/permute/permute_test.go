package permute

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// recorder is a [TB] that remembers rather than fails, so this file can assert
// that the guard refuses something.
type recorder struct{ failures []string }

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

func TestStableRefusesAnOrderDependentAnswer(t *testing.T) {
	// The whole package is an oracle, and an oracle that accepts everything
	// satisfies its callers vacuously: five rows across three packages would
	// stay green while the defect they name walked back in.
	var r recorder
	Stable(&r, []string{"b", "a", "c"}, func(in []string) string { return in[0] })

	if len(r.failures) != 1 {
		t.Fatalf("first-element-wins must be refused exactly once, got %d refusals", len(r.failures))
	}
	if !strings.Contains(r.failures[0], "depends on the order") {
		t.Errorf("the refusal must say what is wrong, got %q", r.failures[0])
	}
}

func TestStableAcceptsAnOrderIndependentAnswer(t *testing.T) {
	var r recorder
	Stable(&r, []string{"b", "a", "c"}, slices.Min)

	if len(r.failures) != 0 {
		t.Errorf("an answer that is a function of the set alone must pass: %v", r.failures)
	}
}

func TestStableHandsTheSubjectItsOwnCopy(t *testing.T) {
	// Several of the entry points guarded here sort in place. One shared
	// backing array would mean every ordering after the first was the sorted
	// one, and the guard would agree with itself for the wrong reason.
	var r recorder
	in := []string{"b", "a", "c"}
	Stable(&r, in, func(got []string) string {
		slices.Sort(got)
		return got[0]
	})

	if len(r.failures) != 0 {
		t.Errorf("sorting in place must not be visible to the next call: %v", r.failures)
	}
	if !slices.Equal(in, []string{"b", "a", "c"}) {
		t.Errorf("the caller's slice was reordered: %v", in)
	}
}

func TestEveryOrderingIsTried(t *testing.T) {
	// The count is the assertion that matters: an orderings that returned only
	// the input would make every table in the tree pass without asking a
	// question, and nothing else in this package would notice.
	for n, want := range map[int]int{1: 1, 2: 2, 3: 6, 4: 24, 5: 120} {
		in := make([]int, n)
		for i := range in {
			in[i] = i
		}

		got := orderings(in)
		if len(got) != want {
			t.Errorf("%d elements produced %d orderings, want %d!", n, len(got), want)
		}
		if distinct(got) != want {
			t.Errorf("%d elements produced %d distinct orderings of %d", n, distinct(got), len(got))
		}
		for _, order := range got {
			if len(order) != n {
				t.Fatalf("an ordering of %d elements has %d: %v", n, len(order), order)
			}
		}
	}
}

func TestALargerInputGetsShuffledRatherThanEnumerated(t *testing.T) {
	// Above the exhaustive threshold the factorial is unaffordable, so the set
	// is the input, its reversal and a fixed number of shuffles. Fixed, so a
	// failure is reproducible: a guard that is red on one run and green on the
	// next is a guard somebody deletes.
	in := make([]int, exhaustiveUpTo+1)
	for i := range in {
		in[i] = i
	}

	got := orderings(in)
	if len(got) != shuffles+2 {
		t.Fatalf("got %d orderings, want the input, its reversal and %d shuffles", len(got), shuffles)
	}
	if !slices.Equal(got[0], in) {
		t.Errorf("the first ordering must be the input as given, got %v", got[0])
	}
	reversed := slices.Clone(in)
	slices.Reverse(reversed)
	if !slices.Equal(got[1], reversed) {
		t.Errorf("the second ordering must be the reversal, got %v", got[1])
	}
	if distinct(got) < shuffles/2 {
		t.Errorf("only %d of %d orderings are distinct", distinct(got), len(got))
	}
}

func distinct(orders [][]int) int {
	seen := map[string]bool{}
	for _, order := range orders {
		seen[fmt.Sprint(order)] = true
	}
	return len(seen)
}
