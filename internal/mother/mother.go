// Package mother builds Kubernetes objects for tests.
//
// Two layers, and both matter. Mothers name archetypes — [SmallNode],
// [DaemonSetPod] — so a test says what it needs in a few words and the meaning
// of "an ordinary 4GB node" lives in one place. Options customise them —
// SmallNode(Cordoned()) — so a test states only the thing it is about rather
// than restating an entire API object.
//
// This is more than convenience here. binpack's test tables are the readable
// specification of its decision procedure, and a table where each row is
// thirty lines of struct literal specifies nothing anybody will read.
package mother

import (
	"maps"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/motleyhand/binpack/internal/engine"
)

// NodeOption customises a node archetype.
type NodeOption func(*corev1.Node)

// PodOption customises a pod archetype.
type PodOption func(*corev1.Pod)

// Node is the base archetype: schedulable, Ready, no taints, and enough
// capacity that resources are never accidentally the reason a test fails.
// Prefer a more specific mother where one fits.
func Node(name string, opts ...NodeOption) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("4"),
				corev1.ResourceMemory: resource.MustParse("16Gi"),
				corev1.ResourcePods:   resource.MustParse("110"),
			},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
	for _, o := range opts {
		o(n)
	}
	return n
}

// SmallNode is a 2 vCPU / 4GB worker after kubelet reservations — the shape
// that makes percentage-based reasoning go wrong, and the reason binpack works
// in absolute quantities.
//
// The figures are chosen so that a test table's arithmetic can be read at a
// glance, not to model a node any provider sells: 1360Mi of a nominal 4GB is a
// larger reservation than anything real, and it is not in the same proportion
// as [LargeNode]'s. What the tests need is only that the two differ enough to
// make a mixed-size cluster where the smaller node's workload fits on the
// larger one. Real allocatable is a provider question — DigitalOcean publishes
// 2.5 GiB on a 4 GiB node and 6 GiB on an 8 GiB one — and nothing outside this
// package depends on these values, so treat them as arithmetic rather than as
// a claim about hardware.
func SmallNode(name string, opts ...NodeOption) *corev1.Node {
	return Node(name, append([]NodeOption{
		Allocatable(corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1900m"),
			corev1.ResourceMemory: resource.MustParse("1360Mi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		}),
	}, opts...)...)
}

// LargeNode is a 4 vCPU / 8GB worker after reservations. Paired with
// [SmallNode] it produces the mixed-size cluster where a node at 81 percent
// holds less than one at 60 percent.
func LargeNode(name string, opts ...NodeOption) *corev1.Node {
	return Node(name, append([]NodeOption{
		Allocatable(corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("3900m"),
			corev1.ResourceMemory: resource.MustParse("6800Mi"),
			corev1.ResourcePods:   resource.MustParse("110"),
		}),
	}, opts...)...)
}

// GPUNode advertises an extended resource, which a node without one can never
// satisfy however much CPU and memory it has free.
func GPUNode(name string, count int64, opts ...NodeOption) *corev1.Node {
	return Node(name, append([]NodeOption{
		func(n *corev1.Node) {
			n.Status.Allocatable["nvidia.com/gpu"] = *resource.NewQuantity(count, resource.DecimalSI)
		},
	}, opts...)...)
}

// Allocatable replaces a node's allocatable resources outright.
func Allocatable(rl corev1.ResourceList) NodeOption {
	return func(n *corev1.Node) { n.Status.Allocatable = rl.DeepCopy() }
}

// Cordoned marks the node unschedulable, as `kubectl cordon` does.
func Cordoned() NodeOption {
	return func(n *corev1.Node) { n.Spec.Unschedulable = true }
}

// NotReady flips the Ready condition.
func NotReady() NodeOption {
	return func(n *corev1.Node) {
		for i := range n.Status.Conditions {
			if n.Status.Conditions[i].Type == corev1.NodeReady {
				n.Status.Conditions[i].Status = corev1.ConditionFalse
			}
		}
	}
}

// Tainted adds a taint.
func Tainted(key, value string, effect corev1.TaintEffect) NodeOption {
	return func(n *corev1.Node) {
		n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: key, Value: value, Effect: effect})
	}
}

// NodeLabels merges labels onto the node.
func NodeLabels(labels map[string]string) NodeOption {
	return func(n *corev1.Node) {
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}
		maps.Copy(n.Labels, labels)
	}
}

// NodeAnnotations merges annotations onto a node.
func NodeAnnotations(annotations map[string]string) NodeOption {
	return func(n *corev1.Node) {
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		maps.Copy(n.Annotations, annotations)
	}
}

// InPool labels a node with the DOKS pool labels binpack discovers pools by.
func InPool(name, id string) NodeOption {
	return NodeLabels(map[string]string{
		"doks.digitalocean.com/node-pool":    name,
		"doks.digitalocean.com/node-pool-id": id,
	})
}

