# Quick wins before installing binpack

Seven fixes worth doing regardless of whether you ever run this tool. Several of them recover
more capacity than binpack can, because they remove *permanent* blocks rather than optimising
around them. All of them make the cluster-autoscaler you already run work the way you assumed
it did.

If you do all of these and your node count still doesn't drop, that is when binpack has
something to add. Start here.

## 1. Fix PDBs that permit zero disruptions

```bash
kubectl get pdb -A
```

Any row showing `ALLOWED DISRUPTIONS: 0` prevents its node from ever being drained. The usual
cause is a `minAvailable: 1` PDB on a Deployment scaled to one replica — common in staging,
demo and review environments, where the replica count was tuned per environment and the PDB
template was not.

In non-production namespaces, delete the PDB. In production, run at least two replicas. Either
way, the pattern as it stands protects nothing and costs a node.

Full explanation: [the PodDisruptionBudget that costs you
money](../explanation/the-poddisruptionbudget-that-costs-money.md).

**Highest value on this list by a distance.** It is usually a one-line change.

## 2. Right-size your resource requests

The autoscaler scores nodes on requested resources, not used ones. Requests inflated three to
five times above real consumption — the default state of anything nobody revisited after the
first week — make every node look busy when none of them are.

Install the Vertical Pod Autoscaler in recommendation-only mode. It changes nothing and tells
you everything:

```yaml
spec:
  updatePolicy:
    updateMode: "Off"
```

Then read its recommendations against your current requests. Note that VPA does not use the
Metrics API, so this works even if `kubectl top` is broken.

This is the highest-leverage *structural* fix available, and it improves scheduling density,
scale-down behaviour and cost simultaneously.

## 3. Scale test environments to zero out of hours

If you run staging, demo or review environments that nobody touches overnight or at weekends,
scaling their Deployments to zero on a schedule cuts your node floor directly and with no
cleverness required.

Tools exist for this — `kube-downscaler` and similar. It only works where the quiet windows are
predictable, which for test environments they usually are even when production traffic isn't.

## 4. Annotate scratch-only `emptyDir` pods as safe to evict

A pod using `emptyDir` or `hostPath` blocks eviction, because the autoscaler cannot know whether
the data matters. If it doesn't — a cache, a scratch directory, a working area rebuilt on start
— say so:

```yaml
metadata:
  annotations:
    cluster-autoscaler.kubernetes.io/safe-to-evict: "true"
```

Check the volume is genuinely disposable before adding this. The annotation is a promise that
losing its contents is acceptable.

## 5. Set the `least-waste` expander, if you run multiple pools

```bash
doctl kubernetes cluster update <name> --expanders least-waste
```

This affects scale-**up** only: it picks the pool whose new node wastes the least capacity for
the pending pods. It does nothing for scale-down, so it will not fix the problem this project
addresses — but it is a free improvement and takes seconds.

## 6. Add topology spread constraints where you need spreading

If you are spreading replicas across nodes for availability, say so declaratively rather than
relying on a cluster-wide heuristic or on the scheduler's default behaviour:

```yaml
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        app: <name>
```

Use `ScheduleAnyway`, not `DoNotSchedule`, unless you are certain. The hard variant creates
Pending-forever failure modes when the topology cannot be satisfied — and it will also prevent
binpack from consolidating, since it cannot model hard spread constraints and refuses rather
than guessing.

If you add these, drop `RemoveDuplicates` from any descheduler policy you run; it does the same
job more bluntly and causes avoidable churn.

## 7. Fix your Metrics API if `kubectl top` is broken

```bash
kubectl top nodes
```

If this reports that metrics are unavailable, you are also missing CPU and memory-based HPA
scaling, which may be silently not working. See
[fix the Metrics API](fix-metrics-api-on-managed-kubernetes.md).

Strictly speaking this doesn't affect scale-down — the autoscaler uses requests, not usage — but
it is how you *see* the gap between requested and used, which is what makes fix 2 actionable.

## After all that

Re-run the [diagnosis](diagnose-scale-down-blockers.md) and give the cluster a few hours. If the
autoscaler now reaps the extra nodes, you're done and you don't need this tool.

If it still reports `NoCandidates` while sitting at 50-something percent utilisation across
every node, you have hit the actual gap: the cluster would fit on fewer nodes, but only if the
workload were rearranged, and the autoscaler will never attempt that. That is what binpack is
for.
