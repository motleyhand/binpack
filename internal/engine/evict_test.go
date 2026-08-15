package engine_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"

	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

func codes(blockers []engine.EvictionBlocker) []string {
	out := make([]string, len(blockers))
	for i, b := range blockers {
		out[i] = b.Code
	}
	return out
}

func TestCheckEvictablePerPod(t *testing.T) {
	cfg := engine.DefaultEvictConfig()

	tests := []struct {
		name string
		pod  *corev1.Pod
		want string // empty means evictable
	}{
		{
			name: "ordinary controller-owned pod",
			pod:  mother.Pod("default", "web"),
		},
		{
			name: "explicitly refused",
			pod:  mother.Pod("default", "web", mother.SafeToEvict("false")),
			want: engine.BlockedSafeToEvict,
		},
		{
			name: "bare pod would be destroyed",
			pod:  mother.Pod("default", "orphan", mother.Bare()),
			want: engine.BlockedBarePod,
		},
		{
			name: "emptyDir counts as local storage",
			pod:  mother.Pod("default", "cache", mother.WithEmptyDir("scratch")),
			want: engine.BlockedLocalStorage,
		},
		{
			name: "hostPath counts as local storage",
			pod:  mother.Pod("default", "agent", mother.WithHostPathVolume("logs", "/var/log")),
			want: engine.BlockedLocalStorage,
		},
		{
			// The documented escape hatch: an operator asserting the volume
			// is disposable.
			name: "local storage explicitly permitted",
			pod: mother.Pod("default", "cache",
				mother.WithEmptyDir("scratch"), mother.SafeToEvict("true")),
		},
		{
			name: "kube-system pod with no budget blocks node removal",
			pod:  mother.Pod("kube-system", "coredns"),
			want: engine.BlockedSystemPod,
		},
		{
			// An owner reference without Controller set is a
			// garbage-collection link, not a controller that would recreate
			// the pod. Counting it would let a pod be deleted permanently.
			name: "owner reference that does not control is still bare",
			pod:  mother.Pod("default", "orphan", mother.OwnedButNotControlled("ReplicaSet", "rs")),
			want: engine.BlockedBarePod,
		},
		{
			name: "kube-system pod explicitly permitted",
			pod:  mother.Pod("kube-system", "coredns", mother.SafeToEvict("true")),
		},
		{
			name: "mirror pod cannot be evicted at all",
			pod:  mother.MirrorPod("kube-system", "static"),
			want: engine.BlockedMirrorPod,
		},
		{
			// A refusal must win over a permission, or the annotation could
			// be used to override the kubelet, which it cannot.
			name: "explicit refusal beats every exemption",
			pod:  mother.Pod("default", "web", mother.SafeToEvict("false"), mother.Bare()),
			want: engine.BlockedSafeToEvict,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := engine.CheckEvictable([]*corev1.Pod{tc.pod}, nil, cfg)

			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("expected evictable, blocked by %v: %s", codes(got), got[0].Message)
				}
				return
			}
			if len(got) != 1 || got[0].Code != tc.want {
				t.Fatalf("codes = %v, want [%s]", codes(got), tc.want)
			}
			if got[0].Message == "" {
				t.Error("a blocker must carry a message naming the cause")
			}
		})
	}
}

func TestAutoscalerFlagsAreHonoured(t *testing.T) {
	// A self-hosted autoscaler may be configured differently. binpack
	// predicts what the autoscaler will do, so its assumptions follow.
	permissive := engine.EvictConfig{}

	local := mother.Pod("default", "cache", mother.WithEmptyDir("scratch"))
	system := mother.Pod("kube-system", "coredns")

	if got := engine.CheckEvictable([]*corev1.Pod{local, system}, nil, permissive); len(got) != 0 {
		t.Errorf("with both skip flags off, neither should block, got %v", codes(got))
	}
}

