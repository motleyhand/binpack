# ADR-0005: Name, licence and API group

- **Status:** accepted
- **Date:** 2026-08-15

## Context

Four identifiers are cheap to choose now and mechanically annoying to change later: the
project name, the Go module path, the API group, and the licence. They propagate into the
container image name, the Helm chart name, the Prometheus metric prefix, the node annotation
domain, and every document that references any of them.

The repository began life as `node-killer`.

## Decision

### Name: `binpack`

`node-killer` was rejected. The tool's entire value proposition is that it is *conservative* —
it refuses to drain unless the arithmetic proves the workload fits elsewhere. A name promising
destruction argues against that message with exactly the cautious audience the project needs,
and "should I run something called node-killer against production?" is a real objection that
costs adoption.

`binpack` is the literal term for what the tool does. Kubernetes' own scheduler documentation
uses bin-packing to describe the `MostAllocated` strategy, so it lands precisely with people
who have already read enough to have this problem. Seven characters, no hyphen, clean in every
downstream identifier.

Alternatives considered: `nodepack` (good, slightly overloaded — bundlers, Packer, packaging),
`nodedefrag` (best explanatory metaphor, clunky as an identifier; note that the shorter
`nodefrag` reads as node + *frag*, gaming slang for killing, which inverts the message), and
`compactor` (technically precise, but generic and says nothing about Kubernetes).

The trade-off accepted: `binpack` assumes the reader knows the term. It is therefore a weaker
headline for writing aimed at people who have not yet diagnosed the problem, which is a
documentation concern rather than a naming one.

### Module path: `github.com/motleyhand/binpack`

### API group: `binpack.motleyhand.com`

Kubernetes requires annotation prefixes and API groups to be DNS subdomains, and convention is
to use one you control. This gives:

- Config files: `apiVersion: binpack.motleyhand.com/v1alpha1`
- Node opt-out annotation: `binpack.motleyhand.com/skip: "true"`

A dedicated domain (`binpack.dev` or similar) was considered and deferred. It would be shorter
in annotations and give the project an independent identity, but it costs a registration and
detaches the project from the MotleyHand umbrella that already anticipates multiple projects.
Should the project acquire its own domain later, the old API group must continue to be accepted
for at least one release.

### Metric prefix: `binpack_`

Prometheus convention is a single lowercase word with no separators, e.g.
`binpack_nodes_drained_total`. This is a direct consequence of the name and is one reason a
hyphenated name was rejected: `node_killer_` reads as an afterthought.

### Licence: Apache-2.0

The Kubernetes ecosystem default — Kubernetes itself, controller-runtime, Helm, the descheduler
and Karpenter are all Apache-2.0. Crucially it includes an explicit patent grant, which is why
corporate legal review generally waves it through without escalation. For a tool whose purpose
is to be adopted inside companies, that matters more than the marginal simplicity of MIT.

AGPL-3.0 was considered and rejected: many organisations ban it outright, and the project's
goal is reach.

## Consequences

- The GitHub repository was renamed from `node-killer` to `binpack`. GitHub redirects the old
  path, but nothing should rely on that.
- Every identifier above is treated as public API from the first release. Changing the API group
  or metric names later requires a deprecation period.
- The name assumes domain knowledge, so the README must state the problem in plain language
  before it ever uses the phrase "bin-packing".
