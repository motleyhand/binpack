package engine_test

import (
	"strings"
	"testing"
	"time"

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
			// Young enough that the autoscaler is still waiting for it. The
			// age is the whole condition, so a fixture without one would pass
			// this case for a reason no cluster ever supplies.
			name: "kube-system pod with no budget blocks node removal",
			pod:  mother.Pod("kube-system", "coredns", mother.CreatedAt(now.Add(-5*time.Minute))),
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
			pod: mother.Pod("kube-system", "coredns", mother.SafeToEvict("true"),
				mother.CreatedAt(now.Add(-5*time.Minute))),
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
			got := engine.CheckEvictable([]*corev1.Pod{tc.pod}, nil, cfg, now)

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

	if got := engine.CheckEvictable([]*corev1.Pod{local, system}, nil, permissive, now); len(got) != 0 {
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

	got := engine.CheckEvictable(pods, pdbs, engine.DefaultEvictConfig(), now)

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

	if got := engine.CheckEvictable(pods, pdbs, engine.DefaultEvictConfig(), now); len(got) != 0 {
		t.Errorf("two allowed disruptions cover two evictions, got %v", codes(got))
	}
}

func TestZeroDisruptionsBlocks(t *testing.T) {
	// The single-replica trap: minAvailable 1 on one replica allows zero
	// voluntary disruptions, permanently.
	labelled := map[string]string{"app": "lonely"}
	pods := []*corev1.Pod{mother.Pod("staging", "lonely-1", mother.PodLabels(labelled))}
	pdbs := []*policyv1.PodDisruptionBudget{mother.PDB("staging", "lonely", 0, labelled)}

	got := engine.CheckEvictable(pods, pdbs, engine.DefaultEvictConfig(), now)

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

	got := engine.CheckEvictable([]*corev1.Pod{pod}, pdbs, engine.DefaultEvictConfig(), now)

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

	if got := engine.CheckEvictable([]*corev1.Pod{pod}, pdbs, engine.DefaultEvictConfig(), now); len(got) != 0 {
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

	got := engine.CheckEvictable([]*corev1.Pod{pod}, pdbs, engine.DefaultEvictConfig(), now)

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
		engine.DefaultEvictConfig(), now)

	if len(got) != 1 || got[0].Code != engine.BlockedPDBStale {
		t.Fatalf("codes = %v, want [%s] despite an allowance of 5", codes(got), engine.BlockedPDBStale)
	}
}

func TestUnreadyPodsMayNotConsumeTheBudget(t *testing.T) {
	// The eviction API skips the budget entirely for a pod that is not Ready,
	// in two cases. Counting such a pod would refuse a drain Kubernetes would
	// have permitted — and a CrashLoopBackOff replica under a tight budget is
	// exactly the sort of thing that pins a node in the first place.
	labelled := map[string]string{"app": "web"}
	broken := mother.Pod("default", "web-1", mother.PodLabels(labelled), mother.Unready())
	healthy := mother.Pod("default", "web-2", mother.PodLabels(labelled))

	t.Run("AlwaysAllow exempts it regardless of the budget", func(t *testing.T) {
		pdb := mother.AlwaysAllowUnhealthy(mother.PDB("default", "web", 0, labelled))

		got := engine.CheckEvictable([]*corev1.Pod{broken},
			[]*policyv1.PodDisruptionBudget{pdb}, engine.DefaultEvictConfig(), now)

		if len(got) != 0 {
			t.Errorf("AlwaysAllow permits evicting an unready pod with zero allowance, got %v", codes(got))
		}
	})

	t.Run("default policy exempts it while the application is healthy", func(t *testing.T) {
		// Evicting an already-broken replica disrupts nothing, so it is not
		// charged for.
		pdb := mother.Healthy(mother.PDB("default", "web", 0, labelled), 3, 2)

		got := engine.CheckEvictable([]*corev1.Pod{broken},
			[]*policyv1.PodDisruptionBudget{pdb}, engine.DefaultEvictConfig(), now)

		if len(got) != 0 {
			t.Errorf("a healthy budget permits evicting an unready pod, got %v", codes(got))
		}
	})

	t.Run("default policy charges for it when the application is not healthy", func(t *testing.T) {
		pdb := mother.Healthy(mother.PDB("default", "web", 0, labelled), 1, 2)

		got := engine.CheckEvictable([]*corev1.Pod{broken},
			[]*policyv1.PodDisruptionBudget{pdb}, engine.DefaultEvictConfig(), now)

		if len(got) != 1 || got[0].Code != engine.BlockedPDBInsufficint {
			t.Errorf("an unhealthy application still guards its budget, got %v", codes(got))
		}
	})

	t.Run("a budget that desires nothing charges for it", func(t *testing.T) {
		// The other end of the exemption, and the reason it carries a second
		// clause. A budget desiring nothing reports currentHealthy 0 against
		// desiredHealthy 0, so "the application is meeting its budget" is
		// true as arithmetic and false as a statement about the workload —
		// there is no health to meet. The eviction API charges the pod, and
		// so must binpack: without the desiredHealthy > 0 guard an unready
		// pod would be evicted for free against an allowance of zero, and the
		// drain would be approved for evictions the API server then refuses.
		pdb := mother.DesiresNothing(mother.PDB("default", "web", 0, labelled))

		got := engine.CheckEvictable([]*corev1.Pod{broken},
			[]*policyv1.PodDisruptionBudget{pdb}, engine.DefaultEvictConfig(), now)

		if len(got) != 1 || got[0].Code != engine.BlockedPDBInsufficint {
			t.Errorf("a budget desiring nothing still guards its allowance, got %v", codes(got))
		}
	})

	t.Run("a ready pod always draws on the allowance", func(t *testing.T) {
		pdb := mother.AlwaysAllowUnhealthy(mother.PDB("default", "web", 0, labelled))

		got := engine.CheckEvictable([]*corev1.Pod{healthy},
			[]*policyv1.PodDisruptionBudget{pdb}, engine.DefaultEvictConfig(), now)

		if len(got) != 1 || got[0].Code != engine.BlockedPDBInsufficint {
			t.Errorf("AlwaysAllow applies only to unready pods, got %v", codes(got))
		}
	})
}

func TestBudgetBlockersNameAPod(t *testing.T) {
	// A blocker that names only a budget cannot be rendered next to the
	// workload it affects, and explain has to handle every blocker uniformly.
	labelled := map[string]string{"app": "web"}
	pods := []*corev1.Pod{
		mother.Pod("default", "web-1", mother.PodLabels(labelled)),
		mother.Pod("default", "web-2", mother.PodLabels(labelled)),
	}
	pdbs := []*policyv1.PodDisruptionBudget{mother.PDB("default", "web", 1, labelled)}

	got := engine.CheckEvictable(pods, pdbs, engine.DefaultEvictConfig(), now)

	if len(got) != 1 {
		t.Fatalf("codes = %v", codes(got))
	}
	if got[0].Pod == nil {
		t.Fatal("a shortfall must name an affected pod, as EvictionBlocker.Pod documents")
	}
	if got[0].Pod.Namespace != "default" {
		t.Errorf("named pod = %s/%s", got[0].Pod.Namespace, got[0].Pod.Name)
	}
}

func TestPDBsInOtherNamespacesDoNotApply(t *testing.T) {
	labelled := map[string]string{"app": "web"}
	pods := []*corev1.Pod{mother.Pod("production", "web-1", mother.PodLabels(labelled))}
	pdbs := []*policyv1.PodDisruptionBudget{mother.PDB("staging", "web", 0, labelled)}

	if got := engine.CheckEvictable(pods, pdbs, engine.DefaultEvictConfig(), now); len(got) != 0 {
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
		engine.DefaultEvictConfig(), now); len(got) != 0 {
		t.Errorf("a null selector selects no pods, got %v", codes(got))
	}
}

func TestEmptySelectorMatchesEveryPodInNamespace(t *testing.T) {
	pod := mother.Pod("default", "web")

	empty := mother.PDB("default", "catch-all", 0, map[string]string{})

	got := engine.CheckEvictable([]*corev1.Pod{pod}, []*policyv1.PodDisruptionBudget{empty},
		engine.DefaultEvictConfig(), now)

	if len(got) != 1 || got[0].Code != engine.BlockedPDBInsufficint {
		t.Fatalf("an empty selector covers the whole namespace, got %v", codes(got))
	}
}

// TestCheckEvictableIgnoresMemoryBackedEmptyDir closes the gap that makes a
// service-mesh cluster undrainable end to end.
//
// An emptyDir with medium Memory is tmpfs. Nothing of it reaches the node's
// disk, so the cluster-autoscaler's isLocalVolume excludes it by name
// (cluster-autoscaler utils/drain/drain.go) and removes the node without
// hesitation. Istio injects one into every meshed pod as istio-envoy, Linkerd
// as linkerd-identity-end-entity — so on such a cluster every node hosts at
// least one, and a blocker here refuses every candidate for ever.
func TestCheckEvictableIgnoresMemoryBackedEmptyDir(t *testing.T) {
	pod := mother.Pod("default", "meshed", mother.WithMemoryBackedEmptyDir("istio-envoy"))

	if got := engine.CheckEvictable([]*corev1.Pod{pod}, nil, engine.DefaultEvictConfig(), now); len(got) != 0 {
		t.Errorf("a tmpfs volume blocked the drain with %v: %s", codes(got), got[0].Message)
	}
}

// TestCheckEvictableHonoursSafeToEvictLocalVolumes covers the autoscaler's
// per-volume escape hatch, which its own FAQ recommends ahead of the blanket
// safe-to-evict=true.
//
// The unlisted volume is the half that matters. Every change in this area
// makes binpack accept a drain it used to refuse, so a test that only asserts
// the listed volumes pass would go green against an implementation that
// stopped looking at local storage altogether.
func TestCheckEvictableHonoursSafeToEvictLocalVolumes(t *testing.T) {
	listed := mother.Pod("default", "app",
		mother.WithEmptyDir("cache"),
		mother.WithEmptyDir("data"),
		mother.SafeToEvictLocalVolumes("cache", "data"))

	if got := engine.CheckEvictable([]*corev1.Pod{listed}, nil, engine.DefaultEvictConfig(), now); len(got) != 0 {
		t.Errorf("volumes the operator named as disposable still blocked: %v: %s",
			codes(got), got[0].Message)
	}

	unlisted := mother.Pod("default", "app",
		mother.WithEmptyDir("cache"),
		mother.WithEmptyDir("state"),
		mother.SafeToEvictLocalVolumes("cache"))

	got := engine.CheckEvictable([]*corev1.Pod{unlisted}, nil, engine.DefaultEvictConfig(), now)
	if len(got) != 1 || got[0].Code != engine.BlockedLocalStorage {
		t.Fatalf("codes = %v, want [%s]: state is not named, so it still blocks",
			codes(got), engine.BlockedLocalStorage)
	}
	if !strings.Contains(got[0].Message, "state") {
		t.Errorf("message = %q, want it to name the volume that is still blocking", got[0].Message)
	}

	// The annotation is split on commas and nothing else, because that is what
	// the autoscaler does with it. Trimming would be the friendlier reading and
	// the wrong one: the autoscaler would still block on this pod, so binpack
	// approving the drain buys an emptied node nothing then removes.
	spaced := mother.Pod("default", "app",
		mother.WithEmptyDir("cache"),
		mother.WithEmptyDir("data"),
		mother.Annotated("cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes", "cache, data"))

	got = engine.CheckEvictable([]*corev1.Pod{spaced}, nil, engine.DefaultEvictConfig(), now)
	if len(got) != 1 || got[0].Code != engine.BlockedLocalStorage {
		t.Fatalf("codes = %v, want [%s]: \"cache, data\" names a volume called \" data\", "+
			"which is no volume at all — and the autoscaler reads it the same way",
			codes(got), engine.BlockedLocalStorage)
	}
	if !strings.Contains(got[0].Message, "data") {
		t.Errorf("message = %q, want it to name data, the volume the spacing failed to exempt",
			got[0].Message)
	}
}

// TestCheckEvictableDoesNotBlockOnAKubeSystemPodPastTheAutoscalersGrace
// follows the System drainability rule as it has stood since
// cluster-autoscaler 1.33: a kube-system pod with no budget blocks removal
// only until --blocking-system-pod-distruption-timeout has passed since the
// pod was created, after which the autoscaler evicts it and takes the node
// (cluster-autoscaler simulator/drainability/rules/system/rule.go).
//
// The five-minute case is the half that matters, and it is deliberately not
// the mirror of the two-hour one: this change makes binpack accept drains it
// used to refuse, so an implementation that simply deleted the branch would
// satisfy the first assertion alone.
func TestCheckEvictableDoesNotBlockOnAKubeSystemPodPastTheAutoscalersGrace(t *testing.T) {
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
		want string // empty means evictable
	}{
		{
			// The steady state of essentially every cluster: coredns,
			// metrics-server, a CSI controller, none of them an hour young.
			name: "two hours old, so the autoscaler's grace has passed",
			pod:  mother.Pod("kube-system", "coredns", mother.CreatedAt(now.Add(-2*time.Hour))),
		},
		{
			name: "five minutes old, so it still blocks",
			pod:  mother.Pod("kube-system", "coredns", mother.CreatedAt(now.Add(-5*time.Minute))),
			want: engine.BlockedSystemPod,
		},
		{
			// Upstream's isBspPassedDisruptionTimeout guards on IsZero
			// before comparing, so a pod with no creation timestamp is one
			// whose age the autoscaler declines to reason about. Mirrored,
			// because the alternative reads an unset field as the epoch and
			// grants every such pod the grace.
			name: "no creation timestamp at all, so no age to have passed",
			pod:  mother.Pod("kube-system", "coredns"),
			want: engine.BlockedSystemPod,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := engine.CheckEvictable([]*corev1.Pod{tc.pod}, nil, engine.DefaultEvictConfig(), now)

			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("expected evictable, blocked by %v: %s", codes(got), got[0].Message)
				}
				return
			}
			if len(got) != 1 || got[0].Code != tc.want {
				t.Fatalf("codes = %v, want [%s]", codes(got), tc.want)
			}
		})
	}

	// A grace of zero is how an operator says their autoscaler has none —
	// every release before 1.33, which the stated floor of 1.30 includes.
	// Nothing about the pod changes; only what binpack believes about the
	// component that will remove the node.
	old := engine.DefaultEvictConfig()
	old.BlockingSystemPodDistruptionTimeout = 0
	ancient := mother.Pod("kube-system", "coredns", mother.CreatedAt(now.Add(-30*24*time.Hour)))

	got := engine.CheckEvictable([]*corev1.Pod{ancient}, nil, old, now)
	if len(got) != 1 || got[0].Code != engine.BlockedSystemPod {
		t.Fatalf("codes = %v, want [%s]: with no grace configured the blocker never expires",
			codes(got), engine.BlockedSystemPod)
	}
}

