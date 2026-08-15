# binpack architecture

- **Status:** accepted, partially implemented
- **Date:** 2026-08-15

The design specification. Individual decisions and their rationale live in the ADRs alongside
this file; this document describes the system they add up to.

## What binpack does

Once per interval, binpack asks a single question about one node:

> If this node were drained, would every one of its pods actually be schedulable onto the
> remaining nodes, without triggering a scale-up?

If yes, it drains the node and lets the cluster-autoscaler reap it. If no, it explains why and
does nothing. Everything below is in service of answering that question correctly and being
able to show its working.

## Conventions

Fixed, and treated as public API from the first release:

| Thing | Value |
|---|---|
| API group | `binpack.motleyhand.com` |
| Config `apiVersion` | `binpack.motleyhand.com/v1alpha1` |
| Node opt-out annotation | `binpack.motleyhand.com/skip: "true"` |
| Metric prefix | `binpack_` |

The opt-out annotation key is **not configurable**. A fixed key means one thing to document,
one thing to search for, and no possibility of two clusters disagreeing about what protects a
node.

## Components

```
cmd/binpack            thin main(), calls internal/cli

api/v1alpha1           configuration types (CRD-shaped), defaulting, validation

internal/
  engine               PURE. no Kubernetes imports. Snapshot -> Decision
  fit                  FitChecker: can this pod go on this node? upstream logic
  collect              the ONLY package holding Kubernetes clients. builds a Snapshot
  cli                  cobra commands: explain, diagnose, run, version
  controller           controller-runtime manager + periodic Runnable
  executor             cordon, evict, and the uncordon safeguard
  state                cooldown and anti-thrash memory
  metrics              Prometheus collectors

charts/binpack         Helm chart, RBAC, ConfigMap
```

The dependency graph is deliberately shallow. `engine` depends on nothing but the standard
library. `collect` depends on Kubernetes client libraries and on `engine`'s types. `cli` and
`controller` depend on both, and on each other not at all.

### Data flow

```
                  ┌─────────────┐
kubeconfig  ──►   │   collect   │  ──►  Snapshot  ──►  ┌────────┐
(CLI)             │             │                      │ engine │ ──► Decision
informer cache ──►│             │       Config    ──►  └────────┘        │
(controller)      └─────────────┘                                        │
                                            ┌────────────────────────────┴──────┐
                                            ▼                                   ▼
                                    render (explain)                   executor (run)
```

The CLI path makes one-shot `List` calls. The controller path reads from controller-runtime's
watch-backed cache. Both produce the same `Snapshot` type, so the engine cannot tell them
apart — which is what guarantees `explain` describes what `run` will do.

## Preflight

Before any evaluation, on every command:

1. **Is a cluster-autoscaler running?** Read the `cluster-autoscaler-status` ConfigMap in
   `kube-system`. If it is absent, stale, or reports anything other than `Running`, binpack
   refuses to act and says why. Draining nodes that nothing will reap is strictly worse than
   doing nothing — see [ADR-0004](adr-0004-provider-agnostic-no-cloud-api.md).
2. **Which node groups autoscale, and what are their bounds?** Discovered from the same
   ConfigMap where the node group identifier can be mapped to a node label, configured
   otherwise.

`explain` and `diagnose` report preflight failures rather than exiting silently: "no
cluster-autoscaler found" is itself a useful diagnosis.

## The decision procedure

Evaluated per run, cheapest checks first.

1. **Cooldown.** Has the cluster grown recently? Draining immediately after a scale-up is how
   oscillation starts, so binpack mirrors the autoscaler's own `scale-down-delay-after-add`.
   The status ConfigMap's `scaleUp.lastTransitionTime` gives this directly.
2. **Scope.** Consider only nodes in autoscaling node groups. A node the autoscaler does not
   manage — a static pool, a control-plane node — must never be a candidate.
3. **Pool floor.** Is the group above its minimum? At the minimum, the autoscaler replaces
   whatever is drained.
4. **Candidate ordering.** Order in-scope nodes by allocated resources ascending and evaluate
   the least loaded first. This is an ordering, not a filter — see *No utilisation threshold*
   below.
