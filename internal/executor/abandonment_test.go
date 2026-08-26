package executor_test

import (
	"slices"
	"testing"

	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/metrics"
)

// TestOnlyTwoVerdictsCanEndADrain is the independent half of
// metrics.AbandonmentVerdicts.
//
// That list is both the source of the pre-initialised zero series and the
// reference guard's allowed set — one source of truth, and one place to be
// wrong. A verdict added there and to the table satisfies the zero-series
// check, the reverse-documentation check and the uniqueness check at once,
// and nothing in that package can tell that no failed step ever carries it.
//
// Which verdicts can is decided here, in two places. Advance reaches the
// abandonment branch only when the verdict is not drainable, so drainable
// cannot be one. And a skipped assessment carries a skip code by construction
// — every branch that sets Skipped sets one, which internal/engine's
// TestEverySkipReasonCarriesItsOwnCode holds — so the code is what gets
// published and the verdict never is. That leaves infeasible and blocked.
//
// Driven from the assessments the engine produces rather than asserted as a
// pair, so a verdict added to the vocabulary is included until something here
// shows it cannot be.
func TestOnlyTwoVerdictsCanEndADrain(t *testing.T) {
	var canEnd []string
	for _, verdict := range engine.Verdicts() {
		a := assessmentWithVerdict(t, verdict)

		// The branch guard: a drainable node is the drain continuing.
		if a.Verdict() == engine.VerdictDrainable {
			continue
		}
		// And the code an abandonment publishes is the skip code when there is
		// one, so a verdict that always carries one is never published itself.
		if a.SkipCode != "" {
			continue
		}
		canEnd = append(canEnd, verdict)
	}

	want := metrics.AbandonmentVerdicts()
	slices.Sort(canEnd)
	slices.Sort(want)
	if !slices.Equal(canEnd, want) {
		t.Errorf("a failed step can carry %v and metrics pre-initialises %v; the counter "+
			"either creates a series nothing emits, or emits one it never zeroed — and "+
			"an unzeroed counter is invisible to rate() until the first occurrence",
			canEnd, want)
	}
}

// assessmentWithVerdict is the assessment the engine produces for one verdict.
//
// Built through the fields Verdict() reads rather than by naming the verdict,
// so the mapping this depends on is the engine's own.
func assessmentWithVerdict(t *testing.T, verdict string) engine.NodeAssessment {
	t.Helper()

	var a engine.NodeAssessment
	switch verdict {
	case engine.VerdictSkipped:
		// Skipped and its code are set together, always.
		a.Skipped, a.SkipCode = true, engine.SkipCordoned
	case engine.VerdictInfeasible:
		a.Simulation = &engine.Simulation{Feasible: false}
	case engine.VerdictBlocked:
		a.Simulation = &engine.Simulation{Feasible: true}
		a.Blockers = []engine.EvictionBlocker{{Message: "a budget will not allow it"}}
	case engine.VerdictDrainable:
		a.Simulation = &engine.Simulation{Feasible: true}
	default:
		t.Fatalf("engine.Verdicts() holds %q and this test does not know how a node "+
			"reaches it; a new verdict has to be classified before the abandonment "+
			"counter can say whether it zeroes a series for it", verdict)
	}

	if got := a.Verdict(); got != verdict {
		t.Fatalf("the fixture for %q assesses as %q, so this is not testing the verdict "+
			"it names", verdict, got)
	}
	return a
}
