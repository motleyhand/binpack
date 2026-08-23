# Install binpack

> **Start in dry run.** The chart defaults to deciding without acting, and to a role holding no
> permission that could change anything — which is the state to install and leave running for a
> while. [Let binpack drain nodes](let-binpack-drain-nodes.md) is the step after it.

## What binpack needs

One thing, and it is not optional: a cluster-autoscaler publishing a
`cluster-autoscaler-status` ConfigMap. binpack drains a node, and the autoscaler is the
component that then removes it — without one, a drained node would stay cordoned and empty
indefinitely, so binpack refuses to act at all rather than produce that.

The autoscaler writes that object into the namespace it runs in, which is whatever its own
`--namespace` flag says and therefore whatever namespace it was installed into. Find yours:

```bash
kubectl get configmap cluster-autoscaler-status -A
```

If the namespace it prints is not `kube-system`, set `config.discovery.autoscalerNamespace` to
it at install time — binpack reads that namespace and no other, and the chart puts binpack's
Role for reading it there too.

Managed clusters publish it once a pool has autoscaling switched on. `kind`, `minikube`, `k3d`
and `kubeadm` do not run a cluster-autoscaler at all, so binpack has nothing to offer them: it
will still read the cluster and describe every node, and it will say it would not act.

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

The chart and the image it names are released together and carry the same version, so there is
no compatibility matrix — see [versioning](../reference/versioning.md).

To run an unreleased build instead, install the chart from a checkout and point it at an image
you have pushed somewhere the cluster can pull from:

```bash
helm install binpack ./charts/binpack \
  --namespace binpack-system --create-namespace \
  --set image.repository=<your-registry>/binpack \
  --set image.tag=dev
```

On a local cluster `kind load docker-image` and `minikube image load` both put an image where the
kubelet will find it, with `image.pullPolicy=IfNotPresent`.

**None of this is necessary to evaluate binpack.** `binpack diagnose` and `binpack explain` run
against your own kubeconfig from the [release binaries](https://github.com/motleyhand/binpack/releases),
need no in-cluster identity, and are the recommended way to decide whether the controller is
worth installing at all.

The chart's defaults are the ones to start with:

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

An evaluation that chose no node says so as well, on every node it looked at — so whichever node
you describe, there is an answer:

```
Normal  NoneFeasible  2m  binpack  binpack evaluated the cluster and chose no node to drain: nodes
                                   were simulated and none could be emptied onto the rest of the
                                   cluster. This is the cluster's answer, written on every node
                                   binpack looked at; `binpack explain` gives this node's own reason
```

The reason names which wall it hit — `NoneFeasible` when nodes were simulated and none could be
emptied, `NoCandidates` when every node was ruled out before it got that far — matching the codes
in `binpack_evaluations_total`. The note is the cluster's answer rather than the node's, and it is
the same sentence on each node. For why *this* node was not chosen, `binpack explain` prints the
arithmetic per node.

The logs carry the same decisions:

```bash
kubectl -n binpack-system logs -l app.kubernetes.io/name=binpack -f
```

To ask the running binpack rather than a fresh one, run `explain` inside the pod. It reads the
mounted ConfigMap, so it answers about the configuration this install is actually using:

```bash
kubectl -n binpack-system exec deploy/binpack -- binpack explain
```

The same command on your laptop, with no `-f`, answers about built-in defaults instead — every
command reports which configuration it read, and the [configuration
reference](../reference/configuration.md) explains that line.

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
each controller's pod template — not with load.

The cache is not the whole of it, and the initial LIST is not the peak. Every evaluation asks the
cache for a *fresh copy* of everything it reads: controller-runtime deep-copies each object out
of the informer on the way out, binpack issues seven list calls per evaluation (nodes, pods,
disruption budgets, and the four controller kinds), and it holds the result for the length of the
decision. So the steady state includes a recurring transient roughly the size of the cached set
itself, and lowering `interval` raises how often it is allocated in direct proportion.

On a large cluster, raise the **limit** rather than the request, and set it from what the process
actually does rather than from the object count. Where the Metrics API is working,

```bash
kubectl -n binpack-system top pod
```

sampled across a few evaluations answers it directly, and is worth more than the arithmetic.

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
`diagnose` suggests fixes and never applies them. No Secrets. No ConfigMaps outside the one
namespace `discovery.autoscalerNamespace` names, where it reads exactly one object.

The [RBAC reference](../reference/rbac.md) lists every permission and what each is for.

## Uninstall

binpack's only state outside the release is on the nodes it has drained: the
`binpack.motleyhand.com/draining` label and the drain annotations, written when a drain starts
and cleared when it ends. `helm uninstall` does not touch them, and a drain ends only when
binpack next looks at the node — which is exactly what uninstalling stops.

Asking whether a drain is in flight is only worth doing once nothing can start one. A binpack
that is still running decides again every interval, so a check that comes back empty says nothing
about the moment a second later when `helm uninstall` removes the pod. Stop it first:

```bash
kubectl -n binpack-system scale deployment/binpack --replicas=0
```

```bash
kubectl get nodes -l binpack.motleyhand.com/draining=true
```

```bash
helm uninstall binpack --namespace binpack-system
```

Scaling to zero freezes a drain that is already under way rather than ending it — the node stays
cordoned and marked, because releasing it is itself an action and there is nothing left running to
take it. So if the middle command lists a node, choose before you uninstall. Scaling back up hands
the drain to a binpack that will finish it or abandon it and uncordon; leaving it at zero means
repairing by hand. The three commands for that — **uncordon, then clear the annotations, then
remove the label** — are in the
[annotations reference](../reference/annotations.md#if-a-node-is-stuck-cordoned), with the reason
that order is not arbitrary. `binpack diagnose` reports the same node as an abandoned drain and
prints what to clear; it runs against your own kubeconfig, so it still answers with nothing
installed in the cluster.

Reinstalling also resolves it, if the reinstall can act. Recovery asks the cluster rather than
its own memory — a pod still terminating within its grace period says the drain is alive, and a
node holding fewer pods than the marker records says it moved while binpack was away — so a
binpack with `dryRun: false` resumes a drain that is still going and uncordons the node when it
is not. A binpack in dry run leaves the node exactly as it is and says so in its logs, because
uncordoning would itself be a change.