// TestPendingPodsBypassBudgetsAsTheEvictionAPIDoes pins the phase, not the
// readiness, as what decides whether a budget is consulted at all.
//
// The eviction subresource asks canIgnorePDB before it looks a budget up
// (kubernetes pkg/registry/core/pod/storage/eviction.go, Create): for a pod
// that is Pending, Succeeded, Failed or already terminating it deletes the pod
// outright, and getPodDisruptionBudgets, the more-than-one refusal and
// checkAndDecrement are all downstream of that gate. A pod wedged in
// ContainerCreating is the case that matters — it is exactly the pod an
// operator wants consolidated away, and binpack used to refuse its node.
func TestPendingPodsBypassBudgetsAsTheEvictionAPIDoes(t *testing.T) {
	labelled := map[string]string{"app": "web", "team": "platform"}

	// Short of its desired health, so evictableWhileUnready does not excuse
	// the pod first and the assertion is about the phase rather than about
	// the readiness that comes with it.
	tight := func() *policyv1.PodDisruptionBudget {
		return mother.Healthy(mother.PDB("default", "web", 0, map[string]string{"app": "web"}), 1, 2)
	}
	both := func() []*policyv1.PodDisruptionBudget {
		return []*policyv1.PodDisruptionBudget{
			mother.PDB("default", "team-wide", 5, map[string]string{"team": "platform"}),
			mother.PDB("default", "api-specific", 5, map[string]string{"app": "web"}),
		}
	}

	ignored := []struct {
		name string
		pod  *corev1.Pod
	}{
		{"starting", mother.Pod("default", "web-0", mother.OnNode("a"),
			mother.PodLabels(labelled), mother.Starting())},
		{"succeeded", mother.Pod("default", "web-0", mother.OnNode("a"),
			mother.PodLabels(labelled), mother.Succeeded())},
		{"failed", mother.Pod("default", "web-0", mother.OnNode("a"),
			mother.PodLabels(labelled), mother.Failed())},
		{"terminating", mother.Pod("default", "web-0", mother.OnNode("a"),
			mother.PodLabels(labelled), mother.Terminating(now, 30*time.Second))},
	}

	for _, tc := range ignored {
		t.Run(tc.name+" is charged no budget", func(t *testing.T) {
			got := engine.CheckEvictable([]*corev1.Pod{tc.pod},
				[]*policyv1.PodDisruptionBudget{tight()}, engine.DefaultEvictConfig(), now)

			if len(got) != 0 {
				t.Errorf("the eviction API deletes such a pod without consulting a budget, got %v", codes(got))
			}
		})

		t.Run(tc.name+" does not trigger the permanent two-budget refusal", func(t *testing.T) {
			// The worse half: multiple-pdbs is reported as unfixable by
			// anything, so diagnose sends an operator to rewrite selectors
			// that the API server would never have looked at.
			got := engine.CheckEvictable([]*corev1.Pod{tc.pod}, both(), engine.DefaultEvictConfig(), now)

			if len(got) != 0 {
				t.Errorf("the two-budget refusal is downstream of canIgnorePDB, got %v", codes(got))
			}
		})
	}

	// The control, and the direction that must not move: a Running pod is
	// still charged, and still permanently refused under two budgets.
	running := mother.Pod("default", "web-0", mother.OnNode("a"), mother.PodLabels(labelled))

	t.Run("a running pod is still charged", func(t *testing.T) {
		got := engine.CheckEvictable([]*corev1.Pod{running},
			[]*policyv1.PodDisruptionBudget{tight()}, engine.DefaultEvictConfig(), now)

		if len(got) != 1 || got[0].Code != engine.BlockedPDBInsufficint {
			t.Fatalf("codes = %v, want [%s]", codes(got), engine.BlockedPDBInsufficint)
		}
	})

	t.Run("a running pod under two budgets is still permanently stuck", func(t *testing.T) {
		got := engine.CheckEvictable([]*corev1.Pod{running}, both(), engine.DefaultEvictConfig(), now)

		if len(got) != 1 || got[0].Code != engine.BlockedMultiplePDBs {
			t.Fatalf("codes = %v, want [%s]", codes(got), engine.BlockedMultiplePDBs)
		}
	})

	// The second control, and the reason the gate sits below checkPod rather
	// than above it. What canIgnorePDB governs is the eviction API's budget
	// arithmetic and nothing else; the refusals in checkPod are the
	// cluster-autoscaler's, and the autoscaler does not read a pod's phase
	// before declining to remove its node. Hoisting the gate would have
	// binpack approve a drain the autoscaler then refuses to finish, which is
	// the one direction that costs more than a consolidation.
	autoscalerRules := []struct {
		name string
		pod  *corev1.Pod
		code string
	}{
		{"bare", mother.Pod("default", "web-0", mother.OnNode("a"), mother.PodLabels(labelled),
			mother.Starting(), mother.Bare()), engine.BlockedBarePod},
		{"local storage", mother.Pod("default", "web-0", mother.OnNode("a"), mother.PodLabels(labelled),
			mother.Starting(), mother.WithHostPathVolume("state", "/var/lib/state")), engine.BlockedLocalStorage},
		{"refused outright", mother.Pod("default", "web-0", mother.OnNode("a"), mother.PodLabels(labelled),
			mother.Starting(), mother.SafeToEvict("false")), engine.BlockedSafeToEvict},
	}

	for _, tc := range autoscalerRules {
		t.Run("a starting pod is still refused for being "+tc.name, func(t *testing.T) {
			got := engine.CheckEvictable([]*corev1.Pod{tc.pod},
				[]*policyv1.PodDisruptionBudget{tight()}, engine.DefaultEvictConfig(), now)

			if len(got) != 1 || got[0].Code != tc.code {
				t.Fatalf("codes = %v, want [%s]", codes(got), tc.code)
			}
		})
	}
}

