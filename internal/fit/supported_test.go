package fit_test

import (
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/component-helpers/nodedeclaredfeatures"
	"k8s.io/component-helpers/nodedeclaredfeatures/features"
	"k8s.io/component-helpers/nodedeclaredfeatures/features/restartallcontainers"

	"github.com/motleyhand/binpack/internal/fit"
	"github.com/motleyhand/binpack/internal/mother"
)

func TestUnsupportedPod(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		// want is a fragment the message must name, because a refusal that
		// does not say which feature caused it is not actionable.
		want string
	}{
		{
			name: "ordinary pod is supported",
			pod:  mother.Pod("default", "web"),
			want: "",
		},
		{
			name: "host port",
			pod:  mother.Pod("default", "ingress", mother.WithHostPort(8080)),
			want: "hostPort 8080",
		},
		{
			// Host networking makes container ports into host ports without a
			// hostPort ever being written down.
			name: "host network",
			pod:  mother.Pod("default", "agent", mother.HostNetwork()),
			want: "host networking",
		},
		{
			name: "persistent volume claim",
			pod:  mother.Pod("default", "db", mother.WithPVC("data")),
			want: "uses volume data",
		},
		{
			// An inline CSI source has no PVC, but its driver can still count
			// against the destination's attachment limit. Enumerating volume
			// types that constrain placement would have missed this; naming
			// the ones that do not cannot.
			name: "inline CSI volume without a PVC",
			pod:  mother.Pod("default", "db", mother.WithInlineCSIVolume("vol", "ebs.csi.aws.com")),
			want: "uses volume vol",
		},
		{
			name: "required pod anti-affinity",
			pod:  mother.Pod("default", "sharded", mother.WithRequiredAntiAffinity("app", "sharded")),
			want: "required pod anti-affinity",
		},
		{
			name: "hard topology spread",
			pod:  mother.Pod("default", "spread", mother.WithHardTopologySpread("topology.kubernetes.io/zone")),
			want: "DoNotSchedule topology spread",
		},
		{
			name: "non-default scheduler",
			pod:  mother.Pod("default", "batch", mother.ScheduledBy("volcano")),
			want: "scheduled by volcano",
		},
		{
			name: "scheduling gates",
			pod:  mother.Pod("default", "waiting", mother.Gated("example.com/hold")),
			want: "scheduling gates",
		},
		{
			// A gang member is not an independently schedulable object: it
			// waits at Permit until the whole group can be placed, so a
			// single-pod placement proved for it says nothing about whether
			// its replacement will ever run.
			name: "scheduling group",
			pod:  mother.Pod("default", "worker", mother.InSchedulingGroup("gang-a")),
			want: "scheduling group gang-a",
		},
		{
			// A resize in flight means the requests are changing underneath
			// us, so anything computed from them is about to be stale.
			name: "in-place resize in progress",
			pod:  mother.Pod("default", "web", mother.Resizing()),
			want: "in-place resize in progress",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := fit.UnsupportedPod(tc.pod)

			if tc.want == "" {
				if !got.Empty() {
					t.Fatalf("expected support, got refusal: %s", got.Message)
				}
				return
			}
			if got.Empty() {
				t.Fatalf("expected a refusal mentioning %q, got none", tc.want)
			}
			if got.Code != fit.ReasonUnsupportedPod {
				t.Errorf("code = %q, want %q", got.Code, fit.ReasonUnsupportedPod)
			}
			if !strings.Contains(got.Message, tc.want) {
				t.Errorf("message should name the feature %q, got: %s", tc.want, got.Message)
			}
		})
	}
}

func TestPlacementNeutralVolumesAreSupported(t *testing.T) {
	// Projections of API objects and node-local scratch exist on any node and
	// need no attachment. Refusing them would refuse most pods in a cluster.
	pod := mother.Pod("default", "web",
		mother.WithEmptyDir("cache"),
		mother.WithConfigMapVolume("settings"),
	)
	if r := fit.UnsupportedPod(pod); !r.Empty() {
		t.Errorf("placement-neutral volumes must not cause a refusal, got: %s", r.Message)
	}
}

