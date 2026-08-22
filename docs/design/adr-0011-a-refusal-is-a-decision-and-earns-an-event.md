# ADR-0011: A refusal is a decision, and it earns an Event

- **Status:** accepted. Amends the Observability section of
  [the architecture](2026-08-15-architecture.md), which said the controller emits an Event on the
  Node for every decision — a rule the code did not implement. The rule is kept and the code now
  implements it, with one exception this document names.
- **Date:** 2026-08-22

## Context

binpack tells the operator, in three separate places, that what it decided is on the node:

- the chart's install notes, printed by `helm install`: "Every decision also lands as an Event on
  the node itself, which needs no access to binpack's logs at all", followed by the
  `kubectl describe node` command to run;
- `binpack run --help`: "Decisions surface as Kubernetes Events on the node as well as in the log,
  because on a managed control plane `kubectl describe node` is the one place a cluster user can
  reliably look";
- `values.yaml`, above `dryRun`: "a week of events on nodes tells you exactly what it would have
  done".

The justification is the load-bearing part, and it is the same one each time. On a managed control
plane the autoscaler's logs are unreachable, and binpack's own may be too — a cluster user with
`get nodes` and nothing else still has `kubectl describe node`. That is why the Event surface
exists at all rather than being a nicety on top of the log.

Until this change binpack wrote an Event only when it had chosen a node to drain, or when a drain
it had started ended. An evaluation that considered the cluster and chose nothing wrote a log line
and stopped. So the two outcomes binpack's own vocabulary names as decisions — `no-candidates` and
`none-feasible`, both published as `binpack_evaluations_total{code}` — reached the node not at all.

That is not a rare corner. It is the steady state of a healthy cluster that is already as tight as
it can be, which is the cluster most installs are pointed at: the first thing a new operator does
is follow the install notes to a command that prints nothing, on a controller that is working
perfectly.

### The argument for leaving it alone, and why it does not hold

Three surfaces already carry a refusal, and each was weighed:

- `binpack_nodes_skipped{code}` gives the true per-code counts every evaluation;
- `binpack_last_evaluation_timestamp_seconds` distinguishes "nothing to do" from "stopped
  deciding", and the metrics reference ranks it first of the three things to alert on;
- `binpack explain` prints every node's own reason, and the how-to guide designates it under
  "Check what it is refusing, and why".

All three are real and none is a substitute. Two require a Prometheus scrape; the third requires
the binpack binary and a kubeconfig with cluster-wide read. The claim the three strings make is not
that the information exists somewhere. It is that it is *on the node*, reachable with one command
by someone who has no access to anything else. Narrowing the strings to "drain decisions" would
have kept the code as it was at the price of retracting the reason the surface was built, in the
one place — the install notes — where it is printed to every new operator.

So the sentence is right and the code was wrong.

## Decision

**An evaluation that chose no node writes an Event, action `Consolidate`, on every node it
assessed — with a reason naming which wall it hit: `NoCandidates`, `NoneFeasible`, or
`NoNodeChosen` for a refusal reached by some later route.**

Five parts of that need saying, because each was a choice.

### Every assessed node, not one of them

The operator picks which node to describe, and binpack has no way to know which. A single Event on
a single node would make `kubectl describe node` answer or stay silent depending on a choice the
reader makes at random — and there is no node that deserves it more than the others, because the
decision is not about any one of them.

The cost is one Event per node per distinct refusal rather than one per evaluation, which the
Consequences below quantify.

### The note says whose answer it is

A refusal's answer is about the cluster, not about the node the Event sits on. Written bare it
would read as a claim about that node, so the note frames itself:

> binpack evaluated the cluster and chose no node to drain: nodes were simulated and none could be
> emptied onto the rest of the cluster. This is the cluster's answer, written on every node binpack
> looked at; `binpack explain` gives this node's own reason.

The pointer at the end is not filler. The per-node question — why not *this* node — has a better
answer than this Event can give, and saying where it is costs one clause.

### The wall travels in the reason, not in the note

The clause naming the wall comes from the decision's *code*, and each code gets its own Event
reason. It is not the engine's prose sentence, and it is not carried in the note.

That is forced by the aggregation rule in the section below, and the first version of this change
got it wrong: it used one reason, `NoNodeChosen`, and put the engine's sentence in the note. Since
the aggregation key does not cover the note, a cluster moving from one wall to the other — every
node ruled out before simulation, then nodes simulated and none emptiable — would have kept the
first sentence on the node for ever, under a timestamp that kept advancing. Not stale information:
a confident lie with a fresh timestamp on it.

The rule that avoids it is already in this codebase, stated in `ReasonDraining`'s own comment:
"A separate reason rather than a different note, so the two can never be confused by anything
filtering events — including a person skimming `kubectl describe node`." When the thing differs,
the reason differs.

The two names are the ones `binpack_evaluations_total{code}` already uses, so the node and the
metric say the same word. A third, `NoNodeChosen`, exists so the branch's question can stay "did
this decision name a node" rather than becoming a list of codes: a refusal code added later gets an
Event that is true but unspecific, instead of silently getting none.

### A decision that names a node is not a refusal

A drain already in progress reaches the same branch: no action, nothing chosen this evaluation. It
is not a refusal, and it already has its own Event (`Draining` while it runs, `WouldAdvanceDrain`
in dry run, `Drained` or `DrainAbandoned` when it ends). Writing "binpack chose no node" beside
those would contradict them on the node they are about. The condition that separates the two is
exactly whether the decision names a node.

