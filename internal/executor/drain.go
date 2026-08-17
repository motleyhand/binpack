package executor

import (
	"context"
	"fmt"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

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

	// StepUnschedulable: a pod that moved off this node could not be placed.
	// The simulation proved a valid assignment existed; the scheduler is not
	// obliged to choose that one, and this is what it looks like when it
	// chose differently.
	StepUnschedulable = "replacement-unschedulable"
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
	if err := Annotate(ctx, w, node, map[string]string{
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

	case a.SkipCode == engine.SkipUncordoned:
		// Marked but schedulable, so a previous evaluation stopped between
		// the two writes. Repaired rather than abandoned — nothing has been
		// evicted on the strength of a bad snapshot, because revalidation
		// refuses to look at an uncordoned node at all. The next evaluation
		// re-asks against a snapshot taken after this cordon.
		if err := Cordon(ctx, w, a.Node); err != nil {
			return Step{}, err
		}
		return Step{Code: StepCordoned,
			Reason: "the node carried a drain marker but was not cordoned"}, nil

	case a.Verdict() != engine.VerdictDrainable:
		// The cluster moved underneath the drain. Nothing here distinguishes
		// a drain that has evicted nothing from one that is half done: both
		// end the same way, with the node handed back.
		return Abandon(ctx, w, a.Node, a.SkipCode, revalidationReason(a), s.Now)
	}

	pods := podsOn(s, name)

	if pod := unschedulableSince(s, a.Node); pod != nil {
		// Detected, not inferred from a timeout. The stall bound would catch
		// this eventually and report "no progress"; naming the pod that could
		// not be placed says what actually happened.
		return Abandon(ctx, w, a.Node, StepUnschedulable, fmt.Sprintf(
			"pod %s/%s could not be scheduled anywhere after moving off this node",
			pod.Namespace, pod.Name), s.Now)
	}

	assessment := drain.Assess(
		drain.State{Node: a.Node, Pods: pods, Now: s.Now}, policy)

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
		return Abandon(ctx, w, a.Node, "unaccounted-pods", fmt.Sprintf(
			"%d pods remain that the simulation did not account for", len(pods)), s.Now)
	}

	if err := Evict(ctx, w, next); err != nil {
		return Step{}, err
	}

	// An accepted eviction is itself progress, and the only progress signal
	// that is an event rather than a state — nothing in the next snapshot
	// distinguishes "just evicted" from "never touched".
	assessment.Progressed = true
	if err := record(ctx, w, a.Node, assessment, s.Now); err != nil {
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
	err := Annotate(ctx, w, node, map[string]string{
		engine.AnnotationDrainStarted:       "",
		engine.AnnotationDrainProgress:      "",
		engine.AnnotationDrainPodsRemaining: "",

		engine.AnnotationDrainAttempts: strconv.Itoa(attempts),
		engine.AnnotationBackoffUntil:  until.UTC().Format(time.RFC3339),
		engine.AnnotationLastFailure:   reason,
	})
	if err != nil {
		return Step{}, fmt.Errorf("recording the failed drain on %s: %w", node.Name, err)
	}

	return Step{Code: code, Reason: reason, Done: true, Failed: true}, nil
}

// record refreshes the progress markers, and only when there is progress to
// record: ADR-0007 asks for at most one such write per evaluation, and writing
// an unchanged timestamp every interval would reset the stall clock forever.
func record(
	ctx context.Context, w Writer,
	node *corev1.Node, a drain.Assessment, now time.Time,
) error {
	if !a.Progressed {
		return nil
	}
	return Annotate(ctx, w, node, map[string]string{
		engine.AnnotationDrainProgress:      now.UTC().Format(time.RFC3339),
		engine.AnnotationDrainPodsRemaining: strconv.Itoa(a.Remaining),
	})
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

func terminating(pods []*corev1.Pod) *corev1.Pod {
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil && !engine.NodeBound(pod) {
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

// unschedulableSince finds a pod the scheduler has given up on since this
// drain started.
//
// The scheduler sets PodScheduled=False with reason Unschedulable after trying
// and failing, so this is a statement of fact rather than a guess from
// elapsed time. Bounded to pods created after the drain began: a cluster with
// a chronically unplaceable pod would otherwise never consolidate at all,
// which is a much worse failure than the one being guarded against.
func unschedulableSince(s engine.Snapshot, node *corev1.Node) *corev1.Pod {
	started, err := time.Parse(time.RFC3339, node.Annotations[engine.AnnotationDrainStarted])
	if err != nil {
		return nil
	}

	for _, pod := range s.Pods {
		if pod.Spec.NodeName != "" || pod.CreationTimestamp.Time.Before(started) {
			continue
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse &&
				c.Reason == corev1.PodReasonUnschedulable {
				return pod
			}
		}
	}
	return nil
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
