package engine_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

const cutoff = int32(-10)

func defaultCfg() engine.SimConfig {
	return engine.SimConfig{ExpendablePriorityCutoff: cutoff}
}

// sized builds a node with exactly the memory a test cares about, and enough
// CPU and pod slots that neither is accidentally the binding constraint.
func sized(name, memory string, opts ...mother.NodeOption) *corev1.Node {
	return mother.Node(name, append([]mother.NodeOption{
		mother.Allocatable(corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("16"),
			corev1.ResourceMemory: resource.MustParse(memory),
			corev1.ResourcePods:   resource.MustParse("110"),
		}),
	}, opts...)...)
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want engine.PodClass
	}{
		{"ordinary pod", mother.Pod("default", "web"), engine.Relocatable},
		{"daemonset pod", mother.DaemonSetPod("kube-system", "cilium"), engine.NodeLocal},
		{"mirror pod", mother.MirrorPod("kube-system", "static"), engine.NodeLocal},
		{
			"below the expendable cutoff",
			mother.Pod("default", "filler", mother.Priority(-100)),
			engine.Expendable,
		},
		{
			// Pause pods sit at or above the cutoff by necessity, which is
			// what makes them real workload for consolidation purposes.
			"overprovisioning pause pod",
			mother.PausePod("default", "buffer"),
			engine.Relocatable,
		},
		{
			// Exactly at the cutoff is not below it, matching the
			// autoscaler's own comparison.
			"exactly at the cutoff",
			mother.Pod("default", "edge", mother.Priority(cutoff)),
			engine.Relocatable,
		},
		{
			// Node-local wins: evicting it would have the DaemonSet
			// controller put it straight back on the same node.
			"daemonset pod below the cutoff is still node-local",
			mother.DaemonSetPod("kube-system", "agent", mother.Priority(-100)),
			engine.NodeLocal,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := engine.Classify(tc.pod, cutoff); got != tc.want {
				t.Errorf("Classify = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTerminatingPodIsNodeLocal(t *testing.T) {
	pod := mother.Pod("default", "leaving")
	now := metav1.Now()
	pod.DeletionTimestamp = &now

	if got := engine.Classify(pod, cutoff); got != engine.NodeLocal {
		t.Errorf("a terminating pod needs no destination and no eviction, got %s", got)
	}
}

func TestSimulateStraightforwardMove(t *testing.T) {
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "4Gi")
	pods := []*corev1.Pod{
		mother.Pod("default", "web", mother.OnNode("candidate"), mother.Requests("100m", "1Gi")),
	}

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, mother.Templates(pods...), candidate, defaultCfg())

	if !sim.Feasible {
		t.Fatalf("expected feasible, blocked: %s", sim.Blocked.Summary)
	}
	if len(sim.Relocated) != 1 || sim.Relocated[0].Node.Name != "destination" {
		t.Errorf("expected one pod moved to destination, got %+v", sim.Relocated)
	}
}

// TestAggregateCapacityIsNotEnough is the case the whole design turns on.
//
// Three nodes with 1Gi free each total 3Gi, which is exactly what the pod
// wants — and none of them can take it. Any implementation that compares sums
// says yes here, drains, and provokes the scale-up binpack exists to prevent.
func TestAggregateCapacityIsNotEnough(t *testing.T) {
	candidate := sized("candidate", "8Gi")
	nodes := []*corev1.Node{
		candidate,
		sized("a", "1Gi"), sized("b", "1Gi"), sized("c", "1Gi"),
	}
	pods := []*corev1.Pod{
		mother.Pod("default", "chunky", mother.OnNode("candidate"), mother.Requests("100m", "3Gi")),
	}

	sim := engine.Simulate(nodes, pods, mother.Templates(pods...), candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("3Gi does not fit in three separate 1Gi holes; summing free capacity is not schedulability")
	}
	if sim.Blocked == nil || sim.Blocked.Pod.Name != "chunky" {
		t.Fatalf("the blocked pod should be named, got %+v", sim.Blocked)
	}
	if len(sim.Blocked.PerNode) != 3 {
		t.Errorf("every candidate destination should record why it refused, got %v", sim.Blocked.PerNode)
	}
}

// TestMixedNodeSizes is the case percentage-based tools get wrong: a small
// node at high utilisation holds less than a large node at moderate
// utilisation has free.
func TestMixedNodeSizes(t *testing.T) {
	small := mother.SmallNode("small") // 1360Mi allocatable
	large := mother.LargeNode("large") // 6800Mi allocatable
	pods := []*corev1.Pod{
		// ~81% of the small node.
		mother.Pod("default", "app", mother.OnNode("small"), mother.Requests("100m", "1100Mi")),
		// ~60% of the large node, leaving ~2700Mi free.
		mother.Pod("default", "big", mother.OnNode("large"), mother.Requests("100m", "4100Mi")),
	}

	sim := engine.Simulate([]*corev1.Node{small, large}, pods, mother.Templates(pods...), small, defaultCfg())

	if !sim.Feasible {
		t.Fatalf("1100Mi fits in 2700Mi of free space; blocked: %s", sim.Blocked.Summary)
	}
}

func TestNodeLocalPodsNeedNoDestination(t *testing.T) {
	// A node carrying nothing but DaemonSets is trivially drainable. If they
	// were counted as needing somewhere to go, no cluster would ever
	// consolidate — every node runs several.
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "256Mi")
	pods := []*corev1.Pod{
		mother.DaemonSetPod("kube-system", "cilium", mother.OnNode("candidate"), mother.Requests("100m", "300Mi")),
		mother.DaemonSetPod("kube-system", "logs", mother.OnNode("candidate"), mother.Requests("100m", "300Mi")),
	}

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, mother.Templates(pods...), candidate, defaultCfg())

	if !sim.Feasible {
		t.Fatalf("a node of only DaemonSets should drain, blocked: %s", sim.Blocked.Summary)
	}
	if len(sim.Relocated) != 0 {
		t.Errorf("DaemonSet pods must not be relocated, got %+v", sim.Relocated)
	}
	if len(sim.Evicted) != 0 {
		t.Errorf("DaemonSet pods must not be evicted either, got %+v", sim.Evicted)
	}
}