5. **Feasibility by simulation.** Simulate placing every relocatable pod onto a specific
   remaining node. If any pod cannot be placed, skip this candidate.
6. **Evictability.** Predict, rather than discover, whether each pod can actually be evicted.
7. **Drain.** Cordon, evict, then watch for the node's removal — and uncordon if it does not
   happen.

**Default: one node per run.** Iterative beats clever. The next run observes fresh state, which
is both safer and far simpler to reason about than planning a multi-node consolidation against
a cluster that is changing underneath you.

### Feasibility must simulate placement, not sum capacity

The tempting check is: sum the relocatable requests on the candidate, sum free allocatable
across the other nodes, compare. **This is wrong**, and wrong in the dangerous direction.

Aggregate free capacity is the fractional relaxation of a bin-packing problem — necessary but
not sufficient. Three nodes with 1GB free each do not hold a 3GB pod, but the sum says they do.
The check passes, binpack drains, the pod goes Pending, and the autoscaler adds the node back.
That is precisely the outcome binpack exists to prevent, and it would be reported as a success.

So feasibility is a real placement simulation: sort relocatable pods largest-first and place
each onto a specific node with sufficient remaining room, honouring node selectors, affinity and
taints. First-fit-decreasing is sufficient — it is a heuristic, so it can fail to find a packing
that exists, but it never claims a packing that does not. Erring towards "infeasible" is the
correct direction: the cost is a missed consolidation, not a scale-up.

This is cheap at cluster scale: tens of nodes, hundreds of pods, run once a minute.

#### Every resource the scheduler accounts for, not three of them

Capacity is modelled as an **open map of resource name to quantity**, taken from whatever
appears in a node's `status.allocatable` and a pod's `resources.requests`. It is deliberately
not a fixed struct of CPU, memory and pod count.

The scheduler's `NodeResourcesFit` plugin does not privilege those three. It compares requests
against allocatable for *every* resource name, which includes `ephemeral-storage`, `hugepages-*`
and extended resources such as `nvidia.com/gpu`. A pod requesting one GPU is unschedulable on a
node with no GPUs no matter how much CPU and memory are free.

Hardcoding three dimensions would therefore produce exactly the failure this section exists to
prevent: the simulation places a GPU pod onto a GPU-less node, declares the drain safe, and the
real scheduler leaves the pod Pending until the autoscaler adds a node. Iterating over the
resource names actually present is both more correct and less code, and it means binpack
supports resources that did not exist when it was written.

#### What counts as a pod's request

The map a pod contributes is **not** a copy of `resources.requests`. Building it naively
under-reserves the destination in three separate ways, each of which lets the simulation approve
a node that cannot host the pod.

**Init containers and sidecars.** The scheduler reserves the *effective* pod request, which for
each resource is the larger of the sum across regular containers and the peak across init
containers — because init containers run sequentially before the regular ones, but the regular
ones run concurrently. Native sidecars (init containers with `restartPolicy: Always`) stay
running, so they add to the running total rather than participating in the maximum. A pod with a
lightweight main container and a 2GB init container needs 2GB at admission time.

**Pod overhead.** `spec.overhead`, populated from the RuntimeClass, is added on top. It is the
resource cost of the sandbox itself, and clusters using gVisor or Kata have a non-trivial amount
of it.

This arithmetic is fiddly and upstream already gets it right, so `internal/collect` uses
`k8s.io/component-helpers/resource` rather than hand-rolling it. That is precisely the kind of
work the collect boundary exists for: the engine receives a finished number and stays free of
Kubernetes libraries.

**Pod slots.** `pods` appears in `status.allocatable` but never in a pod's requests, so a
uniform "subtract request from remaining" loop would never consume a slot, and the simulation
would happily pack an unlimited number of pods onto a node capped at 110. Every pod's request
map therefore carries a synthetic `pods: 1`, added during collection. The kubelet's pod limit is
a real scheduling constraint and a genuine ceiling that clusters hit while showing plenty of
free CPU and memory.

Synthesising the entry at the collect boundary keeps the engine's arithmetic uniform: it
subtracts maps from maps and knows nothing about which keys are special.

