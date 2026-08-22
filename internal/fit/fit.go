// Package fit answers one question: could this pod be scheduled onto that
// node?
//
// It is the highest-risk package in binpack. Everything downstream trusts its
// answer, and a wrong "yes" causes exactly the outcome the project exists to
// prevent — a drain that leaves a pod Pending and provokes a scale-up.
//
// Two rules follow from that, both from ADR-0006:
//
// Soundness runs one way. If fit says a pod fits, the real scheduler must
// agree. The converse is explicitly allowed: refusing a placement the
// scheduler would have accepted costs a missed consolidation, which the next
// run may find. Every uncertainty resolves towards "no".
//
// What binpack understands is an allowlist. Constraints outside it are
// detected and refused, never ignored. A list of exceptions we remembered
// could not stay complete as Kubernetes grows; refusing by default can.
//
// Go has no default branch over a struct's fields, so the second rule is held
// from outside the code that implements it: TestPodSpecFieldsAreAccountedFor
// reflects over corev1.PodSpec and requires every field to be named as one
// this package accounts for or as one no scheduler Filter plugin reads. A
// field the next release adds fails CI naming itself.
//
// This package holds no clients and performs no I/O. It takes API objects as
// data.
package fit

import (
	"github.com/go-logr/logr"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	resourcehelper "k8s.io/component-helpers/resource"
	schedcorev1 "k8s.io/component-helpers/scheduling/corev1"
	"k8s.io/component-helpers/scheduling/corev1/nodeaffinity"
)

// Reason explains a refusal. The zero value means no objection.
type Reason struct {
	// Code is stable and machine-readable, for metrics and tests.
	Code string
	// Message names the specific thing that caused the refusal, for humans
	// reading `binpack explain`.
	Message string
}

func (r Reason) Empty() bool { return r.Code == "" }
func (r Reason) String() string {
	if r.Empty() {
		return ""
	}
	return r.Message
}

// Refusal codes. Stable identifiers, since metrics and tests key on them.
const (
	ReasonUnschedulable    = "node-unschedulable"
	ReasonNodeNotReady     = "node-not-ready"
	ReasonUntoleratedTaint = "untolerated-taint"
	ReasonNodeAffinity     = "node-affinity"
	ReasonInsufficient     = "insufficient-resources"
	ReasonUnsupportedPod   = "unsupported-pod-feature"
	ReasonUnsupportedNode  = "unsupported-node-feature"
)

// CanFit reports whether pod could be placed on node, given the capacity still
// free there.
//
// remaining is the caller's simulation state: a node's allocatable minus
// whatever the caller has decided stays or has already placed. Start it with
// [Allocatable] and subtract with the entry point that matches where each pod
// came from — [ObservedRequests] for the residents the node was already
// holding, [EffectiveRequests] for anything the caller built and is placing —
// so that the arithmetic here and the caller's agree by construction.
//
// residents are the pods already on node. They matter because inter-pod
// anti-affinity is symmetric: a pod that is already there can reject an
// incoming one.
//
// domains carries that same symmetry the rest of the way, since a term keyed
// on anything wider than the hostname rejects nodes that host no matching pod
// at all. Build it with [NewAntiAffinityDomains] from the cluster the caller
// holds. An empty one is honest but blind — see [UnsupportedDestination].
func CanFit(
	pod *corev1.Pod,
	node *corev1.Node,
	remaining corev1.ResourceList,
	residents []*corev1.Pod,
	domains AntiAffinityDomains,
) (bool, Reason) {
	if r := UnsupportedPod(pod); !r.Empty() {
		return false, r
	}
	if r := UnsupportedDestination(pod, node, residents, domains); !r.Empty() {
		return false, r
	}

	if node.Spec.Unschedulable {
		return false, Reason{ReasonUnschedulable, "node " + node.Name + " is cordoned"}
	}
	if !isReady(node) {
		return false, Reason{ReasonNodeNotReady, "node " + node.Name + " is not Ready"}
	}

	// NoSchedule and NoExecute block placement; PreferNoSchedule only affects
	// scoring, which binpack never needs to model.
	// The logger parameter is an upstream signature detail. logr.Discard is a
	// no-op sink: this package must do no I/O, and klog itself is a logging
	// implementation that could, so it is deliberately not imported —
	// klog.Logger is only an alias for logr.Logger anyway.
	//
	// The final parameter is enableComparisonOperators, and false is what the
	// scheduler computes: it passes the TaintTolerationComparisonOperators gate,
	// which is alpha and off by default, so a Gt or Lt toleration does not match
	// there. Honouring one here would accept a destination the scheduler
	// refuses, and the pod would go Pending after binpack had already evicted it.
	// The error direction of false is the safe one — on a cluster that has
	// turned the gate on, binpack under-tolerates and misses a destination,
	// which costs a consolidation rather than a wrong answer.
	if taint, untolerated := schedcorev1.FindMatchingUntoleratedTaint(
		logr.Discard(), node.Spec.Taints, pod.Spec.Tolerations, schedulingTaint, false,
	); untolerated {
		return false, Reason{ReasonUntoleratedTaint,
			"node " + node.Name + " has untolerated taint " + taint.Key + "=" + taint.Value + ":" + string(taint.Effect)}
	}

	// Covers both nodeSelector and required node affinity. An error here means
	// an unparseable selector, which is a refusal like any other uncertainty.
	matches, err := nodeaffinity.GetRequiredNodeAffinity(pod).Match(node)
	if err != nil {
		return false, Reason{ReasonNodeAffinity,
			"node affinity on " + podRef(pod) + " could not be evaluated: " + err.Error()}
	}
	if !matches {
		return false, Reason{ReasonNodeAffinity,
			podRef(pod) + " requires node labels that " + node.Name + " does not have"}
	}

	// pod.Spec.NodeName is deliberately not compared against node.Name.
	//
	// Every pod binpack sees is already bound, so its NodeName always names
	// the node it is being relocated *from*. Refusing when they differ would
	// refuse every relocation there is.
	//
	// The gap this leaves: a controller whose pod *template* pins NodeName
	// recreates its pod on the same node regardless of what binpack decides,
	// and such a pod ignores cordon because setting NodeName bypasses the
	// scheduler entirely. Detecting that needs the owner's template, which
	// binpack does not read. The consequence is bounded — the pod reappears on
	// the node being drained, no pod goes Pending, so no scale-up follows, and
	// the drain stalls and backs off exactly as ADR-0007 provides for an
	// undetected blocker. It costs a wasted drain, not the failure this
	// project exists to prevent.

	if short, ok := firstShortfall(EffectiveRequests(pod), remaining); !ok {
		return false, Reason{ReasonInsufficient,
			"node " + node.Name + " has insufficient " + short}
	}

	return true, Reason{}
}