func TestExpendablePodsAreEvictedNotPlaced(t *testing.T) {
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "256Mi")
	pods := []*corev1.Pod{
		mother.Pod("default", "filler", mother.OnNode("candidate"),
			mother.Priority(-100), mother.Requests("100m", "2Gi")),
	}

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, mother.Templates(pods...), candidate, defaultCfg())

	if !sim.Feasible {
		t.Fatalf("an expendable pod needs no destination, blocked: %s", sim.Blocked.Summary)
	}
	if len(sim.Evicted) != 1 || sim.Evicted[0].Name != "filler" {
		t.Errorf("expected the expendable pod to be evicted, got %+v", sim.Evicted)
	}
	if len(sim.Relocated) != 0 {
		t.Errorf("an expendable pod must not consume a destination, got %+v", sim.Relocated)
	}
}

func TestPausePodsMustFitSomewhere(t *testing.T) {
	// The trap: pause pods look like filler, but they sit above the cutoff so
	// the autoscaler treats them as real workload. Excluding them here would
	// approve a drain that immediately provokes a scale-up.
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "256Mi")
	pods := []*corev1.Pod{
		mother.PausePod("default", "buffer", mother.OnNode("candidate"), mother.Requests("100m", "2Gi")),
	}

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, mother.Templates(pods...), candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("pause pods are real workload for consolidation; 2Gi does not fit in 256Mi")
	}
}

func TestResidentsConsumeDestinationCapacity(t *testing.T) {
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "2Gi")
	pods := []*corev1.Pod{
		mother.Pod("default", "moving", mother.OnNode("candidate"), mother.Requests("100m", "1500Mi")),
		mother.Pod("default", "sitting", mother.OnNode("destination"), mother.Requests("100m", "1Gi")),
	}

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, mother.Templates(pods...), candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("the destination already holds 1Gi of a 2Gi node; 1500Mi does not fit alongside it")
	}
}

func TestPacksLargestFirst(t *testing.T) {
	// Placing the big pod last would strand it: 3Gi and 2Gi fit on separate
	// nodes only if the 3Gi one is placed first.
	candidate := sized("candidate", "8Gi")
	nodes := []*corev1.Node{candidate, sized("a", "3Gi"), sized("b", "3Gi")}
	pods := []*corev1.Pod{
		mother.Pod("default", "small", mother.OnNode("candidate"), mother.Requests("100m", "2Gi")),
		mother.Pod("default", "large", mother.OnNode("candidate"), mother.Requests("100m", "3Gi")),
	}

	sim := engine.Simulate(nodes, pods, mother.Templates(pods...), candidate, defaultCfg())

	if !sim.Feasible {
		t.Fatalf("both pods fit if the larger is placed first, blocked: %s", sim.Blocked.Summary)
	}
	if sim.Relocated[0].Pod.Name != "large" {
		t.Errorf("largest should be placed first, got %s", sim.Relocated[0].Pod.Name)
	}
}

