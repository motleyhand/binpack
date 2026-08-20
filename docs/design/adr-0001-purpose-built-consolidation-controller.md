# ADR-0001: Build a purpose-built consolidation controller

- **Status:** accepted
- **Date:** 2026-08-15

## Context

On managed Kubernetes, the cluster-autoscaler scales up reliably and scales down poorly.

This is not a bug. The cluster-autoscaler is deliberately reactive rather than optimising. It
never asks "could I rearrange this cluster to use fewer nodes?" It asks, once per node, a much
narrower question: "is this node unneeded right now, and can its pods be evicted onto existing
nodes immediately?" It then waits for natural churn — deploys, restarts, scale events — to
produce an unambiguously empty node.

Two things make that wait indefinite in practice.

The default scheduler profile is `LeastAllocated`, which spreads pods towards the emptiest
node. That is correct for availability and directly hostile to bin-packing. And most managed
providers weld the autoscaler's dials shut: DigitalOcean Kubernetes exposes minimum nodes,
maximum nodes and expander choice, and nothing else. `scale-down-utilization-threshold`,
`scale-down-unneeded-time` and `scale-down-delay-after-add` are unavailable, and the control
plane is managed so the autoscaler's logs cannot be read.

The result is a stable, symmetric, expensive equilibrium: every node sitting at 40–70 percent
of requested capacity, none of them clearly removable, all of them billed.

The behaviour that prompted this project: a cluster with a floor of five nodes routinely sat at
six or seven for days or weeks after a load spike receded. Deleting a single pod by hand from
the extra node broke the symmetry, the replacement landed elsewhere, and the autoscaler reaped
the node within minutes. The capacity was genuinely unnecessary. The autoscaler simply never
found the opening on its own.

## Decision

Build a small, purpose-built controller that answers the one question the autoscaler never
asks: **if this specific node were drained, would every one of its pods actually be schedulable
onto the remaining nodes, without triggering a scale-up?** Drain only when the answer is yes.

Answering it properly means simulating placement pod by pod onto specific nodes, not comparing
percentages and not summing free capacity across the cluster — aggregate capacity is necessary
but not sufficient for schedulability, and getting that wrong causes exactly the scale-up this
tool exists to prevent. It is not algorithmically difficult. It is simply not what any existing
tool does.

## Alternatives considered

### Tune the cluster-autoscaler

Rejected: not possible. DOKS exposes minimum, maximum and expanders. The `least-waste` expander
is worth setting if you run multiple pools, but it improves which pool a scale-**up** chooses
and does nothing for scale-down.

More fundamentally, this would not be sufficient even if the knobs were available. The
autoscaler has no rebalancing code path at any configuration. Raising
`scale-down-utilization-threshold` widens the set of nodes it will consider removing — a node
qualifies when its utilisation is *below* the threshold — but a wider net only helps if the
cluster ever produces a nearly-empty node. Tuning does not make it arrive at that state.

### Right-size resource requests

Adopted, but not as a substitute. The autoscaler scores nodes on requests, not usage, so
requests inflated three to five times above real consumption make every node look busy. VPA in
recommendation-only mode (`updateMode: "Off"`) surfaces the gap without changing anything.

This is the highest-leverage structural fix and should be done regardless — see
[quick wins](../how-to/quick-wins-before-installing-binpack.md). It does not solve the problem,
because right-sized requests still leave every node at 60 percent with no clear
consolidation candidate.

### The Kubernetes descheduler

Rejected after being deployed and observed in production. The `HighNodeUtilization` plugin is
the right idea and has three structural gaps that no configuration closes: it classifies nodes
by percentage and is therefore blind to absolute capacity on mixed node sizes; it cannot
consolidate across node pools; and it will thrash on a node that is genuinely needed. The full
analysis, including what it does well, is in
[why the descheduler can't solve this](../explanation/why-not-descheduler.md).

### Karpenter

Architecturally the correct answer, and its consolidation feature is a strict superset of what
this project does. Rejected because no third party can ship a provider for a managed
Kubernetes service. See [ADR-0005](adr-0005-why-not-a-karpenter-doks-provider.md).

### CAST AI

Commercial, genuinely solves this problem, and supports DigitalOcean. Rejected on price: entry
pricing exceeds the savings by an order of magnitude on a small cluster. A reasonable choice
once infrastructure spend reaches four figures per month.

### Kubecost / OpenCost

Visibility only. Useful for deciding whether right-sizing is worth the effort. Does not move
pods.

### Scheduled scale-to-zero (kube-downscaler and similar)

Effective when traffic is predictable, and an excellent fit for test, staging and review
environments specifically. Rejected as a general answer because it cannot respond to
unpredictable load. Complementary rather than competing.

### Spot or preemptible nodes

Not offered by DigitalOcean.

### Manual `kubectl drain`

Works perfectly and costs thirty seconds of attention whenever someone happens to notice. This
project is, in essence, "automate this correctly" — with the feasibility check that a human
performs implicitly and a cron job would not.

## Consequences

- The project's scope is deliberately narrow. It does not provision nodes, choose instance
  types, or replace the cluster-autoscaler. It nudges an autoscaler that is already working
  into a state where its own scale-down logic can fire.
- Because it cooperates with the existing autoscaler rather than replacing node lifecycle
  management, adoption risk is low: binpack provisions nothing and owns no part of the node
  lifecycle, so removing it hands the cluster back to the autoscaler that was already running
  it. The exception is a node binpack is draining at that moment, which is left cordoned and
  marked with nothing running to release it — accepted deliberately, and the marker is what
  makes the state self-describing rather than mysterious — see
  [the architecture](2026-08-15-architecture.md) on recovery state outliving the process.
- The value proposition depends entirely on the feasibility check being *correct*. A tool that
  drains a node whose workload does not fit elsewhere is worse than no tool, because it causes
  churn and an immediate scale-up. This is why the decision engine is a pure, exhaustively
  tested function ([ADR-0003](adr-0003-pure-decision-engine.md)) and why the read-only
  `explain` command ships before anything that acts.
