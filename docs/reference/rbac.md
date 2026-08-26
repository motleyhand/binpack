# RBAC reference

> **The authoritative manifests ship with the Helm chart** —
> [charts/binpack/templates/rbac.yaml](../../charts/binpack/templates/rbac.yaml). This page says
> what each rule is for, and is the one to write a role from if you manage RBAC yourself.

binpack needs **no cloud provider credentials** on any platform. A Kubernetes role in the
cluster it runs in is its entire permission surface — see
[ADR-0004](../design/adr-0004-provider-agnostic-no-cloud-api.md).

Permissions come in three groups, and it is worth granting only the ones you are actually using:

| Group | Needed by | Mutates |
|---|---|---|
| Read | `explain`, `diagnose`, `run` | nothing |
| Report | `run` | Events, and the leader-election Lease |
| Act | `run` with `dryRun: false` | nodes (cordon, the drain label and annotations), pod evictions |

## Read

Everything binpack decides is derived from these.

```yaml
# ClusterRole
rules:
  - apiGroups: [""]
    resources: [nodes, pods]
    verbs: [get, list, watch]

  # Predicting evictability before attempting it.
  - apiGroups: ["policy"]
    resources: [poddisruptionbudgets]
    verbs: [get, list, watch]

  # Pod templates: what a controller would create to replace an evicted pod.
  - apiGroups: ["apps"]
    resources: [replicasets, statefulsets, daemonsets]
    verbs: [get, list, watch]

  - apiGroups: ["batch"]
    resources: [jobs]
    verbs: [get, list, watch]
```

### Why the controllers are read

binpack asks whether a pod's *replacement* would fit elsewhere, and the running pod is not
always a reliable stand-in for it. A pod resized downward in place carries smaller requests than
its replacement will ask for, and nothing on the pod records that this happened — so sizing the
move on what is running can approve a node the replacement does not fit. The controller's
template is the only source of truth for that, and reading it is what makes the decision sound
rather than usually-right. See
[ADR-0006](../design/adr-0006-scheduler-fidelity.md).

Only the pod template is read. The cache drops status and annotations before storing, which
matters because a cluster keeps ten ReplicaSet revisions per Deployment by default and binpack
looks at one field of each. (A ReplicaSet does not inherit its Deployment's
`last-applied-configuration` — the Deployment controller skips that annotation — so the saving
there is status and the revision bookkeeping. A StatefulSet, DaemonSet or Job you applied
directly does carry it.)

Pods owned by a controller binpack cannot read a template for — an operator's own CRD — are not
moved at all, rather than being sized from the running pod.

The cluster-autoscaler's status ConfigMap is granted separately, as a **Role in the namespace
`discovery.autoscalerNamespace` names** — the namespace the autoscaler runs in, which defaults
to `kube-system` and is not always it:

```yaml
# Role, named <release>-autoscaler-status, in the namespace discovery.autoscalerNamespace names
rules:
  - apiGroups: [""]
    resources: [configmaps]
    verbs: [get, list, watch]
```

The chart creates that Role and its binding in whatever `config.discovery.autoscalerNamespace`
says, so the two cannot drift apart. **If you manage RBAC yourself, they can**: a Role left in
`kube-system` while binpack reads elsewhere installs cleanly and then 403s on every read, which
the controller reports as a failed evaluation and the one-shot commands as a failure to read the
cluster. Neither says the Role is in the wrong namespace.

### Why that one is not restricted by name

The obvious rule — a ClusterRole on `configmaps` with
`resourceNames: [cluster-autoscaler-status]` — does not work, and fails in a way worth
understanding before you write it.

`resourceNames` restricts a request by the name in its path. `list` and `watch` requests carry
no name, so they cannot be restricted this way and the rule authorises nothing for them. The
one-shot commands would work, because they issue a `get`; the controller would not start at
all, because its cache issues a `list` followed by a `watch`.

Narrowing by namespace is the restriction that does hold. binpack still reads only the one
object: the cache is configured with a field selector on `metadata.name`, which the API server
applies, so no other ConfigMap in that namespace is ever transmitted or held in memory. The
difference is that this is binpack declining to read them rather than Kubernetes refusing —
so grant the Role in that one namespace alone, and never cluster-wide, where it would cover
application configuration binpack has no business seeing.

## Report

```yaml
# ClusterRole
rules:
  # Explaining decisions on the Node object itself.
  - apiGroups: ["events.k8s.io"]
    resources: [events]
    verbs: [create, patch]
```

```yaml
# Role, named <release>-leader-election, in binpack's own namespace
rules:
  - apiGroups: ["coordination.k8s.io"]
    resources: [leases]
    verbs: [get, list, watch, create, update, patch]

  # client-go's LeaseLock uses get, create and update (resourcelock/leaselock.go
  # at v0.36.4); list, watch and patch are the conventional set and are not
  # exercised. Left as they are:
  # this Role is namespaced to binpack's own namespace, and nothing else here
  # holds a Lease.

  # Leader election announces itself on the Lease through the *core* events
  # API, not the one binpack reports decisions through. Both are needed.
  - apiGroups: [""]
    resources: [events]
    verbs: [create, patch]
```

