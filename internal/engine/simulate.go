package engine

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"

	"github.com/motleyhand/binpack/internal/fit"
)

// SimConfig is the subset of configuration the simulation needs.
type SimConfig struct {
	// ExpendablePriorityCutoff mirrors the autoscaler's flag. Pods strictly
	// below it need not fit anywhere.
	ExpendablePriorityCutoff int32

	// ReserveForLargestPod requires that, after the drain, some node still
	// has room for a pod the size of the largest relocatable pod in the
	// cluster.
	ReserveForLargestPod bool
}

// Placement is one pod's simulated destination.
type Placement struct {
	Pod  *corev1.Pod
	Node *corev1.Node
}

// Blocked records why a simulation failed, in enough detail to be worth
// printing. "It did not fit" is not an answer anyone can act on.
type Blocked struct {
	// Pod is the one that could not be placed, or nil when the simulation
	// failed for a reason that is not about a specific pod.
	Pod *corev1.Pod
	// PerNode maps node name to the reason that node refused, so `explain`
	// can show that three nodes were short of memory and a fourth had a
	// taint.
	PerNode map[string]string
	// Summary is a one-line description.
	Summary string
}

// Simulation is the result of asking whether a node could be emptied.
type Simulation struct {
	Feasible bool

	// Relocated lists where each relocatable pod would go.
	Relocated []Placement
	// Evicted lists pods that are removed without needing a destination:
	// expendable ones. Node-local pods appear in neither, since they are
	// neither moved nor evicted.
	Evicted []*corev1.Pod

	Blocked *Blocked
}

// Simulate asks whether every pod that would need to leave candidate could be
// placed elsewhere.
//
// It is a first-fit-decreasing packing: relocatable pods are tried largest
// first, onto the fullest node that still accepts them. That is a heuristic,
// so it can fail to find a packing that exists — but it never claims one that
// does not, which is the direction that matters. A missed consolidation costs
// nothing; a wrong one costs a scale-up.
//
// Every placement decision is delegated to internal/fit, so the simulation
// composes answers that have been checked against the real scheduler rather
// than inventing its own.
func Simulate(nodes []*corev1.Node, pods []*corev1.Pod, candidate *corev1.Node, cfg SimConfig) Simulation {
	byNode := indexPodsByNode(pods)

	var sim Simulation
	var toPlace []*corev1.Pod

	for _, pod := range byNode[candidate.Name] {
		if !occupies(pod) {
			continue
		}
		switch Classify(pod, cfg.ExpendablePriorityCutoff) {
		case Relocatable:
			toPlace = append(toPlace, pod)
		case Expendable:
			sim.Evicted = append(sim.Evicted, pod)
		case NodeLocal:
			// Neither moved nor evicted: it dies with the node.
		}
	}

	// Destinations start at allocatable minus whatever already sits on them.
	// The candidate is excluded: the whole point is that it goes away.
	remaining := make(map[string]corev1.ResourceList, len(nodes))
	residents := make(map[string][]*corev1.Pod, len(nodes))
	var destinations []*corev1.Node

	for _, node := range nodes {
		if node.Name == candidate.Name {
			continue
		}
		destinations = append(destinations, node)

		free := fit.Allocatable(node)
		for _, pod := range byNode[node.Name] {
			if !occupies(pod) {
				continue
			}
			fit.Subtract(free, fit.EffectiveRequests(pod))
			residents[node.Name] = append(residents[node.Name], pod)
		}
		remaining[node.Name] = free
	}

	// Largest first. A big pod placed late is a big pod with nowhere to go.
	sort.SliceStable(toPlace, func(i, j int) bool {
		return memoryOf(toPlace[i]) > memoryOf(toPlace[j])
	})

	for _, pod := range toPlace {
		placedOn, perNode := place(pod, destinations, remaining, residents)
		if placedOn == nil {
			sim.Blocked = &Blocked{
				Pod:     pod,
				PerNode: perNode,
				Summary: fmt.Sprintf("%s/%s has nowhere to go", pod.Namespace, pod.Name),
			}
			return sim
		}

		fit.Subtract(remaining[placedOn.Name], fit.EffectiveRequests(pod))
		residents[placedOn.Name] = append(residents[placedOn.Name], pod)
		sim.Relocated = append(sim.Relocated, Placement{Pod: pod, Node: placedOn})
	}

	if cfg.ReserveForLargestPod {
		if blocked := checkHeadroom(pods, destinations, remaining, residents, cfg); blocked != nil {
			sim.Blocked = blocked
			return sim
		}
	}

	sim.Feasible = true
	return sim
}

