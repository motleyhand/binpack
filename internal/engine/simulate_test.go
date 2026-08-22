package engine_test

import (
	"maps"
	"slices"
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

func TestTheReserveAsksAboutSizeNotAboutIdentity(t *testing.T) {
	// The reserve is a margin — "leave room for something as big as your
	// biggest workload" — and never a claim that one specific pod must be
	// placeable. sizeProbe rebuilds the spec field by field to say so, but it
	// copies ObjectMeta wholesale, so the probe still carries the largest
	// workload's labels and namespace. Anything that matches on those reads
	// the probe as that workload and answers a question the reserve is not
	// asking.
	//
	// A cluster-wide anti-affinity index is exactly such a thing, and it fails
	// far wider than a node-local check can: one zone-scoped term matching the
	// biggest workload would veto the margin on every node in the zone and
	// stop consolidation there, reported as "no room" on a cluster with 7Gi
	// free.
	const zone = corev1.LabelTopologyZone
	inZone := mother.NodeLabels(map[string]string{zone: "z1"})
	candidate := sized("candidate", "4Gi", inZone)
	destination := sized("destination", "8Gi", inZone)
	neighbour := sized("neighbour", "8Gi", inZone)

	// The largest relocatable pod in the cluster, and so the shape the reserve
	// probes with. It declares nothing itself; it is only labelled.
	big := mother.Pod("default", "big", mother.OnNode("destination"),
		mother.PodLabels(map[string]string{"app": "web"}), mother.Requests("100m", "512Mi"))
	// The term covering the whole zone, declared by something else and — the
	// point of the third node — sitting nowhere the destination's own resident
	// list would reveal it. Only a cluster-wide index reaches it, so only the
	// cluster-wide index can be what refuses.
	db := mother.Pod("default", "db", mother.OnNode("neighbour"),
		mother.Requests("100m", "128Mi"), mother.WithRequiredAntiAffinityAt(zone, "app", "web"))
	// The pod actually being relocated, which the term cannot select.
	small := mother.Pod("default", "small", mother.OnNode("candidate"),
		mother.Requests("100m", "128Mi"))
	pods := []*corev1.Pod{big, db, small}

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true

	sim := engine.Simulate([]*corev1.Node{candidate, destination, neighbour}, pods,
		mother.Templates(pods...), candidate, cfg)

	if !sim.Feasible {
		t.Errorf("the reserve refused a drain with ~7Gi free, because the probe "+
			"inherited the labels of a workload something else has anti-affinity to: %s",
			sim.Blocked.Summary)
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
		// Both halves. A preferred-only PodAffinity is what a pod asking to
		// sit near a cache carries, and reading it as a required term would
		// make that pod permanently unrelocatable with nothing going red.
		PodAffinity: &corev1.PodAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight: 100,
				PodAffinityTerm: corev1.PodAffinityTerm{
					TopologyKey:   corev1.LabelHostname,
					LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "cache"}},
				},
			}},
		},
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

