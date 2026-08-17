# Annotations

binpack reads one annotation you set, and writes six of its own. All are on **nodes**; it
annotates no other kind of object.

The keys are fixed rather than configurable: one thing to document, one thing to grep for, and
no possibility of two clusters disagreeing about what protects a node.

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

## What binpack writes

You do not need to set these, and setting them by hand is a way to confuse a running drain
rather than to control one. They are documented because they are what `kubectl describe node`
shows you, and because a drain that has gone wrong is explained by them.

### While a drain is in progress

| Annotation | Meaning |
|---|---|
| `drain-started` | When this drain began, RFC 3339 |
| `drain-progress` | When binpack last observed the drain moving |
| `drain-pods-remaining` | How many pods were still to leave when it last looked |
| `drain-awaiting` | The controller owing a replacement pod, as `<owner UID>@<RFC 3339>` |

`drain-started` is also the marker: while it is present, binpack advances *this* drain and makes
no new decision anywhere in the cluster. One node at a time.

`drain-progress` is what the stall bound measures against, and it moves only when something
actually happened — a pod left, a pod is shutting down inside its grace period, or an eviction
was accepted. A drain with a long `terminationGracePeriodSeconds` can sit here for a long time
and be perfectly healthy; see
[ADR-0007](../design/adr-0007-drain-progress-not-deadlines.md).

`drain-awaiting` is why evictions are sequential. binpack waits for the replacement pod to be
*bound to a node*, not merely created, before evicting the next one.

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
mid-drain — the repair is to clear the markers and uncordon:

```bash
kubectl annotate node NODE binpack.motleyhand.com/drain-started- binpack.motleyhand.com/drain-progress- binpack.motleyhand.com/drain-pods-remaining- binpack.motleyhand.com/drain-awaiting-
kubectl uncordon NODE
```

binpack itself repairs the commoner half of this: a node carrying `drain-started` that is *not*
cordoned is one where the process stopped between the two writes, and the next evaluation
cordons it and re-checks rather than acting on a stale view.

## See also

- [Metrics](metrics.md) — `binpack_nodes_in_backoff` and `binpack_drains_abandoned_total`
- [Configuration](configuration.md) — `stallTimeout` and `removalTimeout`
- [ADR-0007](../design/adr-0007-drain-progress-not-deadlines.md) — why progress, not deadlines
