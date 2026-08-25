# Annotations and labels

binpack reads one annotation you set, writes seven of its own, plus one label — and on clusters
whose provider labels do not carry the autoscaler's pool identifier, reads a second label you
apply. All are on **nodes**; it annotates and labels no other kind of object.

The keys binpack writes are fixed rather than configurable: one thing to document, one thing to
grep for, and no possibility of two clusters disagreeing about what protects a node. The one it
reads for pool membership is the exception, and has to be — the key that works is whichever one
your provider happens to put the identifier in.

## What you set

### `binpack.motleyhand.com/skip`

Set to `"true"` to exclude a node from consideration.

```bash
kubectl annotate node NODE binpack.motleyhand.com/skip=true
```

Checked on every evaluation, including during a drain — so annotating a node binpack is already
draining stops that drain and hands the node back. Remove it with a trailing hyphen:

```bash
kubectl annotate node NODE binpack.motleyhand.com/skip-
```

### `binpack.motleyhand.com/node-group`

Only where discovery needs it. binpack maps a node to its autoscaling pool through the label
named by `discovery.nodeGroupIDLabel`, and matches on that label's **value**: it has to be the
identifier the cluster-autoscaler publishes in `nodeGroups[].name`, which is its cloud
provider's own name for the group.

Where no label your provider already applies carries that value, apply one — this is the key
binpack suggests, because a key in a provider's own namespace is one the provider may start
writing itself:

```bash
kubectl label nodes <node>... binpack.motleyhand.com/node-group=<group>
```

