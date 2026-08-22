# ADR-0013: Where nothing matches outright, derive the pool mapping from the names the identifiers were built from

- **Status:** accepted. Amends
  [ADR-0012](adr-0012-pool-mapping-needs-a-value-matching-node-label.md) rather than superseding
  it. ADR-0012's requirement stands unchanged — a node must be joinable to the identifier the
  cluster-autoscaler publishes, and pool bounds come from that autoscaler and nowhere else. What
  changes is that binpack may now work the join out for itself instead of only being told it, and
  that an operator may state it directly. ADR-0012's "there is no second mode" is the sentence
  this amends.
- **Date:** 2026-08-22

## Context

ADR-0012 established the join and made its absence loud: `discovery.nodeGroupIDLabel` names a
node label, and a node is in a pool when that label's **value equals** the identifier the
autoscaler published. It also established what that costs, from the provider implementations
rather than by inference — the identifier is `nodeGroup.Id()`, which is a node pool ID on
DigitalOcean, an Auto Scaling group name on AWS, a VM Scale Set name on Azure, and a full
instance-group URL on GCE.

DigitalOcean's `doks.digitalocean.com/node-pool-id` carries that identifier, which is why it is
the default and why DOKS works out of the box. **Nothing on EKS, GKE or AKS does.** The remedy
ADR-0012 gives is real and it works, but it is a `kubectl label nodes` an operator has to run
before binpack does anything at all — on the three platforms most clusters are on. And on GCE it
is not available even in principle: a Kubernetes label value is at most 63 characters and may
contain neither `/` nor `:` (`IsLabelValue`, `k8s.io/apimachinery`), and the identifier is a URL.

### The observation ADR-0012 did not make

The provider does not choose these identifiers arbitrarily. It **generates** them from the pool
name — the name the operator typed, which is exactly what the provider's own node label carries.
So the label's value is a **substring** of the identifier:

| | node label | its value | the identifier the autoscaler publishes |
|---|---|---|---|
| EKS managed node group | `eks.amazonaws.com/nodegroup` | `my-eks-nodegroup` | `eks-my-eks-nodegroup-a8c75f2f-df78-a72f-4063-4b69af3de5b1` |
| eksctl, self-managed | `alpha.eksctl.io/nodegroup-name` | `testing-24` | `eksctl-testing-nodegroup-testing-24-NodeGroup-iOzTWO7QftDh` |
| AKS agent pool | `kubernetes.azure.com/agentpool` | `nodepool1` | `aks-nodepool1-33555069-vmss` |
| GKE node pool | `cloud.google.com/gke-nodepool` | `default-pool` | `…/instanceGroups/test-cluster-default-pool-a0c72690-grp` |

**This rescues GCE, which equality cannot reach by construction.** ADR-0012's argument that no
label value can hold a MIG identifier is correct and stays correct — but the MIG *name* is inside
that URL, and a substring join does not need the whole of it.

Where each row came from, because ADR-0012's warning about confident asides applies to this table
above all — a format that is wrong here does not fail, it silently declines to fire on that
provider, and nobody finds out:

- **eksctl** is the only row observed on a real cluster: an EKS cluster whose nodes eksctl created
  and whose autoscaler discovers Auto Scaling groups by tag, read from its own
  `cluster-autoscaler-status` ConfigMap and `kubectl get nodes --show-labels`. It is also the row
  that motivated all of this.
- **EKS managed node groups** from AWS's own `aws eks describe-nodegroup` reference example, which
  shows `nodegroupName: "my-eks-nodegroup"` and, in the same response,
  `resources.autoScalingGroups[0].name: "eks-my-eks-nodegroup-a8c75f2f-df78-a72f-4063-4b69af3de5b1"`.
  The `eks.amazonaws.com/nodegroup` label carries the node group's name; EKS documents that it
  labels managed nodes under the `eks.amazonaws.com` prefix.
