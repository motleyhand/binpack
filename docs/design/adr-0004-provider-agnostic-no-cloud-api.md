# ADR-0004: Discover pool bounds from the autoscaler, never from a cloud API

- **Status:** accepted. The resolution order in
  [Mapping nodes to node groups](#mapping-nodes-to-node-groups) is **superseded by
  [ADR-0012](adr-0012-pool-mapping-needs-a-value-matching-node-label.md)**: step 2 was never
  built, and there is no configured mode. Everything else here stands.
- **Date:** 2026-08-15

## Context

binpack needs three things that are not obviously part of the Kubernetes API:

1. **Is a cluster-autoscaler even running?** binpack drains nodes and relies on something else
   to delete them. Without an autoscaler it is not merely useless, it is harmful: it would
   cordon nodes that nothing will ever reap.
2. **Which node pools autoscale**, so that only their nodes are drain candidates. Draining a
   node from a static pool is pure churn — nothing will remove it.
3. **Each autoscaling pool's minimum size**, because a pool already at its minimum will
   immediately replace whatever is drained.

The obvious source is the cloud provider's API. That means a credential, a provider SDK, and a
separate implementation per provider — and it makes the tool useless on any provider nobody has
implemented yet.

## Decision

Take none of it from a cloud API. binpack requires **no cloud credentials of any kind**. Its
only permission is a Kubernetes RBAC role in the cluster it runs in.

### The autoscaler publishes its own configuration

The cluster-autoscaler writes a `cluster-autoscaler-status` ConfigMap into `kube-system`. This
is present and fully populated even on managed control planes where the autoscaler's pods and
logs are invisible — verified on DigitalOcean Kubernetes:

```yaml
autoscalerStatus: Running
clusterWide:
  scaleDown: {status: NoCandidates}
  scaleUp:   {status: NoActivity, lastTransitionTime: "..."}
nodeGroups:
  - name: da8977ba-244f-4cfe-9ea1-834a39370f6d
    health:
      minSize: 0
      maxSize: 24
      cloudProviderTarget: 2
```

That single object answers all three questions:

- `autoscalerStatus` is the **preflight check**. Absent ConfigMap or a non-`Running` status
  means binpack refuses to act.
- `nodeGroups` lists **only autoscaling-managed groups**. A pool absent from this list is
  static, and its nodes must never be drain candidates. On the cluster this was verified
  against, one of two pools was absent — correctly identifying it as static, with no
  configuration required.
- `minSize` and `maxSize` are the pool bounds, straight from the component that enforces them.

It also yields two signals worth more than what was originally designed. `scaleUp
lastTransitionTime` gives a precise cooldown reference, replacing an inference from node
creation timestamps. And `scaleDown.status: NoCandidates` is the autoscaler stating binpack's
entire reason for existing in its own words — which makes it an excellent thing to surface in
`diagnose`.

### Mapping nodes to node groups

One gap remains: the node group `name` is a provider-specific identifier — a pool UUID on
DOKS, an Auto Scaling group name on AWS, a VMSS name on Azure. Mapping a node to its group
therefore needs a node label whose **value equals that name**.

DOKS provides exactly this as `doks.digitalocean.com/node-pool-id`. Not every provider will.

> **Superseded by [ADR-0012](adr-0012-pool-mapping-needs-a-value-matching-node-label.md).**
> Step 2 below was published, reasoned about in the Costs section, repeated in the architecture
> document — and never implemented. There is no configuration field that states a pool's
> membership or its minimum, and `v1alpha1.Load` uses `UnmarshalStrict`, so a document trying to
> is rejected outright. binpack has one resolution mode, not two: a node label whose value is the
> autoscaler's own identifier for the group. The order is kept below as the record of what was
> intended; read ADR-0012 for what binpack does.

So the resolution order is:

1. **Discovered.** If a configured `nodeGroupIDLabel` is present on nodes and its values match
   node group names in the status ConfigMap, use discovered bounds. Nothing to configure beyond
   the label key, which ships as a per-provider documented default.
2. **Configured.** Otherwise, fall back to pool membership by label and operator-stated
   minimums.
3. **Preflight always works.** The "is an autoscaler running?" check needs no mapping at all,
   so it applies on every provider regardless.

## Consequences

### Benefits

- Works on any managed Kubernetes with no per-provider Go code, no provider SDK, and no vendor
  to keep up with.
- No API token to create, mount, rotate or leak. For a tool asking permission to drain nodes,
  "it holds no cloud credentials" is a materially easier conversation than any amount of
  reassurance about how carefully a token is handled.
- Where discovery works, there is no configuration to drift. Pool bounds come from the
  component that enforces them, so they are correct by construction.
- binpack refuses to run where it cannot work, instead of quietly cordoning nodes nothing will
  reap.

### Costs

> **The mode this section costs out does not exist**; see the note above. What follows is kept
> because the safeguard it argues for is real, load-bearing and unchanged — only its stated
> justification narrows, to the second reason given at the end.

In configured mode, the stated minimum can drift from reality — someone raises the pool minimum
in a console and does not update binpack.

The failure is bounded: binpack drains a node, the autoscaler declines to delete it, and the
node sits cordoned with its workload moved elsewhere. Nothing is destroyed.

But a permanently cordoned node **is** lost schedulable capacity, and that is not acceptable as
a resting state. If the pool is also at its maximum, later pods can stay Pending indefinitely
while a perfectly good node sits idle and cordoned. So the executor carries a mandatory
safeguard: **if a drained node still exists after a configured timeout, binpack uncordons it**
and records the drain as failed. A metric alone is not sufficient — the cluster must be left in
a working state without human intervention.

Discovery makes this particular failure theoretical — the bounds come from the component that
enforces them — but the safeguard stands on its own remaining justification, which is the
sufficient one: a drain can fail to produce a deletion for reasons beyond a stale minimum.

### Not decided here

Whether to offer optional, opt-in cloud API integration for providers with no usable mapping
label. It would remain optional and binpack would work without it, but no such provider has
been identified yet, so the question is deferred until one is.
