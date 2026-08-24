# Changelog

What changed in each release, and which of it will bite you.

[docs/reference/versioning.md](docs/reference/versioning.md) defines what a version number
promises: which names are public, that a **minor** bump may break one of them while binpack is
0.x, and that a **patch** bump may not. This file is where a break is named. A deprecated name
appears under **Deprecated** for at least one minor release before it goes, still working and
emitted alongside its replacement; a name that has actually gone appears under **Breaking**, with
what to do instead.

Entries are grouped as Keep a Changelog suggests — Breaking, Added, Changed, Deprecated, Removed,
Fixed, Security — and only the headings a release needs are present.

## [Unreleased]

The release in preparation is **v0.3.0**, and it is where binpack starts telling the truth about
itself: it installs, and it drains. The README and three reference pages said otherwise for four
tags after both had stopped being true.

This section is filled in as changes land and finalised when the tag is cut. It is not yet
complete.

### Added

- `discovery.autoscalerNamespace`, so the cluster-autoscaler's status ConfigMap is read from the
  namespace the autoscaler actually runs in rather than from `kube-system` by assumption. The
  chart's Role is bound in the same namespace.
- `discovery.nodeGroups`, for clusters where no node label carries the autoscaler's node-group
  identifier and the mapping has to be given explicitly.
- `policy.autoscaler.skipNodesWithLocalStorage` and `policy.autoscaler.skipNodesWithSystemPods`,
  which mirror the cluster-autoscaler flags of the same names, so binpack stops proposing drains
  the autoscaler would refuse to finish.
- Events on the node when binpack decides *nothing*, so an operator can tell "binpack looked and
  found nothing to do" from "binpack is not running".

### Changed

- The drain protocol is bounded on every branch: each evaluation ends with the node deleted or
  uncordoned, and a wait on another component is bounded by that component's own reported state
  rather than by elapsed time.
- `policy.backoff.initial` and `policy.backoff.max` are read. They were parsed, validated and
  reported back by `binpack config validate`, and then the backoff arithmetic used two package
  constants instead — so a cluster configured for a five-minute initial backoff got thirty
  minutes and was told it had got five.
- `policy.drain.removalTimeout` defaults to 25 minutes rather than 15, which is the
  cluster-autoscaler's own worst case rather than a guess at it.

### Fixed

- Several places where binpack's fit predicate disagreed with the scheduler and would have
  accepted a placement the scheduler refuses: toleration comparison operators, node features a
  pod declares, and inter-pod anti-affinity across a topology domain rather than one node.
- PodDisruptionBudget arithmetic now mirrors the eviction subresource's own `canIgnorePDB` gate,
  so a node is no longer refused over a pod the API server would have deleted without consulting
  a budget at all — a replica wedged in `ContainerCreating`, for instance, which is exactly the
  node an operator most wants consolidated away.

### Security

- **Released binaries and the container image are now built with a patched Go toolchain.** `go.mod`
  pins `toolchain go1.26.7`; before this, CI resolved the compiler from the `go` directive and so
  built every artifact with go1.26.0 — the exact patch `k8s.io/*` v0.36 pushed that directive to.
  `govulncheck` run under go1.26.0 reports 19 standard-library advisories reachable from binpack's
  own code, among them `crypto/x509` certificate verification on the eviction path and `net/url`
  parsing in the client config; under 1.26.7 it reports none. Nothing else would have found this:
  the standard library is not in the dependency graph, so a Dependabot alert cannot exist for it.
  **Upgrade to get the rebuilt image**; there is no configuration change.
- The ClusterRole no longer requests `update` on `events.k8s.io/events`. binpack never issued
  one; `patch` is what the recorder uses to aggregate a repeated event, and `create` is the first
  write and the fallback. An externally managed role can drop the verb with no change in
  behaviour.

## [0.2.2] - 2026-08-18

### Fixed

- The leader-election lease is sized for a controller that evaluates once a minute, rather than
  for one that reconciles continuously.

## [0.2.1] - 2026-08-17

### Added

- `binpack.motleyhand.com/draining`, a label on the node binpack is draining, so the drain is
  visible to `kubectl get nodes -l` and to anything else watching.

### Changed

- Soundness is re-asked of the node being drained on every evaluation; preference is not. A drain
  that has become unsound stops, and one that has merely stopped being the best idea does not.
- binpack stands aside once the cluster-autoscaler has started removing the node, rather than
  continuing to act on a node that is on its way out.

## [0.2.0] - 2026-08-17

The release in which binpack acts. Before it, every build decided and reported and wrote nothing.

### Added

- The executor: cordon, annotate, evict, uncordon — the only four changes binpack makes to a
  cluster. Off unless `config.dryRun` is false *and* the chart is installed with
  `rbac.allowDraining=true`.
- The drain protocol, one step per evaluation, with the recovery state on the node itself rather
  than in process memory.
- Re-validation between evictions: the question that selected the node is asked again of that
  node before each further eviction.

### Changed

- A drain is judged by whether it is making progress, not against a wall clock, so a pod with a
  long `terminationGracePeriodSeconds` is not mistaken for a stuck one. See
  [ADR-0007](docs/design/adr-0007-drain-progress-not-deadlines.md).
- The feasibility reserve is sized by shape rather than by memory alone.

### Fixed

- The chart grants core-group `events` on the leader-election Role. Leader election announces
  itself through client-go's legacy recorder, which writes to the core group and not to the
  `events.k8s.io` API binpack reports decisions through; without it, leader election logged an
  authorization failure at startup and did not retry.
- The release workflow refuses a tag whose release is already published, and says why, rather
  than failing at the asset upload with a registry error that names no cause.

## [0.1.2] - 2026-08-16

### Fixed

- The image declares a numeric `USER 65532:65532`. A kubelet enforcing `runAsNonRoot` decides
  whether the image's user is root before starting the container and cannot resolve a name, so
  the named `nonroot` user failed to start on any cluster with that guard on.

Note that **v0.1.1 has no release**. It was created in GitHub's web UI, which publishes
immediately and freezes the release, so the workflow arrived to find a release it could not fill:
the image reached the registry and the chart and binaries did not. The version was skipped rather
than reused — see [docs/reference/versioning.md](docs/reference/versioning.md).

## [0.1.0] - 2026-08-16

First release. Dry run only: this build decided and reported, and wrote nothing to any cluster.

### Added

- The decision procedure: for one candidate node at a time, whether every pod on it would be
  schedulable elsewhere, simulated onto specific nodes rather than against aggregate free
  capacity.
- `internal/fit`, the scheduler-fidelity predicate, and the differential harness that checks it
  against the real kube-scheduler's own Filter plugins.
- `binpack explain <node>`, `binpack diagnose` and `binpack run`.
- Metrics under the `binpack_` prefix.
- A Helm chart, defaulting to dry run and read-only RBAC.
- The configuration API, `binpack.motleyhand.com/v1alpha1`.

[Unreleased]: https://github.com/motleyhand/binpack/compare/v0.2.2...HEAD
[0.2.2]: https://github.com/motleyhand/binpack/compare/v0.2.1...v0.2.2
[0.2.1]: https://github.com/motleyhand/binpack/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/motleyhand/binpack/compare/v0.1.2...v0.2.0
[0.1.2]: https://github.com/motleyhand/binpack/compare/v0.1.0...v0.1.2
[0.1.0]: https://github.com/motleyhand/binpack/releases/tag/v0.1.0
