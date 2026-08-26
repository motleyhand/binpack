package engine_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"

	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

// TestEverySkipCodeDecideProducesIsEnumerated is the guard the enumerators
// exist for and did not have.
//
// [engine.SkipCodes] says it is "every reason a node can be ruled out", and
// three separate lists of that one set had all drifted from it in the same
// direction: a code added to the constant block, assigned in eligibility, and
// added to none of them. Nothing failed, because every list was hand-written
// by whoever last remembered.
//
// Driving Decide is what makes this different from a fourth list. A code that
// no branch can produce cannot make it pass, and a branch added later without
// a constant in the enumerator fails it the first time a fixture reaches the
// branch.
func TestEverySkipCodeDecideProducesIsEnumerated(t *testing.T) {
	enumerated := engine.SkipCodes()

	for _, tc := range []struct {
		name string
		want string
		s    engine.Snapshot
	}{
		{
			// The one that was missing. The autoscaler tainting a node it has
			// committed to deleting is ordinary scale-down, so this reaches
			// binpack_nodes_skipped on any autoscaling cluster.
			name: "the autoscaler is already removing the node",
			want: engine.SkipBeingRemoved,
			s: cluster([]*corev1.Node{
				inPool("a", mother.Tainted(
					engine.TaintToBeDeleted, "1786971071", corev1.TaintEffectNoSchedule)),
				inPool("b"), inPool("c"),
			}, nil),
		},
		{
			name: "somebody else cordoned it",
			want: engine.SkipCordoned,
			s: cluster([]*corev1.Node{inPool("a", mother.Cordoned()), inPool("b"), inPool("c")},
				nil),
		},
		{
			name: "the operator annotated it",
			want: engine.SkipAnnotated,
			s: cluster([]*corev1.Node{
				inPool("a", mother.NodeAnnotations(map[string]string{engine.AnnotationSkip: "true"})),
				inPool("b"), inPool("c"),
			}, nil),
		},
		{
			name: "a drain of it failed recently",
			want: engine.SkipBackoff,
			s: cluster([]*corev1.Node{
				inPool("a", mother.NodeAnnotations(map[string]string{
					engine.AnnotationBackoffUntil: now.Add(time.Hour).Format(time.RFC3339),
					engine.AnnotationLastFailure:  "eviction refused",
				})),
				inPool("b"), inPool("c"),
			}, nil),
		},
		{
			name: "its pool is not one the autoscaler manages",
			want: engine.SkipNotAutoscaled,
			s: cluster([]*corev1.Node{
				sized("stray", "4Gi", mother.InPool("unmanaged", "no-such-group")),
				inPool("b"), inPool("c"),
			}, nil),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := engine.Decide(tc.s, config())

			a := assessmentFor(d, tc.s.Nodes[0].Name)
			if a == nil {
				t.Fatalf("%s was not assessed at all", tc.s.Nodes[0].Name)
			}
			if a.SkipCode != tc.want {
				t.Fatalf("skip code = %q, want %q — the fixture no longer reaches the branch",
					a.SkipCode, tc.want)
			}
			if !slices.Contains(enumerated, a.SkipCode) {
				t.Errorf("Decide produced %q, which SkipCodes() does not enumerate: "+
					"it is published as binpack_nodes_skipped{code=%q} and as a node's "+
					"code in explain --output json, and nothing documents it",
					a.SkipCode, a.SkipCode)
			}
		})
	}
}

// TestEverySkipCodeRevalidateProducesIsEnumerated is the same property for the
// other entry point, which has codes of its own.
//
// Revalidate answers a different question from Decide — "can this drain still
// finish" rather than "should this node be chosen" — and three of its codes
// are unreachable from selection. They are published all the same, as
// binpack_drains_abandoned_total{reason}, so an enumeration that stops at the
// selection path leaves the abandonment vocabulary unguarded.
func TestEverySkipCodeRevalidateProducesIsEnumerated(t *testing.T) {
	enumerated := engine.SkipCodes()

	marked := func(opts ...mother.NodeOption) *corev1.Node {
		return inPool("a", append([]mother.NodeOption{
			mother.NodeAnnotations(map[string]string{
				engine.AnnotationDrainStarted: now.Add(-5 * time.Minute).Format(time.RFC3339),
			}),
		}, opts...)...)
	}

	for _, tc := range []struct {
		name string
		want string
		s    func() engine.Snapshot
	}{
		{
			name: "the node has gone",
			want: engine.SkipGone,
			s: func() engine.Snapshot {
				return cluster([]*corev1.Node{inPool("b"), inPool("c")}, nil)
			},
		},
		{
			name: "the drain is marked but the node is schedulable",
			want: engine.SkipUncordoned,
			s: func() engine.Snapshot {
				return cluster([]*corev1.Node{marked(), inPool("b"), inPool("c")}, nil)
			},
		},
		{
			// The split PUBLIC-01 asked for. Before it, a drain stranded by a
			// dead autoscaler and a node whose pool was never autoscaled
			// abandoned under one reason, and the two need different answers
			// from whoever reads the alert.
			name: "the autoscaler stopped reporting",
			want: engine.SkipAutoscalerNotLive,
			s: func() engine.Snapshot {
				s := cluster([]*corev1.Node{marked(mother.Cordoned()), inPool("b"), inPool("c")}, nil)
				s.Autoscaler.LastProbe = now.Add(-engine.MaxStatusAge - time.Minute)
				return s
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := engine.Revalidate(tc.s(), "a", config())

			if a.SkipCode != tc.want {
				t.Fatalf("skip code = %q, want %q — the fixture no longer reaches the branch",
					a.SkipCode, tc.want)
			}
			if !slices.Contains(enumerated, a.SkipCode) {
				t.Errorf("Revalidate produced %q, which SkipCodes() does not enumerate: "+
					"it is published as binpack_drains_abandoned_total{reason=%q} and "+
					"nothing documents it", a.SkipCode, a.SkipCode)
			}
		})
	}
}

