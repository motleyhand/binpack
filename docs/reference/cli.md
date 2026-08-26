# CLI reference

Every command binpack ships, its flags and their defaults, the exit codes, the JSON each
command emits, and the Events the controller writes.

Commands, flag names and flag **value** vocabularies are public API, on the terms
[versioning.md](versioning.md) sets out. The prose each command prints is not: summaries, fixes
and `explain`'s reasons may be reworded in any release, which is why every one of them carries a
code beside it.

```
binpack explain     Show what binpack would do, and the arithmetic behind it
binpack diagnose    Report what is stopping this cluster from shrinking
binpack run         Run binpack as a controller
binpack config validate
                    Check a configuration file and show what it resolves to
binpack version     Print build information
```

## Global flags

| Flag | Default | Meaning |
|---|---|---|
| `--output` | `text` | Output format: `text` or `json` |
| `--help`, `-h` | — | Help for the command |

`--output` is inherited by every command. Both values are public: a script pinned to
`--output json` will keep working, and so will one parsing the text of `binpack version`.

Three commands read a cluster — `explain`, `diagnose` and `run` — and all three take the same
connection flags:

| Flag | Default | Meaning |
|---|---|---|
| `--kubeconfig` | the usual rules (`$KUBECONFIG`, then `~/.kube/config`; `run` tries in-cluster first) | Path to a kubeconfig |
| `--context` | the kubeconfig's current context | Context to use |
| `--file`, `-f` | `/etc/binpack/config.yaml` if it exists, else built-in defaults | Configuration file |

