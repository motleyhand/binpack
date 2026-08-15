# Install binpack

> **Status: pre-release.** binpack decides and reports; it does not yet act. The chart defaults
> to dry run and grants no permission that could change anything, which is the state to install
> and leave running for a while.

## Before installing anything

Two of binpack's three commands need nothing installed in your cluster. Run them first — they
work against your own kubeconfig with read-only access, and they are how you decide whether to
trust the thing.

```bash
binpack diagnose
```

That reports what is currently stopping nodes being removed, and most of it blocks the
cluster-autoscaler just as thoroughly. If it finds a disruption budget permitting zero
disruptions, fix that before installing anything: it pins a node permanently and no consolidation
tool can move it. See the [diagnostics reference](../reference/diagnostics.md) and
[quick wins](quick-wins-before-installing-binpack.md).

```bash
binpack explain
```

That prints the decision binpack would reach and the arithmetic behind it, for every node. If it
says nothing is drainable, installing the controller will not change that — it runs the same
function.

## Install

```bash
helm install binpack oci://ghcr.io/motleyhand/charts/binpack \
  --namespace binpack-system --create-namespace
```

The defaults are the ones to start with:

| Default | Why |
|---|---|
| `config.dryRun: true` | Decides everything, changes nothing. Acting should be something you switched on |
| `rbac.allowDraining: false` | The cordon and eviction verbs are not in the role at all |
| `leaderElection.enabled: true` | A rolling update briefly runs two pods, and two binpacks acting at once is the failure the lease prevents |
| `replicaCount: 1` | Only the leader acts, so a second replica is a warm spare rather than more throughput |

## Watch what it decides

Decisions land as Events on the node, which needs no access to binpack's logs:

```bash
kubectl describe node <node> | sed -n '/^Events:/,$p'
```

```
Normal  WouldDrain  4m  binpack  binpack would drain this node: all 2 of its
                                 relocatable pods fit elsewhere. No action taken — dry run
```

A standing decision aggregates into one Event with a count rather than one per minute, so this
stays readable over days.

The logs carry the same decision plus the cluster-wide reason when there is nothing to do:

```bash
kubectl -n binpack-system logs -l app.kubernetes.io/name=binpack -f
```

## Metrics

The endpoint is served by default; discovery is opt-in, because a chart that creates a monitor
your Prometheus does not select has done nothing while looking like it worked.

Check which selector your Prometheus uses:

```bash
kubectl get prometheus -A -o jsonpath='{range .items[*]}{.spec.podMonitorSelector}{"\n"}{end}'
```

Then set the matching label:

```yaml
metrics:
  podMonitor:
    enabled: true
    labels:
      release: prometheus   # whatever your selector above requires
```

A `ServiceMonitor` (and the `Service` it needs) is available instead, for a Prometheus configured
to discover only those. See the [metrics reference](../reference/metrics.md) for what to alert
on — `binpack_last_evaluation_timestamp_seconds` going stale is the one that catches a binpack
that has quietly stopped.

**A replica that is not the leader publishes no `binpack_` series at all.** That is deliberate:
a standby full of zeroes is indistinguishable from a cluster in trouble. A target reporting
nothing is a standby, not a fault.

## Sizing

The defaults are measured rather than guessed: against a 4-node, 145-pod cluster binpack settles
at 43 MiB resident and spends about half a second per evaluation.

Memory scales with the number of objects cached — every pod, node and disruption budget, plus
each controller's pod template — not with load. On a large cluster raise the **limit** rather
than the request: the initial LIST of every pod is the peak, not the steady state.

There is no CPU limit by default, and adding one is a poor trade: throttling a controller that
wakes once a minute buys nothing and turns a slow evaluation into a stalled one.

## Configuration

Everything under `config` is rendered into a ConfigMap verbatim. Every field is optional, because
pools and their bounds are discovered from the cluster-autoscaler rather than declared.

Check a document before applying it:

```bash
binpack config validate -f my-values-fragment.yaml
```

Editing the ConfigMap restarts the deployment — binpack reads its configuration once, at startup,
and the chart hashes it into a pod annotation so the change takes effect immediately rather than
whenever the pod next happens to restart.

See the [configuration reference](../reference/configuration.md).

## What it is not granted

No cloud provider credentials, on any platform. No `pods: delete`. No write access to workloads —
`diagnose` suggests fixes and never applies them. No Secrets. No ConfigMaps outside `kube-system`,
where it reads exactly one.

The [RBAC reference](../reference/rbac.md) lists every permission and what each is for.

## Uninstall

```bash
helm uninstall binpack --namespace binpack-system
```

binpack holds no state of its own beyond annotations on nodes it has drained, and this build
never writes those. If a future version leaves a node cordoned with a
`binpack.motleyhand.com/drain-started` annotation, `binpack diagnose` reports it as an abandoned
drain and tells you what to clear.
