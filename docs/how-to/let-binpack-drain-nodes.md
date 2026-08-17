# Let binpack drain nodes

binpack ships in dry run: it decides everything and changes nothing. This is how to turn that
off, and what to look at before you do.

The decisions are identical either way. Dry run is not a reduced mode — it runs the whole
procedure and stops at the point of acting, so what you see in dry run is what it will do.

## Before

### Read a week of its decisions

```bash
kubectl get events --field-selector reason=WouldDrain -A --sort-by=.lastTimestamp
```

Each one names a node and says why it was chosen. If binpack has never chosen a node, turning it
on changes nothing, and [`binpack diagnose`](../reference/diagnostics.md) will tell you what is
in the way.

### Check what it is refusing, and why

```bash
binpack explain
```

`infeasible` means the workload genuinely does not fit elsewhere — that is a capacity answer and
draining would be wrong. `blocked` means a PodDisruptionBudget would not allow the eviction, and
is worth understanding before binpack starts requesting them.

Pay particular attention to nodes reported as **unmodelled**. binpack refuses those rather than
guessing, but they are also the places where your cluster does something binpack does not
understand, and they are worth reading before it starts acting anywhere.

### Decide what you do not want touched

```bash
kubectl annotate node NODE binpack.motleyhand.com/skip=true
```

Per pool, `policy.byPool.<name>.enabled: false` is the broader instrument. See
[configuration](../reference/configuration.md).

## Turning it on

Two values, and the chart refuses one without the other:

```yaml
config:
  dryRun: false

rbac:
  allowDraining: true
```

Setting `dryRun: false` alone fails at `helm install` rather than at runtime. That combination
would otherwise install cleanly and then be refused by RBAC on every eviction — a binpack that
holds its lease, serves its metrics, and does nothing.

## What it can now do

Four changes, and no others:

| Change | Why |
|---|---|
| Cordon a node | Stop its pod set growing while the drain runs |
| Annotate a node | The drain's recovery state, which must outlive the process |
| Uncordon a node | Every drain ends here, or with the node gone |
| Evict a pod | Through the eviction API, so disruption budgets are respected |

It deletes nothing. Pods leave because an eviction was accepted; the emptied node is removed by
the cluster-autoscaler, which is the component whose job that is.

## After

```bash
kubectl get events --field-selector reason=Draining -A --sort-by=.lastTimestamp
```

The metrics worth watching are `binpack_drains_completed_total` against
`binpack_drains_abandoned_total`, and `binpack_drain_attempts_max`. A cluster where abandonments
outnumber completions is one where binpack keeps starting work it cannot finish; the `reason`
label says which kind, and [the metrics reference](../reference/metrics.md) says what each means.

`kubectl describe node` shows the drain annotations on a node currently being drained — see
[annotations](../reference/annotations.md).

## Turning it back off

```yaml
config:
  dryRun: true
```

A drain already in progress is left exactly as it is: still cordoned, still marked, not advanced.
binpack has been told to change nothing, and uncordoning would itself be a change. Finish it by
hand, or set `dryRun: false` again and let binpack finish it.

To hand a node back yourself:

```bash
kubectl annotate node NODE binpack.motleyhand.com/drain-started- binpack.motleyhand.com/drain-progress- binpack.motleyhand.com/drain-pods-remaining- binpack.motleyhand.com/drain-awaiting-
kubectl uncordon NODE
```

## What binpack cannot promise

The simulation proves a valid assignment **exists**. It does not oblige the scheduler to choose
that one. If pod A fits either N1 or N2 while pod B fits only N2, the scheduler may put A on N2
and leave B Pending — every check was sound, and the placement was still wrong.

binpack evicts one pod at a time and waits for each replacement to be scheduled before evicting
the next, so a wrong prediction costs one eviction rather than all of them, and shows up as an
abandoned drain naming the pod that could not be placed. It makes a scale-up unlikely and
immediately detectable. It cannot make one impossible.
