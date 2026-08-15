package engine

import (
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	// NoTemplate marks the refusal as "binpack could not read what the
	// replacement would look like" rather than "it did not fit". They are
	// counted apart, because the first is a gap in what binpack models and the
	// second is a fact about the cluster.
	NoTemplate bool
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
func Simulate(
	nodes []*corev1.Node,
	pods []*corev1.Pod,
	templates map[OwnerRef]*corev1.PodTemplateSpec,
	candidate *corev1.Node,
	cfg SimConfig,
) Simulation {
	byNode := indexPodsByNode(pods)

	var sim Simulation
	var toPlace []*corev1.Pod
	// The running pod each replacement stands in for, so a report names an
	// object an operator can find rather than a spec binpack synthesised.
	running := map[*corev1.Pod]*corev1.Pod{}

	for _, pod := range byNode[candidate.Name] {
		if !occupies(pod) {
			continue
		}
		switch Classify(pod, cfg.ExpendablePriorityCutoff) {
		case Relocatable:
			// Placed as the pod its controller will create, not as the pod
			// leaving. See [replacement].
			next, ok := replacement(pod, templates)
			if !ok {
				sim.Blocked = &Blocked{
					Pod: pod,
					Summary: fmt.Sprintf(
						"%s/%s has no readable controller template, so binpack cannot tell what "+
							"its replacement would request", pod.Namespace, pod.Name),
					NoTemplate: true,
				}
				return sim
			}
			toPlace = append(toPlace, next)
			running[next] = pod
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
			was := running[pod]
			sim.Blocked = &Blocked{
				Pod:     was,
				PerNode: perNode,
				Summary: fmt.Sprintf("%s/%s has nowhere to go", was.Namespace, was.Name),
			}
			return sim
		}

		fit.Subtract(remaining[placedOn.Name], fit.EffectiveRequests(pod))
		residents[placedOn.Name] = append(residents[placedOn.Name], pod)
		sim.Relocated = append(sim.Relocated, Placement{Pod: running[pod], Node: placedOn})
	}

	if cfg.ReserveForLargestPod {
		if blocked := checkHeadroom(pods, templates, destinations, remaining, residents, cfg); blocked != nil {
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
	templates map[OwnerRef]*corev1.PodTemplateSpec,
	destinations []*corev1.Node,
	remaining map[string]corev1.ResourceList,
	residents map[string][]*corev1.Pod,
	cfg SimConfig,
) *Blocked {
	// Sized as replacements, like every other placement question: this asks
	// whether a pod of that shape could be *created* somewhere, and a pod
	// resized downward in place would otherwise understate the margin by
	// exactly the amount that matters.
	//
	// Pods whose template cannot be read are skipped rather than refused.
	// Nothing is relocating them — one on the candidate node has already
	// blocked the simulation — and they only inform how large a pod this
	// cluster tends to want. Skipping makes the margin an under-estimate,
	// which costs a missed consolidation rather than a wrong one.
	var largest, largestRunning *corev1.Pod
	for _, pod := range pods {
		if !occupies(pod) || Classify(pod, cfg.ExpendablePriorityCutoff) != Relocatable {
			continue
		}
		next, ok := replacement(pod, templates)
		if !ok {
			continue
		}
		if largest == nil || memoryOf(next) > memoryOf(largest) {
			largest, largestRunning = next, pod
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
		Pod:     largestRunning,
		PerNode: refusals,
		Summary: fmt.Sprintf(
			"draining would leave nowhere for a pod the size of %s/%s, the largest in the cluster",
			largestRunning.Namespace, largestRunning.Name),
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

// OwnerRef identifies a pod's controller, and so the template its replacement
// will be built from.
type OwnerRef struct {
	Namespace string
	Kind      string
	Name      string
}

// ControllerOf returns the reference to a pod's controlling owner.
func ControllerOf(pod *corev1.Pod) (OwnerRef, bool) {
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return OwnerRef{}, false
	}
	return OwnerRef{Namespace: pod.Namespace, Kind: owner.Kind, Name: owner.Name}, true
}

// replacement is the pod that will exist after this one is evicted.
//
// This is the whole point of reading owner templates. `fit` is asked whether a
// *replacement* can be placed and was being handed the *running* pod, which is
// usually the same thing and sometimes is not:
//
//   - A pod resized downward in place carries requests smaller than its
//     template's, and its replacement will ask for the larger figure. Approving
//     a node on the running pod's numbers leaves the replacement Pending and
//     provokes exactly the scale-up binpack exists to prevent. Nothing on the
//     pod records that a completed resize happened — the kubelet updates
//     allocatedResources to match — so the template is the only source of truth.
//   - A template pinning spec.nodeName produces a replacement that bypasses the
//     scheduler entirely. Every running pod has a nodeName, so the running pod
//     cannot show this; the template can.
//
// Identity comes from the running pod so that anything reported names an object
// an operator can actually go and look at. Labels are the union of both: they
// drive anti-affinity matching, and matching more is the refusing direction.
func replacement(pod *corev1.Pod, templates map[OwnerRef]*corev1.PodTemplateSpec) (*corev1.Pod, bool) {
	ref, owned := ControllerOf(pod)
	if !owned {
		return nil, false
	}
	template, ok := templates[ref]
	if !ok {
		return nil, false
	}

	out := &corev1.Pod{
		ObjectMeta: *pod.ObjectMeta.DeepCopy(),
		Spec:       *template.Spec.DeepCopy(),
	}
	for k, v := range template.Labels {
		if out.Labels == nil {
			out.Labels = map[string]string{}
		}
		out.Labels[k] = v
	}
	// NodeName comes from the template and nowhere else, which is what makes a
	// pinned template visible: every running pod names a node, so inheriting
	// it would hide exactly the case worth catching.
	return out, true
}