func TestSoftConstraintsAreIgnoredNotRefused(t *testing.T) {
	// Soft constraints only affect the scheduler's scoring and can never cause
	// a placement to fail. Refusing on them would make binpack stricter than
	// the scheduler, costing consolidations for no safety gain — the wrong
	// direction of the one-directional guarantee.
	pod := mother.Pod("default", "web")
	pod.Spec.TopologySpreadConstraints = []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       corev1.LabelHostname,
		WhenUnsatisfiable: corev1.ScheduleAnyway,
	}}
	// Both halves, because they are two checks. A preferred-only PodAffinity
	// is what a pod asking to sit near a cache or a shard carries, and a
	// service mesh may inject one; reading it as a required term would make
	// that pod permanently unrelocatable without anything going red.
	pod.Spec.Affinity = &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight: 100,
				PodAffinityTerm: corev1.PodAffinityTerm{
					TopologyKey: corev1.LabelHostname,
					LabelSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"app": "cache"},
					},
				},
			}},
		},
		PodAntiAffinity: &corev1.PodAntiAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
				Weight:          100,
				PodAffinityTerm: corev1.PodAffinityTerm{TopologyKey: corev1.LabelHostname},
			}},
		},
	}

	if r := fit.UnsupportedPod(pod); !r.Empty() {
		t.Errorf("soft constraints must not cause a refusal, got: %s", r.Message)
	}
}

func TestEmptySchedulerNameIsDefault(t *testing.T) {
	// An unset schedulerName means the default scheduler. Treating "" as a
	// foreign scheduler would refuse essentially every pod.
	pod := mother.Pod("default", "web")
	pod.Spec.SchedulerName = ""

	if r := fit.UnsupportedPod(pod); !r.Empty() {
		t.Errorf("an unset schedulerName is the default scheduler, got: %s", r.Message)
	}
}

