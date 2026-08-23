# ADR-0008: The engine operates on Kubernetes API types

- **Status:** accepted. Supersedes the mechanism of
  [ADR-0003](adr-0003-pure-decision-engine.md), and keeps its principle.
- **Date:** 2026-08-15

## Context

[ADR-0003](adr-0003-pure-decision-engine.md) established that the decision engine is a pure
function over plain values, and enforced it with a rule: `internal/engine` may not import
`k8s.io/*` or `sigs.k8s.io/*`. Everything the engine needed was mirrored into bespoke snapshot
types, and `internal/collect` translated Kubernetes objects into them.

That rule has now been tested by use, and it does not hold up.

### The rule was a proxy for a different rule

ADR-0003 claimed four benefits. It is worth asking, of each, what actually produces it:

| Property | What it actually requires |
|---|---|
| The CLI and controller cannot disagree | One shared decision function |
| Tests need no cluster | The function takes **data**, not clients |
| `explain` output falls out for free | Reasons are returned values |
| The engine is deterministic | State is an input, not read internally |

None of them require the absence of Kubernetes *types*. `corev1.Pod` is an inert struct that can
be written as a literal in a test. What forces a cluster into a test is holding a **client**, not
naming a **type**.

The real invariant is: *the decision logic performs no I/O and holds nothing that can*. "No
Kubernetes imports" was a stand-in for that — one that agrees with it most of the time and
diverges precisely where the cost is highest.

### The divergence is measurable, because it already happened

Every substantive defect found while reviewing the design was a property of the mirror:

- extended resources and hugepages absent from the modelled capacity
- effective pod requests computed differently from the scheduler (init containers, sidecars,
  `spec.overhead`)
- `pods` present in `allocatable` but never synthesised into a pod's requests
- DaemonSet and mirror pods not distinguished from relocatable ones

Each is the same shape: *the mirror was missing something Kubernetes has, or computed it
differently.* The mirror did not insulate the engine from API details; it hid them until they
caused a wrong decision. The translation layer built to feed it became, by its own
documentation, the highest-risk package in the project — a risk that exists only because the
translation exists.

### And it was about to force an epicycle

`internal/fit` must evaluate node affinity, taints and a feature allowlist. Those need real API
objects. Under ADR-0003 the engine could not call such a function directly, which forced a
choice between precomputing a static eligibility relation and injecting a `FitChecker` interface
expressed in mirror types with a lookup back to the originals. Both are machinery in service of
the proxy rather than the property.

## Decision

The engine operates on Kubernetes API types, and holds no clients.

```go
func Decide(s Snapshot, cfg Config) Decision

type Snapshot struct {
    Nodes []*corev1.Node
    Pods  []*corev1.Pod
    PDBs  []*policyv1.PodDisruptionBudget
    Now   time.Time
    // cooldown and backoff state remain inputs
}
```

The enforced rule becomes **no cluster access and no I/O**, rather than no Kubernetes packages,
and it is expressed as an **allowlist**: `internal/engine` may import the standard library, this
module, and `k8s.io/api`, `k8s.io/apimachinery` and `k8s.io/component-helpers`. Anything else is
refused.

An allowlist rather than a denylist for the same reason
[ADR-0006](adr-0006-scheduler-fidelity.md) gives for unmodelled scheduler constraints: a list of
clients we remembered can never be complete. `k8s.io/client-go` is the obvious one, but
`k8s.io/metrics` ships a clientset too, every cloud SDK ships one, and the next release may add
another. Refusing by default is the only formulation that stays correct as the ecosystem moves.

The I/O-capable corners of the standard library — `os`, `os/exec`, `net`, `net/http`,
`database/sql` — are denied explicitly, since the standard library is allowed wholesale for the
arithmetic and formatting the engine genuinely needs. That is still a `depguard` rule, still
fails CI, and now expresses the invariant instead of standing in for it.

**Inputs are strictly read-only.** Informers hand back pointers into a shared cache, and mutating
one corrupts it for every other consumer in the process. The engine must never write to a Node,
Pod or PDB it is given. This is the one hazard the change introduces that the mirror types made
impossible, and it cannot be linted — it is a review rule, recorded in `CLAUDE.md`.

## Consequences

- **`internal/collect` mostly stops existing.** Its job was translation, and there is nothing
  left to translate: it lists objects and hands them over. The risk concentrated there does not
  move elsewhere, it evaporates, because the transformation that could be wrong is no longer
  performed.
- **`internal/fit` needs no interface.** It takes `*corev1.Pod` and `*corev1.Node`, and the
  engine calls it directly. No eligibility relation, no lookup table, no injected logic. See
  [ADR-0006](adr-0006-scheduler-fidelity.md).
- **The rule outgrew the engine, so it is called `purity` and covers four packages.** `fit` was
  the first to join, on this ADR's own reasoning. `internal/drain` and `api/v1alpha1` joined
  later, on reasoning this ADR did not supply, and the gap between what was decided here and
  what was written elsewhere is worth recording: the architecture document had described
  `api/v1alpha1` as guarded by this allowlist for some time before it was, and the package doc
  of `internal/drain` had claimed to be a pure function with nothing holding it to it. Neither
  claim was false about the code — both packages satisfied the rule the whole time — but
  "enforced" and "true so far" are different assurances, and only one of them survives the next
  person who needs one more fact. Widening the rule made the documents true rather than the
  reverse.

  `api/v1alpha1` brings one entry with it: `sigs.k8s.io/yaml`, which parses bytes it is handed
  and opens nothing. Allowing a parser is not a hole in "no I/O" — `Load` takes its document as
  an argument precisely so that reading it stays somebody else's job.
- **Upstream helpers are used at the point of use.** Effective pod requests come from
  `k8s.io/component-helpers/resource.PodRequests` where they are needed, rather than being
  precomputed into a mirror that can drift from what the scheduler does.
- **Engine tests build API literals.** Wordier than a bespoke struct. In exchange the fixtures
  are the same shape as production data, so a test that passes is evidence about the real thing
  rather than about the mirror.

  The verbosity is handled with two complementary patterns, and both are wanted:

  **Object mothers** name canonical objects — `mother.SmallNode()`, `mother.DaemonSetPod()`,
  `mother.PodWithHostPort(8080)`. A test that needs "an ordinary 4GB node" says so in three
  words, and the meaning of "ordinary" lives in one place. This is what keeps tests *short*.

  **Builders** customise from there — `mother.SmallNode().WithTaint(...)`,
  `mother.WebPod().Requesting("2Gi")`. This is what keeps tests *specific* without each one
  restating an entire object.

  Mothers return builders, so the two compose: start from a named archetype, change the one
  thing the test is about. A test that reads `mother.SmallNode().WithTaint(noSchedule)` states
  its subject in a line, which matters more here than in most codebases — the test table *is*
  the specification of the decision procedure.
- **Everything ADR-0003 was for survives.** One decision function, no I/O, no clients, state as
  input, reasons as returned values, tests that need no cluster. Only the mechanism changed.

## What would reverse this

If the engine ever needs to depend on something that is not an inert API type — a client, a
lister, a context — that is the signal this went wrong, and the boundary should be re-drawn
rather than the rule relaxed.