func TestPDBDemandIsAggregated(t *testing.T) {
	// Two pods of one Deployment on the same node, one allowed disruption.
	// A per-pod check passes this and then half-drains the node.
	labelled := map[string]string{"app": "web"}
	pods := []*corev1.Pod{
		mother.Pod("default", "web-1", mother.PodLabels(labelled)),
		mother.Pod("default", "web-2", mother.PodLabels(labelled)),
	}
	pdbs := []*policyv1.PodDisruptionBudget{
		mother.PDB("default", "web", 1, labelled),
	}

	got := engine.CheckEvictable(pods, pdbs, engine.DefaultEvictConfig())

	if len(got) != 1 || got[0].Code != engine.BlockedPDBInsufficint {
		t.Fatalf("codes = %v, want [%s]", codes(got), engine.BlockedPDBInsufficint)
	}
	if !strings.Contains(got[0].Message, "allows 1 disruption(s) but the drain needs 2") {
		t.Errorf("message should show the arithmetic, got: %s", got[0].Message)
	}
}

func TestPDBWithEnoughSlackIsFine(t *testing.T) {
	labelled := map[string]string{"app": "web"}
	pods := []*corev1.Pod{
		mother.Pod("default", "web-1", mother.PodLabels(labelled)),
		mother.Pod("default", "web-2", mother.PodLabels(labelled)),
	}
	pdbs := []*policyv1.PodDisruptionBudget{mother.PDB("default", "web", 2, labelled)}

	if got := engine.CheckEvictable(pods, pdbs, engine.DefaultEvictConfig()); len(got) != 0 {
		t.Errorf("two allowed disruptions cover two evictions, got %v", codes(got))
	}
}

func TestZeroDisruptionsBlocks(t *testing.T) {
	// The single-replica trap: minAvailable 1 on one replica allows zero
	// voluntary disruptions, permanently.
	labelled := map[string]string{"app": "lonely"}
	pods := []*corev1.Pod{mother.Pod("staging", "lonely-1", mother.PodLabels(labelled))}
	pdbs := []*policyv1.PodDisruptionBudget{mother.PDB("staging", "lonely", 0, labelled)}

	got := engine.CheckEvictable(pods, pdbs, engine.DefaultEvictConfig())

	if len(got) != 1 || got[0].Code != engine.BlockedPDBInsufficint {
		t.Fatalf("codes = %v, want [%s]", codes(got), engine.BlockedPDBInsufficint)
	}
	if got[0].PDB != "staging/lonely" {
		t.Errorf("the blocker should name the budget to change, got %q", got[0].PDB)
	}
}

func TestPodMatchedByTwoPDBsIsPermanentlyStuck(t *testing.T) {
	// The eviction API does not arbitrate between budgets — it returns HTTP
	// 500. Nothing can evict this pod, and both budgets report healthy, which
	// is what makes it so hard to spot by hand.
	pod := mother.Pod("default", "api", mother.PodLabels(map[string]string{
		"app": "api", "team": "platform",
	}))
	pdbs := []*policyv1.PodDisruptionBudget{
		mother.PDB("default", "team-wide", 5, map[string]string{"team": "platform"}),
		mother.PDB("default", "api-specific", 5, map[string]string{"app": "api"}),
	}

	got := engine.CheckEvictable([]*corev1.Pod{pod}, pdbs, engine.DefaultEvictConfig())

	if len(got) != 1 || got[0].Code != engine.BlockedMultiplePDBs {
		t.Fatalf("codes = %v, want [%s]", codes(got), engine.BlockedMultiplePDBs)
	}
	// Both budgets have ample slack; the block is structural, not arithmetic.
	if !strings.Contains(got[0].Message, "team-wide") || !strings.Contains(got[0].Message, "api-specific") {
		t.Errorf("message should name both budgets, got: %s", got[0].Message)
	}
}