// TestEveryDocumentedDiagnosisIsReachable asks the question the two
// catalogue-against-reference tests never did.
//
// Both of those compare the catalogue with docs/reference/diagnostics.md, in
// both directions, so an entry present in both passes however it got there.
// `mirror-pod` was present in both and producible by neither: `Classify` puts
// a mirror pod in the node-local class, so it is never simulated and never
// evicted, and both of checkPod's callers filter it out before the branch
// declaring it blocking could run. The reference documented, at `blocking`
// severity, a diagnosis binpack could not emit — and asserted of Kubernetes
// something that is not true of it either.
//
// The fixtures are deliberately several small clusters rather than one large
// one. Some of these codes are mutually exclusive by construction: a snapshot
// with no autoscaler produces no per-pool findings at all, and a pod whose
// template cannot be read is not a pod whose template diverges.
func TestEveryDocumentedDiagnosisIsReachable(t *testing.T) {
	diverged := func(pod *corev1.Pod) engine.Snapshot {
		s := cluster([]*corev1.Node{inPool("a"), inPool("b")}, []*corev1.Pod{pod})
		mother.TemplateFor(s.Templates, pod) // the template lacks the pod's own constraint
		return s
	}

	pods := []*corev1.Pod{
		mother.Pod("default", "orphan", mother.OnNode("a"), mother.Bare()),
		mother.Pod("default", "cache", mother.OnNode("a"), mother.WithEmptyDir("scratch")),
		mother.Pod("kube-system", "coredns", mother.OnNode("a")),
		mother.Pod("default", "pinned", mother.OnNode("a"), mother.SafeToEvict("false")),
		mother.PausePod("default", "filler", mother.OnNode("a"), mother.Priority(cutoff-1)),
		mother.Pod("default", "double", mother.OnNode("a"),
			mother.PodLabels(map[string]string{"app": "web", "tier": "front"})),
		mother.Pod("default", "shard-0", mother.OnNode("a"),
			mother.OwnedBy("KafkaCluster", "events")),
	}
	pdbs := []*policyv1.PodDisruptionBudget{
		mother.PDB("default", "by-app", 1, map[string]string{"app": "web"}),
		mother.PDB("default", "by-tier", 1, map[string]string{"tier": "front"}),
		mother.PDB("default", "zero", 0, map[string]string{"app": "zero"}),
		mother.Healthy(mother.PDB("default", "sick", 0, map[string]string{"app": "sick"}), 1, 2),
		mother.SelectsNothing(mother.PDB("default", "empty", 0, map[string]string{"app": "gone"})),
		mother.Stale(mother.PDB("default", "edited", 1, map[string]string{"app": "edited"})),
		mother.SyncFailed(mother.PDB("default", "unsyncable", 0, map[string]string{"app": "orphaned"}),
			`found no controllers for pod "orphaned-0"`),
		mother.UnparseableSelector(mother.PDB("default", "grandfathered", 1, nil)),
	}

	crowded := cluster([]*corev1.Node{
		inPool("a", mother.Cordoned(), mother.NodeAnnotations(map[string]string{
			engine.AnnotationDrainStarted: now.Add(-time.Hour).Format(time.RFC3339),
			engine.AnnotationBackoffUntil: now.Add(time.Hour).Format(time.RFC3339),
			engine.AnnotationLastFailure:  "eviction refused",
		})),
		sized("static", "8Gi", mother.InPool("pool-8g", "static-id")),
	}, pods)
	crowded.PDBs = pdbs
	crowded.Autoscaler.ScaleDownStatus = "NoCandidates"
	crowded.Autoscaler.Groups = []engine.NodeGroup{{ID: poolID, MinSize: 1, MaxSize: 10, Ready: 1}}

	dead := cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
	dead.Autoscaler = engine.Autoscaler{}

	seen := map[string]bool{}
	for _, s := range []engine.Snapshot{
		crowded,
		dead,
		diverged(mother.Pod("default", "web", mother.OnNode("a"),
			mother.WithNodeSelector("tier", "app"))),
	} {
		for _, f := range engine.Diagnose(s, config()) {
			seen[f.Code] = true
		}
	}

	for _, d := range engine.Diagnoses() {
		if !seen[d.Code] {
			t.Errorf("%s is catalogued and documented, and no cluster produces it: "+
				"either a fixture is missing here, or the entry describes something "+
				"binpack cannot observe", d.Code)
		}
	}
}

