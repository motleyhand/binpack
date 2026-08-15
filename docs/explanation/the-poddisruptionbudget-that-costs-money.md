# The PodDisruptionBudget that costs you money

There is a configuration that appears in an enormous number of clusters, protects nothing at
all, and permanently prevents a node from ever being removed. It is worth knowing about whether
or not you ever install binpack, because no consolidation tool can work around it — it blocks
`kubectl drain` and the cluster-autoscaler just as completely.

## The pattern

```yaml
# Deployment
spec:
  replicas: 1
---
# PodDisruptionBudget
spec:
  minAvailable: 1
```

A PDB's allowance is `currentHealthy − minAvailable`. With one replica and `minAvailable: 1`,
that is `1 − 1 = 0`.

**Zero voluntary disruptions. Permanently.** Not "until things settle", not "under load" —
always. The Eviction API refuses every request. The autoscaler cannot drain the node, so the
node lives forever, and you pay for it forever.

The PDB is protecting nothing. A single replica has no availability guarantee to protect: if
that pod dies for any *involuntary* reason — node failure, OOM kill, hardware fault — it goes
down regardless, because PDBs govern only voluntary disruption.

So it costs real money and provides no benefit whatsoever.

## Where it comes from

Almost nobody writes this deliberately. It is what you get by combining two individually
sensible decisions:

1. Production runs three replicas with a PDB of `minAvailable: 1`. Correct: two replicas of
   slack, rollouts and drains proceed safely.
2. Staging, demo and review environments run **one** replica to save money — using the same Helm
   chart or manifest template, PDB included.

The replica count was tuned per environment. The PDB was not. The result is that the environments
you care least about are the ones nailing nodes to the floor, and the saving from scaling to one
replica is more than cancelled by the node that can now never be reclaimed.

## Finding it

```bash
kubectl get pdb -A
```

Every row showing `ALLOWED DISRUPTIONS: 0` is a node anchor. If the matching workload has one
replica, it is anchored permanently rather than momentarily.

## Fixing it

In rough order of cleanliness.

**Delete the PDB in non-production namespaces.** With `strategy: RollingUpdate`, `maxSurge: 1`,
`maxUnavailable: 0`, your rollouts are already zero-downtime without it. The PDB governs only
*involuntary* disruption — which, in a test environment, is exactly what you want to permit.

**Or invert the condition:** replace `minAvailable: 1` with `maxUnavailable: 1`. Same shape of
protection, opposite phrasing, and it permits eviction at the cost of roughly thirty seconds of
downtime while the replacement starts.

**In production, keep the PDB and run at least two replicas**, so `minAvailable: 1` leaves
genuine slack.

## Why `maxSurge` does not save you

This is the most common misconception about the pattern, and it is worth understanding because
it explains behaviour that otherwise looks random.

The reasoning goes: my Deployment has `maxSurge: 1`, so Kubernetes will start a replacement
before removing the old pod, so eviction should be fine.

It isn't. Surge belongs to the Deployment controller's **rollout** path, which runs when the pod
template changes. Eviction is a **deletion**: the ReplicaSet notices a missing replica and
creates a new one *after* the old one is gone. There is no create-first path for eviction
anywhere in core Kubernetes.

What *is* true — and this is the part that explains the randomness — is that **during** a
rollout there are transiently two healthy pods, so the allowance briefly becomes 1 and the PDB
briefly stops blocking. That is the "natural churn" the autoscaler is waiting for. It is why a
blocked cluster sometimes shrinks a few minutes after an unrelated deploy, for no reason anyone
can identify at the time.

It is not something to rely on. The window is short, it is not correlated with when you need it,
and a drain that begins inside one and continues after it closes leaves a half-drained node.

## The other PDB trap: two budgets, one pod

Rarer, harder to spot, and strictly worse.

If a pod is selected by **more than one** PodDisruptionBudget, the Eviction API does not
arbitrate between them. It refuses outright:

> This pod has more than one PodDisruptionBudget, which the eviction subresource does not
> support.

That is returned as an HTTP **500** — not a 429, not retryable, not time-limited. The pod cannot
be evicted by anything: not `kubectl drain`, not the cluster-autoscaler, not binpack. Its node
is permanently undrainable.

The reason it is hard to spot is that nothing looks wrong. Both PDBs report healthy status and
sensible allowed disruptions, because each is computed independently and neither is aware of the
other. Overlapping selectors are easy to create by accident — a team-wide budget selecting
`team: platform` alongside a per-service budget selecting `app: api`, on a pod carrying both
labels.

To find them, list PDB selectors per namespace and check for pods matching more than one.
`binpack diagnose` will report this directly.

## Why this is worth fixing first

If you take one thing from this project's documentation, take this one. Auditing your PDBs costs
a few minutes. On many clusters it recovers more capacity than any consolidation tool can,
because it removes a *permanent* block rather than optimising around it — and it makes every
other mechanism, including the autoscaler you already run, work the way you assumed it did.
