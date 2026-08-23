# ADR-0001: Build a purpose-built consolidation controller

- **Status:** accepted, and **corrected in five places** where the text asserted something about
  a third party that does not hold. All five are marked where they appear, and none of them
  reaches the decision: the autoscaler has no rebalancing code path at any configuration, which
  is the whole of the argument. (1) `LeastAllocated` is one plugin's scoring default, not the
  default profile. (2) DOKS exposes two of the three scale-down settings this ADR said were
  unavailable. (3) `least-waste` has been the cluster-autoscaler's default expander since 1.33, so
  "worth setting" asserts something about the reader's cluster. (4) Karpenter's consolidation is
  a superset only for the nodes Karpenter provisions. (5) DigitalOcean does now sell spot
  capacity, though not for the Droplets a DOKS pool uses.
- **Date:** 2026-08-15

## Context

On managed Kubernetes, the cluster-autoscaler scales up reliably and scales down poorly.

This is not a bug. The cluster-autoscaler is deliberately reactive rather than optimising. It
never asks "could I rearrange this cluster to use fewer nodes?" It asks, once per node, a much
narrower question: "is this node unneeded right now, and can its pods be evicted onto existing
nodes immediately?" It then waits for natural churn — deploys, restarts, scale events — to
produce an unambiguously empty node.

Two things make that wait indefinite in practice.

The default scheduler profile spreads pods towards the emptiest node. That is correct for
availability and directly hostile to bin-packing. And managed providers restrict the
autoscaler's dials: on DigitalOcean Kubernetes, `scale-down-delay-after-add` is unavailable and
the control plane is managed so the autoscaler's logs cannot be read.

**Correction (1).** This paragraph named `LeastAllocated` as "the default scheduler profile".
It is the default `ScoringStrategy` of one plugin — `NodeResourcesFit`, at Score weight 1 of the
12 the default profile distributes (`pkg/scheduler/apis/config/v1/default_plugins.go` and
`defaults.go`). The correction strengthens the premise rather than weakening it:
`NodeResourcesBalancedAllocation` pushes the same way at equal weight, and nothing in the
default profile rewards packing at all.

**Correction (2).** This paragraph also said DOKS exposes "minimum nodes, maximum nodes and
expander choice, and nothing else", listing `scale-down-utilization-threshold` and
`scale-down-unneeded-time` among the unavailable. Both are in fact exposed: DigitalOcean's
`cluster_autoscaler_configuration` carries `scale_down_utilization_threshold`,
`scale_down_unneeded_time` and `expanders`, settable through `doctl kubernetes cluster
create|update` (`digitalocean/doctl` `args.go`, `commands/kubernetes.go`; read 2026-08-23).
`scale-down-delay-after-add` really is absent. The claim was also stated as a property of
managed platforms generally, which does not hold either — where the autoscaler runs as a
Deployment in the operator's own cluster, as on Civo, every flag is reachable with `kubectl
edit`. Neither changes the conclusion, because the conclusion never rested on the knobs.

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

Rejected: insufficient, which is a stronger objection than unavailable. DOKS exposes minimum,
maximum, expanders, `scale-down-utilization-threshold` and `scale-down-unneeded-time`, and those
are worth setting on their own terms — but the expander only improves which pool a scale-**up**
chooses, and the two thresholds only widen or hasten the removal of a node that is *already*
nearly empty.

**Correction (3).** An earlier revision recommended setting `least-waste` outright. It has been
the cluster-autoscaler's default since 1.33 — `config/flags/flags.go` moved from
`expander.RandomExpanderName` to `expander.LeastWasteExpanderName` at that release and has kept
it through 1.36 — so on a current autoscaler this is likely to be no change at all. Per the
project's own writing rule, the documentation points at the read-only command that answers it
rather than asserting the reader's value.

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
this project does **on the nodes Karpenter provisions**. Rejected because no third party can
ship a provider for a managed Kubernetes service. See
[ADR-0005](adr-0005-why-not-a-karpenter-doks-provider.md).

**Correction (4).** The superset was originally stated without that scope. Karpenter's
disruption controller only considers nodes it owns: `ValidateNodeDisruptable`
(`pkg/controllers/state/statenode.go`) rejects a node with no `NodeClaim` outright, with
"node isn't managed by karpenter". On a cluster where a Karpenter NodePool runs alongside a
cluster-autoscaler-managed or static pool — a partial migration, or a deliberate static baseline
— the rest of the cluster gets no consolidation from Karpenter at all.

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

Not offered by DigitalOcean for the general-purpose Droplets a DOKS node pool uses.

**Correction (5).** This said "Not offered by DigitalOcean" flat. Spot GPU Droplets exist, at a
variable rate that tracks available capacity and is never above the on-demand price
([Droplet pricing](https://docs.digitalocean.com/products/droplets/details/pricing/), read
2026-08-23). Whether a DOKS node pool can be backed by them has not been established here, and
should be checked rather than assumed before this alternative is reopened.

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
