# Metrics reference

`binpack run` serves Prometheus metrics on `:8080/metrics` by default
(`--metrics-bind-address`, or `0` to disable). Every name is prefixed `binpack_`.

**These names are public API from the first release.** People build alerts on them, and an alert
that silently stops firing because a series was renamed is worse than no alert at all.

## What to alert on

Three things, in order of usefulness.

### `binpack_last_evaluation_timestamp_seconds` going stale

A binpack that has stopped deciding looks exactly like one with nothing to do: no drains, no
events, no errors. This is the series that tells them apart.

```promql
time() - binpack_last_evaluation_timestamp_seconds > 600
```

### `binpack_autoscaler_up == 0`

binpack refuses to act at all without a live cluster-autoscaler, because draining a node
nothing will remove is strictly worse than doing nothing. A zero here means consolidation has
stopped, and it also means your cluster is not scaling for any other reason either.

### `binpack_drainable_nodes` persistently zero — **with `infeasible` nodes**

The one the design calls out, but zero on its own does not mean what it looks like. It is also
zero when every node was skipped, when every feasible node is blocked by a disruption budget,
and when there is no autoscaler at all. Alerting on the bare number produces a standing "you
need these nodes" that is usually false.

The capacity case is zero drainable **while nodes were actually simulated and failed**:

```promql
max_over_time(binpack_drainable_nodes[6h]) == 0
  and min_over_time(binpack_nodes{verdict="infeasible"}[6h]) > 0
  and min_over_time(binpack_autoscaler_up[6h]) == 1
```

That is a cluster which genuinely needs the nodes it has: no amount of consolidation will
change it, and the answer is smaller requests, fewer replicas, or a different node size.

The other three zeroes are different problems with different answers, and are worth separate
alerts rather than being folded into this one:

| Also zero when | Query | What it means |
|---|---|---|
| Everything was blocked | `binpack_nodes{verdict="blocked"} > 0` | A node's workload fits elsewhere but some pod cannot be evicted. Run `binpack diagnose` |
| Everything was skipped | `binpack_nodes{verdict="skipped"} > 0 and binpack_nodes{verdict="infeasible"} == 0` | Nothing was even considered. `binpack_nodes_skipped` says why |
| No autoscaler | `binpack_autoscaler_up == 0` | Covered by its own alert above |

## Every series

### Evaluation

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `binpack_evaluations_total` | counter | `code` | Evaluations completed, by outcome |
| `binpack_evaluation_errors_total` | counter | — | Evaluations that could not be completed: a failed read, or a write the API server refused |
| `binpack_evaluation_duration_seconds` | histogram | — | Reading the cluster through to reaching a decision |
| `binpack_last_evaluation_timestamp_seconds` | gauge | — | When the last evaluation completed |

`code` is one of:

| Code | Meaning |
|---|---|
| `drain` | A node was chosen |
| `no-autoscaler` | No live cluster-autoscaler; binpack will not act |
| `no-candidates` | Every node was ruled out before any simulation ran |
| `none-feasible` | Nodes were simulated and none could be emptied |
| `draining` | A drain was already under way, so this evaluation advanced it rather than deciding afresh |

The last two are worth telling apart: `no-candidates` is a configuration answer, `none-feasible`
is a capacity one.

### Nodes

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `binpack_nodes` | gauge | `verdict` | Nodes by verdict at the last evaluation |
| `binpack_nodes_skipped` | gauge | `code` | Nodes ruled out before simulation, by reason |
| `binpack_drainable_nodes` | gauge | — | Nodes whose whole workload was shown to fit elsewhere |
| `binpack_nodes_unmodelled` | gauge | — | Nodes refused because a pod's controller template could not be read |

`verdict` is one of `skipped`, `infeasible`, `blocked`, `drainable`. All four are always
published, reporting zero rather than disappearing — "no drainable nodes" and "binpack is not
reporting" must not look the same.

`code` on `binpack_nodes_skipped` is one of:

