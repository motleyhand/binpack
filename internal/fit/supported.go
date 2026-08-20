package fit

import (
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/component-helpers/nodedeclaredfeatures"
)

// defaultSchedulerName is the only scheduler whose behaviour binpack models.
const defaultSchedulerName = "default-scheduler"

// schedulingTargetVersion is the Kubernetes version binpack infers node
// feature requirements at. It matches the k8s.io release this module pins,
// which is the choice kube-scheduler makes too — its plugin passes its own
// binary's version.
//
// The parameter only ever *removes* requirements: InferForPodScheduling drops
// any feature whose MaxVersion the target has passed, on the grounds that
// every kubelet still within the version skew implements it unconditionally
// and has stopped declaring it. So a value that is too low costs a
// consolidation and a value that is too high accepts a node the scheduler
// refuses, and only one of those is a direction this package may err in. Left
// behind on a dependency bump, it errs the safe way.
//
// TestTheSchedulingTargetVersionDropsNoFeature holds it to that: every
// registered feature is unbounded today, and the release that bounds one fails
// there rather than quietly widening what binpack accepts.
var schedulingTargetVersion = version.MajorMinor(1, 36)

// SchedulingTargetVersion returns that version, so a test can run upstream's
// inference at exactly the version this package runs it at.
func SchedulingTargetVersion() *version.Version { return schedulingTargetVersion }

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

	// GangScheduling. A pod naming a scheduling group is not an independently
	// schedulable object: its group's policy decides when it may enter the
	// queue at all, and it is then held at Permit until the whole group can be
	// placed together. A single-pod placement proved for it is not a claim
	// that its replacement will run.
	if group := pod.Spec.SchedulingGroup; group != nil {
		named := "a scheduling group"
		if group.PodGroupName != nil {
			named = "scheduling group " + *group.PodGroupName
		}
		return unsupportedPod(pod, "belongs to "+named)
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
// destination, including reasons that come from pods other than the one being
// placed.
//
// The allowlist applies in both directions, but not symmetrically, because the
// scheduler's filters are not symmetric:
//
//   - Another pod's required *anti*-affinity can reject an incoming pod, so it
//     disqualifies every node in the domain that term covers. Only for a term
//     keyed on kubernetes.io/hostname is that domain the declaring pod's own
//     node; see AntiAffinityDomains.
//   - Another pod's required *affinity* is not re-evaluated when a pod
//     arrives, so it disqualifies nothing.
//   - A resident's topology spread constraints are not evaluated for an
//     incoming pod either; only the incoming pod's own constraints are, and
//     those are handled by UnsupportedPod.
//   - A pod can require a capability of the destination's *kubelet*, which is
//     a property of the pair rather than of either side, so UnsupportedPod
//     cannot decide it at all.
//
// Checking only the incoming pod would let the first and last cases through
// and leave the replacement Pending.
//
// domains carries the first case beyond the node in front of it and may be
// empty, which says the caller holds no wider view; residents is then all the
// anti-affinity binpack can see. Both are asked, because they fail in
// different places: a hostname-keyed term is caught by residents whether or
// not the node is labelled into any domain at all.
func UnsupportedDestination(
	pod *corev1.Pod,
	node *corev1.Node,
	residents []*corev1.Pod,
	domains AntiAffinityDomains,
) Reason {
	if r := undeclaredFeatures(pod, node); !r.Empty() {
		return r
	}

	for _, resident := range residents {
		if antiAffinityCouldReject(resident, pod) {
			return Reason{ReasonUnsupportedNode,
				"node " + node.Name + " hosts " + podRef(resident) +
					", whose required pod anti-affinity could reject " + podRef(pod)}
		}
	}

	if declarer, domain, rejected := domains.rejects(pod, node); rejected {
		return Reason{ReasonUnsupportedNode,
			"node " + node.Name + " is in " + domain + " with " + podRef(declarer) +
				", whose required pod anti-affinity could reject " + podRef(pod)}
	}
	return Reason{}
}

// AntiAffinityDomains records where the cluster's required pod anti-affinity
// applies, keyed the way the scheduler keys it: by the topology domain a term
// covers, rather than by the node the pod declaring it happens to sit on.
//
// The two coincide only at kubernetes.io/hostname, where the domain is one
// node. A term keyed on the zone rejects every node in that zone — including
// nodes hosting no matching pod of their own — so asking each candidate node
// about its own residents asks a narrower question than the scheduler's, and
// gets it wrong in the accepting direction: binpack approves a destination
// that will refuse the replacement, and the eviction has already happened.
// That is why this is built once from the whole cluster and handed down to the
// per-node question rather than computed inside it.
//
// The scheduler builds the same index, over every node hosting a pod with
// required anti-affinity, and then checks the candidate node's own labels
// against it (interpodaffinity's getExistingAntiAffinityCounts and
// satisfyExistingPodsAntiAffinity). This is that, kept to the same shape.
//
// The pods are held by pointer and never written to; they come from a shared
// informer cache upstream of here.
type AntiAffinityDomains []antiAffinityDomain

// antiAffinityDomain is one required anti-affinity term, with the topology
// domain the declaring pod's node places it in.
type antiAffinityDomain struct {
	declarer *corev1.Pod
	term     corev1.PodAffinityTerm
	value    string
}

// NewAntiAffinityDomains indexes every required anti-affinity term in the
// cluster by the domain it covers.
//
// Pods are located by spec.nodeName, so one that is not scheduled yet
// contributes nothing: it is in no domain. A term whose topologyKey the
// declaring pod's node does not carry contributes nothing either, which is
// what upstream does — a domain a node is not labelled into is one it is not
// in, and inventing an empty-string domain would make every unlabelled node
// share it.
//
// Everything else contributes, terminating and completed pods included. They
// are in the scheduler's snapshot until they are gone, and a term that counts
// slightly too long costs a consolidation where one dropped too early costs a
// Pending pod.
func NewAntiAffinityDomains(nodes []*corev1.Node, pods []*corev1.Pod) AntiAffinityDomains {
	byName := make(map[string]*corev1.Node, len(nodes))
	for _, node := range nodes {
		byName[node.Name] = node
	}

	var domains AntiAffinityDomains
	for _, pod := range pods {
		affinity := pod.Spec.Affinity
		if affinity == nil || affinity.PodAntiAffinity == nil {
			continue
		}
		node, ok := byName[pod.Spec.NodeName]
		if !ok {
			continue
		}
		for _, term := range affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
			value, ok := node.Labels[term.TopologyKey]
			if !ok {
				continue
			}
			domains = append(domains, antiAffinityDomain{declarer: pod, term: term, value: value})
		}
	}
	return domains
}