- **AKS** from Microsoft Learn's node-access page, whose worked example lists nodes named
  `aks-nodepool1-33555069-vmss000000`. Those are VM Scale Set *instance* names, so the scale set
  — which is what the Azure provider's `Id()` returns — is `aks-nodepool1-33555069-vmss`, and the
  agent pool name `nodepool1` is inside it. Azure limits a Linux agent pool name to 12 characters
  and a Windows one to 6, so there is no room for the truncation GKE does.
- **GKE** from Google's cluster-autoscaler visibility documentation, whose example event carries
  `"mig": {"name": "test-cluster-default-pool-a0c72690-grp", "nodepool": "default-pool"}`.

**GKE truncates, and the same document proves it.** A second example in it pairs the node pool
`nap-n1-standard-1-1kwag2qv` with the instance group `test-cluster-nap-n1-standard--b4fcc348-grp`
— the pool name cut off mid-word. So the pool name is a substring of the MIG name only while the
cluster and pool names are short enough, and a long one is not derivable at all. That is the
`discovery.nodeGroups` case below, and it is why this ADR does not claim GKE works, only that it
works where the name survives.

### Why ADR-0012 rejected exactly this

ADR-0012 considered "infer the mapping from an existing label by searching for a value that
matches" and rejected it, in one sentence that is the whole of the problem:

> A key discovered by coincidence on one evaluation is a key that stops matching when a pool is
> added, and the failure would be a silent change of scope rather than a refusal.

That objection is correct about a rule that matches keys one at a time. A substring alone is
worse than a coincidence — `linux` and `amd64` sit inside plenty of identifiers, and a rule that
accepted them would put static nodes in a pool and apply that pool's floor to them. **A wrong
mapping is a confidently wrong decision**, which is the failure class `internal/fit` exists to
prevent, and it deserves the same treatment.

## Decision

**Three ways to establish the join, tried in order, and binpack reports which one it used.**

1. **Equality**, as ADR-0012 describes. Unchanged, and tried first, so DigitalOcean and every
   hand-labelled cluster take exactly the path they took before.
2. **A stated join**, `discovery.nodeGroups`, for where neither of the others can work.
3. **Derivation**, only when neither of the above matches a single node.

### The derivation, and what makes it safe

The answer to ADR-0012's objection is not a better substring rule. It is that **nothing resolves
unless the whole cluster resolves**, which converts "a silent change of scope" into a refusal:

1. Take a candidate label key K, and partition the nodes by their value of K.
2. Require the number of partitions to equal the number of published groups with a ready node.
3. Draw an edge from a value to a group when the value is a substring of that group's identifier
   **and** the number of nodes carrying the value is a size that group could have.
4. Require a **perfect matching** over those edges, and require it to be the **only** one.
5. If several keys yield a matching, they must **agree about every node**, or binpack refuses.

Add a pool and the partition count no longer matches, so binpack refuses rather than quietly
managing fewer nodes than it did yesterday — which is precisely the property ADR-0012 asked for.
The derivation is re-run every evaluation and never cached, so there is no state to go stale.

**The refusal is not the end of the road**, because refusing is only acceptable if it hands the
operator something. `engine.ResolvePools` returns the near misses with the reason each was turned
down, and the preflight error prints them under the list ADR-0012 already gave — so a cluster
where the derivation declines gets `alpha.eksctl.io/nodegroup-name, but 3 node(s) carry it while
the pool it names is a pool of 2 node(s)` rather than being left to spot for itself that one
string is inside the other.

### Three refinements, each forced by a case

**A bipartite matching rather than per-value uniqueness.** Two pools named `web` and `web-2` have
identifiers `eks-web-1111-aaaa` and `eks-web-2-2222-bbbb`. `web` is a substring of both, so a rule
demanding that each value name exactly one pool refuses the key outright — and with equal pool
sizes nothing else separates them. A matching resolves it: `web-2` can only go one place, which
forces `web` to the other. Loosening the rule was considered and is not the fix; the constraint is
global, so the algorithm has to be.