func TestReserveForLargestPod(t *testing.T) {
	// A packing that fits exactly leaves nowhere for the next restart. With
	// the reservation on, that is refused; with it off, allowed.
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "2Gi")
	pods := []*corev1.Pod{
		mother.Pod("default", "moving", mother.OnNode("candidate"), mother.Requests("100m", "2Gi")),
	}
	nodes := []*corev1.Node{candidate, destination}

	tight := engine.Simulate(nodes, pods, mother.Templates(pods...), candidate, defaultCfg())
	if !tight.Feasible {
		t.Fatalf("without the reservation an exact fit is allowed, blocked: %s", tight.Blocked.Summary)
	}

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true
	reserved := engine.Simulate(nodes, pods, mother.Templates(pods...), candidate, cfg)

	if reserved.Feasible {
		t.Fatal("packing the cluster to the last byte leaves nowhere for the next pod that restarts")
	}
	if reserved.Blocked == nil || reserved.Blocked.Pod.Name != "moving" {
		t.Errorf("the headroom refusal should name the largest relocatable pod, got %+v", reserved.Blocked)
	}
}

func TestFinishedPodsDoNotOccupy(t *testing.T) {
	// A completed Job's pod has released its resources; counting it would
	// make nodes look fuller than they are and cost consolidations.
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "2Gi")

	done := mother.Pod("default", "finished", mother.OnNode("destination"), mother.Requests("100m", "2Gi"))
	done.Status.Phase = corev1.PodSucceeded

	pods := []*corev1.Pod{
		mother.Pod("default", "moving", mother.OnNode("candidate"), mother.Requests("100m", "1Gi")),
		done,
	}

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, mother.Templates(pods...), candidate, defaultCfg())

	if !sim.Feasible {
		t.Fatalf("a Succeeded pod holds nothing, blocked: %s", sim.Blocked.Summary)
	}
}

func TestUnschedulableDestinationIsRefused(t *testing.T) {
	// A cordoned node is not a destination — including one that binpack or an
	// operator cordoned moments ago.
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "4Gi", mother.Cordoned())
	pods := []*corev1.Pod{
		mother.Pod("default", "web", mother.OnNode("candidate"), mother.Requests("100m", "1Gi")),
	}

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, mother.Templates(pods...), candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("a cordoned node must not be used as a destination")
	}
}

func TestAPodResizedDownwardIsPlacedAtItsTemplateSize(t *testing.T) {
	// The unbounded gap ADR-0006 recorded, and the reason this PR exists.
	//
	// In-place vertical scaling rewrites a running pod's requests without
	// touching its controller's template, and the kubelet updates
	// allocatedResources to match — so a *completed* downward resize leaves
	// nothing on the pod to notice. Sizing the move on what is running would
	// approve a node the replacement does not fit, leaving it Pending and
	// provoking exactly the scale-up binpack exists to prevent.
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "1Gi")

	// Running at 512Mi after a resize; its template still says 3Gi.
	shrunk := mother.Pod("default", "web", mother.OnNode("candidate"), mother.Requests("100m", "512Mi"))
	pods := []*corev1.Pod{shrunk}

	asRunning := mother.Templates(pods...)
	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, asRunning, candidate, defaultCfg())
	if !sim.Feasible {
		t.Fatalf("setup: the shrunken pod should fit 1Gi, got %s", sim.Blocked.Summary)
	}

	templates := mother.Templates(pods...)
	mother.TemplateFor(templates, shrunk, mother.Requests("100m", "3Gi"))

	sim = engine.Simulate([]*corev1.Node{candidate, destination}, pods, templates, candidate, defaultCfg())

	if sim.Feasible {
		t.Error("approved a drain the replacement does not fit: it asks for 3Gi, the destination has 1Gi")
	}
}

func TestAPodWithNoReadableTemplateIsRefused(t *testing.T) {
	// A pod created by an operator's own controller. binpack cannot tell what
	// its replacement would request, and guessing from the running pod is the
	// guess that is unsound. Counted, so ADR-0006's "settle it against
	// measurement" can settle it.
	candidate := mother.LargeNode("candidate")
	destination := mother.LargeNode("destination")
	exotic := mother.Pod("default", "shard-0", mother.OnNode("candidate"),
		mother.OwnedBy("KafkaCluster", "events"))
	pods := []*corev1.Pod{exotic}

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		mother.Templates(pods...), candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("moved a pod whose replacement binpack cannot predict")
	}
	if !sim.Blocked.NoTemplate {
		t.Errorf("refusal is not marked as a gap in what binpack models: %+v", sim.Blocked)
	}
	if !strings.Contains(sim.Blocked.Summary, "no readable controller template") {
		t.Errorf("summary does not say why: %s", sim.Blocked.Summary)
	}
}

