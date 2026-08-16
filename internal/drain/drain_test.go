package drain_test

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/motleyhand/binpack/internal/drain"
	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

var now = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

func policy() drain.Policy {
	return drain.Policy{
		StallTimeout:   10 * time.Minute,
		RemovalTimeout: 15 * time.Minute,
	}
}

// draining builds a node carrying drain markers, with progress last seen `ago`
// and `remaining` pods recorded at that point.
func draining(ago time.Duration, remaining string) *corev1.Node {
	return mother.SmallNode("a", mother.Cordoned(), mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainStarted:       now.Add(-time.Hour).Format(time.RFC3339),
		engine.AnnotationDrainProgress:      now.Add(-ago).Format(time.RFC3339),
		engine.AnnotationDrainPodsRemaining: remaining,
	}))
}

func TestAssess(t *testing.T) {
	for _, tc := range []struct {
		name string
		node *corev1.Node
		pods []*corev1.Pod

		want       drain.Action
		code       string
		remaining  int
		progressed bool
		// mentions is a substring the reason must contain, so an operator
		// gets the specific thing rather than a category.
		mentions string
	}{
		{
			name: "pods remain and the count has just dropped",
			node: draining(9*time.Minute, "3"),
			pods: []*corev1.Pod{mother.Pod("default", "a"), mother.Pod("default", "b")},
			want: drain.Continue, remaining: 2, progressed: true,
		},
		{
			// The whole point of bounding progress rather than elapsed time.
			// A pod with an hour's grace period is behaving correctly by
			// taking 45 minutes, and no configuration should be needed to
			// tolerate it.
			name: "a long graceful shutdown is progress, not a stall",
			node: draining(40*time.Minute, "1"),
			pods: []*corev1.Pod{mother.Pod("default", "a",
				mother.Terminating(now.Add(-40*time.Minute), time.Hour))},
			want: drain.Continue, remaining: 1, progressed: true,
		},
		{
			// Past its deadline plus slack: the kubelet should have sent
			// SIGKILL. Something is holding it — a finalizer, a volume that
			// will not detach — and saying which pod is far more use than
			// "the drain timed out".
			name: "a pod past its termination deadline is stuck, and is named",
			node: draining(time.Minute, "1"),
			pods: []*corev1.Pod{mother.Pod("monitoring", "prometheus-0",
				mother.Terminating(now.Add(-20*time.Minute), 5*time.Minute))},
			want: drain.Abandon, code: drain.AbandonStuck, remaining: 1,
			mentions: "monitoring/prometheus-0 is 13m past its termination deadline",
		},
		{
			name: "nothing moving for longer than stallTimeout",
			node: draining(11*time.Minute, "2"),
			pods: []*corev1.Pod{mother.Pod("default", "a"), mother.Pod("default", "b")},
			want: drain.Abandon, code: drain.AbandonStalled, remaining: 2,
			mentions: "no progress for 11m",
		},
		{
			name: "nothing moving, but not yet for long enough",
			node: draining(9*time.Minute, "2"),
			pods: []*corev1.Pod{mother.Pod("default", "a"), mother.Pod("default", "b")},
			want: drain.Continue, remaining: 2,
		},
		{
			name: "empty, and the autoscaler still has time",
			node: draining(5*time.Minute, "1"),
			want: drain.AwaitRemoval, remaining: 0, progressed: true,
		},
		{
			// A different bound from the stall timeout, because it is a
			// different question. Conflating them is what hid this one.
			name: "empty for longer than removalTimeout",
			node: draining(16*time.Minute, "0"),
			want: drain.Abandon, code: drain.AbandonNotRemoved,
			mentions: "the cluster-autoscaler has not removed it",
		},
		{
			// Evicting these livelocks the drain: the DaemonSet controller
			// recreates the pod on the same still-existing node. Counting them
			// means the drain can never reach zero.
			name: "DaemonSet and mirror pods are not this drain's business",
			node: draining(5*time.Minute, "1"),
			pods: []*corev1.Pod{
				mother.DaemonSetPod("kube-system", "cilium-abc"),
				mother.MirrorPod("kube-system", "kube-proxy-a"),
			},
			want: drain.AwaitRemoval, remaining: 0, progressed: true,
		},
		{
			// And a wedged DaemonSet pod is some other rollout's problem. It
			// would otherwise abandon every drain on the node.
			name: "a stuck DaemonSet pod does not abandon the drain",
			node: draining(time.Minute, "0"),
			pods: []*corev1.Pod{mother.DaemonSetPod("kube-system", "cilium-abc",
				mother.Terminating(now.Add(-time.Hour), time.Minute))},
			want: drain.AwaitRemoval, remaining: 0,
		},
		{
			// Expendable pods are evicted without needing to fit anywhere, so
			// they still have to leave before the node is empty.
			name: "expendable pods still count as remaining",
			node: draining(time.Minute, "1"),
			pods: []*corev1.Pod{mother.Pod("default", "batch", mother.Priority(-100))},
			want: drain.Continue, remaining: 1,
		},
		{
			// A marker nobody can read is a reason to write a fresh one, not
			// to destroy a drain that may be perfectly healthy.
			name: "unreadable markers do not abandon a drain",
			node: mother.SmallNode("a", mother.Cordoned(), mother.NodeAnnotations(
				map[string]string{engine.AnnotationDrainStarted: "not a timestamp"})),
			pods: []*corev1.Pod{mother.Pod("default", "a")},
			want: drain.Continue, remaining: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := drain.Assess(
				drain.State{Node: tc.node, Pods: tc.pods, Now: now}, policy())

			if got.Action != tc.want {
				t.Errorf("action: got %v, want %v (%s)", got.Action, tc.want, got.Reason)
			}
			if got.Code != tc.code {
				t.Errorf("code: got %q, want %q", got.Code, tc.code)
			}
			if got.Remaining != tc.remaining {
				t.Errorf("remaining: got %d, want %d", got.Remaining, tc.remaining)
			}
			if got.Progressed != tc.progressed {
				t.Errorf("progressed: got %v, want %v", got.Progressed, tc.progressed)
			}
			if tc.mentions != "" && !strings.Contains(got.Reason, tc.mentions) {
				t.Errorf("reason: got %q, want it to mention %q", got.Reason, tc.mentions)
			}
		})
	}
}

