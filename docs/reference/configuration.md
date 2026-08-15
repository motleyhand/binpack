# Configuration reference

> **Status: the format is implemented; the behaviour it configures is not yet.**
> Fields are validated and resolved today, and `binpack config validate` will tell you exactly
> what a document means. Nothing acts on it until the controller lands.

binpack is configured by a YAML document mounted from a ConfigMap. **Every field is optional.**
An empty document is valid and produces a working, safe configuration, because node pools and
their bounds are discovered from the cluster-autoscaler rather than declared here.

Check a document before applying it:

```bash
binpack config validate -f binpack.yaml
kubectl -n kube-system get configmap binpack -o jsonpath='{.data.config\.yaml}' | binpack config validate
```

It prints the *resolved* settings — defaults and overrides already applied — rather than echoing
back what you wrote. Add `--output json` for a machine-readable form.

## A complete document

Every value shown is the default.

```yaml
apiVersion: binpack.motleyhand.com/v1alpha1
kind: BinpackConfig

# How often to evaluate the cluster. Minimum 10s.
interval: 1m0s

# Decide everything, change nothing. Acting is opt-in.
dryRun: true

discovery:
  # The node label whose value matches the cluster-autoscaler's node group
  # identifier. This is what lets binpack read pool bounds without any cloud
  # credentials.
  nodeGroupIDLabel: doks.digitalocean.com/node-pool-id
  # The node label holding a human-readable pool name, so that `pools` entries
  # below can be written as "pool-4g" rather than a UUID.
  poolNameLabel: doks.digitalocean.com/node-pool

# Applies to every discovered pool that has no override.
policy:
  enabled: true

  feasibility:
    expendablePriorityCutoff: -10
    reserveForLargestPod: true

  drain:
    maxPodsPerDrain: 0
    verifyRemovalTimeout: 15m0s

  cooldown:
    afterScaleUp: 10m0s
    afterDrain: 15m0s

  exclusions:
    namespaces: []

# Overrides for specific discovered pools. Pools are never declared here.
pools: []
```

## Fields

### `interval`

How often binpack evaluates the cluster. Minimum 10 seconds; below that the cost of re-reading
cluster state outweighs any benefit.

Reading is cheap — the controller works from watch-backed caches, not repeated API calls — so a
short interval is not expensive. One node is drained per evaluation regardless.

### `dryRun`

`true` decides everything and changes nothing: the same arithmetic runs, the same decisions are
reached, and they are reported through logs, Kubernetes Events and metrics without any node
being touched.

Defaults to `true`. A tool that drains nodes should be opted into acting.

### `discovery.nodeGroupIDLabel`

The node label whose *value* equals the name the cluster-autoscaler uses for that node's group.
binpack matches nodes to entries in the `cluster-autoscaler-status` ConfigMap through this label,
which is how it learns which pools autoscale and what their minimum and maximum sizes are —
without any cloud provider credentials.

Defaults to DigitalOcean's. Other providers use a different key; if the values do not match the
node group names in the status ConfigMap, preflight fails loudly rather than guessing.

### `discovery.poolNameLabel`

Purely for readability: it lets `pools` entries below be written as `pool-4g` rather than a UUID.
A `pools` entry matches on either identifier.

### `policy.enabled`

`false` leaves a pool's nodes alone entirely — never a drain candidate, though its free capacity
is still counted as a destination for pods leaving other pools.

Useful for rolling binpack out one pool at a time.

### `policy.feasibility.expendablePriorityCutoff`

Mirrors the cluster-autoscaler's `--expendable-pods-priority-cutoff`. Pods with priority
**strictly below** this value are excluded from the placement simulation, because the autoscaler
will terminate a node running them without ceremony.

Values above `0` are rejected: they would classify ordinary workloads as expendable and exclude
them from the arithmetic, which is precisely how binpack would drain a node whose pods have
nowhere to go.

**Overprovisioning pause pods are not expendable**, and should not be made so by lowering their
priority below this cutoff — that breaks warm-capacity replenishment entirely. See
[overprovisioning and expendable pods](../explanation/overprovisioning-and-expendable-pods.md).

### `policy.feasibility.reserveForLargestPod`

When `true`, a drain is only considered feasible if, afterwards, some schedulable node still has
room for a pod the size of the largest relocatable pod running in the cluster.

This exists instead of a percentage of headroom. A percentage is blind to absolute capacity —
the same objection binpack raises against the descheduler — and the risk being guarded against
is not "the cluster is full" but "the next pod that restarts cannot be placed", which is a
question about bytes.

Set it to `false` to pack as tightly as the arithmetic allows. Expect the cluster to scale up
sooner after any restart.

### `policy.drain.maxPodsPerDrain`

Caps how many pods a single drain may relocate. `0` means unlimited.

A blast-radius guard, expressed directly rather than as a utilisation threshold standing in for
one. Because candidates are evaluated least-loaded-first and one node is drained per run,
low-churn nodes are already preferred.

### `policy.drain.verifyRemovalTimeout`

How long to wait for the autoscaler to delete a drained node before **uncordoning it** and
recording the drain as failed.

This is a correctness bound, not a convenience. A cordoned node that nothing removes is lost
schedulable capacity, and if the pool is at its maximum, pods can stay Pending while a healthy
node sits idle. Must be positive.

### `policy.cooldown.afterScaleUp` and `policy.cooldown.afterDrain`

Suppress action for a period after the cluster grew, and after binpack itself drained something.
The first mirrors the autoscaler's own `scale-down-delay-after-add`; the second prevents a
drain and an immediate re-drain oscillating.

### `policy.exclusions.namespaces`

Namespaces whose pods binpack will not evict. A node hosting such a pod is not a drain
candidate — the pods are **protected, not ignored**. Ignoring them would remove them from the
feasibility arithmetic while leaving them on the node, which is unsound.

## Per-pool overrides

A `pools` entry overrides `policy` for one discovered pool, field by field. Anything unset is
inherited.

```yaml
policy:
  drain:
    maxPodsPerDrain: 10
  exclusions:
    namespaces: [kube-system, monitoring]

pools:
  # Never touch the static-ish pool.
  - name: pool-8g
    enabled: false

  # Be more cautious on the busy pool, and narrow the exclusions.
  - name: pool-4g
    drain:
      maxPodsPerDrain: 3
    exclusions:
      namespaces: [kube-system]
```

Two things to know:

- **Lists replace, they do not merge.** `pool-4g` above excludes only `kube-system`, not
  `kube-system` plus `monitoring`. This lets a pool narrow the global set as well as widen it.
- **Pools are discovered, never declared.** An entry here adjusts a pool that discovery found;
  it does not create one. Naming a pool that does not exist is a configuration error.

## Notes on parsing

**Unknown fields are rejected.** A typo is an error, not a silent fall-back to the default —
`maxPodsPerDrian: 5` fails rather than being quietly ignored while you believe you set
something.

**Field names are matched case-insensitively**, a consequence of parsing YAML through JSON,
which is how Kubernetes behaves too. `dryrun` and `dryRun` are the same field; `dryRunn` is an
error.

**Every problem is reported at once**, so a bad document can be fixed in one pass rather than
several.

**Durations are strings**: `30s`, `10m`, `1h30m`.
