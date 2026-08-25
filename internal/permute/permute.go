// Package permute is the standing guard that binpack names the same object
// twice.
//
// Almost every report binpack writes singles one object out of many — the node
// it chose, the budget it blamed, the pod it is waiting for — and an operator
// reads those reports by diffing them: against the last evaluation, and against
// what `binpack explain` predicted `binpack run` would do. So the choice has to
// be a function of the cluster and of nothing else, and two orderings of the
// same objects are the same cluster.
//
// Disagreement arrives by two routes and neither guard catches the other's. A
// slice reaches a comparator that is not total and the sort hands the tie back
// to input order: the controller's order comes from a watch-backed cache and
// `explain`'s from a live client, so the two frontends describe an unchanged
// node differently. A map is ranged over and the runtime reshuffles the walk on
// every range statement, so one process disagrees with itself between two
// consecutive evaluations of nothing having happened. [Stable] therefore
// permutes *and* repeats.
//
// It lives in its own package because the discipline has now stopped at a
// package boundary twice — the engine's orderings were made total and the same
// defect reappeared in the eviction blockers and then in the drain assessment.
// A guard each package can import is what makes the third recurrence a test
// failure rather than another review.
package permute

import (
	"math/rand/v2"
	"slices"
)

// TB is the part of testing.T this package uses.
//
// Narrow deliberately, so the guard can be pointed at a recorder and made to
// fail on demand. An oracle nothing has seen refuse anything is the failure
// this package exists to refuse: a vacuous orderings would leave every row in
// every table green while asking one question once.
type TB interface {
	Helper()
	Errorf(format string, args ...any)
}

// Repeats is how many times one ordering is asked the same question.
//
// More than once because map iteration is a coin the runtime tosses per range
// statement — and a biased one: two budgets in a map flip the reported subject
// on roughly one evaluation in seven, so a single call per ordering would pass
// against the defect most of the time. That is exactly the failure this file
// exists to refuse, a guard that is green on the run that mattered.
const Repeats = 16

// exhaustiveUpTo is the largest input every ordering of which is tried. 5! is
// 120 orderings, 6! is 720, and multiplied by [Repeats] the second stops being
// a test and starts being a benchmark.
const exhaustiveUpTo = 5

// shuffles is how many orderings a larger input gets instead.
const shuffles = 64

// Stable fails t unless subject names the same thing for every ordering of in.
//
// subject is handed its own copy of the slice on every call, so it is free to
// sort in place — and it has to be, because several of the entry points guarded
// here do exactly that, and one shared backing array would mean the later
// orderings were never actually tried.
func Stable[T any](t TB, in []T, subject func([]T) string) {
	t.Helper()

	want := subject(slices.Clone(in))
	for i, order := range orderings(in) {
		for range Repeats {
			got := subject(slices.Clone(order))
			if got == want {
				continue
			}
			// One report rather than one per ordering. A second failure says
			// nothing the first did not, and this guard is a table wide enough
			// that repeating would bury the row that broke.
			t.Errorf("the answer depends on the order of the input:\n"+
				"  first ordering  = %s\n  ordering %d      = %s", want, i, got)
			return
		}
	}
}

// orderings returns the input orders worth asking about.
//
// Exhaustive while that is affordable, because a comparator total for every
// pair but one is still caught only by the ordering that puts that pair the
// wrong way round. Above [exhaustiveUpTo] the factorial is not affordable and
// the set becomes the input, its reversal and a fixed number of shuffles from a
// seeded generator — seeded, because a guard that fails on one run in twenty
// and passes on the next is a guard somebody deletes.
func orderings[T any](in []T) [][]T {
	if len(in) <= exhaustiveUpTo {
		return permutations(slices.Clone(in))
	}

	reversed := slices.Clone(in)
	slices.Reverse(reversed)
	out := [][]T{slices.Clone(in), reversed}

	r := rand.New(rand.NewPCG(1, 2))
	for range shuffles {
		s := slices.Clone(in)
		r.Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
		out = append(out, s)
	}
	return out
}

// permutations returns every ordering of in, by Heap's algorithm. It permutes
// in place, so the caller owns the slice it passes.
func permutations[T any](in []T) [][]T {
	var out [][]T

	var generate func(k int)
	generate = func(k int) {
		if k <= 1 {
			out = append(out, slices.Clone(in))
			return
		}
		for i := range k {
			generate(k - 1)
			if k%2 == 0 {
				in[i], in[k-1] = in[k-1], in[i]
			} else {
				in[0], in[k-1] = in[k-1], in[0]
			}
		}
	}
	generate(len(in))

	return out
}
