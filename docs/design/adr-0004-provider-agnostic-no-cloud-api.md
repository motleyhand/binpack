# ADR-0004: Provider-agnostic, with no cloud API credentials

- **Status:** accepted
- **Date:** 2026-08-15

## Context

binpack needs two pieces of information that are not part of the Kubernetes API:

1. **Which node pool a node belongs to**, so that only autoscale-pool nodes are drain
   candidates. Draining a node the autoscaler cannot delete is pure churn.
2. **The pool's configured minimum size**, because a pool already at its minimum will simply
   replace any node that is drained.

Both are properties of the cloud provider's control plane. The obvious way to get them is to
call the provider's API — for DigitalOcean, list the cluster's node pools and read `min_nodes`.

The problem was originally observed on DOKS, and the pool label
`doks.digitalocean.com/node-pool` is provider-specific. But the underlying behaviour is not:
every managed Kubernetes service that ships a constrained cluster-autoscaler has this problem.

## Decision

Take neither piece of information from a cloud API. binpack requires **no cloud credentials of
any kind**.

- **Pool membership** comes from a node label whose key is configurable. Every managed provider
  applies one; `doks.digitalocean.com/node-pool` is merely the default. Linode, Vultr,
  Scaleway, GKE, EKS and AKS all have an equivalent, and these ship as documented configuration
  presets rather than as Go code.
- **Pool minimum size** is a number in binpack's configuration, stated by the operator.

The only permission binpack needs is a Kubernetes RBAC role in the cluster it runs in.

## Consequences

### Benefits

- Works on any managed Kubernetes on day one, with no per-provider implementation, no provider
  SDK dependency, and no vendor to keep up with.
- No API token to create, mount, rotate or leak. For a tool asking permission to drain nodes,
  "it holds no cloud credentials" is a materially easier conversation than any amount of
  reassurance about how carefully the token is handled.
- Adding a provider means writing a paragraph of documentation, not shipping a release.

### Costs, and why they are acceptable

The configured minimum can drift from reality. If someone raises the pool minimum in the
DigitalOcean console and does not update binpack's configuration, binpack believes it has more
room to shrink than it does.

The failure is bounded and visible. binpack drains a node; the autoscaler declines to delete
it because the pool is at its minimum; the node is left cordoned and its workload has moved.
Nothing is destroyed, no capacity is lost, and the cluster keeps working. binpack's own metrics
show a drain that produced no node reduction.

Mitigations, in order of appearance:

- Report observed pool size alongside configured minimum in `explain` and `diagnose`, so drift
  is visible before it matters.
- Emit a metric for drains that did not reduce node count within the expected window, which is
  the direct signal that configuration is stale.
- Consider an optional, opt-in cloud API integration later, purely to *verify* configured
  minimums rather than to replace them. It would remain optional, and binpack would work
  identically without it.

### Not decided here

Whether to eventually offer provider-specific packages for people who want auto-discovery. That
is a v2 question, and [ADR-0006](adr-0006-why-not-a-karpenter-doks-provider.md) argues the
broader audience is served better by staying credential-free.