// DeclaringFeature publishes a feature in the node's status.declaredFeatures,
// as a kubelet does for each feature it implements.
//
// The list is kept sorted, because that is what the kubelet writes and what
// upstream's reader assumes: MatchNode merge-joins the node's list against its
// registry, so an out-of-order entry is silently dropped rather than rejected.
// A fixture that skipped the sort would model a node no kubelet produces, and
// would quietly answer "feature absent" for a feature it had just declared.
func DeclaringFeature(name string) NodeOption {
	return func(n *corev1.Node) {
		n.Status.DeclaredFeatures = append(n.Status.DeclaredFeatures, name)
		slices.Sort(n.Status.DeclaredFeatures)
	}
}

// Pod is the base archetype: an ordinary Deployment-owned workload with modest
// requests, nothing unusual about it.
func Pod(namespace, name string, opts ...PodOption) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			OwnerReferences: []metav1.OwnerReference{
				// A UID, because every real owner reference has one and
				// binpack correlates a replacement pod to its controller by
				// it. A fixture without one would make a drain's wait for the
				// replacement untestable, and untested.
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: name + "-rs",
					UID: ownerUID(name + "-rs"), Controller: new(true)},
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			}},
		},
		// Running and Ready by default. Readiness is load-bearing for
		// eviction: an unready pod may be evictable without drawing on its
		// disruption budget, so an archetype that silently defaulted to
		// unready would make tests agree with the code for the wrong reason.
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	for _, o := range opts {
		o(p)
	}
	return p
}

// DaemonSetPod is node-local: it is never relocated and never evicted, because
// an equivalent already runs on every other node and the controller would
// recreate it on this one.
func DaemonSetPod(namespace, name string, opts ...PodOption) *corev1.Pod {
	return Pod(namespace, name, append([]PodOption{
		func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "DaemonSet", Name: name, Controller: new(true)},
			}
		},
	}, opts...)...)
}

// MirrorPod is a static pod managed directly by a kubelet. It cannot be
// evicted at all.
func MirrorPod(namespace, name string, opts ...PodOption) *corev1.Pod {
	return Pod(namespace, name, append([]PodOption{
		Annotated(corev1.MirrorPodAnnotationKey, "true"),
		func(p *corev1.Pod) { p.OwnerReferences = nil },
	}, opts...)...)
}

// PausePod is overprovisioning filler. It sits at or above the autoscaler's
// expendable cutoff by necessity, which is why it counts as real workload.
func PausePod(namespace, name string, opts ...PodOption) *corev1.Pod {
	return Pod(namespace, name, append([]PodOption{
		Priority(0),
		PriorityClass("overprovisioning"),
	}, opts...)...)
}

// WithPodLevelResources sets requests on the pod rather than on a container.
//
// Beta and enabled by default since 1.34 (k8s.io/kubernetes v1.36.3
// pkg/features/kube_features.go, PodLevelResources), so this is live on a
// stock cluster rather than an opt-in curiosity.
//
// It does not replace the container aggregate wholesale. PodRequests computes
// the container aggregate first and then overrides only the names that are
// both present in spec.resources.requests and pod-level-supported — cpu,
// memory and hugepages-* (k8s.io/component-helpers v0.36.3
// resource/helpers.go, IsSupportedPodLevelResource); every other name keeps
// its container-aggregate value, and spec.overhead is added on top of the
// result. So a pod whose containers ask for almost nothing can still be
// large, but only in the dimensions the pod names.
//
// Either argument may be empty, meaning that name is left out of the pod-level
// map — which is the shape that distinguishes the two readings, and the one
// the fixture could not express while this comment said "in place of the
// container aggregate".
func WithPodLevelResources(cpu, memory string) PodOption {
	return func(p *corev1.Pod) {
		requests := corev1.ResourceList{}
		if cpu != "" {
			requests[corev1.ResourceCPU] = resource.MustParse(cpu)
		}
		if memory != "" {
			requests[corev1.ResourceMemory] = resource.MustParse(memory)
		}
		p.Spec.Resources = &corev1.ResourceRequirements{Requests: requests}
	}
}

// Requests replaces the first container's requests.
func Requests(cpu, memory string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		}
	}
}

// Requesting sets an arbitrary resource on the first container, including
// extended resources such as nvidia.com/gpu.
func Requesting(name corev1.ResourceName, quantity string) PodOption {
	return func(p *corev1.Pod) {
		if p.Spec.Containers[0].Resources.Requests == nil {
			p.Spec.Containers[0].Resources.Requests = corev1.ResourceList{}
		}
		p.Spec.Containers[0].Resources.Requests[name] = resource.MustParse(quantity)
	}
}

