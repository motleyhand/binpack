# ADR-0010: A scale-up stops a drain that has not started, not one that has

- **Status:** accepted. Supersedes the soundness list in
  [ADR-0009](adr-0009-revalidation-asks-soundness-not-preference.md): "has a scale-up started"
  moves from Soundness to Preference. Everything else in ADR-0009 stands. Amended since, not
  superseded: the soundness list below is qualified by
  [a blocker that has not been computed yet is not an answer](#a-blocker-that-has-not-been-computed-yet-is-not-an-answer),
  which says what stopping means for a refusal that lifts on its own.
- **Date:** 2026-08-21

## Context

[ADR-0009](adr-0009-revalidation-asks-soundness-not-preference.md) split revalidation's checks
by what a wrong answer costs, and re-asks only the ones that make a drain *unsound*. Its
soundness list reads:

> Is the node still eligible at all: is the autoscaler alive, is the pool above its minimum, has
> a scale-up started, has an operator annotated it?

Three of those four belong there. The third does not, judged by ADR-0009's own criterion — and
that criterion is stated in the same document: "Both of these answer 'was this a good idea', not
'is this safe'."

The code says so too, in the comment above the check it implements: "Draining straight after the
cluster grew is how oscillation starts, and the autoscaler pauses its own scale-down then
anyway." Oscillation is a cost. It is the reason the cooldown exists and a perfectly good reason
not to *start* a drain. It is not a statement about whether a drain already under way is safe.

### What that cost, in practice

`Revalidate` runs the whole eligibility check on every evaluation of a drain in flight, and the
executor turns any resulting skip into an abandonment: the node is uncordoned, the drain marker
is cleared, `binpack_drains_abandoned_total` increments, and thirty minutes of per-node backoff
are recorded. So on a node that had already evicted pods:

- the cluster-autoscaler publishing `clusterWide.scaleUp.status: InProgress`, or
- `clusterWide.scaleUp.lastTransitionTime` falling inside `policy.cooldown.afterScaleUp`

ended the drain. The pods that had already been relocated stayed where they went, the
consolidation they were paying for was lost, and the node — which had done nothing wrong — was
excluded for half an hour, doubling towards a daily retry if it happened again.

Three things make that worse than it first sounds.

**The trigger is cluster-wide, not pool-scoped.** `clusterWide.scaleUp` is one field for the
whole cluster, so a scale-up in a pool binpack is not touching ends a drain in a pool it is.

**It fires without a scale-up.** The cluster-autoscaler holds its cluster-state registry in
process memory, so a restart restamps `scaleUp.lastTransitionTime` to now with nothing having
been added. On a managed control plane — an upgrade, an OOM kill, a leader-election handover —
that is a likelier cause of this abandonment than growth.

**The exposure is the whole drain, and the worst moment is the last one.** binpack performs one
eviction per evaluation, so a fifteen-pod node is exposed for a quarter of an hour at the default
interval. A node that has been *fully emptied* and is waiting for the autoscaler to reap it is
abandoned by the same check — every pod already moved, one autoscaler pass from success.

## Decision

**"Has a scale-up started" is a preference.** Both arms of it — `scale-up-in-progress` and
`cooldown-after-scale-up` — stop applying once the drain has evicted something.
`cooldown-after-drain` is gated with them, so all three read the same way.[^1]

**Soundness — re-asked before every eviction:**

- Do the remaining pods still fit somewhere?
- Are they still evictable, given their PodDisruptionBudgets?
- Is there still a live autoscaler that could remove this node?
- Is the pool still above its minimum?
- Has an operator annotated the node?

**Preference — asked once, at the decision, and again only until the first eviction:**

- `feasibility.reserveForLargestPod`
- `drain.maxPodsPerDrain`
- `cooldown.afterScaleUp`, and the live `scale-up-in-progress` flag beside it

What gates the narrowing is **committedness, not resuming**. ADR-0009's first-revalidation
behaviour is kept exactly: immediately after the cordon, with nothing evicted, every preference
still applies and aborting is free. Committedness is read from the node's own marker, in one
place, by both halves of revalidation — the eligibility check that decides which questions apply
and the simulation that answers them.

### Why a scale-up cannot make a committed drain unsound

A scale-up adds nodes. It cannot make the pods still on the draining node fit less well than
they did a minute ago.

The questions that *can* make a committed drain unsound are asked directly, separately, on every
evaluation, and none of them consults the scale-up state: the simulation re-answers "do the
remaining pods fit" against the current node set, the evictability check re-answers the
disruption budgets, and the autoscaler's own published status re-answers whether anything will
ever remove this node. Abandoning on a scale-up added no safety on top of those. It only threw
away work.

### A blocker that has not been computed yet is not an answer

Amended after the fact, and it qualifies the soundness list above rather than moving anything
out of it. "Are they still evictable" stays a soundness question, re-asked before every eviction,
and a drain whose remaining pods cannot be evicted still stops. What the list did not say is what
*stopping* should mean, and the answer is not the same for every blocker.

A PodDisruptionBudget whose `status.observedGeneration` is behind its `metadata.generation` has
not been recomputed by the disruption controller yet. The eviction subresource refuses outright
in that state, whatever `disruptionsAllowed` says, so binpack refuses too — but the refusal is
"binpack is looking at this budget between the write and the recomputation", not "this
application cannot spare a replica". The controller resyncs on the very update that bumped the
generation, so the condition lasts one sync, while the response to it lasted thirty minutes of
backoff on a node that had already relocated pods. That is the same trade ADR-0009 draws for
preferences, arrived at from the other side: the check is right, and destroying a committed drain
is the wrong thing to do with the answer.

So a blocker that lifts without anybody acting **pauses** the drain — no eviction this evaluation,
and the drain assessment decides what happens next. Nothing new bounds it: if the blocker turns
out to be durable the node stops getting emptier, and `stallTimeout` ends the drain exactly as it
ends any other absence of progress.

The set is one member wide and the asymmetry is why. Waiting for a blocker that really does lift
costs one evaluation. Classing a durable blocker transient converts an abandonment that is
immediate and names its cause into a cordoned node that ends eleven minutes later reporting "no
progress" — the same outcome, later, described worse. `pdb-insufficient` is therefore *not*
transient, though it often resolves: a budget at `minAvailable: 100%` never has slack, and
nothing in the object distinguishes the two.

The mirror holds too, and it was the same reasoning run backwards. A refusal nothing can lift —
the eviction API's HTTP 500 for a pod covered by two budgets, which it returns because it will
not arbitrate between them — now ends the drain at the eviction, rather than travelling out of the
executor as an error and leaving a cordoned node carrying no record of why until a later
evaluation reaches the same conclusion through revalidation. It ends under that same later
route's reason code, so arriving sooner does not move which series counts it.

### Why `pool-at-minimum` does not move with it

It looks like the same kind of check from inside the eligibility switch, and it is not. At the
pool's floor the autoscaler will never remove this node, so carrying the drain to completion
strands an empty cordoned node until `removalTimeout` abandons it anyway — after more pods have
been bounced, not fewer. That is a drain that cannot succeed, which is unsoundness in the sense
that matters here. The same goes for a pool that has left the autoscaler's managed set.

### Extend the window rather than abandon

Keeping a drain alive through a real scale-up means binpack and the autoscaler are acting on the
same capacity at once, and something has to give. Two answers were available: end the drain, or
let the scale-up *extend* the window binpack is prepared to wait in. **The second is chosen**,
and this ADR is the first of its two halves.

The half made here stops the abandonment. The half not made here is sizing the removal window so
that a scale-up cannot expire it: the autoscaler pauses its own scale-down for
`scale-down-delay-after-add` after growth, and `removalTimeout` — the bound on an emptied node
waiting to be reaped — is currently shorter than that delay plus the autoscaler's own
`scale-down-unneeded-time`. So a scale-up landing early in such a wait can push the autoscaler's
scale-down past binpack's deadline. Before this ADR that node was abandoned on the spot; after
it, it is abandoned at the removal deadline with a reason that names what actually happened.
Strictly better, and still not right; the derivation of that number belongs beside the number.

That direction is [ADR-0007](adr-0007-drain-progress-not-deadlines.md)'s shape applied one level
up. ADR-0007 bounds the absence of progress rather than the wall clock, because a clock cannot
tell a slow shutdown from a wedged one. A scale-up is the cluster saying the reaping will be
slow. Reading that as "stop" discards work; reading it as "this will take longer" is the same
instrument.

[^1]: `cooldown-after-drain` cannot be reached from the resuming path today — the controller
    short-circuits into the drain executor before it assigns the last-drain timestamp, so that
    field is always zero during a drain. That is an ordering accident rather than a guarantee,
    and leaving one arm behind it would invite this defect straight back the day the ordering
    changed.

## Consequences

- **`binpack_drains_abandoned_total{reason="scale-up-in-progress"}` and
  `{reason="cooldown-after-scale-up"}` narrow.** They no longer appear for a drain that has
  evicted anything, so as abandonments they now mean only "the cluster grew between the cordon
  and the first eviction". Both remain reachable, and both are unchanged on
  `binpack_nodes_skipped`, where they still mean what the reference says. A dashboard read across
  this change is comparing two different populations.
- **A drain can now run to completion while the cluster is growing.** Deliberate, and the point.
  It is still bounded, by everything ADR-0007 governs: the stall timeout, the removal timeout and
  the positive detection of a pod past its termination deadline. None of those changes here.
- **Selection is untouched.** `explain` and `diagnose` never resume a drain, so both still apply
  the full set of checks and still report a cooldown as the reason a node was ruled out — even a
  node that a drain is already under way on. ADR-0009 asserted that in prose; it is now a test.
- **Committedness is only as durable as the annotation, and the annotation is written after the
  eviction it records.** `Advance` evicts, then patches the node. If that patch fails, a pod has
  been disrupted and the node carries no marker — a state indistinguishable from a drain that has
  cordoned and evicted nothing, because it is the same node. The next evaluation therefore reads
  the drain as uncommitted, and inside a cooldown window abandons it: the outcome this ADR is
  about, through a door it does not close.

  This window is not opened here. The marker has been the sole committedness signal since
  [ADR-0009](adr-0009-revalidation-asks-soundness-not-preference.md), and the same partial write
  already re-armed `reserveForLargestPod` and `maxPodsPerDrain` —
  `TestTheReserveRefusesADrainThatHasNotStarted` builds exactly that node and asserts exactly that
  refusal. What changes is the cost of falling into it, which is now a drain abandoned rather than
  a preference re-applied.

  Closing it is not simply a matter of writing the marker first. The marker's value names the
  controller whose replacement the drain is waiting for, so writing it before the eviction claims
  a replacement is owed — and an eviction refused after that claim (a `429` from a disruption
  budget, the routine failure on this path) leaves the drain waiting for a pod that was never
  evicted, until the stall bound ends it eleven minutes later. Measured, not assumed. What is
  wanted is a value meaning "committed, and no replacement owed", written before the eviction and
  upgraded to the controller's identity after it. That value is being introduced for its own
  reasons by the work that gives `drain-awaiting` a settled state, and it belongs there rather
  than here.
- One fewer reason for a node to accumulate backoff it did not earn. Candidates are ordered
  least-loaded-first, so the same node was chosen and abandoned each cycle, and seven cluster-wide
  events could put binpack's best candidate on a daily retry without it ever having had a
  problem of its own.

## Alternatives considered

**Narrow only the cooldown, keeping `scale-up-in-progress` as soundness.** The reading ADR-0009
recorded, and defensible: a live scale-up means the autoscaler is racing binpack for the same
capacity, which growth that finished ten minutes ago does not. Rejected, because racing is a cost
question and not a safety one — the drain's soundness is re-checked directly every evaluation,
and a node being added can only improve the answer. Keeping the arm would also have left the
worst case open, since the emptied node waiting to be reaped is exactly where the cluster is most
likely to be growing.

**Gate on `resuming` rather than on committedness.** Simpler: the eligibility check already
carries that flag. Rejected because it would have silently reversed ADR-0009's most deliberate
decision — the first revalidation, immediately after the cordon, is the cheapest possible moment
to notice the cluster has changed its mind, and it costs nothing to honour. Nothing about the
change would have said so out loud.

**Keep abandoning, but stop charging the node for it.** A cluster-wide condition is written to
one node as a per-node failure: `last-failure` reads "the cluster scaled up 12s ago", and the
attempt count doubles the exclusion window. Worth fixing on its own merits, and left open. But it
treats the symptom: the pods that already moved stay moved either way, and the consolidation is
lost either way.

**Pause the drain instead of ending it.** Return "waiting" while the cluster grows, and resume
after. Rejected: a paused drain is a cordoned node nothing is finishing, which is the failure the
closure property exists to prevent — and the pause would need a bound drawn from a clock, which
is what ADR-0007 says not to do. There is no state between "carry on" and "hand it back" that is
safe to hold indefinitely.
