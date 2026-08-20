package executor

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/motleyhand/binpack/internal/drain"
	"github.com/motleyhand/binpack/internal/engine"
)

// Step codes. Stable, like the diagnostic codes and the metric names.
const (
	StepCordoned     = "cordoned"
	StepEvicted      = "evicted"
	StepWaiting      = "waiting"
	StepAwaitRemoval = "awaiting-removal"
	StepRemoved      = "removed"

	// StepHandedOver: the cluster-autoscaler has committed to deleting the
	// node and is draining it itself.
	StepHandedOver = "handed-over"
)

// Step is what one evaluation did to a drain.
type Step struct {
	// Code names what happened, or why the drain was abandoned.
	Code   string
	Reason string
	// Done: no drain is in flight on this node any more.
	Done bool
	// Failed distinguishes an abandoned drain from a completed one. A drain
	// that ends with the node removed is the whole point; one that ends with
	// it uncordoned is a failure, and they must not be counted together.
	Failed bool
}

// Begin starts a drain: mark, then cordon.
//
// In that order, and the order is the design. Cordoning first and recording
// afterwards would leave a process that stopped in between holding a cordoned
// node nothing knows to finish — the failure the recovery state exists to
// prevent. Marking first leaves evidence instead, and [engine.Revalidate]
// reports a marked-but-schedulable node so the next evaluation can repair it.
//
// Nothing is evicted here. The decision that chose this node was made against
// a snapshot that is already stale, and until the node is unschedulable its
// pod set can still grow — so the eviction decision belongs to the next
// evaluation, against the post-cordon pod set.
func Begin(ctx context.Context, w Writer, node *corev1.Node, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339)
	if err := patchMeta(ctx, w, node,
		// A label as well as the markers, because `kubectl get nodes` shows a
		// cordoned node as SchedulingDisabled and says nothing about who did
		// it — binpack, a human, or something else entirely. A label rather
		// than a taint: taints are an array, so setting one means replacing
		// the whole list, on a field the cluster-autoscaler is editing on
		// these same nodes. A label is a map key and merge-patches cleanly.
		map[string]string{engine.LabelDraining: "true"},
		map[string]string{
			engine.AnnotationDrainStarted:  stamp,
			engine.AnnotationDrainProgress: stamp,
		}); err != nil {
		return fmt.Errorf("marking node %s: %w", node.Name, err)
	}
	return Cordon(ctx, w, node)
}

