// Package drain judges whether a drain in progress is alive, finished or lost.
//
// It answers one question — given a node carrying drain markers and the pods
// still on it, what should happen next — and answers it as a pure function so
// the judgement can be stated as a table. Carrying the answer out is the
// executor's job.
//
// The bound is on the *absence of progress* rather than on elapsed time,
// because a wall-clock deadline cannot tell a pod with a one-hour
// terminationGracePeriodSeconds from one wedged on a finalizer: any single
// timeout is too short for the first and too long for the second. See
// ADR-0007.
//
// Objects passed in come from a shared informer cache and are read-only.
package drain

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/motleyhand/binpack/internal/engine"
)

// Slack is how long past a pod's termination deadline binpack waits before
// calling it stuck rather than slow.
//
// Enough for SIGKILL and for the API server to catch up, short enough that a
// genuinely wedged pod is not mistaken for a slow one. Deliberately not
// configurable: it describes how Kubernetes behaves, not a preference.
const Slack = 2 * time.Minute

// State is everything the judgement reads.
type State struct {
	// Node carries the drain markers. Read-only.
	Node *corev1.Node
	// Pods are the pods currently on that node, terminating ones included —
	// a pod on its way out is still occupying it.
	Pods []*corev1.Pod
	Now  time.Time
}

// Policy is the part of a resolved pool policy this package needs.
//
// No expendable cutoff: the bounds here ask whether a drain is moving, and a
// pod below the cutoff still has to leave the node before it is empty. What a
// pod's priority changes is whether it needed a destination, which was settled
// before the drain started.
//
// The two backoff durations are read only by [Backoff], and only once a drain
// has been given up on. They travel with the timeouts because they are
// resolved from the same per-pool policy and the alternative is resolving
// pools twice: the value that says how long to wait would then be free to
// disagree with the value that decided the wait was owed.
//
// All four are taken as resolved. Nothing here defaults a zero, for the same
// reason [Assess] does not: a caller that built this by hand has skipped the
// layer that fills values in, and quietly supplying one is how a configured
// setting comes to differ from the setting that is applied.
type Policy struct {
	StallTimeout   time.Duration
	RemovalTimeout time.Duration
	BackoffInitial time.Duration
	BackoffMax     time.Duration
}

// Action is what to do with a drain that is already under way.
type Action int

const (
	// Continue: pods remain and the drain is alive. Evict the next one when
	// nothing is in flight.
	Continue Action = iota

	// AwaitRemoval: nothing left to evict. The node is now the
	// cluster-autoscaler's to delete.
	AwaitRemoval

	// Abandon: give up. Uncordon, clear the markers, record backoff. Code and
	// Reason say why.
	Abandon
)

func (a Action) String() string {
	switch a {
	case AwaitRemoval:
		return "await-removal"
	case Abandon:
		return "abandon"
	default:
		return "continue"
	}
}

// Codes naming why a drain was abandoned. Stable, like the diagnostic codes
// and the metric names: this is what an alert keys on.
const (
	// AbandonStuck: a pod is past its termination deadline. The kubelet should
	// have sent SIGKILL by now, so this is a finalizer, a volume that will not
	// detach, or an unhealthy kubelet.
	AbandonStuck = "stuck"

	// AbandonStalled: nothing has moved for stallTimeout and nothing is
	// shutting down. Distinct from stuck, which is positively detected and far
	// more useful to report.
	AbandonStalled = "stalled"

	// AbandonNotRemoved: the node is empty but the autoscaler has not deleted
	// it. Uncordoning matters more than usual here — an empty cordoned node is
	// pure waste.
	AbandonNotRemoved = "not-removed"

	// AbandonUnschedulable: a pod that moved off the node could not be placed.
	// The simulation proved a valid assignment existed; the scheduler is not
	// obliged to choose that one, and this is what it looks like when it chose
	// differently. Decided by the executor, which can see the replacement,
	// but named here so every reason a drain ends is in one list.
	AbandonUnschedulable = "replacement-unschedulable"

	// AbandonUnaccounted: pods remain that the simulation did not name. What
	// binpack understands is an allowlist, so an unrecognised set is a reason
	// to hand the node back rather than to guess.
	AbandonUnaccounted = "unaccounted-pods"
)

// AbandonCodes is every reason a drain can end that binpack decides for
// itself. The engine's skip codes can also end one, when the cluster changes
// underneath it.
func AbandonCodes() []string {
	return []string{
		AbandonStuck, AbandonStalled, AbandonNotRemoved,
		AbandonUnschedulable, AbandonUnaccounted,
	}
}

// PodsOn is the pods a drain assessment reads. Terminating ones included: a pod
// on its way out is still occupying the node.
//
// The same filter the executor applies before its own [Assess] call, and it has
// to stay the same — which is why there is one of it rather than one per
// caller. What it feeds tells an operator what acting would do, so a pod set
// that differed from the acting path's would describe a drain nobody would get.
func PodsOn(s engine.Snapshot, name string) []*corev1.Pod {
	var on []*corev1.Pod
	for _, pod := range s.Pods {
		if pod.Spec.NodeName == name {
			on = append(on, pod)
		}
	}
	return on
}

