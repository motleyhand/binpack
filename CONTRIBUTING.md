# Contributing to binpack

Thanks for looking. binpack is small on purpose, and the goal is that it stays easy to read and
easy to trust.

## Getting started

Requires Go 1.25 or newer. Once the Go scaffold lands:

```bash
make build      # binary into ./bin
make test       # go test ./...
make lint       # golangci-lint
make fmt        # gofmt
```

`make lint && make test && make build` is exactly what CI runs. If those pass locally, CI should
be green.

## Architecture rules

Two rules matter more than style, because the project's credibility rests on them.

### 1. The decision engine is pure

`internal/engine` must not import `k8s.io/*` or `sigs.k8s.io/*`. It takes a plain `Snapshot`
value and returns a `Decision` value. Conversion from Kubernetes API objects happens in
`internal/collect` and nowhere else.

This is enforced by a `depguard` rule in `.golangci.yml`, so a violating import fails CI rather
than being caught in review. If you find yourself needing a Kubernetes type inside the engine,
the right fix is almost always to add the field you need to the engine's own snapshot types.

The reason is in [ADR-0003](docs/design/adr-0003-pure-decision-engine.md): it is what guarantees
`binpack explain` describes exactly what the controller will do, and it is what makes the
decision logic testable without a cluster.

### 2. Engine changes are test-first

Every rule in the decision procedure is a table-driven test over a hand-built `Snapshot`. Write
the failing case first, then the code. The test table is the readable specification of the
decision procedure, and it should be possible to understand what binpack does by reading it.

This discipline applies to `internal/engine`. Frontend wiring — CLI commands, controller setup,
Helm templates — gets ordinary coverage without the test-first requirement.

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