// WithInitContainer adds an init container, whose peak request the scheduler
// takes the maximum of against the regular-container sum.
func WithInitContainer(name, cpu, memory string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{
			Name: name,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpu),
					corev1.ResourceMemory: resource.MustParse(memory),
				},
			},
		})
	}
}

// WithSidecar adds a native sidecar — an init container with an Always restart
// policy — which stays running and therefore adds to the total rather than
// participating in the init-container maximum.
func WithSidecar(name, cpu, memory string) PodOption {
	always := corev1.ContainerRestartPolicyAlways
	return func(p *corev1.Pod) {
		p.Spec.InitContainers = append(p.Spec.InitContainers, corev1.Container{
			Name:          name,
			RestartPolicy: &always,
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cpu),
					corev1.ResourceMemory: resource.MustParse(memory),
				},
			},
		})
	}
}

// WithOverhead sets the RuntimeClass overhead the scheduler adds on top.
func WithOverhead(cpu, memory string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.Overhead = corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(memory),
		}
	}
}

// OnNode binds the pod to a node.
func OnNode(name string) PodOption {
	return func(p *corev1.Pod) { p.Spec.NodeName = name }
}

// Terminating marks a pod as shutting down, as the API server does once a
// delete or an eviction is accepted.
//
// requestedAt is when deletion was asked for; the deletion timestamp is set to
// requestedAt + grace, because that is what the API server writes — the field
// records when the grace period *expires*, not when deletion was requested.

func Terminating(requestedAt time.Time, grace time.Duration) PodOption {
	return func(p *corev1.Pod) {
		stamp := metav1.NewTime(requestedAt.Add(grace))
		p.DeletionTimestamp = &stamp
		seconds := int64(grace.Seconds())
		p.DeletionGracePeriodSeconds = &seconds
	}
}

// ControlledBy replaces the pod's controller with a named one binpack can read
// a template for, so several pods can share an owner — a Deployment's
// replicas, or the pod a controller creates to replace an evicted one.
func ControlledBy(kind, name string) PodOption {
	return func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: kind, Name: name,
			UID: ownerUID(name), Controller: new(true),
		}}
	}
}

// OwnerUID is the UID [ControlledBy] gives a named controller, so a test can name
// what a drain is waiting for without reaching into a pod to read it.
func OwnerUID(name string) types.UID { return ownerUID(name) }

func ownerUID(name string) types.UID { return types.UID("uid-" + name) }

// WithHostPort claims a host port, which binpack does not model.
func WithHostPort(port int32) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.Containers[0].Ports = append(p.Spec.Containers[0].Ports,
			corev1.ContainerPort{ContainerPort: port, HostPort: port})
	}
}

// WithPVC attaches a persistent volume claim.
func WithPVC(claim string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
			Name: claim,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
			},
		})
	}
}

// WithEmptyDir attaches scratch space, which constrains placement not at all.
func WithEmptyDir(name string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
			Name:         name,
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
	}
}

// WithConfigMapVolume projects a ConfigMap, which exists on every node.
func WithConfigMapVolume(name string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
				},
			},
		})
	}
}

// WithInlineCSIVolume attaches a CSI volume without a PVC. Its driver may
// still count against the destination's attachment limit.
func WithInlineCSIVolume(name, driver string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
			Name:         name,
			VolumeSource: corev1.VolumeSource{CSI: &corev1.CSIVolumeSource{Driver: driver}},
		})
	}
}

// WithRequiredAntiAffinity declares required pod anti-affinity at the hostname
// topology, which is symmetric: it can reject an incoming pod as well as
// constrain this one.
func WithRequiredAntiAffinity(labelKey, labelValue string) PodOption {
	return WithRequiredAntiAffinityAt(corev1.LabelHostname, labelKey, labelValue)
}

// WithRequiredAntiAffinityAt declares the same term at an arbitrary topology.
//
// The distinction is the whole of the difference between what binpack checks
// and what the scheduler checks. A hostname-keyed term rejects only the node
// the declaring pod sits on; a term keyed wider — zone, region — rejects every
// node in that domain, including empty ones binpack would otherwise consider a
// destination.
func WithRequiredAntiAffinityAt(topologyKey, labelKey, labelValue string) PodOption {
	return func(p *corev1.Pod) {
		if p.Spec.Affinity == nil {
			p.Spec.Affinity = &corev1.Affinity{}
		}
		p.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey: topologyKey,
				LabelSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{labelKey: labelValue},
				},
			}},
		}
	}
}

// WithHardTopologySpread declares a DoNotSchedule spread constraint. The soft
// variant is deliberately not offered: it only affects scoring and binpack
// ignores it.
func WithHardTopologySpread(topologyKey string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.TopologySpreadConstraints = append(p.Spec.TopologySpreadConstraints,
			corev1.TopologySpreadConstraint{
				MaxSkew:           1,
				TopologyKey:       topologyKey,
				WhenUnsatisfiable: corev1.DoNotSchedule,
			})
	}
}