**A size window rather than a size.** A cluster mid scale-up has four nodes carrying the label
while the autoscaler still reports three ready, because the fourth has registered and not yet
been counted. `len(partition) == ready` refuses it, which would make binpack flap on every scale
event. The window is `[min(ready, target), max(ready, target)]` — `cloudProviderTarget`, which
`engine.NodeGroup` already parses — and it covers the scale-down direction too, where the target
has dropped and the node is still there. Deliberately **not** `NodeGroup.Size()`: that answers a
different question and answers it conservatively, taking the *smaller* of intent and reality
because either being at the floor means a drained node comes straight back. Using it here
collapses the scale-up half of the window to a point, which the first implementation did and a
test caught.

**A matching that is the only one.** Two pools whose identifiers each contain the other's name —
`eks-alpha-beta-1111` and `eks-beta-alpha-2222`, at equal sizes — admit two perfect matchings, one
of which is wrong. Picking either is a coin toss reported as a fact, and which one you get depends
on iteration order. Uniqueness is checked by removing each matched edge and asking whether another
perfect matching survives; the graph has one vertex per pool, so the cost is nothing. This
refinement is not in the prototype the design came from; it was added here because the sabotage
pass showed the guard was missing rather than because a real cluster hit it.

### Determinism

**Which key binpack reports must not vary between runs**, or an operator diffing two reports sees
a change of scope that never happened. On the real single-pool EKS cluster *two* keys resolve it —
`alpha.eksctl.io/cluster-name` and `alpha.eksctl.io/nodegroup-name` — and with one pool they
agree, so either is correct.

Candidate keys are taken in sorted order and the first is used; the rest are reported as agreeing,
which is what stops the choice reading as arbitrary. Published groups are sorted by identifier for
the same reason, because a refusal naming a pool chosen by iteration order is one nobody can diff
against the last one. `collect.Snapshot` appends nodes in whatever order the API server listed
them and sorts nothing, so the tests permute the fixture rather than re-running one snapshot: a
stability test that re-runs one input tests purity, not determinism over the inputs that vary.

### The stated join

```yaml
discovery:
  nodeGroupIDLabel: eks.amazonaws.com/nodegroup
  nodeGroups:
    - labelValue: workers
      group: eks-workers-a2c1d3e4-1111
```

**Membership only.** Pool bounds still come from the `cluster-autoscaler-status` ConfigMap and
nowhere else. This is ADR-0004's withdrawn step 2 in the one form that is safe: what made that
step drift-prone was `pools[].minSize`, a number stated in binpack's configuration and enforced
by the autoscaler, where the one binpack would act on is the one nobody updates when a pool is
resized in a console. Nothing here restates a bound.

It is **additive**: a value the document does not name still joins by equality, so stating one
pool never takes the others out of scope. And `group` has to name a node group the autoscaler
publishes, checked on the same terms as a `pools` override: an entry that matches nothing is
silently no entry at all, which is worse here than for an override — the nodes it meant to place
stay outside every pool and the derivation may answer instead, so the operator's own statement
about their cluster is the thing that gets ignored. A pool scaled to zero still counts as
published, because refusing over one would take binpack down for a pool that is merely empty. It sits under `discovery` rather than in `pools[]`
because `pools[]` is documented as adjusting policy for pools that are **discovered, never
declared**, and its `name` already means two things — a `poolNameLabel` value or the identifier
itself. Membership is discovery's question, and giving `name` a third meaning would make the
existing "overrides pools that do not exist" check ambiguous about which one it was asking.

### Reporting

A heuristic that decides *scope* has to be visible, so `binpack explain` says how nodes were
matched to pools, names the key, says the join was derived rather than configured, lists what each
value resolved to, and names the other keys that would have produced the same answer. The JSON
report carries the same under `pools`. It says nothing at all where the join is plain equality:
that is what the configuration already says binpack does, and restating it every run is the noise
that would stop the derived case standing out.

