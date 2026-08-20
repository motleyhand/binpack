package engine

import (
	"fmt"
	"maps"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

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

	// Built from the whole cluster, once. A required anti-affinity term keyed
	// on anything wider than the hostname rejects every node in its domain,
	// and the node that holds the pod declaring it need not be one binpack is
	// looking at — so the per-node question cannot compute this for itself.
	domains := fit.NewAntiAffinityDomains(nodes, pods)

	var sim Simulation
	var toPlace []*corev1.Pod
	// The running pod each replacement stands in for, so a report names an
	// object an operator can find rather than a spec binpack synthesised.
	running := map[*corev1.Pod]*corev1.Pod{}

	for _, pod := range byNode[candidate.Name] {
		if !Occupies(pod) {
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
			if !Occupies(pod) {
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
		placedOn, perNode := place(pod, destinations, remaining, residents, domains)
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
		if blocked := checkHeadroom(pods, templates, destinations, remaining, residents, domains, cfg); blocked != nil {
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
	domains fit.AntiAffinityDomains,
) (*corev1.Node, map[string]string) {
	ordered := make([]*corev1.Node, len(destinations))
	copy(ordered, destinations)
	sort.SliceStable(ordered, func(i, j int) bool {
		return freeMemory(remaining[ordered[i].Name]) < freeMemory(remaining[ordered[j].Name])
	})

	refusals := make(map[string]string, len(ordered))
	for _, node := range ordered {
		ok, reason := fit.CanFit(pod, node, remaining[node.Name], residents[node.Name], domains)
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
	domains fit.AntiAffinityDomains,
	cfg SimConfig,
) *Blocked {
	// Sized as replacements, like every other placement question: this asks
	// whether a pod of that shape could be *created* somewhere, and a pod
	// resized downward in place would otherwise understate the margin by
	// exactly the amount that matters.
	//
	// A pod whose template cannot be read blocks the proof rather than being
	// skipped. It may be the largest replacement in the cluster, and the
	// reserve is a lower bound on space that must remain: under-estimating it
	// approves a drain that should have been refused, which is a wrong answer
	// and not a missed one. Skipping had that backwards.
	var largest, largestRunning *corev1.Pod
	for _, pod := range pods {
		if !Occupies(pod) || Classify(pod, cfg.ExpendablePriorityCutoff) != Relocatable {
			continue
		}
		// Sized, not validated for relocation: this pod is not being moved, and
		// its placement constraints are discarded by sizeProbe regardless. An
		// unreadable template still blocks, because without one there is no
		// size to reserve for.
		next, ok := sizedReplacement(pod, templates)
		if !ok {
			return &Blocked{
				Pod: pod,
				Summary: fmt.Sprintf(
					"%s/%s has no readable controller template, so binpack cannot tell how much "+
						"room to reserve for a pod of its size", pod.Namespace, pod.Name),
				NoTemplate: true,
			}
		}
		if largest == nil || memoryOf(next) > memoryOf(largest) {
			largest, largestRunning = next, pod
		}
	}
	if largest == nil {
		return nil
	}

	// Asked about a pod of that *shape*, not about that pod. The reserve is a
	// margin — "leave room for something as big as your biggest workload" —
	// and was never a claim that one specific pod must be placeable.
	//
	// The difference is not academic. A StatefulSet's claim makes its pod
	// unmodellable to fit, so asking about the pod itself refuses every
	// destination on every cluster with a PVC-backed StatefulSet, turning the
	// default reserve into "never drain anything" — and reporting it as "no
	// room", which is not what happened.
	//
	// Size still comes from the real replacement, so nothing is
	// under-estimated: only the constraints that make a specific pod
	// unplaceable are dropped.
	probe := sizeProbe(largest)

	refusals := make(map[string]string, len(destinations))
	for _, node := range destinations {
		if ok, _ := fit.CanFit(probe, node, remaining[node.Name], residents[node.Name], domains); ok {
			return nil
		}
		refusals[node.Name] = "no room for a pod the size of the largest relocatable one"
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
	// APIVersion and UID are part of the key, not decoration. Kind and name
	// alone alias a custom resource onto a built-in one that happens to share
	// them, and alias a controller onto its own deleted predecessor — either
	// of which would hand a pod an unrelated workload's template and size the
	// move for the wrong shape.
	APIVersion string
	Kind       string
	Name       string
	UID        types.UID
}

// ControllerOf returns the reference to a pod's controlling owner.
func ControllerOf(pod *corev1.Pod) (OwnerRef, bool) {
	owner := metav1.GetControllerOf(pod)
	if owner == nil {
		return OwnerRef{}, false
	}
	return OwnerRef{
		Namespace:  pod.Namespace,
		APIVersion: owner.APIVersion,
		Kind:       owner.Kind,
		Name:       owner.Name,
		UID:        owner.UID,
	}, true
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
// Neither spec alone is safe, so the replacement is built from both.
//
// The template understates in the other direction whenever admission mutates a
// pod on creation: a service-mesh sidecar injected by a webhook, or requests
// filled in by a LimitRange, are on the running pod and absent from the stored
// template. Sizing on the template alone would then approve a node the real
// replacement does not fit — the identical failure, from the opposite cause.
//
// So requests are the per-resource maximum of the two. Each source understates
// a different case and neither overstates, which makes the larger of them
// conservative in both.
//
// Labels come from the template, not the running pod and not a union of the
// two. Selector matching is not monotonic — an extra label makes an `In`
// selector match and a `DoesNotExist` selector stop matching — so "more labels"
// is not a safe direction and only the set the replacement will actually carry
// is right. Identity stays the running pod's, so anything reported names an
// object an operator can go and look at.
// replacement is [sizedReplacement] plus the checks that decide whether this
// pod can be *moved*.
//
// Placement constraints the running pod carries and its template does not are
// admission's work, and there is no safe merge for most of them: a required
// affinity term is not something you can take the larger of. Refusing is the
// allowlist rule, and it is counted.
//
// Separate from sizing because the reserve asks a different question. It scans
// every relocatable pod in the cluster to find the largest, and it is not
// moving any of them — so a webhook-mutated workload on some unrelated node
// would otherwise make every candidate refuse, which is the third route to the
// same "one pod blocks all drains" failure this change has now closed.
func replacement(pod *corev1.Pod, templates map[OwnerRef]*corev1.PodTemplateSpec) (*corev1.Pod, bool) {
	out, ok := sizedReplacement(pod, templates)
	if !ok {
		return nil, false
	}
	ref, _ := ControllerOf(pod)
	template := templates[ref]
	if restrictiveDivergence(template, pod) != "" || mutatedVolume(template, pod) != "" {
		return nil, false
	}
	return out, true
}

// sizedReplacement is the pod a controller would create, as far as its
// resource shape goes.
//
// Everything that decides where a particular pod may go is left to
// [replacement]. What remains is what the reserve needs: the template's spec,
// with requests raised to the running pod's and any volume it has but the
// template does not carried over.
func sizedReplacement(pod *corev1.Pod, templates map[OwnerRef]*corev1.PodTemplateSpec) (*corev1.Pod, bool) {
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
		// Carried over so the status-based checks still apply to the
		// replacement. Without it fit sees an empty Status, and an in-flight
		// resize — whose requests are changing underneath the snapshot — stops
		// being refused at exactly the moment it matters most.
		Status: *pod.Status.DeepCopy(),
	}
	out.Labels = maps.Clone(template.Labels)
	raiseRequests(out, pod)
	addMissingVolumes(out, pod)

	// NodeName comes from the template and nowhere else, which is what makes a
	// pinned template visible: every running pod names a node, so inheriting
	// it would hide exactly the case worth catching.
	return out, true
}

// raiseRequests lifts each of the replacement's container requests to the
// running pod's where the running pod asks for more.
//
// Container-by-container and resource-by-resource, matched by name: a sidecar
// present only on the running pod is carried over wholesale, since admission
// will inject it again and the node must have room for it.
func raiseRequests(replacement, running *corev1.Pod) {
	raise := func(target *[]corev1.Container, source []corev1.Container) {
		byName := map[string]int{}
		for i := range *target {
			byName[(*target)[i].Name] = i
		}
		for _, from := range source {
			i, known := byName[from.Name]
			if !known {
				// Injected at admission, so it will be injected again.
				(*target) = append(*target, *from.DeepCopy())
				continue
			}
			into := &(*target)[i]
			if into.Resources.Requests == nil && len(from.Resources.Requests) > 0 {
				into.Resources.Requests = corev1.ResourceList{}
			}
			for name, quantity := range from.Resources.Requests {
				if have, ok := into.Resources.Requests[name]; !ok || quantity.Cmp(have) > 0 {
					into.Resources.Requests[name] = quantity.DeepCopy()
				}
			}
		}
	}

	raise(&replacement.Spec.Containers, running.Spec.Containers)
	raise(&replacement.Spec.InitContainers, running.Spec.InitContainers)

	// RuntimeClass overhead is added by admission from the class, so the
	// running pod carries it and the template does not.
	if replacement.Spec.Overhead == nil {
		replacement.Spec.Overhead = running.Spec.Overhead.DeepCopy()
	}
}

// addMissingVolumes carries over any volume the running pod has and the
// template does not.
//
// Two sources, both legitimate: a StatefulSet's volumeClaimTemplates become
// pod volumes without appearing in spec.template, and admission injects
// volumes for meshes and secret stores. Merging rather than refusing is safe
// because a volume can only *add* placement constraints — a PVC binds the pod
// to its volume's zone, a hostPath to whatever provides it — never remove one.
func addMissingVolumes(replacement, running *corev1.Pod) {
	present := map[string]bool{}
	for _, v := range replacement.Spec.Volumes {
		present[v.Name] = true
	}
	for _, v := range running.Spec.Volumes {
		if !present[v.Name] {
			replacement.Spec.Volumes = append(replacement.Spec.Volumes, *v.DeepCopy())
		}
	}
}

// mutatedVolume reports a volume whose name appears on both sides with a
// different source.
//
// Merging by name alone would keep the template's version, so admission
// rewriting a placement-neutral emptyDir into a PVC of the same name would be
// discarded — and the replacement would pass fit while the real one is bound
// to a volume. Neither source can be called the more constraining of the two in
// general, so this refuses rather than choosing.
func mutatedVolume(template *corev1.PodTemplateSpec, running *corev1.Pod) string {
	declared := make(map[string]corev1.VolumeSource, len(template.Spec.Volumes))
	for _, v := range template.Spec.Volumes {
		declared[v.Name] = v.VolumeSource
	}
	for _, v := range running.Spec.Volumes {
		if was, ok := declared[v.Name]; ok && !equality.Semantic.DeepEqual(was, v.VolumeSource) {
			return v.Name
		}
	}
	return ""
}

// restrictiveDivergence reports a constraint the running pod carries that its
// template does not, naming the first found.
//
// Only fields that *narrow* where a pod may go are checked, and that is the
// whole design. Measured against a real cluster, the fields split three ways:
//
//   - Additive: containers and volumes. The running pod may have more, from an
//     injected sidecar or a volumeClaimTemplate. Merged, since more of either
//     can only narrow placement further.
//   - Permissive: tolerations. The API server adds two NoExecute tolerations to
//     every pod, so these always differ. The template's are used as-is: fewer
//     tolerations means the replacement tolerates less, which costs a missed
//     destination rather than a wrong one.
//   - Restrictive: these. A nodeSelector or required affinity present only after
//     admission makes the replacement look freer than it is, and binpack would
//     approve a destination the scheduler refuses — leaving the pod Pending and
//     provoking the scale-up it exists to prevent.
//
// An earlier attempt compared every field and refused 80 of 122 pods, all of it
// API-server defaulting; the conclusion drawn then was that the check could not
// work. It was comparing the wrong things. Restricted to these five, the same
// cluster diverges on none.
func restrictiveDivergence(template *corev1.PodTemplateSpec, running *corev1.Pod) string {
	switch {
	case !maps.Equal(template.Spec.NodeSelector, running.Spec.NodeSelector):
		return "nodeSelector"
	// Only the required terms. Preferred affinity affects scoring and never
	// filters, so a webhook adding one cannot make a placement fail — and fit
	// deliberately accepts it. Refusing over it would disable consolidation
	// for something that cannot change where a pod can go.
	case !equality.Semantic.DeepEqual(
		requiredAffinity(template.Spec.Affinity), requiredAffinity(running.Spec.Affinity)):
		return "affinity"
	case template.Spec.SchedulerName != running.Spec.SchedulerName:
		return "schedulerName"
	// Likewise, ScheduleAnyway is a preference. ADR-0006 ignores the soft
	// counterparts of every constraint for exactly this reason.
	case !equality.Semantic.DeepEqual(
		hardSpread(template.Spec.TopologySpreadConstraints),
		hardSpread(running.Spec.TopologySpreadConstraints)):
		return "topologySpreadConstraints"
	case !equality.Semantic.DeepEqual(
		template.Spec.RuntimeClassName, running.Spec.RuntimeClassName):
		return "runtimeClassName"
	}
	return ""
}

// requiredAffinity keeps only the terms that filter, discarding preferences.
func requiredAffinity(a *corev1.Affinity) *corev1.Affinity {
	if a == nil {
		return nil
	}
	out := &corev1.Affinity{}
	if a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		out.NodeAffinity = &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		}
	}
	if a.PodAffinity != nil && len(a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0 {
		out.PodAffinity = &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		}
	}
	if a.PodAntiAffinity != nil && len(a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0 {
		out.PodAntiAffinity = &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution,
		}
	}
	if out.NodeAffinity == nil && out.PodAffinity == nil && out.PodAntiAffinity == nil {
		return nil
	}
	return out
}

// hardSpread keeps only the constraints that refuse a placement.
func hardSpread(cs []corev1.TopologySpreadConstraint) []corev1.TopologySpreadConstraint {
	var out []corev1.TopologySpreadConstraint
	for _, c := range cs {
		if c.WhenUnsatisfiable == corev1.DoNotSchedule {
			out = append(out, c)
		}
	}
	return out
}

// sizeProbe is a pod carrying the resource shape of another and nothing else.
//
// Rebuilt field by field rather than copied and stripped, because the question
// is which fields *contribute to size* and everything else is a liability. A
// host port copied along with a container makes fit refuse the probe on every
// node, so one such workload anywhere in the cluster would block every drain —
// the same failure this probe was introduced to fix, arriving by a different
// route.
//
// What size means here is whatever resource.PodRequests reads: the regular
// containers, the init-container peak, native sidecars kept running (hence
// their restart policy), RuntimeClass overhead, and pod-level requests where
// the cluster has them.
func sizeProbe(pod *corev1.Pod) *corev1.Pod {
	shape := func(cs []corev1.Container, keepRestartPolicy bool) []corev1.Container {
		out := make([]corev1.Container, 0, len(cs))
		for _, c := range cs {
			only := corev1.Container{Name: c.Name, Resources: *c.Resources.DeepCopy()}
			if keepRestartPolicy {
				only.RestartPolicy = c.RestartPolicy
			}
			out = append(out, only)
		}
		return out
	}

	return &corev1.Pod{
		ObjectMeta: *pod.ObjectMeta.DeepCopy(),
		Spec: corev1.PodSpec{
			Containers: shape(pod.Spec.Containers, false),
			// A native sidecar is an init container with an Always restart
			// policy, and it is added to the running total rather than folded
			// into the init peak. Dropping the policy would understate it.
			InitContainers: shape(pod.Spec.InitContainers, true),
			Overhead:       pod.Spec.Overhead.DeepCopy(),
			Resources:      pod.Spec.Resources.DeepCopy(),
		},
	}
}
