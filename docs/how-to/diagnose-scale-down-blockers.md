# Diagnose why your cluster won't scale down

A sequence of read-only commands for working out why a node is still there. Nothing here
modifies anything, so it is safe to run against production.

This works on managed Kubernetes where the autoscaler's own logs are unreachable. For the
background on *why* each of these matters, see
[why your cluster doesn't shrink](../explanation/why-clusters-dont-shrink.md).

## 1. Ask the autoscaler directly

The most useful object in the cluster, and the least known. The cluster-autoscaler writes its
status into `kube-system`, and this works even on a managed control plane where its pods and
logs are invisible to you:

```bash
kubectl -n kube-system get configmap cluster-autoscaler-status -o jsonpath='{.data.status}'
```

You get, in the autoscaler's own words:

- `autoscalerStatus` — whether it is running at all
- `clusterWide.scaleDown.status` — `NoCandidates` means it has looked and found nothing to
  remove. That single line distinguishes "the autoscaler is stuck" from "the autoscaler is
  working and your cluster genuinely needs these nodes"
- `nodeGroups[]` — **only** the pools it actually manages, each with `minSize`, `maxSize` and
  `cloudProviderTarget`

That last point is worth dwelling on. Any pool *absent* from `nodeGroups` is not autoscaling,
which means nothing will ever remove its nodes no matter how empty they get. Comparing this list
against your actual pools catches a surprising number of "why won't this shrink" questions
outright.

If the ConfigMap does not exist, the likeliest explanation by far is that no cluster-autoscaler
is running — in which case nothing else in this document will help, because nothing is going to
remove a node however empty it gets. It can also mean the autoscaler is running with status
reporting disabled, which is unusual on managed platforms but worth ruling out before concluding
anything.

## 2. Check for PDBs allowing zero disruptions

The most common single cause, and a one-line check:

```bash
kubectl get pdb -A
```

Any row with `ALLOWED DISRUPTIONS: 0` is pinning its node. If the workload has one replica, it
is pinned permanently — see
[the PodDisruptionBudget that costs you money](../explanation/the-poddisruptionbudget-that-costs-money.md).

## 3. Read the events the autoscaler leaks

Its decisions surface as events even when its logs don't:

```bash
kubectl get events -A --field-selector source=cluster-autoscaler
kubectl get events -A | grep -iE 'scale|autoscaler|evict|TriggeredScaleUp'
```

`TriggeredScaleUp` events are also how you learn the shape of your bursts — whether you are
adding one node at a time or several, which is the input to any decision about node sizing.

## 4. Look at requests versus allocatable, per node

```bash
kubectl describe node <name>
```

The "Allocated resources" section is what the autoscaler actually scores on. Compare it against
`kubectl top node <name>` if you have a working Metrics API: a large gap between requested and
used means your requests are inflated, and inflated requests make every node look busy. That is
the highest-leverage thing you can fix.

If `kubectl top` reports that metrics are unavailable, see
[fix the Metrics API](fix-metrics-api-on-managed-kubernetes.md).

## 5. See exactly what is pinned to a node

```bash
kubectl get pods -A -o wide --field-selector spec.nodeName=<name>
```

Work down the list asking which of these could not move. DaemonSet pods are fine — they are
recreated per node and don't need to relocate. What you are hunting for is a bare pod with no
controller, a pod with `safe-to-evict: "false"`, or a pod whose affinity only this node
satisfies.

## 6. Find the biggest requests in the cluster

```bash
kubectl get pods -A -o json | jq -r '
  .items[] | "\(.metadata.namespace)/\(.metadata.name) \(.spec.containers[].resources.requests.memory // "none")"
' | sort -k2 -h | tail -20
```

One oversized request can be the reason a node cannot be emptied — there may be nowhere else in
the cluster with a single contiguous block that large. Aggregate free capacity across the
cluster is irrelevant here: a 3GB pod does not fit in three nodes with 1GB free each.

## 7. Count pods per node

```bash
kubectl get pods -A -o wide --no-headers | awk '{print $8}' | sort | uniq -c
```

The kubelet's default ceiling is 110. A node at that limit is unschedulable regardless of how
much CPU and memory it has free, and no CPU/memory dashboard will show you this.

## 8. Check for pods matched by more than one PDB

Rare and easy to miss, because nothing looks wrong. A pod selected by two PDBs cannot be evicted
by anything — the Eviction API returns HTTP 500 rather than arbitrating, permanently.

There is no neat one-liner; you need to compare each PDB's selector against pod labels in its
namespace. `binpack diagnose` will report it directly.

## What to do with the answer

If step 1 said `NoCandidates` and steps 2–8 found nothing, then at the current placement the
cluster does need its nodes — which is the case binpack was written for. The workload may still
fit on fewer nodes if it were rearranged, and the autoscaler will never try that. Whether it
would fit on yours is an arithmetic question, and `binpack explain` answers it read-only.

If steps 2–8 found something, fix that first. It is cheaper than any tool, and several of those
blocks would defeat a consolidation tool as thoroughly as they defeat the autoscaler. The
[quick wins](quick-wins-before-installing-binpack.md) list covers the fixes.
