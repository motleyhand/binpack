# ADR-0012: Mapping a node to its pool needs a label whose value is the autoscaler's identifier

- **Status:** accepted. Supersedes the resolution order in
  [ADR-0004](adr-0004-provider-agnostic-no-cloud-api.md): its step 2, "**Configured.** Otherwise,
  fall back to pool membership by label and operator-stated minimums", was never implemented and
  is withdrawn rather than deferred. ADR-0004's decision — take nothing from a cloud API — stands
  unchanged, and this narrows what that costs.
- **Date:** 2026-08-22

## Context

binpack needs to know which pool a node belongs to, and what that pool's bounds are. ADR-0004
settled where the answer comes from: the `cluster-autoscaler-status` ConfigMap, never a cloud
API. It also identified the gap that leaves — the autoscaler names a pool with a
provider-specific identifier, so a node has to carry that identifier for the two to be joined —
and published a three-step resolution order to close it. Step 1 was discovery through a
configured node label. Step 2 was a fallback: "pool membership by label and operator-stated
minimums".

**Step 2 does not exist and never did.** `v1alpha1.Discovery` holds two label *keys* and nothing
else; `v1alpha1.PoolOverride` holds a name and a policy; `engine.NodeGroup.MinSize` is written in
exactly one place, from the status ConfigMap. `v1alpha1.Load` uses `yaml.UnmarshalStrict`, so a
document that tries to state a pool minimum is not ignored — it is rejected as an unknown field.
The ADR's Costs section then reasons at length about the operating characteristics of "configured
mode", which is a mode a reader can neither reach nor switch off.

What that cost, before this ADR, was not a missing feature. It was silence. A cluster whose nodes
carry no matching value skips every node with `not-autoscaled`, and binpack prints that verdict
directly beneath its own list of healthy autoscaling pools: `explain` reports a running
autoscaler, lists the pools with their sizes, and then says every node is "not part of an
autoscaling pool" — since the summary began carrying a count, it says so about *n* of *n* nodes,
which makes the wrong answer sound more certain rather than less. Nothing in any of it names the
label, the values it was compared against, or the values the nodes actually have. An operator
reading the design documents to find the fallback found a step 2 with no field to set.

`diagnose` is quieter still, and the mechanism is worth stating exactly, because it is narrower
than it first looks. `staticNodes` returns every node whose label maps to no managed pool — under
a mismatch, all of them — and `diagnoseWorkloads` marks each finding it groups as freeing nothing
when all of the nodes it sits on are static. `exitFor` then excludes those findings from
`--fail-on` unless `--fail-on-static` is set. So a gate built on workload findings — local
storage, a bare pod, an unevictable replica — moves from failing to exit 0 with nothing in the
workload having changed. Budget, pool and node findings do **not** go through that path and are
unaffected: a zero-disruption PDB still fails the gate under a mismatch exactly as it does
without one. Traced in both directions rather than read off the code, because reading
`staticNodes` alone suggests the stronger claim.

### What the identifier actually is

The cluster-autoscaler builds each status entry as `api.NodeGroupStatus{Name: nodeGroup.Id()}`
(`GetStatus`, `pkg/clusterstate/clusterstate.go`), so `nodeGroups[].name` is whatever the cloud
provider's `Id()` returns. That is a different string per provider, and it is not in general the
pool name anyone typed into a console. Read from the provider implementations rather than
inferred:

| Provider | `Id()` returns | Source |
|---|---|---|
| DigitalOcean | the node pool's ID | `digitalocean/digitalocean_node_group.go` |
| AWS | the Auto Scaling group's name | `aws/aws_cloud_provider.go` |
| Azure | the VM Scale Set's name | `azure/azure_scale_set.go` |
| GCE | the managed instance group's full API URL, `https://www.googleapis.com/compute/v1/projects/<project>/zones/<zone>/instanceGroups/<name>` | `gce/gce_cloud_provider.go` via `GenerateMigUrl` |

DOKS is the case that works out of the box, because `doks.digitalocean.com/node-pool-id` carries
that same pool ID — which is why it ships as the default.

Elsewhere, whether a label a provider applies happens to carry the identifier is a question about
a particular cluster, and two read-only commands answer it: `kubectl -n kube-system get configmap
cluster-autoscaler-status -o yaml` prints the names, and `kubectl get nodes --show-labels` prints
the labels. What can be said in general is narrower than it looks, and is about two specific
labels. `eks.amazonaws.com/nodegroup` carries the *EKS managed node group's* name, while the
identifier is the name of the Auto Scaling group EKS generates to back it;
`kubernetes.azure.com/agentpool` carries the agent pool's name, while the identifier is the scale
set's. In both, the two strings are related but not equal, so a reader who assumes the key is the
answer because it looks like the answer gets the silent failure above.

Nodes that did not come from a managed node group carry neither label, and the tool that created
them may supply one of its own that is no better. Checked against a real EKS cluster whose nodes
eksctl created and whose autoscaler discovers Auto Scaling groups by tag: the published identifier
was `eksctl-testing-nodegroup-testing-24-NodeGroup-iOzTWO7QftDh` — the generated CloudFormation
name — while the nearest label was `alpha.eksctl.io/nodegroup-name=testing-24`, which is a
substring of it and not equal to it. No `eks.amazonaws.com/*` label existed at all. A self-managed
Auto Scaling group's name is the operator's to choose, so on some clusters it *will* already be a
label value — which is exactly why binpack points at the two commands rather than asserting.

