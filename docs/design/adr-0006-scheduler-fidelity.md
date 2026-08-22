# ADR-0006: Borrow the scheduler's logic, and test against the real one

- **Status:** accepted
- **Date:** 2026-08-15

## Context

binpack's central claim is that draining a node will not trigger a scale-up. That claim is only
as good as its model of what the kube-scheduler will accept.

Early drafts of the design hand-rolled that model, and review found five separate defects in
quick succession — extended resources ignored, DaemonSet pods treated as relocatable, pod slots
never consumed, container requests used instead of effective pod requests, PodDisruptionBudget
allowances checked per pod rather than per drain. These were not five unrelated mistakes. They
were one design pressure appearing repeatedly: **the gap between "the resources add up" and
"the scheduler will actually place this."**

Hand-rolling a scheduler model is a losing game. The rules are numerous, subtle, and change
between releases.

Neither of the serious tools in this space plays it. Cluster-autoscaler imports the scheduler
framework and runs the real Filter plugins against a simulated snapshot. Karpenter does the
same. This ADR decides how far binpack should follow.

## The asymmetry that makes this tractable

binpack needs soundness in **one direction only**.

It must never report a placement as feasible when the scheduler would refuse. It is entirely
free to be wrong the other way: refusing a drain that would have worked costs a missed
consolidation, which the next run may find. Refusing is the safe error.

Two consequences follow, and they shrink the problem considerably.

**Only filtering matters, never scoring.** The scheduler's Filter plugins decide whether a pod
*can* go on a node; Score plugins decide which node it *prefers*. binpack only needs the former.
That is both the smaller half and the half available without heavyweight dependencies. It also
means the soft variants of constraints — `whenUnsatisfiable: ScheduleAnyway`,
`preferredDuringSchedulingIgnoredDuringExecution` — can be ignored outright, because they only
affect scoring.

That is a claim about scoring, and it holds. It is not a claim that the framework has two halves.
The 1.36 default profile also enables `SchedulingGates`, a PreEnqueue plugin: a gated pod never
enters the scheduling queue, so no Filter runs on it and no Filter refuses it. `GangScheduling`,
off by default at this release, is PreEnqueue and Permit, and can hold a pod that every Filter
accepted. A replacement blocked at either point never binds, while a simulation that asks only
whether a destination filters cleanly sees nothing wrong. Refusing those pods is the allowlist's
job rather than the fit predicate's — `internal/fit` refuses a pod carrying `spec.schedulingGates`,
which covers the gate a workload declares in its own template.

**An unmodelled constraint is a bounded limitation, not a bug** — provided its *presence* is
detected and causes a refusal. "We do not model hard topology spread" is acceptable. "We
silently ignore hard topology spread" is not.

### What the guarantee does not cover

The one-directional property is about the **fit predicate**: if binpack says a pod fits a node,
the scheduler agrees. It says nothing about which of several valid nodes the scheduler will
actually pick, and that gap is real.

A feasible packing is an *existence* proof. If pod A fits N1 or N2 while pod B fits only N2, the
simulation may assign A to N1 and B to N2 and be entirely correct that a valid assignment
exists — and the scheduler may still place A on N2, fill it, and leave B Pending. Every filter
check was sound and the outcome was still wrong.

binpack cannot close this, because it does not place pods and has no supported way to steer the
scheduler. It closes it operationally instead, by never having more than one pod in flight:
evictions are sequential and the state is revalidated between each, so the only open question at
any moment is where a single pod lands, and that is observed before anything else moves.

The consequence must be stated honestly in user-facing documentation rather than implied away:
**binpack makes a scale-up unlikely and immediately detectable; it cannot make it impossible.**

## Decision

### Keep fit in one package, called directly

Fit lives in `internal/fit` and is called directly by the engine:

```go
func CanFit(pod *corev1.Pod, node *corev1.Node) (bool, Reason)
```

An earlier draft of this ADR wrapped it in an interface so the engine could avoid naming
Kubernetes types. [ADR-0008](adr-0008-engine-uses-api-types.md) removed that constraint, and
with it the interface, the lookup back to the original objects, and the choice between them.
The engine imports `internal/fit` like any other package.

