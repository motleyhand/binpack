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

	// ReasonWouldAdvanceDrain is a drain already in progress that binpack is
	// not advancing, because it is running in dry-run. Its own reason for the
	// same purpose the two above have theirs: nothing filtering events should
	// have to read a note to tell a node binpack is emptying from one it has
	// stopped touching. It is also the only one of these that repeats
	// indefinitely — a frozen drain is ended by an operator, not by binpack —
	// which is why it carries what advancing it would do.
	ReasonWouldAdvanceDrain = "WouldAdvanceDrain"

	// ReasonNoCandidates and ReasonNoneFeasible are the two walls an
	// evaluation that chose nothing can hit, named the way
	// binpack_evaluations_total{code} names them so the node and the metric
	// say the same word. Unlike the five above they are not about a particular
	// node, and the note says so: see ADR-0011.
	//
	// Two reasons rather than one carrying the wall in its note, and that is
	// forced rather than stylistic. The aggregation key covers the reason and
	// not the note, so a cluster moving from one wall to the other under a
	// single reason would leave the first note on the node for ever. The rule
	// is the one ReasonDraining states above: when the thing differs, the
	// reason differs.
	ReasonNoCandidates = "NoCandidates"
	ReasonNoneFeasible = "NoneFeasible"

	// ReasonNoNodeChosen is the same decision reached by some other route. It
	// exists so that reportRefusal's question stays "did this decision name a
	// node" rather than becoming a list of codes: a refusal code added later
	// gets an event that is true but unspecific, instead of silently getting
	// none.
	ReasonNoNodeChosen = "NoNodeChosen"

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
		// Considered means the same thing here as in the reason sentence
		// beside it. It did not: this counted every node looked at, including
		// those ruled out before any simulation ran, so the line read
		// "nodesConsidered: 4" next to "2 node(s) considered".
		considered := engine.Considered(d.Assessments)
		e.log.Info("nothing to do",
			"reason", d.Reason,
			"nodesConsidered", considered,
			"nodesSkipped", len(d.Assessments)-considered,
			"nodes", len(s.Nodes),
			"pods", len(s.Pods))
		return e.reportRefusal(ctx, d)
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
	// recorder aggregates repeats into one Event carrying a count and a first
	// and last timestamp, so a decision that holds for an hour reads as one
	// line saying so rather than sixty saying the same thing — and the note is
	// not part of what it compares, so a note that moved would not open a
	// second Event but leave the first sentence under a fresh timestamp. See
	// [refusalNote], which has the upstream citation.
	// The note is chosen by mode, not decorated with it. An event reading
	// "No action taken — dry run" while pods are being evicted is worse than
	// no event at all: it is the one surface a cluster user reliably has, and
	// it would be telling them the opposite of what is happening.
	reason, note := ReasonWouldDrain, fmt.Sprintf(
		"binpack would drain this node: %s. No action taken — dry run",
		engine.RelocationSummary(relocating))
	if !e.opts.DryRun {
		reason, note = ReasonDraining, fmt.Sprintf(
			"binpack is draining this node: %s. Pods are evicted one at a time, and the "+
				"cluster-autoscaler removes the node once it is empty",
			engine.RelocationSummary(relocating))
	}

	// Returned rather than logged here: whether a lost report is fatal depends
	// on whether anything will try again, which the caller knows and this does
	// not.
	if err := e.reporter.emit(ctx, d.Node, reason, ActionConsolidate, note); err != nil {
		return fmt.Errorf("recording the decision on %s: %w", d.Node.Name, err)
	}
	return nil
}