// WithNodeSelector constrains the pod to nodes carrying a label.
func WithNodeSelector(key, value string) PodOption {
	return func(p *corev1.Pod) {
		if p.Spec.NodeSelector == nil {
			p.Spec.NodeSelector = map[string]string{}
		}
		p.Spec.NodeSelector[key] = value
	}
}

// Tolerating tolerates a taint key with the given effect.
func Tolerating(key string, effect corev1.TaintEffect) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{
			Key:      key,
			Operator: corev1.TolerationOpExists,
			Effect:   effect,
		})
	}
}

// ToleratingGt tolerates a numeric taint whose value is greater than value.
//
// Gt and Lt tolerations are gated: the scheduler only honours them when
// TaintTolerationComparisonOperators is on, and it is alpha and off by default.
// A fixture exists for that reason — the interesting case is a toleration the
// API server accepts and the scheduler declines to match.
func ToleratingGt(key, value string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{
			Key:      key,
			Operator: corev1.TolerationOpGt,
			Value:    value,
			Effect:   corev1.TaintEffectNoSchedule,
		})
	}
}

// ToleratingEverything tolerates every taint, the way a chart's "run anywhere"
// values do.
//
// The blanket form specifically, because it is the only one that changes what a
// drain means. Per k8s.io/api core/v1 Toleration.ToleratesTaint, a toleration
// matches the node.kubernetes.io/unschedulable:NoSchedule taint a cordon
// carries only when its key is empty — which the API requires to pair with
// operator Exists, i.e. tolerate everything — or is literally that key. A
// key-scoped Exists, `{key: nvidia.com/gpu, operator: Exists}`, which is what
// most spot and GPU-pool workloads actually carry, does not match it.
//
// So this is the fixture for a pod the scheduler is free to place back onto the
// node binpack is draining.
func ToleratingEverything() PodOption {
	return func(p *corev1.Pod) {
		p.Spec.Tolerations = append(p.Spec.Tolerations, corev1.Toleration{
			Operator: corev1.TolerationOpExists,
		})
	}
}

// PDB builds a PodDisruptionBudget selecting pods by label, with the given
// number of disruptions currently allowed.
//
// disruptionsAllowed is status rather than spec: it is what the controller
// computed from current replica health, and it is what the eviction API
// actually consults.
//
// The rest of the status is filled in to match — a two-replica workload, both
// healthy, requiring however many the allowance implies. A budget carrying
// only an allowance is not a budget any cluster produces, and one with
// expectedPods left at zero is indistinguishable from a budget whose selector
// matches nothing, which is a real and separately diagnosable condition.
func PDB(namespace, name string, disruptionsAllowed int32, selector map[string]string) *policyv1.PodDisruptionBudget {
	const replicas = 2
	desired := int32(replicas) - disruptionsAllowed
	minAvailable := intstr.FromInt32(desired)

	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:     &metav1.LabelSelector{MatchLabels: selector},
			MinAvailable: &minAvailable,
		},
		Status: policyv1.PodDisruptionBudgetStatus{
			DisruptionsAllowed: disruptionsAllowed,
			ExpectedPods:       replicas,
			CurrentHealthy:     replicas,
			DesiredHealthy:     desired,
		},
	}
}

// Replicas sets how many pods a budget selects, independently of how many are
// healthy. The gap between the two is what separates a budget temporarily out
// of slack from one that can never have any.
func Replicas(pdb *policyv1.PodDisruptionBudget, expected int32) *policyv1.PodDisruptionBudget {
	pdb.Status.ExpectedPods = expected
	return pdb
}

// SelectsNothing empties a budget's status the way the disruption controller
// does when its selector matches no pods — as a chart whose pod labels changed
// leaves behind. Such a budget blocks nothing and protects nothing, and looks
// identical to a healthy one until you read expectedPods.
func SelectsNothing(pdb *policyv1.PodDisruptionBudget) *policyv1.PodDisruptionBudget {
	pdb.Status.ExpectedPods = 0
	pdb.Status.CurrentHealthy = 0
	pdb.Status.DesiredHealthy = 0
	pdb.Status.DisruptionsAllowed = 0
	return pdb
}

// Bare removes the controller reference, making the pod one nothing would
// recreate after eviction.
func Bare() PodOption {
	return func(p *corev1.Pod) { p.OwnerReferences = nil }
}

// OwnedBy names a controller of an arbitrary kind, as an operator's own
// controller does. binpack can read no template for such a pod.
func OwnedBy(kind, name string) PodOption {
	return func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{
			{APIVersion: "example.com/v1", Kind: kind, Name: name,
				UID: ownerUID(name), Controller: new(true)},
		}
	}
}

