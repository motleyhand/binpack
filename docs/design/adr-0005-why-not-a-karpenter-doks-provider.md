# ADR-0005: Why not write a Karpenter provider instead

- **Status:** accepted
- **Date:** 2026-08-15

## Context

Karpenter is the architecturally correct answer to this problem, and it is fair to ask why this
project exists at all rather than a Karpenter provider for DigitalOcean.

Karpenter replaces the cluster-autoscaler entirely. Instead of node pools with a minimum and
maximum managed by the cloud provider, it watches for unschedulable pods, computes which
instance type would fit them most cheaply, and provisions that instance directly. Consolidation
is first-class: it continuously evaluates whether the current set of nodes could be replaced by
a cheaper set, including replacing one large node with a smaller one.

Karpenter's consolidation is a **strict superset** of what binpack does. On any cluster running
Karpenter, binpack has nothing useful to contribute.

So the question is real: is binpack merely a workaround for Karpenter's absence, and would a
DigitalOcean provider make it redundant?

## Decision

Build binpack. Do not attempt a Karpenter provider for DOKS.

## Rationale

### A third party cannot build one

Karpenter's `CloudProvider` interface requires creating **one instance at a time** and having
it join the cluster: `Create(NodeClaim)`, `Delete(NodeClaim)`, `List`, `GetInstanceTypes`,
`IsDrifted`. The provider owns node bootstrapping.

On DOKS, nodes exist only as members of a node pool. There is no published join-token or
bootstrap endpoint, and DigitalOcean's own documentation states that changes made to worker
nodes ["are overwritten by the reconciler and do not persist"][do-managed]. The platform
actively reconciles worker nodes back to pool-defined state.

The only workaround is one node pool per node. That means node provisioning latency measured in
minutes, [account-level limits on pool count][do-limits], and DigitalOcean's own pool-level
auto-repair competing with Karpenter for control of the same resources. That is not a provider;
it is a wrestling match with the platform.

### The provider ecosystem confirms the pattern

Surveying the Karpenter providers that exist (August 2026):

| Provider | Author | Environment |
|---|---|---|
| AWS (~7.7k ★) | AWS | own platform |
| Azure / AKS (~554 ★) | Microsoft | own platform |
| Alibaba Cloud, Tencent TKE, Oracle OCI | the vendor | own platform |
| GCP, IBM Cloud | third party / SIG | |
| Proxmox, Hetzner, Cluster API | third party | **self-managed control planes** |

Every provider for a *managed* Kubernetes service was written by that service's vendor. Every
third-party provider targets an environment where the operator controls node bootstrapping.
There is no counterexample.

The one existing attempt at a DOKS provider, `k8ubeify/karpenter-provider-doks`, was created on
29 January 2025 and last pushed eight minutes later. No releases, no subsequent commits.
Someone opened the door, looked at the bootstrapping problem, and left.

The Cluster API provider is the theoretical escape hatch, but DigitalOcean's Cluster API support
targets self-managed clusters — which means running your own control plane, giving up the
reason DOKS was chosen in the first place.

### Even if it existed, binpack would still have a job

Two independent reasons.

**Scope of audience.** Karpenter *replaces* node provisioning; binpack *cooperates* with the
autoscaler already running. That is the difference between "re-architect your node lifecycle"
and "install something that drains one node when the arithmetic says it is safe." binpack
targets every managed Kubernetes service with a constrained autoscaler — DOKS, Linode LKE,
Vultr, Scaleway, Civo, OVH — plus GKE and EKS users who have not adopted Karpenter, for a small
fraction of the engineering effort of a single provider.

**Permanence of the gap.** binpack is not a workaround for DigitalOcean hiding
`scale-down-utilization-threshold`. Even a fully tunable cluster-autoscaler never rebalances;
it has no code path that asks whether the cluster could be rearranged. If every tuning knob
were exposed tomorrow, the seventh node would still sit there. The gap binpack fills is
architectural, not configurational.

## Consequences

- binpack's positioning is "consolidation for managed Kubernetes where you do not control the
  autoscaler", not "Karpenter for DigitalOcean". The documentation says so plainly.
- Where Karpenter *is* available, the documentation presents the comparison and leaves the
  choice to the reader rather than deferring automatically. Karpenter solves a strictly larger
  problem, and it is a strictly larger commitment: replacing node provisioning means new
  failure modes, new operational surface, and a migration. Someone who wants a lower node count
  rather than a new provisioning model may reasonably prefer a tool that cooperates with the
  autoscaler already running, on any platform. That is a trade to make deliberately, not a
  consolation prize.
- This decision should be revisited if DigitalOcean ships first-party Karpenter support, in the
  way Microsoft did for AKS. That would give DOKS users a genuine choice, which they currently
  do not have.

[do-managed]: https://docs.digitalocean.com/products/kubernetes/details/managed/
[do-limits]: https://docs.digitalocean.com/products/kubernetes/details/limits/
