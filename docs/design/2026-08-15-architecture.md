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
| Drain markers on a node | `binpack.motleyhand.com/drain-started`, `binpack.motleyhand.com/drain-progress`, `binpack.motleyhand.com/drain-pods-remaining` |
| Backoff markers on a node | `binpack.motleyhand.com/drain-attempts`, `binpack.motleyhand.com/backoff-until`, `binpack.motleyhand.com/last-failure` |
| Metric prefix | `binpack_` |

The opt-out annotation key is **not configurable**. A fixed key means one thing to document,
one thing to search for, and no possibility of two clusters disagreeing about what protects a
node.

## Components

```
cmd/binpack            thin main(), calls internal/cli

api/v1alpha1           configuration types (CRD-shaped), defaulting, validation

internal/
  engine               Snapshot -> Decision. no clients, no I/O, inputs read-only
  fit                  can this pod go on this node? upstream predicates. no clients
  collect              reads cluster state into a Snapshot
  controller           owns the manager, caches and leader election
  executor             cordons, evicts, uncordons, writes drain markers
  cli                  cobra commands: explain, diagnose, run, version
  state                cooldown and backoff bookkeeping
  metrics              Prometheus collectors

charts/binpack         Helm chart, RBAC, ConfigMap
```

The dependency graph is deliberately shallow, and the line that matters is which packages may
touch a cluster at all.

| Package | Cluster access |
|---|---|
| `engine`, `fit`, `api/v1alpha1` | **None.** API types as data, no clients, no I/O — enforced by a `depguard` allowlist, see [ADR-0008](adr-0008-engine-uses-api-types.md) |
| `collect` | Reads. Lists nodes, pods, PDBs and the autoscaler status into a `Snapshot`, without transforming them |
| `controller` | Owns the controller-runtime manager, its caches and leader election |
| `executor` | Writes. Cordon, uncordon, eviction, and the drain marker annotations |

`collect` is the read adapter rather than the sole owner of cluster access: the manager's caches
live in `controller`, and every mutation goes through `executor`. Keeping writes in one package
means the set of things binpack can change to a cluster is enumerable by reading one directory.

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
watch-backed cache. Both produce the same `Snapshot` — Kubernetes objects as returned, not
translated — so the engine cannot tell them apart, which is what guarantees `explain` describes
what `run` will do.

Objects reaching the engine are **read-only**. The controller path hands out pointers into a
shared informer cache, and writing to one corrupts it for every other consumer in the process.

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

This arithmetic is fiddly and upstream already gets it right, so binpack calls
`k8s.io/component-helpers/resource.PodRequests` rather than hand-rolling it — at the point of
use, not precomputed into a mirror that can drift from what the scheduler does.

**Pod slots.** `pods` appears in `status.allocatable` but never in a pod's requests, so a
uniform "subtract request from remaining" loop would never consume a slot, and the simulation
would happily pack an unlimited number of pods onto a node capped at 110. Every pod's request
map therefore carries a synthetic `pods: 1`, added during collection. The kubelet's pod limit is
a real scheduling constraint and a genuine ceiling that clusters hit while showing plenty of
free CPU and memory.

The engine adds it when accounting for a placement, so the arithmetic stays uniform: it
subtracts resource lists from resource lists, with pod count simply one more entry.

#### Fit predicates come from upstream, not from us

Deciding whether a pod can go on a node lives in `internal/fit`, built on Kubernetes' own
staging libraries — `resource.PodRequests` for effective requests, `nodeaffinity` for selectors
and affinity, `corev1` for taints — and called directly by the engine. Hand-rolling this is how
the defects above arose in the first place.

What binpack understands is a **closed allowlist**, never a list of known exceptions. Stage 1
models `NodeUnschedulable`, `NodeName`, `NodeAffinity` and `nodeSelector`, `NodeResourcesFit`
and `TaintToleration`. Anything outside that set — a `hostPort`, a persistent volume claim,
required inter-pod affinity, hard topology spread, a custom scheduler — makes the node an
invalid candidate, with the specific feature named as the reason.

The direction matters: a denylist of constraints we remembered can never be complete, and every
Kubernetes release lengthens it. An unrecognised feature must refuse by default. Soft
constraints are the exception, ignored rather than refused, because they affect only scoring and
can never cause a placement to fail.

