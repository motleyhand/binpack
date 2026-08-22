package drain

import (
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/motleyhand/binpack/internal/engine"
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
// The bounds come from the resolved policy. They were package constants here
// until the two configuration fields naming them — policy.backoff.initial and
// policy.backoff.max — turned out to be parsed, defaulted, validated and
// printed back by `binpack config validate` while reaching no decision point:
// an operator lengthening backoff.max for a fragile pool was told it had been
// set, and binpack went on retrying the node every day. The defaults now have
// one home, in api/v1alpha1, which is the layer that fills them in; a second
// copy here could only ever agree with that one by coincidence, and would
// take the unconfigured case with it when it stopped.
//
// The cap is not a give-up. A node is never permanently skipped after N
// attempts, because clearing that would need a human — which contradicts
// leaving the cluster working without intervention, and would strand a node
// blocked by something transient. A retry that is slow enough to be harmless
// is still fast enough to recover on its own.
//
// Self-cleaning: a drain that succeeds deletes the node and takes the
// annotations with it.
func Backoff(node *corev1.Node, now time.Time, policy Policy) (attempts int, until time.Time) {
	attempts = priorAttempts(node) + 1

	wait := min(policy.BackoffInitial, policy.BackoffMax)
	for i := 1; i < attempts && wait < policy.BackoffMax; i++ {
		// Clamped before the doubling rather than after it. time.Duration is
		// int64 nanoseconds and runs out a little past 292 years, so a pair of
		// bounds well inside the type can still double past the end of it —
		// and it wraps rather than saturating. Capping afterwards therefore
		// compares against a negative number, lets it through, and puts
		// backoff-until in the past: the node whose drain just failed becomes
		// a candidate again immediately, with fewer pods than before, which is
		// the exact outcome this function exists to prevent. Halving the cap
		// cannot overflow, and nothing below it can.
		if wait > policy.BackoffMax/2 {
			wait = policy.BackoffMax
			break
		}
		wait *= 2
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
