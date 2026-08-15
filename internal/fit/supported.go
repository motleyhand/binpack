package fit

import (
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
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
	if name, ok := firstConstrainingVolume(pod); ok {
		return unsupportedPod(pod, "uses volume "+name+", which may constrain placement")
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

	// An in-flight in-place resize means the pod's requests are changing
	// underneath us, so any answer computed from them is about to be stale.
	//
	// A resize that has already *completed* is handled elsewhere and better:
	// the engine asks about the pod a controller would create, built from its
	// template, so the running pod's shrunken requests never enter the
	// arithmetic. See the replacement doc comment in internal/engine.
	if r := resizeInFlight(pod); r != "" {
		return unsupportedPod(pod, "has an in-place resize "+r)
	}

	// A template that pins spec.nodeName produces pods that bypass the
	// scheduler entirely: they land on the named node whatever binpack decides,
	// so a placement computed for them is fiction. Reachable only because the
	// caller passes the replacement — every *running* pod names a node, which
	// is why this could not be checked before.
	if pod.Spec.NodeName != "" {
		return unsupportedPod(pod, "is pinned to node "+pod.Spec.NodeName+" by its controller template")
	}

	return Reason{}
}

// resizeInFlight reports an in-progress in-place vertical scaling operation,
// via the conditions that replaced the deprecated status.resize field, or that
// field itself for older API servers.
func resizeInFlight(pod *corev1.Pod) string {
	for _, c := range pod.Status.Conditions {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		switch c.Type {
		case corev1.PodResizePending:
			return "pending"
		case corev1.PodResizeInProgress:
			return "in progress"
		}
	}
	if pod.Status.Resize != "" {
		return string(pod.Status.Resize)
	}
	return ""
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
func UnsupportedDestination(pod *corev1.Pod, node *corev1.Node, residents []*corev1.Pod) Reason {
	for _, resident := range residents {
		if antiAffinityCouldReject(resident, pod) {
			return Reason{ReasonUnsupportedNode,
				"node " + node.Name + " hosts " + podRef(resident) +
					", whose required pod anti-affinity could reject " + podRef(pod)}
		}
	}
	return Reason{}
}

// antiAffinityCouldReject reports whether a resident's required anti-affinity
// might apply to the incoming pod.
//
// The selector has to be evaluated, not merely detected. Almost every cluster
// runs a CNI DaemonSet with anti-affinity to *itself* — Cilium's agent selects
// k8s-app=cilium, so that it lands once per node — and that term is on every
// node in the cluster. Treating its presence as disqualifying would rule out
// every destination on every such cluster, which is to say most of them.
//
// So a term matters only when its selector matches the incoming pod's labels
// and its namespace scope covers that pod. Anything binpack cannot evaluate —
// an unparseable selector, a namespace selector needing Namespace objects it
// does not read — counts as a possible match, because refusing is the safe
// direction.
func antiAffinityCouldReject(resident, incoming *corev1.Pod) bool {
	affinity := resident.Spec.Affinity
	if affinity == nil || affinity.PodAntiAffinity == nil {
		return false
	}

	for _, term := range affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		// A namespace selector would need the Namespace objects to evaluate,
		// which this package does not have.
		if term.NamespaceSelector != nil {
			return true
		}

		// An empty Namespaces list means the resident's own namespace.
		scope := term.Namespaces
		if len(scope) == 0 {
			scope = []string{resident.Namespace}
		}
		if !slices.Contains(scope, incoming.Namespace) {
			continue
		}

		selector, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
		if err != nil {
			return true
		}
		if selector.Matches(labels.Set(incoming.Labels)) {
			return true
		}
	}

	return false
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

// firstConstrainingVolume returns the first volume binpack cannot prove is
// placement-neutral.
//
// An allowlist, for the same reason everything else here is one. Enumerating
// the volume types that *do* constrain placement means listing persistent
// volume claims, generic ephemeral volumes, inline CSI sources, and every
// in-tree cloud disk — and then adding to that list whenever Kubernetes gains
// another. Naming the handful that are demonstrably node-independent is a
// finite job, and a volume type nobody has considered refuses by default.
//
// The allowed kinds are projections of API objects or node-local scratch: they
// exist on any node, need no attachment, and impose no scheduling constraint.
// Note that hostPath does affect *evictability* — the autoscaler will not
// evict a pod using one without an explicit safe-to-evict annotation — but
// that is a separate question from where a replacement could be placed.
func firstConstrainingVolume(pod *corev1.Pod) (string, bool) {
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.EmptyDir != nil,
			v.ConfigMap != nil,
			v.Secret != nil,
			v.DownwardAPI != nil,
			v.Projected != nil,
			v.HostPath != nil:
			continue
		default:
			return v.Name, true
		}
	}
	return "", false
}

func unsupportedPod(pod *corev1.Pod, what string) Reason {
	return Reason{ReasonUnsupportedPod, podRef(pod) + " " + what + ", which binpack does not model"}
}