| Code | Meaning |
|---|---|
| `not-autoscaled` | The node's pool is not managed by the cluster-autoscaler |
| `pool-disabled` | `enabled: false` for this pool |
| `scale-up-in-progress` | The cluster is growing right now |
| `cooldown-after-scale-up` | The cluster grew recently |
| `cooldown-after-drain` | binpack drained a node recently |
| `pool-at-minimum` | The pool is at its configured floor |
| `annotated-skip` | The node carries `binpack.motleyhand.com/skip` |
| `drain-in-progress` | A binpack drain is already under way on this node |
| `backoff` | A previous drain failed; binpack is waiting before retrying |
| `cordoned` | Already cordoned by somebody else |
| `protected-pod` | The node runs a pod binpack must not evict |
| `too-many-pods` | The drain would exceed `maxPodsPerDrain` |

These are the same codes `binpack explain --output json` reports per node, so a dashboard and an
investigation use one vocabulary.

### Drains

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `binpack_drains_started_total` | counter | — | Drains begun, by marking and cordoning a node |
| `binpack_drains_completed_total` | counter | — | Drains that ended with the autoscaler removing the node |
| `binpack_drains_abandoned_total` | counter | `reason` | Drains handed back by uncordoning, by reason code |
| `binpack_nodes_in_backoff` | gauge | — | Nodes excluded because a drain of them recently failed |
| `binpack_drain_attempts_max` | gauge | — | The highest consecutive failed-drain count on any node |

`reason` is one of:

| Code | Meaning |
|---|---|
| `stuck` | A pod is past its termination deadline — a finalizer, a volume that will not detach, or an unhealthy kubelet |
| `stalled` | Nothing moved for `stallTimeout`, and nothing was shutting down |
| `not-removed` | The node was emptied but the autoscaler did not remove it within `removalTimeout` |
| `replacement-unschedulable` | A pod that moved off the node could not be placed anywhere |
| `unaccounted-pods` | Pods remained that the simulation had not accounted for |

Any of the skip codes above can also appear, when the cluster changed underneath a drain and
revalidation stopped it — a pool reaching its minimum, an operator annotating the node, the
cluster growing before anything had moved. So can `infeasible` and `blocked`, for the two outcomes that carry no skip
code: the remaining pods stopped fitting elsewhere, and a disruption budget stopped allowing
their eviction.

`not-autoscaled` is worth reading carefully in this position. On `binpack_nodes_skipped` it means
what the table says; as an abandonment it also covers the cluster-autoscaler's status having gone
stale mid-drain, including while binpack was waiting for the autoscaler to remove a node it had
tainted itself. The note on the `DrainAbandoned` event says which.

`scale-up-in-progress` and `cooldown-after-scale-up` are narrower here than the table above
suggests. As an abandonment, either means the cluster grew **between the cordon and the first
eviction** — the window in which stopping costs nothing, because nothing has moved yet. Once a
drain has relocated a pod, growth elsewhere in the cluster no longer ends it: abandoning would
leave those pods where they went and buy nothing, while the questions that could make the drain
unsound are re-asked directly on every evaluation.
[ADR-0010](../design/adr-0010-a-scale-up-stops-a-drain-that-has-not-started.md) has the
reasoning. On `binpack_nodes_skipped` both codes are unchanged.

`blocked` moves in both directions here, and the two halves are worth reading apart. It no
longer fires for a budget whose controller has not caught up with its spec — `observedGeneration`
behind `generation`, which the eviction API refuses outright whatever the recorded allowance
says. The disruption controller resyncs on the very write that bumped the generation, so that
condition lasts one sync; a drain in flight now waits for it instead of ending over it, bounded
by `stallTimeout` like every other wait. It does now fire for the eviction API refusing a pod
outright at the moment of eviction — a pod covered by two budgets, created between the
assessment and the eviction. That case previously ended the evaluation instead, leaving a
cordoned node with no recorded reason until a later evaluation reached this same code through
revalidation. On `binpack_nodes{verdict="blocked"}` nothing changes:
at selection a blocker of any kind still rules a candidate out, where refusing costs nothing.

Every one of these has a series from startup, at zero. A counter that only appears once it has
fired makes a `rate()` alert silently useless until the first occurrence.

A rising `binpack_drains_abandoned_total` is worth attention whatever the reason: every
abandoned drain is churn that bought nothing. `binpack_drain_attempts_max` climbing means the
same node keeps failing, and the backoff behind it doubles from 30 minutes to a daily retry.

Deliberately no node label on either gauge. Node names would carry into the monitoring system,
which the rest of this surface avoids, and the question worth alerting on — is binpack failing
repeatedly — is answered without them. `binpack diagnose` names the node.