// reportRefusal records an evaluation that chose no node, on the nodes it was
// made about.
//
// The log line above is not enough on its own. On a managed control plane
// `kubectl describe node` is the one surface a cluster user reliably has, and
// the chart's install notes send a new operator straight to it — so a refusal
// that reaches only the log is a decision they are told to look for and cannot
// find. A cluster with nothing to consolidate is the ordinary steady state, so
// this is the first thing binpack has to say to most installs. See ADR-0011.
//
// Every assessed node rather than one of them, because the operator chooses
// which node to describe and binpack cannot know which. The note says whose
// answer it is for the same reason: it is the cluster's, not the node's.
func (e *evaluator) reportRefusal(ctx context.Context, d engine.Decision) error {
	// A decision naming a node is not a refusal. A drain in progress reaches
	// here too — no action, nothing chosen this evaluation — but the node is
	// named, and in dry run it already carries the event [evaluator.frozen]
	// wrote for it. A second one saying binpack chose no node would contradict
	// it on the node it is about.
	if d.Node != nil {
		return nil
	}

	// The nodes the decision assessed, and not every node in the snapshot.
	// Where the two differ, binpack never got as far as looking: without a
	// live autoscaler it declines to evaluate rather than evaluating and
	// refusing, so there are no assessments and this writes nothing. That is
	// right twice over — there is no node the answer is about, and the
	// sentence it would carry is the one in the vocabulary that counts up on
	// its own, which this surface cannot hold (see [refusalNote]). What that
	// condition has instead is binpack_autoscaler_up, which the metrics
	// reference already ranks second of the three things to alert on.
	reason, note := refusal(d)
	for i := range d.Assessments {
		node := d.Assessments[i].Node
		// Returned rather than logged, as the chosen-node report does and for
		// the same reason: whether a lost report matters depends on whether
		// anything will try again, which the caller knows. A failure part-way
		// leaves some nodes carrying the event and some not, which the next
		// evaluation repairs by writing all of them again.
		if err := e.reporter.emit(ctx, node, reason, ActionConsolidate, note); err != nil {
			return fmt.Errorf("recording the decision on %s: %w", node.Name, err)
		}
	}
	return nil
}

// refusal is the reason and the note for an evaluation that chose no node.
//
// One function returning both, because they are not independent: **the note
// has to be decided by the reason**, and returning them together is the only
// shape in which that cannot quietly stop being true.
//
// The events.k8s.io aggregation key covers the type, action, reason, reporting
// controller and instance, and the object the event is about — and not the
// note (k8s.io/client-go@v0.36.3, tools/events/event_broadcaster.go, getKey).
// An event whose key matches one the recorder has already seen is not recorded
// at all: recordToSink bumps the cached event's series count and observed time
// and drops the new event, and the patch it then sends carries only the
// series. So the note the API server holds is the one the *first* event of
// that series carried. A series ends only after finishTime — six minutes —
// with nothing further observed, and binpack re-decides every minute, so while
// it keeps refusing the series never ends and the note never moves.
//
// Two consequences, and the first of them was got wrong once. A note that
// varies with the *cluster* is as unusable as one that varies with the clock:
// a cluster going from every-node-ruled-out to nothing-fits would keep the
// first sentence on the node under a timestamp that keeps advancing, which is
// a confident lie rather than stale information. That is why the wall is in
// the reason, where the key can see it, and why what is left in the note is
// fixed prose. The detail that does move lives where it can: the log line
// above, binpack_nodes_skipped, and `binpack explain`.
//
// One note for both modes, deliberately. The drain events distinguish dry run
// from acting because what happens differs; here nothing happens either way,
// so a mode in the note would split one series into two and tell the reader
// nothing.
func refusal(d engine.Decision) (reason, note string) {
	reason, wall := ReasonNoNodeChosen, ""
	switch d.Code {
	case engine.CodeNoCandidates:
		reason, wall = ReasonNoCandidates,
			": every node was ruled out before it could be simulated"
	case engine.CodeNoneFeasible:
		reason, wall = ReasonNoneFeasible,
			": nodes were simulated and none could be emptied onto the rest of the cluster"
	}
	return reason, "binpack evaluated the cluster and chose no node to drain" + wall +
		". This is the cluster's answer, written on every node binpack looked at; " +
		"`binpack explain` gives this node's own reason"
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