// rejects reports whether node sits in a domain holding anti-affinity that
// could reject pod, naming the pod that declared it and the domain.
func (d AntiAffinityDomains) rejects(pod *corev1.Pod, node *corev1.Node) (*corev1.Pod, string, bool) {
	for _, domain := range d {
		value, ok := node.Labels[domain.term.TopologyKey]
		if !ok || value != domain.value {
			continue
		}
		if termCouldReject(domain.term, domain.declarer, pod) {
			return domain.declarer, domain.term.TopologyKey + "=" + value, true
		}
	}
	return nil, "", false
}

// undeclaredFeatures reports whether node lacks a capability pod's spec
// implies, which is what the scheduler's NodeDeclaredFeatures Filter decides.
//
// Both halves are upstream's, deliberately. The requirement is inferred by
// k8s.io/component-helpers' registry and matched against
// node.status.declaredFeatures by the same code the plugin calls, so the
// question binpack asks tracks the pinned release rather than a list somebody
// wrote down. Recognising the one live shape by hand — a container carrying a
// RestartAllContainers restart rule — would be right today and stale the day
// upstream registers a sixth feature or gives an inert one a scheduling
// meaning. That failure would be silent and it would be in the accepting
// direction, which is the same object as the hand-written feature-gate list
// the differential harness had to stop keeping.
//
// Deriving also buys a better answer than a blanket refusal could. binpack has
// the destination in hand here, so a pod using container restart rules is
// refused exactly the nodes whose kubelets have not declared the feature —
// none at all on a homogeneous cluster — where refusing in UnsupportedPod
// would make every such workload permanently unrelocatable everywhere.
//
// The pod's spec goes to upstream by pointer, without a copy. Nothing on this
// path writes to it, and the scheduler's own plugin does the same with pods
// straight out of an informer cache.
func undeclaredFeatures(pod *corev1.Pod, node *corev1.Node) Reason {
	framework := nodedeclaredfeatures.DefaultFramework

	required, err := framework.InferForPodScheduling(
		&nodedeclaredfeatures.PodInfo{Spec: &pod.Spec}, schedulingTargetVersion)
	if err != nil {
		return Reason{ReasonUnsupportedNode,
			"the node features " + podRef(pod) + " needs could not be inferred: " + err.Error()}
	}
	// The plugin skips its own Filter when a pod requires nothing, which is
	// almost every pod: the inference returns an empty set unless the spec
	// names one of the handful of registered features.
	if required.IsEmpty() {
		return Reason{}
	}

	match, err := framework.MatchNode(required, node)
	if err != nil {
		return Reason{ReasonUnsupportedNode,
			"node " + node.Name + " could not be matched against the features " +
				podRef(pod) + " needs: " + err.Error()}
	}
	if match.IsMatch {
		return Reason{}
	}

	// A node that has published no declared features at all lands here too,
	// and correctly: an older kubelet, or one with the gate off, publishes an
	// empty list and the scheduler refuses it for the same reason.
	return Reason{ReasonUnsupportedNode,
		"node " + node.Name + " has not declared " +
			strings.Join(match.UnsatisfiedRequirements, ", ") + ", which " +
			podRef(pod) + " requires"}
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
		if termCouldReject(term, resident, incoming) {
			return true
		}
	}

	return false
}

// termCouldReject applies one term, declared by declarer, to incoming.
//
// Shared with the domain index deliberately: the two paths ask about the same
// term from different distances, and a term that resolves to "cannot tell,
// so refuse" on one of them has to resolve the same way on the other. Written
// twice, they would drift, and the drift would be silent in the accepting
// direction on whichever copy was left behind.
func termCouldReject(term corev1.PodAffinityTerm, declarer, incoming *corev1.Pod) bool {
	// A namespace selector would need the Namespace objects to evaluate,
	// which this package does not have.
	if term.NamespaceSelector != nil {
		return true
	}

	// An empty Namespaces list means the declaring pod's own namespace.
	scope := term.Namespaces
	if len(scope) == 0 {
		scope = []string{declarer.Namespace}
	}
	if !slices.Contains(scope, incoming.Namespace) {
		return false
	}

	selector, err := metav1.LabelSelectorAsSelector(term.LabelSelector)
	if err != nil {
		return true
	}
	return selector.Matches(labels.Set(incoming.Labels))
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