// PolicyFor resolves the bounds that govern the drain of one named node, from
// that node's own pool.
func PolicyFor(cfg engine.Config, s engine.Snapshot, name string) Policy {
	for _, node := range s.Nodes {
		if node.Name != name {
			continue
		}
		p := cfg.PolicyFor(node.Labels[cfg.NodeGroupIDLabel], node.Labels[cfg.PoolNameLabel])
		return policyFrom(p)
	}
	return policyFrom(cfg.Default)
}

// policyFrom narrows a resolved engine policy to the part this package reads.
//
// One conversion rather than one per branch above: the fall-through and the
// matched node have to agree, and two literals listing the same fields is an
// invariant with nobody enforcing it.
func policyFrom(p engine.Policy) Policy {
	return Policy{
		StallTimeout:   p.StallTimeout,
		RemovalTimeout: p.RemovalTimeout,
		BackoffInitial: p.BackoffInitial,
		BackoffMax:     p.BackoffMax,
	}
}

// WouldHappen describes a drain already under way in one sentence.
//
// Both halves, because neither answers alone. Revalidation says whether this
// node could still be emptied; the assessment says whether this drain is
// getting anywhere. A node whose pods still fit elsewhere revalidates drainable
// however long it has been stuck — so reporting only the verdict tells an
// operator a stalled drain is healthy, in the one situation they are most
// likely to be asking.
//
// What the two observed, though, and not a rehearsal of [executor.Advance].
// Predicting the ending would mean a second copy of that function's precedence
// living here, and most of the conditions below do not have one ending: a node
// the autoscaler is already removing is handed over rather than back, and one
// marked but uncordoned is repaired. A copy that drifted would tell an operator
// something confidently wrong about their own cluster, which is worse than
// telling them less.
func WouldHappen(a engine.NodeAssessment, assessment Assessment) string {
	switch {
	case a.Skipped:
		return "The cluster has moved underneath it: " + a.SkipReason + "."
	case len(a.Blockers) > 0:
		return "A pod on it can no longer be evicted: " + a.Blockers[0].Message + "."
	case a.Verdict() == engine.VerdictInfeasible:
		return "The pods still on it no longer fit anywhere else."
	case assessment.Action == Abandon:
		return fmt.Sprintf("It has passed its bound and would be handed back: %s (%s).",
			assessment.Reason, assessment.Code)
	default:
		return fmt.Sprintf("It is within its bounds, with %d pods left to move.",
			assessment.Remaining)
	}
}

// Assessment is the answer, and what to record on the node.
type Assessment struct {
	Action Action
	// Code names an Abandon, from the set above. Empty otherwise.
	Code string
	// Reason says why, in a sentence an operator can act on.
	Reason string
	// Remaining counts the pods still to leave, terminating ones included.
	// Recorded on the node so the next evaluation can tell whether the count
	// moved — including across a restart, when it is the only record of what
	// binpack last saw.
	Remaining int
	// Progressed is whether a keep-alive signal was observed, meaning the
	// progress marker should be refreshed.
	//
	// Note this reports only what is visible in State. An eviction request
	// that was just accepted is also progress, and the caller knows about
	// that one without being told.
	Progressed bool
}

