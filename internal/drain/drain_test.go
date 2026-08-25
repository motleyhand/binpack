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
	"github.com/motleyhand/binpack/internal/permute"
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
			// Exactly at the bound, which is the one instant the operator
			// configured. The comparison is strict, so the timeout is the
			// last moment a drain is still allowed to be quiet rather than
			// the first moment it is not.
			name: "exactly at stallTimeout is not yet a stall",
			node: draining(10*time.Minute, "2"),
			pods: []*corev1.Pod{mother.Pod("default", "a"), mother.Pod("default", "b")},
			want: drain.Continue, remaining: 2,
		},
		{
			// The same reading of the same kind of bound, on the branch that
			// hands the node back to the autoscaler rather than to the
			// scheduler.
			name: "exactly at removalTimeout is not yet not-removed",
			node: draining(15*time.Minute, "0"),
			want: drain.AwaitRemoval, remaining: 0,
		},
		{
			// The seam between "shutting down" and "stuck", and the two are
			// exact complements: a pod at its deadline plus slack is on the
			// shutting-down side, so the drain goes on waiting and counts the
			// wait as progress. One instant later it is a finalizer problem
			// and the drain ends. Nothing else says which side owns the
			// boundary.
			name: "exactly at the deadline plus slack is still shutting down",
			node: draining(time.Minute, "1"),
			pods: []*corev1.Pod{mother.Pod("default", "a",
				mother.Terminating(now.Add(-7*time.Minute), 5*time.Minute))},
			want: drain.Continue, remaining: 1, progressed: true,
		},
		{
			// A finished Job pod is a real object that nothing removes on its
			// own, so counting it would stall every drain of the node it
			// landed on — the mirror image of the DaemonSet rows above, which
			// are excluded for the opposite reason.
			name: "a finished Job pod does not keep the node occupied",
			node: draining(5*time.Minute, "1"),
			pods: []*corev1.Pod{mother.Pod("batch", "backup-28471", mother.Succeeded())},
			want: drain.AwaitRemoval, remaining: 0, progressed: true,
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

func TestTheWorstStuckPodIsNamed(t *testing.T) {
	// The reason string is this package's entire product, and a drain wedges
	// with several pods terminating: the executor evicts sequentially, so
	// earlier evictions are still on their way out when a later one sticks.
	// Naming the pod that is a minute over while another is three hours over
	// sends the operator to the wrong workload and understates the overrun by
	// the whole difference.
	//
	// Run both orderings, because "the worst" and "the last" agree on
	// whichever ordering happens to put the worst last.
	worst := mother.Pod("monitoring", "prometheus-0",
		mother.Terminating(now.Add(-3*time.Hour), time.Minute))
	milder := mother.Pod("default", "a",
		mother.Terminating(now.Add(-90*time.Minute), time.Minute))

	for _, tc := range []struct {
		name string
		pods []*corev1.Pod
	}{
		{"worst first", []*corev1.Pod{worst, milder}},
		{"worst last", []*corev1.Pod{milder, worst}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := drain.Assess(
				drain.State{Node: draining(time.Minute, "2"), Pods: tc.pods, Now: now}, policy())

			if got.Action != drain.Abandon || got.Code != drain.AbandonStuck {
				t.Fatalf("two pods past their deadlines is a stuck drain, got %+v", got)
			}
			// 3h less the one-minute grace the deadline already includes, less
			// the two minutes of slack.
			if !strings.Contains(got.Reason, "monitoring/prometheus-0 is 2h57m") {
				t.Errorf("reason names the wrong pod or the wrong overrun: %q", got.Reason)
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
		Default: engine.Policy{
			StallTimeout: time.Minute, RemovalTimeout: 2 * time.Minute,
			CooldownAfterScaleUp: 3 * time.Minute,
		},
		ByPool: map[string]engine.Policy{
			"slow": {
				StallTimeout: time.Hour, RemovalTimeout: 2 * time.Hour,
				CooldownAfterScaleUp: 30 * time.Minute,
			},
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
	// The autoscaler's post-growth pause travels with the rest, from the one
	// setting that describes it. An operator whose autoscaler holds scale-down
	// for half an hour configures that once, and both the refusal to start a
	// drain and the refusal to give up on one read the same number.
	if got := drain.PolicyFor(cfg, s, "a"); got.ScaleUpPause != 30*time.Minute {
		t.Errorf("scale-up pause for a = %s, want the slow pool's 30m", got.ScaleUpPause)
	}
	if got := drain.PolicyFor(cfg, s, "b"); got.ScaleUpPause != 3*time.Minute {
		t.Errorf("scale-up pause for b = %s, want the default 3m", got.ScaleUpPause)
	}
}

// TestAssessRemovalWaitsThroughScaleUpDelay covers the one bound whose
// deadline belongs to a component binpack does not control.
//
// The cluster-autoscaler suppresses all scale-down for
// scale-down-delay-after-add following any scale-up anywhere in the cluster,
// so a scale-up landing late in an emptied node's wait holds the removal past
// removalTimeout however that number is sized. Abandoning there uncordons a
// node the autoscaler was about to delete: every pod moved for nothing, the
// consolidation lost, and backoff filed against a node with no problem of its
// own.
//
// ADR-0007's shape, applied to the reaping phase. A scale-up is the cluster
// saying the removal will be slow, and reading that as "stop" is the same
// mistake as reading a one-hour termination grace period as a wedged pod.
func TestAssessRemovalWaitsThroughScaleUpDelay(t *testing.T) {
	p := policy()
	p.ScaleUpPause = 10 * time.Minute

	for _, tc := range []struct {
		name        string
		lastScaleUp time.Time
		inProgress  bool
		want        drain.Action
		code        string
	}{
		{
			// The finding: 16 minutes empty is past the bound, and the
			// autoscaler's own gate is still shut for another eight.
			name:        "a scale-up inside the pause defers the abandonment",
			lastScaleUp: now.Add(-2 * time.Minute),
			want:        drain.AwaitRemoval,
		},
		{
			// The control, and the half that keeps the wait terminating: with
			// nothing holding the autoscaler back, an empty node it has not
			// removed is handed back on the bound.
			name: "no scale-up, so the bound still applies",
			want: drain.Abandon, code: drain.AbandonNotRemoved,
		},
		{
			// The pause defers; it does not restart the clock. Once the
			// autoscaler's gate opens the node has still been empty for
			// sixteen minutes, and the very next evaluation says so — which is
			// what stops a cluster that keeps growing from holding a cordoned
			// node indefinitely.
			name:        "a scale-up older than the pause defers nothing",
			lastScaleUp: now.Add(-11 * time.Minute),
			want:        drain.Abandon, code: drain.AbandonNotRemoved,
		},
		{
			// The two fields describe one episode from different ends, and the
			// timestamp is the end that does not move: it is stamped when the
			// scale-up began, so a slow one ages past the pause while it is
			// still going on. Reading only the timestamp would expire the
			// removal wait in the middle of the growth it exists to wait out —
			// and hand back an emptied node, with backoff, at the moment the
			// cluster is least able to absorb the churn.
			name:        "a scale-up still in progress defers however old the stamp is",
			lastScaleUp: now.Add(-11 * time.Minute), inProgress: true,
			want: drain.AwaitRemoval,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := drain.Assess(drain.State{
				Node: draining(16*time.Minute, "0"), Now: now,
				LastScaleUp: tc.lastScaleUp, ScaleUpInProgress: tc.inProgress,
			}, p)
			if got.Action != tc.want || got.Code != tc.code {
				t.Errorf("Assess() = %v/%q, want %v/%q",
					got.Action, got.Code, tc.want, tc.code)
			}
			// Never progress. Recording one would restamp the marker every
			// evaluation the pause lasted, so the removal clock would restart
			// from zero when it lifted rather than expiring at once — the
			// self-emitted keep-alive ADR-0007 withdrew its fourth progress
			// signal over, arrived at by a different route.
			if got.Progressed {
				t.Error("a scale-up is not progress on this node")
			}
		})
	}
}

func TestTheStuckPodIsChosenByATotalOrder(t *testing.T) {
	// A drain evicts a batch of pods belonging to one workload, and those pods
	// share terminationGracePeriodSeconds — so the API server writes them
	// identical deletion timestamps and their overruns are equal to the
	// second. A tie here is the normal case, not a corner of one.
	//
	// Ranking on the overrun alone therefore named whichever pod the input
	// slice happened to list first, and that slice is filled from a
	// watch-backed cache in the controller and from a live client in
	// `binpack explain`. The two frontends then named different pods as the
	// reason for the same abandonment.
	requested := now.Add(-2 * time.Hour)
	pods := []*corev1.Pod{
		mother.Pod("default", "web-1", mother.OnNode("a"),
			mother.Terminating(requested, time.Minute)),
		mother.Pod("default", "web-2", mother.OnNode("a"),
			mother.Terminating(requested, time.Minute)),
		mother.Pod("default", "web-3", mother.OnNode("a"),
			mother.Terminating(requested, time.Minute)),
	}
	node := mother.SmallNode("a")

	permute.Stable(t, pods, func(pods []*corev1.Pod) string {
		got := drain.Assess(drain.State{Node: node, Pods: pods, Now: now}, policy())
		if got.Code != drain.AbandonStuck {
			t.Fatalf("expected a stuck pod, got %s: %s", got.Code, got.Reason)
		}
		return got.Reason
	})
}
