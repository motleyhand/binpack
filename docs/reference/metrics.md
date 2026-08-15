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
| `binpack_evaluation_errors_total` | counter | — | Evaluations that could not be completed, usually a failed read |
| `binpack_evaluation_duration_seconds` | histogram | — | Reading the cluster through to reaching a decision |
| `binpack_last_evaluation_timestamp_seconds` | gauge | — | When the last evaluation completed |

`code` is one of:

| Code | Meaning |
|---|---|
| `drain` | A node was chosen |
| `no-autoscaler` | No live cluster-autoscaler; binpack will not act |
| `no-candidates` | Every node was ruled out before any simulation ran |
| `none-feasible` | Nodes were simulated and none could be emptied |

The last two are worth telling apart: `no-candidates` is a configuration answer, `none-feasible`
is a capacity one.

### Nodes

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `binpack_nodes` | gauge | `verdict` | Nodes by verdict at the last evaluation |
| `binpack_nodes_skipped` | gauge | `code` | Nodes ruled out before simulation, by reason |
| `binpack_drainable_nodes` | gauge | — | Nodes whose whole workload was shown to fit elsewhere |

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

## See also

- [Diagnostics](diagnostics.md) — `binpack diagnose`, which explains blocked scale-down in prose
- [RBAC](rbac.md) — the metrics endpoint needs no permissions; scraping it is a plain HTTP GET