Staging remains possible without a rewrite: the implementation behind `CanFit` can change
completely as long as its answer stays sound in the direction described above.

### Stage 1: the light path, plus honest refusal

Use upstream code wherever it is available from Kubernetes *staging* repositories, which are
published as ordinary Go modules with none of the pain of depending on `k8s.io/kubernetes`
itself. `k8s.io/component-helpers` states in its own package documentation that it exists for
"a core component and another kubernetes project (cluster-autoscaler, descheduler)" — precisely
this use case.

| Need | Upstream |
|---|---|
| Effective pod requests: init-container peaks, native sidecars, `spec.overhead`, pod-level resources — and, for a pod already on a node, what that node actually allocated it | `resource.PodRequests` |
| Required node affinity and node selectors | `nodeaffinity.GetRequiredNodeAffinity` |
| Taints and tolerations | `corev1.FindMatchingUntoleratedTaint` |
| Node capabilities a pod's spec implies, and whether a node declares them | `nodedeclaredfeatures.DefaultFramework` |

Resource fit against `status.allocatable` is implemented directly, since the rules are now
understood and the inputs come from upstream.

Borrowing a function is not the same as borrowing its configuration. `FindMatchingUntoleratedTaint`
takes an `enableComparisonOperators` flag, and the scheduler passes the
`TaintTolerationComparisonOperators` gate rather than a constant — alpha and off by default, so
`Gt` and `Lt` tolerations do not match there. binpack passes `false` for the same reason it derives
the differential harness's feature gates from the release rather than listing them: what the
cluster actually does is the only fidelity that counts. Where the answer is version-dependent,
binpack takes the value that under-tolerates, which costs a consolidation rather than a wrong one.

The rule has a second shape, where the right configuration depends on *which question is being
asked* rather than on a gate. The scheduler calls `resource.PodRequests` twice, with different
options each time. `noderesources.Fit` sizes the incoming pod with none of them, on grounds it
states at the call site — a "pod hasn't scheduled yet so we don't need to worry about
InPlacePodVerticalScalingEnabled". `PodInfo.CalculateResource` sizes a pod already on a node with
`UseStatusResources`, which resolves each container to `max(spec, actuated, allocated)`. The two
answers differ exactly while an in-place vertical scale is in flight, and for a memory decrease
the kubelet cannot actuate below current usage — so "in flight" lasts as long as the workload's
usage does, not as long as an API round trip.

binpack makes the same split, as `fit.EffectiveRequests` and `fit.ObservedRequests`, and it is a
split by where the pod came from rather than by what it looks like. A destination starts at
allocatable minus its residents, and a resident is an occupancy: sized from spec alone it hands
the simulation room the node has already given away, and both the packing and the reserve
computed from it believe the invented figure. A replacement or a size probe is a demand and has
actuated nothing, so it is sized from spec — which is also what keeps the reserve's two halves
agreeing, since the frontier is computed from a replacement that carries the running pod's
`Status` and the probe it is measured through is rebuilt without one.

This one is not an instance of "resolve towards no", and it is worth being clear that it is not.
Charging a resident `max(spec, actuated, allocated)` is usually more than spec and so happens to
be conservative — but a resize the kubelet has rejected as infeasible resolves to
`max(actuated, allocated)`, ignoring spec, and can be less. Matching the scheduler is the rule
here. Conservatism is what binpack falls back on where the scheduler's answer is version-dependent
or unknown, and this answer is neither.

#### What is modelled is a closed allowlist, not an open denylist

An earlier draft named two constraints as unmodelled and implied the rest were covered. That
structure is unsound, and demonstrably so: the list omitted `hostPort` conflicts, persistent
volume node affinity and zone constraints, and CSI attachment limits, each of which can make
every destination invalid while resource, affinity and taint checks all pass.

A denylist of things we forgot can never be complete, and every future Kubernetes release adds
to it. So the model is inverted. `internal/fit` maintains an **allowlist of pod features it
knows how to reason about**, and any relocatable pod using a feature outside that list makes its
node an invalid candidate, with the specific feature named as the reason.

