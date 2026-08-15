# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

binpack is a consolidation controller for managed Kubernetes. Once per interval it asks whether
draining one specific node would leave every one of its pods schedulable elsewhere, and drains
it only when the answer is yes. The cluster-autoscaler then reaps the node.

It exists because the cluster-autoscaler never rebalances — it only removes nodes that have
*already* become nearly empty, and no amount of tuning changes that. Read
[docs/design/2026-08-15-architecture.md](docs/design/2026-08-15-architecture.md) before making
design decisions; it is the specification, and the ADRs beside it record why each choice was
made.

## Commands

```bash
make check                       # lint, test, build, smoke test — exactly what CI runs
make test                        # go test -race ./...
make lint                        # golangci-lint run
make help                        # every target

go test ./internal/version/ -run TestResolve -v      # one test
go test ./internal/cli/ -run 'TestVersion.*'         # one package, matching tests
```

CI additionally verifies `go.mod`/`go.sum` are tidy and runs `goreleaser release --snapshot
--skip=publish`, so release-config breakage surfaces on the PR rather than on a tag.

## Architecture rules

These two are load-bearing. Breaking either silently destroys the property the project is sold
on.

**`internal/engine` imports no Kubernetes libraries.** It is a pure function,
`Decide(Snapshot, Config) → Decision`, over plain values. Enforced by a `depguard` rule in
`.golangci.yml`, so a violating import fails CI. If the engine seems to need a Kubernetes type,
add the field to its own snapshot types and populate it in `internal/collect`. Rationale:
[ADR-0003](docs/design/adr-0003-pure-decision-engine.md). This is what guarantees `binpack
explain` and the controller cannot disagree, and it is why engine tests need no cluster.

**`internal/collect` and `internal/fit` are the high-risk packages**, not the boring adapter
layers. Every defect found in design review was a translation error at that boundary — a
resource type unaccounted for, a pod class unrecognised, a request computed differently from how
the scheduler computes it. A wrong number there produces a *confidently wrong* decision that no
engine unit test can catch. Prefer upstream Kubernetes libraries over hand-rolled equivalents
even when the hand-rolled version looks obviously correct, and test against real API fixtures.

## Domain traps

Each of these was got wrong at least once during design and caught in review. They are
counter-intuitive, and the code will look correct while being wrong.

**Aggregate capacity is not schedulability.** Summing free resources across nodes is the
fractional relaxation of bin-packing. Three nodes with 1GB free each do not hold a 3GB pod.
Feasibility must simulate placement onto *specific* nodes.

**A feasible packing existing is not the scheduler choosing it.** If pod A fits N1 or N2 while
pod B fits only N2, the simulation may assign A→N1 and be correct that a valid assignment
exists — and the scheduler may still put A on N2 and leave B Pending. This is why evictions are
sequential with revalidation between each. binpack makes a scale-up unlikely and immediately
detectable, never impossible; do not write documentation claiming otherwise.

**DaemonSet and mirror pods are node-local: never simulated, never evicted.** Counting them as
needing relocation inflates the requirement on every node. Evicting them livelocks the drain —
the controller recreates the pod on the same (still-existing, cordoned) node. This is why
`kubectl drain` requires `--ignore-daemonsets`.

**Overprovisioning pause pods are NOT expendable.** They must sit at or above the autoscaler's
`--expendable-pods-priority-cutoff` (-10) or the buffer never replenishes — so by the
autoscaler's accounting they are real workload. Excluding them makes every drain of a
buffer-holding node trigger the scale-up it promised to avoid. Only pods *strictly below* the
cutoff are excluded.

**`pods` is in `status.allocatable` but never in a pod's requests.** A uniform
subtract-request-from-remaining loop never consumes a pod slot, so the simulation would pack
unlimited pods onto a node capped at 110. `internal/collect` synthesises `pods: 1` into every
pod's request map.

**Effective requests ≠ container requests.** The scheduler reserves the larger of the
regular-container sum and the init-container peak, keeps native sidecars in the running total,
and adds RuntimeClass `spec.overhead`. Use `k8s.io/component-helpers/resource.PodRequests`.

**PDB demand aggregates across the whole drain.** Two pods matching one PDB with
`disruptionsAllowed: 1` passes a naive zero-check and then half-drains the node. And a pod
matched by *two* PDBs can never be evicted at all — the eviction subresource returns HTTP 500,
not a retryable 429.

**What binpack understands is an allowlist, never a denylist** — and it applies to pods on
*destination* nodes too, not just relocating pods, because inter-pod affinity is symmetric. See
[ADR-0006](docs/design/adr-0006-scheduler-fidelity.md).

**Every drain branch must end with the node deleted or uncordoned**, and the recovery state
lives in node annotations rather than process memory. The failure an in-memory timer guards
against and the failure that destroys it are the same class of event.

## Conventions

- **API group `binpack.motleyhand.com`**, metric prefix `binpack_`. Both are public API from the
  first release.
- **No cloud credentials, ever.** Pool bounds and autoscaler presence come from the
  `cluster-autoscaler-status` ConfigMap in `kube-system`, which is populated even on managed
  control planes. [ADR-0004](docs/design/adr-0004-provider-agnostic-no-cloud-api.md).
- **Dependencies are added as a PR first needs them**, not upfront. Note that adding `k8s.io/*`
  will push the `go` directive past 1.25 — that is a deliberate decision, not an accident to
  accept silently.
- **Non-obvious decisions become ADRs** in `docs/design/`, numbered sequentially, following the
  existing shape: context, decision, alternatives, consequences. Contradicting an ADR is fine;
  superseding it explicitly is required.
- **Docs are split by purpose**: `design/` (why), `reference/` (look-up), `how-to/`
  (task-oriented), `explanation/` (background Kubernetes behaviour). Keep the README's table of
  contents current in the same PR as the change.
- **`notes/` is gitignored and private.** It holds working material that must never be
  published, and nothing in the repository should reference it.

## Working with the maintainer

- **Open PRs as drafts** (`gh pr create --draft`). Codex review triggers on open and on
  draft→ready, and the maintainer wants their own findings in first. They un-draft manually —
  never re-draft a PR that is already open for review.
- **The maintainer merges.** Branch, commit, push, open the PR; do not merge.
- Keep PRs reviewable and self-contained. This project is deliberately built as a sequence of
  small steps.

## Writing style for user-facing docs

The documentation's credibility is the product. Two rules that were learned the hard way:

- **Do not claim knowledge of the reader's cluster.** "You might not need this" rather than "you
  don't need this". Where possible, point at the read-only command that answers the question
  instead of asserting on their behalf.
- **Verify incidental claims about Kubernetes behaviour.** The dangerous errors are not the ones
  about binpack, which get scrutinised — they are the confident asides about how Kubernetes
  works. Six of them shipped in one PR and had to be corrected.
