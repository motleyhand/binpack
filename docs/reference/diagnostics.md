# Diagnostics reference

Every code `binpack diagnose` can report, what it means, and what to do about it.

```bash
binpack diagnose
binpack diagnose --output json
binpack diagnose --fail-on blocking     # for CI
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
| `blocking` | The node stays open until the object itself changes — a budget rewritten, a controller added, a manifest removed. Nothing binpack or the cluster-autoscaler can be configured to do clears one. `no-autoscaler` is the exception, and is described on its own below |
| `warning` | The cluster-autoscaler declining by policy, or a condition that resolves itself. One annotation, one budget, or one recovered replica changes it |
| `info` | The cluster working as intended. Reported so a question already answered does not have to be asked again |

Severity is a property of the code, not a judgement about an individual object. What a
condition costs does not vary by which namespace it is in.

### "frees nothing today"

A finding on a node whose pool the autoscaler does not manage is real but currently costs
nothing: that node was never going to be removed. Such findings are marked rather than hidden —
they go live the moment autoscaling is enabled on the pool — but they are not worth acting on
first. In JSON this is `"freesNothing": true`.

## Exit codes

`diagnose` exits **0 whatever it finds**, by default. Reporting is the job; failing a build is
opt-in, because a diagnostic that broke pipelines the day it was installed would be uninstalled
the same day.

`--fail-on` turns it into a gate:

| Value | Fails on |
|---|---|
| `never` (default) | nothing; always exits 0 |
| `blocking` | `blocking` findings |
| `warning` | `warning` **and** `blocking` findings |

It is a threshold, not a filter: `--fail-on warning` fails on blockers too. There is
deliberately no `info` level — those findings are the cluster working as intended, so a job
configured that way would be red on a perfectly healthy cluster, and a check that is always red
is a check nobody reads.

| Exit | Meaning |
|---|---|
| `0` | Ran, and nothing reached the threshold |
| `1` | Could not run: unreachable cluster, bad flag, invalid configuration |
| `2` | Ran, and findings reached the threshold |

Findings and failures are separate codes on purpose. A job needs to distinguish "your cluster
has blockers" from "diagnose could not reach the cluster", and every other binpack command
already exits 1 for the latter.

**Findings marked `freesNothing` are not counted** towards `--fail-on`. A gate that fails today
over a node that was never going away is one a team turns off, and then it catches nothing at
all. Pass `--fail-on-static-pools` to count them anyway — worth doing if you intend to enable
autoscaling on those pools. When any are excluded, the message says how many, so a count that
disagrees with the report above it is explained rather than merely puzzling:

```
binpack: 15 finding(s) at or above warning (6 more on pools that are not autoscaled
were not counted; pass --fail-on-static-pools to include them)
```

The count goes to stderr, so `--output json` on stdout stays machine-readable.

---

## Cluster and pool

### `no-autoscaler` — blocking

The cluster-autoscaler is not in a state to remove a node, so nothing will remove one however
empty it becomes.

binpack refuses to act at all in this state: draining a node nothing will reap is strictly
worse than doing nothing.

**Five observations reach this finding, and the detail line says which.** They are different
facts about your cluster, and only one of them means no autoscaler is running:

| What binpack observed | What it means |
|---|---|
| No status object where binpack looked | Either there is no autoscaler, or it publishes elsewhere, or it runs with `--write-status-configmap=false`. binpack cannot tell which from one read, and does not claim to |
| The object is there and carries no status | Something is writing that ConfigMap without a status document in it |
| `autoscalerStatus` is something other than `Running` | The autoscaler is there and said so itself — `Initializing` is the value you are most likely to see, during start-up |
| No probe time, or one older than five minutes | The document outlived the process that wrote it. A status ConfigMap keeps reporting `Running` indefinitely after its autoscaler is gone, which is why freshness is checked separately |
| `clusterWide.health.status: Unhealthy` | The autoscaler is alive and scanning, and has stopped scaling in **both** directions until the cluster recovers. See below |

**Fix.** Read the detail line for which of those it was, then check that cluster autoscaling is
enabled for the cluster, that the status ConfigMap is being updated, and that
`discovery.autoscalerNamespace` and `discovery.autoscalerStatusName` name where the autoscaler
publishes it. The report's closing line names the object binpack read, because two healthy
clusters look identical to one with no autoscaler from where binpack is standing: an autoscaler
publishing somewhere binpack was not pointed at, and one running with status reporting turned
off. `kubectl get configmap -A | grep autoscaler` tells the two apart.

**An `Unhealthy` cluster is worth calling out**, because `autoscalerStatus` stays `Running`
throughout it and the probe time keeps advancing — both of binpack's other guards pass. The
autoscaler sets this when more than `--ok-total-unready-count` nodes *and* more than
`--max-total-unready-percentage` of the cluster are NotReady; it then logs "Cluster is not ready
for autoscaling" and returns before any scale-up or scale-down. A drain started then evicts real
workload during an incident and reaches a node nothing removes. The evaluation is counted under
its own outcome code, `autoscaler-unhealthy`, so it is distinguishable in
[the metrics](metrics.md) from a cluster with no autoscaler at all.

Requires **cluster-autoscaler 1.30 or later**, which is where the structured status document
arrived. An older autoscaler is reported as such rather than as a parse failure — see
[the version floor](configuration.md#supported-cluster-autoscaler-versions).

When there is no autoscaler, the pool checks are skipped entirely: every pool is absent from a
status document that does not exist, and reporting each one would bury the single answer that
matters.

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

The budget permits no disruption because a replica it selects is missing or unhealthy.

A different problem with a different owner, and reported separately so you do not go and edit a
budget that is behaving exactly as intended.

Health is judged against `status.expectedPods`, not `status.desiredHealthy`. A budget that has
temporarily lost exactly its slack — three replicas, `maxUnavailable: 1`, one of them down —
reports `currentHealthy` *equal* to `desiredHealthy` and zero disruptions allowed, and is fine
again the moment the third recovers. The converse also matters: `minAvailable` set above the
replica count leaves `currentHealthy` below `desiredHealthy` while every selected pod is
healthy, and that block is permanent — reported as `pdb-zero-disruptions`.

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

### `pdb-unparseable-selector` — warning

The budget's `spec.selector` cannot be turned into a label query, so the eviction API skips it
and it guards nothing.

An invalid label value is the usual cause — a space, a `!`, or anything else outside the
character set labels allow. Such an object can be stored because policy validation grandfathers
a selector that is already there, so a budget written against an older API server survives
edits that never touch the selector.

Nothing is blocked by it. `getPodDisruptionBudgets` in the eviction subresource skips a budget
whose selector will not parse, and the disruption controller cannot read it either, so it never
gets a computed status. That combination is what makes it hard to see from the object: `kubectl
get pdb` shows `ALLOWED DISRUPTIONS: 0`, which reads as a budget doing its job.

**Fix.** Correct the label value, or delete the budget. Before you do, it is worth knowing
whether the workload it was meant to cover is protected by anything else — `kubectl get pdb -n
NS` lists every budget in the namespace, and `binpack diagnose` reports the ones that select
nothing separately.

### `pdb-sync-failed` — blocking

The disruption controller could not compute this budget, so it reports zero allowed disruptions
whatever the budget itself says.

When a sync fails the controller calls `failSafe`, which forces `status.disruptionsAllowed` to
zero and records the reason on the `DisruptionAllowed` condition. It changes nothing else, so
`currentHealthy`, `desiredHealthy` and `expectedPods` keep the values the last successful sync
wrote — and a budget that has never synced keeps zeros. Read without the condition, the first
looks like a budget with no slack and the second like a selector matching nothing.

The condition is the only field that distinguishes them:

```bash
kubectl get pdb -n NS NAME -o jsonpath='{.status.conditions}'
```

A `reason` of `SyncFailed` is this finding; `InsufficientPods` or `SufficientPods` means the
controller computed the budget and the numbers are to be trusted. binpack reports the
controller's `message` verbatim, and it usually names the cause. The two commonest are a pod
whose controlling owner cannot be resolved to a replica count — `found no controllers for pod
"..."` — and an owner whose custom resource exposes no `scale` subresource, which the message
names by group and kind.

Blocking, because while it lasts nothing can evict a pod the budget selects: the eviction API
reads the same zero and returns 429.

**Fix.** Usually not in the budget. Read the condition, and fix what its message names — most
often a workload whose controller was deleted while its pods stayed, or a controller whose CRD
needs a `scale` subresource for the disruption controller to size it. If the message points at
RBAC, the disruption controller's scale client is being denied and the effect is cluster-wide
rather than confined to this budget.

### `pdb-stale` — warning

The budget was edited and its controller has not caught up, so the eviction API refuses
disruptions until it does.

The eviction subresource compares `status.observedGeneration` against `metadata.generation` and
rejects outright when the status is behind, whatever the recorded allowance says. binpack applies
the same precedence, so this finding outranks every other reading of the status — including
`pdb-sync-failed`, whose condition survives a spec edit until the next sync and can therefore be
describing the budget as it was rather than as it is.

**Fix.** Usually momentary; re-run. If it persists, read the condition:

```bash
kubectl get pdb -n NS NAME -o jsonpath='{.status.conditions}'
```

A `reason` of `SyncFailed` means the controller is running and cannot compute this particular
budget — see [`pdb-sync-failed`](#pdb-sync-failed--blocking), and note that editing the budget
will not settle it. Anything else that persists suggests the disruption controller is not running
at all.

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

Every check here except `priority-below-cutoff` considers only pods that are **on a node**: an
unscheduled pod holds nothing open, so it cannot be why a node is still there.

### `bare-pod` — blocking

The pods have no *controlling* owner, so evicting one would delete it permanently.

Nothing will do that: not binpack, and not the cluster-autoscaler, for the same reason. Note
that an owner reference with `controller` unset does not count — those exist for garbage
collection, and nothing is responsible for replacing the pod.

`kubectl drain --force` is not bound by this and will delete the pod. The eviction API is not
either: it deletes a bare pod without complaint. The refusal is binpack's and the autoscaler's
own policy, not the API's.

**Fix.** Give the pod a controller that will recreate it: a Deployment, or — for work that ends
— a Job. A Job needs one qualification, because it is the one controller kind that stops
recreating:

```yaml
spec:
  backoffLimit: 6
  podFailurePolicy:
    rules:
      - action: Ignore
        onPodConditions:
          - type: DisruptionTarget
  template:
    spec:
      restartPolicy: Never   # Kubernetes permits podFailurePolicy only with this
