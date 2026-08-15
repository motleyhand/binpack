# binpack architecture

- **Status:** accepted, partially implemented
- **Date:** 2026-08-15

The design specification. Individual decisions and their rationale live in the ADRs alongside
this file; this document describes the system they add up to.

## What binpack does

Once per interval, binpack asks a single question about one node:

> If this node were drained, would its workload fit on the remaining nodes without triggering a
> scale-up?

If yes, it drains the node and lets the cluster-autoscaler reap it. If no, it explains why and
does nothing. Everything below is in service of answering that question correctly and being
able to show its working.

## Components

```
cmd/binpack            thin main(), calls internal/cli

api/v1alpha1           configuration types (CRD-shaped), defaulting, validation

internal/
  engine               PURE. no Kubernetes imports. Snapshot -> Decision
  collect              the ONLY package holding Kubernetes clients. builds a Snapshot
  cli                  cobra commands: explain, diagnose, run, version
  controller           controller-runtime manager + periodic Runnable
  executor             cordon and evict, via k8s.io/kubectl/pkg/drain
  state                cooldown and anti-thrash memory
  metrics              Prometheus collectors

charts/binpack         Helm chart, RBAC, ConfigMap
```

The dependency graph is deliberately shallow. `engine` depends on nothing but the standard
library. `collect` depends on Kubernetes client libraries and on `engine`'s types. `cli` and
`controller` depend on both and on each other not at all.

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

## The decision procedure

Evaluated per run. Steps are ordered cheapest-first so that common no-ops cost nothing.

1. **Cooldown.** Has the cluster grown recently? This mirrors the cluster-autoscaler's own
   `scale-down-delay-after-add`: draining immediately after a scale-up is how oscillation
   starts. Answerable from node `creationTimestamp`s, so it needs no persistence.
2. **Scope.** Consider only nodes in a configured autoscale pool. A node the autoscaler cannot
   delete — a static pool, a control-plane node — must never be a candidate, because draining
   it produces churn and no saving.
3. **Pool floor.** Is the pool above its configured minimum? At the minimum, the autoscaler
   will replace whatever is drained.
4. **Candidate selection.** Among in-scope nodes, pick the least loaded. A utilisation
   threshold bounds the work and avoids churning nodes doing real work.
5. **Feasibility.** Sum the relocatable resource requests on the candidate. Sum free allocatable
   capacity across all *other* schedulable nodes. If the former exceeds the latter, draining
   triggers a scale-up and achieves nothing — skip. Computed per resource: memory, CPU and pod
   count.
6. **Evictability.** Predict, rather than discover, whether each pod can actually be evicted:
   PodDisruptionBudget slack, `cluster-autoscaler.kubernetes.io/safe-to-evict`, controller
   ownership, local storage, node selectors and affinity. A drain that will hit a PDB wall
   should be predicted and skipped, not attempted and failed.
7. **Drain.** Cordon, then evict. The autoscaler reaps the node on its own schedule, typically
   within ten minutes.

**Default: one node per run.** Iterative beats clever. The next run observes fresh state, which
is both safer and dramatically simpler to reason about than planning a multi-node consolidation
against a cluster that is changing underneath you.

### Feasibility is computed on requests, not usage

The scheduler places pods according to requests, so a feasibility check on actual usage would
be answering a different question than the one that determines whether the pods can be
rescheduled. Usage may optionally inform *step 4* — "is this node doing real work?" — but never
step 5.

### Three properties that distinguish this from the descheduler

- **Absolute quantities, not percentages.** On a cluster of mixed node sizes, a 4GB node at 81
  percent holds around 1.1GB of requests, while an 8GB node at 60 percent has around 2.7GB
  free. Percentage thresholds call the first node "busy" and do nothing. Byte arithmetic sees
  that it fits with room to spare.
- **Cross-pool awareness.** Free capacity is summed across **all** schedulable nodes, while only
  autoscale-pool nodes are ever drain candidates. This asymmetry is what makes a static-plus-
  elastic topology work, and it is not expressible in the descheduler's configuration.
- **Feasibility before action.** The descheduler evicts and finds out. binpack decides and then
  evicts.

### Expendable pods

Pods below the cluster-autoscaler's `--expendable-pods-priority-cutoff` (default -10) do not
need to fit anywhere: the autoscaler will terminate a node running them without ceremony. They
are excluded from the "must fit elsewhere" set.