// Advance moves the drain on one node forward by at most one step.
//
// One step per evaluation, because evaluation and drain execution are separate
// state machines: a drain may legitimately outlast many intervals, and without
// this every one of those intervals would be free to select a second node.
//
// Every path through this function ends with the node removed, still being
// drained, or uncordoned. A cordoned node nothing will finish is capacity that
// is paid for and cannot be used.
//
// "Still being drained" is the answer that needs watching, because it is the
// one that can be given for ever. It is only safe while every path that
// returns it has first consulted the drain assessment, which is why the
// assessment is computed above all of them rather than beside the branch that
// happened to need it.
func Advance(
	ctx context.Context, w Writer,
	s engine.Snapshot, name string, cfg engine.Config, policy drain.Policy,
) (Step, error) {
	a := engine.Revalidate(s, name, cfg)

	switch {
	case a.SkipCode == engine.SkipGone:
		// What the drain was working towards. Nothing to clean up: the
		// annotations went with the node.
		return Step{Code: StepRemoved, Done: true,
			Reason: "the cluster-autoscaler removed the node"}, nil

	case engine.BeingRemoved(a.Node):
		// Asked of the node directly rather than read from the skip code,
		// because eligibility reports one reason and a node can satisfy
		// several. The likeliest overlap is the one this must survive: the
		// autoscaler deleting a node is frequently *what brings the pool to
		// its minimum*, so pool-at-minimum would be reported instead, this
		// branch would be skipped, and the drain abandoned — uncordoning a
		// node mid-deletion, which is the single thing the branch exists to
		// prevent. A scale-up elsewhere, or an operator annotating the node,
		// reach it the same way.
		//
		// The autoscaler owns the node now. Not abandoned — abandoning
		// uncordons, and uncordoning a node another component is deleting is
		// two controllers disagreeing about whether it accepts pods. Not
		// advanced either: it is evicting the same pods, and doubling that up
		// gains nothing.
		//
		// The markers stay, so the drain is still binpack's to observe. When
		// the node goes, the next evaluation records the completion it would
		// otherwise have missed — on the cluster where this was found, binpack
		// scored two abandonments for an operation that succeeded.
		//
		// Progress is recorded because there is progress: the autoscaler is
		// emptying the node. Without it, an autoscaler that changed its mind
		// and removed its taint would hand back a drain whose stall clock had
		// been running the whole time, and the next evaluation would abandon
		// it on the spot.
		if err := Annotate(ctx, w, a.Node, map[string]string{
			engine.AnnotationDrainProgress: s.Now.UTC().Format(time.RFC3339),
		}); err != nil {
			return Step{}, err
		}
		return Step{Code: StepHandedOver,
			Reason: "the cluster-autoscaler is removing this node"}, nil
	}

	// The bound, computed before any branch that would otherwise return
	// without one.
	//
	// It used to sit fifty lines below, under the repair and under the wait
	// for a replacement, and both of those returned above it — so on those two
	// paths stallTimeout, removalTimeout and the stuck detector were not
	// merely unmet, they were never evaluated. That is the closure property
	// failing in the one place it is load-bearing: a node nothing will finish
	// stays cordoned, and because the controller short-circuits every
	// evaluation to this function while a drain is marked, binpack stops
	// consolidating anywhere in the cluster.
	//
	// Assessing here costs nothing that returning early saved: it reads the
	// snapshot and writes nothing.
	pods := podsOn(s, name)

	assessment := drain.Assess(
		drain.State{Node: a.Node, Pods: pods, Now: s.Now}, policy)

	switch {
	case a.SkipCode == engine.SkipUncordoned:
		// Marked but schedulable, so a previous evaluation stopped between
		// the two writes — or something else keeps clearing the flag: a
		// controller that owns spec.unschedulable, or an operator answering an
		// unexpected SchedulingDisabled with kubectl uncordon. Repaired rather
		// than abandoned on that account alone: nothing has been evicted on
		// the strength of a bad snapshot, because revalidation refuses to look
		// at an uncordoned node at all.
		//
		// The cordon still comes first, even when the drain is about to end,
		// because a node accepting pods must stop accepting them before
		// anything else is decided. Abandon then hands it back, which is the
		// same field written again in the other direction.
		if err := Cordon(ctx, w, a.Node); err != nil {
			return Step{}, err
		}
		if assessment.Action == drain.Abandon {
			return Abandon(ctx, w, a.Node, assessment.Code, assessment.Reason, s.Now)
		}
		// Recorded rather than asserted: record moves the progress marker only
		// when the assessment saw progress of its own, so the repair leaves a
		// mark when the cluster moved and stays silent when it did not.
		// Repairing a cordon is not itself progress, and a path that claimed
		// otherwise would hold the stall clock at zero for as long as the
		// repair kept being needed — the same wedge in a different disguise.
		if err := record(ctx, w, a.Node, assessment, s.Now); err != nil {
			return Step{}, err
		}
		return Step{Code: StepCordoned,
			Reason: "the node carried a drain marker but was not cordoned"}, nil

	case a.Verdict() != engine.VerdictDrainable:
		// The cluster moved underneath the drain. Nothing here distinguishes
		// a drain that has evicted nothing from one that is half done: both
		// end the same way, with the node handed back.
		//
		// The verdict rather than the skip code, because a node that became
		// infeasible or blocked carries no skip code at all — and publishing
		// an empty label would put a value outside the documented vocabulary
		// into the metric, on the two outcomes most worth telling apart.
		//
		// Ahead of the assessment's own abandonment, because it names what
		// changed. "The remaining pods no longer fit" tells an operator where
		// to look; a node that has also been quiet for eleven minutes is the
		// same node with the reason filed off.
		return Abandon(ctx, w, a.Node, revalidationCode(a), revalidationReason(a), s.Now)
	}

	// A replacement owed by an earlier eviction settles what happens next,
	// before anything else is considered. binpack does not place pods and
	// cannot steer the scheduler: the simulation proves a valid assignment
	// exists, not that the scheduler will choose it. Keeping one replacement
	// in flight is the whole of what binpack can do about that, and it only
	// works if "in flight" means bound rather than merely gone from here.
	if owner, since, ok := awaiting(a.Node); ok {
		switch state, pod := replacementFor(s, name, owner, since); state {
		case refused:
			// The scheduler said so itself — PodScheduled=False, reason
			// Unschedulable — so this is detected rather than inferred from a
			// timeout, and it names the pod. Uncordoning is also the repair:
			// this node is where that pod can go.
			return Abandon(ctx, w, a.Node, drain.AbandonUnschedulable, fmt.Sprintf(
				"pod %s/%s could not be scheduled after moving off this node",
				pod.Namespace, pod.Name), s.Now)

		case awaited:
			// Bounded, because having a controller does not mean the
			// controller will produce a bound pod carrying that same UID. A
			// rollout or a scale-down supersedes the ReplicaSet and every
			// later pod is owned by a different one; a Job at its backoffLimit
			// creates nothing at all; an admission-attached scheduling gate
			// leaves the replacement neither placed nor refused. None of those
			// is distinguishable from a replacement that is merely slow, and
			// all of them show up as an absence of progress — which is the
			// question the assessment already answers.
			if assessment.Action == drain.Abandon {
				return Abandon(ctx, w, a.Node, assessment.Code, assessment.Reason, s.Now)
			}
			// Recorded like every other wait, and here it is what makes the
			// bound above reachable rather than decorative. record runs before
			// the eviction it accompanies, so the count on the node is one
			// higher than what the next evaluation finds: that evaluation sees
			// fewer pods and calls it progress, correctly, once. Returning
			// without lowering the count leaves the same departure being read
			// as fresh progress every interval afterwards, and a stall clock
			// that restarts every interval never runs out.
			if err := record(ctx, w, a.Node, assessment, s.Now); err != nil {
				return Step{}, err
			}
			return Step{Code: StepWaiting,
				Reason: "waiting for the replacement pod to be scheduled"}, nil
		}
		// landed: the next eviction overwrites this marker, so there is
		// nothing to clear.
	}

	switch assessment.Action {
	case drain.Abandon:
		return Abandon(ctx, w, a.Node, assessment.Code, assessment.Reason, s.Now)

	case drain.AwaitRemoval:
		if err := record(ctx, w, a.Node, assessment, s.Now); err != nil {
			return Step{}, err
		}
		return Step{Code: StepAwaitRemoval,
			Reason: "the node is empty; waiting for the cluster-autoscaler to remove it"}, nil
	}

	// Sequential, with revalidation between each. The simulation proves a
	// valid assignment exists, not that the scheduler will choose it — so
	// binpack keeps at most one pod in flight, which makes a wrong prediction
	// visible after one eviction rather than after all of them.
	if inFlight := terminating(pods); inFlight != nil {
		if err := record(ctx, w, a.Node, assessment, s.Now); err != nil {
			return Step{}, err
		}
		return Step{Code: StepWaiting, Reason: fmt.Sprintf(
			"waiting for pod %s/%s to finish terminating",
			inFlight.Namespace, inFlight.Name)}, nil
	}

	next := nextToEvict(a.Simulation, pods)
	if next == nil {
		// Pods remain that the simulation did not name. Rather than guess at
		// them, hand the node back: acting on a set binpack cannot account for
		// is exactly what the allowlist exists to prevent.
		return Abandon(ctx, w, a.Node, drain.AbandonUnaccounted, fmt.Sprintf(
			"%d pods remain that the simulation did not account for", len(pods)), s.Now)
	}

	if err := Evict(ctx, w, next); err != nil {
		return Step{}, err
	}

	// Recorded on the assessment's terms, not on the eviction's. An accepted
	// eviction feels like progress and is not: it is an event, and a bound
	// defined over the *absence* of progress cannot be kept alive by something
	// that can be emitted every interval for ever. A node whose population
	// never falls is not being emptied however many evictions it has accepted
	// — which is the state a workload tolerating the cordon produces, since
	// every pod evicted off the node is placed straight back onto it.
	//
	// Nothing is lost by not asserting it. record writes the count as it was
	// *before* this eviction, so the next evaluation finds one fewer pod than
	// the node records and reads the departure as progress — from the state,
	// where it can be checked, rather than from an event only this process
	// remembers.
	if err := record(ctx, w, a.Node, assessment, s.Now); err != nil {
		return Step{}, err
	}
	if err := Annotate(ctx, w, a.Node, map[string]string{
		engine.AnnotationDrainAwaiting: awaitingMarker(next, s.Now),
	}); err != nil {
		return Step{}, err
	}

	return Step{Code: StepEvicted,
		Reason: fmt.Sprintf("evicted pod %s/%s", next.Namespace, next.Name)}, nil
}