func TestAZoneAntiAffinityRefusesEveryNodeInTheZone(t *testing.T) {
	// The scheduler counts the pods declaring required anti-affinity across
	// the whole domain a term names, so a zone-keyed term rejects every node
	// in that zone — one holding no matching pod included. Asking each
	// candidate about its own residents asks a narrower question and gets it
	// wrong in the accepting direction, which is the direction that costs:
	// binpack evicts, and the replacement goes Pending in the zone it was
	// sent to.
	//
	// The domain view is built in Simulate and handed down, so this is also
	// what fails if a later refactor stops passing it: internal/fit's own
	// tests would stay green while the simulation went back to being blind.
	const zone = corev1.LabelTopologyZone
	inZone := mother.NodeLabels(map[string]string{zone: "z1"})
	candidate := mother.LargeNode("candidate")
	guarded, bare := mother.LargeNode("guarded", inZone), mother.LargeNode("bare", inZone)

	web := mother.Pod("default", "web", mother.OnNode("candidate"),
		mother.PodLabels(map[string]string{"app": "web"}))
	db := mother.Pod("default", "db", mother.OnNode("guarded"),
		mother.WithRequiredAntiAffinityAt(zone, "app", "web"))
	pods := []*corev1.Pod{web, db}

	sim := engine.Simulate([]*corev1.Node{candidate, guarded, bare}, pods,
		mother.Templates(pods...), candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("accepted a destination in a zone whose anti-affinity rejects the replacement")
	}
	if got := sim.Blocked.PerNode["bare"]; !strings.Contains(got, zone+"=z1") {
		t.Errorf("the empty node in the zone should be refused for the domain it is in, got: %q", got)
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

// TestANodeTheAutoscalerIsRemovingIsNotADestination is the destination-side
// half of the question the candidate side already answers.
//
// The autoscaler taints; it does not necessarily cordon. Its
// --cordon-node-before-terminating flag defaulted to false through
// cluster-autoscaler 1.33 and only became true at 1.34, and operators set it
// either way — so spec.unschedulable is false on a node that is minutes from
// deletion, and the cordon check in fit does not see it. What kept binpack
// away from such a node was only that the taint is NoSchedule and the pod
// happened not to tolerate it, which is a property of the workload rather than
// a decision binpack made.
func TestANodeTheAutoscalerIsRemovingIsNotADestination(t *testing.T) {
	candidate := sized("candidate", "4Gi")
	doomed := sized("doomed", "4Gi",
		mother.Tainted(engine.TaintToBeDeleted, "1786971071", corev1.TaintEffectNoSchedule))

	// Tolerating everything is the blanket `operator: Exists` a chart's
	// "run anywhere" values produce. It is what makes the doomed node look
	// like capacity.
	pods := []*corev1.Pod{
		mother.Pod("default", "web", mother.OnNode("candidate"),
			mother.Requests("100m", "1Gi"), mother.ToleratingEverything()),
	}

	sim := engine.Simulate([]*corev1.Node{candidate, doomed}, pods,
		mother.Templates(pods...), candidate, defaultCfg())

	if sim.Feasible {
		t.Fatalf("a node the autoscaler is deleting is not capacity; %s/%s was placed on %s",
			sim.Relocated[0].Pod.Namespace, sim.Relocated[0].Pod.Name, sim.Relocated[0].Node.Name)
	}
}

// TestHeadroomNamesWhyEachDestinationRefused is the difference between an
// operator raising a pool maximum and an operator removing a taint.
//
// The reserve probes every destination with a pod the size of the largest
// relocatable one, and fit answers with the wall each node hit — cordoned, not
// Ready, tainted, or genuinely short. Reporting all four as "no room" is a
// capacity claim about a node with 64Gi free, and none of the responses it
// invites touches the thing that actually refused.
func TestHeadroomNamesWhyEachDestinationRefused(t *testing.T) {
	// The pod fits `tight` exactly, so the relocation succeeds and the
	// refusal under test is the reserve's, not the placement's.
	candidate := sized("candidate", "4Gi")
	tight := sized("tight", "2Gi")
	roomy := sized("roomy", "64Gi", mother.Tainted("gpu", "true", corev1.TaintEffectNoSchedule))

	pods := []*corev1.Pod{
		mother.Pod("default", "moving", mother.OnNode("candidate"), mother.Requests("100m", "2Gi")),
	}

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true
	sim := engine.Simulate([]*corev1.Node{candidate, tight, roomy}, pods,
		mother.Templates(pods...), candidate, cfg)

	if sim.Feasible {
		t.Fatal("packing the cluster to the last byte leaves nowhere for the next pod that restarts")
	}
	if got := sim.Blocked.PerNode["roomy"]; !strings.Contains(got, "gpu") {
		t.Errorf("roomy refused with %q, want the taint that actually stopped it — "+
			"it has 64Gi free and nothing about it is short of room", got)
	}

	// The other direction, and the one a wholesale switch to fit's message
	// would lose: a genuine shortfall has to keep saying what was being asked
	// for. "insufficient memory" alone leaves the reader without the size,
	// which is the number the reserve is about.
	short := sim.Blocked.PerNode["tight"]
	if !strings.Contains(short, "memory") {
		t.Errorf("tight refused with %q, want the resource it ran out of", short)
	}
	if !strings.Contains(short, "largest") {
		t.Errorf("tight refused with %q, want the size the reserve was asking about", short)
	}
	// And only there. A node that refused for a taint refused whatever size
	// was being asked for, so pinning the reserve's question onto its sentence
	// would put a capacity claim back on a node with 64Gi free.
	if strings.Contains(sim.Blocked.PerNode["roomy"], "largest") {
		t.Errorf("roomy refused with %q, which reads as a shortfall and is not one",
			sim.Blocked.PerNode["roomy"])
	}
}

// TestAPodLevelRefusalIsNotReportedPerDestination is a wall binpack hit before
// it looked at a single node, reported once per node.
//
// fit answers "this pod declares something binpack does not model" before any
// node property is read, so storing that answer under every destination's name
// asserts N node-specific walls that were never observed — under a verdict and
// a summary the how-to guide primes an operator to read as capacity. The
// cluster may have room to spare; binpack simply declined to model the pod.
func TestAPodLevelRefusalIsNotReportedPerDestination(t *testing.T) {
	candidate := sized("candidate", "4Gi")
	pods := []*corev1.Pod{
		mother.Pod("default", "web", mother.OnNode("candidate"),
			mother.Requests("100m", "128Mi"), mother.WithHostPort(8080)),
	}
	nodes := []*corev1.Node{candidate,
		sized("dest-a", "64Gi"), sized("dest-b", "64Gi"), sized("dest-c", "64Gi")}

	sim := engine.Simulate(nodes, pods, mother.Templates(pods...), candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("a pod binpack does not model cannot be shown to fit anywhere")
	}
	if len(sim.Blocked.PerNode) != 0 {
		t.Errorf("PerNode = %v, want no per-destination refusals: "+
			"no node was asked, and none of them refused", sim.Blocked.PerNode)
	}
	if !strings.Contains(sim.Blocked.Summary, "hostPort") {
		t.Errorf("summary = %q, want the thing binpack does not model", sim.Blocked.Summary)
	}
}

// TestAGenuineShortfallIsStillReportedPerDestination is the negative half of
// the hoist above, and the half a condition placed one line too high takes
// away silently.
//
// "Which wall did each node hit" is the question the refusal map exists to
// answer, and it is answerable exactly when the walls are node properties.
// Suppressing it for a pod binpack does not model must not suppress it for a
// pod that simply does not fit.
func TestAGenuineShortfallIsStillReportedPerDestination(t *testing.T) {
	candidate := sized("candidate", "4Gi")
	pods := []*corev1.Pod{
		mother.Pod("default", "web", mother.OnNode("candidate"), mother.Requests("100m", "4Gi")),
	}
	nodes := []*corev1.Node{candidate, sized("dest-a", "1Gi"), sized("dest-b", "1Gi")}

	sim := engine.Simulate(nodes, pods, mother.Templates(pods...), candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("a 4Gi pod does not fit on a 1Gi node")
	}
	if len(sim.Blocked.PerNode) != 2 {
		t.Errorf("PerNode = %v, want a reason for each destination that refused", sim.Blocked.PerNode)
	}
}

// wideNode is a destination whose CPU and memory are set independently, so a
// test can build an exact tie in one dimension and a difference in the other —
// which is the shape a single-dimension ordering key cannot see.
func wideNode(name, cpu, memory string) *corev1.Node {
	return mother.Node(name, mother.Allocatable(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(memory),
		corev1.ResourcePods:   resource.MustParse("110"),
	}))
}

// placements renders a simulation as pod name to node name, which is the part
// of the answer that has to be identical between two runs over the same
// cluster.
func placements(sim engine.Simulation) map[string]string {
	out := make(map[string]string, len(sim.Relocated))
	for _, p := range sim.Relocated {
		out[p.Pod.Name] = p.Node.Name
	}
	return out
}

// TestPackingDoesNotDependOnInputOrder is the property both frontends rest on.
//
// The controller lists from a watch-backed cache, whose List walks a Go map;
// `explain` lists from a live client, whose List is ordered by storage key.
// Neither order is wrong, and neither is stable — the cache's differs between
// two calls in the same process. So any tie the packing breaks by slice order
// makes the same cluster feasible for one frontend and infeasible for the
// other, and makes a mid-drain revalidation disagree with the decision that
// started the drain.
func TestPackingDoesNotDependOnInputOrder(t *testing.T) {
	// An exact tie on free memory between two destinations that differ in the
	// dimension a memory key cannot see. Whichever is tried first takes the
	// 8Gi pod, and only one of the two answers leaves the 6-core pod a home.
	candidate := sized("candidate", "16Gi")
	thin := wideNode("thin", "1", "8Gi")
	wide := wideNode("wide", "8", "8Gi")

	memPod := mother.Pod("default", "mem-pod",
		mother.OnNode("candidate"), mother.Requests("100m", "8Gi"))
	cpuPod := mother.Pod("default", "cpu-pod",
		mother.OnNode("candidate"), mother.Requests("6", "8Gi"))

	orders := []struct {
		name  string
		nodes []*corev1.Node
		pods  []*corev1.Pod
	}{
		{"thin first, mem first", []*corev1.Node{candidate, thin, wide}, []*corev1.Pod{memPod, cpuPod}},
		{"wide first, mem first", []*corev1.Node{candidate, wide, thin}, []*corev1.Pod{memPod, cpuPod}},
		{"thin first, cpu first", []*corev1.Node{candidate, thin, wide}, []*corev1.Pod{cpuPod, memPod}},
		{"wide first, cpu first", []*corev1.Node{candidate, wide, thin}, []*corev1.Pod{cpuPod, memPod}},
	}

	var wantFeasible bool
	var want map[string]string
	for i, o := range orders {
		sim := engine.Simulate(o.nodes, o.pods, mother.Templates(o.pods...), candidate, defaultCfg())
		got := placements(sim)
		if i == 0 {
			wantFeasible, want = sim.Feasible, got
			continue
		}
		if sim.Feasible != wantFeasible {
			t.Errorf("%s: Feasible = %t, want %t — the same cluster, listed differently",
				o.name, sim.Feasible, wantFeasible)
		}
		if !maps.Equal(got, want) {
			t.Errorf("%s: placements = %v, want %v", o.name, got, want)
		}
	}

	// Stated separately from the comparison above, which two consistently
	// wrong answers would also satisfy: a packing exists, and both orders
	// must find it.
	if !wantFeasible {
		t.Errorf("both pods have a home — mem-pod on thin and cpu-pod on wide — and the packing missed it")
	}
}

// TestTheHardestPodToPlaceIsPlacedFirst pins the ordering key itself.
//
// First-fit-decreasing needs one scalar per pod, and memory was it. A 7-core
// pod with a trivial memory request therefore sorted below every 2Gi pod on
// the node — last in the packing, and so last in the eviction order the
// executor reads off it, which is the opposite of what that order is for.
func TestTheHardestPodToPlaceIsPlacedFirst(t *testing.T) {
	candidate := wideNode("candidate", "16", "16Gi")
	dest := wideNode("dest", "16", "16Gi")

	web := func(name string) *corev1.Pod {
		return mother.Pod("default", name, mother.OnNode("candidate"), mother.Requests("500m", "2Gi"))
	}
	hog := mother.Pod("default", "hog", mother.OnNode("candidate"), mother.Requests("7", "100Mi"))
	nodes := []*corev1.Node{candidate, dest}

	// hog first because it is the hardest to place; then the three identical
	// web pods by name, because equal difficulty must still be a total order.
	want := []string{"hog", "web-a", "web-b", "web-c"}

	// Listed against the answer both times, and in two different wrong orders.
	// One list would not settle it: at this size the sort falls back to
	// insertion, which leaves a tied run in the order it found it — so a
	// fixture that happened to list the ties correctly would pass with no
	// tie-break at all, agreeing with the defect rather than catching it.
	for _, pods := range [][]*corev1.Pod{
		{web("web-c"), web("web-b"), web("web-a"), hog},
		{web("web-b"), hog, web("web-c"), web("web-a")},
	} {
		listed := []string{pods[0].Name, pods[1].Name, pods[2].Name, pods[3].Name}
		sim := engine.Simulate(nodes, pods, mother.Templates(pods...), candidate, defaultCfg())

		if !sim.Feasible {
			t.Fatalf("listed %v: the destination holds all four: %+v", listed, sim.Blocked)
		}
		var order []string
		for _, p := range sim.Relocated {
			order = append(order, p.Pod.Name)
		}
		if !slices.Equal(order, want) {
			t.Errorf("listed %v: packed in order %v, want %v", listed, order, want)
		}
	}
}

// TestTheReservedShapeDoesNotDependOnInputOrder covers the third comparison in
// the packing, which is a maximum rather than a sort and ties just as often:
// a Deployment's replicas are the same size as each other.
//
// The pod the reserve is held for decides the shape probed against every
// destination, so a maximum settled by list position asks one question through
// the controller and a different one through `explain` — and the two shapes
// need not both fit.
func TestTheReservedShapeDoesNotDependOnInputOrder(t *testing.T) {
	candidate := wideNode("candidate", "16", "16Gi")
	hostA := wideNode("host-a", "16", "16Gi")
	hostB := wideNode("host-b", "16", "16Gi")

	small := mother.Pod("default", "small",
		mother.OnNode("candidate"), mother.Requests("100m", "1Gi"))
	// Equal memory, opposite CPU. Whichever of the two is picked as "largest"
	// decides the verdict — after the drain no node has room for an 8-core
	// replacement and every node has room for a 100m one — and memory alone
	// cannot separate them.
	wide := mother.Pod("default", "wide-one",
		mother.OnNode("host-a"), mother.Requests("8", "4Gi"))
	narrow := mother.Pod("default", "narrow-one",
		mother.OnNode("host-a"), mother.Requests("100m", "4Gi"))
	// Smaller than either, so it never wins the maximum, and wide enough that
	// host-b cannot take an 8-core replacement either.
	filler := mother.Pod("default", "filler",
		mother.OnNode("host-b"), mother.Requests("15", "1Gi"))

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true

	// Both orders must agree; which verdict they agree on is the reserve's own
	// question and not this test's.
	var want *bool
	for _, pods := range [][]*corev1.Pod{
		{small, filler, wide, narrow},
		{small, filler, narrow, wide},
	} {
		sim := engine.Simulate([]*corev1.Node{candidate, hostA, hostB}, pods,
			mother.Templates(pods...), candidate, cfg)
		if want == nil {
			want = &sim.Feasible
			continue
		}
		if sim.Feasible != *want {
			t.Errorf("Feasible = %t, want %t — the same two pods, listed the other way round",
				sim.Feasible, *want)
		}
	}
}

// TestExpendablePodsAreEvictedInAFixedOrder covers the one list the packing
// never sorted, because nothing ranks a pod that needs no destination.
//
// Nothing ranks them, but they are still evicted one at a time and the
// executor reads this order — so leaving it at list position makes which
// expendable pod goes first differ between the controller and `explain`, and
// between one evaluation of an unchanged cluster and the next.
func TestExpendablePodsAreEvictedInAFixedOrder(t *testing.T) {
	candidate := sized("candidate", "4Gi")
	// Listed out of name order, so passing means the sort did the work.
	pods := []*corev1.Pod{
		mother.Pod("default", "filler-c", mother.OnNode("candidate"), mother.Priority(-100)),
		mother.Pod("default", "filler-a", mother.OnNode("candidate"), mother.Priority(-100)),
		mother.Pod("default", "filler-b", mother.OnNode("candidate"), mother.Priority(-100)),
	}
	nodes := []*corev1.Node{candidate, sized("dest", "4Gi")}

	sim := engine.Simulate(nodes, pods, mother.Templates(pods...), candidate, defaultCfg())

	if !sim.Feasible {
		t.Fatalf("expendable pods need no destination: %+v", sim.Blocked)
	}
	var order []string
	for _, pod := range sim.Evicted {
		order = append(order, pod.Name)
	}
	want := []string{"filler-a", "filler-b", "filler-c"}
	if !slices.Equal(order, want) {
		t.Errorf("evicting in order %v, want %v", order, want)
	}
}

// TestEquallyEmptyDestinationsAreTriedInNameOrder pins the other half.
//
// Which of two interchangeable nodes takes the pod does not matter; that the
// answer is the same one every time does. The name is the only key the engine
// has that is unique, stable, and identical through both frontends.
func TestEquallyEmptyDestinationsAreTriedInNameOrder(t *testing.T) {
	candidate := sized("candidate", "4Gi")
	pods := []*corev1.Pod{
		mother.Pod("default", "web", mother.OnNode("candidate"), mother.Requests("100m", "1Gi")),
	}
	// Listed out of name order, so passing means the sort did the work.
	nodes := []*corev1.Node{candidate, sized("zulu", "4Gi"), sized("alpha", "4Gi")}

	sim := engine.Simulate(nodes, pods, mother.Templates(pods...), candidate, defaultCfg())

	if !sim.Feasible {
		t.Fatalf("either destination holds it: %+v", sim.Blocked)
	}
	if got := sim.Relocated[0].Node.Name; got != "alpha" {
		t.Errorf("placed on %s, want alpha — the tie is arbitrary, but it must be fixed", got)
	}
}