Overprovisioning pause pods are a subtler case. They must sit **at or above** the cutoff, or the
autoscaler ignores their Pending state and never replenishes the warm-capacity buffer — so by
the autoscaler's accounting they are real workload. But their entire purpose is to be
preempted. Counting them as workload that must be relocated makes consolidation look infeasible
when it is not.

Resolution: exclusion is configurable **by PriorityClass name**, so the operator states which
classes are filler. Guessing — for example, treating the lowest priority in the cluster as
expendable — is the kind of inference that is right most of the time and catastrophic the rest.

## Configuration

Mounted from a ConfigMap, parsed into `api/v1alpha1` types. Sketch, not final — the field-level
reference is written with the implementation.

```yaml
apiVersion: binpack.motleyhand.com/v1alpha1
kind: BinpackConfig

interval: 60s
dryRun: true                       # safe by default; acting is opt-in

pools:
  - labelKey: doks.digitalocean.com/node-pool
    labelValue: pool-4g
    minNodes: 2

feasibility:
  resources: [memory, cpu, pods]
  headroomPercent: 10              # do not plan to fill the cluster exactly

candidates:
  drainBelowPercent: 60
  maxDrainsPerRun: 1

cooldown:
  afterScaleUp: 10m
  afterDrain: 15m

exclusions:
  expendablePriorityClasses: [overprovisioning]
  namespaces: []
  nodeAnnotation: binpack.motleyhand.com/skip
```

## Command surface

| Command | Reads | Writes | Purpose |
|---|---|---|---|
| `binpack explain` | kubeconfig | nothing | Print the arithmetic and the verdict for every node |
| `binpack diagnose` | kubeconfig | nothing | Report what is currently blocking scale-down |
| `binpack run` | in-cluster | drains, if enabled | The controller |
| `binpack version` | — | — | |

`explain` and `diagnose` are the adoption path: they are useful before anything is installed,
they need only read access, and they are how someone decides whether to trust the tool.

`diagnose` deserves particular emphasis. In the cluster that prompted this project, every
consolidation failure was a PodDisruptionBudget on a single-replica deployment permitting zero
voluntary disruptions. No consolidation tool could have helped; the fix was a one-line change
to a PDB. Reporting that is higher-value and far lower-risk than anything binpack does by
draining.

## Observability

The controller emits a Kubernetes Event on the Node for every decision, so `kubectl describe
node` explains why a node was or was not drained without access to binpack's logs. This
deliberately mirrors how the cluster-autoscaler's decisions surface on a managed control plane.

Prometheus metrics (prefix `binpack_`) cover: decisions by action and reason, drains attempted
and succeeded, eviction failures, per-pool node counts, and the feasibility gap in bytes. The
last is the one to alert on — a persistent gap means the cluster genuinely needs its nodes.

## Roadmap

1. ✅ Foundation and decisions
2. Harvested documentation
3. Go scaffold and CI
4. `api/v1alpha1` configuration types
5. Engine: `Snapshot` types and the feasibility check
6. Engine: evictability prediction and `Decision` rendering
7. `internal/collect` and `binpack explain`
8. `binpack diagnose`
9. `binpack run`, dry-run only
10. Executor: real cordon and eviction, cooldown, anti-thrash state
11. Helm chart and RBAC
12. Release pipeline

Each of steps 4 onward gets its own specification before implementation.

## Open questions

**How aggressive should the utilisation threshold be, or is it needed at all?** Too low and
binpack never fires; too high and it churns nodes doing real work. There is a case that the
threshold should not exist: "drain if the workload fits elsewhere" may be sufficient on its
own, with a percentage guard serving only to bound how much work each run does. Resolve with
real data from `explain` before choosing a default.

**Should node sizing be recommended?** Per-node kubelet and system reservations are roughly
fixed, so larger nodes waste proportionally less — a 4GB node loses 15–17 percent to
reservations, an 8GB node around 9 percent. A single pool of larger nodes also pushes the
110-pod-per-node ceiling further away. The cost is coarser scale granularity. This is advice
binpack could offer from data it already collects, but it is not a v1 concern.

**Should `diagnose` ever remediate?** Flipping `minAvailable: 1` to `maxUnavailable: 1` on an
offending PDB is a one-line, well-understood fix. But a cost tool mutating availability policy
is a hard sell, and reporting is the safer default. Revisit only with explicit opt-in and a
diff shown first.
