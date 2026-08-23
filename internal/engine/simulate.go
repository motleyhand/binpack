package engine

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

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
	// has room for a pod of every maximal shape among the relocatable pods
	// running in the cluster. See [checkHeadroom] for why that is not "the
	// largest pod".
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
	// Unmodelled marks the refusal as "binpack could not predict what the
	// replacement would be" rather than "it did not fit", and names which of
	// the two causes it was — [FindingNoTemplate] or
	// [FindingTemplateDivergence]. Empty for an ordinary shortfall.
	//
	// The distinction from a shortfall is the older one: "the workload does
	// not fit" is a fact about the cluster and "binpack cannot tell what the
	// workload is" is a gap in binpack, and they call for different responses.
	//
	// A cause rather than a flag because that reasoning goes one level
	// further, and stopping at the flag made both surfaces say the wrong
	// thing. An unreadable template is a gap in binpack's controller allowlist
	// and widens on evidence; a template the running pod disagrees with is a
	// fact about *this* cluster that no widening reaches. Reported as one, the
	// report contradicts itself — it names a ReplicaSet and then says binpack
	// cannot read ReplicaSets — and it asks the operator to file a bug about a
	// controller when the answer is in their own cluster.
	//
	// The values are the diagnosis codes, so the metric arm, the diagnosis and
	// the reference documentation are one vocabulary rather than three.
	Unmodelled string
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
// It is a first-fit-decreasing packing: relocatable pods are tried hardest to
// place first, onto the fullest node that still accepts them. That is a
// heuristic, so it can fail to find a packing that exists — but it never claims
// one that does not, which is the direction that matters. A missed
// consolidation costs nothing; a wrong one costs a scale-up.
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
			next, diverged, ok := replacement(pod, templates)
			if !ok {
				sim.Blocked = unpredictable(pod, diverged)
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
		// A node the autoscaler has committed to deleting is not capacity: it
		// is going away, and a replacement sent there is evicted again minutes
		// later with the candidate now cordoned behind it — which is the
		// scale-up binpack exists to prevent, arrived at through a drain that
		// reported success.
		//
		// Asked as a predicate rather than left to the taint, because the
		// taint only repels a pod that does not tolerate it, and the cordon
		// that would catch the rest need not be there:
		// --cordon-node-before-terminating defaulted to false through
		// cluster-autoscaler 1.33 and operators set it either way. Soundness
		// resting on how narrow a workload's tolerations happen to be is not
		// soundness. The cost is a consolidation missed on a node that is
		// about to disappear anyway, which ADR-0006 accepts.
		if BeingRemoved(node) {
			continue
		}
		destinations = append(destinations, node)

		// Observed rather than effective, and it is the one loop in this
		// function where the difference decides anything. These pods are
		// occupancies: what the node has already handed out, which under an
		// in-place vertical scale is not what their specs ask for. Everything
		// else placed into this map is a pod that does not exist yet, and a
		// pod that does not exist has actuated nothing.
		free := fit.Allocatable(node)
		for _, pod := range byNode[node.Name] {
			if !Occupies(pod) {
				continue
			}
			fit.Subtract(free, fit.ObservedRequests(pod))
			residents[node.Name] = append(residents[node.Name], pod)
		}
		remaining[node.Name] = free
	}

	// Hardest first, and the order is total.
	//
	// A pod with few homes placed late is a pod with none, so the packing
	// tries the awkward ones while there is still room to be awkward in. That
	// is also the order the executor evicts in — sim.Relocated is this slice —
	// which is why the key has to mean difficulty rather than size: a drain
	// abandoned halfway has wasted every eviction before the one that failed.
	//
	// The tie-break is not decoration. Without it the comparator is a partial
	// order and the sort hands ties back to the input slice, which the
	// controller fills from a watch-backed cache — Go map iteration order,
	// different between two calls in the same process — and `explain` fills
	// from a live client, ordered by storage key. Which of two equal pods goes
	// first is arbitrary; that both frontends make the same arbitrary choice
	// is the whole of what makes `explain` a preview of `run`.
	largest := largestFree(remaining)
	hardness := make(map[*corev1.Pod]float64, len(toPlace))
	for _, pod := range toPlace {
		hardness[pod] = difficultyOf(pod, largest)
	}
	slices.SortFunc(toPlace, func(a, b *corev1.Pod) int {
		return cmp.Or(
			cmp.Compare(hardness[b], hardness[a]),
			// A replacement carries the running pod's ObjectMeta, so this
			// names the object an operator can find.
			cmp.Compare(podRef(a), podRef(b)),
		)
	})

	// Expendable pods need no destination, so nothing ranks them — but they
	// are still evicted one at a time, in this order, and a list order is not
	// an order. Sorted for the same reason as the packing above.
	slices.SortFunc(sim.Evicted, func(a, b *corev1.Pod) int {
		return cmp.Compare(podRef(a), podRef(b))
	})

	for _, pod := range toPlace {
		placedOn, unsupported, perNode := place(pod, destinations, remaining, residents, domains)
		if placedOn == nil {
			was := running[pod]
			sim.Blocked = &Blocked{
				Pod:     was,
				PerNode: perNode,
				Summary: fmt.Sprintf("%s/%s has nowhere to go", was.Namespace, was.Name),
			}
			// A refusal about the pod rather than about anywhere it might
			// have gone: its own sentence is the whole answer, and there is
			// no per-node half to report. It names the running pod, because
			// a replacement carries the running pod's ObjectMeta.
			if !unsupported.Empty() {
				sim.Blocked.Summary = unsupported.Message
			}
			return sim
		}

		fit.Subtract(remaining[placedOn.Name], fit.EffectiveRequests(pod))
		residents[placedOn.Name] = append(residents[placedOn.Name], pod)
		sim.Relocated = append(sim.Relocated, Placement{Pod: running[pod], Node: placedOn})
	}

	if cfg.ReserveForLargestPod {
		if blocked := checkHeadroom(pods, templates, destinations, remaining, cfg); blocked != nil {
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
// The second return value is a refusal binpack made about the pod itself; the
// third is one refusal per destination. Never both, because they are answers
// to different questions.
func place(
	pod *corev1.Pod,
	destinations []*corev1.Node,
	remaining map[string]corev1.ResourceList,
	residents map[string][]*corev1.Pod,
	domains fit.AntiAffinityDomains,
) (*corev1.Node, fit.Reason, map[string]string) {
	// Asked once, above the loop, because it is a verdict about the pod alone:
	// fit reaches it before reading a single node property. Left inside, it
	// wrote the identical sentence under every destination's name — asserting
	// N node-specific walls that were never observed, under a verdict and a
	// summary the how-to guide primes an operator to read as a capacity
	// answer. The cluster may have room to spare; binpack declined to model
	// the pod.
	if r := fit.UnsupportedPod(pod); !r.Empty() {
		return nil, r, nil
	}

	ordered := make([]*corev1.Node, len(destinations))
	copy(ordered, destinations)
	// Fullest first, then by name so the order is total. Two interchangeable
	// nodes in one pool tie here constantly, and a tie settled by list order
	// is a placement that differs between the controller and `explain`, and
	// between one evaluation and the next.
	slices.SortFunc(ordered, func(a, b *corev1.Node) int {
		return cmp.Or(
			cmp.Compare(freeMemory(remaining[a.Name]), freeMemory(remaining[b.Name])),
			cmp.Compare(a.Name, b.Name),
		)
	})

	refusals := make(map[string]string, len(ordered))
	for _, node := range ordered {
		ok, reason := fit.CanFit(pod, node, remaining[node.Name], residents[node.Name], domains)
		if ok {
			return node, fit.Reason{}, nil
		}
		refusals[node.Name] = reason.Message
	}
	return nil, fit.Reason{}, refusals
}

// checkHeadroom enforces ReserveForLargestPod: after the drain, some node must
// still be able to take a pod of every shape the cluster's relocatable
// workloads would be recreated at.
//
// This exists instead of a percentage of headroom. A percentage is blind to
// absolute capacity — the objection binpack raises against the descheduler —
// and the risk being guarded against is not "the cluster is full" but "the
// next pod that restarts cannot be placed", which is a question about bytes.
//
// "The largest pod" is the shape that question wants and it does not exist.
// Across more than one resource there is no maximum, only maximal elements: a
// 7-core pod requesting 2Gi and a 500m pod requesting 24Gi are both largest,
// each in its own dimension, and neither one's placement says anything about
// the other's. Keyed on memory alone this reserved room for the second and
// approved a drain leaving nowhere for the first — a cluster bound by CPU, by
// a GPU or by the pod cap got a margin measured in the dimension that was not
// running out, while the operator was told they had the guard. So the check
// asks about every shape on the Pareto frontier of relocatable replacements,
// and one has to be found room for all of them.
func checkHeadroom(
	pods []*corev1.Pod,
	templates map[OwnerRef]*corev1.PodTemplateSpec,
	destinations []*corev1.Node,
	remaining map[string]corev1.ResourceList,
	cfg SimConfig,
) *Blocked {
	shapes, blocked := relocatableShapes(pods, templates, cfg)
	if blocked != nil {
		return blocked
	}

	for _, shape := range maximalShapes(shapes) {
		// Asked about a pod of that *shape*, not about that pod. The reserve
		// is a margin — "leave room for something as big as your biggest
		// workload" — and was never a claim that one specific pod must be
		// placeable.
		//
		// The difference is not academic. A StatefulSet's claim makes its pod
		// unmodellable to fit, so asking about the pod itself refuses every
		// destination on every cluster with a PVC-backed StatefulSet, turning
		// the default reserve into "never drain anything" — and reporting it
		// as "no room", which is not what happened.
		//
		// Size still comes from the real replacement, so nothing is
		// under-estimated: only the constraints that make a specific pod
		// unplaceable are dropped. Built here rather than in the scan because
		// a probe costs an allocation per container, and the frontier has
		// already discarded all but a handful of the shapes that reached it.
		probe := sizeProbe(shape.replacement)

		// What was observed, rather than what it would mean. Every destination
		// refusing is not the same as the cluster being full: three of them may
		// be cordoned.
		refusals := make(map[string]string, len(destinations))
		accepted := false
		for _, node := range destinations {
			// Neither an anti-affinity index nor the destination's residents,
			// and that is the point of both arguments being nil rather than
			// merely convenient. Both carry the same thing — some *other*
			// pod's required anti-affinity, evaluated against this one's
			// identity — and identity is what a probe deliberately has none
			// of. Handed either, one zone-scoped term matching the biggest
			// workload in the cluster vetoes the margin on every node in the
			// zone, and one per-node singleton with hard self-anti-affinity
			// vetoes it on every node it runs on. Both report it as "no room",
			// which is the failure the probe exists to avoid, and both are
			// questions the reserve is not asking.
			ok, reason := fit.CanFit(probe, node, remaining[node.Name], nil, nil)
			if ok {
				accepted = true
				break
			}
			refusals[node.Name] = headroomRefusal(reason, shape.running)
		}
		if !accepted {
			return &Blocked{
				Pod:     shape.running,
				PerNode: refusals,
				Summary: fmt.Sprintf(
					"no destination would accept a pod the size of %s/%s, which nothing "+
						"relocatable in the cluster is larger than in every resource",
					shape.running.Namespace, shape.running.Name),
			}
		}
	}
	return nil
}

// headroomShape is one relocatable replacement's resource shape, together with
// the pod it was read from so that a refusal names an object an operator can go
// and look at.
type headroomShape struct {
	// running is the pod in the cluster, not the replacement: a replacement
	// carries the running pod's ObjectMeta but exists nowhere.
	running *corev1.Pod
	// replacement is the pod its controller would create, which [sizeProbe]
	// reduces to a probe once the frontier has decided this shape is worth
	// asking about.
	replacement *corev1.Pod
	// requests is what a probe of it would ask for, read once because the
	// frontier compares every shape against every other. Reading it from the
	// replacement rather than from the probe is the same number: sizeProbe
	// keeps exactly the fields resource.PodRequests looks at, which is what
	// TestTheProbeCarriesEverySourceOfSize is there to hold it to — and
	// fit.EffectiveRequests reads none of the Status that sizeProbe drops,
	// which is what keeps the equality true for a replacement built from a
	// pod mid-resize. Both halves are load-bearing.
	requests corev1.ResourceList
}

// relocatableShapes collects one entry per distinct replacement shape among the
// relocatable pods running in the cluster.
//
// Sized as replacements, like every other placement question: this asks whether
// a pod of that shape could be *created* somewhere, and a pod resized downward
// in place would otherwise understate the margin by exactly the amount that
// matters.
//
// A pod whose template cannot be read blocks the proof rather than being
// skipped. Its shape may be one nothing has room for, and the reserve is a
// lower bound on space that must remain: under-estimating it approves a drain
// that should have been refused, which is a wrong answer and not a missed one.
// Skipping had that backwards.
//
// Unbound pods are skipped, which every sibling loop already does —
// indexPodsByNode has the guard, and diagnose writes the reason out. A pod the
// scheduler has never placed consumes nothing on any node and is not a pod that
// will restart, so admitting one to the reserve let a single replica with a
// memory typo in its template refuse every drain in the cluster for ever, and
// report it as "no room". The documented promise says *running* in the cluster;
// this is the code agreeing with it.
func relocatableShapes(
	pods []*corev1.Pod,
	templates map[OwnerRef]*corev1.PodTemplateSpec,
	cfg SimConfig,
) ([]headroomShape, *Blocked) {
	shapes := make([]headroomShape, 0, len(pods))
	for _, pod := range pods {
		if pod.Spec.NodeName == "" {
			continue
		}
		if !Occupies(pod) || Classify(pod, cfg.ExpendablePriorityCutoff) != Relocatable {
			continue
		}
		// Sized, not validated for relocation: this pod is not being moved, and
		// its placement constraints are discarded by sizeProbe regardless. An
		// unreadable template still blocks, because without one there is no
		// size to reserve for.
		next, ok := sizedReplacement(pod, templates)
		if !ok {
			return nil, &Blocked{
				Pod: pod,
				Summary: fmt.Sprintf(
					"%s/%s has no readable controller template, so binpack cannot tell how much "+
						"room to reserve for a pod of its size", pod.Namespace, pod.Name),
				Unmodelled: FindingNoTemplate,
			}
		}
		shapes = append(shapes, headroomShape{
			running: pod, replacement: next, requests: fit.EffectiveRequests(next),
		})
	}
	// Total, like every other ordering in the engine, and sorted before the
	// collapse below rather than after it so that which member of a group of
	// equal shapes survives is a fact about their names. A tie settled by list
	// order would name one pod through the controller, whose snapshot comes
	// from a watch-backed cache in Go map order, and another through `explain`,
	// whose comes from a live client in storage-key order.
	slices.SortFunc(shapes, func(a, b headroomShape) int {
		return cmp.Compare(podRef(a.running), podRef(b.running))
	})

	// Two workloads of the same shape are one question, and on a real cluster
	// most of this list is duplicates — a Deployment's replicas, and the several
	// Deployments in a namespace that were copied from one another. Collapsing
	// them is the whole of what keeps [maximalShapes] from being quadratic in
	// *pods*, and it runs once per candidate node.
	//
	// Keyed on the shape and not on the owner reference, which is the cheaper
	// key and the wrong one twice over. It collapses nothing on a cluster of
	// single-replica Deployments — a thousand of those are a thousand owners and
	// perhaps a dozen shapes — and within one owner it would have to *assume*
	// the replicas match, which [sizedReplacement] makes untrue for any replica
	// an in-place resize or an admission webhook has touched.
	//
	// The whole of it is a performance device. [shapeKey] only buckets and
	// [sameShape] decides, so a bad key costs comparisons; and a duplicate that
	// survives the collapse entirely is pruned by [dominates] a moment later
	// anyway, which is where the correctness lives.
	//
	// First occurrence wins, and the sort above is what makes that the lowest
	// name rather than whichever the caller happened to list first.
	unique := make([]headroomShape, 0, len(shapes))
	buckets := make(map[string][]int, len(shapes))
	for _, shape := range shapes {
		key := shapeKey(shape.requests)
		duplicate := false
		for _, at := range buckets[key] {
			if sameShape(unique[at], shape) {
				duplicate = true
				break
			}
		}
		if duplicate {
			continue
		}
		buckets[key] = append(buckets[key], len(unique))
		unique = append(unique, shape)
	}
	return unique, nil
}

// shapeKey buckets shapes that may be equal. It decides nothing, and that is
// worth stating plainly rather than leaving to be discovered: [sameShape]
// settles every bucket, so a key that put every shape in one bucket would still
// give the right answer, slowly. Measured, not assumed — the suite is green
// with this returning a constant.
//
// So what the key is for is short buckets, and every resource name is in it for
// that reason: two workloads alike but for a GPU belong in different buckets,
// or the scan does the comparison the key exists to avoid. And MilliValue
// saturates rather than wrapping past about nine petabytes, which is a
// collision and therefore a comparison, not an answer.
func shapeKey(requests corev1.ResourceList) string {
	names := make([]string, 0, len(requests))
	for name := range requests {
		names = append(names, string(name))
	}
	slices.Sort(names)

	// Appended into one buffer, and the quantity as a scaled integer rather
	// than through fmt: this is per relocatable pod on every candidate node, so
	// a reflection-based format here is the difference between the collapse
	// paying for itself and not.
	key := make([]byte, 0, 64)
	for _, name := range names {
		quantity := requests[corev1.ResourceName(name)]
		key = append(key, name...)
		key = append(key, '=')
		key = strconv.AppendInt(key, quantity.MilliValue(), 10)
		key = append(key, ';')
	}
	return string(key)
}

// maximalShapes reduces the cluster's relocatable shapes to the ones worth
// probing: those no other shape is at least as large as in every resource.
//
// Proving room for a shape proves room for everything it dominates, so the
// frontier is the whole of what the reserve has to ask, and it is a handful of
// shapes on a cluster with a handful of workload sizes: anything smaller than
// something else in every dimension collapses into it. That matters, because
// each survivor costs one sweep of the destinations and [assess] simulates every
// candidate node rather than stopping at the first that works.
//
// Quadratic, and in the number of *distinct shapes* — which is the whole reason
// [relocatableShapes] collapses duplicates on the shape rather than on the owner
// reference. Keyed on the owner it would be quadratic in controllers instead,
// and a cluster of single-replica Deployments has as many controllers as pods.
// Left quadratic on shapes because that input is small and does not grow with
// the cluster: a sort by any one resource does not order a frontier, so the
// alternative is a divide and conquer whose crossover point is far above any
// number of distinct shapes this will be handed.
func maximalShapes(shapes []headroomShape) []headroomShape {
	out := make([]headroomShape, 0, len(shapes))
	for i, shape := range shapes {
		dominated := false
		for j, other := range shapes {
			if i != j && dominates(other, shape) {
				dominated = true
				break
			}
		}
		if !dominated {
			out = append(out, shape)
		}
	}
	return out
}

// dominates reports whether room for a is room for b as well.
//
// True when a asks for at least as much of everything b asks for. The claim it
// licenses is about what fit will answer, so it holds only because a probe is
// built from a fixed field list and carries nothing else that fit reads: of
// those, tolerations are the one field that is not a quantity, and they have to
// match exactly. Tolerations are permissions rather than demands, so a bigger
// pod tolerating less is not a harder pod to place — it is a different question
// — and comparing the two would be comparing a maximum with a minimum. Grouping
// by equality costs a missed collapse where one workload's tolerations are a
// superset of another's, which is a probe binpack does not skip rather than an
// answer it gets wrong.
//
// The pod slot takes part and can never decide anything, which is the whole
// difference between a frontier and the maximum [difficultyOf] deliberately
// hides it from. fit.EffectiveRequests synthesises `pods: 1` for every pod, so
// under a maximum that share grows to outweigh every real demand as a node
// fills and censors the dimensions that can rank. Under domination an axis
// every shape ties on is inert: it never blocks a domination, because equal
// satisfies "at least as much", and it never establishes one, because equal is
// not strictly more. Excluding it would change no answer and would invite the
// reader to think the reserve does not hold a slot — it does, and it must:
// fit.CanFit checks `pods` against what is left like any other resource, so a
// destination at its kubelet cap refuses the probe however much memory it has.
//
// Ties are broken by name, and that clause is a dedupe rather than a decision:
// equal shapes give equal answers, and [relocatableShapes] has already put them
// in name order, so which one a refusal names is settled with or without it.
// What it settles is how many sweeps run. Without it neither of two equal
// shapes is strictly larger, so neither dominates, so both survive — and a
// Deployment's replicas are equal shapes, which is most of the cluster.
func dominates(a, b headroomShape) bool {
	if !sameTolerations(a, b) {
		return false
	}

	strictly := false
	for name, want := range b.requests {
		// A zero request is not a demand, and fit skips it for the same
		// reason: a node that does not advertise the resource still satisfies
		// asking for none of it.
		if want.IsZero() {
			continue
		}
		have, present := a.requests[name]
		if !present || have.Cmp(want) < 0 {
			return false
		}
		if have.Cmp(want) > 0 {
			strictly = true
		}
	}
	// A resource a needs and b does not makes a strictly larger, which is what
	// keeps a GPU shape on the frontier beside a memory-heavier one.
	for name, have := range a.requests {
		if _, present := b.requests[name]; !present && !have.IsZero() {
			strictly = true
		}
	}
	if strictly {
		return true
	}
	return podRef(a.running) < podRef(b.running)
}

// sameShape reports whether two replacements would produce the same probe, as
// far as anything fit reads is concerned.
//
// The exact half of the collapse in [relocatableShapes], asked only of shapes
// that already share a bucket. Quantities go through Cmp rather than through a
// value or a string, so two workloads asking for 1Gi and for 1024Mi are the one
// shape that they are.
func sameShape(a, b headroomShape) bool {
	if len(a.requests) != len(b.requests) || !sameTolerations(a, b) {
		return false
	}
	for name, want := range a.requests {
		have, present := b.requests[name]
		if !present || have.Cmp(want) != 0 {
			return false
		}
	}
	return true
}

// sameTolerations compares the one field of a probe that is not a quantity.
//
// Order-sensitive, because a semantic comparison of two lists as sets is more
// machinery than the answer is worth: two workloads listing the same
// tolerations in a different order are a collapse binpack does not make, which
// costs a probe rather than an answer.
func sameTolerations(a, b headroomShape) bool {
	return equality.Semantic.DeepEqual(
		a.replacement.Spec.Tolerations, b.replacement.Spec.Tolerations)
}

// headroomRefusal says why one destination would not take the probe.
//
// fit's own sentence, kept rather than replaced by a capacity one. The reserve
// probes every destination, and a node that refused because it is cordoned,
// not Ready, or carrying a taint the probe does not tolerate refused for
// something an operator can act on — reported as "no room" it becomes a
// capacity claim about a node with tens of gigabytes free, and none of the
// responses that invites (raise the pool maximum, add a node, turn the reserve
// off) touches the thing that actually refused.
//
// The shortfall is the one message that is not enough by itself. fit says
// which resource ran out; the reserve has to add how much of it was being
// asked for, because that is the whole of what this check is about — and it
// names the pod, because there is more than one shape being asked about and
// "the largest" would not say which.
func headroomRefusal(r fit.Reason, running *corev1.Pod) string {
	if r.Code == fit.ReasonInsufficient {
		return r.Message + " for a pod the size of " + podRef(running)
	}
	return r.Message
}

// RelocationSummary says what draining a node would move, in one clause.
//
// One sentence with two callers rather than two renderings of one number. The
// Event on the node and `binpack explain` describe the same decision from the
// same field, and an operator checking one against the other must not find
// them disagreeing about the arithmetic — which they did, by the Event stating
// the count and explain omitting it.
func RelocationSummary(pods int) string {
	switch pods {
	case 0:
		return "it runs nothing that would need to move"
	case 1:
		return "its 1 relocatable pod fits elsewhere"
	default:
		return fmt.Sprintf("all %d of its relocatable pods fit elsewhere", pods)
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

// memoryOf sizes a pod in the one dimension that is almost always the binding
// constraint. It is what [workloadOn] totals to rank nodes, and its only
// caller now.
//
// Neither of the two questions about *pods* uses it any more, and for the same
// reason: "largest" has no meaning across dimensions, so choosing one
// privileges whichever cluster happens to be bound by it. The packing ranks by
// [difficultyOf] and the reserve asks about a frontier rather than a maximum.
// Ordering nodes is the case that survives, because a node is being compared
// against other nodes of much the same shape and nothing is claimed about the
// answer beyond which one to try first.
func memoryOf(pod *corev1.Pod) int64 {
	// Observed, because its one caller totals pods that are running: how much
	// work a node is doing is a question about what it has handed out, not
	// about what its residents have most recently asked for.
	requests := fit.ObservedRequests(pod)
	mem := requests[corev1.ResourceMemory]
	return mem.Value()
}

func freeMemory(remaining corev1.ResourceList) int64 {
	mem := remaining[corev1.ResourceMemory]
	return mem.Value()
}

// difficultyOf ranks a pod by how hard it will be to place: the largest share
// of any one resource it needs of the largest hole that exists in that
// resource anywhere among the destinations.
//
// First-fit-decreasing needs one number per pod, and memory was it. But a
// 7-core pod requesting 100Mi then sorted below every 2Gi web pod on the node,
// so on a CPU-, GPU- or pod-slot-bound cluster the hardest placement was tried
// last — in the packing, where it is the one likeliest to find nothing left,
// and in the eviction order read off the result, where its failure is the one
// that wastes every eviction before it.
//
// Normalising against the largest hole is what makes the number comparable
// across resources: it answers "how much of the best chance this pod has does
// it need", so a request no single destination can hold scores above one, and
// an extended resource nothing advertises scores infinite. Correctness still
// does not depend on the choice — fit.CanFit checks every resource regardless,
// so a poor ordering costs a missed packing, never an invalid one.
//
// The pod slot is left out, and it is the one exclusion here. `pods` is not a
// demand a workload makes: fit.EffectiveRequests synthesises exactly one for
// every pod, so the subtraction loop consumes a slot at all. Being identical
// for every candidate it can never rank two of them — and on a node near its
// cap its share approaches one and outgrows every real demand, at which point a
// maximum over it hides every dimension that can rank. The result was a
// packing that fell through to names on precisely the clusters where the order
// matters most: with one slot left on each of a wide and a narrow destination,
// an alphabetically earlier small pod took the wide node and stranded the
// CPU-heavy one that had nowhere else to go. The cap itself is untouched —
// fit.CanFit still checks `pods` like any other resource, so what changed is
// the order pods are offered in and never whether one fits.
//
// Only this one. A workload genuinely requesting the same amount of something
// on every pod — one GPU each, say — is a fact about the cluster, and pods that
// really are equally constrained by it should rank equally.
//
// A float, because this ranks and nobody acts on the number. The name
// tie-break at the call site is what makes the order total, so a rounding
// coincidence costs a swap rather than a stable answer. Dividing by a hole of
// zero is deliberate and needs no guard: a resource no destination advertises
// yields +Inf, which sorts the pod first, which is exactly where a pod nothing
// can hold belongs.
func difficultyOf(pod *corev1.Pod, largest map[corev1.ResourceName]float64) float64 {
	worst := 0.0
	for name, request := range fit.EffectiveRequests(pod) {
		if name == corev1.ResourcePods {
			continue
		}
		want := request.AsApproximateFloat64()
		if want <= 0 {
			continue
		}
		if share := want / largest[name]; share > worst {
			worst = share
		}
	}
	return worst
}

// largestFree is the biggest hole in each resource anywhere among the
// destinations — the denominator [difficultyOf] normalises against.
//
// The maximum rather than the sum, and for the same reason feasibility is a
// simulation rather than a subtraction: three nodes with a core free each do
// not hold a three-core pod. A denominator that added them up would rank that
// pod easy at precisely the moment it is impossible.
func largestFree(remaining map[string]corev1.ResourceList) map[corev1.ResourceName]float64 {
	largest := make(map[corev1.ResourceName]float64)
	for _, free := range remaining {
		for name, quantity := range free {
			if room := quantity.AsApproximateFloat64(); room > largest[name] {
				largest[name] = room
			}
		}
	}
	return largest
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

// TemplateKind is a controller kind binpack can read a pod template from.
//
// The identity only. How each kind is listed, which field the template lives
// in, and what an RBAC rule has to grant to reach it all belong to the package
// that does the reading — but *which* owners binpack understands is a fact
// about what it can predict, and so a fact about the engine. The diagnosis
// that tells an operator their pod cannot be moved is built from this list, in
// this package, which is what keeps the sentence and the set from parting
// company.
type TemplateKind struct {
	// APIVersion and Kind are exactly what an ownerReference carries, so a
	// kind here and the [OwnerRef] a pod produces are comparable without
	// translation.
	APIVersion string
	Kind       string
}

// TemplateKinds is every controller kind binpack can read a pod template from.
//
// Enumerable for the same reason [SkipCodes] is: it is a closed set that
// several things elsewhere have to agree with, and a set nothing can range
// over is one they agree with only by inspection. The four were chosen against
// measurement rather than guessed — see ADR-0006 — and widening the list is a
// change `binpack diagnose` explicitly invites operators to ask for.
func TemplateKinds() []TemplateKind {
	return slices.Clone(templateKinds)
}

var templateKinds = []TemplateKind{
	{APIVersion: "apps/v1", Kind: "ReplicaSet"},
	{APIVersion: "apps/v1", Kind: "StatefulSet"},
	// A DaemonSet pod is node-local and never relocated, so no simulation ever
	// asks for its template — [NodeBound] recognises the kind by name, before
	// a template is looked up. It is here because a DaemonSet is one of the
	// kinds a pod can name as its controller, and an absent entry is
	// indistinguishable from a kind binpack genuinely cannot read, which is
	// the distinction the whole refusal path turns on.
	{APIVersion: "apps/v1", Kind: "DaemonSet"},
	{APIVersion: "batch/v1", Kind: "Job"},
}

// templateKindsProse names the readable kinds in a sentence: "ReplicaSets,
// StatefulSets, DaemonSets and Jobs".
//
// Built rather than written out, because the sentence it goes into is the only
// place an operator learns which owners binpack understands, and a hand-copied
// list is one that goes stale silently — the reader has no way to tell.
func templateKindsProse() string {
	kinds := TemplateKinds()
	names := make([]string, 0, len(kinds))
	for _, k := range kinds {
		names = append(names, k.Kind+"s")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
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
//
// Three outcomes, not two. ok reports whether the pod can be moved; when it
// cannot, diverged names the field admission added, or is empty when there was
// no template to read in the first place. Both helpers below already compute
// that name, and discarding it was what let one refusal be reported as the
// other — see [Blocked.Unmodelled].
func replacement(
	pod *corev1.Pod, templates map[OwnerRef]*corev1.PodTemplateSpec,
) (out *corev1.Pod, diverged string, ok bool) {
	out, ok = sizedReplacement(pod, templates)
	if !ok {
		return nil, "", false
	}
	ref, _ := ControllerOf(pod)
	template := templates[ref]
	if field := restrictiveDivergence(template, pod); field != "" {
		return nil, field, false
	}
	if volume := mutatedVolume(template, pod); volume != "" {
		// Named as the field it is, since "volumes" alone would not tell an
		// operator which of a meshed pod's half-dozen to go and look at.
		return nil, "volume " + volume, false
	}
	return out, "", true
}

// unpredictable is the refusal for a pod whose replacement binpack cannot
// predict, in whichever of the two vocabularies fits what was observed.
//
// Observed, and not diagnosed: a divergence is reported as a divergence and
// not as admission's work, because binpack cannot establish that and admission
// is not the only cause. See [FindingTemplateDivergence]'s catalogue entry.
//
// One function because the two summaries are the same sentence about different
// facts, and because keeping them apart is what previously drifted: the
// divergence case borrowed the other's wording and told operators to report a
// controller that binpack reads perfectly well.
func unpredictable(pod *corev1.Pod, diverged string) *Blocked {
	if diverged == "" {
		return &Blocked{
			Pod: pod,
			Summary: fmt.Sprintf(
				"%s/%s has no readable controller template, so binpack cannot tell what "+
					"its replacement would request", pod.Namespace, pod.Name),
			Unmodelled: FindingNoTemplate,
		}
	}
	return &Blocked{
		Pod: pod,
		Summary: fmt.Sprintf(
			"%s/%s carries a %s its controller template does not, so binpack cannot tell "+
				"where its replacement would be allowed to go",
			pod.Namespace, pod.Name, diverged),
		Unmodelled: FindingTemplateDivergence,
	}
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
		//
		// For detection, and never for arithmetic. This Status belongs to the
		// pod being replaced: it records what some kubelet actuated for an
		// object that is going away. Size a replacement through
		// fit.ObservedRequests and a pod mid-downward-resize takes over the
		// frontier at its old, larger figure — which [sizeProbe] then strips
		// back off, so the reserve enforced is the small one and the two
		// halves of one check approve a drain by disagreeing. Everything built
		// here is sized through fit.EffectiveRequests.
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

// sizeProbe is a pod carrying the resource shape of another, and the
// permissions it would be placed with, and nothing else.
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
//
// Tolerations are kept, and they are the exception the rule needed. A
// toleration is a *permission*, not a constraint: dropping one cannot make the
// probe fit where the real replacement would not, only make it refusable by
// taints the replacement tolerates. So the probe was strictly harder to place
// than the pod it stood for wherever taints exist at all, and a cluster whose
// spare capacity sits in a tainted pool — a batch pool, a GPU pool, an ARM
// pool, a wholly tainted single-pool cluster — failed the reserve for ever and
// was told "no room" about nodes that were empty.
//
// ObjectMeta is rebuilt too, and for the same reason as the spec: labels are
// not size. Copied wholesale, they let a *resident's* required anti-affinity
// select the probe as the workload it was taken from, so a workload running
// one hard-anti-affine replica per node — the ordinary way to get per-node
// placement without a DaemonSet — refused the margin on every node it ran on.
// Namespace and name stay because a refusal has to name something an operator
// can look at.
//
// That failure is closed twice over, at two different levels, and deliberately:
// [checkHeadroom] hands fit no residents and no domain index, so it does not
// ask the identity question, and this drops the labels, so the probe carries no
// answer to it. Either alone is enough today, which means neither is
// individually observable and a mutation sweep will report both as surviving —
// equivalent mutants rather than gaps. They are kept because they fail
// differently under change: the call site is what a reader weighing "should the
// reserve see residents?" will find and reason about, and this is what stops a
// future check that reads labels from quietly reopening the same door. This is
// the fourth route to "one workload blocks every drain" that the probe was
// built to close; the pattern has earned two locks.
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
		ObjectMeta: metav1.ObjectMeta{Namespace: pod.Namespace, Name: pod.Name},
		Spec: corev1.PodSpec{
			Containers: shape(pod.Spec.Containers, false),
			// A native sidecar is an init container with an Always restart
			// policy, and it is added to the running total rather than folded
			// into the init peak. Dropping the policy would understate it.
			InitContainers: shape(pod.Spec.InitContainers, true),
			Overhead:       pod.Spec.Overhead.DeepCopy(),
			Resources:      pod.Spec.Resources.DeepCopy(),
			Tolerations:    slices.Clone(pod.Spec.Tolerations),
		},
	}
}
