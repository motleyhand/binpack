# binpack

**Your managed Kubernetes cluster is probably running more nodes than it needs, and its
autoscaler will never notice.**

The cluster-autoscaler removes a node only when that node is already nearly empty. It has no
code path that asks whether the cluster could be *rearranged* to use fewer nodes. Combined with
a scheduler that spreads pods towards the emptiest node, this produces a stable, expensive
equilibrium: every node at 40–70 percent of requested capacity, none of them clearly removable,
all of them billed.

binpack closes that gap. Once per interval it asks one question about one node — *if this node
were drained, would its workload fit on the remaining nodes without triggering a scale-up?* — and
drains it only when the arithmetic says yes. The autoscaler then reaps the node as it normally
would.

> **Status: 0.x.** binpack decides and reports, and drains nodes once acting is switched on. It
> stays at 0.x until it has done that on somebody else's cluster — see
> [versioning](docs/reference/versioning.md) for what the number covers and what it does not.

## Is this for you?

**Yes, if** you run Kubernetes on a managed service that constrains the cluster-autoscaler —
DigitalOcean DOKS, Linode LKE, Vultr, Scaleway, Civo, OVH — and your node count drifts upward
after load spikes and stays there. On these platforms the alternatives are thin: the descheduler
helps only in narrow cases, and everything else is commercial.

**Also worth considering if** you run EKS, GKE or AKS. [Karpenter][karpenter] is the more
complete answer on those platforms: it provisions right-sized nodes and treats consolidation as
first-class, so it solves a strictly larger problem than binpack does. It is also a much larger
commitment — it replaces node provisioning entirely, which means re-architecting node lifecycle
management, new failure modes to learn, and ongoing operational surface.

binpack does one thing and cooperates with the autoscaler you already run. If what you want is
lower node count rather than a new provisioning model, that is a reasonable trade to make
deliberately, on any platform. [ADR-0005](docs/design/adr-0005-why-not-a-karpenter-doks-provider.md)
sets out the comparison honestly, including why binpack exists at all given that Karpenter's
consolidation is a superset of it.

**Either way, binpack needs a cluster-autoscaler** publishing a `cluster-autoscaler-status`
ConfigMap — it drains a node, and the autoscaler is what removes it. The autoscaler writes that
object into the namespace it runs in, which is often but not always `kube-system`:

```bash
kubectl get configmap cluster-autoscaler-status -A
```

Whichever namespace that names is the one to set as `discovery.autoscalerNamespace`. Without an
autoscaler binpack refuses to act, so it has nothing to offer a `kind` or `minikube` cluster.

