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
			// The scheduler matches Gt and Lt only when
			// TaintTolerationComparisonOperators is on, and it is alpha and
			// off by default. Honouring one the scheduler would not is the
			// unsound direction: binpack would approve a destination the
			// scheduler refuses.
			name:     "a greater-than toleration does not tolerate the taint",
			pod:      mother.Pod("default", "web", mother.ToleratingGt("shard", "3")),
			node:     mother.SmallNode("node-a", mother.Tainted("shard", "5", corev1.TaintEffectNoSchedule)),
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
			// The resident's term selects app=sharded, and so does the
			// incoming pod's label — so it could genuinely be rejected.
			name: "resident anti-affinity that could match disqualifies the node",
			pod:  mother.Pod("default", "web", mother.PodLabels(map[string]string{"app": "sharded"})),
			node: mother.SmallNode("node-a"),
			residents: []*corev1.Pod{
				mother.Pod("default", "sharded", mother.WithRequiredAntiAffinity("app", "sharded")),
			},
			wantCode: fit.ReasonUnsupportedNode,
		},
		{
			// The CNI case. Cilium's agent has anti-affinity to itself so it
			// lands once per node, and it runs on every node — so treating
			// its presence as disqualifying would rule out every destination
			// on every cluster running it, which on DOKS is all of them.
			name: "resident anti-affinity that cannot match does not disqualify",
			pod:  mother.Pod("default", "web"),
			node: mother.SmallNode("node-a"),
			residents: []*corev1.Pod{
				mother.DaemonSetPod("kube-system", "cilium",
					mother.PodLabels(map[string]string{"k8s-app": "cilium"}),
					mother.WithRequiredAntiAffinity("k8s-app", "cilium")),
			},
			wantFit: true,
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
			ok, reason := fit.CanFit(tc.pod, tc.node, free(tc.node), tc.residents, nil)

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
	// CanFit is handed the pod a controller *would create*, never the running
	// one, so an unset NodeName is the normal case and a set one means the
	// template pins it. That was not always true: while CanFit received the
	// running pod, spec.NodeName always named the node being drained, and
	// refusing on it would have refused every relocation there is.
	//
	// This test stays as the tripwire for that older mistake — a replacement
	// with no NodeName must be placeable anywhere — and its counterpart below
	// covers what the change made checkable.
	pod := mother.Pod("default", "web")
	destination := mother.SmallNode("node-b")

	ok, reason := fit.CanFit(pod, destination, free(destination), nil, nil)
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
	ok, reason := fit.CanFit(pod, node, remaining, nil, nil)

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

// TestObservedRequests pins the difference between the two entry points, which
// is a single option and the whole of what a resident costs.
//
// The scheduler sizes a pod already on a node from max(spec, actuated,
// allocated) — PodInfo.CalculateResource passes UseStatusResources, and
// InPlacePodVerticalScaling has been GA and locked on since 1.35, so there is
// no cluster where it does not. A pod resized downward in place has a lowered
// spec and an unchanged actuated figure until the kubelet catches up, and for
// a memory decrease below current usage it never does.
func TestObservedRequests(t *testing.T) {
	mem := func(rl corev1.ResourceList) string {
		q := rl[corev1.ResourceMemory]
		return q.String()
	}

	requests := func(cpu, memory string) corev1.ResourceList {
		return corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		}
	}

	t.Run("a resize still in flight is charged what it actuated", func(t *testing.T) {
		pod := mother.Pod("default", "web",
			mother.ResizingFrom(requests("100m", "1Gi"), requests("100m", "4Gi")))

		if got := mem(fit.ObservedRequests(pod)); got != "4Gi" {
			t.Errorf("memory = %s, want 4Gi: the cgroup still holds it and so does the node", got)
		}
		if got := mem(fit.EffectiveRequests(pod)); got != "1Gi" {
			t.Errorf("memory = %s, want 1Gi: a pod created from this shape asks for the spec", got)
		}
	})

	t.Run("a settled pod is the same under both", func(t *testing.T) {
		// The upgrade path's whole cost, and the reason it is not a
		// consolidation regression on an ordinary cluster:
		// max(spec, actuated, allocated) is spec once a resize has finished,
		// and there is nothing to differ on where none ever started.
		pod := mother.Pod("default", "web", mother.Requests("100m", "1Gi"))

		if got, want := mem(fit.ObservedRequests(pod)), mem(fit.EffectiveRequests(pod)); got != want {
			t.Errorf("memory = %s under ObservedRequests and %s under EffectiveRequests", got, want)
		}
	})

	t.Run("a pod-level resize is read at the pod level", func(t *testing.T) {
		// The second option, and the reason it is not decoration. Where a pod
		// sets spec.resources the scheduler prefers it to the container
		// aggregate outright, so without
		// InPlacePodLevelResourcesVerticalScalingEnabled the pod-level spec
		// overwrites the status-aware container figure and lands back at 1Gi —
		// UseStatusResources alone does not reach this pod. Both gates are on
		// by default at the pinned release: PodLevelResources is Beta from
		// 1.34 and InPlacePodLevelResourcesVerticalScaling Beta from 1.36.
		pod := mother.Pod("default", "web",
			mother.WithPodLevelResources("100m", "1Gi"),
			mother.ResizingFrom(requests("100m", "1Gi"), requests("100m", "4Gi")))

		if got := mem(fit.ObservedRequests(pod)); got != "4Gi" {
			t.Errorf("memory = %s, want 4Gi: the pod-level allocation the node made", got)
		}
		if got := mem(fit.EffectiveRequests(pod)); got != "1Gi" {
			t.Errorf("memory = %s, want 1Gi: the pod-level request a replacement makes", got)
		}
	})

	t.Run("always carries one pod slot", func(t *testing.T) {
		// Both entry points feed the same subtraction loop, so both owe it the
		// synthetic slot. Losing it on one would let the simulation pack
		// unlimited pods onto a node capped at 110.
		got := fit.ObservedRequests(mother.Pod("default", "web"))[corev1.ResourcePods]
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
		ok, _ := fit.CanFit(pod, node, remaining, nil, nil)
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