func TestATemplateThatPinsANodeIsRefused(t *testing.T) {
	// The second gap. Such a pod ignores cordon entirely — setting nodeName
	// bypasses the scheduler — and reappears on the node being drained. Every
	// running pod names a node, so this was invisible until binpack read the
	// template.
	candidate := mother.LargeNode("candidate")
	destination := mother.LargeNode("destination")
	pinned := mother.Pod("default", "agent", mother.OnNode("candidate"))
	pods := []*corev1.Pod{pinned}

	templates := mother.Templates(pods...)
	mother.TemplateFor(templates, pinned, mother.OnNode("candidate"))

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, templates, candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("planned to relocate a pod its own template pins to the node being drained")
	}
}

func TestReportsNameTheRunningPodNotTheReplacement(t *testing.T) {
	// The replacement is a spec binpack synthesised; it has no existence an
	// operator can go and look at. Everything reported must name what is
	// actually running.
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "256Mi")
	pod := mother.Pod("default", "web", mother.OnNode("candidate"), mother.Requests("100m", "2Gi"))
	pods := []*corev1.Pod{pod}

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		mother.Templates(pods...), candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("setup: 2Gi should not fit in 256Mi")
	}
	if sim.Blocked.Pod != pod {
		t.Errorf("blocked names %v, want the running pod", sim.Blocked.Pod)
	}
	if !strings.Contains(sim.Blocked.Summary, "default/web") {
		t.Errorf("summary does not name a real object: %s", sim.Blocked.Summary)
	}
}

func TestTheReplacementCarriesItsTemplateLabels(t *testing.T) {
	// Labels decide anti-affinity, and anti-affinity is symmetric: a pod
	// already on a destination can refuse an incoming one whose labels match
	// its selector. The replacement will carry the template's labels, so those
	// are what a destination must be asked about.
	candidate := mother.LargeNode("candidate")
	destination := mother.LargeNode("destination")

	// The resident refuses anything labelled app=web.
	resident := mother.Pod("default", "resident", mother.OnNode("destination"),
		mother.WithRequiredAntiAffinity("app", "web"))
	// The moving pod is unlabelled today; its template says app=web.
	moving := mother.Pod("default", "moving", mother.OnNode("candidate"))
	pods := []*corev1.Pod{resident, moving}

	templates := mother.Templates(pods...)
	ref, _ := engine.ControllerOf(moving)
	templates[ref].Labels = map[string]string{"app": "web"}

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, templates, candidate, defaultCfg())

	if sim.Feasible {
		t.Error("placed a pod the destination's anti-affinity will refuse once it carries its template's labels")
	}
}

func TestHeadroomIsSizedOnReplacementsToo(t *testing.T) {
	// The reserve asks whether a pod of the largest shape could still be
	// *created* after the drain. Sizing it on what is running understates the
	// margin by exactly the amount an in-place downward resize removed — and
	// the reserve exists to stop the next scale-up, which is provoked by the
	// replacement, not by the pod that shrank.
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "4Gi")

	// Nothing on the candidate, so the drain is trivially feasible; the
	// question is entirely whether the reserve is big enough.
	shrunk := mother.Pod("default", "big", mother.OnNode("destination"),
		mother.Requests("100m", "512Mi"))
	pods := []*corev1.Pod{shrunk}

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true

	asRunning := mother.Templates(pods...)
	if sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		asRunning, candidate, cfg); !sim.Feasible {
		t.Fatalf("setup: 4Gi has room to spare for another 512Mi, got %s", sim.Blocked.Summary)
	}

	// Its template says 3.6Gi. The destination holds it at its shrunken 512Mi,
	// leaving 3584Mi — enough for another shrunken copy, not for a real one.
	templates := mother.Templates(pods...)
	mother.TemplateFor(templates, shrunk, mother.Requests("100m", "3600Mi"))

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, templates, candidate, cfg)

	if sim.Feasible {
		t.Error("reserved headroom for the shrunken pod rather than for the replacement it would be recreated as")
	}
}

func TestTheReserveDoesNotRefuseAClusterWithObviousRoom(t *testing.T) {
	// The reserve is checked by asking whether a pod of the largest shape
	// could be placed. It must be asked about a *replacement*: a running pod
	// carries a nodeName, and fit refuses those outright now that an unset one
	// is the normal case. Handing it the running pod would refuse every
	// cluster there is — while printing the message a genuine shortfall
	// prints, which is what makes this worth its own test.
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "8Gi")
	pods := []*corev1.Pod{
		mother.Pod("default", "small", mother.OnNode("destination"), mother.Requests("100m", "256Mi")),
	}

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		mother.Templates(pods...), candidate, cfg)

	if !sim.Feasible {
		t.Errorf("refused a drain with 7.75Gi free and a 256Mi largest pod: %s", sim.Blocked.Summary)
	}
}