#### Fit predicates come from upstream, not from us

Deciding whether a pod can go on a node is delegated to a `FitChecker`, implemented in
`internal/fit` using Kubernetes' own staging libraries — `resource.PodRequests` for effective
requests, `nodeaffinity` for selectors and affinity, `corev1` for taints. Hand-rolling this is
how the defects above arose in the first place.

Two hard constraints are not modelled — required inter-pod (anti-)affinity, and topology spread
with `whenUnsatisfiable: DoNotSchedule`. Their *presence* is detected, and any candidate holding
such a pod is refused with that stated as the reason. Their soft counterparts are ignored
outright, because they affect only the scheduler's scoring, never its filtering.

Every gap in the model must fail towards refusing to drain, and `internal/fit` is tested
against a real `kube-scheduler` with a one-directional property: if binpack says a pod fits, the
scheduler must agree. See [ADR-0006](adr-0006-scheduler-fidelity.md).

### Feasibility is computed on requests, not usage

The scheduler places pods according to requests, so a feasibility check on actual usage would
answer a different question than the one that determines whether the pods can be rescheduled.
Usage may inform reporting; it never decides.

### No utilisation threshold

Earlier drafts had a "only consider nodes below N percent utilised" guard. It has been removed.

Its real job was hiding the weakness of the aggregate-sum check: relocating fewer pods left
less room for the sum to be wrong. Once feasibility is a genuine placement simulation, that job
disappears.

Nothing else justifies it. "Don't churn a node doing real work" is not a coherent goal — if the
work demonstrably fits elsewhere, the cluster does not need the node, which is the entire
thesis. And blast radius is better expressed directly, as an optional cap on how many pods a
single drain may relocate, than as a percentage proxy nobody can pick a correct value for.
Because candidates are evaluated least-loaded-first and one node is drained per run, low-churn
nodes are preferred by construction.

### Which pods must fit, and which need not

Every pod on a candidate node falls into exactly one of three classes. Getting this wrong in
either direction is a correctness bug, so the classification is explicit rather than implied by
the word "relocatable".

**Node-local — not simulated, not evicted.** These pods do not move to another node when this
one goes away; they cease to exist with it, and an equivalent already runs elsewhere.

- DaemonSet-owned pods. The CNI agent, `kube-proxy`, log shippers, node exporters. Every
  remaining node already runs its own copy, so there is nothing to relocate.
- Mirror pods (static pods managed directly by a kubelet, marked with
  `kubernetes.io/config.mirror`). They cannot be evicted at all; the kubelet owns them.
- Pods already terminating.

Both halves of "not simulated, not evicted" matter. Counting DaemonSet pods as workload
needing a destination inflates the requirement on every single node — every ordinary node runs
several — which would make otherwise-valid candidates look infeasible and could reject the
entire cluster. And *evicting* them is worse than pointless: the DaemonSet controller
immediately recreates the pod on the same node, which still exists, so the drain never
completes. This is the same reason `kubectl drain` requires `--ignore-daemonsets`.

For the same reason, DaemonSet requests are excluded when ordering candidates by load. Their
footprint is roughly constant per node, so including it measures node count rather than
workload.

**Expendable — not simulated, but evicted.** Pods whose **priority is strictly below the
autoscaler's expendable cutoff** (`--expendable-pods-priority-cutoff`, default -10). The
autoscaler ignores such pods for both scale-up and scale-down, so it will terminate a node
running them without ceremony. binpack mirrors that rule exactly and adds nothing to it.

**Relocatable — simulated and evicted.** Everything else. These must be placeable somewhere and
must pass the evictability check below.

**Overprovisioning pause pods are not expendable, and treating them as such is a trap.** The
warm-capacity pattern requires those pods to sit at or above the cutoff — below it, the
autoscaler ignores their Pending state and never replenishes the buffer, silently breaking the
pattern after the first burst. Being above the cutoff, they behave exactly like real workload:
evict them and they go Pending, and the autoscaler adds a node. If binpack excluded them from
the simulation, every drain of a node holding buffer would trigger the scale-up it promised to
avoid.

