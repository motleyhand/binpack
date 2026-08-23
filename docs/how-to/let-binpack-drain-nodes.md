# Let binpack drain nodes

binpack ships in dry run: it decides everything and changes nothing. This is how to turn that
off, and what to look at before you do.

Each evaluation reaches the identical decision either way. Dry run is not a reduced mode — it
runs the whole procedure and stops at the point of acting, so the node it names is the node it
would drain, for the reason it gives.

What it cannot show you is what follows. In this mode nothing is drained, so the cluster never
consolidates: binpack goes on choosing a *first* node every interval, and the controls that
govern a second one — `cooldown.afterDrain`, per-node backoff, and the rule that only one drain
runs at a time — are unreachable, because each of them measures from a drain that never happened.
Read a week of dry run as a blast radius, not as a plan.

## Before

### Read a week of its decisions

```bash
kubectl get events --field-selector reason=WouldDrain -A --sort-by=.lastTimestamp
```

Each one names a node and says why it was chosen. Repeats of an unchanged decision aggregate into
one Event carrying a count, so a week is usually a handful of rows rather than a list — and each
row is a first choice, repeated, rather than a step in a sequence. If binpack has never chosen a
node, turning it on changes nothing, and [`binpack diagnose`](../reference/diagnostics.md) will
tell you what is in the way.

### Check what it is refusing, and why

```bash
binpack explain
```

`infeasible` means the workload genuinely does not fit elsewhere — that is a capacity answer and
draining would be wrong. `blocked` means a PodDisruptionBudget would not allow the eviction, and
is worth understanding before binpack starts requesting them.

Pay particular attention to the refusals binpack marks **unmodelled**, and to any detail ending
`which binpack does not model`. Both mean the same thing — binpack declined to guess rather than
finding a wall in your cluster — and they are the places where your cluster does something binpack
does not understand, so they are worth reading before it starts acting anywhere. The first is
counted by `binpack_nodes_unmodelled`, and [`binpack diagnose`](../reference/diagnostics.md)
reports it per workload under `unreadable-template` — binpack found no template to read — or
`template-divergence`, where it read one and the running pods disagree with it. The metric's
`cause` label carries the same distinction, which is worth keeping: the first widens on a bug
report, and the second is answered in your own cluster.

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
| `WouldAdvanceDrain` | A drain is in progress and dry run is on, so binpack is not advancing it |

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

Unlike a drain binpack is advancing, this one has no end of its own — nothing clears the markers
while dry run is on — so binpack writes a `WouldAdvanceDrain` event on that node every
evaluation, saying what advancing it would do:

```bash
kubectl get events -A --field-selector reason=WouldAdvanceDrain
```

Every other node goes on being evaluated and reported on as usual.

To hand a node back yourself, uncordon it first and clear the markers afterwards:

```bash
kubectl uncordon NODE
```

```bash
kubectl annotate node NODE binpack.motleyhand.com/drain-started- binpack.motleyhand.com/drain-progress- binpack.motleyhand.com/drain-pods-remaining- binpack.motleyhand.com/drain-awaiting-
```

```bash
kubectl label node NODE binpack.motleyhand.com/draining-
```

The order is the point, because three commands are three chances to be interrupted. Clearing the
markers while the node is still cordoned leaves it reading as a cordon somebody else applied:
binpack skips it as `cordoned` and will not touch it again, so if the uncordon never happens —
a denied patch, a runbook that stopped, a shell that died — the node stays out of service with
nothing on it to say why. Doing the uncordon first means every state you can stop in has the
node back in service, which is what you came here for.

While dry run is on, that is where it stays: binpack changes nothing, so a schedulable node with
markers on it is left alone rather than cordoned and resumed. Set `dryRun: false` before you
start if what you want is for binpack to finish the drain instead.

The label goes too, last. Nothing clears it for you, and a node still reporting `draining=true`
after the drain has ended makes the label worth nothing. It is also what `binpack diagnose` reads
to report a node that is *still cordoned* and has lost its markers — the residue of the other
order — so leaving it on until the end is what keeps a half-finished hand-back visible.

## What binpack cannot promise

The simulation proves a valid assignment **exists**. It does not oblige the scheduler to choose
that one. If pod A fits either N1 or N2 while pod B fits only N2, the scheduler may put A on N2
and leave B Pending — every check was sound, and the placement was still wrong.

binpack evicts one pod at a time and waits for each replacement to be scheduled before evicting
the next, so a wrong prediction costs one eviction rather than all of them, and shows up as an
abandoned drain naming the pod that could not be placed. It makes a scale-up unlikely and
immediately detectable. It cannot make one impossible.

### Batch work

binpack's guard against destroying a pod is that something will recreate it: a pod with a
controlling owner is safe to move, one without is refused outright. A Job pod passes that guard
and is relocated like any other — but the Job controller is the one kind that stops recreating.

An eviction adds the `DisruptionTarget` condition to the pod, and the Job controller counts a
deleted pod as a failure. Once `.status.failed` passes `.spec.backoffLimit` the Job is terminally
`Failed` and starts nothing, so the work is lost rather than moved. `backoffLimit: 0` is the
ordinary setting for a database migration, a Helm `pre-upgrade` hook or a one-shot CI Job. The
commoner and milder version is a Job with budget left: the work is redone from the beginning.

binpack cannot see any of this. It reads a Job's pod template to predict what a replacement would
request, and not its status, so it has no way to know how much failure budget is left. Nor is
this worse than what a node upgrade or a spot reclaim does to the same Job. It is a cost the
simulation does not weigh, which makes it yours to weigh.

The upstream remedy makes the Job drain-safe against every disruption, not only binpack's:

```yaml
spec:
  podFailurePolicy:
    rules:
      - action: Ignore
        onPodConditions:
          - type: DisruptionTarget
  template:
    spec:
      restartPolicy: Never   # Kubernetes permits podFailurePolicy only with this
```

With that rule the disruption does not count against the Job's backoff budget, so a replacement
pod starts instead of the Job failing.