func TestAdmissionInjectedContainersAreCarriedOntoTheReplacement(t *testing.T) {
	// The template understates whenever admission mutates a pod on creation —
	// a service-mesh sidecar from a webhook, requests filled in by a
	// LimitRange. Sizing on the template alone approves a node the real
	// replacement does not fit: the same failure as an in-place resize, from
	// the opposite cause. Requests are the maximum of both for that reason.
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "1Gi")

	// Running with an injected sidecar its template knows nothing about.
	injected := mother.Pod("default", "web", mother.OnNode("candidate"), mother.Requests("100m", "256Mi"))
	injected.Spec.Containers = append(injected.Spec.Containers, corev1.Container{
		Name: "istio-proxy",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("900Mi")},
		},
	})
	pods := []*corev1.Pod{injected}

	templates := mother.Templates(pods...)
	mother.TemplateFor(templates, injected, mother.Requests("100m", "256Mi"))

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, templates, candidate, defaultCfg())

	if sim.Feasible {
		t.Error("sized the move on the raw template: the injected sidecar needs 900Mi more than it accounts for")
	}
}

func TestTheReplacementTakesTheLargerRequestOfEitherSpec(t *testing.T) {
	// Each source understates a different case and neither overstates.
	running := mother.Pod("default", "web", mother.OnNode("a"), mother.Requests("500m", "128Mi"))
	pods := []*corev1.Pod{running}
	templates := mother.Templates(pods...)
	mother.TemplateFor(templates, running, mother.Requests("100m", "2Gi"))

	candidate := sized("a", "8Gi")
	// Room for 2Gi but only 400m of CPU: the template wins on memory, the
	// running pod on CPU, and both must be respected at once.
	destination := mother.Node("b", mother.Allocatable(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("400m"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	}))

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, templates, candidate, defaultCfg())

	if sim.Feasible {
		t.Error("took only the template's requests: the running pod asks for 500m and the destination has 400m")
	}
}

func TestHeadroomRefusesWhenATemplateCannotBeRead(t *testing.T) {
	// The reserve is a lower bound on space that must remain. An unreadable
	// pod may be the largest replacement in the cluster, so under-estimating
	// it approves a drain that should have been refused — a wrong answer, not
	// a missed one.
	candidate := mother.LargeNode("candidate")
	destination := mother.LargeNode("destination")
	exotic := mother.Pod("default", "shard-0", mother.OnNode("destination"),
		mother.OwnedBy("KafkaCluster", "events"))
	pods := []*corev1.Pod{exotic}

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		mother.Templates(pods...), candidate, cfg)

	if sim.Feasible {
		t.Fatal("reserved headroom while unable to size the cluster's largest pod")
	}
	if !sim.Blocked.NoTemplate {
		t.Errorf("refusal is not marked as a gap in what binpack models: %+v", sim.Blocked)
	}
}

func TestRuntimeClassOverheadSurvivesOnTheReplacement(t *testing.T) {
	// Overhead is added at admission from the RuntimeClass, so it is on the
	// running pod and absent from the template — and the scheduler reserves
	// it. Losing it understates every gVisor or Kata pod by its sandbox cost.
	candidate := sized("candidate", "4Gi")
	destination := sized("destination", "1Gi")
	heavy := mother.Pod("default", "sandboxed", mother.OnNode("candidate"),
		mother.Requests("100m", "512Mi"), mother.WithOverhead("100m", "700Mi"))
	pods := []*corev1.Pod{heavy}

	templates := mother.Templates(pods...)
	mother.TemplateFor(templates, heavy, mother.Requests("100m", "512Mi"))

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, templates, candidate, defaultCfg())

	if sim.Feasible {
		t.Error("dropped the sandbox overhead: 512Mi + 700Mi does not fit 1Gi")
	}
}

func TestAnInFlightResizeIsStillRefusedOnTheReplacement(t *testing.T) {
	// The replacement is synthesised, so an empty Status would silently drop
	// every status-based refusal — including the one for requests that are
	// changing underneath the snapshot, which is where reasoning through them
	// is least defensible.
	candidate := mother.LargeNode("candidate")
	destination := mother.LargeNode("destination")
	resizing := mother.Pod("default", "web", mother.OnNode("candidate"), mother.Resizing())
	pods := []*corev1.Pod{resizing}

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		mother.Templates(pods...), candidate, defaultCfg())

	if sim.Feasible {
		t.Error("reasoned through a pod whose requests are changing underneath the snapshot")
	}
}