// TestUnparseableSelectorIsSkippedAsTheEvictionAPISkipsIt reverses the
// direction binpack's comment reached for.
//
// getPodDisruptionBudgets skips a budget whose selector will not parse — "This
// object has an invalid selector, it does not match the pod" — so treating it
// as matching makes binpack's matched set a superset of the API server's.
// Being wrong that way is not conservative: matching more budgets is precisely
// what produces the more-than-one refusal, which binpack reports as permanent.
func TestUnparseableSelectorIsSkippedAsTheEvictionAPISkipsIt(t *testing.T) {
	labelled := map[string]string{"app": "web"}
	pod := mother.Pod("default", "web-0", mother.PodLabels(labelled))

	broken := mother.UnparseableSelector(mother.PDB("default", "grandfathered", 0, nil))
	valid := mother.PDB("default", "web", 5, labelled)

	t.Run("it does not make a second budget into a pair", func(t *testing.T) {
		got := engine.CheckEvictable([]*corev1.Pod{pod},
			[]*policyv1.PodDisruptionBudget{broken, valid}, engine.DefaultEvictConfig(), now)

		if len(got) != 0 {
			t.Errorf("a budget the API server skips cannot make a pod doubly covered, got %v", codes(got))
		}
	})

	t.Run("it is charged no demand on its own", func(t *testing.T) {
		got := engine.CheckEvictable([]*corev1.Pod{pod},
			[]*policyv1.PodDisruptionBudget{broken}, engine.DefaultEvictConfig(), now)

		if len(got) != 0 {
			t.Errorf("an allowance the API server never consults cannot block, got %v", codes(got))
		}
	})

	// The control: a valid selector that matches must still match, or the fix
	// has stopped binpack seeing budgets rather than stopped it inventing one.
	t.Run("two valid selectors are still a pair", func(t *testing.T) {
		second := mother.PDB("default", "team-wide", 5, labelled)

		got := engine.CheckEvictable([]*corev1.Pod{pod},
			[]*policyv1.PodDisruptionBudget{valid, second}, engine.DefaultEvictConfig(), now)

		if len(got) != 1 || got[0].Code != engine.BlockedMultiplePDBs {
			t.Fatalf("codes = %v, want [%s]", codes(got), engine.BlockedMultiplePDBs)
		}
	})
}