// place finds a destination for one pod, preferring the fullest node that
// still accepts it.
//
// Packing onto the fullest node leaves the emptier ones emptier, which is the
// shape that makes the *next* run's candidate obvious. Spreading would undo
// the consolidation as it went.
func place(
	pod *corev1.Pod,
	destinations []*corev1.Node,
	remaining map[string]corev1.ResourceList,
	residents map[string][]*corev1.Pod,
) (*corev1.Node, map[string]string) {
	ordered := make([]*corev1.Node, len(destinations))
	copy(ordered, destinations)
	sort.SliceStable(ordered, func(i, j int) bool {
		return freeMemory(remaining[ordered[i].Name]) < freeMemory(remaining[ordered[j].Name])
	})

	refusals := make(map[string]string, len(ordered))
	for _, node := range ordered {
		ok, reason := fit.CanFit(pod, node, remaining[node.Name], residents[node.Name])
		if ok {
			return node, nil
		}
		refusals[node.Name] = reason.Message
	}
	return nil, refusals
}

// checkHeadroom enforces ReserveForLargestPod: after the drain, some node must
// still be able to take a pod the size of the largest relocatable one running
// in the cluster.
//
// This exists instead of a percentage of headroom. A percentage is blind to
// absolute capacity — the objection binpack raises against the descheduler —
// and the risk being guarded against is not "the cluster is full" but "the
// next pod that restarts cannot be placed", which is a question about bytes.
func checkHeadroom(
	pods []*corev1.Pod,
	destinations []*corev1.Node,
	remaining map[string]corev1.ResourceList,
	residents map[string][]*corev1.Pod,
	cfg SimConfig,
) *Blocked {
	var largest *corev1.Pod
	for _, pod := range pods {
		if !occupies(pod) || Classify(pod, cfg.ExpendablePriorityCutoff) != Relocatable {
			continue
		}
		if largest == nil || memoryOf(pod) > memoryOf(largest) {
			largest = pod
		}
	}
	if largest == nil {
		return nil
	}

	refusals := make(map[string]string, len(destinations))
	for _, node := range destinations {
		if ok, _ := fit.CanFit(largest, node, remaining[node.Name], residents[node.Name]); ok {
			return nil
		}
		refusals[node.Name] = "no room for the largest relocatable pod"
	}

	return &Blocked{
		Pod:     largest,
		PerNode: refusals,
		Summary: fmt.Sprintf(
			"draining would leave nowhere for a pod the size of %s/%s, the largest in the cluster",
			largest.Namespace, largest.Name),
	}
}

func indexPodsByNode(pods []*corev1.Pod) map[string][]*corev1.Pod {
	byNode := make(map[string][]*corev1.Pod)
	for _, pod := range pods {
		if pod.Spec.NodeName == "" {
			continue
		}
		byNode[pod.Spec.NodeName] = append(byNode[pod.Spec.NodeName], pod)
	}
	return byNode
}

// memoryOf is the ordering key for packing. Memory is chosen because it is
// almost always the binding constraint, and because first-fit-decreasing needs
// a single dimension to sort on. Correctness does not depend on the choice:
// fit.CanFit checks every resource regardless, so a poor ordering costs a
// missed packing, never an invalid one.
func memoryOf(pod *corev1.Pod) int64 {
	requests := fit.EffectiveRequests(pod)
	mem := requests[corev1.ResourceMemory]
	return mem.Value()
}

func freeMemory(remaining corev1.ResourceList) int64 {
	mem := remaining[corev1.ResourceMemory]
	return mem.Value()
}