There is a deeper reason not to be clever here. A warm-capacity buffer is capacity you are
deliberately paying to keep free. Consolidating it away is not a saving; it is silently
shrinking a buffer someone sized on purpose. If the buffer is too large, that is a
configuration decision for its owner.

The corollary is a useful `diagnose` check: pause pods sitting *below* the cutoff mean the
overprovisioning pattern is already broken. Nothing surfaces this today.

### Evictability is predicted, not discovered

A drain that will hit a PodDisruptionBudget wall should be predicted and skipped, not attempted
and failed. binpack checks PDB slack, the `cluster-autoscaler.kubernetes.io/safe-to-evict`
annotation, controller ownership, local storage, and node selectors and affinity.

**PDB demand is aggregated across the whole drain, not checked per pod.** Draining a node is not
one eviction, it is many, and they all draw on the same allowances. So for every PDB, binpack
counts how many of the candidate's pods match it and requires that total to be within the PDB's
current `disruptionsAllowed`.

Checking only for zero would be insufficient in a common case: two pods of the same Deployment
landing on one node, with `disruptionsAllowed: 1`. The zero test passes, the first eviction
consumes the sole allowance, the second is refused, and the node is left cordoned and half
drained until the uncordon safeguard fires. Requiring `matching pods ≤ disruptionsAllowed` per
PDB rejects that candidate before anything is touched.

This is conservative in a knowable way. An allowance does replenish once an evicted pod's
replacement becomes ready elsewhere, so a patient sequential drain might eventually succeed
where this check refuses. binpack does not attempt that: it would mean holding a node cordoned
for an unbounded time while betting on rescheduling that has not happened yet. Refusing costs a
missed consolidation the next run may find; proceeding costs a half-drained node.

It is worth being explicit about a common misconception, because it explains otherwise baffling
cluster behaviour. A Deployment with one replica and a PDB of `minAvailable: 1` allows zero
voluntary disruptions **permanently** at steady state. Rolling-update `maxSurge` does not
rescue it: surge belongs to the Deployment controller's rollout path, whereas eviction is a
deletion, and the ReplicaSet creates a replacement only *after* the pod is gone. There is no
create-first path for eviction anywhere in core Kubernetes.

What is true is that *during* a rollout there are transiently two healthy pods, so the PDB
briefly permits one disruption. That is the "natural churn" the autoscaler waits for, and it is
why blocked clusters sometimes shrink shortly after an unrelated deploy. Because binpack
re-evaluates every interval, it will catch those windows on its own. It must not rely on them:
evicting into a window that closes mid-drain leaves a partially drained, cordoned node. So the
window is a bonus, never a strategy.

### Three properties that distinguish this from the descheduler

- **Absolute quantities, simulated.** On a cluster of mixed node sizes, a 4GB node at 81 percent
  holds around 1.1GB of requests while an 8GB node at 60 percent has around 2.7GB free.
  Percentage thresholds call the first node "busy" and do nothing.
- **Cross-pool awareness.** Free capacity is considered across **all** schedulable nodes, while
  only autoscaling-group nodes are ever drain candidates. This asymmetry is what makes a
  static-plus-elastic topology work, and it is not expressible in descheduler configuration.
- **Feasibility before action.** The descheduler evicts and finds out. binpack decides, then
  evicts.

## Draining safely

Cordon, evict in order, then **verify**. If the node still exists after a configured timeout,
binpack uncordons it and records the drain as failed.

This is not optional bookkeeping. A cordoned node that the autoscaler never removes is lost
schedulable capacity: if the pool is at its maximum, later pods can stay Pending indefinitely
while a healthy node sits idle. binpack must leave the cluster in a working state without
human intervention, including when its own prediction was wrong.

## Configuration

Mounted from a ConfigMap, parsed into `api/v1alpha1` types. Sketch, not final — the field-level
reference is written with the implementation.

