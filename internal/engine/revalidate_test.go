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