Stage 1 models the constraints corresponding to these scheduler Filter plugins:
`NodeUnschedulable`, `NodeName`, `NodeAffinity` (including `nodeSelector`), `NodeResourcesFit`,
`TaintToleration` and `NodeDeclaredFeatures`.

Everything else triggers a refusal. That includes, non-exhaustively: `hostPort` (`NodePorts`);
any persistent volume claim, since deciding whether a volume can follow the pod means modelling
`VolumeBinding`, `VolumeZone`, `VolumeRestrictions` and per-node CSI attachment limits; required
inter-pod affinity or anti-affinity; topology spread with `whenUnsatisfiable: DoNotSchedule`; a
non-default `schedulerName`; scheduling gates; and dynamic resource allocation claims.

The point of the allowlist is that this paragraph does not have to be exhaustive for the design
to be sound. An unrecognised feature refuses by default.

That was the intent, and for a while it was not what the code did. `firstConstrainingVolume` is
a `switch` whose `default` refuses, so an unknown volume source really does refuse; the pod-field
checks were a sequence of `if`s ending in "no objection", so an unknown *field* was accepted.
Kubernetes 1.36 supplied the demonstration: `spec.schedulingGroup` holds a pod at the scheduler's
`Permit` phase until its whole gang can be placed, and it fell straight through. The inversion
cannot be written into the control flow — there is no `default` branch over a struct's fields —
so it is enforced from outside instead. `TestPodSpecFieldsAreAccountedFor` reflects over
`corev1.PodSpec` and requires every field to be named either as one `internal/fit` accounts for
or as one no Filter plugin reads, with the reason. A field the next release adds fails CI naming
itself, which is what makes the paragraph above true rather than aspirational.

**A requirement can be about the destination, and then it must be derived, not recognised.**
`NodeDeclaredFeatures`, on by default since 1.36, refuses a node whose kubelet has not published
a capability the pod's spec implies — today, a container carrying a `RestartAllContainers`
restart rule. That is a question about the *pair*, so it is asked in `UnsupportedDestination`
rather than `UnsupportedPod`, and it is asked by running upstream's own inference and node match
(`k8s.io/component-helpers/nodedeclaredfeatures`) rather than by recognising the one shape that
is live today. The reasoning is the same as for the differential harness's feature gates, and the
stakes are worse: a hand-written list that upstream has moved past does not fail, it accepts. The
one number the derivation still needs — the version requirements are inferred at — is pinned to
the release this module depends on, and is held by a test to a value that drops no registered
feature, because dropping one is the accepting direction.

#### Closed: the running pod was a proxy for the replacement

> **Closed.** binpack reads owner templates and places the pod a controller *would create*.
> Recorded here as it was found, because the reasoning is what justifies the permissions it
> costs.

`CanFit` was asked whether a *replacement* pod could be placed, and was handed the *running*
one. Almost always these agree, and where they did not, binpack could not tell. Two cases were
known, and they differed in how much they mattered.

**A controller template that pins `spec.nodeName`.** Every pod binpack sees is already bound, so
its own `nodeName` names the node it is leaving and cannot distinguish a pinned template from an
ordinary scheduling decision. The consequence is bounded: such a pod ignores cordon — setting
`nodeName` bypasses the scheduler — and reappears on the node being drained rather than going
Pending, so no scale-up follows and the drain stalls and backs off as
[ADR-0007](adr-0007-drain-progress-not-deadlines.md) provides for any undetected blocker. It
costs a wasted drain.

**A pod resized downward in place, in the role of the pod being moved.** In-place vertical
scaling changes a running pod's requests without touching its controller's template, so the
running pod's own requests report less than the replacement will actually ask for. This one is
*not* bounded: binpack could approve a node the replacement does not fit, leaving it Pending and
provoking exactly the scale-up this design exists to prevent. A resize still in flight is refused
outright, but a completed one leaves no marker on the pod — `status.resize` and the resize
conditions track pending and in-progress operations only.

