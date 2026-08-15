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
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
				{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: name + "-rs"},
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
				{APIVersion: "apps/v1", Kind: "DaemonSet", Name: name},
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
func PDB(namespace, name string, disruptionsAllowed int32, selector map[string]string) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: selector},
		},
		Status: policyv1.PodDisruptionBudgetStatus{DisruptionsAllowed: disruptionsAllowed},
	}
}

// Bare removes the controller reference, making the pod one nothing would
// recreate after eviction.
func Bare() PodOption {
	return func(p *corev1.Pod) { p.OwnerReferences = nil }
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
