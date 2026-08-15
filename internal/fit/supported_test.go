package fit_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

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
	pod.Spec.Affinity = &corev1.Affinity{
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
		if r := fit.UnsupportedDestination(mother.Pod("default", "incoming"), node, nil); !r.Empty() {
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
		r := fit.UnsupportedDestination(incoming, node, residents)

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
		if r := fit.UnsupportedDestination(incoming, node, residents); !r.Empty() {
			t.Errorf("a term scoped to another namespace must not disqualify, got: %s", r.Message)
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
		if r := fit.UnsupportedDestination(mother.Pod("default", "incoming", mother.PodLabels(map[string]string{"app": "member"})), node, residents); !r.Empty() {
			t.Errorf("asymmetric features on residents must not disqualify a node, got: %s", r.Message)
		}
	})
}
