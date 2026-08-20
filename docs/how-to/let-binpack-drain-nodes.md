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

And it stops when the autoscaler takes over. Once a node carries
`ToBeDeletedByClusterAutoscaler`, the autoscaler has committed to removing it and is draining it
itself — so binpack stops evicting, leaves the node cordoned, and waits for it to go. Uncordoning
at that point would be two controllers disagreeing about whether a node accepts pods. The softer
`DeletionCandidateOfClusterAutoscaler` is only an opinion that the node is unneeded, and does not
stop a drain.

That wait is bounded, but not by a clock. Only a running cluster-autoscaler ever removes that
taint, so a wait for one that has stopped is a wait for nothing — the node would stay cordoned,
and binpack would keep returning to it instead of considering anywhere else. binpack ends the
wait and uncordons when the autoscaler's own status says it could not finish: either the status
is more than five minutes stale, or the node's pool is no longer one the status lists. Elapsed
time on its own is not a signal, because a node can be under deletion for as long as the
autoscaler's `--max-graceful-termination-sec` allows.

The taint itself is left alone — it is the autoscaler's, and binpack writes no taints. So a node
handed back this way is uncordoned but still repelling pods until an autoscaler returns to clear
it, or you do:

```bash
kubectl taint nodes <node> ToBeDeletedByClusterAutoscaler-
```

## After

binpack writes three events per drain, on the node itself:

| Reason | Meaning |
|---|---|
| `Draining` | A drain has started on this node |
| `Drained` | It finished, and the cluster-autoscaler removed the node |
| `DrainAbandoned` | It stopped, the node is uncordoned, and the note says why |

```bash
kubectl get events -A --sort-by=.lastTimestamp --field-selector reason=DrainAbandoned
```

In dry run the first of these is `WouldDrain` instead, and says so in its note. The reason
differs rather than only the wording, so nothing filtering events can confuse a drain that
happened with one that was merely decided on.

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
```

```bash
kubectl label node NODE binpack.motleyhand.com/draining-
```

```bash
kubectl uncordon NODE
```

The label goes too. binpack never reads it, so nothing clears it for you — and a node still
reporting `draining=true` after the drain has ended makes the label worth nothing.

## What binpack cannot promise

The simulation proves a valid assignment **exists**. It does not oblige the scheduler to choose
that one. If pod A fits either N1 or N2 while pod B fits only N2, the scheduler may put A on N2
and leave B Pending — every check was sound, and the placement was still wrong.

binpack evicts one pod at a time and waits for each replacement to be scheduled before evicting
the next, so a wrong prediction costs one eviction rather than all of them, and shows up as an
abandoned drain naming the pod that could not be placed. It makes a scale-up unlikely and
immediately detectable. It cannot make one impossible.
