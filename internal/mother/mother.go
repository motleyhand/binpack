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
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		for k, v := range labels {
			n.Labels[k] = v
		}
	}
}

// NodeAnnotations merges annotations onto a node.
func NodeAnnotations(annotations map[string]string) NodeOption {
	return func(n *corev1.Node) {
		if n.Annotations == nil {
			n.Annotations = map[string]string{}
		}
		for k, v := range annotations {
			n.Annotations[k] = v
		}
	}
}

// InPool labels a node with the DOKS pool labels binpack discovers pools by.
func InPool(name, id string) NodeOption {
	return NodeLabels(map[string]string{
		"doks.digitalocean.com/node-pool":    name,
		"doks.digitalocean.com/node-pool-id": id,
	})
}

// Pod is the base archetype: an ordinary Deployment-owned workload with modest
// requests, nothing unusual about it.
func Pod(namespace, name string, opts ...PodOption) *corev1.Pod {
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			OwnerReferences: []metav1.OwnerReference{
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: name + "-rs",
					Controller: ptr(true)},
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
				{APIVersion: "apps/v1", Kind: "DaemonSet", Name: name, Controller: ptr(true)},
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
// An alpha field behind the PodLevelResources gate, and where it is set the
// scheduler reserves it in place of the container aggregate — so a pod whose
// containers ask for almost nothing can still be large.
func WithPodLevelResources(cpu, memory string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.Resources = &corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse(cpu),
				corev1.ResourceMemory: resource.MustParse(memory),
			},
		}
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
// Both fields are set because both are real: deletionGracePeriodSeconds records
// the grace period actually applied, which need not be the one the spec asks
// for.
func Terminating(at time.Time, grace time.Duration) PodOption {
	return func(p *corev1.Pod) {
		stamp := metav1.NewTime(at)
		p.DeletionTimestamp = &stamp
		seconds := int64(grace.Seconds())
		p.DeletionGracePeriodSeconds = &seconds
	}
}

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

// WithRequiredAntiAffinity declares required pod anti-affinity, which is
// symmetric: it can reject an incoming pod as well as constrain this one.
func WithRequiredAntiAffinity(labelKey, labelValue string) PodOption {
	return func(p *corev1.Pod) {
		if p.Spec.Affinity == nil {
			p.Spec.Affinity = &corev1.Affinity{}
		}
		p.Spec.Affinity.PodAntiAffinity = &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey: corev1.LabelHostname,
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
			{APIVersion: "example.com/v1", Kind: kind, Name: name, Controller: ptr(true)},
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

// Resizing marks an in-place vertical scale as in progress.
func Resizing() PodOption {
	return func(p *corev1.Pod) {
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{
			Type:   corev1.PodResizeInProgress,
			Status: corev1.ConditionTrue,
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
		for k, v := range labels {
			p.Labels[k] = v
		}
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

// Gated adds a scheduling gate, which holds the pod unschedulable until
// something removes it.
func Gated(name string) PodOption {
	return func(p *corev1.Pod) {
		p.Spec.SchedulingGates = append(p.Spec.SchedulingGates,
			corev1.PodSchedulingGate{Name: name})
	}
}

func ptr[T any](v T) *T { return &v }

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
	readable := map[string]bool{
		"ReplicaSet": true, "StatefulSet": true, "DaemonSet": true, "Job": true,
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
