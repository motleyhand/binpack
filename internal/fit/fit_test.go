package fit_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/motleyhand/binpack/internal/fit"
	"github.com/motleyhand/binpack/internal/mother"
)

// free is shorthand for "this node has room for anything the test cares about".
func free(node *corev1.Node) corev1.ResourceList { return fit.Allocatable(node) }

func TestCanFit(t *testing.T) {
	tests := []struct {
		name      string
		pod       *corev1.Pod
		node      *corev1.Node
		residents []*corev1.Pod
		wantFit   bool
		wantCode  string
	}{
		{
			name:    "ordinary pod on an ordinary node",
			pod:     mother.Pod("default", "web"),
			node:    mother.SmallNode("node-a"),
			wantFit: true,
		},
		{
			name:     "cordoned node is not a destination",
			pod:      mother.Pod("default", "web"),
			node:     mother.SmallNode("node-a", mother.Cordoned()),
			wantCode: fit.ReasonUnschedulable,
		},
		{
			name:     "not-ready node is not a destination",
			pod:      mother.Pod("default", "web"),
			node:     mother.SmallNode("node-a", mother.NotReady()),
			wantCode: fit.ReasonNodeNotReady,
		},
		{
			name:     "untolerated NoSchedule taint",
			pod:      mother.Pod("default", "web"),
			node:     mother.SmallNode("node-a", mother.Tainted("dedicated", "db", corev1.TaintEffectNoSchedule)),
			wantCode: fit.ReasonUntoleratedTaint,
		},
		{
			name:    "tolerated taint is fine",
			pod:     mother.Pod("default", "web", mother.Tolerating("dedicated", corev1.TaintEffectNoSchedule)),
			node:    mother.SmallNode("node-a", mother.Tainted("dedicated", "db", corev1.TaintEffectNoSchedule)),
			wantFit: true,
		},
		{
			// PreferNoSchedule affects scoring only, so it must not refuse.
			// Modelling it would make binpack stricter than the scheduler,
			// which costs consolidations for no safety gain.
			name:    "PreferNoSchedule taint does not block",
			pod:     mother.Pod("default", "web"),
			node:    mother.SmallNode("node-a", mother.Tainted("spot", "true", corev1.TaintEffectPreferNoSchedule)),
			wantFit: true,
		},
		{
			name:     "node selector not satisfied",
			pod:      mother.Pod("default", "web", mother.WithNodeSelector("disk", "ssd")),
			node:     mother.SmallNode("node-a"),
			wantCode: fit.ReasonNodeAffinity,
		},
		{
			name:    "node selector satisfied",
			pod:     mother.Pod("default", "web", mother.WithNodeSelector("disk", "ssd")),
			node:    mother.SmallNode("node-a", mother.NodeLabels(map[string]string{"disk": "ssd"})),
			wantFit: true,
		},
		{
			name:     "not enough memory",
			pod:      mother.Pod("default", "hungry", mother.Requests("100m", "8Gi")),
			node:     mother.SmallNode("node-a"),
			wantCode: fit.ReasonInsufficient,
		},
		{
			// The case that motivates modelling every resource rather than
			// three: plenty of CPU and memory, no GPU at all.
			name:     "extended resource the node does not advertise",
			pod:      mother.Pod("default", "trainer", mother.Requesting("nvidia.com/gpu", "1")),
			node:     mother.LargeNode("node-a"),
			wantCode: fit.ReasonInsufficient,
		},
		{
			name:    "extended resource the node does advertise",
			pod:     mother.Pod("default", "trainer", mother.Requesting("nvidia.com/gpu", "1")),
			node:    mother.GPUNode("node-a", 2),
			wantFit: true,
		},
		{
			name: "resident with required anti-affinity disqualifies the node",
			pod:  mother.Pod("default", "web"),
			node: mother.SmallNode("node-a"),
			residents: []*corev1.Pod{
				mother.Pod("default", "sharded", mother.WithRequiredAntiAffinity("app", "sharded")),
			},
			wantCode: fit.ReasonUnsupportedNode,
		},
		{
			// Only anti-affinity is symmetric. A resident's *affinity* is not
			// re-evaluated when another pod arrives, so refusing on it would
			// cost consolidations for nothing.
			name: "resident with required affinity does not disqualify the node",
			pod:  mother.Pod("default", "web"),
			node: mother.SmallNode("node-a"),
			residents: []*corev1.Pod{
				func() *corev1.Pod {
					p := mother.Pod("default", "paired")
					p.Spec.Affinity = &corev1.Affinity{
						PodAffinity: &corev1.PodAffinity{
							RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
								TopologyKey: corev1.LabelHostname,
							}},
						},
					}
					return p
				}(),
			},
			wantFit: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := fit.CanFit(tc.pod, tc.node, free(tc.node), tc.residents)

			if ok != tc.wantFit {
				t.Fatalf("CanFit = %t (%s), want %t", ok, reason.Message, tc.wantFit)
			}
			if !tc.wantFit && reason.Code != tc.wantCode {
				t.Errorf("reason code = %q, want %q (message: %s)", reason.Code, tc.wantCode, reason.Message)
			}
			if !tc.wantFit && reason.Message == "" {
				t.Error("a refusal must carry a message naming what caused it")
			}
		})
	}
}

