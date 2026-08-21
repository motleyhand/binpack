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
	"strconv"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/motleyhand/binpack/internal/drain"
	"github.com/motleyhand/binpack/internal/engine"
)

// gated wraps the collectors that describe a cluster and reports nothing until
// an evaluation has actually produced them.
//
// Every replica serves its own metrics endpoint, but only the leader
// evaluates — and during a rolling update there are always two. Without this,
// a standby publishes a complete set of zeroes: no drainable nodes, no
// autoscaler, an evaluation timestamp at the epoch. Scraped, that is
// indistinguishable from a cluster in serious trouble, and the alerts in the
// reference would fire or flap depending on which pod answered.
//
// An absent series is the honest answer for a process that has not looked. It
// covers the startup window too, where even the leader has not yet decided
// anything.
type gated struct {
	evaluated atomic.Bool
	inner     []prometheus.Collector
}

func newGated(collectors ...prometheus.Collector) *gated {
	return &gated{inner: collectors}
}

func (g *gated) Describe(ch chan<- *prometheus.Desc) {
	for _, c := range g.inner {
		c.Describe(ch)
	}
}

func (g *gated) Collect(ch chan<- prometheus.Metric) {
	if !g.evaluated.Load() {
		return
	}
	for _, c := range g.inner {
		c.Collect(ch)
	}
}

// publish runs write and only then starts serving.
//
// The order is the point, which is why it is a function taking the writes
// rather than an open() anyone can call wherever they like: a scrape landing
// between the gate opening and the values being written would see a
// half-written set, and on the first evaluation that means a full set of
// zeroes — the exact reading this type exists to prevent.
func (g *gated) publish(write func()) {
	write()
	g.evaluated.Store(true)
}

// Collectors, registered with controller-runtime's registry so they are served
// on the manager's existing metrics endpoint.
var (
	evaluations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "binpack_evaluations_total",
		Help: "Evaluations completed, by outcome code.",
	}, []string{"code"})

	evaluationErrors = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "binpack_evaluation_errors_total",
		Help: "Evaluations that could not be completed: a failed read of the cluster, " +
			"or a write the API server refused.",
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

	unmodelled = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "binpack_nodes_unmodelled",
		Help: "Nodes refused because binpack could not read a pod's controller template, " +
			"and so cannot predict what its replacement would request. " +
			"A gap in what binpack models rather than a fact about the cluster: " +
			"persistently above zero means the allowlist is too narrow for this cluster.",
	})

	drainable = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "binpack_drainable_nodes",
		Help: "Nodes whose entire workload was shown to fit elsewhere. " +
			"A persistent zero is the signal to alert on: it means the cluster genuinely " +
			"needs the nodes it has, and no amount of consolidation will change that.",
	})

	// Drain outcomes. Counters rather than gauges, and ungated: they are
	// cumulative, meaningful from zero, and only the leader ever increments
	// them, so a standby serving zeroes is telling the truth.
	drainsStarted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "binpack_drains_started_total",
		Help: "Drains binpack began, by marking and cordoning a node.",
	})

	drainsCompleted = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "binpack_drains_completed_total",
		Help: "Drains that ended with the cluster-autoscaler removing the node.",
	})

	drainsAbandoned = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "binpack_drains_abandoned_total",
		Help: "Drains handed back by uncordoning the node, by reason code. " +
			"Alert on a rising rate: every one of these is churn that bought nothing.",
	}, []string{"reason"})

	// Backoff depth, not backoff per node. A node label would carry names into
	// the monitoring system, which this package deliberately does not do — and
	// the question worth alerting on is whether binpack is failing repeatedly,
	// which these answer without naming anything.
	nodesInBackoff = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "binpack_nodes_in_backoff",
		Help: "Nodes excluded from consideration because a drain of them recently failed.",
	})

	drainAttemptsMax = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "binpack_drain_attempts_max",
		Help: "The highest consecutive failed-drain count recorded on any node. " +
			"A cluster where this climbs is one where binpack cannot do its job.",
	})

	poolNodes = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "binpack_pool_nodes",
		Help: "Nodes registered and ready in each autoscaling pool, as the " +
			"cluster-autoscaler reports them.",
	}, []string{"pool"})

	poolMin = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "binpack_pool_min_nodes",
		Help: "The configured minimum size of each autoscaling pool.",
	}, []string{"pool"})

	poolMax = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "binpack_pool_max_nodes",
		Help: "The configured maximum size of each autoscaling pool.",
	}, []string{"pool"})
)