```

An eviction adds the `DisruptionTarget` condition to the pod, and the Job controller counts a
deleted pod as a failure. Once `.status.failed` passes `.spec.backoffLimit` the Job is
terminally `Failed` and creates nothing — and `backoffLimit: 0` is the ordinary setting for a
migration, a Helm hook or a one-shot CI Job. The `Ignore` rule above is what stops a disruption
spending that budget.

Otherwise, accept that the node cannot be drained while the pod is there. The
`cluster-autoscaler.kubernetes.io/safe-to-evict=true` annotation does override this, and means
what it says: the pod is deleted and nothing brings it back.

### `local-storage` — warning

The pods use an `emptyDir` or `hostPath` volume, which the cluster-autoscaler will not disturb
by default in case the contents matter.

The report names the volume, because that is what makes the decision possible: `wasm-cache`
answers "is this disposable?" and `data` does not.

Two kinds of volume are **not** reported here, because the autoscaler does not block on them
either. A tmpfs volume — `emptyDir` with `medium: Memory` — is RAM with a filesystem over it,
and nothing of it reaches the node's disk; this is the volume Istio and Linkerd inject into
every pod they mesh. And any volume already named in the pod's
`cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes` annotation.

**Fix.** Annotate the pod `cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes` with
the volume names whose contents need not survive, comma-separated — a cache, a scratch
directory, a rendered config:

```yaml
metadata:
  annotations:
    cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes: "cache,tmp"
