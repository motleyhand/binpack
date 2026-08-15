package fit

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// defaultSchedulerName is the only scheduler whose behaviour binpack models.
const defaultSchedulerName = "default-scheduler"

// UnsupportedPod reports why binpack cannot reason about where this pod could
// go, or the zero Reason if it can.
//
// This is the allowlist from ADR-0006, applied to the pod being relocated. The
// list below is not required to be exhaustive for the design to be sound —
// that is the point of refusing by default — but each entry names a scheduler
// Filter plugin whose behaviour binpack does not implement.
func UnsupportedPod(pod *corev1.Pod) Reason {
	// NodePorts: a host port is a cluster-wide-per-node resource, and binpack
	// does not track which ports are taken.
	if port, ok := firstHostPort(pod); ok {
		return unsupportedPod(pod, fmt.Sprintf("uses hostPort %d", port))
	}
	// With host networking, container ports become host ports implicitly, so
	// the same objection applies without a hostPort ever being written down.
	if pod.Spec.HostNetwork {
		return unsupportedPod(pod, "uses host networking")
	}

	// VolumeBinding, VolumeZone, VolumeRestrictions, CSI attachment limits.
	// Whether a volume can follow a pod depends on the PV's node affinity, its
	// zone, and how many volumes the destination already has attached — none
	// of which binpack models.
	if name, ok := firstPersistentVolume(pod); ok {
		return unsupportedPod(pod, "uses persistent volume claim "+name)
	}

	// InterPodAffinity. Required terms constrain placement relative to other
	// pods, which needs a view of the whole cluster per candidate node.
	if affinity := pod.Spec.Affinity; affinity != nil {
		if a := affinity.PodAffinity; a != nil && len(a.RequiredDuringSchedulingIgnoredDuringExecution) > 0 {
			return unsupportedPod(pod, "declares required pod affinity")
		}
		if a := affinity.PodAntiAffinity; a != nil && len(a.RequiredDuringSchedulingIgnoredDuringExecution) > 0 {
			return unsupportedPod(pod, "declares required pod anti-affinity")
		}
	}

	// PodTopologySpread, but only the hard variant: ScheduleAnyway affects
	// scoring alone and can never cause a placement to fail, so it is ignored
	// rather than refused.
	for _, c := range pod.Spec.TopologySpreadConstraints {
		if c.WhenUnsatisfiable == corev1.DoNotSchedule {
			return unsupportedPod(pod,
				"declares a DoNotSchedule topology spread constraint on "+c.TopologyKey)
		}
	}

	// A different scheduler may apply any rules at all.
	if name := pod.Spec.SchedulerName; name != "" && name != defaultSchedulerName {
		return unsupportedPod(pod, "is scheduled by "+name+" rather than the default scheduler")
	}

	// A gated pod is not schedulable until something removes the gate, and
	// binpack has no way to know when or whether that happens.
	if len(pod.Spec.SchedulingGates) > 0 {
		return unsupportedPod(pod, "has scheduling gates")
	}

	// Dynamic resource allocation: placement depends on device availability
	// tracked outside the node object.
	if len(pod.Spec.ResourceClaims) > 0 {
		return unsupportedPod(pod, "uses dynamic resource allocation claims")
	}

	return Reason{}
}

// UnsupportedDestination reports why node cannot be considered as a
// destination, including reasons that come from the pods already on it.
//
// The allowlist applies in both directions, but not symmetrically, because the
// scheduler's filters are not symmetric:
//
//   - A resident's required *anti*-affinity can reject an incoming pod, so it
//     disqualifies the node.
//   - A resident's required *affinity* is not re-evaluated when another pod
//     arrives, so it does not.
//   - A resident's topology spread constraints are not evaluated for an
//     incoming pod either; only the incoming pod's own constraints are, and
//     those are handled by UnsupportedPod.
//
// Checking only the incoming pod would let the first case through and leave
// the replacement Pending.
func UnsupportedDestination(node *corev1.Node, residents []*corev1.Pod) Reason {
	for _, resident := range residents {
		affinity := resident.Spec.Affinity
		if affinity == nil || affinity.PodAntiAffinity == nil {
			continue
		}
		if len(affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution) > 0 {
			return Reason{ReasonUnsupportedNode,
				"node " + node.Name + " hosts " + podRef(resident) +
					", which declares required pod anti-affinity that binpack does not model"}
		}
	}
	return Reason{}
}

func firstHostPort(pod *corev1.Pod) (int32, bool) {
	for _, containers := range [][]corev1.Container{pod.Spec.Containers, pod.Spec.InitContainers} {
		for _, c := range containers {
			for _, p := range c.Ports {
				if p.HostPort != 0 {
					return p.HostPort, true
				}
			}
		}
	}
	return 0, false
}

func firstPersistentVolume(pod *corev1.Pod) (string, bool) {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName, true
		}
		// Generic ephemeral volumes are PVCs created on the pod's behalf, and
		// carry the same placement constraints.
		if v.Ephemeral != nil {
			return v.Name + " (generic ephemeral)", true
		}
	}
	return "", false
}

func unsupportedPod(pod *corev1.Pod, what string) Reason {
	return Reason{ReasonUnsupportedPod, podRef(pod) + " " + what + ", which binpack does not model"}
}
