# Diagnostics reference

Every code `binpack diagnose` can report, what it means, and what to do about it.

```bash
binpack diagnose
binpack diagnose --output json
```

`diagnose` is read-only and **never remediates**. It tells you what to change and leaves the
changing to you — partly because a cost tool should not quietly rewrite availability policy,
and partly because Flux or Argo would reconcile the change away within minutes, leaving you
with a tool whose reported actions did not happen.

It needs no configuration and no binpack installation. Most of what stops a cluster
consolidating also stops the cluster-autoscaler, so the report is worth reading before you
decide whether you want the controller at all.

## How to read the report

Findings are grouped by **code**, because these conditions arrive in bulk: fifteen namespaces
deploying one Helm chart produce fifteen identical disruption budgets, which is one mistake and
not fifteen. Each group states the diagnosis and the fix once, then lists the objects it
applies to.

The code is the stable part. Prose may be reworded between releases; codes are not, so they are
what to grep for, alert on, and link to.

### Severities

| Severity | Meaning |
|---|---|
| `blocking` | The eviction API itself refuses. No setting of binpack's or the autoscaler's changes the outcome — the pods cannot be evicted at all, so their nodes cannot be removed by anything, including `kubectl drain` |
| `warning` | The cluster-autoscaler declining by policy, or a condition that resolves itself. One annotation, one budget, or one recovered replica changes it |
| `info` | The cluster working as intended. Reported so a question already answered does not have to be asked again |

Severity is a property of the code, not a judgement about an individual object. What a
condition costs does not vary by which namespace it is in.

### "frees nothing today"

A finding on a node whose pool the autoscaler does not manage is real but currently costs
nothing: that node was never going to be removed. Such findings are marked rather than hidden —
they go live the moment autoscaling is enabled on the pool — but they are not worth acting on
first. In JSON this is `"freesNothing": true`.

---

## Cluster and pool

### `no-autoscaler` — blocking

No cluster-autoscaler is running, so nothing will remove a node however empty it becomes.

binpack refuses to act at all in this state: draining a node nothing will reap is strictly
worse than doing nothing. The check is deliberately strict — a status ConfigMap outlives the
autoscaler that wrote it and keeps reporting `Running` indefinitely, so binpack also requires a
recent probe time.

**Fix.** Check that cluster autoscaling is enabled for the cluster, and that
`kube-system/cluster-autoscaler-status` is being updated. When there is no autoscaler, the pool
checks are skipped entirely: every pool is absent from a status document that does not exist,
and reporting each one would bury the single answer that matters.

### `autoscaler-no-candidates` — info

The cluster-autoscaler has looked for a node to remove and found none.