// TestAnUnrecognisedEvictionPolicyChargesTheBudget applies CLAUDE.md's
// allowlist rule to unhealthyPodEvictionPolicy, which is where policy/v1 asks
// for it too: "Clients making eviction decisions should disallow eviction of
// unhealthy pods if they encounter an unrecognized policy in this field."
//
// Every policy is asserted against a budget in both states, because that is
// what separates the arms. AlwaysAllow and IfHealthyBudget agree while the
// application is healthy; IfHealthyBudget and an unrecognised value agree
// while it is not. A table covering one state per policy would pass whichever
// arm was deleted.
//
// The unrecognised value is invented rather than borrowed. Asserting against a
// policy that exists would let the test agree with the code by construction —
// and the hazard here is by definition a value nobody has written yet.
func TestAnUnrecognisedEvictionPolicyChargesTheBudget(t *testing.T) {
	labelled := map[string]string{"app": "web"}
	broken := mother.Pod("default", "web-1", mother.PodLabels(labelled), mother.Unready())

	// Two replicas, both up, minAvailable 2: no slack, and nothing wrong.
	healthy := func() *policyv1.PodDisruptionBudget {
		return mother.PDB("default", "web", 0, labelled)
	}
	// The same budget a replica short, so the application it guards is
	// currently disrupted.
	short := func() *policyv1.PodDisruptionBudget {
		return mother.Healthy(mother.PDB("default", "web", 0, labelled), 1, 2)
	}
	withPolicy := func(pdb *policyv1.PodDisruptionBudget,
		p policyv1.UnhealthyPodEvictionPolicyType,
	) *policyv1.PodDisruptionBudget {
		pdb.Spec.UnhealthyPodEvictionPolicy = &p
		return pdb
	}
	const future = policyv1.UnhealthyPodEvictionPolicyType("EvictionRequestOnly")

	tests := []struct {
		name   string
		pdb    *policyv1.PodDisruptionBudget
		charge bool
	}{
		{"AlwaysAllow frees it while the application is healthy",
			withPolicy(healthy(), policyv1.AlwaysAllow), false},
		{"AlwaysAllow frees it while the application is not",
			withPolicy(short(), policyv1.AlwaysAllow), false},

		{"IfHealthyBudget frees it while the application is healthy",
			withPolicy(healthy(), policyv1.IfHealthyBudget), false},
		{"IfHealthyBudget charges it while the application is not",
			withPolicy(short(), policyv1.IfHealthyBudget), true},

		{"an absent policy is IfHealthyBudget while healthy", healthy(), false},
		{"an absent policy is IfHealthyBudget while not", short(), true},

		{"an unrecognised policy charges it even while healthy",
			withPolicy(healthy(), future), true},
		{"an unrecognised policy charges it while not",
			withPolicy(short(), future), true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := engine.CheckEvictable([]*corev1.Pod{broken},
				[]*policyv1.PodDisruptionBudget{tc.pdb}, engine.DefaultEvictConfig(), now)

			charged := len(got) == 1 && got[0].Code == engine.BlockedPDBInsufficint
			if charged != tc.charge {
				t.Errorf("charged = %v, want %v (codes %v)", charged, tc.charge, codes(got))
			}
		})
	}
}
