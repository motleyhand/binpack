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
			// The one place binpack is deliberately stricter than the
			// scheduler, asserted so the divergence is a decision rather than
			// a discovery. There is no NodeReady Filter plugin; a NotReady
			// node is repelled by node.kubernetes.io/not-ready:NoSchedule,
			// which the node-lifecycle controller writes from the condition —
			// so the scheduler *would* place this pod here, and binpack still
			// will not. Costs a consolidation, never a wrong placement.
			name: "not-ready node is refused even when the pod tolerates the taint",
			pod: mother.Pod("default", "web",
				mother.Tolerating("node.kubernetes.io/not-ready", corev1.TaintEffectNoSchedule)),
			node: mother.SmallNode("node-a", mother.NotReady(),
				mother.Tainted("node.kubernetes.io/not-ready", "", corev1.TaintEffectNoSchedule)),
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
			wantCode: fit.ReasonAntiAffinity,
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

func TestAPodRequestingExactlyWhatRemainsFits(t *testing.T) {
	// The boundary between fits and does not, in the direction that costs a
	// consolidation rather than causing one: read as "at least as much as
	// remains is not enough", a node is refused for the pod it can hold
	// exactly — which is the shape of a well-packed node and so of every node
	// worth consolidating onto.
	//
	// It has to be asserted here. The differential harness cannot close it,
	// and not because exact fits are rare — they occur in its own corpus. Its
	// check is one-directional by design: it fails when binpack accepts a
	// placement the scheduler refuses, and an over-conservative refusal is
	// recorded as conservative and passes. Generating more exact fits would
	// leave it green either way.
	node := mother.GPUNode("node-a", 1)
	pod := mother.Pod("default", "exact", mother.Requests("1900m", "1360Mi"),
		mother.Requesting("nvidia.com/gpu", "1"))
	// What the scheduler would reserve, in every dimension at once and the
	// pod slot included, since EffectiveRequests synthesises `pods: 1`.
	exactly := fit.EffectiveRequests(pod)

	if ok, reason := fit.CanFit(pod, node, exactly, nil, nil); !ok {
		t.Fatalf("a pod requesting exactly what remains must fit: %s", reason.Message)
	}

	// And one unit short of any of them must not, per resource kind — so the
	// assertion above is about where the boundary sits rather than about the
	// arithmetic having been switched off.
	for name, unit := range map[corev1.ResourceName]string{
		corev1.ResourceCPU:    "1m",
		corev1.ResourceMemory: "1Ki",
		corev1.ResourcePods:   "1",
		"nvidia.com/gpu":      "1",
	} {
		t.Run(string(name)+" one unit short", func(t *testing.T) {
			short := fit.EffectiveRequests(pod)
			have := short[name]
			have.Sub(resource.MustParse(unit))
			short[name] = have

			ok, reason := fit.CanFit(pod, node, short, nil, nil)
			if ok {
				t.Fatalf("a node one %s short of the request must refuse", name)
			}
			if reason.Code != fit.ReasonInsufficient {
				t.Errorf("reason code = %q, want %q", reason.Code, fit.ReasonInsufficient)
			}
		})
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

	t.Run("a pod-level request overrides one name, not the whole map", func(t *testing.T) {
		// PodRequests computes the container aggregate first and then
		// overrides only the pod-level-supported names actually present in
		// spec.resources.requests (k8s.io/component-helpers v0.36.3
		// resource/helpers.go). Every other name keeps its container-aggregate
		// value.
		//
		// The cpu assertion is the one that settles it. "The scheduler
		// reserves the pod-level figure in place of the container aggregate"
		// — which is what this fixture's comment said — predicts no cpu entry
		// at all, because the pod names only memory. A memory-only assertion
		// agrees with both readings and would have let the wrong one stand.
		pod := mother.Pod("default", "web",
			mother.Requests("500m", "1Gi"),
			mother.WithPodLevelResources("", "2Gi"),
		)
		got := fit.EffectiveRequests(pod)

		if cpu := got[corev1.ResourceCPU]; cpu.String() != "500m" {
			t.Errorf("cpu = %s, want 500m from the container aggregate: the pod "+
				"names only memory, so cpu is not overridden", cpu.String())
		}
		if mem := got[corev1.ResourceMemory]; mem.String() != "2Gi" {
			t.Errorf("memory = %s, want 2Gi from the pod level", mem.String())
		}
		if pods := got[corev1.ResourcePods]; pods.Value() != 1 {
			t.Errorf("pods = %d, want 1", pods.Value())
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

	t.Run("a container resize the kubelet refused is charged what is in force", func(t *testing.T) {
		// The same refused state without pod-level requests, and here there is
		// no ambiguity to be conservative about: UseStatusResources comes from
		// a GA-locked gate, so every scheduler charges max(actuated,
		// allocated) and drops the spec nobody has granted. binpack follows it
		// *down* to 1Gi.
		//
		// Worth asserting rather than assuming, because it is the case that
		// shows this change is not "resolve towards no": matching the
		// scheduler is the rule, and matching it here means charging less than
		// the spec. It also pins the fixture — the kubelet has admitted
		// nothing in this state, so allocatedResources holds the old figure
		// alongside actuated, and a mother that wrote the new one would answer
		// 4Gi and agree with nothing.
		pod := mother.Pod("default", "web",
			mother.ResizeInfeasibleFrom(requests("100m", "4Gi"), requests("100m", "1Gi")))

		if got := mem(fit.ObservedRequests(pod)); got != "1Gi" {
			t.Errorf("memory = %s, want 1Gi: the node has allocated 1Gi and refused the rest", got)
		}
		if got := mem(fit.EffectiveRequests(pod)); got != "4Gi" {
			t.Errorf("memory = %s, want 4Gi: a replacement is created at the spec", got)
		}
	})

	t.Run("a pod-level resize the kubelet refused is charged the larger reading", func(t *testing.T) {
		// The one place the scheduler's own answer is not knowable from the
		// cluster. InPlacePodLevelResourcesVerticalScaling is Beta rather than
		// GA-locked, so an operator can turn it off — and with it off the
		// scheduler charges a pod-level request its spec, while with it on it
		// charges max(actuated, allocated) and drops spec, because the resize
		// is infeasible. Those are 4Gi and 1Gi for this pod, and nothing
		// binpack can read says which scheduler it is talking to.
		//
		// So it charges the larger. Under-charging a resident invents free
		// space and approves a drain the scheduler refuses; over-charging
		// costs a consolidation, which the next run may find.
		pod := mother.Pod("default", "web",
			mother.WithPodLevelResources("100m", "4Gi"),
			mother.ResizeInfeasibleFrom(requests("100m", "4Gi"), requests("100m", "1Gi")))

		if got := mem(fit.ObservedRequests(pod)); got != "4Gi" {
			t.Errorf("memory = %s, want 4Gi: a scheduler with the pod-level gate off charges "+
				"the spec, and binpack cannot see which way that gate is set", got)
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