**The allowlist applies in both directions.** It is not enough to inspect the pods being
relocated; the pods already resident on each prospective destination must be inspected too.
Inter-pod affinity is symmetric — the scheduler's filter rejects an incoming pod if an existing
pod on that node declares required anti-affinity matching it. A relocating pod using nothing but
allowlisted features can still be refused by a destination it knows nothing about.

So a destination is disqualified if any pod on it uses a feature outside the allowlist, exactly
as a relocating pod is. Checking only one side would let precisely this case through and leave
the replacement Pending.

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
overprovisioning pattern is already broken. Nothing surfaces this today. Background:
[overprovisioning and expendable pods](../explanation/overprovisioning-and-expendable-pods.md).

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

**A pod matched by more than one PDB can never be evicted at all.** This is not a matter of
allowances. The eviction subresource refuses outright:

> This pod has more than one PodDisruptionBudget, which the eviction subresource does not
> support.

— returned as an HTTP **500**, which is neither retryable nor time-limited. Any candidate
holding such a pod is refused.

This is worth surfacing rather than merely handling, because it is close to invisible. Two PDBs
with overlapping label selectors are easy to create by accident — a team-wide budget and a
service-specific one — and neither object shows anything wrong. Both report healthy status and
sensible allowed disruptions. The only symptom is that the pod is permanently unevictable, so
its node can never be drained by anything, including `kubectl drain` and the cluster-autoscaler
itself. `diagnose` reports it.

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

A drain is not one decision followed by a batch of evictions. The cluster keeps moving
underneath it, and a drain can legitimately take far longer than an evaluation interval, so the
protocol is sequential, revalidates at every step, and is bounded by *progress* rather than by
elapsed time. The reasoning is in [ADR-0007](adr-0007-drain-progress-not-deadlines.md).

0. **Resume before deciding.** Each evaluation first checks whether any node carries a drain
   marker. If one does, that drain is advanced and no new decision is made. Without this, a
   forty-minute drain would have forty evaluations running alongside it, each free to start a
   second one.
1. **Mark, then cordon.** Write the drain annotations, then cordon. Cordoning first and deciding
   afterwards is what makes the next step meaningful: until the node is unschedulable, its pod
   set can still grow.
2. **Re-snapshot and re-decide.** The decision that selected this candidate was made against a
   snapshot that is now stale. Between it and the cordon, the scheduler may have bound new pods
   to this very node — pods whose fit, evictability and PDB demand were never assessed. So
   feasibility and evictability are recomputed against the actual post-cordon pod set. If the
   answer is now no, uncordon, unmark, and record the drain as aborted. Nothing has been evicted
   at this point, so aborting is free.
3. **Evict one pod, then wait.** Evict a single pod and wait for its replacement to be scheduled
   somewhere — not merely created, but bound to a node.
4. **Revalidate and repeat.** Re-snapshot and re-check the remaining pods before each subsequent
   eviction. If the remaining set no longer fits, stop, uncordon, and record a partial drain.
5. **Verify removal.** Once the node is empty, wait up to `removalTimeout` for the autoscaler to
   delete it. If it still exists, uncordon and record the drain as failed.

Every branch of this protocol ends with the node either deleted or uncordoned. That is not
tidiness, it is the central safety property: a cordoned node the autoscaler never removes is
lost schedulable capacity, and if the pool is at its maximum, pods can stay Pending indefinitely
while a healthy node sits idle. binpack must leave the cluster working without human
intervention, including — especially — when its own prediction was wrong.

Sequential eviction makes drains slow. That is acceptable: binpack drains one node at a time,
and trading minutes for the ability to abort at any point is the right side of that trade for a
tool that deletes capacity.

### Slow is not stuck

Steps 3 and 4 have no deadline, because a wall-clock deadline cannot tell a workload that is
shutting down properly from one that is wedged. A StatefulSet with
`terminationGracePeriodSeconds: 3600` behaves correctly by taking 45 minutes; a pod held by a
finalizer never finishes at all. Any single timeout is too short for the first and too long for
the second.

So `stallTimeout` bounds the **absence of progress**. Any of these keeps a drain alive:

- an eviction request was accepted
- the count of relocatable pods on the node decreased
- a pod acquired a `deletionTimestamp`
- a pod is terminating and still within `deletionTimestamp + terminationGracePeriodSeconds`
  plus a small fixed slack

The last is a state rather than an event, which is what makes long grace periods work without
configuration: while a pod is legitimately shutting down, the stall clock does not run.