**The same pod in the role of a resident is a different question, and gets a different answer.**
A pod mid-resize sitting on a *destination* is not being moved and has no replacement; what
matters about it is how much of that node it is holding, which is what the scheduler charges it
and not what its spec has most recently been patched to. So it is accounted through
`fit.ObservedRequests` rather than refused. Refusing instead — disqualifying any destination
hosting a pod mid-resize — would also be sound, and would cost every relocation onto a node with
gigabytes genuinely free. The refusal named above is about the relocating side only, which is why
it never covered this.

Both closed the same way: `collect` reads `ReplicaSet`, `StatefulSet`, `DaemonSet` and `Job`
templates, and the engine builds the replacement from the template while keeping the running
pod's identity for anything it reports. `fit` needed no signature change — it was always asking
about a pod, and now receives the right one.

The second case became *checkable* rather than merely bounded in the same change. Every running
pod names a node, so a pinned template was invisible; a replacement's `nodeName` comes from the
template alone, so `fit` now refuses it outright instead of relying on the drain stalling.

**A pod whose controller kind has no readable template is refused, not guessed at.** An
operator's own CRD provides no template, and inferring the replacement from the running pod is
precisely the inference that is unsound. This follows the allowlist rule, and it was settled
against measurement as this ADR asks: on a real cluster all 145 pods resolved a template through
the four kinds above, so the refusal cost nothing there. A metric counts it, so if that turns
out not to be general the evidence will exist before the rule is relaxed.

**The template alone is not the answer either.** Admission mutates a pod on creation — a
service-mesh sidecar injected by a webhook, requests filled in by a LimitRange, RuntimeClass
overhead — so the stored template understates the replacement exactly as often as a resize
overstates it, and in the same unsound direction. Requests are therefore the per-resource
maximum of template and running pod, and a container present only on the running pod is carried
over whole: each source understates a different case, neither overstates, so the larger is
conservative in both.

Labels come from the template alone rather than a union, because selector matching is not
monotonic. An extra label makes an `In` selector match and makes a `DoesNotExist` selector stop
matching, so "more labels" is not a safe direction and only the set the replacement will carry
is right.

A residual gap remains and is worth naming: a label added at admission is on the running pod and
not the template, so a destination whose pods select on it is not consulted about it. Unlike the
resource case there is no conservative merge — adding the label is unsafe for `DoesNotExist`
selectors and omitting it is unsafe for `In` ones — and closing it properly means running the
admission chain, which is far outside what this project should do. It is bounded: it costs a
placement the scheduler refuses, which stalls the drain and backs off rather than causing a
scale-up, because the pod is rejected rather than left unschedulable somewhere new.

#### Closed: admission-added placement constraints

Requests are merged safely because the larger of two figures is meaningful. Placement
constraints are not: a mutating webhook that adds a `nodeSelector`, a required affinity term, a
toleration, a scheduler name or a volume leaves the template looking *less* constrained than the
pod the scheduler will receive, and there is no "larger of" for an affinity term.

The obvious remedy — refuse whenever the running pod carries a constraint its template does not —
was implemented and withdrawn once, because it refused **80 of 122 pods** on a real cluster, none
of them from a webhook. That measurement was right and the conclusion drawn from it was wrong: it
was not evidence the check could not work, but that it was comparing the wrong things.

Measuring again, by field and excluding pods that are never relocated, the differences separate
into three kinds:

| Kind | Fields | Divergence on a real cluster | Treatment |
|---|---|---|---|
| Permissive | tolerations | 150 of 150 | Use the template's. The API server adds two `NoExecute` tolerations to every pod; tolerating *less* costs a missed destination, never a wrong one |
| Additive | containers, volumes | 137 of 150 | Merge. An injected sidecar or a `volumeClaimTemplate` can only narrow placement further |
| Restrictive | `nodeSelector`, affinity, topology spread, `schedulerName`, `runtimeClassName` | **0 of 122** | Refuse. These make the replacement look freer than it is |

Nearly all of the original 80 were the projected service-account token volume and the two
defaulted tolerations. The remaining affinity differences were DaemonSet pods, which the
DaemonSet controller pins with `matchFields: metadata.name` and which binpack never relocates.

So the check refuses only on the fields where a missing constraint is unsound, and on the cluster
that made the first attempt look unworkable it now refuses nothing at all.

