package executor_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/motleyhand/binpack/internal/executor"
	"github.com/motleyhand/binpack/internal/mother"
	"github.com/motleyhand/binpack/internal/permute"
)

// TestEveryReportedSubjectIsStableUnderPermutation is this package's third of a
// guard the engine and drain packages carry too, under the same name.
//
// The class of defect is one this project has now fixed three times: an answer
// that singles out one object from many, arrived at by a comparison that is not
// total or a map that is not ordered, so the answer moves when nothing has.
// Here the answer is not a sentence but an eviction, which is why the row runs
// the whole of Advance rather than reading a field: what is being asserted is
// that two evaluations of an unchanged node delete the same pod.
//
// A row here is an entry point that picks one object out of many. Add one when
// you add such an entry point; if the answer is *deliberately* order-dependent,
// the row says so and says why, rather than being left out.
func TestEveryReportedSubjectIsStableUnderPermutation(t *testing.T) {
	t.Run("the pod one step of a drain evicts", func(t *testing.T) {
		// Three pods alike in every dimension the packing ranks by, so which
		// one goes first is the tie-break and nothing else. Advance evicts one
		// per step, and the ordering it walks is the simulation's — so a tie
		// settled by list order means the controller and `explain` disagree
		// about which pod is about to be deleted.
		pods := []*corev1.Pod{
			mother.Pod("default", "one", mother.OnNode("a")),
			mother.Pod("default", "two", mother.OnNode("a")),
			mother.Pod("default", "three", mother.OnNode("a")),
		}
		nodes := []*corev1.Node{marked("a", time.Minute, time.Minute, "3"), node("b")}

		permute.Stable(t, pods, func(pods []*corev1.Pod) string {
			s := snapshot(nodes, pods)
			c := clientFor(s)

			step, err := executor.Advance(
				context.Background(), c, s, "a", engineConfig(), drainPolicy())
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if step.Code != executor.StepEvicted {
				t.Fatalf("expected an eviction, got %+v", step)
			}
			return evicted(t, c, pods)
		})
	})
}

// evicted names the one pod of want that Advance deleted.
func evicted(t *testing.T, c client.Client, want []*corev1.Pod) string {
	t.Helper()

	var left corev1.PodList
	if err := c.List(context.Background(), &left); err != nil {
		t.Fatalf("listing pods: %v", err)
	}
	remaining := make(map[string]bool, len(left.Items))
	for i := range left.Items {
		remaining[left.Items[i].Namespace+"/"+left.Items[i].Name] = true
	}

	var gone []string
	for _, pod := range want {
		if ref := pod.Namespace + "/" + pod.Name; !remaining[ref] {
			gone = append(gone, ref)
		}
	}
	if len(gone) != 1 {
		t.Fatalf("expected exactly one pod evicted, %d were: %v", len(gone), gone)
	}
	return gone[0]
}
