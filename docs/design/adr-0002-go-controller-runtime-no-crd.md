# ADR-0002: Go with controller-runtime, configuration as a versioned file

- **Status:** accepted
- **Date:** 2026-08-15

## Context

binpack needs to periodically read every node, pod and PodDisruptionBudget in a cluster, decide
whether one node can be drained, and act. Three implementation shapes were considered.

A **shell script in a CronJob** would be trivial to write and audit — the whole thing is
`kubectl`, `jq` and arithmetic — and a working prototype exists. But state is genuinely hard in
that shape: cooldowns and anti-thrash memory have nowhere to live between runs. Resource
arithmetic in bash is unpleasant and hard to test. And each run re-reads the entire cluster
through the API server, which is exactly what produced the descheduler's client-side throttling
during evaluation.

A **Go operator** built with kubebuilder would give the full treatment: a custom resource,
webhooks, the standard scaffold. But binpack has no resource whose lifecycle it manages. It
makes one cluster-wide judgement on a timer. A CRD would be configuration wearing a costume.

## Decision

Write it in Go using `sigs.k8s.io/controller-runtime`, but **without** kubebuilder's custom
resource machinery.

Use the controller-runtime **manager** for what it genuinely provides:

- **A shared informer cache.** This is the main reason. The decision procedure reads every pod
  in the cluster on every pass. Backed by watch-maintained in-memory caches, that is
  effectively free, so binpack can evaluate every 60 seconds instead of hourly.
- Leader election, so running two replicas is safe.
- A Prometheus `/metrics` endpoint and `/healthz`, `/readyz` probes.
- Graceful shutdown, so a drain in progress is not abandoned mid-eviction.

The periodic evaluation is registered as a `manager.Runnable` that ticks on an interval. There
is no `Reconcile` method, because there is no object to reconcile.

Configuration is a **versioned Go API type** in `api/v1alpha1`, serialised as YAML and mounted
from a ConfigMap:

```yaml
apiVersion: binpack.motleyhand.com/v1alpha1
kind: BinpackConfig
```

This is the pattern the Kubernetes descheduler uses for its policy file, and the important
property is that the schema **is** a CRD spec. It has an API group, a version, a kind, proper
JSON tags, defaulting and validation. Promoting it to a real CustomResourceDefinition later is
additive — the same Go types, registered with a scheme — rather than a breaking migration for
everyone who wrote a config file.

## Consequences

- Installing binpack requires no CRD. `helm install` creates a Deployment, a ServiceAccount, an
  RBAC role and a ConfigMap, and uninstalling removes all four: no schema stays registered in
  the API server, and no object of a kind binpack invented is left for someone to find. What can
  outlive the release is state on nodes binpack has drained — the drain label and annotations,
  written when a drain starts and cleared when it ends. That is state on objects the cluster
  already owns rather than a kind of binpack's own, so it is a node to uncordon rather than a
  resource to garbage-collect.
- The CLI (`explain`, `diagnose`) deliberately does **not** start a manager. Starting informers
  and waiting for cache sync to answer one question would be slow and surprising. The CLI makes
  one-shot `List` calls against the user's kubeconfig instead. Both paths feed the same decision
  engine ([ADR-0003](adr-0003-pure-decision-engine.md)), so they cannot disagree.
- Configuration validation must be written by hand, since there is no API server enforcing an
  OpenAPI schema. Invalid configuration must fail at startup with a clear message rather than
  at the first evaluation.
- The shell prototype is not shipped. It is missing the PodDisruptionBudget pre-check, handles
  memory only, and uses `kubectl drain --force`, which silently deletes pods that have no
  controller. Publishing it would invite people to run it.