`-f` defaulting to the chart's mount path is deliberate: `kubectl exec deploy/binpack -- binpack
explain` then answers about the binpack running beside it, rather than about one configured with
defaults. Those are different questions.

Everything else is configuration rather than a flag — see
[configuration.md](configuration.md). There is no flag for a policy setting, and adding one
would give an operator two places to look for the same answer.

## Exit codes

| Exit | Meaning |
|---|---|
| `0` | The command ran and had nothing to report |
| `1` | The command could not run, or refused to: an unreachable cluster, an unknown flag, an invalid configuration, or a preflight refusal |
| `2` | `binpack diagnose --fail-on` only: the command ran and findings reached the threshold |

`2` is reserved for `diagnose`'s gate, so a CI job can tell "your cluster has blockers" from
"diagnose could not reach the cluster". Every other command uses `0` and `1` only.

**A preflight refusal exits 1, including from `diagnose`.** `explain`, `diagnose` and `run` all
refuse to answer when a live cluster-autoscaler publishes node groups and no node carries a value
any of them answers to, because every node would otherwise be reported skipped for a reason that
is really a misconfiguration. That is diagnose declining to run rather than diagnose finding
something, so it is `1` and not `2`.

## `binpack explain`

Reads the cluster and prints the decision binpack would reach, with its reasoning for every
node. Read-only: it never cordons, evicts or writes anything.

It runs the same decision function the controller runs. Where it cannot see what the controller
sees — the cooldown after a drain, which lives in the controller's memory — it says so rather
than answering as though the control were unset.

Connection flags only; see above.

### `explain --output json`

```json
{
  "autoscaler": {
    "running": true,
    "scaleDownStatus": "NoCandidates",
    "pools": [{"id": "…", "name": "pool-4g", "min": 1, "max": 10, "ready": 4}]
  },
  "pools": {"source": "…", "label": "…", "groups": {"…": "…"}, "alsoAgreed": ["…"]},
  "config": "/etc/binpack/config.yaml",
  "dryRun": true,
  "notEvaluated": ["…"],
  "action": "drain",
  "code": "drain",
  "node": "node-a",
  "reason": "…",
  "drain": {"node": "node-b", "wouldHappen": "…"},
  "nodes": [
    {
      "name": "node-a", "pool": "pool-4g", "group": "…",
      "chosen": true, "draining": false, "verdict": "drainable",
      "code": "", "detail": "…", "relocates": 3,
      "blockers": [{"code": "bare-pod", "message": "…"}],
      "unmodelled": false,
      "refusals": {"node-b": {"code": "untolerated-taint", "message": "…"}}
    }
  ]
}
```

| Field | Type | Meaning |
|---|---|---|
| `autoscaler` | object | What binpack found out about the component that would remove a drained node |
| `autoscaler.running` | bool | Whether binpack found a cluster-autoscaler it would rely on |
| `autoscaler.scaleDownStatus` | string | The autoscaler's own `scaleDown` value, when it published one |
| `autoscaler.pools` | array | One entry per node group the autoscaler publishes. **Always an array**, never `null` |
| `autoscaler.pools[].id` | string | The identifier the cluster-autoscaler uses. The join key |
| `autoscaler.pools[].name` | string | The readable pool name, where the cluster carries one |
| `autoscaler.pools[].min`, `.max`, `.ready` | int | The pool's floor, ceiling, and ready node count |
| `pools.source` | string | How nodes were joined to groups. A sentence, not an enum |
| `pools.label` | string | The node label the join reads |
| `pools.groups` | object | Label value to published identifier, where the two differ |
| `pools.alsoAgreed` | array | Other labels that would have produced the same join |
| `config` | string | Where the configuration came from, or `built-in defaults` |
| `dryRun` | bool | That configuration's mode. The verdict is a prediction under one value and a plan under the other |
| `notEvaluated` | array | Controls governing the deployed binpack that `explain` cannot evaluate, as sentences |
| `action` | string | `drain` or `none` |
| `code` | string | The outcome, from the set [metrics.md](metrics.md#evaluations) documents for `binpack_evaluations_total` |
| `node` | string | The node chosen, when one was |
| `reason` | string | Prose. **Not public** |
| `drain` | object | The drain already under way, absent when there is none. Present even when the decision is `no-autoscaler` |
| `drain.node` | string | The node being drained |
| `drain.wouldHappen` | string | What revalidation and the drain's own bound observed. Prose, **not public**, and not a prediction of how the drain ends |
| `nodes` | array | One entry per node assessed. **Always an array**, never `null` |
| `nodes[].name` | string | The node |
| `nodes[].pool` | string | What to call its pool: the readable name where the cluster carries one, the group identifier otherwise |
| `nodes[].group` | string | The group identifier, joining this row to `autoscaler.pools[].id` |
| `nodes[].chosen` | bool | Whether this is the node the decision named |
| `nodes[].draining` | bool | Whether this row's verdict came from revalidating a drain in progress rather than from selection |
| `nodes[].verdict` | string | `skipped`, `infeasible`, `blocked` or `drainable` |
| `nodes[].code` | string | Why it was skipped, from the set [metrics.md](metrics.md#nodes) documents for `binpack_nodes_skipped` |
| `nodes[].detail` | string | Prose. **Not public** |
| `nodes[].relocates` | int | How many pods a drain would move |
| `nodes[].blockers` | array | Pods that could not be evicted, as `{code, message}` |
| `nodes[].unmodelled` | bool | Whether the refusal was "binpack could not predict the replacement" rather than "it did not fit". Exactly the set `binpack_nodes_unmodelled` counts |
| `nodes[].refusals` | object | Destination name to `{code, message}`: which wall each candidate destination hit |

`blockers[].code` is an eviction blocker code — the same vocabulary
[diagnostics.md](diagnostics.md) documents, since six of the seven are also diagnosis codes. The
seventh, `pdb-insufficient`, appears only here: it says a budget allows fewer disruptions than
*this* drain needs, which is not a condition `diagnose` can report about a cluster at rest.
`refusals[].code` is one of the placement refusal codes:

| Code | The destination refused because |
|---|---|
| `node-unschedulable` | It is cordoned |
| `node-not-ready` | Its `Ready` condition is not true |
| `untolerated-taint` | It carries a taint the pod does not tolerate |
| `node-affinity` | The pod's required node affinity or node selector does not match it, or could not be evaluated |
| `insufficient-resources` | It has not enough of some resource left, `pods` included |
| `pod-anti-affinity` | Some pod's required anti-affinity could reject this one there — a resident of that node, or a pod anywhere in a topology domain it is in |
| `unsupported-pod-feature` | The pod uses something binpack does not model, so no destination was assessed at all |
| `unsupported-node-feature` | It has not declared a feature the pod requires, or binpack could not work out which features the pod requires |

`pod-anti-affinity` is a **conservative** refusal: binpack refuses whenever a term *could* match
rather than deciding that it does, so it declines some placements the scheduler would accept.
That direction is deliberate — the other one drains a node whose pods then cannot be placed.

`unsupported-pod-feature` is binpack declining to model your pod, not a statement about your
cluster: it is the refusal [ADR-0006](../design/adr-0006-scheduler-fidelity.md) says the
allowlist should be widened on, and these codes are the evidence that argument was always meant
to rest on.

## `binpack diagnose`

Reports what is stopping the cluster from shrinking. Read-only, and it never remediates.

Needs no configuration and no binpack installation, so it is worth running before deciding
whether you want the controller at all.

| Flag | Default | Meaning |
|---|---|---|
| `--fail-on` | `never` | Exit `2` when findings reach this severity: `never`, `blocking` or `warning` |
| `--fail-on-static-pools` | false | Also count findings on pools the cluster-autoscaler does not manage |

`--fail-on` is a threshold and not a filter: `warning` fails on blockers too. There is
deliberately no `info` level. The severity vocabulary and every code are in
[diagnostics.md](diagnostics.md).

### `diagnose --output json`

An array of findings, **always an array** — a healthy cluster yields `[]`, so nothing downstream
needs a special case for it.

| Field | Type | Meaning |
|---|---|---|
| `severity` | string | `blocking`, `warning` or `info` |
| `code` | string | The diagnosis, from [diagnostics.md](diagnostics.md) |
| `subject` | string | The object the finding is about |
| `detail` | string | Specifics — a number, a volume, a node. Prose, **not public** |
| `summary` | string | What it means. Prose, **not public** |
| `fix` | string | What to change. Prose, **not public** |
| `freesNothing` | bool | The finding is real but on a pool nothing would remove a node from today |

## `binpack run`

Runs binpack as a controller: once per interval it evaluates the cluster and, unless `dryRun` is
set, drains one node.

| Flag | Default | Meaning |
|---|---|---|
| `--once` | false | Evaluate once and exit, for running as a CronJob rather than a Deployment |
| `--metrics-bind-address` | `:8080` | Prometheus endpoint, or `0` to disable |
| `--health-probe-bind-address` | `:8081` | Health and readiness endpoints, or `0` to disable |
| `--leader-election` | true | Coordinate through a Lease so only one binpack acts at a time |
| `--leader-election-namespace` | the namespace binpack runs in | Namespace holding the Lease |
| `--lease-duration` | `1m` | How long a lease is held before another replica may take it |
| `--renew-deadline` | `40s` | How long the leader keeps trying to renew before giving up and exiting |
| `--retry-period` | `10s` | How often the lease is renewed or contested |

`run` emits no JSON: what it decided is in [the metrics](metrics.md), in its log, and in the
Events below.

### Events

binpack writes Events onto the **node** a decision is about, or — for a decision about no
particular node — onto the node it assessed last. On a managed control plane `kubectl describe
node` is the one surface a cluster user reliably has.

Every Event carries `action: Consolidate`, so they filter as a group. The reason says which:

```bash
kubectl get events --field-selector reason=Draining -A
```

| Reason | When |
|---|---|
| `WouldDrain` | A drain was decided on and not performed, because `dryRun` is true |
| `Draining` | The same decision, acted on |
| `WouldAdvanceDrain` | A drain already in progress that binpack is not advancing, because `dryRun` is true |
| `Drained` | A drain finished: the node was emptied and removed |
| `DrainAbandoned` | A drain ended without the node going away. The note says what stopped it |
| `NoCandidates` | Nothing was eligible — every node was ruled out |
| `NoneFeasible` | Nodes were simulated and none could be emptied |
| `NoNodeChosen` | No node was chosen, for some other reason the note gives |

The first two refusal reasons are the `binpack_evaluations_total{code}` values in CamelCase, so
a dashboard and a node say the same word.

**The note is not public and cannot be relied on to change.** The events.k8s.io aggregation key
covers the reason and not the note, and an Event whose key is already cached is discarded rather
than merged — so the note the API server holds is the one the *first* Event of a series carried,
for as long as binpack keeps emitting. Anything that varies belongs in a reason of its own,
which is why there are eight of these and not three.

## `binpack config validate`

Parses a configuration file, applies defaults and validates it, then prints what it resolves to.
Reads standard input when no file is given, so it can be piped from `kubectl get configmap … -o
jsonpath`.

Empty input is valid and yields the defaults: pools and their bounds are discovered rather than
declared, so there is nothing an operator must supply.

| Flag | Default | Meaning |
|---|---|---|
| `--file`, `-f` | standard input | Configuration file to read |

Two fields name something only a cluster can confirm, and **neither is checked here**: a
`pools[]` entry has to name a pool the cluster has, and a `discovery.nodeGroups` entry has to name
a node group the cluster-autoscaler publishes. This command reads a document, not a cluster, so it
resolves and prints both like any other setting and then says underneath that it did not check
them. Each line appears only when the document carries that field — there is nothing to disclose
about a document that states neither.

`binpack explain` and `binpack diagnose` check both against the cluster, and the controller treats
an unknown name as a fatal configuration error. It resolves pools on every evaluation but holds
that error until it has resumed any drain already in progress — exiting one tick into a drain
would leave a node cordoned with nothing left to uncordon it — so `binpack run --once` can advance
a drain and exit without reporting the error. A drain advances by at most one step per evaluation,
so it can span many one-shot runs; the error is reported by the first run that finds no drain in
progress, not by the next one. `binpack explain` reports it immediately either way, and is the
command to reach for when a `--once` run has said nothing.

`apiVersion` and `kind` are emitted so the report is a document rather than a report *about* a
document — see below.

### `config validate --output json`

The **effective** settings, in the configuration document's own vocabulary — a document that
would produce exactly this binpack, with everything inherited or defaulted written out
explicitly. So it round-trips: feeding the output back through `-f` is valid, and resolves to
the same settings.

```json
{
  "apiVersion": "binpack.motleyhand.com/v1alpha1",
  "kind": "BinpackConfig",
  "interval": "1m0s",
  "dryRun": true,
  "discovery": {"nodeGroupIDLabel": "…", "poolNameLabel": "…", "autoscalerNamespace": "…",
                "autoscalerStatusName": "…", "nodeGroups": [{"labelValue": "…", "group": "…"}]},
  "policy": {"enabled": true, "feasibility": {…}, "autoscaler": {…}, "drain": {…},
             "backoff": {…}, "cooldown": {…}, "exclusions": {…}},
  "pools": [{"name": "pool-4g", "enabled": true, "feasibility": {…}, "…": {}}]
}
```

Every key is a configuration field — including no disclosure line, which would make the report
unloadable — and is documented in
[configuration.md](configuration.md) — `policy` is the resolved default, and each `pools[]` entry
is that pool's policy already resolved against it, inlined exactly as the document inlines it.
`interval` and every other duration is a string such as `"1m0s"`.

Reloading the report pins every default as an explicit setting, which is a different document
from the one you wrote and describes the same binpack. That is the point of it: it is what
binpack will actually do, not what you typed.

## `binpack version`

Prints build information. Takes no flags of its own.

### `version --output json`

| Field | Type | Meaning |
|---|---|---|
| `version` | string | The release, or a `git describe` of the build |
| `commit` | string | The commit it was built from |
| `date` | string | When it was built, RFC 3339 |
| `goVersion` | string | The Go toolchain |
| `platform` | string | `GOOS/GOARCH` |

## See also

- [Configuration](configuration.md) — every field these commands read
- [Diagnostics](diagnostics.md) — every code `diagnose` reports
- [Metrics](metrics.md) — every series `run` publishes
- [Annotations and labels](annotations.md) — what `run` writes on nodes
- [Versioning](versioning.md) — what these promises cover