Being stuck is then **detected**, not inferred. A pod still present past its termination
deadline plus slack is not slow — the kubelet should have sent SIGKILL — so something is wrong:
a finalizer, a volume that will not detach, an unhealthy kubelet. binpack names the pod and how
far past its deadline it is. "Pod monitoring/prometheus-0 is 12 minutes past its termination
deadline" tells an operator where to look; "the drain timed out" does not.

### A failed drain must not be retried immediately

Abandoning a drain uncordons the node — and a partially drained node has *fewer* pods than
before. Since candidates are ordered least-loaded-first, it is now **more** attractive than it
was. Without memory, binpack would preferentially retry its own failures, evicting a few more
pods each time.

So a failed drain records per-node backoff on the node: an attempt count, a
`backoff-until` timestamp and the failure reason. Backoff starts at 30 minutes and doubles to a
24-hour cap, and a node in backoff is not a candidate. Because a successful drain deletes the
node, the state cleans itself up.

This is distinct from `cooldown.afterDrain`, which is cluster-wide and follows a *successful*
drain. One prevents thrash after failure; the other lets the cluster settle after success.

### Why one at a time: binpack does not control placement

There is a limit to what feasibility can promise, and it is worth stating plainly rather than
hiding behind the simulation.

The placement simulation proves that a valid assignment **exists**. It does not oblige the
scheduler to choose that one. Suppose pod A fits either N1 or N2, while pod B fits only N2. The
simulation may pack A onto N1 and B onto N2 and correctly report the drain feasible — and then
the scheduler, following its own scoring, places A on N2, fills it, and leaves B Pending. Every
filter check was sound; the assignment was still wrong.

binpack cannot prevent this, because it does not place pods and cannot steer the scheduler. What
it can do is never have more than one pod in flight. Evicting singly and revalidating means the
only uncertainty at any moment is where one pod lands, and the answer is observed before
anything else is touched. A wrong guess costs one Pending pod and an aborted drain, not a
half-emptied node with several.

The honest statement, which the documentation should make rather than bury: **binpack makes a
scale-up unlikely and immediately detectable; it cannot make it impossible.** A pod that goes
Pending may trigger the autoscaler before binpack aborts. The guarantee in
[ADR-0006](adr-0006-scheduler-fidelity.md) is about the *fit predicate* — if binpack says a pod
fits a node, the scheduler agrees — and does not extend to the scheduler's choice among valid
destinations.

### Recovery state must outlive the process

An in-memory timer is not good enough, because the failure it guards against and the failure
that destroys it are the same kind of event. If binpack is OOM-killed, rescheduled, upgraded, or
simply loses leader election between cordoning a node and observing its removal, an in-process
watch dies with it. The node stays cordoned with nobody left who intends to uncordon it — the
exact outcome the safeguard exists to prevent, reached by a different route.

So the drain marker is written **on the node itself**, before the cordon:

```
binpack.motleyhand.com/drain-started:        2026-08-15T09:15:37Z
binpack.motleyhand.com/drain-progress:       2026-08-15T09:31:02Z
binpack.motleyhand.com/drain-pods-remaining: 4
```

These record how the drain is doing rather than when someone once decided it should be over.

The node is the right home for it. It survives any process failure, it needs no CRD, ConfigMap
or Lease, it requires no permission binpack does not already hold for cordoning, and it is
self-cleaning: when the drain succeeds the node is deleted and the marker goes with it. It is
also visible in `kubectl describe node`, so a human debugging a cordoned node finds out who
cordoned it and when it was due to be released.

On startup and on acquiring leadership, binpack reconciles before doing anything else. For each
node carrying a drain marker it checks **live pod state first**: a pod still terminating within
its grace period means the drain is alive, and fewer pods remaining than the marker records means
progress happened while binpack was away. Either resumes the drain. Only when neither holds, and
the recorded progress has gone stale, is the node uncordoned and the drain recorded as failed.

Reading the annotation age alone would be wrong in the case that matters most: a controller
unavailable for twenty minutes during a legitimate forty-minute shutdown returns to a stale
timestamp and a perfectly healthy drain, and would kill it. The annotation is a fallback for
when no live signal is observable, not the primary test — which keeps recovery and steady-state
behaviour identical, and means recovery does not depend on the process that started the drain
still being alive.

One case remains unrecoverable by binpack alone: uninstalling it mid-drain leaves a marked,
cordoned node with nothing running to release it. The marker makes that state self-describing
rather than mysterious, and `diagnose` reports it.

## Configuration