func TestKubeSystemPodWithABudgetIsAllowed(t *testing.T) {
	// Per the autoscaler's own FAQ, configuring a PDB for a kube-system pod
	// "overrides the default strategy of not touching the node". Blocking
	// unconditionally would falsely reject candidates hosting PDB-protected
	// CoreDNS, which is a common and correct setup.
	labelled := map[string]string{"k8s-app": "kube-dns"}
	pod := mother.Pod("kube-system", "coredns-1", mother.PodLabels(labelled))
	pdbs := []*policyv1.PodDisruptionBudget{mother.PDB("kube-system", "coredns", 1, labelled)}

	if got := engine.CheckEvictable([]*corev1.Pod{pod}, pdbs, engine.DefaultEvictConfig()); len(got) != 0 {
		t.Errorf("a kube-system pod with an adequate budget is evictable, got %v", codes(got))
	}
}

func TestKubeSystemPodWithARestrictiveBudgetIsBlockedByIt(t *testing.T) {
	// The budget governs once it exists, so the refusal should come from the
	// allowance rather than the blanket kube-system rule — which is the
	// difference between "add a PDB" and "your PDB is too tight".
	labelled := map[string]string{"k8s-app": "kube-dns"}
	pod := mother.Pod("kube-system", "coredns-1", mother.PodLabels(labelled))
	pdbs := []*policyv1.PodDisruptionBudget{mother.PDB("kube-system", "coredns", 0, labelled)}

	got := engine.CheckEvictable([]*corev1.Pod{pod}, pdbs, engine.DefaultEvictConfig())

	if len(got) != 1 || got[0].Code != engine.BlockedPDBInsufficint {
		t.Fatalf("codes = %v, want [%s]", codes(got), engine.BlockedPDBInsufficint)
	}
}

func TestStalePDBStatusIsNotTrusted(t *testing.T) {
	// After a restrictive edit the controller may not have reconciled yet.
	// The eviction API refuses in that state whatever the recorded allowance
	// says, so trusting it would approve a drain that is then rejected
	// mid-flight.
	labelled := map[string]string{"app": "web"}
	pod := mother.Pod("default", "web-1", mother.PodLabels(labelled))
	stale := mother.Stale(mother.PDB("default", "web", 5, labelled))

	got := engine.CheckEvictable([]*corev1.Pod{pod}, []*policyv1.PodDisruptionBudget{stale},
		engine.DefaultEvictConfig())

	if len(got) != 1 || got[0].Code != engine.BlockedPDBStale {
		t.Fatalf("codes = %v, want [%s] despite an allowance of 5", codes(got), engine.BlockedPDBStale)
	}
}

func TestPDBsInOtherNamespacesDoNotApply(t *testing.T) {
	labelled := map[string]string{"app": "web"}
	pods := []*corev1.Pod{mother.Pod("production", "web-1", mother.PodLabels(labelled))}
	pdbs := []*policyv1.PodDisruptionBudget{mother.PDB("staging", "web", 0, labelled)}

	if got := engine.CheckEvictable(pods, pdbs, engine.DefaultEvictConfig()); len(got) != 0 {
		t.Errorf("a budget in another namespace must not apply, got %v", codes(got))
	}
}

func TestNullSelectorMatchesNothing(t *testing.T) {
	// policy/v1: a null selector matches no pods, while an empty one matches
	// every pod in the namespace. Treating them alike would invent a budget
	// that does not apply.
	pod := mother.Pod("default", "web", mother.PodLabels(map[string]string{"app": "web"}))

	null := mother.PDB("default", "null", 0, nil)
	null.Spec.Selector = nil

	if got := engine.CheckEvictable([]*corev1.Pod{pod}, []*policyv1.PodDisruptionBudget{null},
		engine.DefaultEvictConfig()); len(got) != 0 {
		t.Errorf("a null selector selects no pods, got %v", codes(got))
	}
}

func TestEmptySelectorMatchesEveryPodInNamespace(t *testing.T) {
	pod := mother.Pod("default", "web")

	empty := mother.PDB("default", "catch-all", 0, map[string]string{})

	got := engine.CheckEvictable([]*corev1.Pod{pod}, []*policyv1.PodDisruptionBudget{empty},
		engine.DefaultEvictConfig())

	if len(got) != 1 || got[0].Code != engine.BlockedPDBInsufficint {
		t.Fatalf("an empty selector covers the whole namespace, got %v", codes(got))
	}
}