// schedulingTaint selects the taint effects that prevent placement.
func schedulingTaint(t *corev1.Taint) bool {
	return t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute
}

func isReady(node *corev1.Node) bool {
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// firstShortfall reports the first resource for which need exceeds have. It
// returns the resource name rather than a bool alone so the refusal can say
// which dimension ran out, which is the difference between a useful `explain`
// and a shrug.
func firstShortfall(need, have corev1.ResourceList) (string, bool) {
	for name, want := range need {
		if want.IsZero() {
			continue
		}
		got, present := have[name]
		if !present {
			// Extended resources, hugepages and device plugins land here: a
			// node that does not advertise the resource at all can never
			// satisfy a request for it, however much CPU and memory it has.
			return string(name), false
		}
		if got.Cmp(want) < 0 {
			return string(name), false
		}
	}
	return "", true
}

// EffectiveRequests returns what the scheduler would reserve for a pod being
// placed.
//
// This is not a copy of resources.requests. Kubernetes reserves the larger of
// the regular-container sum and the init-container peak, keeps native sidecars
// in the running total, and adds RuntimeClass overhead — arithmetic that is
// fiddly enough to be worth delegating to the same library the scheduler uses.
//
// Spec alone, and that is the whole of what separates it from
// [ObservedRequests]. The scheduler makes the same distinction and states its
// reason at the call site: computePodResourceRequest, which sizes the incoming
// pod for the NodeResourcesFit filter, passes no status option because "pod
// hasn't scheduled yet so we don't need to worry about
// InPlacePodVerticalScalingEnabled". A pod that is not on the node has
// actuated nothing there.
//
// Every caller here is asking about a placement: a replacement built from a
// controller's template, and the probe the reserve asks a destination about.
// A replacement does carry the running pod's Status, so
// that the status-based refusals still apply to it — but that Status describes
// the pod it replaces, and charging what a predecessor's kubelet happens to
// have actuated reserves room for a size nothing will ask for.
//
// It also carries a synthetic pods:1 entry. `pods` appears in a node's
// allocatable but never in a pod's requests, so without it a uniform
// subtract-request-from-remaining loop would never consume a slot and the
// simulation would pack unlimited pods onto a node capped at 110.
func EffectiveRequests(pod *corev1.Pod) corev1.ResourceList {
	return requestsWithPodSlot(pod, resourcehelper.PodResourcesOptions{})
}

// ObservedRequests returns what a node has already reserved for a pod running
// on it.
//
// [EffectiveRequests] plus one option, and the option is the difference
// between what a pod asks for and what it has been given. An in-place vertical
// scale moves the two apart: spec carries the new figure from the moment
// something patches it, and the kubelet actuates afterwards — but a memory
// decrease cannot be actuated below what the workload is currently using, so
// the kubelet keeps PodResizeInProgress set and the gap lasts as long as the
// usage does. Under UseStatusResources, k8s.io/component-helpers
// resource.PodRequests resolves it to max(spec, actuated, allocated), which is
// what the node is holding and therefore what is not free.
//
// The options are the scheduler's own, read off its call site rather than
// chosen. PodInfo.CalculateResource sets UseStatusResources from
// InPlacePodVerticalScaling, which has been GA and LockToDefault since 1.35 —
// there is no cluster where it is off, so `true` there is a fact about every
// scheduler binpack will meet rather than a guess about a gate.
//
// InPlacePodLevelResourcesVerticalScalingEnabled is the one that is not
// settled, and it is handled by asking upstream both ways. It comes from a
// Beta gate — default-on at 1.36, and so switchable off — and neither constant
// is sound alone. With it on, a pod-level request whose resize the kubelet has
// refused as infeasible resolves to max(actuated, allocated), dropping spec
// entirely; with it off, the scheduler charges that spec. So `true`
// under-charges such a resident against a scheduler with the gate disabled,
// and `false` under-charges every pod-level resize against the default one.
// Under-charging a resident is the unsound direction: it invents free space
// the node has already given away, and the packing spends it. binpack cannot
// read another component's feature gates, so it charges the larger of the two
// answers, which is at least what either scheduler reserves. The cost falls
// only on a pod-level request in the refused state, and it is a consolidation
// rather than a wrong answer — ADR-0006's rule for a version-dependent answer,
// applied to a configuration-dependent one.
//
// The second reading is computed only where it can differ. Outside
// IsPodLevelRequestsSet the option is never consulted, so both calls return the
// same map and an ordinary pod pays nothing for the corner.
//
// The split is by where the pod came from, not by what it looks like. Anything
// read from the cluster belongs here — the residents a destination starts out
// holding, and the workload total that orders candidates. Anything binpack
// built belongs in [EffectiveRequests].
func ObservedRequests(pod *corev1.Pod) corev1.ResourceList {
	opts := resourcehelper.PodResourcesOptions{
		UseStatusResources: true,
		InPlacePodLevelResourcesVerticalScalingEnabled: true,
	}
	out := requestsWithPodSlot(pod, opts)

	if resourcehelper.IsPodLevelRequestsSet(pod) {
		opts.InPlacePodLevelResourcesVerticalScalingEnabled = false
		raiseTo(out, requestsWithPodSlot(pod, opts))
	}

	return out
}

// raiseTo lifts have to other wherever other asks for more, in place. The
// counterpart of [Subtract] for a caller holding two readings of one pod that
// has to keep the conservative one.
func raiseTo(have, other corev1.ResourceList) {
	for name, want := range other {
		got, present := have[name]
		if !present || got.Cmp(want) < 0 {
			have[name] = want.DeepCopy()
		}
	}
}

// requestsWithPodSlot is the shared body of the two entry points above: the
// upstream computation under whichever options, plus the pod slot neither of
// them may be without.
func requestsWithPodSlot(pod *corev1.Pod, opts resourcehelper.PodResourcesOptions) corev1.ResourceList {
	requests := resourcehelper.PodRequests(pod, opts)

	out := make(corev1.ResourceList, len(requests)+1)
	for name, quantity := range requests {
		out[name] = quantity.DeepCopy()
	}
	out[corev1.ResourcePods] = *resource.NewQuantity(1, resource.DecimalSI)

	return out
}

// Allocatable returns what a node can hold, as a starting point for
// simulation. It is a copy: callers subtract from it, and the node belongs to
// a shared informer cache that must never be written to.
func Allocatable(node *corev1.Node) corev1.ResourceList {
	out := make(corev1.ResourceList, len(node.Status.Allocatable))
	for name, quantity := range node.Status.Allocatable {
		out[name] = quantity.DeepCopy()
	}
	return out
}

// Subtract removes need from have, in place, flooring at zero. Used by callers
// simulating placements so their arithmetic matches CanFit's.
func Subtract(have, need corev1.ResourceList) {
	for name, want := range need {
		got, present := have[name]
		if !present {
			continue
		}
		got.Sub(want)
		if got.Sign() < 0 {
			got.Set(0)
		}
		have[name] = got
	}
}

func podRef(pod *corev1.Pod) string { return pod.Namespace + "/" + pod.Name }
