# ADR-0007: Abandon drains on lack of progress, not on elapsed time

- **Status:** accepted
- **Date:** 2026-08-15

## Context

binpack cordons a node and evicts its pods. Something has to decide when a drain that is not
working should be given up on, because a node left cordoned forever is lost capacity — the
failure [ADR-0004](adr-0004-provider-agnostic-no-cloud-api.md) commits to never leaving behind.

The original design used a single `verifyRemovalTimeout`, defaulting to 15 minutes. Two problems
with that, found by asking what happens to a workload with a long termination grace period.

**A wall-clock timeout cannot distinguish slow from stuck.** A StatefulSet with
`terminationGracePeriodSeconds: 3600` is behaving correctly when it takes 45 minutes to shut
down. A pod wedged on a finalizer is not. Both look identical to a timer, so any single value is
simultaneously too short for the first and too long for the second.

**The timeout did not cover the part that can actually hang.** As written it began "once the
node is empty" — it bounded the wait for the autoscaler to delete an empty node, while the
eviction loop that precedes it had no bound at all. The realistic failure was not premature
abandonment but an indefinite hang.

A third problem sat behind both. Abandoning a drain uncordons the node, and a partially drained
node has *fewer* pods than before. Since candidates are ordered least-loaded-first, the node
that just failed becomes **more** attractive than it was. Without per-node memory, binpack would
preferentially retry its own failures, evicting a few more pods each time — a capacity-churning
loop that resembles progress.

## Decision

### Progress, not elapsed time

A drain is abandoned when it stops making progress, not when a clock runs out. Any of these
keeps it alive:

1. An eviction request was accepted.
2. The count of relocatable pods on the node decreased.
3. A pod on the node acquired a `deletionTimestamp`.
4. A pod on the node is terminating and still **within** its
   `deletionTimestamp + terminationGracePeriodSeconds + slack`.

The fourth is the important one, and it is a *state* rather than an event: while a pod is
legitimately shutting down, the stall clock does not run. A pod with an hour-long grace period
therefore needs no configuration and no derived budget — it is visibly, checkably in the middle
of doing what it was told to do.

`stallTimeout` (default 10 minutes) governs only the absence of all four.

### Stuck is detected, not inferred

A pod that still exists past `deletionTimestamp + terminationGracePeriodSeconds + slack` is not
slow. The kubelet should have sent SIGKILL and the pod should be gone, so something is wrong:
a finalizer that never completes, a volume that will not detach, or an unhealthy kubelet.

binpack reports that as what it is, naming the pod and how far past its deadline it is, rather
than as "the drain timed out". A timeout tells an operator nothing; "pod X is 12 minutes past
its termination deadline, check its finalizers" tells them where to look.

Slack is a fixed two minutes — enough for SIGKILL and the API server to catch up, short enough
that a genuinely stuck pod is not mistaken for a slow one. It is deliberately not configurable:
it describes how Kubernetes behaves, not a preference.

### Two bounds, because they measure different things

- `drain.stallTimeout` — no progress during eviction.
- `drain.removalTimeout` — the node is empty; how long to wait for the autoscaler to delete it.

The second is a genuinely different question with a genuinely different answer, and conflating
them is what hid the missing bound in the first place.

### Per-node exponential backoff

On abandonment, binpack records on the node itself:

```
binpack.motleyhand.com/drain-attempts: "3"
binpack.motleyhand.com/backoff-until:  "2026-08-15T14:00:00Z"
binpack.motleyhand.com/last-failure:   "pod monitoring/prometheus-0 is 12m past its termination deadline"
```

Backoff starts at 30 minutes and doubles to a 24-hour cap. A node in backoff is not a candidate.

This is a correctness requirement rather than politeness: without it, the candidate ordering
actively prefers nodes that have just failed. It also self-cleans — a drain that succeeds
deletes the node, and the annotations go with it.

It is deliberately not a permanent skip after N attempts. That would need a human to clear an
annotation, which contradicts leaving the cluster working without intervention; a node blocked
by something transient would stay skipped indefinitely. A 24-hour retry is slow enough to be
harmless and fast enough to recover on its own.

### Recovery state records progress, not a deadline

The durable markers become:

```
binpack.motleyhand.com/drain-started:  "2026-08-15T09:15:37Z"
binpack.motleyhand.com/drain-progress: "2026-08-15T09:31:02Z"
```

`drain-progress` is updated whenever a progress signal is observed. On startup or on acquiring
leadership, a node carrying a marker is resumed if progress is recent and abandoned if it is
stale — which is the same judgement the running controller makes, so restart behaviour and
steady-state behaviour cannot diverge.

This replaces a `drain-deadline` annotation, which encoded a decision that is no longer
time-based.

### Evaluation and drain execution are separate state machines

The controller evaluates every `interval`, by default 60 seconds. A drain may legitimately run
for far longer. So each evaluation begins by checking whether any node carries a drain marker
and, if so, advancing that drain rather than deciding afresh.

Without this, a 40-minute drain would have roughly 40 evaluations running alongside it, each
free to select a second node. "One node per run" quietly assumed a run was short.

## Consequences

- Drains have no upper bound in wall-clock time. A drain that is progressing is allowed to
  finish, however long that takes. Deliberate: the alternative is abandoning work that was about
  to succeed, and every abandonment costs churn.
- `explain` and `diagnose` must report backoff state, or a node being skipped becomes
  inexplicable. "Skipped: in backoff until 14:00 after 3 attempts, last failure ..." is the
  minimum useful output.
- Progress updates are API writes. They are bounded by eviction count, which is small, but the
  controller should not update the annotation more than once per evaluation.
- Two new metrics matter: drains abandoned by reason, and current backoff depth per node. A
  cluster where backoff is deepening is one where binpack cannot do its job, and that should be
  visible without reading logs.
- `cooldown.afterDrain` remains, and is now clearly distinct: it is cluster-wide and applies
  after a *successful* drain, while backoff is per-node and applies after a failed one.