// OwnedButNotControlled gives the pod an owner reference with Controller
// unset — a garbage-collection link rather than a controller that would
// recreate it. Such a pod is as bare as one with no owner at all.
func OwnedButNotControlled(kind, name string) PodOption {
	return func(p *corev1.Pod) {
		p.OwnerReferences = []metav1.OwnerReference{
			{APIVersion: "apps/v1", Kind: kind, Name: name},
		}
	}
}

// Stale marks a PDB whose controller has not yet observed the current spec.
// The eviction API refuses disruptions in that state whatever the recorded
// allowance says.
func Stale(pdb *policyv1.PodDisruptionBudget) *policyv1.PodDisruptionBudget {
	pdb.Generation = 7
	pdb.Status.ObservedGeneration = 6
	return pdb
}

// WithHostPathVolume attaches a hostPath volume, which the autoscaler treats
// as local storage.
func WithHostPathVolume(name, path string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
			Name:         name,
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: path}},
		})
	}
}

// SafeToEvict sets the cluster-autoscaler annotation.
func SafeToEvict(value string) PodOption {
	return Annotated("cluster-autoscaler.kubernetes.io/safe-to-evict", value)
}

// Pending is a pod the scheduler has not placed. It holds no node open — and
// for a pod below the autoscaler's expendable cutoff, being unplaced is itself
// the failure, since the autoscaler will not scale up to accommodate one.
func Pending() PodOption {
	return func(p *corev1.Pod) {
		p.Spec.NodeName = ""
		p.Status.Phase = corev1.PodPending
		p.Status.Conditions = []corev1.PodCondition{{
			Type:   corev1.PodScheduled,
			Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable,
		}}
	}
}

// Unready flips the pod's Ready condition, as a CrashLoopBackOff replica has.
func Unready() PodOption {
	return func(p *corev1.Pod) {
		for i := range p.Status.Conditions {
			if p.Status.Conditions[i].Type == corev1.PodReady {
				p.Status.Conditions[i].Status = corev1.ConditionFalse
			}
		}
	}
}

// Healthy sets a budget's health counters, which decide whether an unready pod
// can be evicted without consuming the allowance. expectedPods is widened to
// match, since a budget cannot desire more replicas than it selects.
func Healthy(pdb *policyv1.PodDisruptionBudget, current, desired int32) *policyv1.PodDisruptionBudget {
	pdb.Status.CurrentHealthy = current
	pdb.Status.DesiredHealthy = desired
	if pdb.Status.ExpectedPods < desired {
		pdb.Status.ExpectedPods = desired
	}
	return pdb
}

// AlwaysAllowUnhealthy sets unhealthyPodEvictionPolicy: AlwaysAllow, under
// which an unready pod is evictable regardless of the budget.
func AlwaysAllowUnhealthy(pdb *policyv1.PodDisruptionBudget) *policyv1.PodDisruptionBudget {
	policy := policyv1.AlwaysAllow
	pdb.Spec.UnhealthyPodEvictionPolicy = &policy
	return pdb
}

// Resizing marks an in-place vertical scale as in progress, and says nothing
// about the sizes involved. Use [ResizingFrom] where the arithmetic is what
// the test is about.
func Resizing() PodOption {
	return func(p *corev1.Pod) {
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
			Type:   corev1.PodResizeInProgress,
			Status: corev1.ConditionTrue,
		})
	}
}

// ResizingFrom models a pod the kubelet has admitted an in-place vertical
// scale for and not yet actuated: spec asks for the new figure, the container
// status still reports the old one.
//
// One option writes every half of that state, deliberately. What the scheduler
// charges is max(spec, actuated, allocated) — k8s.io/component-helpers
// resource.PodRequests under UseStatusResources, which is how
// PodInfo.CalculateResource sizes a pod already on a node — so a fixture that
// set the spec here and the status somewhere else could express half the state
// and produce a number no cluster ever shows. The point of a mother is that
// the incoherent version cannot be written.
//
// allocated is derived rather than taken: the kubelet writes
// allocatedResources when it *admits* the resize, and actuates afterwards, so
// while PodResizeInProgress is set allocated is the new figure and only the
// actuated one lags. The state where the two come apart is the other one — a
// resize the node cannot fit, which the kubelet reports as PodResizePending
// with reason Infeasible, and which the same helper sizes as
// max(actuated, allocated), ignoring spec entirely. No mother models that yet;
// binpack over-charges such a pod, which is the safe direction.
//
// The first container only, like [Requests], and it sets the
// PodResizeInProgress condition itself — pairing it with [Resizing] would
// write the condition twice.
//
// The pod-level status is written whichever way the pod is shaped, so that
// composing with [WithPodLevelResources] works in either order. Where no
// pod-level request is set it changes nothing: PodRequests reads the pod-level
// status only behind IsPodLevelRequestsSet, which is a question about the
// spec.
func ResizingFrom(spec, actuated corev1.ResourceList) PodOption {
	return func(p *corev1.Pod) {
		container := &p.Spec.Containers[0]
		container.Resources.Requests = spec.DeepCopy()

		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{
			Name:               container.Name,
			Resources:          &corev1.ResourceRequirements{Requests: actuated.DeepCopy()},
			AllocatedResources: spec.DeepCopy(),
		})
		p.Status.Resources = &corev1.ResourceRequirements{Requests: actuated.DeepCopy()}
		p.Status.AllocatedResources = spec.DeepCopy()

		Resizing()(p)
	}
}