// Abandon hands the node back and records why.
//
// Uncordon first, deliberately. If the annotation write then fails, the node
// is at worst marked and schedulable, which the next evaluation detects and
// repairs — the drain resumes, revalidation governs it, and no capacity is
// lost. The other order fails the other way: a node left cordoned with its
// marker cleared reads as "cordoned by somebody else", and binpack would
// leave it alone indefinitely while the cluster paid for it.
func Abandon(
	ctx context.Context, w Writer,
	node *corev1.Node, code, reason string, now time.Time,
) (Step, error) {
	if err := Uncordon(ctx, w, node); err != nil {
		return Step{}, fmt.Errorf("handing back node %s: %w", node.Name, err)
	}

	attempts, until := drain.Backoff(node, now)

	// One patch: the markers clear and the backoff appears together, or
	// neither does. A node that forgot it was draining but never recorded the
	// failure would be retried immediately, and it is now the emptiest
	// candidate in the pool.
	err := patchMeta(ctx, w, node,
		map[string]string{engine.LabelDraining: ""},
		map[string]string{
			engine.AnnotationDrainStarted:       "",
			engine.AnnotationDrainProgress:      "",
			engine.AnnotationDrainPodsRemaining: "",
			engine.AnnotationDrainAwaiting:      "",

			engine.AnnotationDrainAttempts: strconv.Itoa(attempts),
			engine.AnnotationBackoffUntil:  until.UTC().Format(time.RFC3339),
			engine.AnnotationLastFailure:   reason,
		})
	if err != nil {
		return Step{}, fmt.Errorf("recording the failed drain on %s: %w", node.Name, err)
	}

	return Step{Code: code, Reason: reason, Done: true, Failed: true}, nil
}

