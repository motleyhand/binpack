# ADR-0003: The decision engine is a pure function

- **Status:** accepted
- **Date:** 2026-08-15

## Context

binpack ships two frontends over the same logic: a read-only CLI that explains what it would do
against your kubeconfig, and an in-cluster controller that does it.

That creates an obvious failure mode. If the two frontends each implement the decision
separately, `binpack explain` becomes a plausible lie — it tells you one thing and the
controller does another. For a tool whose entire value proposition is *trust me, the arithmetic
says this is safe*, that failure mode is fatal.

The Kubernetes descheduler illustrates the alternative. Its plugins hold a client and evict
inline, so behaviour is a property of a running process rather than of a value you can inspect.
That is precisely why it is hard to predict what the descheduler will do before running it.

## Decision

The decision engine is a **pure function**:

```go
// package engine
func Decide(s Snapshot, cfg Config) Decision
```

`Snapshot` is a plain value struct — nodes, pods, PodDisruptionBudgets, cooldown state and the
current time. It holds no clients, no contexts and no interfaces that reach the network.

Resource quantities arrive already parsed into an open `map[string]int64` keyed by resource
name, so the engine never touches `resource.Quantity`. CPU is normalised to millicores and
everything else to its base unit. The map is deliberately open rather than a struct of CPU,
memory and pods: the scheduler accounts for `ephemeral-storage`, `hugepages-*` and extended
resources like `nvidia.com/gpu` on equal terms, and a fixed struct would silently ignore them.

`Decision` carries the verdict **and the arithmetic that produced it**: which node was chosen,
what would need to relocate, how much room exists elsewhere, and for every rejected candidate,
the specific reason it was rejected.

**Dependency rule:** `internal/engine` must not import `k8s.io/*` or `sigs.k8s.io/*`.
Conversion from Kubernetes API objects happens at the `internal/collect` boundary and nowhere
else. This is enforced by a `depguard` rule in golangci-lint, so violating it fails CI.

## Consequences

- **The frontends cannot disagree.** `explain` and `run` call the same function over the same
  snapshot type. The only difference is whether the resulting `Decision` is printed or executed.
- **Tests need no cluster.** Every rule is a table-driven test over a hand-built `Snapshot`:
  no API server, no envtest, no kind, no fixtures directory. This makes strict test-first
  development cheap enough to actually sustain, and the test table becomes the readable
  specification of the decision procedure.
- **`--explain` output is free.** The reasoning is a returned value rather than log lines, so
  rendering it is a formatting concern. This is the feature that makes the tool trustworthy,
  and it costs nothing because the design already produced it.
- **The engine stays deterministic.** Cooldown and anti-thrash state are *inputs* to `Decide`,
  not internal mutable state. The same snapshot always yields the same decision, which means a
  bug report can be reduced to a snapshot and replayed as a test case.
- The cost is a conversion layer in `internal/collect` and a set of value types that partly
  mirror Kubernetes API types. This is deliberate: the engine's types carry only the fields the
  decision needs, which keeps the tests legible and prevents the engine from quietly growing a
  dependency on API details it should not care about.
