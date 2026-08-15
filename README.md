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

> **Status: pre-release.** The design is settled and documented below; the implementation is in
> progress. Nothing is installable yet.

## Is this for you?

**Probably, if** you run Kubernetes on a managed service that constrains the cluster-autoscaler
— DigitalOcean DOKS, Linode LKE, Vultr, Scaleway, Civo, OVH — and your node count drifts upward
after load spikes and stays there.

**Probably not, if** you run EKS or AKS and can adopt [Karpenter][karpenter]. Karpenter solves
this problem properly and more thoroughly than binpack ever will. binpack exists for the
clusters Karpenter cannot reach — see
[ADR-0006](docs/design/adr-0006-why-not-a-karpenter-doks-provider.md) for why that gap is
structural rather than temporary.

**Before installing anything**, there are cheaper fixes that are often sufficient on their own.
A `PodDisruptionBudget` with `minAvailable: 1` on a single-replica Deployment permits *zero*
voluntary disruptions, permanently, and will pin a node in place forever. That is a one-line fix
worth more than any tool. A how-to covering these is coming in the next documentation pass.

## Design

The tool is a single Go binary with two frontends over one decision engine: a read-only CLI you
run against your own kubeconfig, and an in-cluster controller.

| Document | What it covers |
|---|---|
| [Architecture](docs/design/2026-08-15-architecture.md) | Components, data flow, the decision procedure, configuration, roadmap, open questions |
| [ADR-0001](docs/design/adr-0001-purpose-built-consolidation-controller.md) | Why a purpose-built controller, and every alternative that was tried and rejected |
| [ADR-0002](docs/design/adr-0002-go-controller-runtime-no-crd.md) | Go and controller-runtime without a CRD; configuration as a versioned file |
| [ADR-0003](docs/design/adr-0003-pure-decision-engine.md) | The decision engine as a pure function, and why that guarantees the CLI cannot lie |
| [ADR-0004](docs/design/adr-0004-provider-agnostic-no-cloud-api.md) | Provider-agnostic by node label; why binpack holds no cloud credentials |
| [ADR-0005](docs/design/adr-0005-name-licence-api-group.md) | Name, licence, API group, metric prefix |
| [ADR-0006](docs/design/adr-0006-why-not-a-karpenter-doks-provider.md) | Why not write a Karpenter provider instead |

Reference documentation, how-to guides and background explanation are being written as the
implementation lands; this table will grow to cover them.

## Design principles

- **Prove it before doing it.** A drain that triggers a scale-up is worse than no drain at all.
  Feasibility is computed against absolute resource quantities before anything is cordoned.
- **Show your working.** Every decision returns the arithmetic that produced it. `binpack
  explain` is not a separate code path from the controller; it is the same function, printed.
- **Safe by default.** Dry-run is the default. Acting is opt-in.
- **No credentials.** binpack needs a Kubernetes RBAC role and nothing else. No cloud API
  token, on any provider.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## Licence

[Apache-2.0](LICENSE).

[karpenter]: https://karpenter.sh/