**Both events APIs are in use, for different reasons.** binpack writes its own decisions through
the modern `events.k8s.io` API, so a rule granting `""/events` alone will not let it report
anything. `patch` is there because the recorder aggregates repeats into one object with a count
rather than writing a new one each time, and it applies that count with a patch.

`update` is not granted. It was, up to and including v0.2.2, and the reason attached to it was
not the reason: the page said aggregation needed it, and aggregation is the patch. client-go's
event sink declares an `Update` method and the broadcaster never calls it — `recordEvent` tries
`Patch` and falls back to `Create`.

Dropping it removes no capability, because `patch` on the same resource already subsumes anything
`update` could do to an existing event. **If you manage this role yourself you can drop the verb,
and there is nothing to do if you keep it.** binpack's own test asserts the recorder issues
`Create` and `Patch` and never `Update`, so a client-go release that started issuing one would
fail the build rather than 403 on your cluster.

Leader election is the other way round: it announces itself on the Lease through client-go's
legacy recorder, which writes to the **core** group. That grant is on the leader-election Role
rather than the ClusterRole, because those events are about a Lease in binpack's own namespace
and because they stop existing entirely when leader election is off.

Granting one and not the other fails quietly. Missing `events.k8s.io` costs the decision events —
the thing `kubectl describe node` is for. Missing `""/events` costs only a log line at startup,
since the leader-election recorder does not retry:

```
Server rejected event (will not retry!) err="events is forbidden: ...
cannot create resource \"events\" in API group \"\""
```

**Leases are only for leader election.** Omit them and pass `--leader-election=false` if you run
a single replica and accept that a rolling update briefly has two. With `--once` they are never
used at all.

## Act — required when `dryRun` is false

With `dryRun: false`, binpack cordons the node it has chosen, marks it, evicts its pods one at a
time through the eviction API, and uncordons it again if the drain ends without the node being
removed. Two rules cover all of it:

```yaml
# ClusterRole
rules:
  # Cordon, uncordon, the drain label, and the drain marker annotations.
  - apiGroups: [""]
    resources: [nodes]
    verbs: [patch]

  # Eviction is a create on a subresource, not a delete on the pod.
  - apiGroups: [""]
    resources: [pods/eviction]
    verbs: [create]
```

**`nodes: patch` is the only mutating verb on cluster state.** It covers cordoning, uncordoning,
writing the `binpack.motleyhand.com/draining` label, and writing the drain marker annotations.
All four are the same verb on the same resource, so there is no narrower grant that covers some
of them and not the rest. binpack never deletes a node: that is the cluster-autoscaler's job, and
the division is deliberate — binpack makes a node removable and something else decides to remove
it.

**`pods/eviction: create`** is how the Eviction API works. It is a create on a subresource, and
it respects PodDisruptionBudgets — unlike `pods: delete`, which does not. binpack does not
request `pods: delete`, and should not be granted it. That distinction is the difference between
draining a node and simply killing things on it.

**The chart refuses `config.dryRun: false` without `rbac.allowDraining: true`**, in either RBAC
mode, because a binpack that decides to drain and is then refused looks healthy from outside: it
holds its lease, serves its metrics, publishes its decisions, and fails every drain on its first
node patch.

What the flag *does*, though, depends on the other one. With `rbac.create: true` it adds the two
rules above to the role the chart writes. With `rbac.create: false` it adds nothing and asserts
something — that you have granted them wherever you manage RBAC — and nothing on either side
checks that you have. So if you are writing the role by hand, this is the group to write with it.

## Read-only evaluation

`binpack explain` and `binpack diagnose` need only the **Read** group above, and against your
own kubeconfig they need no in-cluster identity at all.

This is the recommended way to evaluate binpack: read the arithmetic before granting anything
that can act. `binpack run` with the Read and Report groups is the same decision on an interval,
reported on the nodes themselves — and without the Act group it holds no verb that can cordon a
node or evict a pod, whatever its configuration says.

## What binpack is never granted

- Any cloud provider API credential
- `pods: delete`
- Write access to workloads — Deployments, StatefulSets, PodDisruptionBudgets. `diagnose`
  reports problems and suggests fixes; it never applies them
- ConfigMaps outside the namespace `discovery.autoscalerNamespace` names
- Secrets, of any kind

Note that `apps` and `batch` are granted **read-only**, and only for the four kinds that own
pods. binpack never writes to a workload: scaling a Deployment down would be a far blunter
instrument than moving its pods, and not a decision a consolidation tool should be making.