// ResizeInfeasibleFrom models the other half of the resize story: a scale the
// kubelet has looked at and refused, because the node it is on cannot fit it.
//
// The sibling of [ResizingFrom], and the difference is which figure
// `allocatedResources` carries. There the kubelet has admitted the resize and
// only actuation lags, so allocated is the new spec. Here it has admitted
// nothing, so allocated stays at what is already in force alongside actuated,
// and spec is a request no node has granted.
//
// It matters because upstream sizes the two states differently.
// resource.PodRequests under UseStatusResources branches on
// IsPodResizeInfeasible and returns max(actuated, allocated) here — dropping
// spec from the maximum entirely — where the admitted state returns
// max(spec, actuated, allocated). So this is the one state in which reading
// the status charges *less* than reading the spec would.
func ResizeInfeasibleFrom(spec, actuated corev1.ResourceList) PodOption {
	return func(p *corev1.Pod) {
		container := &p.Spec.Containers[0]
		container.Resources.Requests = spec.DeepCopy()

		p.Status.ContainerStatuses = append(p.Status.ContainerStatuses, corev1.ContainerStatus{
			Name:               container.Name,
			Resources:          &corev1.ResourceRequirements{Requests: actuated.DeepCopy()},
			AllocatedResources: actuated.DeepCopy(),
		})
		p.Status.Resources = &corev1.ResourceRequirements{Requests: actuated.DeepCopy()}
		p.Status.AllocatedResources = actuated.DeepCopy()

		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
			Type:   corev1.PodResizePending,
			Status: corev1.ConditionTrue,
			Reason: corev1.PodReasonInfeasible,
		})
	}
}

// Priority sets the pod's resolved priority value.
func Priority(v int32) PodOption {
	return func(p *corev1.Pod) { p.Spec.Priority = &v }
}

// PriorityClass sets the named class.
func PriorityClass(name string) PodOption {
	return func(p *corev1.Pod) { p.Spec.PriorityClassName = name }
}

// Annotated adds an annotation.
func Annotated(key, value string) PodOption {
	return func(p *corev1.Pod) {
		if p.Annotations == nil {
			p.Annotations = map[string]string{}
		}
		p.Annotations[key] = value
	}
}

// PodLabels merges labels onto the pod.
func PodLabels(labels map[string]string) PodOption {
	return func(p *corev1.Pod) {
		if p.Labels == nil {
			p.Labels = map[string]string{}
		}
		maps.Copy(p.Labels, labels)
	}
}

// HostNetwork puts the pod on the host network, which makes its container
// ports host ports implicitly.
func HostNetwork() PodOption {
	return func(p *corev1.Pod) { p.Spec.HostNetwork = true }
}

// ScheduledBy names a non-default scheduler.
func ScheduledBy(name string) PodOption {
	return func(p *corev1.Pod) { p.Spec.SchedulerName = name }
}

// WithResourceClaim requests a device through dynamic resource allocation,
// whose availability is tracked outside the node object.
func WithResourceClaim(name string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.ResourceClaims = append(p.Spec.ResourceClaims, corev1.PodResourceClaim{
			Name:              name,
			ResourceClaimName: new(name),
		})
	}
}

// Gated adds a scheduling gate, which holds the pod unschedulable until
// something removes it.
//
// The status is what the API server writes, not decoration. PrepareForCreate
// stamps {PodScheduled, False, SchedulingGated} on any pod created with a
// non-empty spec.schedulingGates (kubernetes pkg/registry/core/pod/strategy.go,
// applySchedulingGatedCondition), and that condition is the only thing a gated
// pod ever gets: a gated pod never enters the scheduler's active queue — the
// SchedulingGates PreEnqueue plugin holds it in unschedulablePods — so nothing
// ever writes Unschedulable over it. Reading a gated replacement is reading
// that condition, so a fixture that omitted it would agree with code that
// looked for the wrong one.
//
// The node assignment is dropped for the same reason: ValidatePodCreate
// forbids spec.nodeName while any gate remains (pkg/apis/core/validation),
// so a bound gated pod is an object no cluster ever held.
func Gated(name string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.SchedulingGates = append(p.Spec.SchedulingGates,
			corev1.PodSchedulingGate{Name: name})
		p.Spec.NodeName = ""
		p.Status.Phase = corev1.PodPending
		p.Status.Conditions = []corev1.PodCondition{{
			Type:    corev1.PodScheduled,
			Status:  corev1.ConditionFalse,
			Reason:  corev1.PodReasonSchedulingGated,
			Message: "Scheduling is blocked due to non-empty scheduling gates",
		}}
	}
}

