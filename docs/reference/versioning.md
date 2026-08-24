# Versioning and releases

## What a version number covers

One tag produces everything: the binaries, the container image, and the Helm chart. The chart's
`version` and `appVersion` are stamped from the tag rather than maintained by hand, so
`binpack:0.1.0` is always installed by chart `0.1.0` and there is no compatibility matrix to
keep.

That is deliberate. The chart carries the RBAC the binary needs, and this repository has twice
shipped a role that disagreed with the code — once granting a permission the software never
used, once omitting one it could not start without. Locking the two together makes that class of
mistake impossible rather than merely unlikely.

```bash
helm install binpack oci://ghcr.io/motleyhand/charts/binpack \
  --version 0.1.0 --namespace binpack-system --create-namespace
```

| Artifact | Where |
|---|---|
| Container image | `ghcr.io/motleyhand/binpack:<version>`, `linux/amd64` and `linux/arm64` |
| Helm chart | `oci://ghcr.io/motleyhand/charts/binpack`, version `<version>` |
| Binaries | GitHub releases, Linux and macOS, amd64 and arm64 |

`latest` is only ever moved by a non-prerelease tag. A release candidate does not claim it.

## What is public

These are the names people build on, and they are treated as an interface rather than as
implementation:

| Surface | Example |
|---|---|
| API group | `binpack.motleyhand.com/v1alpha1` |
| Configuration fields | `policy.feasibility.reserveForLargestPod` |
| Node annotations and labels | `binpack.motleyhand.com/skip`, `binpack.motleyhand.com/draining` |
| Metric names and label values | `binpack_drainable_nodes`, `verdict="infeasible"` |
| Diagnostic codes | `pdb-zero-disruptions` |
| Event reasons and actions | `WouldDrain`, `Consolidate` |
| `--output json` field names | `code`, `verdict`, `freesNothing` |
| Exit codes | `0`, `1`, `2` from `diagnose --fail-on` |

An alert that silently stops firing because a series was renamed is worse than no alert, so a
rename is a breaking change even when nothing else about the behaviour moves.

Prose is **not** public: summaries, fixes, log messages and the reasons in `explain` may be
reworded at any time. That is exactly why every one of them has a stable code beside it.

## What 0.x means

While the major version is zero, a **minor** bump may break any of the above, and a **patch**
bump may not.

That is not semver's letter — semver says 0.x may break anything — but it is what the number is
being used to mean here, and [the changelog](../../CHANGELOG.md) says which. In practice the
surfaces above are already documented in detail and tested in both directions against their
references, so breaking one is a decision rather than an accident.

The version stays at 0.x until binpack can act on its own decisions and has done so on somebody
else's cluster. A tool that cannot yet do the thing it is named for should not be claiming
compatibility guarantees.

## Deprecation

A public name that is going away is kept working for at least one minor release, emitted
alongside its replacement, and listed in [the changelog](../../CHANGELOG.md) under a **Deprecated**
heading. A name that has gone appears under **Breaking**, with what to do instead. Nothing is
removed in a patch.

The changelog is [CHANGELOG.md](../../CHANGELOG.md) in the repository root, and it is the
normative record: it is written as the change lands, in the pull request that makes it. The
GitHub release body is generated from commit subjects and is a convenience, not the promise —
a subject line is not a heading that says a name is going away.

Where a name is superseded by a different design rather than renamed, the decision is recorded as
an ADR in [`docs/design/`](../design/) and the old name's entry points at it. Contradicting an
earlier ADR is fine; superseding it silently is not.

## Cutting a release

Tagging is the whole procedure:

```bash
git tag -a v0.1.0 -m "v0.1.0"
git push origin v0.1.0
```

The workflow re-runs the full `make check` before publishing anything. A tag can be pushed hours
and several merges after the branch was last green, and a release built from a commit nobody
verified is the one kind of mistake that cannot be taken back — so it is verified again rather
than trusted.

The GitHub release is created as a **draft**. Everything is built and pushed by the time anyone
reads it; publishing is the last human step, and the only one here that is irreversible.

### Push a tag, do not create a release

Creating a release in GitHub's web UI **publishes it immediately** and creates the tag as a side
effect. The tag then triggers this workflow, which arrives to find a release that already
exists and is already published.

With immutable releases enabled that is fatal: publishing freezes a release, so no assets can be
attached, and the run fails after building everything with a registry error that says nothing
about the cause. Without immutability it merely produces a release nobody drafted.

The workflow refuses up front rather than discovering this at the upload, but the fix is to push
the tag and let the workflow own the release. Immutability is worth keeping — it is doing the
right thing, and freezing a *complete* release is the point.

v0.1.1 was lost this way: its image reached the registry, its chart and binaries did not, and the
version was skipped rather than reused, since a published release cannot be refilled.
