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
| Effective pod requests: init-container peaks, native sidecars, `spec.overhead`, pod-level resources | `resource.PodRequests` |
| Required node affinity and node selectors | `nodeaffinity.GetRequiredNodeAffinity` |
| Taints and tolerations | `corev1.FindMatchingUntoleratedTaint` |

Resource fit against `status.allocatable` is implemented directly, since the rules are now
understood and the inputs come from upstream.

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
`NodeUnschedulable`, `NodeName`, `NodeAffinity` (including `nodeSelector`), `NodeResourcesFit`
and `TaintToleration`.

Everything else triggers a refusal. That includes, non-exhaustively: `hostPort` (`NodePorts`);
any persistent volume claim, since deciding whether a volume can follow the pod means modelling
`VolumeBinding`, `VolumeZone`, `VolumeRestrictions` and per-node CSI attachment limits; required
inter-pod affinity or anti-affinity; topology spread with `whenUnsatisfiable: DoNotSchedule`; a
non-default `schedulerName`; scheduling gates; and dynamic resource allocation claims.

The point of the allowlist is that this paragraph does not have to be exhaustive for the design
to be sound. An unrecognised feature refuses by default.

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

**A pod resized downward in place.** In-place vertical scaling changes a running pod's requests
without touching its controller's template, so `EffectiveRequests` reports less than the
replacement will actually ask for. This one is *not* bounded: binpack could approve a node the
replacement does not fit, leaving it Pending and provoking exactly the scale-up this design
exists to prevent. A resize still in flight is refused outright, but a completed one leaves no
marker on the pod — `status.resize` and the resize conditions track pending and in-progress
operations only.

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

**Verifying the first case mattered more than it looks.** The obvious cheaper fix — compare the
pod's requests against what its container statuses report as allocated — does not work, and the
API says so: the kubelet sets `allocatedResources` to the new value *after* a successful resize,
so a completed one leaves the two in agreement. There is no marker to find, which is why the
template is the only answer.

The allowlist is applied to **both** the pods being relocated and the pods already resident on
each prospective destination. Inter-pod affinity is symmetric: the scheduler rejects an incoming
pod if a pod already on that node declares required anti-affinity matching it. A relocating pod
using nothing but allowlisted features can therefore still be refused by a destination, and
checking only the relocating side would let exactly that case through.

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
  should run against more than one Kubernetes version in CI once there is reason to.
- `explain` must surface the refusal reason, not just the verdict. "Skipped: pod X has required
  anti-affinity, which binpack does not model" is a useful answer. "Skipped" alone is not.
