package engine_test

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"

	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

// draining marks a node the way binpack marks one it has started draining:
// cordoned, with the drain marker set.
func draining() []mother.NodeOption {
	return []mother.NodeOption{
		mother.Cordoned(),
		mother.NodeAnnotations(map[string]string{
			engine.AnnotationDrainStarted: now.Add(-5 * time.Minute).Format(time.RFC3339),
		}),
	}
}

func TestRevalidateAgreesWithTheDecisionThatChoseTheNode(t *testing.T) {
	// The property the whole extraction exists for. If selection and
	// revalidation could answer differently, binpack would cordon a node on
	// one basis and evict its pods on another, and nothing would reveal it.
	// Each case also pins the verdict it is meant to produce. Agreement alone
	// would be satisfied by two functions that both said "skipped" for some
	// unrelated reason, which would test nothing at all.
	for _, tc := range []struct {
		name    string
		verdict string
		pods    []*corev1.Pod
	}{
		{"an empty node", engine.VerdictDrainable, nil},
		{"a node whose pods fit elsewhere", engine.VerdictDrainable, []*corev1.Pod{
			mother.Pod("default", "light", mother.OnNode("a"), mother.Requests("100m", "128Mi")),
		}},
		{"a node whose pods do not fit", engine.VerdictInfeasible, []*corev1.Pod{
			mother.Pod("default", "huge", mother.OnNode("a"), mother.Requests("100m", "3Gi")),
			mother.Pod("default", "filler", mother.OnNode("b"), mother.Requests("100m", "3Gi")),
			mother.Pod("default", "filler2", mother.OnNode("c"), mother.Requests("100m", "3Gi")),
		}},
		{"a node whose pods are blocked by a budget", engine.VerdictBlocked, []*corev1.Pod{
			mother.Pod("default", "web", mother.OnNode("a"),
				mother.PodLabels(map[string]string{"app": "web"})),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := []*corev1.Node{inPool("a"), inPool("b"), inPool("c")}
			s := cluster(nodes, tc.pods)
			if tc.name == "a node whose pods are blocked by a budget" {
				s.PDBs = []*policyv1.PodDisruptionBudget{
					mother.PDB("default", "web", 0, map[string]string{"app": "web"}),
				}
			}

			chosen := assessmentFor(engine.Decide(s, config()), "a")
			if chosen == nil {
				t.Fatal("node a was not assessed at all")
			}

			// Same cluster, but the node is now marked and cordoned, exactly
			// as binpack would have left it.
			marked := cluster(
				[]*corev1.Node{inPool("a", draining()...), inPool("b"), inPool("c")}, tc.pods)
			marked.PDBs = s.PDBs

			got := engine.Revalidate(marked, "a", config())

			if chosen.Verdict() != tc.verdict {
				t.Errorf("the decision said %q, but this case is meant to be %q",
					chosen.Verdict(), tc.verdict)
			}
			if got.Verdict() != tc.verdict {
				t.Errorf("revalidate said %q, but this case is meant to be %q (%s)",
					got.Verdict(), tc.verdict, got.SkipReason)
			}
			if got.Verdict() != chosen.Verdict() {
				t.Errorf("revalidate says %q, the decision said %q (%s / %s)",
					got.Verdict(), chosen.Verdict(), got.SkipReason, chosen.SkipReason)
			}
		})
	}
}

func TestRevalidateIgnoresOnlyWhatBinpackItselfDid(t *testing.T) {
	// The cordon and the marker are binpack's own work, so treating them as
	// reasons to stop would abandon every drain on its second evaluation.
	s := cluster([]*corev1.Node{inPool("a", draining()...), inPool("b"), inPool("c")}, nil)

	if got := engine.Revalidate(s, "a", config()); got.Verdict() != engine.VerdictDrainable {
		t.Errorf("got %q (%s), want the drain to continue", got.Verdict(), got.SkipReason)
	}
}

func TestRevalidateStopsADrainTheClusterHasOvertaken(t *testing.T) {
	// Everything that is not binpack's own marker still applies. Each of these
	// happened after the node was cordoned, and each means the drain should
	// not continue — which is the entire reason for re-asking rather than
	// trusting the decision that started it.
	for _, tc := range []struct {
		name string
		want string
		to   func(*engine.Snapshot)
	}{
		{"a scale-up began", engine.SkipScaleUpInProgress, func(s *engine.Snapshot) {
			s.Autoscaler.ScaleUpInProgress = true
		}},
		{"the pool reached its minimum", engine.SkipPoolAtMinimum, func(s *engine.Snapshot) {
			s.Autoscaler.Groups[0].MinSize = 3
		}},
		{"the autoscaler stopped reporting", engine.SkipNotAutoscaled, func(s *engine.Snapshot) {
			s.Autoscaler.LastProbe = now.Add(-time.Hour)
		}},
		{"an operator asked binpack to leave the node alone", engine.SkipAnnotated,
			func(s *engine.Snapshot) {
				s.Nodes[0] = inPool("a", append(draining(),
					mother.NodeAnnotations(map[string]string{engine.AnnotationSkip: "true"}))...)
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := cluster([]*corev1.Node{inPool("a", draining()...), inPool("b"), inPool("c")}, nil)
			tc.to(&s)

			got := engine.Revalidate(s, "a", config())

			if !got.Skipped || got.SkipCode != tc.want {
				t.Errorf("got skipped=%v code=%q (%s), want %q",
					got.Skipped, got.SkipCode, got.SkipReason, tc.want)
			}
		})
	}
}

