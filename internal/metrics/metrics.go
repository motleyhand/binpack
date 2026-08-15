// Package metrics publishes what binpack decided, as Prometheus series.
//
// Every name is prefixed binpack_ and is public API from the first release:
// people build alerts on these, and an alert that stops firing because a
// series was renamed is worse than no alert at all.
//
// Labels are drawn only from bounded sets — the engine's verdicts, skip codes
// and decision codes. The prose reasons are deliberately not exported here.
// They contain node and pod names, and a label whose values are unbounded
// turns a monitoring system into an outage.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/motleyhand/binpack/internal/engine"
)

// Collectors, registered with controller-runtime's registry so they are served
// on the manager's existing metrics endpoint.
var (
	evaluations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "binpack_evaluations_total",
		Help: "Evaluations completed, by outcome code.",
	}, []string{"code"})

	evaluationErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "binpack_evaluation_errors_total",
		Help: "Evaluations that could not be completed, usually a failed read of the cluster.",
	})

	evaluationDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name: "binpack_evaluation_duration_seconds",
		Help: "How long an evaluation took, from reading the cluster to reaching a decision.",
		// A read of every node, pod and budget plus a placement simulation.
		// Milliseconds on a small cluster, seconds on a large one; the top
		// bucket is where it has stopped being interactive.
		Buckets: []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10},
	})

	lastEvaluation = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "binpack_last_evaluation_timestamp_seconds",
		Help: "When the last evaluation completed. Alert on this going stale: " +
			"a binpack that has stopped deciding looks identical to one with nothing to do.",
	})

	autoscalerUp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "binpack_autoscaler_up",
		Help: "1 when a live cluster-autoscaler was found, 0 otherwise. " +
			"binpack refuses to act at 0, since a drained node would never be removed.",
	})

	nodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "binpack_nodes",
		Help: "Nodes by verdict at the last evaluation: skipped, infeasible, blocked or drainable.",
	}, []string{"verdict"})

	skipped = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "binpack_nodes_skipped",
		Help: "Nodes ruled out before simulation at the last evaluation, by reason code.",
	}, []string{"code"})

	drainable = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "binpack_drainable_nodes",
		Help: "Nodes whose entire workload was shown to fit elsewhere. " +
			"A persistent zero is the signal to alert on: it means the cluster genuinely " +
			"needs the nodes it has, and no amount of consolidation will change that.",
	})

	poolNodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "binpack_pool_nodes",
		Help: "Ready nodes in each autoscaling pool, as the cluster-autoscaler reports them.",
	}, []string{"pool"})

	poolMin = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "binpack_pool_min_nodes",
		Help: "The configured minimum size of each autoscaling pool. " +
			"binpack_pool_nodes sitting on this is why a pool is not shrinking.",
	}, []string{"pool"})

	poolMax = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "binpack_pool_max_nodes",
		Help: "The configured maximum size of each autoscaling pool.",
	}, []string{"pool"})
)

func init() {
	metrics.Registry.MustRegister(
		evaluations, evaluationErrors, evaluationDuration, lastEvaluation,
		autoscalerUp, nodes, skipped, drainable,
		poolNodes, poolMin, poolMax,
	)
}

// Failed records an evaluation that could not be completed.
//
// The gauges are left alone deliberately. They describe the last evaluation
// that reached a conclusion, and zeroing them on a failed read would turn a
// broken permission into what looks like a cluster with nothing to consolidate.
func Failed() {
	evaluationErrors.Inc()
}

// Observe records one completed evaluation.
//
// Takes the decision rather than being called from inside the engine, which
// holds no clients and no global state and is not about to acquire either
// through a metrics registry.
func Observe(s engine.Snapshot, d engine.Decision, cfg engine.Config, took float64) {
	evaluations.WithLabelValues(d.Code).Inc()
	evaluationDuration.Observe(took)
	lastEvaluation.Set(float64(s.Now.Unix()))

	live, _ := s.Autoscaler.Live(s.Now)
	autoscalerUp.Set(boolAsFloat(live))

	observeNodes(d)
	observePools(s, cfg)
}

func observeNodes(d engine.Decision) {
	// Reset before counting: a verdict or skip code that no longer applies to
	// any node must disappear rather than keep reporting the count it had when
	// it last did. Prometheus has no notion of an absent series otherwise.
	nodes.Reset()
	skipped.Reset()

	// Seeded so that a verdict nobody currently holds reports zero rather than
	// vanishing. The difference matters to an alert: "no drainable nodes" and
	// "binpack is not reporting" must not look the same.
	for _, verdict := range []string{
		engine.VerdictSkipped, engine.VerdictInfeasible,
		engine.VerdictBlocked, engine.VerdictDrainable,
	} {
		nodes.WithLabelValues(verdict).Set(0)
	}

	var canDrain float64
	for _, a := range d.Assessments {
		verdict := a.Verdict()
		nodes.WithLabelValues(verdict).Inc()

		switch verdict {
		case engine.VerdictSkipped:
			if a.SkipCode != "" {
				skipped.WithLabelValues(a.SkipCode).Inc()
			}
		case engine.VerdictDrainable:
			canDrain++
		}
	}
	drainable.Set(canDrain)
}

func observePools(s engine.Snapshot, cfg engine.Config) {
	// Same reasoning as above: a pool that has been deleted must stop being
	// reported, not freeze at its final size.
	poolNodes.Reset()
	poolMin.Reset()
	poolMax.Reset()

	// The readable pool name rather than the provider identifier: nobody
	// recognises a UUID on a dashboard or in an alert. Bounded either way —
	// pools are counted in single digits.
	names := engine.PoolNames(s, cfg)

	for _, g := range s.Autoscaler.Groups {
		label := names[g.ID]
		if label == "" {
			label = g.ID
		}
		poolNodes.WithLabelValues(label).Set(float64(g.Size()))
		poolMin.WithLabelValues(label).Set(float64(g.MinSize))
		poolMax.WithLabelValues(label).Set(float64(g.MaxSize))
	}
}

func boolAsFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
