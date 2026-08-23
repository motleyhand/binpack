# Quick wins before installing binpack

Seven fixes worth doing regardless of whether you ever run this tool. Several of them recover
more capacity than binpack can, because they remove *permanent* blocks rather than optimising
around them. All of them make the cluster-autoscaler you already run work the way you assumed
it did.

If you work through all of these and your node count still doesn't drop, that is where binpack
may have something to add. Start here.

## 1. Fix PDBs that permit zero disruptions

```bash
kubectl get pdb -A
```

A row showing `ALLOWED DISRUPTIONS: 0` is worth looking at, though it is not on its own proof
that anything is held open: a budget that selects no pods reports zero as well and pins nothing.
`binpack diagnose` tells the two apart, or `kubectl get pdb -n NS NAME -o
jsonpath='{.status.expectedPods}'` does it one budget at a time — zero there means the selector
matches nothing.

Where the budget does select pods, the usual cause is a `minAvailable: 1` PDB on a Deployment
scaled to one replica — common in staging, demo and review environments, where the replica count
was tuned per environment and the PDB template was not.

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

Then read its recommendations against your current requests.

**Check `kubectl top nodes` works first.** The VPA recommender reads usage samples from the
resource Metrics API, so a broken `metrics.k8s.io` leaves it with nothing to recommend from —
and it fails quietly, producing no recommendations rather than an error. If `kubectl top` is
empty, do [fix 7](#7-fix-your-metrics-api-if-kubectl-top-is-broken) before this one.

This is the highest-leverage *structural* fix available, and it improves scheduling density,
scale-down behaviour and cost simultaneously.

## 3. Scale test environments to zero out of hours

If you run staging, demo or review environments that nobody touches overnight or at weekends,
scaling their Deployments to zero on a schedule cuts your node floor directly and with no
cleverness required.

Tools exist for this — `kube-downscaler` and similar. It only works where the quiet windows are
predictable, which for test environments they usually are even when production traffic isn't.

## 4. Name your scratch volumes as safe to evict

A pod using `emptyDir` or `hostPath` blocks eviction, because the autoscaler cannot know whether
the data matters. If it doesn't — a cache, a scratch directory, a working area rebuilt on start
— say so, naming the volumes:

```yaml
metadata:
  annotations:
    cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes: "cache,tmp"
```

The names are `volumes[].name` entries from the same pod, comma-separated. Match them exactly:
the autoscaler splits on commas and compares the pieces, so `"cache, tmp"` exempts a volume
called `" tmp"` and therefore nothing. Check each volume is genuinely disposable first — the
annotation is a promise that losing its contents is acceptable.

`cluster-autoscaler.kubernetes.io/safe-to-evict: "true"` is the broader version, and worth
knowing the difference. It covers the whole pod rather than named volumes, and it also waives
the autoscaler's refusal to evict a pod with no controller and its refusal to touch a
kube-system pod with no PodDisruptionBudget. Where the storage is what is in the way, the
per-volume annotation says that and nothing else.

**One case needs neither.** An `emptyDir` with `medium: Memory` is tmpfs — RAM with a
filesystem over it, nothing reaching the node's disk — and every cluster-autoscaler binpack
supports excludes it from what counts as local storage. Service meshes inject exactly that volume into every pod they mesh, so if
your cluster runs Istio or Linkerd, most of what looks like an `emptyDir` problem is not one:

```bash
kubectl get pods -A -o json | jq -r '
  .items[]
  | select(.spec.volumes[]? | (.hostPath != null)
      or (.emptyDir != null and .emptyDir.medium != "Memory"))
  | "\(.metadata.namespace)/\(.metadata.name)"' | sort -u
```

lists the pods where it actually is — every pod holding a volume that does block, and none of
the ones that only look like it.

## 5. Check what your provider lets you set on the autoscaler

Less is welded shut than the reputation suggests. DigitalOcean's cluster autoscaler
configuration carries `scale-down-utilization-threshold` and `scale-down-unneeded-time` as well
as a pool's minimum and maximum, so two of the three scale-down dials are yours
([`doctl kubernetes cluster update`](https://docs.digitalocean.com/reference/doctl/reference/kubernetes/cluster/update/),
read 2026-08-23); `scale-down-delay-after-add` is the one that is not. Read yours before
changing anything:

```bash
doctl kubernetes cluster get <name>
```

Raising `scale-down-utilization-threshold` widens the net — a node qualifies for removal when
its utilisation is **below** it, so the dial goes *up* if you want more scale-down. Shortening
`scale-down-unneeded-time` makes the autoscaler act sooner on a node that already qualifies.
Neither makes it rebalance, which is the gap binpack exists for, but both are free and neither
needs a new component.

If your autoscaler is instead a Deployment in your own cluster, every flag is reachable directly
and none of this applies. Its name and namespace vary by install, so look for it by substring:

```bash
kubectl get deploy -A | grep cluster-autoscaler
```

## 6. Check the expander, if you run multiple pools

`least-waste` picks the pool whose new node wastes the least capacity for the pending pods, and
it has been the cluster-autoscaler's own default since 1.33 — it was `random` before that. So
you may already have it, and this may be nothing to do. What your cluster is set to is a
read-only question; on DigitalOcean:

```bash
doctl kubernetes cluster get <name>
```

If it is not what you want:

```bash
doctl kubernetes cluster update <name> --expanders least-waste
```

Either way this affects scale-**up** only: it does nothing for scale-down, so it will not fix
the problem this project addresses.

## 7. Add topology spread constraints where you need spreading

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

## 8. Fix your Metrics API if `kubectl top` is broken

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
autoscaler now reaps the extra nodes, you might not need this tool — at least not yet.

If it still reports `NoCandidates` while sitting at 50-something percent utilisation across
every node, you may have reached the gap this project was built for: the cluster might fit on
fewer nodes, but only if the workload were rearranged, and the autoscaler will never attempt
that. Whether it actually would fit is a question `binpack explain` can answer read-only,
before you install anything.