// record refreshes the drain markers, at most once per evaluation as ADR-0007
// asks, and only where there is something new to say.
//
// The progress timestamp moves only when there was progress: it is a claim
// about the cluster, and refreshing it on an evaluation that saw none would
// push the stall deadline out by an interval every interval, which is a
// deadline that never arrives.
//
// The count is not a claim but the baseline the claim is measured against —
// [drain.Assess] reports progress when the live count is below it — so it is
// also written when the node carries no readable one. Writing it only
// alongside progress is circular, and the circle closes on the first eviction
// of every drain, because a node comes out of [Begin] with no count at all:
// nothing seeds it, so nothing can ever be fewer than it, so no departure is
// ever progress. A node holding more pods than the stall timeout has intervals
// is then abandoned mid-drain as stalled, having relocated every pod it was
// asked to and reported "no progress" about each one.
//
// Seeded, rather than refreshed on every evaluation. A count that followed the
// live population would let one that rose and fell back — a tolerating
// workload arriving on the cordoned node and leaving again — read as a fall
// every other interval, which is the same clock reset in a different disguise.
func record(
	ctx context.Context, w Writer,
	node *corev1.Node, a drain.Assessment, now time.Time,
) error {
	markers := map[string]string{}
	if a.Progressed {
		markers[engine.AnnotationDrainProgress] = now.UTC().Format(time.RFC3339)
	}

	_, err := strconv.Atoi(node.Annotations[engine.AnnotationDrainPodsRemaining])
	unseeded := err != nil
	if a.Progressed || unseeded {
		markers[engine.AnnotationDrainPodsRemaining] = strconv.Itoa(a.Remaining)
	}

	if len(markers) == 0 {
		return nil
	}
	return Annotate(ctx, w, node, markers)
}

func podsOn(s engine.Snapshot, name string) []*corev1.Pod {
	var on []*corev1.Pod
	for _, pod := range s.Pods {
		if pod.Spec.NodeName == name {
			on = append(on, pod)
		}
	}
	return on
}

// terminating finds a pod on the node that is on its way out.
//
// Filtered exactly as [drain.Assess] filters, and that is the point rather
// than a coincidence: a Succeeded or Failed pod held by a finalizer is not
// occupying the node, so the assessment does not count it — and a helper that
// called it in flight would wait on it every evaluation while other pods sat
// there evictable, until the drain was abandoned as stalled.
func terminating(pods []*corev1.Pod) *corev1.Pod {
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil && !engine.NodeBound(pod) && engine.Occupies(pod) {
			return pod
		}
	}
	return nil
}

// nextToEvict picks the pod to evict, in the order the simulation placed them.
//
// Largest first, which is the order the packing found a home for them in. The
// hardest pod to place goes first, so a prediction that was wrong costs one
// eviction rather than all of them.
func nextToEvict(sim *engine.Simulation, on []*corev1.Pod) *corev1.Pod {
	if sim == nil {
		return nil
	}
	present := make(map[string]bool, len(on))
	for _, pod := range on {
		present[pod.Namespace+"/"+pod.Name] = true
	}

	for _, p := range sim.Relocated {
		if present[p.Pod.Namespace+"/"+p.Pod.Name] {
			return p.Pod
		}
	}
	for _, pod := range sim.Evicted {
		if present[pod.Namespace+"/"+pod.Name] {
			return pod
		}
	}
	return nil
}

