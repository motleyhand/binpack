package controller

import (
	"context"
	"fmt"

	"github.com/motleyhand/binpack/internal/engine"
)

// Event reasons and actions binpack writes onto nodes. Public API from the
// first release, like the metric names and the annotations: people alert on
// these.
//
// The events.k8s.io API separates the two: the action is the operation being
// reported on, and the reason is what happened to it. Every binpack event
// about consolidating a node therefore shares one action and differs in
// reason, which is what makes them filterable as a group.
const (
	ActionConsolidate = "Consolidate"

	// ReasonWouldDrain is a drain binpack has decided on but will not perform,
	// because it is running in dry-run.
	ReasonWouldDrain = "WouldDrain"

	// ReasonDraining is the same decision, acted on. A separate reason rather
	// than a different note, so the two can never be confused by anything
	// filtering events — including a person skimming `kubectl describe node`.
	ReasonDraining = "Draining"

	// ReasonDrained and ReasonDrainAbandoned are how a drain ended. Both are
	// worth an event: the first is what binpack exists to do, and the second
	// carries the sentence saying what stopped it.
	ReasonDrained        = "Drained"
	ReasonDrainAbandoned = "DrainAbandoned"
)

// report records a decision where somebody will find it.
//
// Two places, deliberately. The log line is for whoever is watching the
// process; the Event is for whoever is looking at the node, and on a managed
// control plane — where the autoscaler's own logs are unreachable and binpack's
// may be too — `kubectl describe node` is the only surface a cluster user
// reliably has. This mirrors how the autoscaler surfaces its own decisions.
func (e *evaluator) report(ctx context.Context, s engine.Snapshot, d engine.Decision) error {
	if d.Action != engine.Drain || d.Node == nil {
		// Logged every evaluation rather than only on change. A controller
		// that goes quiet is indistinguishable from one that has stopped, and
		// this is the line that says otherwise.
		e.log.Info("nothing to do",
			"reason", d.Reason,
			"nodesConsidered", len(d.Assessments),
			"nodes", len(s.Nodes),
			"pods", len(s.Pods))
		return nil
	}

	chosen := chosenAssessment(d)
	relocating := 0
	if chosen != nil && chosen.Simulation != nil {
		relocating = len(chosen.Simulation.Relocated)
	}

	e.log.Info(map[bool]string{true: "would drain", false: "draining"}[e.opts.DryRun],
		"node", d.Node.Name,
		"pool", poolOf(chosen),
		"podsToRelocate", relocating,
		"dryRun", e.opts.DryRun)

	// The note is kept stable for a given cluster state on purpose. The event
	// recorder aggregates repeats of an identical (object, reason, note) into
	// one Event carrying a count and a first and last timestamp, so a decision
	// that holds for an hour reads as one line saying so rather than sixty
	// saying the same thing.
	// The note is chosen by mode, not decorated with it. An event reading
	// "No action taken — dry run" while pods are being evicted is worse than
	// no event at all: it is the one surface a cluster user reliably has, and
	// it would be telling them the opposite of what is happening.
	reason, note := ReasonWouldDrain, fmt.Sprintf(
		"binpack would drain this node: %s. No action taken — dry run",
		relocationSummary(relocating))
	if !e.opts.DryRun {
		reason, note = ReasonDraining, fmt.Sprintf(
			"binpack is draining this node: %s. Pods are evicted one at a time, and the "+
				"cluster-autoscaler removes the node once it is empty",
			relocationSummary(relocating))
	}

	// Returned rather than logged here: whether a lost report is fatal depends
	// on whether anything will try again, which the caller knows and this does
	// not.
	if err := e.reporter.emit(ctx, d.Node, reason, ActionConsolidate, note); err != nil {
		return fmt.Errorf("recording the decision on %s: %w", d.Node.Name, err)
	}
	return nil
}

func relocationSummary(pods int) string {
	if pods == 0 {
		return "it runs nothing that would need to move"
	}
	if pods == 1 {
		return "its 1 relocatable pod fits elsewhere"
	}
	return fmt.Sprintf("all %d of its relocatable pods fit elsewhere", pods)
}

func chosenAssessment(d engine.Decision) *engine.NodeAssessment {
	for i := range d.Assessments {
		if d.Assessments[i].Chosen {
			return &d.Assessments[i]
		}
	}
	return nil
}

func poolOf(a *engine.NodeAssessment) string {
	if a == nil {
		return ""
	}
	return a.Pool
}