func TestTheAppliedGracePeriodWinsOverTheRequestedOne(t *testing.T) {
	// An eviction may be granted a different grace period from the one the
	// spec asks for, and the API server records what was actually applied.
	// Judging against the spec would call a pod stuck 55 minutes early.
	pod := mother.Pod("default", "a", mother.Terminating(now.Add(-10*time.Minute), time.Hour))
	spec := int64(60)
	pod.Spec.TerminationGracePeriodSeconds = &spec

	got := drain.Assess(drain.State{
		Node: draining(10*time.Minute, "1"), Pods: []*corev1.Pod{pod}, Now: now}, policy())

	if got.Action != drain.Continue {
		t.Errorf("the pod is 10m into an hour of grace: got %v (%s)", got.Action, got.Reason)
	}
}

func TestBackoffDoublesToACap(t *testing.T) {
	// Without this the candidate ordering actively prefers nodes that just
	// failed: abandoning uncordons, and a partially drained node has fewer
	// pods, so least-loaded-first puts it back at the front.
	for _, tc := range []struct {
		recorded string
		attempts int
		wait     time.Duration
	}{
		{"", 1, 30 * time.Minute},
		{"1", 2, time.Hour},
		{"2", 3, 2 * time.Hour},
		{"5", 6, 16 * time.Hour},
		{"6", 7, 24 * time.Hour},
		{"99", 100, 24 * time.Hour},
		// Anything unreadable errs towards retrying sooner: the annotation is
		// writable by anyone with node access.
		{"garbage", 1, 30 * time.Minute},
		{"-3", 1, 30 * time.Minute},
	} {
		t.Run("after "+tc.recorded, func(t *testing.T) {
			node := mother.SmallNode("a", mother.NodeAnnotations(
				map[string]string{engine.AnnotationDrainAttempts: tc.recorded}))

			attempts, until := drain.Backoff(node, now)

			if attempts != tc.attempts {
				t.Errorf("attempts: got %d, want %d", attempts, tc.attempts)
			}
			if got := until.Sub(now); got != tc.wait {
				t.Errorf("wait: got %s, want %s", got, tc.wait)
			}
		})
	}
}

func TestBackoffIsNeverPermanent(t *testing.T) {
	// A permanent skip after N attempts would need a human to clear an
	// annotation, which contradicts leaving the cluster working without
	// intervention — and would strand a node blocked by something transient.
	node := mother.SmallNode("a", mother.NodeAnnotations(
		map[string]string{engine.AnnotationDrainAttempts: "1000"}))

	if _, until := drain.Backoff(node, now); until.Sub(now) > drain.BackoffMax {
		t.Errorf("backoff grew past the cap: %s", until.Sub(now))
	}
}
