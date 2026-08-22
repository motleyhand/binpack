# Contributing to binpack

Thanks for looking. binpack is small on purpose, and the goal is that it stays easy to read and
easy to trust.

## Getting started

Requires Go 1.26 or newer and [golangci-lint](https://golangci-lint.run) v2.

```bash
make check      # lint, test, build, smoke test — what CI runs
make build      # binary into ./bin
make test       # go test -race ./...
make lint       # golangci-lint run
make fmt        # gofmt -s -w .
make help       # list every target
```

`make check` is exactly the sequence CI runs. If it passes locally, CI should be green.

CI additionally verifies that `go.mod` and `go.sum` are tidy, and runs GoReleaser in snapshot
mode so a broken release config fails on the pull request rather than on a tag.

```bash
make test-differential   # check the fit predicate against the real scheduler
```

That one lives in its own Go module under `test/differential`, because it depends on
`k8s.io/kubernetes` and the ~29 replace directives that module requires of every consumer.
Keeping it separate is what stops any of that reaching the module binpack ships. It runs as its
own CI job and is not part of `make check`.

**It consumes the main module through a `replace` directive, so changing the main module's
dependencies leaves it stale.** After any dependency bump, run:

```bash
cd test/differential && go mod tidy
```

CI checks this, so a forgotten tidy fails the differential job rather than going unnoticed —
but it fails on a PR that has nothing to do with dependencies, which is confusing if you do not
know why.

## Architecture rules

Two rules matter more than style, because the project's credibility rests on them.

### 1. The decision engine holds no cluster clients

`internal/engine` takes Kubernetes API objects as data and returns a `Decision`. It may name
`corev1.Pod` — that is an inert struct you can write in a test literal. It may not hold a
client, a lister or anything else that talks to a cluster, because that is what would force a
cluster into its tests.

Enforced by a `depguard` rule in `.golangci.yml` denying `k8s.io/client-go`,
`sigs.k8s.io/controller-runtime` and `k8s.io/kubectl`, so a violating import fails CI rather
than being caught in review.

Objects reaching the engine are **read-only**. The controller passes pointers into a shared
informer cache, and writing to one corrupts it for every other consumer.

The reasoning is in [ADR-0008](docs/design/adr-0008-engine-uses-api-types.md). It guarantees
`binpack explain` describes exactly what the controller will do, and keeps the decision logic
testable without a cluster.

### 2. Engine changes are test-first

Every rule in the decision procedure is a table-driven test over a hand-built `Snapshot`. Write
the failing case first, then the code. The test table is the readable specification of the
decision procedure, and it should be possible to understand what binpack does by reading it.

This discipline applies to `internal/engine`. Frontend wiring — CLI commands, controller setup,
Helm templates — gets ordinary coverage without the test-first requirement.

### 3. `fit` is the risky package, not a boring one

It is tempting to treat the Kubernetes-facing code as plumbing. It is not. Every defect found in
design review so far was the same shape — a resource not accounted for, a pod class not
recognised, a request computed differently from how the scheduler computes it. A wrong answer
there produces a *confidently wrong* decision that no amount of engine testing can catch,
because the engine's arithmetic will be perfectly correct on a bad input.

So `internal/fit` is tested against a real `kube-scheduler`, and upstream Kubernetes libraries
are preferred over hand-rolled equivalents even when the hand-rolled version looks obviously
correct. See [ADR-0006](docs/design/adr-0006-scheduler-fidelity.md).

### 4. Test fixtures: object mothers, then builders

Mothers name archetypes (`mother.SmallNode()`), builders customise them
(`mother.SmallNode().WithTaint(...)`), and mothers return builders so the two compose. Tests
should state the one thing they are about, not restate an entire API object — the test table is
the specification of the decision procedure, so it has to stay readable.

## Design decisions

Non-obvious decisions are recorded as ADRs in [`docs/design/`](docs/design/). If you are
proposing something that contradicts one, that is fine — but say so, and say why, so the ADR can
be superseded rather than quietly ignored.

New ADRs are numbered sequentially and follow the existing shape: context, decision,
alternatives, consequences.

## Documentation

Documentation lives in `docs/` and is split by purpose:

- `docs/design/` — ADRs and specifications. Why the system is the way it is.
- `docs/reference/` — configuration fields, metrics, RBAC. Look-up material.
- `docs/how-to/` — task-oriented, step-by-step guides.
- `docs/explanation/` — background on the underlying Kubernetes behaviour.

The README carries a table of contents linking these. Please keep it current in the same PR as
the change — a table of contents that lags is worse than none.

`notes/` is gitignored and holds private working material. Nothing in it is published, and
nothing in the repository should reference it.

## Pull requests

Keep them reviewable. A PR that does one thing is easier to reason about than one that does
four, and this project is being built as a sequence of small, self-contained steps
deliberately.

CI must be green before merge.

## Licence

By contributing, you agree that your contributions are licensed under
[Apache-2.0](LICENSE).