// TestAStaticPodDoesNotBlockADrain is the behaviour the reference contradicted.
//
// docs/reference/diagnostics.md carried a `mirror-pod` entry at Blocking
// severity — a class it defines as "their nodes cannot be removed by anything"
// — saying a node could not be drained while a static pod ran on it. binpack
// drains it, silently and correctly: Classify puts a mirror pod in the
// node-local class, so it is neither simulated nor evicted, and it dies with
// the node the way a DaemonSet pod does.
//
// Asserted from Decide rather than from CheckEvictable, because CheckEvictable
// is contractually never handed a node-local pod. Calling it directly with one
// is what kept the deleted branch looking covered.
func TestAStaticPodDoesNotBlockADrain(t *testing.T) {
	s := cluster([]*corev1.Node{inPool("a"), inPool("b"), inPool("c")},
		[]*corev1.Pod{mother.MirrorPod("kube-system", "static", mother.OnNode("a"))})

	d := engine.Decide(s, config())

	a := assessmentFor(d, "a")
	if a == nil {
		t.Fatalf("node a was not assessed")
	}
	if a.Verdict() != engine.VerdictDrainable {
		t.Errorf("node a is %s (%s), want drainable: a static pod dies with its node",
			a.Verdict(), a.SkipReason)
	}
	for _, b := range a.Blockers {
		t.Errorf("node a is blocked by %s: %s", b.Code, b.Message)
	}

	// And no diagnosis says otherwise. An operator running a kubeadm-style
	// pool reads the reference to find out why their nodes are not
	// consolidating; it must not answer with something binpack does not think.
	for _, f := range engine.Diagnose(s, config()) {
		if strings.Contains(f.Summary, "static pod") || strings.Contains(f.Fix, "static pod") {
			t.Errorf("diagnose says %q — %s", f.Code, f.Summary)
		}
	}
}

// TestEveryEnumeratedVocabularyIsASet holds the property every other guard in
// this file assumes without checking: that an enumerator's values are distinct.
//
// These lists are turned into sets — by the reachability loop in decide_test.go,
// by the metrics reference guards, by the label-value pre-initialisation — and
// a set silently absorbs a duplicate. Two constants sharing one value therefore
// collapse into one enumerated code, every fixture goes on asserting its own
// constant and passing, and the reference updated to the resulting vocabulary
// agrees with it. What is lost is the distinction itself: two operational
// causes that an alert cannot tell apart, reported under one label value, with
// nothing anywhere saying so.
//
// The names cannot be recovered from the values, which is why this reports the
// value and leaves finding the pair to a grep. Constants are the only place
// the names exist.
func TestEveryEnumeratedVocabularyIsASet(t *testing.T) {
	// The three hand-written slices. [engine.Diagnoses] is deliberately absent:
	// it is built from a map keyed by the code, so a duplicate there is not a
	// thing that can be written. That is the shape to prefer, and the reason
	// these three need a guard is that they are not it.
	for _, vocabulary := range []struct {
		name   string
		values []string
	}{
		{"SkipCodes", engine.SkipCodes()},
		{"Verdicts", engine.Verdicts()},
		{"DecisionCodes", engine.DecisionCodes()},
	} {
		seen := map[string]bool{}
		for _, value := range vocabulary.values {
			if seen[value] {
				t.Errorf("%s() enumerates %q twice, so two constants share it: the pair "+
					"is one label value now, and every check that turns this list into a "+
					"set reports them as one thing an operator cannot tell apart",
					vocabulary.name, value)
			}
			seen[value] = true
		}
		if len(vocabulary.values) == 0 {
			t.Errorf("%s() enumerates nothing, so this asserts nothing about it",
				vocabulary.name)
		}
	}
}
