package engine_test

import (
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

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, candidate, defaultCfg())

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

	sim := engine.Simulate(nodes, pods, candidate, defaultCfg())

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

	sim := engine.Simulate([]*corev1.Node{small, large}, pods, small, defaultCfg())

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

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, candidate, defaultCfg())

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

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, candidate, defaultCfg())

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

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, candidate, defaultCfg())

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

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, candidate, defaultCfg())

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

	sim := engine.Simulate(nodes, pods, candidate, defaultCfg())

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

	tight := engine.Simulate(nodes, pods, candidate, defaultCfg())
	if !tight.Feasible {
		t.Fatalf("without the reservation an exact fit is allowed, blocked: %s", tight.Blocked.Summary)
	}

	cfg := defaultCfg()
	cfg.ReserveForLargestPod = true
	reserved := engine.Simulate(nodes, pods, candidate, cfg)

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

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, candidate, defaultCfg())

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

	sim := engine.Simulate([]*corev1.Node{candidate, destination}, pods, candidate, defaultCfg())

	if sim.Feasible {
		t.Fatal("a cordoned node must not be used as a destination")
	}
}