Mounted from a ConfigMap, parsed into `api/v1alpha1` types. Every field is optional: pools and
their bounds are discovered, so an empty document is a working, safe configuration. Field-level
detail is in the [configuration reference](../reference/configuration.md).

```yaml
apiVersion: binpack.motleyhand.com/v1alpha1
kind: BinpackConfig

interval: 1m0s
dryRun: true                          # safe by default; acting is opt-in

discovery:
  nodeGroupIDLabel: doks.digitalocean.com/node-pool-id
  poolNameLabel: doks.digitalocean.com/node-pool

policy:                               # applies to every discovered pool
  enabled: true
  feasibility:
    expendablePriorityCutoff: -10     # mirrors the autoscaler's own flag
    reserveForLargestPod: true        # not a percentage; see below
  drain:
    maxPodsPerDrain: 0                # 0 = unlimited; a blast-radius guard
    stallTimeout: 10m0s               # abandon if no progress, not if slow
    removalTimeout: 15m0s             # once empty, how long to await deletion
  backoff:
    initial: 30m0s                    # per node, after a failed drain
    max: 24h0m0s
  cooldown:
    afterScaleUp: 10m0s
    afterDrain: 15m0s
  exclusions:
    namespaces: []

pools: []                             # per-pool overrides of the above
```

**Headroom is not a percentage.** An earlier draft had `headroomPercent: 10`, which is exactly
the reasoning this design rejects elsewhere: on a 4GB node it reserves around 136MB, and on an
8GB node twice that, for no principled reason. `reserveForLargestPod` states the actual
requirement instead — after a drain, some node must still hold a pod the size of the largest
relocatable one — because the risk is not "the cluster is full" but "the next pod that restarts
cannot be placed", and that is a question about bytes.

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

`diagnose` will identify PodDisruptionBudgets that permit zero disruptions, pods matched by more
than one PDB and therefore permanently unevictable, pause pods below the expendable cutoff,
other unevictable pods, nodes pinned by affinity, and nodes left cordoned with a stale binpack
drain marker. It will suggest the fix. It will not apply it.

Several of these are worth reporting even to someone who never installs binpack, because they
block the cluster-autoscaler and `kubectl drain` just as thoroughly, and none of them announce
themselves.

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
5. `internal/fit`: the fit predicate, plus the differential test harness against a real
   `kube-scheduler` running on `envtest`
6. Engine: `Snapshot` types and the placement simulation
7. Engine: evictability prediction and `Decision` rendering
8. `internal/collect`, preflight, and `binpack explain`. Integration-tested against real API
   fixtures — init containers, RuntimeClass overhead, extended resources — since a wrong number
   here produces a confidently wrong decision no engine unit test can catch
9. `binpack diagnose`. Reports what blocks scale-down and never remediates: patching a
   disruption budget from a cost tool is both a policy decision that is not binpack's to make
   and one Flux or Argo would reconcile away within minutes, leaving a tool whose reported
   actions did not happen. Severity and prose are properties of the *code* rather than of each
   finding, so instances collapse into one explanation with many subjects. See
   [the diagnostics reference](../reference/diagnostics.md).
10. `binpack run`, dry-run only. The manager, its caches, leader election, and Events on the
    node. `collect` reads through controller-runtime's `client.Reader` for both frontends
    rather than gaining a second path — a direct client for the one-shot commands, the
    manager's cache for the controller — which is what keeps `explain` a truthful preview of
    `run` rather than a parallel implementation of it. `--once` evaluates once and exits, for
    a CronJob. `dryRun: false` is refused while there is no executor, rather than silently
    reporting decisions nobody acts on.
11. Executor: cordon, evict, verify, uncordon; cooldown and backoff state.
    **Blocked on** `collect` reading owner templates: until binpack derives the replacement
    pod's spec from its controller rather than from the running pod, a pod resized downward in
    place can be approved for a node its replacement does not fit. Harmless while nothing acts
    on a decision; not harmless once something does. See
    [ADR-0006](adr-0006-scheduler-fidelity.md).
12. Helm chart and RBAC
13. Release pipeline. **Includes migrating `.goreleaser.yaml` from `dockers` to `dockers_v2`**,
    which GoReleaser now warns is being phased out. Deliberately deferred to here rather than
    done piecemeal: the publishing configuration is written in this step anyway, and the new
    schema is not yet in GoReleaser's published `schema.json`, so it should be verified against
    the version CI actually runs rather than guessed at.

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