and set `discovery.nodeGroupIDLabel: binpack.motleyhand.com/node-group`. binpack never writes
this label; it only reads whichever key the setting names. Where nothing maps, it refuses to
run rather than reporting every node as unmanaged — see
[`discovery.nodeGroupIDLabel`](configuration.md#discoverynodegroupidlabel) and
[ADR-0012](../design/adr-0012-pool-mapping-needs-a-value-matching-node-label.md).

## What binpack writes

You do not need to set these, and setting them by hand is a way to confuse a running drain
rather than to control one. They are documented because they are what `kubectl describe node`
shows you, and because a drain that has gone wrong is explained by them.

### While a drain is in progress

| Annotation | Meaning |
|---|---|
| `drain-started` | When this drain began, RFC 3339 |
| `drain-progress` | When binpack last observed the drain moving |
| `drain-pods-remaining` | How many pods were still to leave when it last saw the drain move |
| `drain-awaiting` | The controller owing a replacement pod, as `<owner UID>@<RFC 3339>`, or `settled` when none is owed |

`drain-started` is also the marker: while it is present, binpack advances *this* drain and makes
no new decision anywhere in the cluster. One node at a time. It also fixes the set of pods the
drain will move — a pod created after it arrived on a node that was already cordoned, so it
tolerates the cordon, and evicting it would put it straight back.

`drain-progress` is what the stall bound measures against, and it moves only when the node's
state changed — a pod left, or a pod is shutting down inside its grace period. Accepting an
eviction is not itself progress: it is something binpack did rather than something that happened
to the node, and a workload tolerating the cordon can be evicted and rescheduled straight back
onto the node it was evicted from. A drain with a long `terminationGracePeriodSeconds` can sit
here for a long time and be perfectly healthy; see
[ADR-0007](../design/adr-0007-drain-progress-not-deadlines.md).

`drain-pods-remaining` is the baseline that comparison is made against, so it appears as soon as
binpack looks at a drain and then follows the progress marker. A later count being lower than it
is what tells binpack a pod left while it was not watching.

`drain-awaiting` is why evictions are sequential. binpack waits for the replacement pod to be
*bound to a node*, not merely created, before evicting the next one.

It carries `settled` when no replacement is owed, which happens three ways: the replacement has
landed, the pod that left was
[expendable](../explanation/overprovisioning-and-expendable-pods.md) and binpack reserved it no
destination in the first place, or an eviction is about to be attempted. binpack writes the marker
immediately *before* it evicts, because a marker written afterwards is lost by the failures it
exists to survive — a refused patch, a lost lease, a restart — while the eviction is not. So a
node whose eviction was then refused by a disruption budget carries `settled` having disrupted
nothing; the annotation records what binpack has committed to, not what the cluster has done.

The distinction between `settled` and the annotation being absent
is load-bearing rather than cosmetic — absent means this drain has evicted nothing at all, and
binpack asks a drain that has moved pods a narrower set of questions than one that has not
([ADR-0009](../design/adr-0009-revalidation-asks-soundness-not-preference.md),
[ADR-0010](../design/adr-0010-a-scale-up-stops-a-drain-that-has-not-started.md)). If you are
clearing these by hand, clear them all together — the command under [If a node is stuck
cordoned](#if-a-node-is-stuck-cordoned) does.

### After a drain has failed

| Annotation | Meaning |
|---|---|
| `drain-attempts` | Consecutive failed drains of this node |
| `backoff-until` | Not a candidate until this time, RFC 3339 |
| `last-failure` | Why the last attempt was abandoned, in a sentence |

Backoff starts at 30 minutes and doubles to a 24-hour cap. It is never permanent: a node blocked
by something transient recovers on its own.

This exists because abandoning a drain uncordons the node — and a partially drained node has
*fewer* pods than before, so the least-loaded-first ordering makes it **more** attractive than
it was. Without backoff, binpack would preferentially retry its own failures.

A successful drain removes the node, so these clean themselves up.

## Reading them

```bash
kubectl get nodes -o custom-columns=\
NAME:.metadata.name,\
DRAINING:.metadata.annotations.binpack\.motleyhand\.com/drain-started,\
BACKOFF:.metadata.annotations.binpack\.motleyhand\.com/backoff-until
```

`binpack diagnose` reports the same state in prose, and `binpack explain` gives the reason a
node in backoff was skipped.

## If a node is stuck cordoned

Every branch of a drain ends with the node either removed or uncordoned, so this should not
happen. If it does — binpack was killed at exactly the wrong moment, or its RBAC was removed
mid-drain — the repair is to uncordon and then clear the markers, in that order:

```bash
kubectl uncordon NODE
```

```bash
kubectl annotate node NODE binpack.motleyhand.com/drain-started- binpack.motleyhand.com/drain-progress- binpack.motleyhand.com/drain-pods-remaining- binpack.motleyhand.com/drain-awaiting-
```

```bash
kubectl label node NODE binpack.motleyhand.com/draining-
```

**Uncordon first**, and the reason is what each order leaves behind if you stop partway. binpack
itself has a third option — it hands a node back in a single merge patch, so there is no partway
— but three `kubectl` commands are three chances to be interrupted by a dead shell, a denied
patch or a runbook that stops.

Clearing the markers first leaves a node that is cordoned and carries nothing. binpack skips it
as `cordoned`, the same bucket as a node you cordoned yourself, and will not touch it again; the
capacity stays paid for and nothing left on the node says why. That state has no repair that
anything will start on its own.

Uncordoning first leaves the opposite half-state, and that one is recoverable either way. A node
carrying `drain-started` that is *not* cordoned is one where a write did not land, so a running
binpack cordons it on the next evaluation and re-checks rather than acting on a stale view — and
if binpack is not running, the node is back in service, which is where you wanted it.

The label goes too. Nothing clears it for you once binpack has stopped looking, and a node still
reporting `draining=true` after the drain has ended makes the label worth nothing. Its one reader
inside binpack is `binpack diagnose`, which reports a cordoned node carrying the label but no
markers as an abandoned drain — that is the half-cleaned state above, and naming it is the only
thing standing between it and being forgotten.

## The label

### `binpack.motleyhand.com/draining`

Set to `"true"` on a node binpack is currently draining, and removed when that drain ends —
in the same write as the markers, so a node is never labelled without them or the reverse.

```bash
kubectl get nodes -L binpack.motleyhand.com/draining
```

It exists because `kubectl get nodes` reports a cordoned node as `SchedulingDisabled` and says
nothing about who cordoned it: binpack, a person, or something else. The annotations already
answer that, but only under `kubectl describe`.

A label rather than a taint, though a taint is what the cluster-autoscaler uses for its own
equivalent markers. Taints are an array, so setting one means replacing the whole list — on a
field the cluster-autoscaler is editing on these same nodes during a scale-down. Every other
write binpack makes is a merge patch built from a bare object precisely so it can never clobber
a concurrent change, and a label keeps that property because it is a map key.

The label is a signal, not state: the annotations are what a drain is recovered from, and
removing the label by hand changes nothing about a drain in flight. Its one reader is
`binpack diagnose`, above — a diagnostic rather than a decision, which is why removing it costs
you the report and not the drain.

## See also

- [Metrics](metrics.md) — `binpack_nodes_in_backoff` and `binpack_drains_abandoned_total`
- [Configuration](configuration.md) — `stallTimeout` and `removalTimeout`
- [ADR-0007](../design/adr-0007-drain-progress-not-deadlines.md) — why progress, not deadlines