func TestRestrictiveConstraintsAddedByAdmissionBlockTheSimulation(t *testing.T) {
	// A webhook adding a nodeSelector leaves the template looking freer than
	// the pod the scheduler will receive. binpack would approve a destination
	// the replacement cannot use, the pod would go Pending, and the autoscaler
	// would add the node binpack exists to avoid — while the drain reported
	// success, since nothing failed from binpack's point of view.
	for _, tc := range []struct {
		name    string
		running mother.PodOption
	}{
		{"nodeSelector", mother.WithNodeSelector("disk", "ssd")},
		{"required affinity", mother.WithRequiredAntiAffinity("app", "web")},
		{"topology spread", mother.WithHardTopologySpread("topology.kubernetes.io/zone")},
		{"scheduler name", mother.ScheduledBy("stork")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate, destination := mother.LargeNode("candidate"), mother.LargeNode("destination")
			pod := mother.Pod("default", "web", mother.OnNode("candidate"), tc.running)
			pods := []*corev1.Pod{pod}

			templates := mother.Templates(pods...)
			mother.TemplateFor(templates, pod) // the template lacks the constraint

			sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
				templates, candidate, defaultCfg())

			if sim.Feasible {
				t.Errorf("planned a move from a template missing the pod's %s", tc.name)
			}
			if !sim.Blocked.NoTemplate {
				t.Errorf("not counted as a gap in what binpack models: %+v", sim.Blocked)
			}
		})
	}
}

func TestPermissiveAndDefaultedDifferencesDoNotBlockAnything(t *testing.T) {
	// The API server adds two NoExecute tolerations and a projected token
	// volume to every pod. Refusing on that rejected 80 of 122 pods on a real
	// cluster the first time this was attempted, which is what made the check
	// look unworkable — it was comparing the wrong things.
	candidate, destination := mother.LargeNode("candidate"), mother.LargeNode("destination")
	pod := mother.Pod("default", "web", mother.OnNode("candidate"),
		mother.Tolerating("node.kubernetes.io/not-ready", corev1.TaintEffectNoExecute),
		mother.WithConfigMapVolume("kube-api-access-abcde"))
	pods := []*corev1.Pod{pod}

	templates := mother.Templates(pods...)
	mother.TemplateFor(templates, pod) // no tolerations, no volumes

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		templates, candidate, defaultCfg())

	if !sim.Feasible {
		t.Errorf("refused over defaulting rather than admission: %s", sim.Blocked.Summary)
	}
}

func TestAVolumeOnlyTheRunningPodHasIsCarriedOver(t *testing.T) {
	// A StatefulSet's volumeClaimTemplates become pod volumes without ever
	// appearing in spec.template. Dropping them would hand fit a replacement
	// with no claim at all — and fit refuses a pod with a PVC, because a bound
	// volume constrains where its pod may go. So the replacement must carry
	// it: merging is safe precisely because a volume can only narrow
	// placement, never widen it.
	candidate, destination := mother.LargeNode("candidate"), mother.LargeNode("destination")
	pod := mother.Pod("default", "db-0", mother.OnNode("candidate"), mother.WithPVC("data"))
	pods := []*corev1.Pod{pod}

	templates := mother.Templates(pods...)
	mother.TemplateFor(templates, pod) // the template declares no volumes

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		templates, candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("placed a pod whose claim was dropped along with the template's volume list")
	}
	// Refused for the claim, not for being unmodellable: the template was read
	// perfectly well, and the volume is a real constraint on the replacement.
	if sim.Blocked.NoTemplate {
		t.Errorf("refused as an unreadable template rather than for the claim: %+v", sim.Blocked)
	}
}

func TestTheReserveAsksAboutASizeNotAParticularPod(t *testing.T) {
	// The reserve is a margin — leave room for something as big as the biggest
	// workload — and never a claim that one specific pod must be placeable.
	//
	// A StatefulSet's claim makes its pod unmodellable to fit, so asking about
	// the pod itself refuses every destination on any cluster with a
	// PVC-backed StatefulSet. That turns the default reserve into "never drain
	// anything" and reports it as "no room", which is not what happened.
	candidate := sized("candidate", "8Gi")
	destination := sized("destination", "8Gi")

	// The largest relocatable pod in the cluster is claim-bound, and lives
	// somewhere other than the candidate so it is only ever the reserve's
	// yardstick.
	bound := mother.Pod("monitoring", "prometheus-0", mother.OnNode("destination"),
		mother.Requests("100m", "1Gi"), mother.WithPVC("data"))
	pods := []*corev1.Pod{bound}

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		mother.Templates(pods...), candidate, cfg)

	if !sim.Feasible {
		t.Errorf("the reserve refused over a claim rather than over capacity: %s",
			sim.Blocked.Summary)
	}
}

