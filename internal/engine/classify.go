// Package engine decides whether draining a node would genuinely reduce node
// count, and refuses when it would not.
//
// It takes Kubernetes API objects as data and holds no cluster clients, which
// is enforced by a depguard allowlist — see ADR-0008. Objects passed in come
// from a shared informer cache in production and must be treated as read-only.
package engine

import (
	corev1 "k8s.io/api/core/v1"
)

// PodClass says how a pod sitting on a drain candidate is treated.
//
// Getting this wrong in either direction is a correctness bug rather than an
// inefficiency, which is why it is named rather than left implicit in a
// filter expression.
type PodClass int

const (
	// Relocatable pods must be shown to fit somewhere else before the node
	// can be drained, and must then be evicted.
	Relocatable PodClass = iota

	// NodeLocal pods are neither simulated nor evicted. They do not move to
	// another node when this one goes away — they cease to exist with it, and
	// an equivalent already runs elsewhere.
	//
	// Both halves matter. Counting them as workload needing a destination
	// inflates the requirement on every node, since every ordinary node runs
	// several. Evicting them is worse than pointless: the controller
	// immediately recreates the pod on the same node, which still exists, so
	// the drain never completes. This is why `kubectl drain` requires
	// --ignore-daemonsets.
	NodeLocal

	// Expendable pods are evicted but need not fit anywhere. The
	// cluster-autoscaler ignores pods below its expendable cutoff for both
	// scale-up and scale-down, so it will terminate a node running them
	// without ceremony. binpack mirrors that rule exactly and adds nothing.
	//
	// Note that overprovisioning pause pods are NOT expendable: they must sit
	// at or above the cutoff or the warm-capacity buffer never replenishes,
	// and being above it they behave exactly like real workload.
	Expendable
)

func (c PodClass) String() string {
	switch c {
	case Relocatable:
		return "relocatable"
	case NodeLocal:
		return "node-local"
	case Expendable:
		return "expendable"
	default:
		return "unknown"
	}
}

// Classify decides how a pod is treated during consolidation.
//
// expendableCutoff mirrors the autoscaler's --expendable-pods-priority-cutoff.
func Classify(pod *corev1.Pod, expendableCutoff int32) PodClass {
	// Node-local is checked first and deliberately wins over expendable. A
	// DaemonSet pod with a very low priority is still node-local: evicting it
	// would have the controller put it straight back.
	if isNodeLocal(pod) {
		return NodeLocal
	}
	if podPriority(pod) < expendableCutoff {
		return Expendable
	}
	return Relocatable
}

func isNodeLocal(pod *corev1.Pod) bool {
	// Already on its way out: it needs no destination and no eviction.
	//
	// Note this is node-local by *circumstance* rather than by nature, and the
	// two come apart outside the simulation — a drain in progress is waiting
	// on exactly these pods. See [NodeBound].
	return NodeBound(pod) || pod.DeletionTimestamp != nil
}

// NodeBound reports whether a pod is tied to its node by nature: it does not
// move when the node goes away, it ceases to exist with it, and an equivalent
// already runs elsewhere.
//
// Separate from [Classify]'s [NodeLocal] because that answers the simulation's
// question — does this pod need a destination — and a terminating pod does
// not, while still occupying the node it is leaving. A drain waiting for pods
// to go needs this narrower predicate; classifying a terminating pod as
// nothing to do with the drain would report an occupied node as empty.
func NodeBound(pod *corev1.Pod) bool {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "DaemonSet" {
			return true
		}
	}
	// A mirror pod is a static pod managed directly by a kubelet. Node-bound
	// by effect rather than by API refusal: the eviction subresource has no
	// mirror-pod branch (k8s.io/kubernetes v1.36.3
	// pkg/registry/core/pod/storage/eviction.go names neither mirror nor
	// static pods anywhere, and has not back to 1.30), so an eviction is
	// accepted and deletes the mirror object — whereupon the kubelet recreates
	// it from the on-disk manifest and the static pod itself never stopped
	// running. It does not leave the node, which is the property this
	// predicate is about.
	//
	// So a static pod does not block a drain, and there is deliberately no
	// diagnosis saying it does. It dies with the node, exactly as a DaemonSet
	// pod does: binpack never asks for a destination for it and never evicts
	// it, and the cluster-autoscaler exempts mirror pods from the kube-system
	// rule that would otherwise hold the node. The reference documented the
	// opposite at Blocking severity for three releases, describing a finding
	// nothing could emit.
	_, mirror := pod.Annotations[corev1.MirrorPodAnnotationKey]
	return mirror
}

// podPriority resolves a pod's priority. Admission normally populates it from
// the PriorityClass; an unset value means zero, the same default Kubernetes
// applies.
func podPriority(pod *corev1.Pod) int32 {
	if pod.Spec.Priority != nil {
		return *pod.Spec.Priority
	}
	return 0
}

// Occupies reports whether a pod still holds resources on its node.
//
// Terminated pods have released theirs; everything else, including a pod that
// is terminating but has not gone yet, still counts. Erring towards "occupies"
// keeps the simulation conservative, which is the safe direction.
func Occupies(pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return false
	default:
		return true
	}
}
