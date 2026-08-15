# RBAC reference

> **Status: provisional.** These are the permissions the design requires. The authoritative
> version ships with the Helm chart; until then, treat this as the intended shape rather than a
> tested manifest.

binpack needs **no cloud provider credentials** on any platform. A Kubernetes role in the
cluster it runs in is its entire permission surface — see
[ADR-0004](../design/adr-0004-provider-agnostic-no-cloud-api.md).

## ClusterRole

```yaml
rules:
  # Read the cluster state, and cordon/uncordon nodes.
  # patch also writes the drain marker annotations.
  - apiGroups: [""]
    resources: [nodes]
    verbs: [get, list, watch, patch]

  - apiGroups: [""]
    resources: [pods]
    verbs: [get, list, watch]

  # Eviction is a create on a subresource, not a delete on the pod.
  - apiGroups: [""]
    resources: [pods/eviction]
    verbs: [create]

  # Ownership: distinguishing DaemonSet pods from relocatable ones.
  - apiGroups: ["apps"]
    resources: [daemonsets, replicasets, statefulsets]
    verbs: [get, list, watch]

  # Predicting evictability before attempting it.
  - apiGroups: ["policy"]
    resources: [poddisruptionbudgets]
    verbs: [get, list, watch]

  # Preflight and pool discovery: the cluster-autoscaler's status ConfigMap.
  - apiGroups: [""]
    resources: [configmaps]
    verbs: [get, list, watch]
    resourceNames: [cluster-autoscaler-status]

  # Explaining decisions on the Node object itself.
  - apiGroups: [""]
    resources: [events]
    verbs: [create, patch]
```

Leader election additionally requires `coordination.k8s.io/leases` in binpack's own namespace,
scoped as a Role rather than a ClusterRole.

## Notes on specific permissions

**`nodes: patch` is the only mutating verb on cluster state.** It covers cordoning, uncordoning,
and writing the drain marker annotations. binpack never deletes a node — that is the
cluster-autoscaler's job, and the division is deliberate: binpack makes a node removable and
something else decides to remove it.

**`pods/eviction: create`** is how the Eviction API works. It is a create on a subresource, and
it respects PodDisruptionBudgets — unlike `pods: delete`, which does not. binpack does not
request `pods: delete`, and should not be granted it. That distinction is the difference between
draining a node and simply killing things on it.

**`configmaps: get` is scoped by `resourceNames`** to the single object binpack reads. This
matters: an unscoped ConfigMap read across all namespaces would give it access to a great deal
of application configuration it has no business seeing.

## Read-only mode

`binpack explain` and `binpack diagnose` need only the read verbs — `get`, `list`, `watch`. They
never patch, evict or create events.

This is the recommended way to evaluate binpack: grant a read-only role, run `explain` against
your own kubeconfig, and read the arithmetic before granting anything that can act.

## What binpack is never granted

- Any cloud provider API credential
- `pods: delete`
- Write access to workloads — Deployments, StatefulSets, PodDisruptionBudgets. `diagnose`
  reports problems and suggests fixes; it never applies them.
- Secrets, of any kind
