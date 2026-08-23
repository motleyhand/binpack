# Overprovisioning, pause pods, and the expendable-priority cutoff

If you run warm capacity — low-priority placeholder pods that reserve room so real workloads
start instantly — there is a narrow band of correct configuration and two ways to fall out of it,
both silent.

## The pattern

Low-priority `pause` pods occupy capacity doing nothing. When real work arrives, the scheduler
preempts them, and the real pods take their place immediately rather than waiting for a node to
be provisioned. The evicted pause pods go Pending, the autoscaler notices unschedulable pods and
adds a node, and the buffer is restored ready for next time.

It is a good pattern. It trades money for latency, deliberately and measurably.

## The cutoff that governs it

The cluster-autoscaler has a flag, `--expendable-pods-priority-cutoff`, defaulting to **-10**.
Pods with priority **below** that value are treated as beneath its notice, in two ways at once:

- they do **not** trigger scale-ups — no node will be added in order to run them
- they do **not** prevent scale-downs — a node running only such pods can be removed freely

Both halves matter, and for warm capacity they pull in opposite directions.

## Too low, and the pattern breaks silently

The intuition is that pause pods are worthless filler, so they should be as expendable as
possible. Setting their priority below -10 acts on that intuition, and breaks the pattern.

Below the cutoff, the autoscaler ignores their Pending state. When the buffer is consumed, no
node is added to replenish it. The pause pods sit Pending forever, the warm capacity is gone,
and the next burst waits for a cold node exactly as if you had never configured any of this.

Nothing alerts. The pods are Pending, which looks like the pattern working. The failure is only
visible if you check that the buffer actually comes back after it has been used once.

**Pause pods must therefore sit at or above -10.**

## Which means they are real workload, and that is the price

Sitting above the cutoff, the autoscaler counts them as ordinary pods when computing node
utilisation. A node whose only tenants are pause pods looks occupied for scale-down purposes.

This is not a bug and there is no configuration that avoids it. It is what you bought: you are
paying to hold capacity warm, and the accounting reflects that.

## Why binpack does not treat them as expendable either

It is tempting for a consolidation tool to be clever here — to notice that these pods exist
purely to be preempted and exclude them from the "must fit elsewhere" arithmetic. binpack
deliberately does not.

The reason is mechanical. Pause pods sit above the cutoff by necessity, so they behave exactly
like real workload: evict them and they go Pending, and the autoscaler responds by adding a
node. If binpack excluded them from its feasibility check, every drain of a node holding buffer
would trigger the very scale-up the tool exists to prevent — and, worse, would report it as a
success.

There is a second reason, less mechanical and more important. A warm-capacity buffer is capacity
you are deliberately paying to keep free. Consolidating it away is not a saving; it is silently
shrinking a buffer somebody sized on purpose, and converting a latency guarantee into a cost
reduction that was never asked for. If the buffer is too big, that is a decision for its owner
to revisit.

So binpack mirrors the autoscaler's rule exactly and adds nothing to it: only pods strictly
below the cutoff are excluded from the feasibility maths.

## Correct configurations

Two requirements, and any configuration satisfying both is fine:

1. Overprovisioning priority strictly below every real workload, so pause pods are preempted
   first.
2. At or above -10, so the autoscaler still replenishes the buffer.

The cluster-autoscaler FAQ's own example puts overprovisioning at `value: -10` with
`globalDefault: false`, relying on unclassed pods defaulting to `0`. That is not an arbitrary
number: it is exactly `--expendable-pods-priority-cutoff`, so it is the lowest value that still
replenishes the buffer. The FAQ says as much beside the manifest — "Priority -10 will be
reserved for overprovisioning pods as it is the lowest priority that triggers scaling clusters."
A default class at `1` with overprovisioning at `0` works equally well.

One trap worth spelling out, because the class involved looks redundant and is not.

If you run a `default` PriorityClass with `globalDefault: true` and value `1`, alongside
overprovisioning at `0`, that default class is **load-bearing**. It is what puts ordinary pods
at priority 1, one step above the pause pods, which is what makes preemption work.

Delete it as tidy-up — it looks like it does nothing, since Kubernetes assigns `0` to unclassed
pods anyway — and every ordinary pod drops to `0`, level with the pause pods. Preemption stops:
the scheduler has no reason to prefer evicting the buffer, and the warm-capacity pattern quietly
becomes a pile of pods occupying space for no benefit.

If you want numeric headroom for higher-priority classes, raise the default's value (some teams
use `100`) rather than removing it. And whatever the values, leave a comment saying which class
is meant to be preempted first, because the relationship between the numbers is the entire
configuration and nothing in the manifests explains it.

## Checking yours

The failure mode to test for is the silent one. Consume the buffer, then confirm it comes back:
watch whether the displaced pause pods are still Pending several minutes later. If they are, and
no node was added, their priority is below the cutoff.

`binpack diagnose` reports this directly — pause pods below the expendable cutoff are flagged as
a broken overprovisioning configuration rather than as an ordinary scheduling failure.
