# Why the descheduler can't solve this

The [Kubernetes descheduler](https://github.com/kubernetes-sigs/descheduler) is the obvious place
to look first, and you should — it is mature, widely deployed, and does several useful things
this project has no intention of duplicating.

It also cannot close the consolidation gap, for three specific structural reasons rather than
because it is badly configured. This page is about what those reasons are, and it is written from
having run it in production rather than from reading its documentation.

## The right plugin, and the wrong one

`LowNodeUtilization` is the plugin people reach for by name, and it does the opposite of what
you want. It evicts pods **from** busy nodes **onto** idle ones. It spreads load. For cost
consolidation it is actively counterproductive.

`HighNodeUtilization` is the correct one. It identifies underutilised nodes and evicts pods
**from** them, so the workload consolidates onto fuller nodes and the emptied node becomes a
scale-down candidate. It takes only `thresholds`, with no `targetThresholds`:

```yaml
- name: HighNodeUtilization
  args:
    thresholds:
      cpu: 50
      memory: 50
      pods: 50
```

That is the right idea. What follows is what happens when you run it.

## Tuning it is a trap with no exit

Set the threshold too low and, between DaemonSets and `kube-system` overhead, every node sits
above it:

```
No node is underutilized, nothing to do here, you might tune your thresholds further
```

Raise it — 80 percent, say — and the failure mode inverts rather than resolves:

```
All nodes are underutilized, nothing to do here
```

There is a band in between where it does something, and its width depends on your node size,
your DaemonSet footprint and your workload mix. It moves when any of those change.

While you are in there, `numberOfNodes` does not mean what its name suggests. It is a *minimum
gate*: the plugin acts only if the count of underutilised nodes is **strictly greater** than the
value. The default of 0 means "act if at least one node is underutilised". Setting it to 1 makes
things worse:

```
Number of nodes underutilized is less or equal than NumberOfNodes, nothing to do here
  underutilizedNodes=1 numberOfNodes=1
```

Leave it at 0, or omit it.

## The three gaps that configuration cannot close

### 1. Percentages are blind to absolute capacity

This is the fundamental one. Consider a mixed pool of 4 GiB and 8 GiB nodes. Reservations are
roughly fixed per node, so what a pod can actually be given is a good deal less than the label
on the pool and proportionally much less on the smaller node — DigitalOcean publishes 2.5 GiB
allocatable on a 4 GiB node and 6 GiB on an 8 GiB one ([DOKS
limits](https://docs.digitalocean.com/products/kubernetes/details/limits/), read 2026-08-23;
`kubectl get node <name> -o jsonpath='{.status.allocatable}'` gives yours):

| Node | Allocatable | Utilisation | Actual |
|---|---|---|---|
| 4 GiB node | 2.5 GiB | 81% | ~2.0 GiB of requests |
| 8 GiB node | 6 GiB | 60% | ~2.4 GiB free |

The 4 GiB node's entire workload would fit on the 8 GiB node with room to spare. The
consolidation is obvious to a human and trivially provable in bytes. But the plugin sees
`81 > 80`, classifies the node as busy, and does nothing.

No threshold fixes this, because the information needed — absolute quantities — is not what the
plugin is comparing. As soon as your nodes are not all the same size, percentage classification
is measuring the wrong thing.

### 2. No cross-pool awareness

`HighNodeUtilization` consolidates within the node set it can see, and the set it can see is the
set it can evict from. Those are the same set, which is the problem.

Scope it to your autoscale pool with a `nodeSelector` and it cannot use free capacity on your
static pool as a destination. Remove the `nodeSelector` and it treats static nodes as drain
candidates — which is worse, because static nodes cannot be reaped, so evicting from them is
pure churn with no possible saving.

There is no configuration expressing "consider capacity everywhere, but only ever drain from
here." That asymmetry is exactly what a static-plus-elastic topology needs.

### 3. Thrash on nodes that are genuinely needed

Suppose node N is genuinely required, but only for one displaced pod. The descheduler classifies
N as underutilised and evicts the pod. The scheduler, using `LeastAllocated`, places it back on
N — because N is now the emptiest node in the cluster. Next run, the same thing.

The cycle repeats indefinitely. Throughput suffers, pods restart, and nothing improves, because
nothing ever asked whether evicting that pod could achieve anything.

## What it does get right

It is worth being fair, because the failure was not incompetence. A representative run:

```
Number of underutilized nodes: 2
Total capacity to be moved: CPU=1970 Mem=1534479935 Pods=357
Evicting pods from node ...
totalEvicted=16
```

It correctly identified two underutilised nodes, correctly computed that the rest of the cluster
could absorb their workload, and evicted 16 pods to do it. Within a homogeneous pool, with
sane PDBs, it does consolidate.

Every remaining failure in that run was a PodDisruptionBudget block, and every one of those was
in a test namespace:

```
Error evicting pod "...": Cannot evict pod as it would violate the pod's disruption budget
```

Which is its own lesson: before blaming any tool, check
[your PDBs](the-poddisruptionbudget-that-costs-money.md). The descheduler was working. The
cluster was configured to refuse it.

## Plugins worth running regardless

None of the above argues against the descheduler generally. These plugins are hygiene and have
nothing to do with consolidation:

- `RemovePodsHavingTooManyRestarts`
- `RemovePodsViolatingNodeTaints`
- `RemovePodsViolatingNodeAffinity`
- `RemovePodsViolatingInterPodAntiAffinity`
- `RemovePodsViolatingTopologySpreadConstraint`

One to drop if your Deployments carry `topologySpreadConstraints`: `RemoveDuplicates`. It
duplicates the same work more bluntly, evicting pods purely for distributional symmetry and
causing avoidable churn. A declarative constraint on the workload beats a cluster-wide heuristic:

```yaml
topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: kubernetes.io/hostname
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        app: <name>
```

Use `ScheduleAnyway` rather than `DoNotSchedule` unless you are certain — the hard variant
creates Pending-forever failure modes when the topology cannot be satisfied.

## What binpack does differently

Three things, corresponding to the three gaps:

- **Absolute quantities, simulated onto specific nodes.** Not percentage classification. The
  4GB-versus-8GB case above is arithmetic, and binpack does the arithmetic.
- **Capacity considered everywhere, drains only from autoscaling pools.** The asymmetry the
  descheduler cannot express is the default behaviour.
- **Feasibility decided before anything is evicted.** The descheduler evicts and finds out.
  binpack proves the workload has somewhere to go, and refuses when it doesn't — which is what
  makes the thrash case impossible rather than merely unlikely.

The design detail is in the [architecture specification](../design/2026-08-15-architecture.md).