func TestTheReserveStillMeasuresTheRealSize(t *testing.T) {
	// Dropping a pod's constraints must not drop its requests: the yardstick
	// is the size of the largest replacement, and under-estimating it approves
	// a drain that should have been refused.
	candidate := sized("candidate", "8Gi")
	destination := sized("destination", "2Gi")
	big := mother.Pod("monitoring", "prometheus-0", mother.OnNode("destination"),
		mother.Requests("100m", "1500Mi"), mother.WithPVC("data"))
	pods := []*corev1.Pod{big}

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		mother.Templates(pods...), candidate, cfg)

	// 2Gi holds the running 1500Mi pod; a second one of that size does not fit.
	if sim.Feasible {
		t.Error("the reserve lost the pod's size along with its constraints")
	}
	if !strings.Contains(sim.Blocked.Summary, "prometheus-0") {
		t.Errorf("the refusal does not name what it measured: %s", sim.Blocked.Summary)
	}
}

func TestTheReserveKeepsRuntimeClassOverhead(t *testing.T) {
	// Overhead is what a sandbox costs on top of the containers, and the
	// scheduler reserves it. A probe that dropped it would understate every
	// gVisor or Kata pod by exactly that amount — and the reserve exists to
	// stop the next scale-up, which the overhead is part of.
	candidate := sized("candidate", "8Gi")
	destination := sized("destination", "3584Mi") // 3.5Gi

	// 1Gi of containers plus 1Gi of sandbox: 2Gi effective, leaving 1.5Gi.
	// A second pod of that shape needs 2Gi and does not fit; one measured
	// without its overhead would look like 1Gi and appear to.
	sandboxed := mother.Pod("default", "sandboxed", mother.OnNode("destination"),
		mother.Requests("100m", "1Gi"), mother.WithOverhead("100m", "1Gi"))
	pods := []*corev1.Pod{sandboxed}

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		mother.Templates(pods...), candidate, cfg)

	if sim.Feasible {
		t.Error("the reserve measured the containers and forgot the sandbox they run in")
	}
}

func TestTheProbeCarriesEverySourceOfSize(t *testing.T) {
	// Whatever resource.PodRequests reads has to reach the probe, or the
	// reserve measures something smaller than the pod it named and approves a
	// drain that should have been refused.
	for _, tc := range []struct {
		name string
		opt  mother.PodOption
	}{
		{"native sidecar", mother.WithSidecar("proxy", "100m", "1Gi")},
		{"init container peak", mother.WithInitContainer("migrate", "100m", "2Gi")},
		{"runtimeclass overhead", mother.WithOverhead("100m", "1Gi")},
		// Pod-level requests replace the container aggregate where set, so a
		// pod whose containers ask for almost nothing can still be large.
		{"pod-level resources", mother.WithPodLevelResources("100m", "1536Mi")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := sized("candidate", "8Gi")
			// 512Mi of containers plus 1Gi from the feature under test, against
			// a destination holding one already and 1Gi to spare.
			destination := sized("destination", "2560Mi")
			big := mother.Pod("default", "big", mother.OnNode("destination"),
				mother.Requests("100m", "512Mi"), tc.opt)
			pods := []*corev1.Pod{big}

			cfg := defaultCfg()
			cfg.ReserveForLargestPod = true

			sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
				mother.Templates(pods...), candidate, cfg)

			if sim.Feasible {
				t.Errorf("the probe lost the pod's %s and measured it too small", tc.name)
			}
		})
	}
}

func TestTheProbeDropsWhatOnlyConstrainsAParticularPod(t *testing.T) {
	// A host port makes fit refuse a pod outright. Copied into a size-only
	// probe it would refuse every destination, so one such workload anywhere
	// in the cluster would block every drain — the failure the probe exists to
	// prevent, reached by another route.
	candidate, destination := mother.LargeNode("candidate"), mother.LargeNode("destination")
	hostPorted := mother.Pod("default", "ingress", mother.OnNode("destination"),
		mother.WithHostPort(8080))
	pods := []*corev1.Pod{hostPorted}

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		mother.Templates(pods...), candidate, cfg)

	if !sim.Feasible {
		t.Errorf("the reserve refused over a host port rather than over capacity: %s",
			sim.Blocked.Summary)
	}
}