```yaml
apiVersion: binpack.motleyhand.com/v1alpha1
kind: BinpackConfig

interval: 60s
dryRun: true                          # safe by default; acting is opt-in

discovery:
  nodeGroupIDLabel: doks.digitalocean.com/node-pool-id
  pools: []                           # only needed when discovery is unavailable

feasibility:
  headroomPercent: 10                 # do not plan to fill the cluster exactly
  expendablePriorityCutoff: -10       # mirrors the autoscaler's own flag

drain:
  maxPodsPerDrain: 0                  # 0 = unlimited; a blast-radius guard
  verifyRemovalTimeout: 15m           # uncordon if the node is still here

cooldown:
  afterScaleUp: 10m
  afterDrain: 15m

exclusions:
  namespaces: []
```

## Command surface

| Command | Reads | Writes | Purpose |
|---|---|---|---|
| `binpack explain` | kubeconfig | nothing | Print the arithmetic and the verdict for every node |
| `binpack diagnose` | kubeconfig | nothing | Report what is currently blocking scale-down |
| `binpack run` | in-cluster | drains, if enabled | The controller |
| `binpack version` | — | — | |

`explain` and `diagnose` are the adoption path: useful before anything is installed, needing
only read access, and the means by which someone decides whether to trust the tool.

### diagnose reports; it never remediates

`diagnose` will identify PodDisruptionBudgets that permit zero disruptions, pause pods below the
expendable cutoff, unevictable pods, and nodes pinned by affinity. It will suggest the fix. It
will not apply it.

Beyond the obvious risk of a cost tool mutating availability policy, there is a decisive
practical reason: in any GitOps-managed cluster — Flux, Argo CD — a patched PDB is reconciled
straight back within minutes. binpack would appear to fix the problem, the fix would silently
evaporate, and the tool would have taught its user to distrust it. Hooking into the GitOps
controller to make the change stick is well outside what this project should do.

The correct output is a precise description of the problem and the change to make, in the
user's own repository.

## Observability

The controller emits a Kubernetes Event on the Node for every decision, so `kubectl describe
node` explains why a node was or was not drained without access to binpack's logs. This
deliberately mirrors how the autoscaler's own decisions surface on a managed control plane.

Prometheus metrics (prefix `binpack_`) cover decisions by action and reason, drains attempted
and succeeded, drains that failed to produce a node removal, evictions failed, per-group node
counts against discovered bounds, and the size of the feasibility shortfall. The last is the one
to alert on: a persistent shortfall means the cluster genuinely needs its nodes.

## Roadmap

1. ✅ Foundation and decisions
2. Harvested documentation
3. Go scaffold and CI
4. `api/v1alpha1` configuration types
5. `internal/fit`: the `FitChecker`, plus the differential test harness against a real
   `kube-scheduler` running on `envtest`
6. Engine: `Snapshot` types and the placement simulation
7. Engine: evictability prediction and `Decision` rendering
8. `internal/collect`, preflight, and `binpack explain`. Integration-tested against real API
   fixtures — init containers, RuntimeClass overhead, extended resources — since a wrong number
   here produces a confidently wrong decision no engine unit test can catch
9. `binpack diagnose`
10. `binpack run`, dry-run only
11. Executor: cordon, evict, verify, uncordon; cooldown and anti-thrash state
12. Helm chart and RBAC
13. Release pipeline

Each of steps 4 onward gets its own specification before implementation.

## Open questions

**How should nodes map to node groups where no identifier label exists?** DOKS provides
`doks.digitalocean.com/node-pool-id`, whose values match the autoscaler's node group names
exactly. Other providers need investigating one at a time. Until then, those providers fall back
to configured pool minimums.

**Should node sizing be recommended?** Per-node kubelet and system reservations are roughly
fixed, so larger nodes waste proportionally less — a 4GB node loses 15–17 percent to
reservations, an 8GB node around 9 percent. A single pool of larger nodes also pushes the
110-pod-per-node ceiling further away, at the cost of coarser scale granularity. binpack could
offer this advice from data it already collects, but it is not a v1 concern.

**How much headroom should the simulation reserve?** Planning to fill the remaining nodes
exactly leaves no room for the next pod that arrives, and would produce a cluster that scales up
the moment anything is deployed. Ten percent is a guess pending real data from `explain`.