// WithRestartAllContainersRule gives the pod's first container a rule that
// restarts every container in the pod when that one exits.
//
// This is the shape that makes a pod's placement depend on what its node can
// do rather than on what it asks for: the scheduler requires the destination
// kubelet to have declared RestartAllContainersOnContainerExits before it will
// place such a pod there.
//
// The two companions of the action are set because the API server requires
// them — a container carrying rules must state its own restartPolicy, and
// every rule must name the exit codes it applies to. A fixture missing either
// is a pod no cluster ever accepted, and one binpack would never meet.
func WithRestartAllContainersRule() PodOption {
	return func(p *corev1.Pod) {
		c := &p.Spec.Containers[0]
		c.RestartPolicy = new(corev1.ContainerRestartPolicyOnFailure)
		c.RestartPolicyRules = append(c.RestartPolicyRules, corev1.ContainerRestartRule{
			Action: corev1.ContainerRestartRuleActionRestartAllContainers,
			ExitCodes: &corev1.ContainerRestartRuleOnExitCodes{
				Operator: corev1.ContainerRestartRuleOnExitCodesOpIn,
				Values:   []int32{42},
			},
		})
	}
}

// InSchedulingGroup puts the pod in a gang, whose group policy decides when
// the whole group may be admitted.
//
// Such a pod is not an independently schedulable object: it waits at the
// scheduler's Permit phase until its group can be placed together, so a
// single-pod placement proved for it means nothing.
func InSchedulingGroup(name string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{PodGroupName: new(name)}
	}
}

// Templates derives a controller template for each pod, matching the pod's own
// spec and labels.
//
// That is the ordinary case and what nearly every test means: a running pod
// looks like what its controller would create again. Tests about the gap
// between the two — a pod resized downward in place, a template pinning
// nodeName — build a divergent template deliberately, which is exactly the
// distinction that makes those tests worth reading.
func Templates(pods ...*corev1.Pod) map[engine.OwnerRef]*corev1.PodTemplateSpec {
	// Only the kinds collect can actually read a template for. A pod owned by
	// an operator's own CRD gets no entry, which is the whole distinction the
	// no-template refusal turns on — building one for every kind would make
	// that case untestable and the fixture a lie.
	//
	// Taken from the engine rather than restated, because a fixture that stops
	// agreeing with the read path does not fail: it goes on producing the
	// refusal the tests were written against, so a kind added to the reader
	// and forgotten here leaves the suite green and the work looking done.
	readable := map[string]bool{}
	for _, kind := range engine.TemplateKinds() {
		readable[kind.Kind] = true
	}

	out := map[engine.OwnerRef]*corev1.PodTemplateSpec{}
	for _, pod := range pods {
		ref, owned := engine.ControllerOf(pod)
		if !owned || !readable[ref.Kind] {
			continue
		}
		spec := *pod.Spec.DeepCopy()
		// A template names no node: its pods are scheduled, not placed.
		spec.NodeName = ""
		out[ref] = &corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: pod.Labels},
			Spec:       spec,
		}
	}
	return out
}

// TemplateFor overrides one pod's template, for the cases where the
// replacement genuinely differs from what is running.
func TemplateFor(
	templates map[engine.OwnerRef]*corev1.PodTemplateSpec,
	pod *corev1.Pod,
	opts ...PodOption,
) {
	ref, owned := engine.ControllerOf(pod)
	if !owned {
		return
	}
	// NodeName is *not* stripped here, unlike in [Templates]: an override says
	// exactly what the template contains, and a template that pins its pods to
	// a node is one of the things worth writing a test about.
	shape := Pod(pod.Namespace, pod.Name, opts...)
	templates[ref] = &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: pod.Labels},
		Spec:       *shape.Spec.DeepCopy(),
	}
}

// WithMemoryBackedEmptyDir attaches a tmpfs scratch volume — an emptyDir whose
// medium is Memory. It looks like local storage and is not: nothing is written
// to the node's disk, so the cluster-autoscaler's isLocalVolume excludes it
// explicitly (cluster-autoscaler utils/drain/drain.go).
//
// Its own archetype rather than an option on WithEmptyDir, because the two
// differ in exactly the way that matters here and a test naming the medium
// inline would say what it sets without saying what it is. This is the volume
// Istio and Linkerd inject into every meshed pod.
func WithMemoryBackedEmptyDir(name string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{
			Name: name,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory},
			},
		})
	}
}