func TestPreferencesAreNotConstraints(t *testing.T) {
	// Preferred affinity and ScheduleAnyway spreads affect scoring and never
	// filter, so a webhook adding one cannot make a placement fail — and fit
	// deliberately accepts both. Refusing over them would disable
	// consolidation for something that cannot change where a pod may go.
	candidate, destination := mother.LargeNode("candidate"), mother.LargeNode("destination")
	pod := mother.Pod("default", "web", mother.OnNode("candidate"))
	pod.Spec.Affinity = &corev1.Affinity{
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight: 100,
				PodAffinityTerm: corev1.PodAffinityTerm{
					TopologyKey:   corev1.LabelHostname,
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}},
				},
			}},
		},
	}
	pod.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone",
		WhenUnsatisfiable: corev1.ScheduleAnyway,
	}}
	pods := []*corev1.Pod{pod}

	templates := mother.Templates(pods...)
	mother.TemplateFor(templates, pod) // the template has neither

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		templates, candidate, defaultCfg())

	if !sim.Feasible {
		t.Errorf("refused over a preference that cannot filter: %s", sim.Blocked.Summary)
	}
}

func TestAVolumeRewrittenUnderTheSameNameIsRefused(t *testing.T) {
	// Merging by name keeps the template's version, so admission turning a
	// placement-neutral emptyDir into a PVC of the same name would be
	// discarded — and the replacement would pass fit while the real one is
	// bound to a volume. Neither source is the more constraining in general,
	// so this refuses rather than choosing.
	candidate, destination := mother.LargeNode("candidate"), mother.LargeNode("destination")
	pod := mother.Pod("default", "web", mother.OnNode("candidate"), mother.WithPVC("data"))
	pods := []*corev1.Pod{pod}

	templates := mother.Templates(pods...)
	// Same name, placement-neutral source.
	mother.TemplateFor(templates, pod, mother.WithEmptyDir("data"))

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		templates, candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("kept the template's emptyDir over the claim the pod actually has")
	}
	if !sim.Blocked.NoTemplate {
		t.Errorf("not counted as a gap in what binpack models: %+v", sim.Blocked)
	}
}

func TestAnAdmissionMutatedPodElsewhereDoesNotBlockTheReserve(t *testing.T) {
	// The reserve scans every relocatable pod in the cluster to find the
	// largest, and moves none of them. A webhook-added selector on some
	// unrelated node says nothing about how much room to leave — and refusing
	// over it would let one such workload block every drain, which is the
	// third route to that failure in this change alone.
	candidate := mother.LargeNode("candidate")
	destination := mother.LargeNode("destination")

	// Nothing on the candidate, so the drain is trivially feasible and the
	// reserve is the only question.
	mutated := mother.Pod("other", "mesh-injected", mother.OnNode("destination"),
		mother.WithNodeSelector("disk", "ssd"))
	pods := []*corev1.Pod{mutated}

	templates := mother.Templates(pods...)
	mother.TemplateFor(templates, mutated) // the template lacks the selector

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		templates, candidate, cfg)

	if !sim.Feasible {
		t.Errorf("a mutated pod on another node blocked the reserve: %s", sim.Blocked.Summary)
	}
}

func TestRelocatingThatSamePodIsStillRefused(t *testing.T) {
	// The counterpart: the identical divergence must still stop the drain when
	// the pod is one that has to move. Sizing and moving ask different
	// questions of the same object.
	candidate := mother.LargeNode("candidate")
	destination := mother.LargeNode("destination")
	mutated := mother.Pod("other", "mesh-injected", mother.OnNode("candidate"),
		mother.WithNodeSelector("disk", "ssd"))
	pods := []*corev1.Pod{mutated}

	templates := mother.Templates(pods...)
	mother.TemplateFor(templates, mutated)

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		templates, candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("planned to move a pod whose template is missing its selector")
	}
	if !sim.Blocked.NoTemplate {
		t.Errorf("not counted as a gap in what binpack models: %+v", sim.Blocked)
	}
}

func TestTheReserveStillNeedsATemplateToSizeAgainst(t *testing.T) {
	// Sizing tolerates a placement difference; it cannot tolerate having no
	// template at all, because then there is no size to reserve for.
	candidate := mother.LargeNode("candidate")
	destination := mother.LargeNode("destination")
	exotic := mother.Pod("default", "shard-0", mother.OnNode("destination"),
		mother.OwnedBy("KafkaCluster", "events"))
	pods := []*corev1.Pod{exotic}

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods,
		mother.Templates(pods...), candidate, cfg)

	if sim.Feasible {
		t.Fatal("reserved headroom while unable to size the cluster's largest pod")
	}
	if !sim.Blocked.NoTemplate {
		t.Errorf("not counted as a gap in what binpack models: %+v", sim.Blocked)
	}
}
