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
//
// The purity half of that is held by the `purity` depguard rule rather than by
// this sentence, which claimed it for a while with nothing behind it. The
// read-only half cannot be linted and remains a review rule. See ADR-0008.
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
// A calibration, not a description of Kubernetes. An earlier version of this
// comment claimed the latter and used it to close the question of making the
// value configurable; the claim does not survive reading the kubelet. What
// upstream bounds is SIGKILL *delivery* — killContainer hands the remaining
// grace period to the CRI's StopContainer. Nothing bounds the teardown after
// it: the object survives until SyncTerminatedPod completes, and that waits
// for volumes to unmount and then polls podVolumesExist in a loop with no
// timeout at all (pkg/kubelet/kubelet.go). The nearest thing to a bound on
// that path, podAttachAndMountTimeout, is 2m3s and is a retry yield rather
// than a deadline, by its own comment.
//
// So the cost of this number is knowable: a pod using its full grace period
// whose volumes need a second unmount attempt is already past it, and gets
// reported stuck while behaving correctly. Fixed rather than configurable
// because there is no upstream quantity to derive a better one from and a
// config key cannot be withdrawn once shipped — a trade, not a fact. See
// ADR-0007, which names the metric that would argue for the knob.
const Slack = 2 * time.Minute

// State is everything the judgement reads.
type State struct {
	// Node carries the drain markers. Read-only.
	Node *corev1.Node
	// Pods are the pods currently on that node, terminating ones included —
	// a pod on its way out is still occupying it.
	Pods []*corev1.Pod
	Now  time.Time

	// LastScaleUp is when the cluster-autoscaler last published a scale-up:
	// clusterWide.scaleUp.lastTransitionTime, verbatim. Zero where it has
	// published none.
	//
	// Taken bare, which is not what the engine's cooldown does with the same
	// field — that one reads it against two controller-held observations,
	// because the autoscaler restamps the transition on every restart and a
	// stand-down of the whole cluster is too much to spend on a scale-up that
	// never happened.
	//
	// The question here is a different one, and the restart answers it
	// truthfully. This is not "did the cluster grow", it is "could the
	// autoscaler remove this node in the next few minutes", and after a
	// restart it could not: the unneeded-since map is process memory, built
	// fresh by unneeded.NewNodes, so every node's scale-down-unneeded-time
	// starts again from the new process's first scan — the same instant the
	// restamped transition names. A different mechanism from the one the
	// timestamp describes, beginning together and, on stock flags, lasting
	// the same ten minutes.
	//
	// The directions of error settle it either way. Believing a restamp
	// leaves an empty node cordoned for at most one pause longer than it
	// needed to be. Disbelieving a real scale-up abandons a drain the
	// autoscaler was going to finish, which is the whole of what this field
	// exists to prevent.
	LastScaleUp time.Time

	// ScaleUpInProgress is clusterWide.scaleUp.status reading InProgress: the
	// autoscaler is adding nodes right now.
	//
	// Needed beside [State.LastScaleUp] because the two describe the same
	// episode from different ends, and only one of them moves. The transition
	// time is stamped when the scale-up *began*, so a slow one — a cloud
	// provider taking its time, a quota refusal being retried — ages out of
	// the pause below while the flag is still set, and the removal wait would
	// expire in the middle of the growth it is meant to wait out. The
	// autoscaler restamps its own gate only on a scale-up that succeeded, so
	// nothing in the document moves during the wait either.
	//
	// binpack already holds this flag as the strongest reason there is not to
	// be removing a node: the eligibility check refuses on it outright, ahead
	// of any cooldown and with no duration involved. Deferring on the tail of
	// an episode while ignoring the episode itself is the same signal read two
	// ways, and it errs at the worst available moment — handing back an
	// emptied node, with backoff, while the cluster is actively growing.
	ScaleUpInProgress bool
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
// All of them are taken as resolved. Nothing here defaults a zero, for the same
// reason [Assess] does not: a caller that built this by hand has skipped the
// layer that fills values in, and quietly supplying one is how a configured
// setting comes to differ from the setting that is applied.
type Policy struct {
	StallTimeout   time.Duration
	RemovalTimeout time.Duration
	BackoffInitial time.Duration
	BackoffMax     time.Duration

	// ScaleUpPause is how long the cluster-autoscaler suppresses scale-down
	// after the cluster grows — its own --scale-down-delay-after-add, which
	// gates all scale-down cluster-wide because --scale-down-delay-type-local
	// defaults to false.
	//
	// Resolved from cooldown.afterScaleUp, which is the same number: that
	// setting is binpack's mirror of that flag and is documented as one.
	// binpack holds one figure for the autoscaler's post-growth pause rather
	// than two that could disagree, and the two uses are consistent — the
	// cooldown declines to *start* a drain the autoscaler would not finish
	// yet, and this declines to give up on one for the same reason.
	ScaleUpPause time.Duration
}

// Action is what to do with a drain that is already under way.
type Action int

const (
	// Continue means pods remain and the drain is alive. Evict the next one
	// when nothing is in flight.
	Continue Action = iota

	// AwaitRemoval means there is nothing left to evict. The node is now the
	// cluster-autoscaler's to delete.
	AwaitRemoval

	// Abandon means give up: uncordon, clear the markers, record backoff.
	// Code and Reason say why.
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
	// have sent SIGKILL by now, so this is usually a finalizer, a volume that
	// will not detach, or an unhealthy kubelet. Usually rather than always:
	// Slack bounds something upstream does not, so a teardown that is merely
	// slow reaches this code too.
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
// The filter every [Assess] call applies, acting and reporting alike, and it
// has to stay the same — which is why there is one of it rather than one per
// caller. What it feeds tells an operator what acting would do, so a pod set
// that differed from the acting path's would describe a drain nobody would
// get. Exported because [StateFor] is not the only way in: a caller assembling
// a [State] by hand still has to filter the same way.
func PodsOn(s engine.Snapshot, name string) []*corev1.Pod {
	var on []*corev1.Pod
	for _, pod := range s.Pods {
		if pod.Spec.NodeName == name {
			on = append(on, pod)
		}
	}
	return on
}

// StateFor builds the assessment's input for one node from a snapshot.
//
// One of it for the same reason there is one [PodsOn]: three places assess a
// drain — the executor before it acts, and `explain` and the dry-run path to
// say what acting would do — and a report assembled from a different set of
// facts describes a drain nobody would get. The pod filter was already shared;
// what this adds is that a fact reaching the judgement reaches all three.
//
// The node is passed rather than looked up because the caller has already
// resolved it: revalidation hands back the node it read, and re-finding it by
// name here would be a second lookup free to disagree with the first.
func StateFor(s engine.Snapshot, node *corev1.Node) State {
	return State{
		Node:              node,
		Pods:              PodsOn(s, node.Name),
		Now:               s.Now,
		LastScaleUp:       s.Autoscaler.LastScaleUp,
		ScaleUpInProgress: s.Autoscaler.ScaleUpInProgress,
	}
}

// PolicyFor resolves the bounds that govern the drain of one named node, from
// that node's own pool.
func PolicyFor(cfg engine.Config, s engine.Snapshot, name string) Policy {
	for _, node := range s.Nodes {
		if node.Name != name {
			continue
		}
		p := cfg.PolicyForNode(node)
		return policyFrom(p)
	}
	return policyFrom(cfg.Default)
}

// policyFrom narrows a resolved engine policy to the part this package reads.
//
// One conversion rather than one per branch above: the fall-through and the
// matched node have to agree, and two literals listing the same fields is an
// invariant with nobody enforcing it.
//
// A field added to [Policy] and left out here arrives at [Assess] as a zero,
// and a zero is a bound rather than an absence — which for StallTimeout means
// every drain in the cluster abandoned as stalled, each node taking backoff,
// presenting as a workload problem rather than as a struct field nobody
// wired. TestPolicyForCarriesEveryFieldTheDrainBoundsRunUnder counts the
// fields and asserts each one, so the omission fails here rather than in a
// cluster.
func policyFrom(p engine.Policy) Policy {
	return Policy{
		StallTimeout:   p.StallTimeout,
		RemovalTimeout: p.RemovalTimeout,
		BackoffInitial: p.BackoffInitial,
		BackoffMax:     p.BackoffMax,
		ScaleUpPause:   p.CooldownAfterScaleUp,
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

// PodsToMove is the pods on a node that this drain is responsible for moving.
//
// One of it for the same reason there is one [PodsOn]: every question about how
// far a drain has got — the count [Assess] records, the executor's wait for a
// pod still in flight, and which residents the simulation should have named —
// is a question about the same set, so a second filter alongside this one is a
// second answer to "is this node nearly empty". The pair drifted exactly that
// way once, and the executor spent it calling a node's CNI DaemonSet pod a pod
// the simulation could not account for.
//
// A DaemonSet pod wedged during an unrelated rollout is not this drain being
// stuck, and counting it as remaining would mean the drain never finishes: the
// DaemonSet controller puts the pod straight back onto the node, which still
// exists.
//
// Deliberately [engine.NodeBound] rather than the engine's NodeLocal class,
// which also covers terminating pods. That is right for the simulation, where a
// pod on its way out needs no destination — and wrong here, where it is
// precisely the pod being waited for. Using the class would report an occupied
// node as empty and hand it to the autoscaler with work still on it.
//
// Completed pods are excluded for the opposite reason: a finished Job pod is a
// real object that never goes away on its own, and counting it would stall
// every drain of the node it landed on.
func PodsToMove(pods []*corev1.Pod) []*corev1.Pod {
	var mine []*corev1.Pod
	for _, pod := range pods {
		if !engine.NodeBound(pod) && engine.Occupies(pod) {
			mine = append(mine, pod)
		}
	}
	return mine
}

// Assess says what should happen next to a drain already under way.
//
// The checks are ordered by how much they tell an operator: a named stuck pod
// beats "the drain stalled", which beats a bare count.
func Assess(s State, policy Policy) Assessment {
	mine := PodsToMove(s.Pods)

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
		//
		// And guarded by the autoscaler's own gate, because this is the one
		// bound whose deadline binpack does not set. See scaleDownPaused.
		if since, known := sinceProgress(s.Node, s.Now); !progressed && known &&
			since > policy.RemovalTimeout && !scaleDownPaused(s, policy) {
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

// scaleDownPaused reports whether the cluster-autoscaler has told binpack it
// will not be removing anything just yet.
//
// It publishes when the cluster last grew, and its own scale-down is gated on
// that: `a.lastScaleUpTime.Add(a.ScaleDownDelayAfterAdd).After(currentTime)`
// in isScaleDownInCooldown suppresses every scale-down cluster-wide, not
// merely in the pool that grew. So an emptied node waiting to be reaped can be
// held past any removalTimeout by a deploy in a namespace nobody was thinking
// about, and abandoning there uncordons a node the autoscaler was going to
// delete a minute later. The same predicate, read from outside.
//
// ADR-0007's argument, one level up: a wall clock cannot tell a slow removal
// from an abandoned one, so bound the absence of progress instead. A scale-up
// is the cluster stating that the removal will be slow.
//
// Two arms, in the order the eligibility check asks the same two questions,
// because they are the same episode seen from different ends and only one of
// them has a clock in it. A scale-up under way is stated outright and takes no
// duration; the transition time it was stamped with covers the tail afterwards.
// Reading only the tail expires the wait in the middle of a slow scale-up — the
// stamp names when growth *began*, and the autoscaler restamps its own gate
// only on one that succeeded, so neither figure moves while a cloud provider
// takes its time.
//
// Deferring, not resetting. The pause suppresses the abandonment and nothing
// else — in particular it is not progress, so the marker is not restamped and
// the elapsed time keeps running underneath. When the gate opens on a node
// that has been empty past its bound, the next evaluation abandons it at once.
// That is what keeps the timed arm terminating: each scale-up buys at most one
// pause measured from itself, never a fresh removalTimeout.
//
// The flag has no clock at all, so nothing in it expires — a status document
// frozen mid-scale-up by an autoscaler that then died would read InProgress for
// ever. What bounds that is what bounds every other wait on this component, and
// it is why nothing new is needed here: revalidation stops believing a document
// that has stopped being refreshed, reports SkipAutoscalerNotLive, and the executor
// hands the node back on the verdict before it reaches this assessment at all.
func scaleDownPaused(s State, policy Policy) bool {
	if s.ScaleUpInProgress {
		return true
	}
	if policy.ScaleUpPause <= 0 || s.LastScaleUp.IsZero() {
		return false
	}
	return s.Now.Sub(s.LastScaleUp) < policy.ScaleUpPause
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
		over := now.Sub(deadline.Add(Slack))
		if over <= 0 {
			continue
		}
		// Ties broken on the name, and a tie here is the normal case rather
		// than a corner of one: a drain evicts a batch of one workload's pods,
		// those pods share terminationGracePeriodSeconds, so the API server
		// writes them identical deadlines and their overruns are equal to the
		// second. Ranking on the overrun alone left the winner to be whichever
		// the input listed first — and that list is filled from a watch-backed
		// cache in the controller and from a live client in `binpack explain`,
		// so the two named different pods as the reason for one abandonment.
		//
		// The nil test leads so the comparator is total by inspection. Without
		// it the reader has to reconstruct why over == by cannot be reached
		// with worst still unset, and that argument stops holding the day
		// somebody changes what by starts at.
		if worst == nil || over > by || (over == by && podRef(pod) < podRef(worst)) {
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

// podRef names a pod the way every other report in this tree names one.
func podRef(pod *corev1.Pod) string { return pod.Namespace + "/" + pod.Name }