func TestRevalidateRefusesAMarkedNodeThatIsNotCordoned(t *testing.T) {
	// The controller stopping between writing the marker and cordoning, or
	// somebody uncordoning a drain in flight. The node is accepting pods
	// again, so anything evicted from it could be relocating work whose fit
	// and PDB demand were never assessed — the race the cordon closes.
	s := cluster([]*corev1.Node{
		inPool("a", mother.NodeAnnotations(map[string]string{
			engine.AnnotationDrainStarted: now.Add(-5 * time.Minute).Format(time.RFC3339),
		})),
		inPool("b"), inPool("c"),
	}, nil)

	got := engine.Revalidate(s, "a", config())

	if !got.Skipped || got.SkipCode != engine.SkipUncordoned {
		t.Errorf("got skipped=%v code=%q (%s), want %q",
			got.Skipped, got.SkipCode, got.SkipReason, engine.SkipUncordoned)
	}
}

func TestRevalidateTreatsAMissingNodeAsGone(t *testing.T) {
	// The autoscaler removing the node is what a drain is working towards, so
	// finding it absent is success arriving early rather than an error.
	s := cluster([]*corev1.Node{inPool("b"), inPool("c")}, nil)

	got := engine.Revalidate(s, "a", config())

	if got.SkipCode != engine.SkipGone {
		t.Errorf("got %q (%s), want %q", got.SkipCode, got.SkipReason, engine.SkipGone)
	}
	if got.Node == nil || got.Node.Name != "a" {
		t.Error("the assessment should still name the node it was asked about")
	}
}

// reserving is a cluster where the drain of "a" is feasible but leaves nowhere
// for a pod the size of the largest relocatable one — so the reserve refuses it
// and nothing else does.
func reserving(t *testing.T, extra ...mother.NodeOption) (engine.Snapshot, engine.Config) {
	t.Helper()
	pods := []*corev1.Pod{
		mother.Pod("default", "small", mother.OnNode("a"), mother.Requests("100m", "1Gi")),
		mother.Pod("default", "large", mother.OnNode("b"), mother.Requests("100m", "2Gi")),
	}
	s := cluster([]*corev1.Node{inPool("a", extra...), inPool("b")}, pods)

	cfg := config()
	cfg.Default.Sim.ReserveForLargestPod = true
	return s, cfg
}

func TestTheReserveRefusesADrainThatHasNotStarted(t *testing.T) {
	// The other half of the pair below: without this, the test that the reserve
	// stops applying would pass against a cluster where it never applied.
	s, cfg := reserving(t, draining()...)
	s.Nodes[0].Annotations[engine.AnnotationDrainAwaiting] = ""

	if got := engine.Revalidate(s, "a", cfg); got.Verdict() == engine.VerdictDrainable {
		t.Error("the reserve should refuse this drain before anything has been evicted")
	}
}

func TestTheReserveStopsApplyingOncePodsHaveLeft(t *testing.T) {
	// Re-asking a preference after the work has begun does not undo the work.
	// It abandons a half-drained node, which leaves the cluster worse than
	// either finishing or never starting — and on a real cluster it bounced a
	// pod off every node in the pool in turn. See ADR-0009.
	s, cfg := reserving(t, draining()...)
	s.Nodes[0].Annotations[engine.AnnotationDrainAwaiting] = "some-uid@2026-08-17T12:00:00Z"

	got := engine.Revalidate(s, "a", cfg)

	if got.Verdict() != engine.VerdictDrainable {
		t.Errorf("a committed drain was abandoned by a preference: %q (%s)",
			got.Verdict(), revalidationDetail(got))
	}
}

func TestSoundnessIsStillReAskedAfterPodsHaveLeft(t *testing.T) {
	// The distinction is preference versus soundness, not "checks before" and
	// "no checks after". A drain whose remaining pods no longer fit anywhere
	// must still stop, however much work has been done.
	pods := []*corev1.Pod{
		mother.Pod("default", "huge", mother.OnNode("a"), mother.Requests("100m", "3Gi")),
		mother.Pod("default", "filler", mother.OnNode("b"), mother.Requests("100m", "3Gi")),
	}
	s := cluster([]*corev1.Node{inPool("a", draining()...), inPool("b")}, pods)
	s.Nodes[0].Annotations[engine.AnnotationDrainAwaiting] = "some-uid@2026-08-17T12:00:00Z"

	if got := engine.Revalidate(s, "a", config()); got.Verdict() != engine.VerdictInfeasible {
		t.Errorf("got %q, want the drain stopped: its pods no longer fit", got.Verdict())
	}
}

func TestEvictabilityIsStillReAskedAfterPodsHaveLeft(t *testing.T) {
	// The other soundness question. A budget that stops allowing disruption
	// mid-drain must stop the drain, however much work has been done — binpack
	// must never be the thing that took an application below its declared
	// availability, and no amount of sunk cost changes that.
	pods := []*corev1.Pod{
		mother.Pod("default", "web", mother.OnNode("a"),
			mother.PodLabels(map[string]string{"app": "web"})),
	}
	s := cluster([]*corev1.Node{inPool("a", draining()...), inPool("b")}, pods)
	s.Nodes[0].Annotations[engine.AnnotationDrainAwaiting] = "some-uid@2026-08-17T12:00:00Z"
	s.PDBs = []*policyv1.PodDisruptionBudget{
		mother.PDB("default", "web", 0, map[string]string{"app": "web"}),
	}

	if got := engine.Revalidate(s, "a", config()); got.Verdict() != engine.VerdictBlocked {
		t.Errorf("got %q, want the drain stopped: its pods cannot be evicted", got.Verdict())
	}
}

func revalidationDetail(a engine.NodeAssessment) string {
	if a.Simulation != nil && a.Simulation.Blocked != nil {
		return a.Simulation.Blocked.Summary
	}
	return a.SkipReason
}
