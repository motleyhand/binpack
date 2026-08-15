# Why your cluster doesn't shrink

Background reading. If you want the commands, see
[diagnose scale-down blockers](../how-to/diagnose-scale-down-blockers.md).

Most people meet this problem as a billing question. Traffic spiked a fortnight ago, the cluster
grew, traffic receded — and the cluster stayed grown. Nothing is wrong. Nothing is alerting.
The nodes are simply still there, at 50-something percent utilisation, being charged for.

The instinct is that the autoscaler is broken or misconfigured. It usually isn't. It is doing
exactly what it was designed to do, and what it was designed to do is narrower than most people
assume.

## The autoscaler is reactive, not optimising

The cluster-autoscaler never asks the question you want it to ask, which is:

> Could this cluster be rearranged to use fewer nodes?

It asks a much narrower one, once per node:

> Is this node unneeded *right now*, and can everything on it be evicted onto existing nodes
> immediately?

The difference is everything. The first question is an optimisation problem over the whole
cluster. The second is a local test on a placement that already exists. The autoscaler has no
code path that moves a pod in order to make room — it only notices when a node has *already*
become nearly empty by itself, and then removes it.

So it waits for natural churn: a deployment, a restart, a scale event that happens to empty a
node. On a busy cluster that churn arrives regularly. On a stable one it may not arrive for
weeks.

## The scheduler is actively working against you

Kubernetes schedules pods with the `LeastAllocated` strategy by default: among the nodes that
fit, prefer the one with the most free room. This spreads load evenly, which is the right
default — it limits the blast radius of losing any single node.

It is also precisely the opposite of bin-packing.

Every new pod goes to the emptiest node. Over time this produces a cluster where every node
carries a similar fraction of the load, and *no* node is the obvious one to remove. The
autoscaler looks at each in turn, finds each one moderately busy, and does nothing. Forever.

This is the equilibrium that costs money: stable, symmetric, and completely invisible unless you
go looking. Every node at 40–70 percent. None clearly removable. All billed.

You can often demonstrate it by hand, in two steps:

```bash
kubectl cordon <node>                     # stop new pods landing here
kubectl delete pod -n <ns> <pod>          # let its replacement be scheduled elsewhere
```

The cordon is not optional, and the reason is the whole problem in miniature. Delete the pod
without it and the node you just freed space on becomes the *emptiest* node in the cluster — so
`LeastAllocated` is quite likely to schedule the replacement straight back onto it. You get a
pod restart and no progress. Cordon first and the replacement has to go elsewhere; the symmetry
breaks; the node crosses from "moderately busy" to "clearly unneeded"; and the autoscaler
usually reaps it within minutes.

When that works, the capacity was never necessary. The autoscaler simply had no way to find the
opening, because finding it required moving something first.

Those two commands are, in essence, what binpack automates — with the arithmetic to establish
beforehand that the displaced pods have somewhere to go, and the bookkeeping to uncordon the
node if they turn out not to.

## The three conditions, all of which must hold

Before removing a node, the autoscaler requires all of the following simultaneously.

**1. The node has been "unneeded" for long enough.** `scale-down-unneeded-time`, ten minutes by
default.

**2. Requested resources are below a utilisation threshold.**
`scale-down-utilization-threshold`, 0.5 by default, for both CPU and memory.

Note *requested*, not used. This catches people out constantly. If your pods request 1GB and use
200MB, the autoscaler sees 1GB. Requests inflated three to five times above reality — the
default state of anything nobody revisited after the first week — make every node look busy when
none of them are. This is why right-sizing requests is the highest-leverage fix available, and
why it belongs on the [quick wins](../how-to/quick-wins-before-installing-binpack.md) list.

**3. Every pod on the node can be evicted and rescheduled elsewhere.**

This is where things get genuinely stuck. A pod blocks eviction if it:

- has no controller — a bare pod, with nothing to recreate it
- uses `emptyDir` or `hostPath` without
  `cluster-autoscaler.kubernetes.io/safe-to-evict: "true"`
- carries `cluster-autoscaler.kubernetes.io/safe-to-evict: "false"`
- is covered by a PodDisruptionBudget currently permitting zero disruptions
- has a node selector or affinity that only this node satisfies
- runs in `kube-system` without a PDB permitting voluntary disruption

The fourth of those is, in practice, the most common and the most expensive. It has
[its own page](the-poddisruptionbudget-that-costs-money.md).

## The delay that quietly cancels everything

`scale-down-delay-after-add` pauses **all** scale-down, cluster-wide, for ten minutes after
**any** scale-up.

On a cluster with frequent autoscaling activity — event-driven workloads, KEDA, anything bursty
— this alone can suppress scale-down indefinitely. Every time the cluster is close to shrinking,
something scales up and the clock resets. The result looks like a cluster that has simply
decided never to get smaller.

## A ceiling nobody watches: pods per node

The kubelet's default limit is 110 pods per node. It is a hard scheduling constraint, and it is
invisible in every dashboard that shows only CPU and memory.

A node can be completely unschedulable on pod count while showing plenty of free memory. If you
are ever puzzling over why a pod is Pending on a cluster with obvious capacity, count pods
before you hunt anywhere else. The autoscaler does treat pod pressure as a scale-up trigger, so
it self-corrects — but the diagnosis is confusing if the possibility hasn't occurred to you.

## Why you can't just tune your way out

On a self-hosted autoscaler you can adjust every threshold above. Most managed Kubernetes
services expose almost none of them — DigitalOcean, for instance, offers minimum nodes, maximum
nodes and expander choice, and nothing else. The control plane is managed, so the autoscaler's
logs aren't yours to read either.

That is a real frustration, but it is worth being clear that the knobs are not the fundamental
problem.

Note the direction of `scale-down-utilization-threshold`, because it is easy to get backwards. A
node qualifies for removal when its utilisation is **below** the threshold. *Raising* it from
0.5 to 0.7 therefore widens the net and makes the autoscaler consider more nodes; lowering it to
0.3 narrows the net and excludes everything between 30 and 50 percent. If you are hoping for
more scale-down, the dial goes up.

But — and this is the point — **even a perfectly tuned autoscaler never rebalances.** Widening
the net makes it readier to remove a node that is *already* nearly empty. It does not make the
cluster arrive at a state where such a node exists, and it does nothing about condition 3, which
is what actually pins most nodes. If every dial were exposed to you tomorrow, the symmetric
equilibrium would still form, and the extra node would still sit there.

That gap is architectural rather than configurational, which is the reason this project exists.