// Assess says what should happen next to a drain already under way.
//
// The checks are ordered by how much they tell an operator: a named stuck pod
// beats "the drain stalled", which beats a bare count.
func Assess(s State, policy Policy) Assessment {
	// Only pods binpack is responsible for moving. A DaemonSet pod wedged
	// during an unrelated rollout is not this drain being stuck, and counting
	// it as remaining would mean the drain never finishes: the DaemonSet
	// controller puts the pod straight back onto the node, which still exists.
	//
	// Deliberately [engine.NodeBound] rather than the engine's NodeLocal
	// class, which also covers terminating pods. That is right for the
	// simulation, where a pod on its way out needs no destination — and wrong
	// here, where it is precisely the pod being waited for. Using the class
	// would report an occupied node as empty and hand it to the autoscaler
	// with work still on it.
	//
	// Completed pods are excluded for the opposite reason: a finished Job pod
	// is a real object that never goes away on its own, and counting it would
	// stall every drain of the node it landed on.
	var mine []*corev1.Pod
	for _, pod := range s.Pods {
		if !engine.NodeBound(pod) && engine.Occupies(pod) {
			mine = append(mine, pod)
		}
	}

	if pod, over := stuck(mine, s.Now); pod != nil {
		return Assessment{
			Action: Abandon, Code: AbandonStuck, Remaining: len(mine),
			Reason: fmt.Sprintf("pod %s/%s is %s past its termination deadline",
				pod.Namespace, pod.Name, round(over)),
		}
	}

	// A pod shutting down within its grace period is progress as a *state*
	// rather than an event, which is what lets a one-hour terminationGrace
	// period work without anyone configuring anything: while it is legitimately
	// shutting down, the stall clock does not run.
	shuttingDown := terminatingWithinGrace(mine, s.Now)

	// Fewer pods than binpack last recorded means the cluster moved, whether
	// or not this process was running at the time. That is what makes restart
	// behaviour and steady-state behaviour the same judgement: a controller
	// down for twenty minutes of a legitimate forty-minute shutdown comes back
	// to a stale timestamp and a demonstrably healthy drain.
	recorded, hasCount := recordedRemaining(s.Node)
	fewer := hasCount && len(mine) < recorded

	progressed := fewer || shuttingDown

	if len(mine) == 0 {
		// The removal clock runs from the last progress, which for an empty
		// node is the last pod leaving it. A different question from the stall
		// timeout, which is why it is a different bound.
		// Guarded by !progressed for the same reason the stall bound is: a node
		// observed empty for the first time became empty *now*, whatever the
		// marker says. Without this, a controller returning from a twenty-minute
		// outage to find the last pod gone would uncordon and back off a node
		// the autoscaler has not yet had a chance to remove.
		if since, known := sinceProgress(s.Node, s.Now); !progressed && known &&
			since > policy.RemovalTimeout {
			return Assessment{
				Action: Abandon, Code: AbandonNotRemoved,
				Reason: fmt.Sprintf(
					"the node has been empty for %s and the cluster-autoscaler has not removed it",
					round(since)),
			}
		}
		return Assessment{Action: AwaitRemoval, Remaining: 0, Progressed: progressed}
	}

	if !progressed {
		if since, known := sinceProgress(s.Node, s.Now); known && since > policy.StallTimeout {
			return Assessment{
				Action: Abandon, Code: AbandonStalled, Remaining: len(mine),
				Reason: fmt.Sprintf("no progress for %s, with %d pods still to move",
					round(since), len(mine)),
			}
		}
	}

	return Assessment{Action: Continue, Remaining: len(mine), Progressed: progressed}
}

// stuck finds a pod that is past its termination deadline, and by how much.
//
// This is what makes being stuck *detected* rather than inferred from a
// timeout. Naming the pod and how far past it is tells an operator where to
// look; "the drain timed out" does not.
func stuck(pods []*corev1.Pod, now time.Time) (*corev1.Pod, time.Duration) {
	var worst *corev1.Pod
	var by time.Duration
	for _, pod := range pods {
		deadline, terminating := terminationDeadline(pod)
		if !terminating {
			continue
		}
		if over := now.Sub(deadline.Add(Slack)); over > 0 && over > by {
			worst, by = pod, over
		}
	}
	return worst, by
}

func terminatingWithinGrace(pods []*corev1.Pod, now time.Time) bool {
	for _, pod := range pods {
		if deadline, terminating := terminationDeadline(pod); terminating &&
			!now.After(deadline.Add(Slack)) {
			return true
		}
	}
	return false
}

// terminationDeadline is when the kubelet should have finished with a pod.
//
// It is the deletion timestamp itself, with nothing added. The API server sets
// that field to the moment the grace period *expires* — `now + gracePeriod` at
// the point deletion is requested — rather than to when deletion was asked
// for, and preserves that invariant when a second delete shortens the period,
// moving the timestamp back by the old grace and forward by the new. See
// BeforeDelete in k8s.io/apiserver/pkg/registry/rest/delete.go.
//
// deletionGracePeriodSeconds is therefore bookkeeping here, not an addend:
// adding it would double every deadline, and a pod wedged on a finalizer with
// an hour's grace would be called healthy for a second hour.
func terminationDeadline(pod *corev1.Pod) (time.Time, bool) {
	if pod.DeletionTimestamp == nil {
		return time.Time{}, false
	}
	return pod.DeletionTimestamp.Time, true
}

func recordedRemaining(node *corev1.Node) (int, bool) {
	var n int
	if _, err := fmt.Sscanf(node.Annotations[engine.AnnotationDrainPodsRemaining], "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

// sinceProgress is how long since binpack last saw this drain move.
//
// Falls back to when the drain started, so a drain that has never yet shown a
// signal is still bounded. Reports unknown rather than zero when neither marker
// parses: a missing marker is a reason to record one, not to abandon a drain
// that may be perfectly healthy.
func sinceProgress(node *corev1.Node, now time.Time) (time.Duration, bool) {
	for _, key := range []string{engine.AnnotationDrainProgress, engine.AnnotationDrainStarted} {
		at, err := time.Parse(time.RFC3339, node.Annotations[key])
		if err != nil {
			continue
		}
		if since := now.Sub(at); since > 0 {
			return since, true
		}
		// A marker in the future is a clock disagreement, not progress that
		// has not happened yet. Treated as "just now".
		return 0, true
	}
	return 0, false
}

// round formats a duration for a human reading an event or an annotation.
//
// Go's own formatting says "13m0s", and these strings are the whole reason
// the reasons are worth writing: an operator reads them to find out where to
// look.
func round(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		if m := int(d.Minutes()) % 60; m > 0 {
			return fmt.Sprintf("%dh%dm", int(d.Hours()), m)
		}
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