func TestUnsupportedDestination(t *testing.T) {
	node := mother.SmallNode("node-a")

	t.Run("no residents", func(t *testing.T) {
		if r := fit.UnsupportedDestination(mother.Pod("default", "incoming"), node, nil, nil); !r.Empty() {
			t.Errorf("expected support, got: %s", r.Message)
		}
	})

	t.Run("a term that could match names the offending resident", func(t *testing.T) {
		// Same namespace and a matching label, so the term genuinely applies.
		incoming := mother.Pod("shard", "incoming", mother.PodLabels(map[string]string{"app": "member"}))
		residents := []*corev1.Pod{
			mother.Pod("shard", "ordinary"),
			mother.Pod("shard", "member", mother.WithRequiredAntiAffinity("app", "member")),
		}
		r := fit.UnsupportedDestination(incoming, node, residents, nil)

		if r.Empty() {
			t.Fatal("an anti-affinity term that could match must disqualify the node")
		}
		if !strings.Contains(r.Message, "shard/member") {
			t.Errorf("message should name the offending resident, got: %s", r.Message)
		}
	})

	t.Run("a term in another namespace does not apply", func(t *testing.T) {
		// With no explicit namespaces the term covers only the resident's own,
		// so a pod elsewhere cannot be the one it is guarding against.
		incoming := mother.Pod("default", "incoming", mother.PodLabels(map[string]string{"app": "member"}))
		residents := []*corev1.Pod{
			mother.Pod("shard", "member", mother.WithRequiredAntiAffinity("app", "member")),
		}
		if r := fit.UnsupportedDestination(incoming, node, residents, nil); !r.Empty() {
			t.Errorf("a term scoped to another namespace must not disqualify, got: %s", r.Message)
		}
	})

	t.Run("a term whose selector does not match leaves the node usable", func(t *testing.T) {
		// The selector has to be evaluated, not merely detected. Almost every
		// node in a real cluster hosts some pod with required anti-affinity to
		// its own kind; treating that as disqualifying would leave binpack no
		// destination anywhere, and nothing would go red saying so.
		incoming := mother.Pod("default", "web", mother.PodLabels(map[string]string{"app": "web"}))
		residents := []*corev1.Pod{
			mother.Pod("default", "db-0", mother.PodLabels(map[string]string{"app": "db"}),
				mother.WithRequiredAntiAffinity("app", "db")),
		}
		if r := fit.UnsupportedDestination(incoming, node, residents, nil); !r.Empty() {
			t.Errorf("a term that cannot select the incoming pod must not disqualify, got: %s", r.Message)
		}
	})

	t.Run("resident anti-affinity at zone topology", func(t *testing.T) {
		// Two nodes in one zone. The pod on b keys its anti-affinity on the
		// zone rather than the hostname, so the scheduler refuses every node
		// in z1 — c included, though c holds nothing at all.
		const zone = corev1.LabelTopologyZone
		inZone := mother.NodeLabels(map[string]string{zone: "z1"})
		nodeB, nodeC := mother.SmallNode("b", inZone), mother.SmallNode("c", inZone)
		residentsOfB := []*corev1.Pod{mother.Pod("default", "sitting",
			mother.OnNode("b"), mother.WithRequiredAntiAffinityAt(zone, "app", "web"))}
		var residentsOfC []*corev1.Pod
		incoming := mother.Pod("default", "web", mother.PodLabels(map[string]string{"app": "web"}))
		domains := fit.NewAntiAffinityDomains([]*corev1.Node{nodeB, nodeC}, residentsOfB)

		if r := fit.UnsupportedDestination(incoming, nodeB, residentsOfB, domains); r.Empty() {
			t.Error("the node hosting the declaring pod must be disqualified")
		}
		// The one that was accepted: c holds nothing, so every question asked
		// of c's own residents answers yes.
		r := fit.UnsupportedDestination(incoming, nodeC, residentsOfC, domains)
		if r.Empty() {
			t.Fatal("a node sharing the term's topology domain must be disqualified too")
		}
		if !strings.Contains(r.Message, zone+"=z1") || !strings.Contains(r.Message, "default/sitting") {
			t.Errorf("message should name the domain and the pod that declared the term, got: %s", r.Message)
		}
	})

	t.Run("residents with unsupported but asymmetric features are fine", func(t *testing.T) {
		// A resident's host port or PVC constrains where *it* could go, not
		// where an incoming pod may land. Refusing here would disqualify most
		// nodes in a real cluster for no reason.
		residents := []*corev1.Pod{
			mother.Pod("default", "ingress", mother.WithHostPort(80)),
			mother.Pod("default", "db", mother.WithPVC("data")),
		}
		if r := fit.UnsupportedDestination(mother.Pod("default", "incoming", mother.PodLabels(map[string]string{"app": "member"})), node, residents, nil); !r.Empty() {
			t.Errorf("asymmetric features on residents must not disqualify a node, got: %s", r.Message)
		}
	})
}

// TestDeclaredFeaturesAreDerivedNotListed pins the destination-side half of
// the allowlist: a pod can require something of the *node's kubelet*, and the
// scheduler refuses every node that has not declared it.
//
// The requirement is derived from k8s.io/component-helpers rather than
// recognised from a list here, so the assertions run upstream's inference
// first. A hand-written RestartAllContainers check would satisfy the binpack
// half of this test and go stale the day upstream registers a sixth feature —
// silently, and in the accepting direction.
func TestDeclaredFeaturesAreDerivedNotListed(t *testing.T) {
	pod := mother.Pod("default", "web", mother.WithRestartAllContainersRule())

	// If upstream stops inferring anything for this pod — the feature passing
	// its MaxVersion, the registry losing the entry — everything below would
	// pass while covering nothing. Fail loudly instead.
	reqs, err := nodedeclaredfeatures.DefaultFramework.InferForPodScheduling(
		&nodedeclaredfeatures.PodInfo{Spec: &pod.Spec}, fit.SchedulingTargetVersion())
	if err != nil {
		t.Fatalf("inferring the pod's feature requirements: %v", err)
	}
	if reqs.IsEmpty() {
		t.Fatal("upstream infers no node feature for a RestartAllContainers rule, so this test covers nothing")
	}

	declaring := mother.SmallNode("declaring",
		mother.DeclaringFeature(restartallcontainers.RestartAllContainersOnContainerExits))
	if ok, r := fit.CanFit(pod, declaring, fit.Allocatable(declaring), nil, nil); !ok {
		t.Errorf("a node declaring the feature must be accepted, got: %s", r)
	}

	// The unsound direction. The scheduler's NodeDeclaredFeatures filter
	// rejects this node with UnschedulableAndUnresolvable, so binpack
	// accepting it drains a node whose replacement is then refused by every
	// node whose kubelet is too old — and the autoscaler adds one back.
	silent := mother.SmallNode("silent")
	ok, r := fit.CanFit(pod, silent, fit.Allocatable(silent), nil, nil)
	if ok {
		t.Fatal("a node that has not declared the feature must be refused; the scheduler refuses it")
	}
	if r.Code != fit.ReasonUnsupportedNode {
		t.Errorf("code = %q, want %q", r.Code, fit.ReasonUnsupportedNode)
	}
	if !strings.Contains(r.Message, restartallcontainers.RestartAllContainersOnContainerExits) {
		t.Errorf("the refusal must name the missing feature, got: %s", r.Message)
	}
}