GCE is worse than a mismatch and worse in kind. A Kubernetes label value is at most 63
characters, may contain only alphanumerics, `-`, `_` and `.`, and must begin and end with an
alphanumeric (`k8s.io/apimachinery`, `IsLabelValue`). A URL is none of those things. **No label
can carry a MIG identifier**, so on GCE the mapping cannot be expressed at all — not by a
provider's label, and not by one an operator applies.

## Decision

**binpack has one way to map a node to a pool: a node label whose value equals the identifier the
cluster-autoscaler publishes.** There is no second mode, and no configuration that states pool
membership or bounds. `discovery.nodeGroupIDLabel` names the key; the value is the cluster's to
supply.

**Where no label already carries it, the remedy is a label the operator applies themselves**,
and binpack documents it as such rather than implying a provider supplies one:

```bash
kubectl label nodes <node>... binpack.motleyhand.com/node-group=<group>
```

with `discovery.nodeGroupIDLabel: binpack.motleyhand.com/node-group`. The suggested key is under
binpack's own API group because a key in a provider's namespace is one the provider may start
writing itself, and a value overwritten underneath you is a mapping that breaks with no
configuration having changed. binpack never writes this label; it reads whichever key the setting
names.

**And where nothing maps, binpack fails preflight.** `engine.CheckPools` — which `explain`,
`diagnose` and `run` all already call before deciding anything — returns an error naming the
configured key, the groups the autoscaler published, and the label keys the nodes do carry, and
ends with the command above. All three commands exit 1.

The refusal is deliberately narrow, because refusing is the expensive direction. It requires all
three of: an autoscaler binpack can vouch for as live, at least one published group with a ready
node in it, and no node whose configured label holds a value any published group answers to. Drop
any one and there is a question rather than an answer — a stale status document says nothing
about the cluster now and `Autoscaler.Live` already names *that* as the problem; a group scaled to
zero has no node to carry its identifier and is not evidence of anything; and one node matching is
proof the mapping works, so a cluster halfway through being relabelled goes on running.

## Alternatives considered

**Build step 2 as ADR-0004 described it** — `pools[].nodeSelector` plus `pools[].minSize`, and a
second producer of `engine.NodeGroup`. Rejected. It reintroduces exactly the drift ADR-0004
existed to eliminate: a minimum stated in binpack's configuration and enforced by the autoscaler
is two numbers that can disagree, and the one binpack would act on is the one nobody updates when
a pool is resized in a console. Worse, it is *unnecessary* everywhere the identifier is a legal
label value — a label is a smaller thing to ask for than a second source of truth about pool
bounds, and it keeps every number coming from the component that enforces it. It would also not
help GCE, where the problem is the identifier's shape rather than the label's absence: a
`nodeSelector` maps nodes to a *declared* pool, so the operator would be restating the bounds as
well, which is the drift again.

**Infer the mapping from an existing label by searching for a value that matches.** binpack could
look for any label key whose value equals a published group name and use it. Rejected as a
decision, though the error message points the operator at the same evidence. A key discovered by
coincidence on one evaluation is a key that stops matching when a pool is added, and the failure
would be a silent change of scope rather than a refusal. What binpack matches on should be
something someone chose.

**Leave it as documentation.** The reference already promised that a mismatch "fails loudly
rather than guessing", so an argument was available that only the documentation was wrong.
Rejected: the promise was the correct design and the code was the part that was missing.
Correcting the sentence downward would have left `explain` contradicting itself on the same
screen, and would have left a `diagnose` gate on workload findings passing for a reason that has
nothing to do with the workload.

**Take the mapping from a cloud API on providers where the label cannot work.** Out of scope
here; it is ADR-0004's deferred question, and see below.

## Consequences

**Three commands that returned quietly now fail.** `explain`, `diagnose` and `run` exit 1 on a
cluster where nothing maps, where before they printed a report or ran an evaluation. This is a
breaking change to observable behaviour, and it is the point: what they printed was wrong.

For a CI job invoking `binpack diagnose --fail-on=...`, the move is in one of two directions
depending on what its cluster's blockers are, and both are improvements. A gate that was passing
because every finding it would have counted was marked as freeing nothing now exits 1 rather than
0. A gate that was already failing on a budget or pool finding now exits 1 rather than 2 — a
different non-zero status for a genuinely different thing, since 2 means "diagnose ran and your
cluster has blockers" and this is diagnose declining to run. No new exit code is introduced.

**A cluster mid-relabel is not affected**, which is what makes the trade acceptable. One node
carrying a matching value is enough, so an operator applying the label pool by pool never has a
window where binpack refuses. The failing case is the one where no node has ever matched.

**The error has to carry the remedy, and a test keeps it doing so.** Converting a silent no-op
into a loud one is only an improvement if the message ends the problem where it is read, so the
command binpack prints and the command the reference gives are checked against a single shared
literal.

**GCE has no answer, and this ADR says so rather than implying one.** ADR-0004 deferred optional
cloud API integration on the grounds that "no such provider has been identified yet". One has.
That does not decide the question — the deferral stands — but the architecture document's open
question can stop suggesting a fallback that would not have helped, because a configured pool
minimum does not give binpack a way to tell which nodes are in the pool either.

**Nothing about binpack's permissions changes.** The check reads the snapshot binpack already
collects: nodes it already lists, and a status ConfigMap it already reads. No cloud credential,
no new RBAC verb, and no new object.