**The gap was not bounded, and this ADR previously said it was.** The reasoning given — that the
scheduler rejects the pod, so the drain stalls and backs off — does not hold. Once a pod is
evicted its replacement is not aimed at the node binpack chose; the scheduler places it wherever
it fits, and if an admission-added constraint means it fits nowhere it goes Pending, which is
precisely what makes the autoscaler add a node. The drain would *complete*, so no backoff would
ever fire: binpack would cause the scale-up it exists to prevent, and report success.

**Verifying the first case mattered more than it looks.** The obvious cheaper fix — compare the
pod's requests against what its container statuses report as allocated — does not work, and the
API says so: the kubelet sets `allocatedResources` to the new value *after* a successful resize,
so a completed one leaves the two in agreement. There is no marker to find, which is why the
template is the only answer.

The allowlist is applied to **both** the pods being relocated and the pods already resident on
each prospective destination. Inter-pod affinity is symmetric: the scheduler rejects an incoming
pod when another pod declares required anti-affinity matching it and sits in the same topology
domain. A relocating pod using nothing but allowlisted features can therefore still be refused by
a destination, and checking only the relocating side would let exactly that case through.

**The domain is the node only at `kubernetes.io/hostname`.** This paragraph originally read "a pod
already on that node", which is that special case stated as the general rule. What upstream does
is build a cluster-wide (topologyKey, topologyValue) → count map over every node hosting a pod
with required anti-affinity, and then check the candidate node's own labels against it
(`interpodaffinity`'s `getExistingAntiAffinityCounts` and `satisfyExistingPodsAntiAffinity`). A
term keyed on `topology.kubernetes.io/zone` — the ordinary way to keep replicas of one workload
in separate zones, on a label every managed provider sets — therefore rejects every node in that
zone, including nodes hosting no matching pod of their own. Asking each candidate node about its
own residents asks a narrower question and gets it wrong in the accepting direction: binpack
approves a destination that will refuse the replacement, and the eviction has already happened.

So `internal/fit` indexes those terms the way the scheduler indexes them.
`NewAntiAffinityDomains` is built once per simulation from the nodes and pods the engine already
holds, and passed down to `CanFit`. It has to be, because the question cannot be answered from
one node: the pod declaring the term need not be on any node the current placement is looking at.
The blunt alternative — refuse every destination whose domain binpack cannot survey — is sound
and useless, since every node in a cloud cluster carries a zone label and consolidation would
stop everywhere.

Widening what `CanFit` takes is a deliberate move and it stays inside ADR-0008's rule: the index
is built from Kubernetes API objects passed as data, holds no client, does no I/O, and can be
written in a test literal. An empty index is a legitimate value meaning the caller has no view
wider than the node in front of it — which is what the differential harness, whose oracle also
models a single node, necessarily passes.

The soft counterparts of these constraints — `preferredDuringSchedulingIgnoredDuringExecution`,
`whenUnsatisfiable: ScheduleAnyway` — are ignored rather than refused, because they affect only
scoring and can never cause a placement to fail.

This is deliberately conservative, and it may turn out to be *too* conservative: refusing every
pod with a PVC would exclude most stateful workloads. That is exactly what the metric is for.
The initial allowlist membership is settled in the `internal/fit` specification, against
measurement rather than intuition, and it can only grow as constraints are genuinely modelled.

### Stage 2: escalate only if measurement justifies it

If the refusal reason above fires often on real clusters, adopt the full scheduler framework
from `k8s.io/kubernetes/pkg/scheduler/framework/plugins`, behind the same `CanFit` signature.

That is deliberately deferred rather than rejected. It buys exact fidelity, and it costs a
dependency on the main Kubernetes repository: replace directives across every staging module, a
pin to a single Kubernetes minor version, a substantially larger binary and slower builds.
Cluster-autoscaler and Karpenter both pay that price and both feel it. binpack should pay it
when it has evidence, not on principle.

### Always: differential-test against a real scheduler

Independent of which stage is in force, `internal/fit` is tested against the actual
kube-scheduler.

`envtest` provides a real API server and etcd; the real `kube-scheduler` binary runs against it
with a kubeconfig. Node objects are created directly with no kubelet, so pods are *bound* but
never run — which is all that is being measured. Prior art for the general technique exists in
`kubernetes-sigs/kube-scheduler-simulator`.

The test asserts the asymmetry rather than equality:

> if binpack says a pod fits a node, the real scheduler must agree

The converse is explicitly permitted. This is a far easier property to hold than matching the
scheduler decision for decision, and it is exactly the property the tool's correctness rests on.

What was built is not the `envtest` harness described above, and the difference is worth stating.
The oracle runs the scheduler's own Filter plugins in-process against API objects, rather than
standing up an API server with the real `kube-scheduler` binary beside it. In-process
plugins are what make three thousand generated scenarios affordable on every pull request. They
are also what turns the plugin *set* into a decision somebody has to make, where the binary would
have made it by construction.

That decision was made badly at first, and recording the correction matters more than recording
the choice, because it is the mistake above one level up. The oracle instantiated four plugins
from a list somebody typed, while the release's default profile enables fourteen that filter. A
plugin the oracle does not run cannot produce a disagreement: the placement is accepted on both
sides, and the harness reports agreement about a question it never asked. Unlike a wrong feature
gate, which announced itself with 171 loud and wrong reports, this failure prints nothing at all.

So the set is derived. `pkg/scheduler/apis/config/latest.Default()` gives the release's own
default profile under the release's own gates, and `plugins.NewInTreeRegistry()` gives the
factories under the same names; every enabled plugin that implements `Filter` must be either
built into the oracle or carry a written exemption naming the `internal/fit` refusal that keeps
binpack away from its input. The exemption is executed, not merely read, so it stops being true
if the refusal it rests on is deleted. A release that adds a Filter plugin now fails CI with that
plugin's name rather than passing quietly.

The exemptions are the honest reading of "an unmodelled constraint is a bounded limitation": each
one is a limitation, written down, somewhere a test can contradict it. Six of the seven rest on
the allowlist refusing a whole class of pod — every pod with a volume, with resource claims, with
a hard spread constraint. The seventh, InterPodAffinity, rests on two refusals, and only the
first is of that kind: a pod declaring required affinity terms of its own is refused outright,
while the symmetric direction — a resident's term rejecting the pod arriving beside it — is
checked node by node where the plugin counts across a whole topology domain. A term keyed on the
zone therefore rejects nodes that hold no matching pod of their own, and binpack approves them.

That shortfall is carried in the map as an input, not as a sentence, because a sentence cannot
stop a green test from reading as coverage. The exemption names the pod-and-node arrangement
binpack accepts and the plugin refuses, the test asserts it is still accepted, and the run logs it
on every pass. Two things follow, and both are the point: a gap cannot be declared where none
exists, and the day the refusal is widened to the topology domain the assertion fails and the
record has to be deleted rather than left behind as a false statement about a package that has
moved on. An exemption narrower than its plugin is a defect with a name, not a limitation
somebody has decided to live with.

## Consequences

- `internal/collect` and `internal/fit` are the project's highest-risk packages, not the boring
  adapter layers. Four of the five review defects were translation errors at that boundary. They
  get integration tests against real API fixtures — pods with init containers, RuntimeClass
  overhead, extended resources — in addition to the differential harness. A wrong number there
  produces a confidently wrong decision that no amount of engine unit testing can catch.
- binpack will refuse some drains that would have succeeded. That is by design, it is visible in
  `explain`, and it is measured so the decision to escalate can be made on data.
- The Kubernetes version binpack is built against becomes semantically relevant: scheduler
  behaviour changes between releases. The differential test is what will catch that, and it
  should run against more than one Kubernetes version in CI once there is reason to. It catches it
  only for plugins the oracle actually runs, which is why that set is derived from the release
  rather than listed, and why a plugin it does not run has to carry an exemption a test can
  falsify.
- `explain` must surface the refusal reason, not just the verdict. "Skipped: pod X has required
  anti-affinity, which binpack does not model" is a useful answer. "Skipped" alone is not.
