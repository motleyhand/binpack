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
// whatever the caller has decided stays or has already placed. Use
// [Allocatable] to start it and [EffectiveRequests] to subtract from it, so
// that the arithmetic here and the caller's agree by construction.
//
// residents are the pods already on node. They matter because inter-pod
// anti-affinity is symmetric: a pod that is already there can reject an
// incoming one.
func CanFit(pod *corev1.Pod, node *corev1.Node, remaining corev1.ResourceList, residents []*corev1.Pod) (bool, Reason) {
	if r := UnsupportedPod(pod); !r.Empty() {
		return false, r
	}
	if r := UnsupportedDestination(node, residents); !r.Empty() {
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
	if taint, untolerated := schedcorev1.FindMatchingUntoleratedTaint(
		logr.Discard(), node.Spec.Taints, pod.Spec.Tolerations, schedulingTaint, true,
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

// EffectiveRequests returns what the scheduler would reserve for pod.
//
// This is not a copy of resources.requests. Kubernetes reserves the larger of
// the regular-container sum and the init-container peak, keeps native sidecars
// in the running total, and adds RuntimeClass overhead — arithmetic that is
// fiddly enough to be worth delegating to the same library the scheduler uses.
//
// It also carries a synthetic pods:1 entry. `pods` appears in a node's
// allocatable but never in a pod's requests, so without it a uniform
// subtract-request-from-remaining loop would never consume a slot and the
// simulation would pack unlimited pods onto a node capped at 110.
func EffectiveRequests(pod *corev1.Pod) corev1.ResourceList {
	requests := resourcehelper.PodRequests(pod, resourcehelper.PodResourcesOptions{})

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
