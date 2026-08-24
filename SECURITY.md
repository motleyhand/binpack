# Security policy

binpack cordons nodes and evicts pods on other people's clusters. A defect that makes it drain
the wrong node, or leave one cordoned, is a production incident somewhere — so please report
anything of that shape privately first.

## Reporting a vulnerability

Use GitHub's private vulnerability reporting:
**[report a vulnerability](https://github.com/motleyhand/binpack/security/advisories/new)**.
That opens a draft advisory only the maintainers can see, and it is the right place even if you
are not sure the thing you found is exploitable.

Please do not open a public issue for a vulnerability. Everything else — bugs, wrong decisions,
documentation that is wrong about Kubernetes — is welcome as an ordinary issue.

Useful things to include, in rough order of usefulness: the binpack version (`binpack version`),
the chart values that matter (`rbac.allowDraining`, `config.dryRun`), what binpack did, and what
you expected instead. A `binpack explain <node>` or `binpack diagnose --output json` from an
affected cluster says more than a description of it.

This is a small project. Reports are looked at as soon as they are seen, which is not the same
as a response time, and it would be dishonest to state one here.

## Supported versions

While the major version is zero, the supported version is **the latest patch of the latest
minor**, and nothing else. A fix is released as a new patch of that minor rather than backported;
[docs/reference/versioning.md](docs/reference/versioning.md) says what a version number promises,
and [CHANGELOG.md](CHANGELOG.md) says what changed in each one.

## What is in scope

The permissions binpack asks for are the shape of its blast radius, and they are documented in
full in [docs/reference/rbac.md](docs/reference/rbac.md). In summary it reads nodes, pods,
PodDisruptionBudgets and the controllers that own pods cluster-wide; with `rbac.allowDraining` it
also holds `nodes: patch` and `pods/eviction: create`. It never deletes a node — that is the
cluster-autoscaler's job — never scales a workload, and never writes to anything else.

So: anything that gets binpack to drain a node it should not have, to leave a node cordoned with
no way back, to evict a pod its PodDisruptionBudget should have protected, or to act at all while
`config.dryRun` is true, is in scope. So is anything that gets it to use a permission it does not
document.

A cluster where somebody can already edit binpack's configuration or its ClusterRole is not: at
that point they can grant themselves what binpack has without going through binpack.

## What is done about dependencies

Dependabot raises an alert and opens a pull request when an advisory is published against
something in the dependency graph, and version updates run monthly. `make vuln` runs
[govulncheck](https://pkg.go.dev/golang.org/x/vuln/cmd/govulncheck) over the module and the
toolchain, which adds reachability and covers standard-library advisories the dependency graph
does not carry; `.github/workflows/vulnerabilities.yaml` runs it weekly and on every push to
`main`.

It is deliberately not part of `make check`. That is what a release tag runs, and govulncheck
answers from the advisory database at the moment it runs rather than from the tree — so it would
block releases, and contributors, on disclosures that have nothing to do with the change in front
of them.