### A decision reached before any node was assessed writes nothing

Without a live cluster-autoscaler binpack refuses before looking at a single node, so there are no
assessments and this writes nothing. That falls out of the rule rather than being bolted onto it —
the subjects *are* the assessed nodes — and it is right for two independent reasons. There is no
node the answer is about, since none was examined; and the sentence it would carry is the one in
the vocabulary that counts up on its own ("the cluster-autoscaler last reported 30m0s ago"), which
this surface cannot hold. That condition has `binpack_autoscaler_up`, which the metrics reference
ranks second of the three things to alert on, and it is the one refusal an operator is most likely
to already be alerting on.

### The note is decided by the reason, and by nothing else

This is a property of the surface rather than a style preference, and it comes from how
`events.k8s.io` aggregates. The key is the event type, action, reason, reporting controller,
reporting instance and the object it is about — verified in `k8s.io/client-go@v0.36.3`,
`tools/events/event_broadcaster.go`, `getKey`. **The note is not in the key**, and the consequence
is stronger than it first looks. An event whose key the recorder has already seen is not written at
all: `recordToSink` increments the cached event's series count and observed time and discards the
new event, and the patch it then sends carries only the series. A series is dropped only by
`finishSeries`, after `finishTime` — six minutes — with nothing further observed. binpack
re-decides every minute, so while a refusal holds the series never ends and the note never moves.

So the note the API server holds is the one the *first* event of that series carried, and anything
that varies underneath a fixed reason is invisible for as long as binpack keeps saying it. That
rules out two things, not one: a clock or a counter, and — less obviously, and the mistake this
document's first version made — the cluster's own changing answer. What is left in the note is
fixed prose per reason. Everything that moves lives where it can: the log line, the metrics, and
`binpack explain`.

The code expresses this by returning the pair from one function. A reason and a note that are
computed separately are two things that can drift; a reason and a note returned together cannot.

One note for both modes follows from the same rule. The drain Events distinguish dry run from
acting because what happens differs; here nothing happens either way, so a mode in the note would
split one series into two and tell the reader nothing.

## Consequences

**`NoCandidates`, `NoneFeasible` and `NoNodeChosen` are public API from the moment they ship**,
like the other five reasons and the metric names. People filter on them. Three rather than one is a
real cost in vocabulary, paid for the property above: one reason would have frozen the first
sentence onto every node.

**Volume, in the controller.** One Event object per assessed node per distinct refusal, refreshed
with a count while the refusal holds — not one per evaluation. A cluster that has nothing to
consolidate for a week holds one Event per node, and each says how many times it has been
observed. This is the same aggregation the `WouldDrain` Event has always relied on.

**Volume, in `--once`.** A one-shot run writes its Events synchronously and gets no aggregation,
because the process exits before the recorder's goroutine would post anything. That trade is
already made and documented on `reporter`; what changes is its multiplier. A CronJob invocation
that refuses now writes one Event object per node, where before it wrote none. On a large cluster
with a frequent schedule that is a real quantity of short-lived objects, bounded by the API
server's event TTL. If it proves too much, the bound to add is on the `--once` path specifically,
and this decision is not what would need revisiting.

**The engine's prose sentences do not reach the node.** They are the right answer for the log line
and for `binpack explain`, both of which are read once and rendered fresh, and three of them count
elapsed time into themselves, which this surface could not have carried in any case. What the node
gets instead is the wall, which is bounded, and a pointer to where the detail is.

**The architecture's Observability rule is now met, with one exception**, and the paragraph names
it rather than being narrowed.

## Alternatives considered

**Narrow the three strings and the spec instead.** Cheapest by far, and rejected on what it costs.
The strings do not merely assert a behaviour; they justify the whole Event surface by an argument
about what a cluster user on a managed control plane can reach. Retracting the behaviour keeps the
argument and removes the thing it argues for, in the install notes printed to every operator.

**One Event on one node per evaluation.** Half the volume and none of the property: the operator
chooses the node, so the answer would be there or not depending on a coin toss.

**One reason, `NoNodeChosen`, with the wall and the engine's sentence in the note.** This is what
the first version of this change did, and it is wrong for the reason set out above: the note is
outside the aggregation key, so the node would keep the first sentence for as long as binpack kept
refusing. It is the more tempting shape precisely because it adds one vocabulary member instead of
three, and the cost is invisible until the cluster changes.

**One Event on an object of binpack's own** — its lease, or its own Pod. Genuinely a stable
subject, and it fails the requirement: it is not on the node, which is the whole of what the three
strings promise. A `--once` run holds no lease either.

**One Event per node carrying that node's own reason** rather than the cluster's. More useful in
principle and rejected on where the answer lives. A node ruled out before simulation has its reason
as a sentence; a node that was simulated and could not be emptied has it as an arithmetic the
controller would have to re-render, duplicating what `binpack explain` exists to print. The
per-node question has a good answer already, and the note points at it.

**An Event per considered node per evaluation, or per evicted pod.** The same shape binpack already
rejected for evictions: "One eviction per evaluation would otherwise put an event on the node for
every pod, and the events worth reading are how the drain started and how it ended." A refusal that
holds is one fact, not one fact per tick, and aggregation is what keeps it that way.
