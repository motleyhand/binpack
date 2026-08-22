# Configuration reference

binpack is configured by a YAML document mounted from a ConfigMap. **Every field is optional.**
An empty document is valid, because node pools and their bounds are discovered from the
cluster-autoscaler rather than declared here. Discovery rests on one node label, though, and the
default is DigitalOcean's — see [`discovery.nodeGroupIDLabel`](#discoverynodegroupidlabel) for
what to set elsewhere, and how to find out whether you need to.

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
  # The namespace the cluster-autoscaler publishes its status ConfigMap into,
  # which is the namespace it runs in.
  autoscalerNamespace: kube-system
  # Membership stated outright, for clusters where binpack cannot work it out.
  # Bounds are never stated here: they come from the status ConfigMap.
  nodeGroups: []

# Applies to every discovered pool that has no override.
policy:
  enabled: true

  feasibility:
    expendablePriorityCutoff: -10
    reserveForLargestPod: true

  drain:
    maxPodsPerDrain: 0
    stallTimeout: 10m0s
    removalTimeout: 15m0s

  backoff:
    initial: 30m0s
    max: 24h0m0s

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

`false` lets it act: cordon a node, annotate it, uncordon it, and evict a pod through the
eviction API. Those four, and nothing else — binpack deletes no object, and the emptied node is
removed by the cluster-autoscaler.

Defaults to `true`. A tool that drains nodes should be opted into acting. The Helm chart also
requires `rbac.allowDraining: true` alongside `dryRun: false`, and refuses the install rather
than letting binpack decide to drain and then be refused by RBAC on every attempt.

Switching back to `true` while a drain is in progress leaves that drain exactly as it is —
cordoned and marked, neither advanced nor undone. Uncordoning would itself be a change, which is
the one thing dry run promises not to make.

That node stays cordoned for as long as the setting stands, so binpack says so on it: a
`WouldAdvanceDrain` event each evaluation, carrying what advancing the drain would do. The rest
of the cluster goes on being evaluated and reported on meanwhile — a frozen drain does not stop
binpack deciding about any other node. Hand the node back yourself, or set `dryRun: false` again
and let binpack finish it. See [Let binpack drain nodes](../how-to/let-binpack-drain-nodes.md).

### `discovery.nodeGroupIDLabel`

The node label whose *value* equals the name the cluster-autoscaler uses for that node's group.
binpack matches nodes to entries in the `cluster-autoscaler-status` ConfigMap through this label,
which is how it learns which pools autoscale and what their minimum and maximum sizes are —
without any cloud provider credentials.