func TestBoundPodCanBePlacedElsewhere(t *testing.T) {
	// Every pod binpack sees is already bound, so spec.NodeName always names
	// the node it is being relocated FROM. Comparing it against the candidate
	// destination would refuse every relocation there is.
	//
	// This test exists to stop that "fix" being applied: the gap it appears to
	// leave — a controller template that pins NodeName — is documented in
	// CanFit and bounded by the stall-and-backoff machinery, because such a
	// pod returns to its own node rather than going Pending.
	pod := mother.Pod("default", "web", mother.OnNode("node-a"))
	destination := mother.SmallNode("node-b")

	ok, reason := fit.CanFit(pod, destination, free(destination), nil)
	if !ok {
		t.Fatalf("a bound pod must be placeable on another node, got refusal: %s", reason.Message)
	}
}

func TestPodCountIsAResource(t *testing.T) {
	// `pods` is in a node's allocatable but never in a pod's requests, so a
	// naive subtract-request-from-remaining loop never consumes a slot and
	// would pack unlimited pods onto a node capped at 110.
	node := mother.SmallNode("node-a")
	remaining := fit.Allocatable(node)
	remaining[corev1.ResourcePods] = *resource.NewQuantity(0, resource.DecimalSI)

	pod := mother.Pod("default", "web")
	ok, reason := fit.CanFit(pod, node, remaining, nil)

	if ok {
		t.Fatal("a node with no pod slots left must refuse, however much CPU and memory it has")
	}
	if reason.Code != fit.ReasonInsufficient {
		t.Errorf("reason = %q, want %q", reason.Code, fit.ReasonInsufficient)
	}
}

func TestEffectiveRequests(t *testing.T) {
	mem := func(rl corev1.ResourceList) string {
		q := rl[corev1.ResourceMemory]
		return q.String()
	}

	t.Run("init container peak beats the container sum", func(t *testing.T) {
		// A light main container behind a heavy init container needs the init
		// container's memory at admission time.
		pod := mother.Pod("default", "web",
			mother.Requests("100m", "128Mi"),
			mother.WithInitContainer("migrate", "100m", "2Gi"),
		)
		if got := mem(fit.EffectiveRequests(pod)); got != "2Gi" {
			t.Errorf("memory = %s, want 2Gi (the init container peak)", got)
		}
	})

	t.Run("native sidecars add rather than max", func(t *testing.T) {
		// A sidecar keeps running alongside the main container, so its request
		// is additive — unlike an ordinary init container.
		pod := mother.Pod("default", "web",
			mother.Requests("100m", "1Gi"),
			mother.WithSidecar("proxy", "100m", "512Mi"),
		)
		if got := mem(fit.EffectiveRequests(pod)); got != "1536Mi" {
			t.Errorf("memory = %s, want 1536Mi (main plus sidecar)", got)
		}
	})

	t.Run("RuntimeClass overhead is added on top", func(t *testing.T) {
		pod := mother.Pod("default", "web",
			mother.Requests("100m", "1Gi"),
			mother.WithOverhead("50m", "256Mi"),
		)
		if got := mem(fit.EffectiveRequests(pod)); got != "1280Mi" {
			t.Errorf("memory = %s, want 1280Mi (request plus overhead)", got)
		}
	})

	t.Run("always carries one pod slot", func(t *testing.T) {
		got := fit.EffectiveRequests(mother.Pod("default", "web"))[corev1.ResourcePods]
		if got.Value() != 1 {
			t.Errorf("pods = %d, want 1", got.Value())
		}
	})
}

func TestSubtractMatchesCanFit(t *testing.T) {
	// The engine subtracts as it places; CanFit compares what is left. If the
	// two disagreed, a simulation could place more pods than actually fit.
	node := mother.SmallNode("node-a")
	remaining := fit.Allocatable(node)

	pod := mother.Pod("default", "web", mother.Requests("500m", "400Mi"))

	placed := 0
	for {
		ok, _ := fit.CanFit(pod, node, remaining, nil)
		if !ok {
			break
		}
		fit.Subtract(remaining, fit.EffectiveRequests(pod))
		placed++
		if placed > 20 {
			t.Fatal("placed more pods than a 1360Mi node could hold: Subtract and CanFit disagree")
		}
	}

	// 1360Mi / 400Mi = 3
	if placed != 3 {
		t.Errorf("placed %d pods, want 3", placed)
	}
}

func TestAllocatableIsACopy(t *testing.T) {
	// Nodes come from a shared informer cache. Writing to one corrupts it for
	// every other consumer in the process.
	node := mother.SmallNode("node-a")
	before := node.Status.Allocatable[corev1.ResourceMemory]

	remaining := fit.Allocatable(node)
	fit.Subtract(remaining, corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")})

	after := node.Status.Allocatable[corev1.ResourceMemory]
	if before.Cmp(after) != 0 {
		t.Errorf("Allocatable must not alias the node: %s became %s", before.String(), after.String())
	}
}
