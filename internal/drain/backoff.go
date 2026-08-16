package drain

import (
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/motleyhand/binpack/internal/engine"
)

// Backoff bounds: 30 minutes doubling to a day.
//
// The cap is not a give-up. A node is never permanently skipped after N
// attempts, because clearing that would need a human — which contradicts
// leaving the cluster working without intervention, and would strand a node
// blocked by something transient. A daily retry is slow enough to be harmless
// and fast enough to recover on its own.
const (
	BackoffInitial = 30 * time.Minute
	BackoffMax     = 24 * time.Hour
)

// Backoff is what to record on a node whose drain has just been abandoned.
//
// This is a correctness requirement rather than politeness. Abandoning a drain
// uncordons the node, and a partially drained node has *fewer* pods than it
// started with — so with candidates ordered least-loaded-first it is now more
// attractive than before. Without this, binpack would preferentially retry its
// own failures, evicting a few more pods each round.
//
// Distinct from cooldown.afterDrain, which is cluster-wide and follows a
// *successful* drain. One prevents thrash after failure; the other lets the
// cluster settle after success.
//
// Self-cleaning: a drain that succeeds deletes the node and takes the
// annotations with it.
func Backoff(node *corev1.Node, now time.Time) (attempts int, until time.Time) {
	attempts = priorAttempts(node) + 1

	wait := BackoffInitial
	for i := 1; i < attempts && wait < BackoffMax; i++ {
		wait *= 2
	}
	if wait > BackoffMax {
		wait = BackoffMax
	}

	return attempts, now.Add(wait)
}

// priorAttempts reads the recorded count, treating anything unreadable as
// none.
//
// Erring towards a shorter backoff on a corrupt value is deliberate: the
// annotation is writable by anyone with node access, and the failure mode of
// reading it wrongly should be binpack retrying sooner, not a node excluded
// from consolidation for a day on the strength of a typo.
func priorAttempts(node *corev1.Node) int {
	n, err := strconv.Atoi(node.Annotations[engine.AnnotationDrainAttempts])
	if err != nil || n < 0 {
		return 0
	}
	return n
}