```

That is the autoscaler's own recommendation, and it says exactly what is meant and nothing
else. The names must match the pod's `volumes[].name` entries exactly: the autoscaler splits
the value on commas and compares the pieces, so `"cache, tmp"` exempts `cache` and a volume
called `" tmp"`, which is to say nothing. binpack reads the annotation the same way rather than
the forgiving way, so that its answer and the autoscaler's are the same answer.

`cluster-autoscaler.kubernetes.io/safe-to-evict: "true"` also works and covers the whole pod
instead — including the `bare-pod`, `system-pod` and storage protections together. Reach for it
when that is what you mean.

### `system-pod` — warning

kube-system pods with no PodDisruptionBudget, which are younger than the grace the autoscaler
gives one.

Since cluster-autoscaler **1.33** the refusal expires: `--blocking-system-pod-distruption-timeout`
(one hour by default; the misspelling is upstream's) is measured from the pod's creation, and
past it the autoscaler evicts the pod and removes the node. So a pod appears here only while it
is new, and a steady-state cluster reports none — which is the opposite of how it reads.

**Fix.** Usually nothing: wait, and the node becomes a candidate on its own.

Give the workload a PodDisruptionBudget if you want it moved sooner, or protected properly
rather than only briefly. Per the autoscaler's own documentation a budget overrides its refusal
to touch the node, and the budget then governs, like any other.

If your cluster-autoscaler is older than 1.33 it has no such grace and a blocking system pod
blocks for as long as it is there. binpack cannot see which version is running, so tell it:

```yaml
policy:
  autoscaler:
    blockingSystemPodDistruptionTimeout: 0s