// state is everything that describes a cluster, and so everything a process
// that has not evaluated one must keep quiet about.
//
// The counters and the histogram are deliberately outside it: a replica that
// has completed no evaluations reporting zero of them is simply true, and a
// counter absent from a scrape is harder to reason about than one at zero.
var state = newGated(
	lastEvaluation, autoscalerUp, nodes, skipped, drainable, unmodelled,
	poolNodes, poolMin, poolMax, nodesInBackoff, drainAttemptsMax,
)

func init() {
	// Every abandon reason gets a zero series up front. A counter that only
	// appears once it has fired makes rate() alerts silently useless until the
	// first occurrence — which is exactly the moment somebody needed them.
	//
	// All three sources, because all three reach this counter: the reasons
	// binpack decides for itself, the skip codes revalidation can stop a drain
	// with, and the two verdicts that carry no skip code.
	for _, code := range drain.AbandonCodes() {
		drainsAbandoned.WithLabelValues(code)
	}
	for _, code := range engine.SkipCodes() {
		drainsAbandoned.WithLabelValues(code)
	}
	for _, code := range []string{engine.VerdictInfeasible, engine.VerdictBlocked} {
		drainsAbandoned.WithLabelValues(code)
	}

	metrics.Registry.MustRegister(
		evaluations, evaluationErrors, evaluationDuration,
		drainsStarted, drainsCompleted, drainsAbandoned, state)
}

// DrainStarted records a node being marked and cordoned.
func DrainStarted() { drainsStarted.Inc() }

// DrainCompleted records a drain that ended with the node removed.
func DrainCompleted() { drainsCompleted.Inc() }

// DrainAbandoned records a drain handed back, under the code that ended it.
func DrainAbandoned(reason string) { drainsAbandoned.WithLabelValues(reason).Inc() }

// Failed records an evaluation that could not be completed.
//
// The gauges are left alone deliberately. They describe the last evaluation
// that reached a conclusion, and zeroing them on a failed read would turn a
// broken permission into what looks like a cluster with nothing to consolidate.
//
// Reached by a failed write as well as a failed read. Neither stops binpack on
// its own — the next evaluation re-reads and re-decides — so this counter, and
// not the process exiting, is what says a cluster is refusing what binpack
// asks of it.
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

	state.publish(func() {
		observeNodes(d)
		observePools(s, cfg)
		observeBackoff(s)
	})
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

	var canDrain, cannotModel float64
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
		case engine.VerdictInfeasible:
			// Counted apart from an ordinary shortfall: "the workload does not
			// fit" is a fact about the cluster and "binpack cannot tell what
			// the workload is" is a gap in binpack, and they call for
			// completely different responses.
			if a.Simulation != nil && a.Simulation.Blocked != nil && a.Simulation.Blocked.NoTemplate {
				cannotModel++
			}
		}
	}
	drainable.Set(canDrain)
	unmodelled.Set(cannotModel)
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
		// Ready, not Size(). They differ while a scale-down is in progress —
		// Size() is the lower of ready and the autoscaler's target, which is
		// the number binpack compares against the floor — and a series called
		// "nodes" that quietly means "intent" is one nobody can trust.
		// Whether a pool is at its floor is reported directly, as the
		// pool-at-minimum skip code.
		poolNodes.WithLabelValues(label).Set(float64(g.Ready))
		poolMin.WithLabelValues(label).Set(float64(g.MinSize))
		poolMax.WithLabelValues(label).Set(float64(g.MaxSize))
	}
}

// observeBackoff reports how much failure binpack is currently carrying.
//
// Read from the nodes rather than counted as drains are abandoned, so a
// restarted controller reports the truth immediately: the state lives on the
// nodes precisely so it outlives the process.
func observeBackoff(s engine.Snapshot) {
	var inBackoff, deepest float64
	for _, node := range s.Nodes {
		// Two questions, two conditions. A node whose backoff has expired is a
		// candidate again but has not succeeded at anything — its attempt
		// count stands until a drain of it works, and that is the number the
		// gauge documents. Gating the depth on the deadline too would make it
		// fall to zero during every retry window and climb back only on the
		// next failure, which reads as recovery.
		if n, err := strconv.Atoi(node.Annotations[engine.AnnotationDrainAttempts]); err == nil &&
			float64(n) > deepest {
			deepest = float64(n)
		}

		until, err := time.Parse(time.RFC3339, node.Annotations[engine.AnnotationBackoffUntil])
		if err == nil && s.Now.Before(until) {
			inBackoff++
		}
	}
	nodesInBackoff.Set(inBackoff)
	drainAttemptsMax.Set(deepest)
}

func boolAsFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