// replacement reports what has become of the pod a controller owes this drain.
type replacement int

const (
	// awaited: the controller has not produced a bound pod yet. Either it has
	// not created one, or the scheduler has not placed it.
	awaited replacement = iota
	// landed: a pod of that controller is bound to a node. The drain may
	// proceed to the next eviction.
	landed
	// refused: the scheduler tried and could not place it.
	refused
)

// awaiting reads the controller this drain is waiting on, and since when.
func awaiting(node *corev1.Node) (types.UID, time.Time, bool) {
	owner, stamp, found := strings.Cut(node.Annotations[engine.AnnotationDrainAwaiting], "@")
	if !found || owner == "" {
		return "", time.Time{}, false
	}
	since, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return "", time.Time{}, false
	}
	return types.UID(owner), since, true
}

// replacementFor looks for the pod a controller created after one of its pods
// was evicted from the draining node.
//
// Correlated by controller UID rather than by time alone. An unrelated
// workload deployed with impossible requests would otherwise abandon a
// perfectly healthy drain and claim, wrongly, that its pod had moved off this
// node. Created-after matters too: a Deployment with replicas elsewhere
// already has bound pods, and one of those would answer "landed" before the
// replacement existed.
func replacementFor(
	s engine.Snapshot, node string, owner types.UID, since time.Time,
) (replacement, *corev1.Pod) {
	// Every candidate is examined rather than the first match returned. A
	// controller can have several pods newer than the eviction — a rollout, a
	// scale-up — and returning on whichever the snapshot happened to list
	// first would make the answer depend on iteration order. Refused wins:
	// a pod the scheduler could not place is the fact worth acting on,
	// whatever its siblings are doing.
	var landedPod, pendingPod *corev1.Pod
	for _, pod := range s.Pods {
		ref := metav1.GetControllerOf(pod)
		if ref == nil || ref.UID != owner || pod.CreationTimestamp.Before(&metav1.Time{Time: since}) {
			continue
		}
		// Bound to the node under drain is not relocated, however bound it
		// looks. kube-scheduler admits a pod onto a cordoned node when the pod
		// tolerates node.kubernetes.io/unschedulable:NoSchedule — a blanket
		// `{operator: Exists}`, which "run anywhere" chart values set — so a
		// replacement can be placed straight back where its predecessor came
		// from. Reading that as a landed replacement counts a pod that never
		// left as a successful relocation and evicts the next one on the
		// strength of it; that one's replacement comes back too, and the node
		// is churned for as long as binpack is running.
		//
		// Skipped rather than reported, because it is neither: nothing has
		// relocated, so the drain goes on waiting, and what ends the wait is
		// the same bound that ends any other — the node's population is not
		// falling, and an absence of progress is what the assessment measures.
		if pod.Spec.NodeName == node {
			continue
		}
		if pod.Spec.NodeName != "" {
			landedPod = pod
			continue
		}
		pendingPod = pod
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse &&
				c.Reason == corev1.PodReasonUnschedulable {
				return refused, pod
			}
		}
	}

	if landedPod != nil {
		return landed, landedPod
	}
	return awaited, pendingPod
}

// awaitingMarker records which controller owes this drain a pod, so the next
// evaluation can tell whether it landed.
//
// The nil case guards a dereference rather than describing a reachable state:
// a pod with no readable controller template makes the node infeasible, so the
// engine refuses the drain long before anything is evicted.
func awaitingMarker(pod *corev1.Pod, now time.Time) string {
	ref := metav1.GetControllerOf(pod)
	if ref == nil {
		return ""
	}
	return string(ref.UID) + "@" + now.UTC().Format(time.RFC3339)
}

// revalidationCode names why a node stopped being drainable, as a label a
// metric can carry: a skip code when there is one, and the verdict otherwise.
func revalidationCode(a engine.NodeAssessment) string {
	if a.SkipCode != "" {
		return a.SkipCode
	}
	return a.Verdict()
}

// revalidationReason renders why a node stopped being drainable, in the terms
// the operator will need.
//
// The specific message every time it exists: this string lands on the node as
// the recorded failure and in the event, and "the drain was abandoned" is
// worth nothing to the person reading it.
func revalidationReason(a engine.NodeAssessment) string {
	switch {
	case a.Skipped:
		return a.SkipReason
	case len(a.Blockers) > 0:
		return a.Blockers[0].Message
	case a.Simulation != nil && a.Simulation.Blocked != nil:
		return a.Simulation.Blocked.Summary
	default:
		return "the node is no longer drainable"
	}
}