**Where no value matches outright, binpack works the mapping out for itself**, so on EKS, AKS and
most GKE clusters there is nothing to set here at all. Read
[`discovery.nodeGroups`](#discoverynodegroups) for what it does, what it refuses to do, and how to
state the mapping by hand where it declines. The rest of this section is about the value match,
which is tried first and is what DOKS uses.

The match is on the *value*, and the value has to be the cloud provider's own identifier for the
group — an Auto Scaling group name on AWS, a VM Scale Set name on Azure, a node pool ID on
DigitalOcean, a full instance group URL on GCE. That identifier is what the autoscaler writes
into `nodeGroups[].name`, so that is what a node has to carry. It is not, in general, the pool
name you chose in a console.

Defaults to `doks.digitalocean.com/node-pool-id`, whose values are exactly those identifiers on
DOKS. Whether a label your provider applies carries them on your cluster is a question two
read-only commands answer:

```bash
kubectl -n <discovery.autoscalerNamespace> get configmap cluster-autoscaler-status -o yaml
kubectl get nodes --show-labels
```

The first prints a `nodeGroups[].name` per pool. If a label already on your nodes holds one of
those names as its value, set `discovery.nodeGroupIDLabel` to that key. If none does, apply a
label yourself, one group at a time:

```bash
kubectl label nodes <node>... binpack.motleyhand.com/node-group=<group>
```

and set `discovery.nodeGroupIDLabel: binpack.motleyhand.com/node-group`. binpack never writes
that label — it reads whichever key you point it at, and the key under its own API group is
suggested only because a provider cannot start writing to it underneath you.

Where the identifier is not a legal label value there is no mapping to express *this way*. GCE is
the known case: the autoscaler identifies a managed instance group by its full API URL, and a
Kubernetes label value is capped at 63 characters and may not contain `/` or `:`. The derivation
below reaches it anyway, because the instance group's *name* is inside that URL.
[ADR-0012](../design/adr-0012-pool-mapping-needs-a-value-matching-node-label.md) records why the
value match is the primary one, and
[ADR-0013](../design/adr-0013-deriving-the-pool-mapping-from-the-names-identifiers-are-built-from.md)
what was added beside it.

If no node carries a value any published group answers to **and nothing can be derived**,
**preflight fails** — on `explain`, `diagnose` and `run` alike, exit status 1 — naming the key it
looked for, the groups the autoscaler published, the labels your nodes do carry, and any label
that came close to resolving the cluster together with what was wrong with it. It used to report
every node as "not part of an autoscaling pool" instead, which reads as a fact about the cluster
rather than as the misconfiguration it is.

### `discovery.nodeGroups`

Usually nothing. Set it only where binpack tells you it could not work the mapping out.

Cloud providers *generate* the identifier they publish from the pool name you chose, so the pool
label they put on your nodes holds a string that is inside it:

| | node label | its value | published identifier |
|---|---|---|---|
| EKS managed node group | `eks.amazonaws.com/nodegroup` | `workers` | `eks-workers-a8c75f2f-…` |
| AKS agent pool | `kubernetes.azure.com/agentpool` | `nodepool1` | `aks-nodepool1-33555069-vmss` |
| GKE node pool | `cloud.google.com/gke-nodepool` | `default-pool` | `…/instanceGroups/my-cluster-default-pool-a0c72690-grp` |

Where no label's value *is* an identifier, binpack looks for one whose values name the pools, and
uses it only if every pool resolves at once: the label's values must correspond one-to-one with
the pools the autoscaler publishes, at sizes those pools could have, in exactly one way — and if
several labels manage it they must agree about every node. Nodes in no autoscaling pool are
expected and do not prevent this; they simply stay outside every pool, and binpack never drains
them. Anything less and it refuses rather
than guessing, because a mapping that is wrong applies one pool's floor to another pool's nodes.
`binpack explain` prints the label it matched on whenever the mapping was derived.

The two ways that leaves you setting this field:

- **The identifier is not built from the pool name.** A self-managed Auto Scaling group you named
  yourself, for instance.
- **The provider truncated the name.** GKE shortens long cluster and node pool names when it
  builds the instance group's, so a pool named `nap-n1-standard-1-1kwag2qv` can appear as
  `…-nap-n1-standard--b4fcc348-grp`, with nothing left to match on.

Then state the mapping. `kubectl -n kube-system get configmap cluster-autoscaler-status -o yaml`
prints the identifiers; `kubectl get nodes --show-labels` prints what your nodes carry:

```yaml
discovery:
  nodeGroupIDLabel: cloud.google.com/gke-nodepool
  nodeGroups:
    - labelValue: nap-n1-standard-1-1kwag2qv
      group: https://www.googleapis.com/compute/v1/projects/p/zones/z/instanceGroups/c-nap-n1-standard--b4fcc348-grp
```

`labelValue` is what your nodes carry under `discovery.nodeGroupIDLabel`; `group` is the
identifier the autoscaler publishes. **This states membership only** — minimum and maximum sizes
still come from the status ConfigMap, so there is no second number to keep in step with the
console. Entries are additive: a value you do not name still matches by equality, so you can state
one pool and leave the rest alone.

`group` must name a node group the autoscaler publishes, or **preflight fails** and prints the
ones it does publish. A pool scaled to zero still counts, so an empty pool is fine; a typo is not,
because an entry that matches nothing is silently no entry at all.

### `discovery.poolNameLabel`

Purely for readability: it lets `pools` entries below be written as `pool-4g` rather than a UUID.
A `pools` entry matches on either identifier.

### `discovery.autoscalerNamespace`

Where the cluster-autoscaler publishes its `cluster-autoscaler-status` ConfigMap. That is the
namespace the autoscaler runs in — it is what its own `--namespace` flag says, and the upstream
Helm chart sets that flag to whatever namespace you install it into.

Defaults to `kube-system`, which is a common answer rather than the only one. Find yours:

```bash
kubectl get configmap cluster-autoscaler-status -A
```

**Two things move with this setting.** binpack reads this namespace and no other, and the Helm
chart creates binpack's Role for reading that ConfigMap in it — so if you manage RBAC yourself,
the Role has to move too, or every read is a 403. See the
[RBAC reference](rbac.md).

Pointed at the wrong namespace, binpack reports that no cluster-autoscaler is running and
refuses to act, which is indistinguishable from a cluster that genuinely has none. `binpack
explain` and `binpack diagnose` both name the object they read, so the output says which
namespace was consulted.

There is deliberately no search across namespaces. A status ConfigMap outlives the autoscaler
that wrote it, so a cluster can hold a stale one beside the live one, and nothing about either
object says which is which — a search would have to guess, and a wrong guess here produces a
confident answer about the wrong autoscaler. See
[ADR-0004](../design/adr-0004-provider-agnostic-no-cloud-api.md).

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

### `policy.drain.stallTimeout`

How long a drain may go **without making progress** before it is abandoned.

This is not a limit on how long a drain may take. A pod that is terminating within its
`terminationGracePeriodSeconds` counts as progress, so a workload that declares an hour-long
grace period drains happily under the 10-minute default — the stall clock does not run while a
pod is legitimately shutting down.

Other progress signals: an eviction being accepted, the node's pod count dropping, a pod
entering `Terminating`.

Being genuinely stuck is detected separately and does not wait for this timeout. A pod still
present past its termination deadline is reported as such, naming the pod, because that is a
finalizer or a stuck volume rather than a slow shutdown.

Raise it if your cluster has workloads that pause between shutdown phases for longer than ten
minutes without any observable change. Must be positive.

### `policy.drain.removalTimeout`

Once the node is **empty**, how long to wait for the autoscaler to delete it before
**uncordoning** and recording the drain as failed.

A different question from `stallTimeout`, which is why it is a different field: that one is
about your workload, this one is about the autoscaler. The default of 15 minutes suits a
cluster-autoscaler that typically reaps within about ten.

This is a correctness bound, not a convenience. A cordoned node that nothing removes is lost
schedulable capacity, and if the pool is at its maximum, pods can stay Pending while a healthy
node sits idle. Must be positive.

### `policy.backoff.initial` and `policy.backoff.max`

How long a node is left alone after a **failed** drain, doubling with each consecutive failure
up to the cap.

Not politeness — correctness. Abandoning a drain uncordons a node that now has fewer pods than
before, and candidates are evaluated least-loaded-first, so the node that just failed is *more*
attractive than it was. Without backoff, binpack would preferentially retry its own failures.

A node in backoff is skipped, and `explain` reports why, including the recorded failure reason.
There is deliberately no permanent give-up: that would need a human to clear an annotation, and
a node blocked by something transient would stay skipped forever.

## Leader election

Not part of the configuration file: these are flags, set by the chart under `leaderElection`.

binpack does not use controller-runtime's default lease timings. Those — 15s lease, 10s renew
deadline, 2s retry — suit a controller that reconciles constantly, where seconds of lost
leadership are seconds of unhandled events. binpack evaluates once a minute and keeps a drain's
state on the node being drained, so a slow handover costs it nothing: the next leader reads the
same markers and carries on.

What a restart *does* cost is the little binpack holds in memory — the after-drain cooldown, and
the record that lets a completed drain be counted once its node has gone. So the defaults are
sized to ride out a control-plane hiccup instead:

| Flag | Default | |
|---|---|---|
| `--lease-duration` | `60s` | how long the lease is held before another replica may take it |
| `--renew-deadline` | `40s` | how long the leader keeps trying before giving up and exiting |
| `--retry-period` | `10s` | how often the lease is renewed or contested |

The trade is failover latency: a leader that genuinely dies leaves the next one waiting up to
`--lease-duration`. For a controller that does nothing for a minute at a time, that is not a
cost worth optimising against a restart every time the API server is briefly slow.

`--renew-deadline` must be shorter than `--lease-duration`, and longer than `--retry-period`
*plus its jitter*: retries are jittered by a factor of 1.2, so the last one before the deadline
can land that much late, and client-go refuses timings that leave no room for it. `60s/10s/9s`
looks ordered and is rejected, because 10s is inside 9s × 1.2.

binpack checks the same thing at startup, using client-go's own jitter constant rather than a
copy of its value, and says so in terms of the flags you set rather than letting the manager
fail deeper with `renewDeadline must be greater than retryPeriod*JitterFactor`.

### `policy.cooldown.afterScaleUp` and `policy.cooldown.afterDrain`

Suppress action cluster-wide for a period after the cluster grew, and after binpack completed a
**successful** drain. The first mirrors the autoscaler's own `scale-down-delay-after-add`; the
second lets the cluster settle before binpack considers removing another node.

`afterScaleUp` is measured from what the cluster-autoscaler publishes about itself, so it
survives a binpack restart. `afterDrain` is measured from binpack's own memory of the drain,
because a successful drain deletes the node that would otherwise have recorded it — so a restart
or a change of leader inside the cooldown window forgets it, and the next drain may come sooner
than the interval you configured.

That memory is why **`afterDrain` cannot be honoured by `run --once`**. Every invocation is a
new process, so a scheduled run starts having forgotten that it ever drained anything, and the
cooldown would never apply at all. binpack refuses that combination at startup rather than
reporting a setting it will not enforce; run it as a Deployment, or set `afterDrain: 0` to say
that consecutive drains are acceptable. A dry-run `--once` is unaffected, since nothing changes
for the cluster to settle after.

Both are distinct from the per-node backoff that follows a *failed* drain, which lives on the
node and does survive a restart — and is therefore the one bound that works in every mode.

Both govern whether a drain **starts**, not whether one continues. A cooldown opening while a
drain is already relocating pods does not end it, and neither does the autoscaler reporting a
scale-up in progress: the pods that have already moved would stay moved, so stopping would spend
the disruption and keep none of the benefit. Up until the first eviction the checks do apply,
which is the window in which stopping is free. Whether the remaining pods still fit, whether they
are still evictable, and whether anything will still remove the node are re-asked before every
eviction regardless —
[ADR-0010](../design/adr-0010-a-scale-up-stops-a-drain-that-has-not-started.md) draws the line
and says why.

Distinct from `backoff`, which is per-node and follows a *failed* drain.

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
  An explicitly empty list clears it outright:

  ```yaml
  pools:
    - name: pool-4g
      exclusions:
        namespaces: []     # no exclusions on this pool, despite the global list
  ```

  Omitting the field means "inherit"; `[]` means "none". They are different.
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

## Which configuration a command used

`explain`, `diagnose` and `run` all report where their configuration came from, and it is the
first thing they print:

```
config: /etc/binpack/config.yaml
```

or, when none was given and none is mounted:

```
config: built-in defaults
```

With no `-f`, they read `/etc/binpack/config.yaml` if it exists — which is where the Helm chart
mounts it, so this inside the pod answers about the binpack running beside it rather than about
one configured with defaults:

```bash
kubectl -n binpack exec deploy/binpack -- binpack explain
```

The two answer different questions, and before the source was reported nothing in the output
said which one you had. A verdict you cannot check against the settings that produced it is a
verdict you have to take on trust.

