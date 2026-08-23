# ADR-0005: Why not write a Karpenter provider instead

- **Status:** accepted, and **corrected in three places** where a claim about a third party has
  not survived re-checking. All three are marked where they appear, and the decision — build
  binpack, do not attempt a Karpenter provider for DOKS — rests on the bootstrapping mechanism,
  which is untouched. (1) The superset claim holds only for the nodes Karpenter provisions.
  (2) "There is no counterexample" has one: an actively maintained third-party Karpenter
  provider for GKE. (3) The audience list asserted a constraint per platform that is false for
  at least one of the platforms named.
- **Date:** 2026-08-15

## Context

Karpenter is the architecturally correct answer to this problem, and it is fair to ask why this
project exists at all rather than a Karpenter provider for DigitalOcean.

Karpenter replaces the cluster-autoscaler entirely. Instead of node pools with a minimum and
maximum managed by the cloud provider, it watches for unschedulable pods, computes which
instance type would fit them most cheaply, and provisions that instance directly. Consolidation
is first-class: it continuously evaluates whether the current set of nodes could be replaced by
a cheaper set, including replacing one large node with a smaller one.

Karpenter's consolidation is a **strict superset** of what binpack does for the nodes Karpenter
provisions. On a cluster where Karpenter owns every node, binpack has nothing to add.

**Correction (1).** Both sentences were originally unscoped — "on any cluster running
Karpenter". Karpenter's disruption loop only considers nodes it owns: `ValidateNodeDisruptable`
(`pkg/controllers/state/statenode.go`) rejects a node with no `NodeClaim` before anything else,
with "node isn't managed by karpenter", and the candidate scan is over Karpenter's own
`StateNode` set. A cluster running a Karpenter NodePool beside a cluster-autoscaler-managed or
static pool — a partial migration, or a deliberate static baseline — gets no consolidation from
Karpenter on the rest of it. That is the shape binpack is otherwise built for, so the unscoped
version argued against the project's own applicability.

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
| GCP / GKE (~325 ★) | third party (cloudpilot-ai) | managed — reuses an existing pool's bootstrap metadata |
| Linode / LKE (~4 ★) | Linode | own platform, alpha |
| IBM Cloud | third party / SIG | |
| Proxmox, Hetzner, Cluster API | third party | **self-managed control planes** |

Star counts and repository state are a snapshot taken 2026-08-23 and will age; the mechanism
below will not.

A third-party provider for a managed service works only where the platform exposes reusable
bootstrap material. cloudpilot-ai's GKE provider is the one that does it, and its own
documentation says how: "Karpenter requires a GKE node pool to read bootstrap metadata
(instance templates and kubelet settings)", which it discovers and reuses from an existing
cluster pool, patching that pool's `kube-env` metadata when it needs a different OS or
architecture. DOKS exposes no such material: worker-node changes are reverted by the reconciler
and no join path is published. That is the distinction that decides this ADR.

**Correction (2).** This section previously generalised instead: "Every provider for a
*managed* Kubernetes service was written by that service's vendor... There is no counterexample."
The GKE provider is one, it is actively maintained rather than abandoned, and it solves the
problem this ADR calls insurmountable — by a technique DOKS happens not to permit. The table's
own blank Environment cell for GCP had already recorded the contradiction. Stating the mechanism
costs nothing and does not age; the universal was the strongest-sounding sentence here and the
only one a reader could falsify.

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
targets anyone whose provider runs the cluster-autoscaler for them and does not expose its
scale-down settings — DOKS and Linode LKE are the worked examples — plus GKE and EKS users who
have not adopted Karpenter, for a small fraction of the engineering effort of a single provider.

**Correction (3).** The audience was previously a list of six platforms asserted to have
"a constrained autoscaler", Civo among them. On Civo the autoscaler is a marketplace application
running in the operator's own cluster, reconfigured by editing its Deployment — Civo's
documentation gives `kubectl edit deployment cluster-autoscaler -n kube-system` and the
`--nodes=min:max:workers` flag as the worked example — so every flag is available. Vultr,
Scaleway and OVH were not checked either way and should not be named again until they are. The
durable form of the claim is not a list but the command that answers it on the reader's own
cluster, `kubectl -n kube-system get deploy cluster-autoscaler`, and the architectural argument
below holds for everyone regardless of the answer.

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