### Pools

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `binpack_pool_nodes` | gauge | `pool` | Ready nodes, as the cluster-autoscaler reports them |
| `binpack_pool_min_nodes` | gauge | `pool` | The pool's configured minimum |
| `binpack_pool_max_nodes` | gauge | `pool` | The pool's configured maximum |

`binpack_pool_nodes` counts nodes that are **registered and ready**, which is what you are
paying for. It is deliberately not the number binpack compares against the floor: that is the
lower of ready and the autoscaler's current target, and the two differ exactly while a
scale-down is in progress — the target has dropped and the nodes are still there.

So do not infer "this pool is at its minimum" by comparing the two series. binpack reports that
conclusion directly:

```promql
binpack_nodes_skipped{code="pool-at-minimum"} > 0
```

`pool` is the human-readable pool name from the node label, falling back to the provider's
group identifier for a pool with no nodes to take a name from.

Pools that no longer exist stop being reported rather than freezing at their final size.

### Autoscaler

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `binpack_autoscaler_up` | gauge | — | 1 when a live cluster-autoscaler was found |

## Notes on cardinality

Every label value is drawn from a bounded set: the verdicts, skip codes and outcome codes above,
plus pool names, which are counted in single digits.

The engine's prose reasons are deliberately **not** exposed as labels. They name individual
nodes and pods — "draining would leave nowhere for a pod the size of
`monitoring/prometheus-…`" — and a label whose values are unbounded is how a monitoring system
falls over. The prose is in the logs, in the Events on the node, and in `binpack explain`.

### When `binpack_nodes_unmodelled` is above zero

binpack asks whether a pod's *replacement* would fit, which it reads from the pod's controller.
For a pod created by an operator's own CRD there is no template to read, and binpack refuses to
move it rather than sizing it from the running pod — the inference that is unsound, since a pod
resized downward in place would be sized too small.

Zero on every cluster measured so far. Persistently above zero means the four readable kinds
(ReplicaSet, StatefulSet, DaemonSet, Job) do not cover your workloads, and it is worth saying
so — [ADR-0006](../design/adr-0006-scheduler-fidelity.md) settles the allowlist against
measurement, and this is the measurement.

It is deliberately separate from the ordinary infeasible count: "the workload does not fit" is a
fact about your cluster, and "binpack cannot tell what the workload is" is a gap in binpack.

## Replicas that have not evaluated

Only the leader evaluates, but every replica serves its own metrics endpoint — and a rolling
update always has two for a while.

A replica that has not completed an evaluation **publishes none of the series above**, rather
than publishing them at zero. A standby full of zeroes — no drainable nodes, no autoscaler, an
evaluation timestamp at the epoch — is indistinguishable from a cluster in serious trouble, and
would make every alert here fire or flap depending on which pod answered the scrape.

The counters are the exception and are always published: a replica that has completed no
evaluations reporting zero of them is simply true.

This means the alerts above need no `leader` filter. It also means a target reporting no
`binpack_` series at all is a standby, not a fault. controller-runtime's own
`leader_election_master_status` identifies the leader if you want it on a dashboard.

## After a failed evaluation

`binpack_evaluation_errors_total` increments and **the gauges are left alone**. They describe
the last evaluation that reached a conclusion.

Zeroing them would turn a broken permission into what looks like a cluster with nothing left to
consolidate — the one state binpack most needs to distinguish. Alert on the error counter and
on the evaluation timestamp; do not infer health from the gauges alone.

One failure does not stop binpack. A read the API server could not answer, an eviction a
disruption budget refused, a node patch a webhook rejected: the evaluation ends, this counter
moves, and the next interval re-reads the cluster and decides again. A drain in progress is
unaffected, because its state is on the node rather than in the process.

**Five consecutive failures do stop it**, and the pod restarts. That bound is what distinguishes
a cluster having a bad minute from a deployment that will never work again — a permission that
has been narrowed, an admission webhook that denies binpack's patches — where retrying quietly
for ever would leave binpack reporting healthy while doing nothing at all. A binpack that is
restarting *and* whose error counter is climbing is telling you which of the two you have.

## See also

- [Diagnostics](diagnostics.md) — `binpack diagnose`, which explains blocked scale-down in prose
- [RBAC](rbac.md) — the metrics endpoint needs no permissions; scraping it is a plain HTTP GET