// SafeToEvictLocalVolumes names the volumes whose contents the operator says
// are disposable — the cluster-autoscaler's per-volume escape hatch, and a
// narrower promise than SafeToEvict("true"), which also waives the bare-pod
// and kube-system rules for the same pod.
//
// Variadic and joined here, because the wire format is one comma-separated
// value and a test that spells the commas itself is testing its own string
// building.
func SafeToEvictLocalVolumes(names ...string) PodOption {
	return Annotated("cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes",
		strings.Join(names, ","))
}

// CreatedAt stamps the pod's creation time.
//
// Left unset by the archetypes, which is what an object that has never been
// through an API server looks like — and the autoscaler treats a zero creation
// timestamp as a pod whose age it cannot reason about. So a test about a pod's
// age has to say the age, and one that does not is asking a different
// question.
func CreatedAt(t time.Time) PodOption {
	return func(p *corev1.Pod) { p.CreationTimestamp = metav1.NewTime(t) }
}

// Starting is a pod bound to a node whose containers are not running yet:
// pulling an image, attaching a volume, ContainerCreating.
//
// Distinct from [Pending], which is a pod no node has been chosen for. Both
// are phase Pending and only this one occupies a node, and the distinction is
// load-bearing for eviction: the eviction subresource ignores disruption
// budgets entirely for a pod in that phase, so it is the phase rather than the
// readiness that decides whether the budget is charged.
func Starting() PodOption {
	return func(p *corev1.Pod) {
		p.Status.Phase = corev1.PodPending
		p.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}
	}
}

// Succeeded is a pod that ran to completion — a Job's, most often. It holds no
// resources, and nothing removes it: it sits on its node until somebody or a
// TTL controller deletes it.
func Succeeded() PodOption {
	return terminalPhase(corev1.PodSucceeded)
}

// Failed is a pod that ran and stopped for good, as one the node evicted under
// disk pressure has. Like [Succeeded] it holds no resources and does not leave
// on its own.
func Failed() PodOption {
	return terminalPhase(corev1.PodFailed)
}

func terminalPhase(phase corev1.PodPhase) PodOption {
	return func(p *corev1.Pod) {
		p.Status.Phase = phase
		p.Status.Conditions = []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}
	}
}

// SyncFailed marks a budget whose controller could not compute it, as the
// disruption controller's failSafe leaves one: disruptionsAllowed forced to
// zero and a DisruptionAllowed condition carrying the reason, with every other
// status field left at whatever the last successful sync wrote.
//
// That combination is the point. The stale counters are what make such a
// budget read as an ordinary misconfiguration, and the condition is the only
// field that says otherwise.
func SyncFailed(pdb *policyv1.PodDisruptionBudget, message string) *policyv1.PodDisruptionBudget {
	pdb.Status.DisruptionsAllowed = 0
	pdb.Status.Conditions = []metav1.Condition{{
		Type:    policyv1.DisruptionAllowedCondition,
		Status:  metav1.ConditionFalse,
		Reason:  policyv1.SyncFailedReason,
		Message: message,
	}}
	return pdb
}

// Insufficient gives a budget the DisruptionAllowed condition the disruption
// controller writes when it has computed the budget successfully and the
// answer is zero — the ordinary tight-budget case.
//
// The counterpart to [SyncFailed], and the fixture that keeps the reason
// rather than the mere presence of a condition doing the discriminating.
func Insufficient(pdb *policyv1.PodDisruptionBudget) *policyv1.PodDisruptionBudget {
	pdb.Status.Conditions = []metav1.Condition{{
		Type:   policyv1.DisruptionAllowedCondition,
		Status: metav1.ConditionFalse,
		Reason: policyv1.InsufficientPodsReason,
	}}
	return pdb
}

// UnparseableSelector gives a budget a selector no client can turn into a
// label query, by way of a label value the current API server would reject.
//
// Such objects exist because validation grandfathers them: policy validation
// passes AllowInvalidLabelValueInSelector, so a selector already stored by an
// older API server survives an update.
//
// The sync failure comes with it, because it always does. getPodsForPdb parses
// the same selector the eviction API does, so the disruption controller cannot
// compute such a budget either and failSafe is the only status it ever
// carries — a fixture that set the selector alone would describe an object no
// cluster holds, and let a test claim the selector was doing work the
// condition was.
func UnparseableSelector(pdb *policyv1.PodDisruptionBudget) *policyv1.PodDisruptionBudget {
	pdb.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"app": "Not A Valid Label Value!"},
	}
	// The controller's own message, rather than one invented to look like it.
	// A nil error would mean the value had become parseable and the fixture
	// silently stopped expressing anything, which is worth a panic.
	_, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	if err == nil {
		panic("mother.UnparseableSelector: the selector parses, so the fixture says nothing")
	}
	return SyncFailed(pdb, err.Error())
}
