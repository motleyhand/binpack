package engine_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"

	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
	"github.com/motleyhand/binpack/internal/permute"
)

// TestEveryReportedSubjectIsStableUnderPermutation is the engine's third of a
// guard the drain and executor packages carry too, under the same name.
//
// The class of defect is one this project has now fixed three times: an answer
// that singles out one object from many, arrived at by a comparison that is not
// total or a map that is not ordered, so the answer moves when nothing has. It
// was fixed across this package once and came back in the eviction blockers and
// again in the drain assessment — which is why the guard is a shared helper and
// a repeated test name rather than an assertion inside whichever test noticed.
//
// A row here is an entry point that picks one object out of many. Add one when
// you add such an entry point; if the answer is *deliberately* order-dependent,
// the row says so and says why, rather than being left out.
func TestEveryReportedSubjectIsStableUnderPermutation(t *testing.T) {
	t.Run("the node binpack chooses, whatever order the nodes arrived in", func(t *testing.T) {
		// b and c are both empty, so the choice between them is the tie-break
		// and nothing else.
		nodes := []*corev1.Node{inPool("a"), inPool("b"), inPool("c"), inPool("d")}
		pods := []*corev1.Pod{
			mother.Pod("default", "heavy", mother.OnNode("a"), mother.Requests("100m", "1Gi")),
		}

		permute.Stable(t, nodes, func(nodes []*corev1.Node) string {
			d := engine.Decide(cluster(nodes, pods), config())
			if d.Action != engine.Drain {
				t.Fatalf("expected a drain, got none: %s", d.Reason)
			}
			return d.Node.Name
		})
	})

	t.Run("the node binpack chooses, whatever order the pods arrived in", func(t *testing.T) {
		nodes := []*corev1.Node{inPool("a"), inPool("b"), inPool("c")}
		// Equal loads, so which of a and b is emptiest is a tie the ordering
		// must not settle.
		pods := []*corev1.Pod{
			mother.Pod("default", "one", mother.OnNode("a"), mother.Requests("100m", "512Mi")),
			mother.Pod("default", "two", mother.OnNode("b"), mother.Requests("100m", "512Mi")),
			mother.Pod("default", "three", mother.OnNode("c"), mother.Requests("100m", "512Mi")),
		}

		permute.Stable(t, pods, func(pods []*corev1.Pod) string {
			d := engine.Decide(cluster(nodes, pods), config())
			if d.Action != engine.Drain {
				t.Fatalf("expected a drain, got none: %s", d.Reason)
			}
			return d.Node.Name
		})
	})

	t.Run("the budget a shortfall blames, whatever order the pods arrived in", func(t *testing.T) {
		pods, pdbs := twoShortBudgets()

		permute.Stable(t, pods, func(pods []*corev1.Pod) string {
			return firstBlocker(t, engine.CheckEvictable(pods, pdbs, engine.DefaultEvictConfig(), now))
		})
	})

	t.Run("the budget a shortfall blames, whatever order the budgets arrived in", func(t *testing.T) {
		pods, pdbs := twoShortBudgets()

		permute.Stable(t, pdbs, func(pdbs []*policyv1.PodDisruptionBudget) string {
			return firstBlocker(t, engine.CheckEvictable(pods, pdbs, engine.DefaultEvictConfig(), now))
		})
	})

	t.Run("the budgets a doubly-covered pod is told about", func(t *testing.T) {
		// The eviction API refuses such a pod outright, so the message is the
		// operator's whole route to the two objects to reconcile — and it
		// names two of them however many there are.
		labels := map[string]string{"app": "web"}
		pods := []*corev1.Pod{mother.Pod("default", "web-1", mother.PodLabels(labels))}
		pdbs := []*policyv1.PodDisruptionBudget{
			mother.PDB("default", "team-wide", 1, labels),
			mother.PDB("default", "app-web", 1, labels),
			mother.PDB("default", "namespace-default", 1, labels),
		}

		permute.Stable(t, pdbs, func(pdbs []*policyv1.PodDisruptionBudget) string {
			got := engine.CheckEvictable(pods, pdbs, engine.DefaultEvictConfig(), now)
			if len(got) != 1 || got[0].Code != engine.BlockedMultiplePDBs {
				t.Fatalf("expected one multiple-budget blocker, got %v", codes(got))
			}
			return got[0].PDB + ": " + got[0].Message
		})
	})

	t.Run("the pod evicted first", func(t *testing.T) {
		// What the executor evicts next is the head of this list, so a tie
		// here is a tie in which pod actually leaves the cluster first.
		nodes := []*corev1.Node{inPool("a"), inPool("b")}
		pods := []*corev1.Pod{
			mother.Pod("default", "one", mother.OnNode("a"), mother.Requests("100m", "256Mi")),
			mother.Pod("default", "two", mother.OnNode("a"), mother.Requests("100m", "256Mi")),
			mother.Pod("default", "three", mother.OnNode("a"), mother.Requests("100m", "256Mi")),
		}

		permute.Stable(t, pods, func(pods []*corev1.Pod) string {
			sim := engine.Simulate(nodes, pods, mother.Templates(pods...), nodes[0],
				engine.SimConfig{ExpendablePriorityCutoff: cutoff})
			if !sim.Feasible || len(sim.Relocated) == 0 {
				t.Fatalf("expected a feasible packing, got %+v", sim.Blocked)
			}
			return podRef(sim.Relocated[0].Pod)
		})
	})

	t.Run("the pod the simulation could not place", func(t *testing.T) {
		// Two pods no destination can hold, alike in every dimension the
		// packing ranks by. Which one is reported is the operator's starting
		// point, so it cannot be whichever the list happened to reach first.
		nodes := []*corev1.Node{inPool("a"), sized("b", "1Gi", mother.InPool(poolName, poolID))}
		pods := []*corev1.Pod{
			mother.Pod("default", "one", mother.OnNode("a"), mother.Requests("100m", "3Gi")),
			mother.Pod("default", "two", mother.OnNode("a"), mother.Requests("100m", "3Gi")),
		}

		permute.Stable(t, pods, func(pods []*corev1.Pod) string {
			sim := engine.Simulate(nodes, pods, mother.Templates(pods...), nodes[0],
				engine.SimConfig{ExpendablePriorityCutoff: cutoff})
			if sim.Feasible || sim.Blocked == nil || sim.Blocked.Pod == nil {
				t.Fatalf("expected a named blocked pod, got feasible=%v", sim.Feasible)
			}
			return podRef(sim.Blocked.Pod)
		})
	})
}

// twoShortBudgets is a node under two budgets each short by exactly one, so
// neither is the obvious answer and the report has to choose between them.
func twoShortBudgets() ([]*corev1.Pod, []*policyv1.PodDisruptionBudget) {
	alpha := map[string]string{"app": "alpha"}
	bravo := map[string]string{"app": "bravo"}
	return []*corev1.Pod{
		mother.Pod("default", "alpha-1", mother.PodLabels(alpha)),
		mother.Pod("default", "alpha-2", mother.PodLabels(alpha)),
		mother.Pod("default", "bravo-1", mother.PodLabels(bravo)),
		mother.Pod("default", "bravo-2", mother.PodLabels(bravo)),
	}, []*policyv1.PodDisruptionBudget{
		mother.PDB("default", "alpha-pdb", 1, alpha),
		mother.PDB("default", "bravo-pdb", 1, bravo),
	}
}

func firstBlocker(t *testing.T, got []engine.EvictionBlocker) string {
	t.Helper()
	if len(got) != 2 {
		t.Fatalf("both budgets should be short, got %v", codes(got))
	}
	return got[0].PDB
}

func podRef(pod *corev1.Pod) string { return pod.Namespace + "/" + pod.Name }