```

`kubectl -n kube-system get deploy cluster-autoscaler -o jsonpath='{.spec.template.spec.containers[0].image}'`
prints the version where the autoscaler runs as a workload you can see. On a platform that runs
it in the control plane it usually tracks the cluster's own minor version, which
`kubectl version` reports.

### `unreadable-template` — warning

The pods are created by a controller binpack cannot read a pod template from, so it cannot tell
what their replacements would request and will not move them.

**Unlike everything else in this reference, this blocks binpack alone.** The cluster-autoscaler
and `kubectl drain` are unaffected — it is a gap in what binpack models, not a property of your
cluster. binpack reads templates from ReplicaSets, StatefulSets, DaemonSets and Jobs; a pod owned
directly by an operator's own custom resource has none, and sizing its move from the running pod
is the inference that is unsound (see
[ADR-0006](../design/adr-0006-scheduler-fidelity.md)).

**Fix.** Nothing on your side. Please report the controller, so the list can be widened against
evidence rather than guesswork. `binpack_nodes_unmodelled{cause="unreadable-template"}` counts the
same thing as a metric.

### `template-divergence` — warning

The pods carry a placement constraint their controller's template does not, so binpack cannot
tell where their replacements would be allowed to run and will not move them.

**Unlike everything else in this reference, this blocks binpack alone.** The cluster-autoscaler
and `kubectl drain` are unaffected. binpack sizes and places the pod a controller *would create*,
which it reads from the template — so a `nodeSelector`, a required node affinity, a
`schedulerName`, a hard topology spread constraint, a `runtimeClassName` or a rewritten volume
that is on the running pod and not in the template describes a replacement binpack cannot predict.
Assuming the replacement is as free to move as its template says would approve a destination the
scheduler then refuses, which is the one direction binpack will not err in
(see [ADR-0006](../design/adr-0006-scheduler-fidelity.md)).

Two quite different things produce that gap, and binpack cannot tell them apart — which is why
this code is named for what it observed rather than for a cause. The finding's detail names the
field; comparing it against the controller's own template says which case you are looking at:

```bash
kubectl get statefulset <name> -o jsonpath='{.spec.template.spec.nodeSelector}{"\n"}{.spec.updateStrategy}'
```

**Something adds the constraint after the template.** A mutating admission webhook, or — on a
cluster whose API server enables the in-tree `PodNodeSelector` admission plugin — the namespace's
`scheduler.alpha.kubernetes.io/node-selector` annotation, which that plugin merges into
`pod.spec.nodeSelector` and into no template. These say which webhooks and which namespace
defaults are in play:

```bash
kubectl get mutatingwebhookconfigurations
```

```bash
kubectl get namespace <namespace> -o jsonpath='{.metadata.annotations}'
```

**Or the template is newer than the pods.** binpack reads a StatefulSet's template from the
StatefulSet itself, so an update the rollout has not reached yet diverges from the pods still
running the old revision. `updateStrategy.type: OnDelete` holds that state until you delete the
pods yourself, and `rollingUpdate.partition` holds it for every ordinal below the partition, both
indefinitely and both deliberately. (Deployment pods are not affected: their controller is a
ReplicaSet, whose template is fixed for the life of that revision.)

**Fix.** In the first case, put the same constraint in the workload's own pod template — binpack
can then see where the replacement would go, and the constraint is the same one, so where the pod
actually runs does not change. In the second there is nothing to fix: the pods become movable as
the rollout reaches them. `binpack_nodes_unmodelled{cause="template-divergence"}` counts the same
thing as a metric.

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

This is the one check that also examines **unscheduled** pods, and the detail counts them
(`3 pending`). A stuck Pending pod below the cutoff is not a symptom of the problem — it *is*
the problem, and a finding covering one is never treated as freeing nothing.

**Fix.** Raise the priority to at or above the cutoff. If these really are throwaway pods,
nothing to do.

## binpack's own state

### `abandoned-drain` — warning

The node is cordoned and still says binpack is draining it. Two ways it can say so: it carries a
drain marker, or the markers have been cleared and the `binpack.motleyhand.com/draining` label is
all that is left. The second is a hand-back that stopped halfway, and that label is the only
thing left to recognise it by.

**Fix.** If binpack is running it will finish or abandon the drain by itself. If it is not —
uninstalled mid-drain, most likely — uncordon the node first, then remove its
`binpack.motleyhand.com/` annotations and its `binpack.motleyhand.com/draining` label. That
order, because a node left cordoned with its markers cleared reads as somebody else's cordon:
binpack skips it, this check stops naming it, and it stays cordoned and billed with nothing left
to say why. The markings exist precisely so this is a self-describing state rather than a
mysteriously cordoned node nobody dares touch.

### `node-in-backoff` — info

binpack failed to drain the node and is waiting before trying again. The detail carries when the
wait ends, how many drains of this node have failed, and the recorded reason — the attempt count
being the number the backoff doubling is computed from, and what tells a first failure from a
seventh. It is omitted when nothing is recorded, which is not the same as none.

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
