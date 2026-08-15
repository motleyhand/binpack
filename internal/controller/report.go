package controller

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"

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
)

// report records a decision where somebody will find it.
//
// Two places, deliberately. The log line is for whoever is watching the
// process; the Event is for whoever is looking at the node, and on a managed
// control plane — where the autoscaler's own logs are unreachable and binpack's
// may be too — `kubectl describe node` is the only surface a cluster user
// reliably has. This mirrors how the autoscaler surfaces its own decisions.
func (e *evaluator) report(s engine.Snapshot, d engine.Decision) {
	if d.Action != engine.Drain || d.Node == nil {
		// Logged every evaluation rather than only on change. A controller
		// that goes quiet is indistinguishable from one that has stopped, and
		// this is the line that says otherwise.
		e.log.Info("nothing to do",
			"reason", d.Reason,
			"nodesConsidered", len(d.Assessments),
			"nodes", len(s.Nodes),
			"pods", len(s.Pods))
		return
	}

	chosen := chosenAssessment(d)
	relocating := 0
	if chosen != nil && chosen.Simulation != nil {
		relocating = len(chosen.Simulation.Relocated)
	}

	e.log.Info("would drain",
		"node", d.Node.Name,
		"pool", poolOf(chosen),
		"podsToRelocate", relocating,
		"dryRun", e.opts.DryRun)

	// The message is kept stable for a given cluster state on purpose. The
	// event recorder aggregates repeats of an identical (object, reason,
	// message) into one Event carrying a count and a first and last timestamp,
	// so a decision that holds for an hour reads as one line saying so rather
	// than sixty saying the same thing.
	e.recorder.Eventf(d.Node, nil, corev1.EventTypeNormal,
		ReasonWouldDrain, ActionConsolidate,
		"binpack would drain this node: %s. No action taken — dry run",
		relocationSummary(relocating))
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