// TestTheSchedulingTargetVersionDropsNoFeature guards the one number the
// derivation still needs.
//
// InferForPodScheduling drops any feature whose MaxVersion the target version
// has passed, so a target that is too high infers fewer requirements than the
// scheduler does — the accepting direction, and the one binpack may not err
// in. Every registered feature is unbounded today, which is what makes the
// current value safe; this fails on the release that changes that, rather
// than after it has quietly widened what binpack accepts.
func TestTheSchedulingTargetVersionDropsNoFeature(t *testing.T) {
	for _, f := range features.AllFeatures {
		bound := f.MaxVersion()
		if bound == nil {
			continue
		}
		if fit.SchedulingTargetVersion().GreaterThan(bound) {
			t.Errorf("feature %s is bounded at %s, which the scheduling target version %s has passed: "+
				"binpack would stop requiring it while a scheduler on an older release still does",
				f.Name(), bound, fit.SchedulingTargetVersion())
		}
	}
}

// modelled names each PodSpec field internal/fit reads, refuses on, or
// otherwise accounts for, and says where.
//
// Together with irrelevantToScheduling it is the allowlist stated over the
// type rather than over the fields somebody remembered — see
// TestPodSpecFieldsAreAccountedFor.
var modelled = map[string]string{
	"Volumes":                   "firstConstrainingVolume refuses every volume it cannot prove node-independent",
	"InitContainers":            "EffectiveRequests takes their peak; firstHostPort and the declared-feature inference read them",
	"Containers":                "EffectiveRequests sums them; firstHostPort and the declared-feature inference read them",
	"NodeSelector":              "CanFit evaluates it through GetRequiredNodeAffinity, which covers selector and affinity alike",
	"NodeName":                  "UnsupportedPod refuses a template that pins one, since such a pod bypasses the scheduler",
	"HostNetwork":               "UnsupportedPod refuses it: container ports become host ports with none written down",
	"HostUsers":                 "with hostNetwork it implies a declared node feature, and hostNetwork itself is already refused",
	"Affinity":                  "UnsupportedPod refuses required pod (anti-)affinity; UnsupportedDestination reads other pods' required anti-affinity, by topology domain; CanFit evaluates required node affinity",
	"SchedulerName":             "UnsupportedPod refuses any scheduler but the default, whose rules are the only ones binpack models",
	"Tolerations":               "CanFit matches them against the node's taints, at the scheduler's own gate setting",
	"Overhead":                  "EffectiveRequests includes it, as the scheduler reserves it",
	"TopologySpreadConstraints": "UnsupportedPod refuses the DoNotSchedule variant; ScheduleAnyway only scores",
	"SchedulingGates":           "UnsupportedPod refuses a gated pod, which nothing may schedule until the gate goes",
	"ResourceClaims":            "UnsupportedPod refuses dynamic resource allocation, tracked outside the node object",
	"SchedulingGroup":           "UnsupportedPod refuses a pod in a gang, which is not an independently schedulable object",
	"Resources":                 "EffectiveRequests reads pod-level requests through PodRequests, which prefers them to the container sum",
	"RuntimeClassName":          "its scheduling effect reaches fit through other fields: admission merges the class's nodeSelector and tolerations into the pod, and EffectiveRequests carries its overhead",
}