This is not a fault, and it is the exact state binpack exists to resolve. The autoscaler only
removes nodes that are *already* nearly empty; it never rebalances to create one. See
[why your cluster doesn't shrink](../explanation/why-clusters-dont-shrink.md).

### `pool-at-minimum` — info

The pool is at its configured minimum size, so no node will be removed from it whatever the
utilisation.

**Fix.** Lower the pool's minimum if it is set higher than you need. binpack will not drain a
node out of a pool at its minimum either — that would cause an immediate scale-up.

### `pool-not-autoscaling` — info

The pool is absent from the cluster-autoscaler's status, so it is not autoscaled.

Its nodes can receive relocated work but will never be removed, however empty they get.
binpack treats such pools as destinations and never as drain candidates, which is the
cross-pool asymmetry the descheduler cannot express.

**Fix.** Enable autoscaling on the pool if you expected it to shrink. Otherwise nothing to do.

## Disruption budgets

### `pdb-zero-disruptions` — blocking

The budget permits no voluntary disruption while its workload is healthy, so no node running
its pods can be drained by anything.

This is the single most expensive misconfiguration in the catalogue, and the most common. A
one-replica Deployment with `minAvailable: 1` protects nothing — there is no availability left
to preserve — while pinning whichever node it lands on, permanently. The budget reports healthy
the entire time.

**Fix.** Run one more replica, or express the budget so it leaves slack. See
[the PodDisruptionBudget that costs money](../explanation/the-poddisruptionbudget-that-costs-money.md).

### `pdb-workload-unhealthy` — warning

The budget permits no disruption because its workload is currently short of replicas.

A different problem with a different owner, and reported separately so you do not go and edit a
budget that is behaving exactly as intended.

**Fix.** Nothing in the budget. Find out why the replicas are unhealthy; it will permit
disruptions again once they recover.

### `pdb-selects-nothing` — warning

The budget selects no pods at all, so it protects nothing.

Reported for the opposite reason to the others: it pins no node, but whoever wrote it believes
a workload is protected when nothing is. Invisible from the object itself unless you read
`status.expectedPods`.

**Fix.** Almost always a selector that no longer matches the workload's labels — a rename, or a
chart whose pod labels changed. Correct the selector, or delete the budget if the workload is
gone.

### `pdb-stale` — warning

The budget was edited and its controller has not caught up, so the eviction API refuses
disruptions until it does.

The eviction subresource compares `status.observedGeneration` against `metadata.generation` and
rejects outright when the status is behind, whatever the recorded allowance says.

**Fix.** Usually momentary; re-run. If it persists, the disruption controller is not running.

### `multiple-pdbs` — blocking

The pods are selected by more than one PodDisruptionBudget.

The eviction API does not arbitrate between budgets. It returns **HTTP 500** — not a retryable
429 — so such a pod cannot be evicted by anything: not binpack, not the autoscaler, not
`kubectl drain`. Every budget involved reports healthy, which is what makes this so hard to
find by hand.

**Fix.** Narrow the selectors until exactly one budget covers each pod.

## Workloads

Workload findings are collapsed by controller, so a twenty-replica Deployment mounting an
`emptyDir` is reported as one finding with a count rather than twenty. Bare pods have no
controller to group by and stay separate, because each is individually something you have to
deal with. The detail names the node, so you can weigh the finding against the cost of the node
it is holding open.

### `bare-pod` — blocking

The pods have no *controlling* owner, so evicting one would delete it permanently.

Nothing will do that: not binpack, and not the cluster-autoscaler, for the same reason. Note
that an owner reference with `controller` unset does not count — those exist for garbage
collection, and nothing is responsible for replacing the pod.

**Fix.** Give the pod a controller — a Deployment or a Job — or accept that its node cannot be
drained while it is there.

### `local-storage` — warning

The pods use an `emptyDir` or `hostPath` volume, which the cluster-autoscaler will not disturb
by default in case the contents matter.

The report names the volume, because that is what makes the decision possible: `wasm-cache`
answers "is this disposable?" and `data` does not.

**Fix.** Annotate the pod `cluster-autoscaler.kubernetes.io/safe-to-evict=true` when the volume
holds nothing that must survive — a cache, a scratch directory, a rendered config. That is the
common case and it is a one-line change.

### `system-pod` — warning

kube-system pods with no PodDisruptionBudget. The autoscaler will not remove a node running
one.

**Fix.** Give the workload a PodDisruptionBudget. Per the autoscaler's own documentation, a
budget overrides its refusal to touch the node — and the budget then governs, like any other.

### `mirror-pod` — blocking

Static pods, created by the kubelet from an on-disk manifest. They cannot be evicted.

**Fix.** Nothing from inside the cluster. The node cannot be drained while a static pod runs on
it. Rare on managed Kubernetes, where the control plane is not yours.

### `safe-to-evict-false` — info

The pods are annotated `cluster-autoscaler.kubernetes.io/safe-to-evict=false`.

**Fix.** Deliberate — the annotation says so. Remove it if the pod can in fact move.

### `priority-below-cutoff` — warning

The pods sit below the cluster-autoscaler's expendable-pods priority cutoff, so it ignores them
entirely — **including when they are Pending**.

This is the silent failure in most overprovisioning setups. If these are pause pods holding
warm capacity, that capacity is consumed by the first burst and never replenished, because the
autoscaler will not scale up for a pod below its cutoff. Nothing anywhere reports this. See
[overprovisioning and expendable pods](../explanation/overprovisioning-and-expendable-pods.md).

**Fix.** Raise the priority to at or above the cutoff. If these really are throwaway pods,
nothing to do.

## binpack's own state

### `abandoned-drain` — warning

The node is cordoned and still carries a binpack drain marker.

**Fix.** If binpack is running it will finish or abandon the drain by itself. If it is not —
uninstalled mid-drain, most likely — uncordon the node and remove its
`binpack.motleyhand.com/` annotations, or it stays cordoned and billed. The marker exists
precisely so this is a self-describing state rather than a mysteriously cordoned node nobody
dares touch.

### `node-in-backoff` — info

binpack failed to drain the node and is waiting before trying again. The detail carries the
recorded reason.

**Fix.** It will retry on its own. The recorded reason is what to fix if it keeps failing.

## JSON output

One flat record per finding, so filtering does not require a join:

```bash
binpack diagnose --output json | jq '[.[] | select(.severity == "blocking" and .freesNothing != true)]'
```

| Field | Notes |
|---|---|
| `severity` | `blocking`, `warning`, or `info` |
| `code` | Stable identifier, as listed above |
| `subject` | The object: a node, a pool, a workload, a `namespace/name`, or `cluster` |
| `detail` | What is true of this subject in particular. Omitted when there is nothing to add |
| `summary` | What the condition is. Identical across every finding sharing a code |
| `fix` | What to change. Identical across every finding sharing a code |
| `freesNothing` | Present and `true` when clearing this would not shrink the cluster today |

A cluster with nothing to report yields `[]`, never `null`.

## See also

- [Diagnose scale-down blockers](../how-to/diagnose-scale-down-blockers.md) — the same
  investigation by hand, with `kubectl`
- [Quick wins before installing binpack](../how-to/quick-wins-before-installing-binpack.md) —
  fixes worth making regardless of whether you run the controller