**It also needs to match each node to its pool**, and what the autoscaler publishes in
`nodeGroups[].name` is its cloud provider's own identifier — an Auto Scaling group name on AWS, a
VM Scale Set name on Azure — not the pool name you picked in a console. DigitalOcean's
`doks.digitalocean.com/node-pool-id` carries that identifier outright and is the default.
Elsewhere binpack works the match out from the pool label your provider already applies, because
the identifier is generated from the same name; it does that only when every published pool
resolves at once and unambiguously, reports the label it used, and refuses rather than guessing. Where it refuses, you
can point `discovery.nodeGroupIDLabel` at a label of your own or state the mapping directly. See
[`discovery.nodeGroupIDLabel`](docs/reference/configuration.md#discoverynodegroupidlabel),
[ADR-0012](docs/design/adr-0012-pool-mapping-needs-a-value-matching-node-label.md) and
[ADR-0013](docs/design/adr-0013-deriving-the-pool-mapping-from-the-names-identifiers-are-built-from.md).

**Before installing anything**, work through the
[quick wins](docs/how-to/quick-wins-before-installing-binpack.md). Several of them recover more
capacity than binpack can, because they remove *permanent* blocks rather than optimising around
them — a `PodDisruptionBudget` with `minAvailable: 1` on a single-replica Deployment permits
*zero* voluntary disruptions, forever, and will pin a node in place no matter what tooling you
add. If those fixes solve it, you might not need this.

If they do not, the chart installs from a registry and defaults to deciding without acting:

```bash
helm install binpack oci://ghcr.io/motleyhand/charts/binpack \
  --namespace binpack-system --create-namespace
```

`binpack diagnose` and `binpack explain` need nothing installed at all. They run against your own
kubeconfig from the [release binaries](https://github.com/motleyhand/binpack/releases), need no
in-cluster identity, and are the recommended way to decide whether the controller is worth
installing. [Install binpack](docs/how-to/install-binpack.md) covers the chart's defaults, what to
watch, and how to uninstall it without stranding a node.

## Documentation

### Start here

| Document | What it covers |
|---|---|
| [Why your cluster doesn't shrink](docs/explanation/why-clusters-dont-shrink.md) | What the autoscaler actually does, why the scheduler works against you, and the three conditions a node must meet before removal |
| [Quick wins before installing binpack](docs/how-to/quick-wins-before-installing-binpack.md) | Seven fixes worth doing regardless. Do these first |
| [Install binpack](docs/how-to/install-binpack.md) | The chart, its defaults, and the read-only path to try first |
| [Diagnose scale-down blockers](docs/how-to/diagnose-scale-down-blockers.md) | Read-only commands for working out why a node is still there, by hand |
| [Diagnostics reference](docs/reference/diagnostics.md) | Every code `binpack diagnose` reports, what it means, and what to change |

### Background

| Document | What it covers |
|---|---|
| [The PodDisruptionBudget that costs you money](docs/explanation/the-poddisruptionbudget-that-costs-money.md) | The single-replica PDB trap, why `maxSurge` doesn't save you, and the two-PDB case that blocks eviction permanently |
| [Overprovisioning and expendable pods](docs/explanation/overprovisioning-and-expendable-pods.md) | Warm capacity, the -10 priority cutoff, and the silent failure either side of it |
| [Why the descheduler can't solve this](docs/explanation/why-not-descheduler.md) | What it does well, and the three structural gaps no configuration closes |

### Reference

| Document | What it covers |
|---|---|
| [Configuration](docs/reference/configuration.md) | Every field, what it does, and why headroom is not a percentage |
| [Diagnostics](docs/reference/diagnostics.md) | Every `binpack diagnose` code, its severity, and the fix |
| [Versioning](docs/reference/versioning.md) | What a version number covers, what is public API, and what 0.x means |
| [Metrics](docs/reference/metrics.md) | Every `binpack_` series, what to alert on, and why prose is never a label |
| [Annotations and labels](docs/reference/annotations.md) | The one you set, the seven binpack writes, the label that says it is draining, and how to hand a stuck node back |
| [Helm chart](charts/binpack) | Values, RBAC, and what each permission is for |
| [RBAC](docs/reference/rbac.md) | Every permission binpack needs, and what it is deliberately never granted |
| [Let binpack drain nodes](docs/how-to/let-binpack-drain-nodes.md) | Turning off dry run: what to read first, and what it can then do |
| [Fix a silently broken Metrics API](docs/how-to/fix-metrics-api-on-managed-kubernetes.md) | When `kubectl top` returns nothing and your HPAs are quietly inert |

### Design

The tool is a single Go binary with two frontends over one decision engine: a read-only CLI you
run against your own kubeconfig, and an in-cluster controller.

| Document | What it covers |
|---|---|
| [Architecture](docs/design/2026-08-15-architecture.md) | Components, data flow, the decision procedure, configuration, roadmap, open questions |
| [ADR-0001](docs/design/adr-0001-purpose-built-consolidation-controller.md) | Why a purpose-built controller, and every alternative that was tried and rejected |
| [ADR-0002](docs/design/adr-0002-go-controller-runtime-no-crd.md) | Go and controller-runtime without a CRD; configuration as a versioned file |
| [ADR-0003](docs/design/adr-0003-pure-decision-engine.md) | The decision engine as a pure function, and why that guarantees the CLI cannot lie *(mechanism superseded by ADR-0008)* |
| [ADR-0004](docs/design/adr-0004-provider-agnostic-no-cloud-api.md) | Discovering pool bounds from the autoscaler itself; why binpack holds no cloud credentials *(resolution order superseded by ADR-0012)* |
| [ADR-0005](docs/design/adr-0005-why-not-a-karpenter-doks-provider.md) | Karpenter compared honestly, and why this project exists anyway |
| [ADR-0006](docs/design/adr-0006-scheduler-fidelity.md) | Borrowing the scheduler's own logic, and testing against the real one |
| [ADR-0007](docs/design/adr-0007-drain-progress-not-deadlines.md) | Why drains are bounded by progress rather than deadlines, and why failures back off |
| [ADR-0008](docs/design/adr-0008-engine-uses-api-types.md) | Why the engine uses Kubernetes API types directly, and what the purity rule was really protecting |
| [ADR-0009](docs/design/adr-0009-revalidation-asks-soundness-not-preference.md) | Revalidation re-asks soundness, not preference *(soundness list narrowed by ADR-0010)* |
| [ADR-0010](docs/design/adr-0010-a-scale-up-stops-a-drain-that-has-not-started.md) | Why a scale-up stops a drain that has not started, and not one that has |
| [ADR-0011](docs/design/adr-0011-a-refusal-is-a-decision-and-earns-an-event.md) | Why a decision to drain nothing earns an Event, and which nodes it goes on |
| [ADR-0012](docs/design/adr-0012-pool-mapping-needs-a-value-matching-node-label.md) | Why mapping a node to its pool needs a label whose value is the autoscaler's own identifier, and what that costs per provider *(second resolution mode added by ADR-0013)* |
| [ADR-0013](docs/design/adr-0013-deriving-the-pool-mapping-from-the-names-identifiers-are-built-from.md) | Deriving that mapping from the pool names the identifiers are built from, and what makes a substring match safe enough to act on |

## Design principles

- **Prove it before doing it.** A drain that triggers a scale-up is worse than no drain at all,
  so binpack simulates placing every pod onto a specific remaining node before it cordons
  anything. Aggregate free capacity is not proof: three nodes with 1GB free each do not hold a
  3GB pod.
- **One pod at a time, and check again.** binpack does not place pods and cannot steer the
  scheduler, so a valid packing existing is not the same as the scheduler choosing it. Evictions
  are sequential and the cluster is re-examined between each one. This makes a scale-up unlikely
  and immediately detectable — not impossible, and the docs say so.
- **Show your working.** Every decision returns the arithmetic that produced it. `binpack
  explain` is not a separate code path from the controller; it is the same function, printed.
- **Borrow the scheduler's logic; never guess it.** Fit uses upstream Kubernetes code, and is
  tested against a real `kube-scheduler` with a one-directional property: if binpack says a pod
  fits, the scheduler must agree. Where a constraint cannot be modelled, binpack detects it and
  refuses rather than assuming.
- **Leave the cluster working.** If a drained node is not removed, binpack uncordons it. Being
  wrong must not cost you capacity.
- **Safe by default.** Dry-run is the default. Acting is opt-in. binpack refuses to run at all
  where no cluster-autoscaler is present, since draining nodes nothing will reap is worse than
  doing nothing.
- **No credentials.** binpack needs a Kubernetes RBAC role and nothing else. Pool bounds come
  from the autoscaler's own status, not from a cloud API.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Licence

[Apache-2.0](LICENSE).

[karpenter]: https://karpenter.sh/