## Alternatives considered

**Leave it at ADR-0012 and document the `kubectl label` better.** Rejected. The command is not
hard to run; the problem is that binpack does not work until somebody runs it, on EKS, GKE and
AKS. A consolidation controller whose first act is to refuse is one most people never get past —
and on GCE no amount of documentation produces a label that can hold a URL.

**Match on a prefix rather than a substring.** `eks-<nodegroup>-<uuid>` and
`aks-<pool>-<id>-vmss` both put the pool name after a provider prefix, so `strings.Contains` is
doing no work a smarter rule could not do more tightly. Rejected because the rule would then be a
list of per-provider patterns — exactly the hand-maintained list of guesses about somebody else's
defaults that the differential harness's feature gates are not allowed to be. eksctl's
`eksctl-testing-nodegroup-testing-24-NodeGroup-iOz…` also carries the name twice and in the
middle, so any prefix rule would already need an exception. The bijection is what makes the match
safe; the substring test only has to be permissive enough not to miss.

**Cache the derived key, or write it to a node annotation.** Rejected. A cached mapping is state
that can be stale, and the failure it would produce is a mapping that no longer matches the
cluster — which is worse than re-deriving, because re-deriving refuses when the cluster changes
shape and a cache does not. The derivation is a partition and a matching over a graph with one
vertex per pool; it costs nothing worth saving.

**Prefer the "most specific-looking" key over sorted order.** On the real cluster,
`alpha.eksctl.io/nodegroup-name` is obviously the better answer than
`alpha.eksctl.io/cluster-name`, which happens to work only because the cluster has one pool.
Rejected as a rule: any measure of specificity is a guess about somebody else's naming, and it
would make the reported key depend on how many labels a provider happens to apply. Sorted order is
arbitrary, but it is stated, stable and independent of the cluster — and both keys are reported,
so a reader can see that the choice was between two equivalent answers rather than a judgement.

**Widen the size window with `registered.total`.** The window would be more accurate against a
pool holding a NotReady node, which `ready` does not count. Not done: `collect` does not parse
`total` today, the case only refuses (never accepts wrongly), and adding a third bound without a
case forcing it is how a guard drifts into being decorative. Recorded here so the next person
meeting a false refusal knows where to look.

## Consequences

**binpack works out of the box on EKS, AKS, and GKE where pool names are not truncated**, on
clusters that previously required a `kubectl label nodes` before anything worked at all — and on
GCE, where equality could never have worked.

**A heuristic now decides which nodes binpack manages.** It does not decide feasibility: nothing
about the simulation, the eviction arithmetic or the PDB accounting changes, and a node in the
wrong pool would still have to be shown drainable before anything happened to it. What it decides
is scope, and a wrong answer would apply one pool's floor to another pool's nodes. Hence: it
refuses rather than guessing, it resolves the whole cluster or none of it, and it reports what it
did.

**A cluster that binpack refuses today may resolve tomorrow, and vice versa.** Adding a pool whose
nodes carry no matching label turns a derived mapping into a refusal — loudly, at preflight, on
all three commands. That is the intended behaviour and the direct answer to ADR-0012's objection,
but it means a cluster change can stop binpack rather than narrowing it. A `discovery.nodeGroups`
entry, or the `kubectl label nodes` ADR-0012 already recommends, settles it permanently for
anyone who does not want a derived answer.

**`discovery.nodeGroups` is new public API**, on the same terms as everything else in
`binpack.motleyhand.com/v1alpha1`, and `binpack explain --output json` gains a `pools` object.
Both are additive.

**Nothing about binpack's permissions changes.** The derivation reads the snapshot binpack already
collects: nodes it already lists, and a status ConfigMap it already reads. No cloud credential, no
new RBAC verb, and no new object.
