# Diagnose why your cluster won't scale down

A sequence of read-only commands for working out why a node is still there. Nothing here
modifies anything, so it is safe to run against production.

> **`binpack diagnose` does all of this in one command**, including the checks that are hard to
> do by hand — a budget matching no pods, a pod matched by two. It needs no configuration and no
> installation, and `--fail-on blocking` makes it a CI gate. See the
> [diagnostics reference](../reference/diagnostics.md).
>
> The manual sequence below is still worth knowing: it is what the tool is doing, it works when
> you have no binary to hand, and it shows you the raw objects rather than binpack's reading of
> them.

This works on managed Kubernetes where the autoscaler's own logs are unreachable. For the
background on *why* each of these matters, see
[why your cluster doesn't shrink](../explanation/why-clusters-dont-shrink.md).

## 1. Ask the autoscaler directly

The most useful object in the cluster, and the least known. The cluster-autoscaler writes its
status into the namespace it runs in, and reading it works even on a managed control plane where
its pods and logs are invisible to you:

```bash
kubectl get configmap cluster-autoscaler-status -A
kubectl -n <that namespace> get configmap cluster-autoscaler-status -o jsonpath='{.data.status}'
```

`kube-system` is the common answer and not the only one — the autoscaler's own Helm chart sets
its `--namespace` to whatever namespace you install it into. Whichever the first command names
is also what binpack's `discovery.autoscalerNamespace` has to be set to.

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

If the ConfigMap does not exist, the likeliest explanation is that no cluster-autoscaler is
running — in which case nothing else here will help, because nothing is going to remove a node
however empty it gets. But treat that as a hypothesis, not a finding. The autoscaler can be
running with status reporting turned off, and an RBAC or reconciliation failure can prevent the
object being written while scaling continues perfectly well. Confirm with step 3: if you see
recent `TriggeredScaleUp` events, an autoscaler is clearly at work regardless of what this step
suggested, and the rest of the checks apply.

## 2. Check for PDBs allowing zero disruptions

The most common single cause, and a one-line check:

```bash
kubectl get pdb -A
```

A row with `ALLOWED DISRUPTIONS: 0` is where to look next, not the answer by itself: a budget
that selects no pods reports zero too, and pins nothing. Settle which you have with
`kubectl get pdb -n NS NAME -o jsonpath='{.status.expectedPods}'` — zero means the selector
matches nothing — or with `binpack diagnose`, which separates them as `pdb-zero-disruptions` and
`pdb-selects-nothing` across the cluster.

Where the budget does select pods and the workload has one replica, its node is pinned
permanently — see
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

One oversized request can be the reason a node cannot be emptied — there may be nowhere else in
the cluster with a single contiguous block that large. Aggregate free capacity is irrelevant
here: a 3GB pod does not fit in three nodes with 1GB free each.

What you want is the **effective pod request**, which is not the same as reading
`resources.requests` off each container. The scheduler reserves the larger of the sum across
regular containers and the peak across init containers, keeps native sidecars in the running
total, and adds `spec.overhead` from the RuntimeClass. A pod with three 1GiB containers, or a
light main container behind a 4GiB init container, is far bigger than a per-container listing
suggests.

```bash
kubectl get pods -A -o json | jq -r '
  def to_bytes:
    if . == null then 0
    elif type == "number" then .
    elif test("Ki$") then (rtrimstr("Ki")|tonumber)*1024
    elif test("Mi$") then (rtrimstr("Mi")|tonumber)*1048576
    elif test("Gi$") then (rtrimstr("Gi")|tonumber)*1073741824
    elif test("Ti$") then (rtrimstr("Ti")|tonumber)*1099511627776
    else (tonumber? // 0) end;
  .items[] | . as $p
  | [ $p.spec.containers[]?.resources.requests.memory | to_bytes ] as $reg
  | [ $p.spec.initContainers[]? | select(.restartPolicy == "Always")
      | .resources.requests.memory | to_bytes ] as $side
  | [ $p.spec.initContainers[]? | select(.restartPolicy != "Always")
      | .resources.requests.memory | to_bytes ] as $init
  | (($reg|add) // 0) as $r | (($side|add) // 0) as $s | (($init|max) // 0) as $i
  | (($p.spec.overhead.memory // 0) | to_bytes) as $o
  | ((([$r + $s, $i + $s] | max) + $o) / 1048576 | floor) as $mib
  | "\($mib)Mi \($p.metadata.namespace)/\($p.metadata.name)"
' | sort -rn | head -20
```

This is a close approximation rather than the exact upstream calculation, and it is deliberately
biased towards overstating rather than understating. `binpack explain` computes the real figure
using the same library the scheduler does.

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
