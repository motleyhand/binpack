# Fix a silently broken Metrics API

Slightly off-topic for a consolidation tool, but it is the kind of failure that costs an
afternoon and is almost impossible to find by searching, so it is written down here.

## Symptoms

`kubectl top` reports no data:

```bash
kubectl top nodes
# error: metrics not available yet
```

But the API itself is healthy — it returns `200 OK` with an empty list rather than an error:

```bash
kubectl get --raw "/apis/metrics.k8s.io/v1beta1/nodes"
# {"kind":"NodeMetricsList","items":[]}
```

And your dashboards look fine. Lens, Grafana, anything reading Prometheus directly shows
perfectly good graphs, because it never touches the Metrics API at all.

That combination — healthy endpoint, empty results, working dashboards — is what makes this hard
to diagnose. Nothing is broken enough to alert.

## What else is quietly not working

Worth knowing, because the blast radius is larger than a missing `kubectl top`:

- **HPA scaling on `cpu` or `memory`** silently never triggers. The HPA reports unknown metrics
  and does nothing.
- **The Vertical Pod Autoscaler recommender**, which reads usage samples from this same API. It
  produces no recommendations rather than an error, so it looks installed and idle rather than
  broken.
- Anything else reading `metrics.k8s.io`.

Not affected, which narrows the search:

- **KEDA scalers using external triggers** — AMQP, cron, Prometheus queries. These bypass the
  Metrics API entirely. Only KEDA's `cpu` and `memory` trigger types are affected.

So a cluster can appear to be autoscaling correctly on KEDA while every CPU-based HPA in it is
inert and VPA has been silently recommending nothing for months.

## The cause: a label mismatch

If you run `prometheus-adapter` as the `metrics.k8s.io` backend, its `nodeQuery` filters on a
label named `node`.

Metrics from `node-exporter`, by default, carry only `instance` — a host-and-port string like
`10.110.0.3:9100`. There is no `node` label to match on, so the query returns zero rows, so the
adapter returns an empty list, so `kubectl top` reports no data. Every component involved is
behaving correctly.

## The fix

Relabel node-exporter's metrics to carry a `node` label, using the service-discovery meta-label
that already holds the node name. In `kube-prometheus-stack` values:

```yaml
prometheus-node-exporter:
  prometheus:
    monitor:
      relabelings:
        - sourceLabels: [__meta_kubernetes_pod_node_name]
          targetLabel: node
```

Check the kubelet/cAdvisor ServiceMonitor for the same problem while you are there. Pod-level
metrics failing while node-level metrics work — or the reverse — points at exactly this, in one
of the two scrape configs.

## Do not run two metrics providers

Only one component can serve the `metrics.k8s.io` APIService. If you install `metrics-server`
alongside `prometheus-adapter`, they fight over the same registration and the winner is
whichever reconciled last.

Pick one. If you already run Prometheus, `prometheus-adapter` avoids a second collection
pipeline. If you don't, `metrics-server` is much simpler and is the right default.

## Relevance to binpack

Limited, deliberately, and worth stating so nobody assumes this is a prerequisite.

binpack's feasibility arithmetic runs on **requests**, not usage, because requests are what the
scheduler honours — a pod using 200MB but requesting 1GB needs 1GB of room wherever it lands. So
a broken Metrics API does not affect any decision binpack makes.

Where it matters is everything you would do *before* reaching for binpack. The gap between
requested and used is what tells you your requests are inflated, and right-sizing them is
[the highest-leverage fix](quick-wins-before-installing-binpack.md) available. You need
`kubectl top` working to see that gap by hand — and the VPA recommender, which is the tool that
does it properly, cannot work at all without this API. So while a broken Metrics API changes
none of binpack's decisions, it blocks the fix that usually matters more.
