package drain_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/motleyhand/binpack/internal/drain"
	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
	"github.com/motleyhand/binpack/internal/permute"
)

// TestEveryReportedSubjectIsStableUnderPermutation is this package's third of a
// guard the engine and executor packages carry too, under the same name.
//
// The class of defect is one this project has now fixed three times: an answer
// that singles out one object from many, arrived at by a comparison that is not
// total or a map that is not ordered, so the answer moves when nothing has. It
// was fixed across the engine once and came back in the eviction blockers and
// again here — which is why the guard is a shared helper and a repeated test
// name rather than an assertion inside whichever test noticed.
//
// A row here is an entry point that picks one object out of many. Add one when
// you add such an entry point; if the answer is *deliberately* order-dependent,
// the row says so and says why, rather than being left out.
func TestEveryReportedSubjectIsStableUnderPermutation(t *testing.T) {
	t.Run("the stuck pod a drain is handed back over", func(t *testing.T) {
		permute.Stable(t, wedged(), func(pods []*corev1.Pod) string {
			return drain.Assess(
				drain.State{Node: mother.SmallNode("a"), Pods: pods, Now: now}, policy(),
			).Reason
		})
	})

	t.Run("the sentence explain and the dry-run controller print", func(t *testing.T) {
		// The same assessment, one layer up: this is the string that reaches a
		// WouldAdvanceDrain event every evaluation interval on a dry-run
		// cluster, and the one `binpack explain` prints beside it. Two
		// frontends disagreeing about an unchanged node is the whole of what
		// this costs.
		//
		// Only the pods are permuted. The order of NodeAssessment.Blockers is
		// the engine's to guarantee — WouldHappen takes the first as given —
		// and the engine's row of this same guard is what holds it there.
		a := engine.NodeAssessment{Node: mother.SmallNode("a")}

		permute.Stable(t, wedged(), func(pods []*corev1.Pod) string {
			return drain.WouldHappen(a, drain.Assess(
				drain.State{Node: mother.SmallNode("a"), Pods: pods, Now: now}, policy(),
			))
		})
	})
}

// wedged is a batch of one workload's pods, all evicted together and all stuck
// the same distance past their deadline — which is what a batch of one
// workload's pods normally looks like, since they share a grace period.
func wedged() []*corev1.Pod {
	requested := now.Add(-2 * time.Hour)
	return []*corev1.Pod{
		mother.Pod("default", "web-1", mother.OnNode("a"),
			mother.Terminating(requested, time.Minute)),
		mother.Pod("default", "web-2", mother.OnNode("a"),
			mother.Terminating(requested, time.Minute)),
		mother.Pod("default", "web-3", mother.OnNode("a"),
			mother.Terminating(requested, time.Minute)),
	}
}
