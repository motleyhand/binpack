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

1. The count of relocatable pods on the node decreased.
2. A pod on the node acquired a `deletionTimestamp`.
3. A pod on the node is terminating and still **within** its
   `deletionTimestamp + terminationGracePeriodSeconds + slack`.

The third is the important one, and it is a *state* rather than an event: while a pod is
legitimately shutting down, the stall clock does not run. A pod with an hour-long grace period
therefore needs no configuration and no derived budget — it is visibly, checkably in the middle
of doing what it was told to do.

`stallTimeout` (default 10 minutes) governs only the absence of all three.

A fourth signal was listed here and has been withdrawn: *an eviction request was accepted*. It
was the only one binpack produced rather than observed, and that is what disqualifies it. A
keep-alive the controller emits itself can be emitted every interval for ever, so a bound resting
on its absence is not a bound. The case that demonstrates it is a workload tolerating
`node.kubernetes.io/unschedulable:NoSchedule`: the scheduler admits such a pod onto a cordoned
node, so each replacement is placed back onto the node being drained, the population never falls,
and the eviction stream restamps the marker for as long as the process runs. Nothing is lost by
withdrawing it, because an eviction that achieves anything shows up as signal 1 or 3 on the next
evaluation — and an eviction that achieves nothing is exactly what must not keep the drain alive.

Signal 1 is measured against `drain-pods-remaining`, which makes that count a baseline rather
than a report. It is therefore written the first time binpack looks at a drain as well as
whenever progress is observed: a node comes out of the marking step carrying no count at all, and
nothing can be fewer than a count that was never recorded. Gating the baseline on progress is
circular, and the circle closes on the first eviction of every drain — leaving a node with more
pods on it than `stallTimeout` has intervals to be abandoned mid-flight as stalled, having
relocated every pod it was asked to. For the same reason the count follows progress rather than
the live population: one that tracked every evaluation would read a population that rose and fell
back as a fresh departure, which is the same clock reset arrived at more slowly.

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

### Three bounds, because they measure different things

- `drain.stallTimeout` — no progress during eviction.
- `drain.removalTimeout` — the node is empty; how long to wait for the autoscaler to delete it.
- The hand-over — the autoscaler has taken the node over; how long to go on waiting for it.

The second is a genuinely different question with a genuinely different answer, and conflating
them is what hid the missing bound in the first place.

The third was added afterwards, and it is not a duration at all. Once a node carries
`ToBeDeletedByClusterAutoscaler` the autoscaler has committed to deleting it and is draining it
itself, so binpack stops evicting and waits — abandoning would uncordon a node another controller
is part-way through removing. That branch had no bound of any kind, which is the worst place to
lack one: the taint is only ever cleared by a live autoscaler, on a failed scale-down, on the
batch rollback of a tainting pass, or by its start-up clean-up, and that last visits only the node
groups the autoscaler currently manages. An autoscaler that stopped between tainting a node and
deleting it therefore leaves a taint nothing in the cluster will remove — as does a pool that
leaves the managed set, which survives even a full restart. binpack refreshed the progress marker
against it once an interval indefinitely, and since evaluation short-circuits to a drain in
flight, one such node stopped consolidation everywhere.

The bound is a fact about the autoscaler rather than a clock. binpack ends the wait when the
autoscaler's own published status cannot vouch for the deletion: the status is more than
`MaxStatusAge` old, or the node's pool is no longer among the groups it lists. Both are already
the engine's single answer to whether anything would ever remove this node, so the abandonment
carries the reason revalidation had computed anyway rather than inventing a code for the occasion.

What is deliberately *not* read is the taint's own value, which is the Unix second the autoscaler
applied it. Elapsed time cannot separate a dead autoscaler from a slow deletion — a node under
deletion may legitimately take as long as the autoscaler's `--max-graceful-termination-sec`
allows — and the failure mode of guessing wrong is uncordoning a node mid-delete, the single thing
the hand-over exists to prevent. It remains available as a fallback signal should a case need one.
This is the same argument as the section above, arrived at from the other side: there, elapsed
time could not tell a long shutdown from a wedged one; here it cannot tell a long deletion from an
abandoned one.

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
binpack.motleyhand.com/drain-started:        "2026-08-15T09:15:37Z"
binpack.motleyhand.com/drain-progress:       "2026-08-15T09:31:02Z"
binpack.motleyhand.com/drain-pods-remaining: "4"
```

`drain-progress` and `drain-pods-remaining` are updated whenever a progress signal is observed,
and `drain-pods-remaining` is seeded on first sight of the drain, for the reason above.

**Recovery reads live pod state first, and the annotation only as a fallback.** The annotation
records when *binpack* last looked, not whether the cluster is making progress — and those come
apart precisely when the controller has been unavailable. A controller down for twenty minutes
during a legitimate forty-minute graceful shutdown returns to find a stale timestamp and a
perfectly healthy drain. Abandoning it on annotation age alone would reintroduce elapsed-time
abandonment through the back door, in the one situation the design is meant to handle.

So on startup or on acquiring leadership, for each node carrying a drain marker:

1. If a pod on it is terminating and still within its grace period plus slack, **resume** —
   the drain is demonstrably alive.
2. If fewer pods remain than `drain-pods-remaining` records, progress happened while binpack was
   away, so **resume** and refresh the markers.
3. Only if neither holds, and the recorded progress is older than `stallTimeout`, **abandon**.

That is the same judgement the running controller makes, evaluated against the same signals, so
restart behaviour and steady-state behaviour cannot diverge. The annotation age is a fallback
for the case where no live signal is observable, not the primary test.

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
- A hand-over binpack ends leaves the node uncordoned but still tainted. The taint is the
  autoscaler's and binpack writes no taints, so the node repels everything that does not tolerate
  it until an autoscaler returns to clear it or an operator does. Ending the wait recovers
  binpack — it stops short-circuiting to a node nothing is finishing — before it recovers the
  node, and that ordering is the honest one: the taint is somebody else's statement about the
  node, and binpack removing it would be binpack claiming the deletion was cancelled.
