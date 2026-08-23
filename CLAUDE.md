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

make test-differential                               # fit vs the real scheduler
```

`test/differential` is a **separate Go module**. It depends on `k8s.io/kubernetes`, which
requires ~29 replace directives from every consumer, and keeping it out of the main module is
the whole point — do not add that dependency to the module binpack ships.

It consumes the main module via `replace`, so **a main-module dependency bump leaves it stale**
and fails the differential CI job on an unrelated PR. Run `cd test/differential && go mod tidy`
after any dependency change.

CI additionally verifies `go.mod`/`go.sum` are tidy and runs `goreleaser release --snapshot
--skip=publish`, so release-config breakage surfaces on the PR rather than on a tag.

## Architecture rules

These two are load-bearing. Breaking either silently destroys the property the project is sold
on.

**`internal/engine` holds no cluster clients.** It is a pure function,
`Decide(Snapshot, Config) → Decision`, taking Kubernetes API objects as data. Naming
`corev1.Pod` is fine — it is an inert struct you can write in a test literal. Holding a client
is not: that is what forces a cluster into a test and makes behaviour depend on the environment.
Enforced by the `purity` `depguard` rule, which is an **allowlist** — a list of clients we
remembered can never be complete — so any import not named in it fails CI. It covers
`internal/engine`, `internal/fit`, `internal/drain`, `api/v1alpha1` and `internal/mother`: the
middle two because each had a document claiming the property before anything held them to it,
and `mother` because the others import it from their test files, which `depguard` also checks.
**The set is closed — a guarded package may import only guarded packages** — because `depguard`
sees one hop and an unguarded intermediary is otherwise a way round the rule. Rationale:
[ADR-0008](docs/design/adr-0008-engine-uses-api-types.md), which supersedes the stricter
no-Kubernetes-imports rule of [ADR-0003](docs/design/adr-0003-pure-decision-engine.md) — that
rule was a proxy, and the mirror types it required caused most of the defects found in review.

**Objects handed to the engine are strictly read-only.** The controller path passes pointers
into a shared informer cache; writing to a Node or Pod corrupts it for every other consumer in
the process. Copy before modifying, always. This cannot be linted — it is a review rule.

**`internal/fit` is the high-risk package**, not a boring adapter. Every defect found in design
review was the same shape: a resource type unaccounted for, a pod class unrecognised, a request
computed differently from how the scheduler computes it. A wrong answer there produces a
*confidently wrong* decision that no amount of engine testing can catch. Prefer upstream
Kubernetes libraries over hand-rolled equivalents even when the hand-rolled version looks
obviously correct.

**A test oracle needs its own assertions, or it cannot fail.** The differential harness checks a
one-directional property — binpack must never accept what the scheduler refuses — and an oracle
that accepted everything would satisfy it vacuously: every binpack refusal logs as
"conservative", and the unsound direction becomes unreachable. So mirror cases assert the
expected scheduler verdict *independently of binpack*, and the generated suite fails if the
oracle refused nothing. Verified by sabotage: an always-accepting oracle must make the suite
fail, and it does.

**The differential harness's feature gates come from `NewSchedulerFeaturesFromGates`, never a
hand-written list.** Its first run reported 171 unsound placements, all one message: sidecar
containers disabled. That was the oracle modelling a cluster that has not existed since 1.33,
not a defect in `fit`. A hand-maintained gate list is a set of guesses about someone else's
defaults, and a wrong guess produces confident, wrong disagreement reports rather than an error.

**Test fixtures use object mothers plus builders.** Mothers name archetypes —
`mother.SmallNode()`, `mother.DaemonSetPod()` — so a test says what it needs in three words and
"ordinary" is defined in one place. Builders customise from there —
`mother.SmallNode().WithTaint(...)` — so a test states only the thing it is about. Mothers
return builders so the two compose. This matters more here than usual: the test table *is* the
specification of the decision procedure, so it has to stay readable.

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

**And that function answers two different questions, chosen by options rather than by argument.**
The scheduler sizes an incoming pod from spec alone and a pod already on a node from
`max(spec, actuated, allocated)` — an in-place vertical scale moves the two apart, durably, since
a memory decrease cannot be actuated below current usage. Hence `fit.ObservedRequests` for
anything read from the cluster and `fit.EffectiveRequests` for anything binpack built. One entry
point taking the options unconditionally does not work: a replacement carries the running pod's
`Status` for the sake of the status-based refusals while the reserve's probe is rebuilt without
one, so it would size the frontier from a figure the probe throws away and under-reserve by the
same amount it had been over-reserving. `ObservedRequests` computes its pod-level half *both* ways
and keeps the larger, because that option's gate is Beta rather than GA-locked and neither
constant is sound: with it on, a refused pod-level resize drops `spec` from the maximum; with it
off, the scheduler charges that `spec`.

**PDB demand aggregates across the whole drain.** Two pods matching one PDB with
`disruptionsAllowed: 1` passes a naive zero-check and then half-drains the node. And a pod
matched by *two* PDBs can never be evicted at all — the eviction subresource returns HTTP 500,
not a retryable 429.

**What binpack understands is an allowlist, never a denylist** — and it applies to pods other
than the one being placed, not just relocating pods, because inter-pod affinity is symmetric.
**That symmetric direction is not node-local.** The scheduler counts matching anti-affinity
across the whole topology domain a term names, so a zone-keyed term rejects every node in the
zone, not only the one hosting the pod that declared it. Reading a candidate's own residents is
the `kubernetes.io/hostname` special case mistaken for the general rule, and it errs by
accepting — which is why `fit` is handed an index of the cluster's terms by domain rather than
computing the answer per node. See [ADR-0006](docs/design/adr-0006-scheduler-fidelity.md).

**Every drain branch must end with the node deleted or uncordoned**, and the recovery state
lives in node annotations rather than process memory. The failure an in-memory timer guards
against and the failure that destroys it are the same class of event. "Still being drained" is
the answer that needs watching, because it is the one a branch can give for ever — and the branch
that waits on another component is the easiest place to forget, since waiting is what it is
*for*. Every such wait needs a bound drawn from that component's own reported state, never from
elapsed time: only a live cluster-autoscaler ever clears its own deletion taint, so a wait for a
dead one is a wait for nothing.

**A partially drained node is *more* attractive to the next evaluation, not less.** It has fewer
pods, and candidates are ordered least-loaded-first — so without per-node backoff, binpack
preferentially retries the node that just failed, evicting a few more pods each round. Failed
drains record exponential backoff on the node itself.

**`deletionTimestamp` is the deadline, not the start of one.** The API server sets it to
`now + gracePeriodSeconds` at the moment deletion is requested, and preserves that invariant when
a second delete shortens the period. Adding the grace period to it doubles every deadline, so a
pod wedged on a finalizer with an hour's grace reads as healthy for a second hour. The test
suite will not catch this on its own: a fixture built by the same misunderstanding cancels it
out exactly, so the mother has to model what the API server writes.

**"Node-local" means two different things, and only one of them is stable.** A DaemonSet or
mirror pod is bound to its node *by nature*; a terminating pod is bound to it *by circumstance*.
The engine's `Classify` lumps them together because for the simulation both answer "needs no
destination" — but a drain is waiting on precisely the terminating ones, and reusing that class
reports an occupied node as empty. Hence `engine.NodeBound` for the narrow question. Completed
pods are the mirror image: real objects that never leave on their own, so counting them stalls
every drain of the node they landed on.

**Slow is not stuck.** A wall-clock drain deadline cannot tell a pod with a one-hour
`terminationGracePeriodSeconds` from one wedged on a finalizer. Bound the *absence of progress*
instead, and count "terminating within its grace period" as progress so long shutdowns need no
configuration. Detect stuck positively — a pod past its termination deadline is a finalizer or a
volume problem, and saying so is far more useful than "timed out". See
[ADR-0007](docs/design/adr-0007-drain-progress-not-deadlines.md).

**Recovery reads live pod state, not the age of the progress annotation.** The annotation
records when binpack last looked, not whether the cluster is progressing — and the two come
apart exactly when the controller has been down. A restart after a twenty-minute outage during a
legitimate forty-minute shutdown must not kill a healthy drain because a timestamp looks old.

## Conventions

- **API group `binpack.motleyhand.com`**, metric prefix `binpack_`. Both are public API from the
  first release.
- **No cloud credentials, ever.** Pool bounds and autoscaler presence come from the
  `cluster-autoscaler-status` ConfigMap, which is populated even on managed control planes.
  [ADR-0004](docs/design/adr-0004-provider-agnostic-no-cloud-api.md). The namespace is
  `discovery.autoscalerNamespace`, not a constant: the autoscaler publishes into the namespace
  it runs in, and its own chart sets that to whatever you install it into. Anything reading or
  documenting that object must take the namespace from configuration — and the chart's Role has
  to be bound in the same one, which fails at runtime rather than at install.
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
