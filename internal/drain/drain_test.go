package drain_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/motleyhand/binpack/internal/drain"
	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

var now = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

// policy is a resolved policy, every field filled the way the layer above
// fills them: the backoff bounds are the documented defaults, so the table in
// TestBackoffDoublesToACap reads as the shape it is testing rather than as two
// arbitrary durations.
func policy() drain.Policy {
	return drain.Policy{
		StallTimeout:   10 * time.Minute,
		RemovalTimeout: 15 * time.Minute,
		BackoffInitial: 30 * time.Minute,
		BackoffMax:     24 * time.Hour,
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

func TestTheDeletionTimestampIsTheDeadline(t *testing.T) {
	// The API server sets deletionTimestamp to when the grace period *expires*,
	// not to when deletion was requested. Adding the grace period to it doubles
	// every deadline, so a pod wedged on a finalizer with an hour's grace would
	// be reported healthy for a second hour while the node stayed cordoned.
	//
	// An hour's grace, requested 63 minutes ago: three minutes past its
	// deadline, one minute past the slack.
	pod := mother.Pod("default", "a", mother.Terminating(now.Add(-63*time.Minute), time.Hour))

	got := drain.Assess(drain.State{
		Node: draining(time.Minute, "1"), Pods: []*corev1.Pod{pod}, Now: now}, policy())

	if got.Code != drain.AbandonStuck {
		t.Errorf("the pod is 3m past its deadline: got %v/%q (%s)",
			got.Action, got.Code, got.Reason)
	}
}

func TestTheSpecGracePeriodIsNotConsulted(t *testing.T) {
	// deletionGracePeriodSeconds records the period actually applied, which an
	// eviction may shorten — but neither it nor the spec's value is an addend,
	// because the timestamp already accounts for it.
	pod := mother.Pod("default", "a", mother.Terminating(now.Add(-90*time.Minute), time.Hour))
	spec := int64(3600)
	pod.Spec.TerminationGracePeriodSeconds = &spec

	got := drain.Assess(drain.State{
		Node: draining(time.Minute, "1"), Pods: []*corev1.Pod{pod}, Now: now}, policy())

	if got.Code != drain.AbandonStuck {
		t.Errorf("30m past its deadline whatever the spec asks for: got %q (%s)",
			got.Code, got.Reason)
	}
}

func TestANodeSeenEmptyForTheFirstTimeGetsTheFullRemovalWindow(t *testing.T) {
	// The marker records when binpack last looked, not when the node became
	// empty. A controller returning from a twenty-minute outage to find the
	// last pod gone must not uncordon and back off a node the autoscaler has
	// not yet had a chance to remove.
	got := drain.Assess(drain.State{
		Node: draining(20*time.Minute, "3"), Now: now}, policy())

	if got.Action != drain.AwaitRemoval {
		t.Errorf("the node only just became observably empty: got %v/%q (%s)",
			got.Action, got.Code, got.Reason)
	}
	if !got.Progressed {
		t.Error("three pods became zero, which is progress worth recording")
	}
}

func TestBackoffHonoursTheConfiguredInitial(t *testing.T) {
	// The configured value has to reach the arithmetic. Everything above this
	// call already carries it — the schema parses it, the defaults fill it in,
	// validation enforces max >= initial and `config validate` prints it back
	// — so a policy that stops here is a safety control the operator was told
	// they had set.
	node := mother.SmallNode("a")

	_, until := drain.Backoff(node, now, drain.Policy{
		BackoffInitial: 5 * time.Minute,
		BackoffMax:     time.Hour,
	})

	if got := until.Sub(now); got != 5*time.Minute {
		t.Errorf("first wait: got %s, want the configured %s", got, 5*time.Minute)
	}
}

func TestBackoffHonoursTheConfiguredMax(t *testing.T) {
	// The cap is the half a test using the defaults cannot see: every other
	// case here caps at 24 hours, so the constant this replaced agreed with
	// all of them and reverting to it was invisible. Both cases below name a
	// cap the default would get wrong, in opposite directions.
	//
	// The second is the reported one. An operator lengthening the pause on a
	// fragile pool sets backoff.max: 72h, is told it is set, and gets a node
	// retried daily — and a partially drained node is the most attractive
	// candidate in its pool, so each retry evicts a few more pods.
	for _, tc := range []struct {
		name     string
		policy   drain.Policy
		recorded string
		wait     time.Duration
	}{
		{"a cap below the default",
			drain.Policy{BackoffInitial: 5 * time.Minute, BackoffMax: 20 * time.Minute},
			"5", 20 * time.Minute},
		{"a cap above the default",
			drain.Policy{BackoffInitial: 30 * time.Minute, BackoffMax: 72 * time.Hour},
			"9", 72 * time.Hour},
		// Validation refuses a max shorter than the initial, so a resolved
		// policy never carries this pair — but the cap is the bound the caller
		// asked for, and answering with something longer than it because the
		// doubling never ran would be the one direction that costs a node
		// availability it was promised.
		{"a cap shorter than the initial",
			drain.Policy{BackoffInitial: time.Hour, BackoffMax: time.Minute},
			"", time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := mother.SmallNode("a", mother.NodeAnnotations(
				map[string]string{engine.AnnotationDrainAttempts: tc.recorded}))

			_, until := drain.Backoff(node, now, tc.policy)

			if got := until.Sub(now); got != tc.wait {
				t.Errorf("wait: got %s, want the configured %s", got, tc.wait)
			}
		})
	}
}

func TestBackoffDoesNotOverflowAHugeConfiguredBound(t *testing.T) {
	// time.Duration is int64 nanoseconds, so it runs out a little past 292
	// years — and validation bounds these two by "positive" and "max is not
	// shorter than initial", nothing else. A pair well inside what the type
	// can hold can still double past the end of it.
	//
	// The wrap is the worst answer available rather than a merely wrong one:
	// it goes negative, so backoff-until lands in the *past* and the node that
	// just failed is a candidate again on the next evaluation — with fewer
	// pods than before, so the ordering puts it first. That is precisely the
	// thing this function exists to prevent, reached through a configuration
	// binpack accepted and printed back.
	policy := drain.Policy{
		BackoffInitial: 2_000_000 * time.Hour,
		BackoffMax:     2_500_000 * time.Hour,
	}

	// Several counts, because the overflow does not stay put: once wait is
	// negative it is below the cap again, so the loop keeps doubling and where
	// it lands depends on how many attempts are recorded.
	for _, recorded := range []string{"1", "2", "5", "99"} {
		t.Run("after "+recorded, func(t *testing.T) {
			node := mother.SmallNode("a", mother.NodeAnnotations(
				map[string]string{engine.AnnotationDrainAttempts: recorded}))

			_, until := drain.Backoff(node, now, policy)

			if !until.After(now) {
				t.Errorf("backoff-until is %s, which is not after %s: the node whose "+
					"drain just failed is a candidate again immediately", until, now)
			}
			if got := until.Sub(now); got > policy.BackoffMax {
				t.Errorf("wait: got %s, want at most the configured %s",
					got, policy.BackoffMax)
			}
		})
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

			attempts, until := drain.Backoff(node, now, policy())

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

	if _, until := drain.Backoff(node, now, policy()); until.Sub(now) > policy().BackoffMax {
		t.Errorf("backoff grew past the cap: %s", until.Sub(now))
	}
}

func TestWhatADrainInProgressIsToldAboutItself(t *testing.T) {
	// The sentence an operator reads about a drain already under way — logged
	// by a dry run, printed by explain — and the one place either can mislead:
	// every row here is something revalidation or the assessment observed,
	// never a prediction of which ending Advance would reach. Two of the
	// conditions below do not have one ending — a node the autoscaler is
	// already removing is handed over rather than back, and one marked but
	// uncordoned is repaired — so a sentence naming an ending would be wrong
	// for them and right for the rest, which is the worst way to be wrong.
	//
	// Both halves are asked, and the last two rows are why. Revalidation alone
	// cannot tell a drain that is moving from one that has been stuck for an
	// hour: a node whose pods still fit elsewhere is drainable either way.
	stalled := drain.Assessment{Action: drain.Abandon,
		Code: drain.AbandonStalled, Reason: "no pod has left for 11m0s"}
	moving := drain.Assessment{Action: drain.Continue, Remaining: 3}

	for _, tc := range []struct {
		name       string
		assessment engine.NodeAssessment
		bound      drain.Assessment
		want       string
	}{
		{"the cluster moved underneath it",
			engine.NodeAssessment{Skipped: true, SkipReason: "pool pool-4g is at its minimum size (1)"},
			moving, "The cluster has moved underneath it: pool pool-4g is at its minimum size (1)."},
		{"a pod can no longer be evicted",
			engine.NodeAssessment{Blockers: []engine.EvictionBlocker{
				{Message: "default/web is covered by two PodDisruptionBudgets"}}},
			moving, "A pod on it can no longer be evicted: default/web is covered by two " +
				"PodDisruptionBudgets."},
		{"the pods no longer fit",
			engine.NodeAssessment{Simulation: &engine.Simulation{Feasible: false}},
			moving, "The pods still on it no longer fit anywhere else."},
		// The bound, which revalidation does not have — and the row that says a
		// drain nobody is advancing has run past it.
		{"past its bound", engine.NodeAssessment{}, stalled,
			"It has passed its bound and would be handed back: no pod has left for 11m0s (stalled)."},
		{"still going", engine.NodeAssessment{}, moving,
			"It is within its bounds, with 3 pods left to move."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := drain.WouldHappen(tc.assessment, tc.bound); got != tc.want {
				t.Errorf("WouldHappen() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPodsOnKeepsTheTerminatingPods is the claim PodsOn's doc comment makes,
// and it had no test while the function was a private copy in the controller.
//
// A pod on its way out is the pod a drain is waiting for. Filtering it away
// reports an occupied node as empty, which moves the drain to await-removal and
// hands a node with running workload to the cluster-autoscaler. The two
// meanings of "node-local" are the trap: a DaemonSet pod is bound to its node
// by nature and needs no destination, while a terminating pod is bound to it by
// circumstance and is precisely what the wait is about. Assess draws that line
// itself; this must hand it everything so it can.
func TestPodsOnKeepsTheTerminatingPods(t *testing.T) {
	s := engine.Snapshot{Now: now, Pods: []*corev1.Pod{
		mother.Pod("default", "web", mother.OnNode("a")),
		mother.Pod("default", "leaving", mother.OnNode("a"),
			mother.Terminating(now.Add(-time.Minute), 30*time.Second)),
		mother.DaemonSetPod("kube-system", "agent", mother.OnNode("a")),
		mother.Pod("default", "elsewhere", mother.OnNode("b")),
	}}

	var names []string
	for _, pod := range drain.PodsOn(s, "a") {
		names = append(names, pod.Name)
	}

	want := []string{"web", "leaving", "agent"}
	if !slices.Equal(names, want) {
		t.Errorf("PodsOn = %v, want %v", names, want)
	}
}

// TestPolicyForResolvesTheNodesOwnPool: the bounds a drain runs under are the
// pool's, not the cluster's, so reading them from the wrong place would bound a
// drain by a number nobody configured for it.
func TestPolicyForResolvesTheNodesOwnPool(t *testing.T) {
	cfg := engine.Config{
		NodeGroupIDLabel: "doks.digitalocean.com/node-pool-id",
		PoolNameLabel:    "doks.digitalocean.com/node-pool",
		Default:          engine.Policy{StallTimeout: time.Minute, RemovalTimeout: 2 * time.Minute},
		ByPool: map[string]engine.Policy{
			"slow": {StallTimeout: time.Hour, RemovalTimeout: 2 * time.Hour},
		},
	}
	s := engine.Snapshot{Nodes: []*corev1.Node{
		mother.SmallNode("a", mother.InPool("slow", "id-slow")),
		mother.SmallNode("b"),
	}}

	if got := drain.PolicyFor(cfg, s, "a"); got.StallTimeout != time.Hour {
		t.Errorf("stall timeout for a = %s, want the slow pool's 1h", got.StallTimeout)
	}
	if got := drain.PolicyFor(cfg, s, "b"); got.StallTimeout != time.Minute {
		t.Errorf("stall timeout for b = %s, want the default 1m", got.StallTimeout)
	}
	// A node that is not in the snapshot at all — a drain whose node the
	// autoscaler has already removed still needs bounds to be assessed against.
	if got := drain.PolicyFor(cfg, s, "gone"); got.RemovalTimeout != 2*time.Minute {
		t.Errorf("removal timeout for a missing node = %s, want the default 2m", got.RemovalTimeout)
	}
}
