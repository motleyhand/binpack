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

## Decision

### Depend on an interface, not an implementation

The engine takes a `FitChecker`:

```go
type FitChecker interface {
    CanFit(pod Pod, node Node) (bool, Reason)
}
```

The engine stays pure and its tests use a trivial fake, so
[ADR-0003](adr-0003-pure-decision-engine.md) is refined rather than contradicted: the *decision
logic* remains free of Kubernetes libraries, while the fit predicate becomes a pluggable
dependency living in `internal/fit`, which may import them.

This is what makes the staging below possible without a rewrite.

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

Two hard constraints are **not** modelled: required inter-pod affinity and anti-affinity, and
topology spread with `whenUnsatisfiable: DoNotSchedule`. Their presence on any relocatable pod
causes the candidate to be refused, with that stated as the reason in `explain` and recorded as
a metric.

### Stage 2: escalate only if measurement justifies it

If the refusal reason above fires often on real clusters, adopt the full scheduler framework
from `k8s.io/kubernetes/pkg/scheduler/framework/plugins` behind the same `FitChecker` interface.

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