// irrelevantToScheduling names each PodSpec field no scheduler Filter plugin
// reads, with the reason. A wrong answer about one of these cannot make a
// placement fail, which is the only property that matters here.
var irrelevantToScheduling = map[string]string{
	"EphemeralContainers":           "attached to a running pod, absent from every controller template, and the scheduler reserves nothing for them",
	"RestartPolicy":                 "decides what the kubelet does after a container exits, not where the pod goes",
	"TerminationGracePeriodSeconds": "how long a shutdown may take; internal/drain reads it, no filter does",
	"ActiveDeadlineSeconds":         "kills a running pod, and constrains nothing about its placement",
	"DNSPolicy":                     "name resolution inside the pod",
	"DNSConfig":                     "name resolution inside the pod",
	"ServiceAccountName":            "identity; the token it projects appears in Volumes, which is checked",
	"DeprecatedServiceAccount":      "the superseded spelling of ServiceAccountName",
	"AutomountServiceAccountToken":  "whether the token is projected at all",
	"ImagePullSecrets":              "registry credentials, which every node can use equally",
	"HostPID":                       "namespace sharing; admission may forbid it, no filter places on it",
	"HostIPC":                       "namespace sharing; admission may forbid it, no filter places on it",
	"ShareProcessNamespace":         "namespace sharing between the pod's own containers",
	"SecurityContext":               "runtime privileges, settled at admission. Kubelet admission can still reject a pod over supplementalGroupsPolicy, which is node capability rather than scheduling and is outside this package",
	"Hostname":                      "the pod's network identity",
	"Subdomain":                     "the pod's network identity",
	"SetHostnameAsFQDN":             "the pod's network identity",
	"HostnameOverride":              "the pod's network identity",
	"HostAliases":                   "extra /etc/hosts entries",
	"PriorityClassName":             "the name Priority is resolved from",
	"Priority":                      "queue order and preemption, not filtering; internal/engine reads it for the autoscaler's expendable cutoff",
	"PreemptionPolicy":              "whether this pod preempts others, which can only widen where the scheduler could place it",
	"ReadinessGates":                "readiness after placement",
	"EnableServiceLinks":            "environment variables injected into containers",
	"OS":                            "the operating system the pod expects; no default Filter plugin reads it, and the kubernetes.io/os node selector that conventionally accompanies it is evaluated by CanFit",
}

// TestPodSpecFieldsAreAccountedFor states the allowlist over corev1.PodSpec
// itself, so a field Kubernetes adds fails CI naming itself.
//
// This is the difference between the allowlist ADR-0006 describes and the one
// the code had. UnsupportedPod is a sequence of checks ending in "no
// objection", so every field nobody thought of was accepted — and 1.36 added
// spec.schedulingGroup, which holds a pod at Permit until its whole gang can
// be placed. The cost of keeping this honest is two tables of strings, read
// once per dependency bump; the cost of not doing it is one silent acceptance
// per release that happens to add a scheduling-relevant field.
func TestPodSpecFieldsAreAccountedFor(t *testing.T) {
	spec := reflect.TypeOf(corev1.PodSpec{})

	for i := range spec.NumField() {
		name := spec.Field(i).Name
		_, known := modelled[name]
		_, inert := irrelevantToScheduling[name]

		switch {
		case known && inert:
			t.Errorf("PodSpec.%s is in both tables; it is either modelled or it is not", name)
		case !known && !inert:
			t.Errorf("PodSpec.%s is in neither table. Kubernetes has added a field to the pod spec: "+
				"decide whether internal/fit must account for it and say so in modelled, or "+
				"why no scheduler filter reads it and say so in irrelevantToScheduling", name)
		}
	}

	// The other direction, so the tables cannot outlive the type they
	// describe: a reason for a field that is gone reads as coverage.
	for _, table := range []map[string]string{modelled, irrelevantToScheduling} {
		for name := range table {
			if _, ok := spec.FieldByName(name); !ok {
				t.Errorf("%q is named in a table but is no longer a PodSpec field", name)
			}
		}
	}
}
