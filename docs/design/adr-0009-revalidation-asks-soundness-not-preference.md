# ADR-0009: revalidation re-asks soundness, not preference

## Status

Accepted, and **superseded in part by
[ADR-0010](adr-0010-a-scale-up-stops-a-drain-that-has-not-started.md)**: "has a scale-up started"
has moved from the soundness list below to the preference list. The split itself, its criterion
and everything else in both lists stand.

Narrows the property recorded in [ADR-0006](adr-0006-scheduler-fidelity.md)'s implementation and
in the revalidation work that followed it.

## Context

A drain revalidates before every eviction, because the cluster keeps moving underneath it. That
much is not in question: the decision that selected a node is made against a snapshot that is
stale by the time anything is evicted, and pods the scheduler bound to that node in between were
never assessed.

The mistake was treating every check in the decision procedure as the same kind of check.

The first drain binpack ever performed on a real cluster went like this. It chose a node, marked
and cordoned it, and one evaluation later evicted the first pod — an overprovisioning pause pod,
which is the largest relocatable pod on the node and therefore the first to go. Its replacement
landed on another node, consuming free space there. The next evaluation revalidated, found that
no node now had room for a pod the size of the largest one in the cluster, and abandoned the
drain: uncordoned, backed off, one pod bounced for nothing.

It then chose the pool's other node, whose pause pod it also evicted, and abandoned that one too
for the same reason.

Every safety property held. The node was handed back, the reason was specific, no workload was
disrupted and no scale-up occurred. But the outcome was churn, and the mechanism generalises:
**relocating a pod consumes the free space the reserve needs, so a drain that satisfies the
reserve at decision time can fail it immediately after its own first eviction.**

## Decision

Split the checks by what a wrong answer costs, and re-ask only the ones that make a drain
*unsound*.

**Soundness — re-asked before every eviction:**

- Do the remaining pods still fit somewhere?
- Are they still evictable, given their PodDisruptionBudgets?
- Is the node still eligible at all: is the autoscaler alive, is the pool above its minimum, has
  a scale-up started, has an operator annotated it?

**Preference — asked once, at the decision:**

- `feasibility.reserveForLargestPod`
- `drain.maxPodsPerDrain`

> **"Has a scale-up started" has since moved to the preference list.** By the criterion stated
> immediately below, it answers "was this a good idea" — and re-asking it abandoned drains that
> had already relocated pods, on growth anywhere in the cluster and on a cluster-autoscaler
> restart with no growth at all. See
> [ADR-0010](adr-0010-a-scale-up-stops-a-drain-that-has-not-started.md), which also records why
> "is the pool above its minimum" did not move with it.

Both of these answer "was this a good idea", not "is this safe". A drain that violates them
after the fact has already violated them; re-asking does not undo the eviction that caused it.
What it does is abandon a half-drained node, which is worse than either finishing or never
starting.

"Committed" is read from the node itself — the marker naming the replacement the drain is
waiting for, written immediately before the first eviction and cleared only when the drain ends.
Read inside `Revalidate` rather than passed by the caller, so nothing can disagree with the node
about what has already happened. Before rather than after, because a marker written afterwards is
lost by exactly the failures it exists to survive while the eviction it records is not; see
[ADR-0010](adr-0010-a-scale-up-stops-a-drain-that-has-not-started.md)'s consequences.

The first revalidation, immediately after the cordon, still applies the preferences. Nothing has
been evicted at that point, so aborting is free — and that is precisely the revalidation that
catches pods the scheduler bound to the node between the decision and the cordon.

## Consequences

- **Selection and revalidation no longer run identical code, and that is now deliberate.** The
  earlier property was that they share one implementation so they cannot drift. It is narrowed
  rather than abandoned: the soundness questions are still asked by exactly the same code, and
  the difference is one named flag with this document behind it. Stated here because the next
  reader of `Revalidate` will otherwise "fix" it back.
- A drain can now complete having left the cluster with less headroom than the reserve wanted.
  That was already true of any drain that finished before the reserve was re-checked; it is now
  true predictably rather than depending on eviction order.
- `explain` and `diagnose` are unaffected. Neither runs against a drain in progress, so both
  always see the full set of checks.

## Alternatives considered

**Drop `reserveForLargestPod`.** Tempting, and the observed failure was entirely its doing. But
the risk it guards is real — a cluster packed exactly full scales up the moment anything is
deployed, which is the oscillation binpack exists to avoid — and the instability was in *when*
it was asked, not in *what* it asked. Removing a safety default to fix a scheduling bug would
have been the wrong trade.

**Scope the reserve to the pool being drained.** The largest relocatable pod in the cluster may
live in a pool binpack cannot touch, and on the cluster where this was found it did: a Prometheus
pod in a pool with no autoscaling set the bar for consolidating a different pool entirely. Per
pool is less arbitrary, but it is still an approximation of "how much room should be left", and
replacing one approximation with a slightly better one is not a reason to change a documented
default. Left open.

**Model preemption in the reserve.** Overprovisioning pause pods exist to be preempted, so a
cluster running them genuinely does have room for a large pod even when the reserve says it does
not. Correct, and a much larger change: it would mean modelling priority-based preemption, which
is a scheduler behaviour binpack deliberately does not implement. Left open, and noted as the
reason a warm buffer is a better instrument than the reserve for clusters that run one.
