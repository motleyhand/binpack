package metrics

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	corev1 "k8s.io/api/core/v1"

	"github.com/motleyhand/binpack/internal/drain"
	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

var now = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

const (
	groupIDLabel = "doks.digitalocean.com/node-pool-id"
	poolLabel    = "doks.digitalocean.com/node-pool"
)

func config() engine.Config {
	return engine.Config{NodeGroupIDLabel: groupIDLabel, PoolNameLabel: poolLabel}
}

// snapshot builds a cluster whose nodes carry the pool labels, which is where
// the readable pool name actually comes from — the autoscaler's status has
// only the provider's identifier.
func snapshot(groups ...engine.NodeGroup) engine.Snapshot {
	var nodes []*corev1.Node
	for _, g := range groups {
		if name := readableName[g.ID]; name != "" {
			nodes = append(nodes, mother.SmallNode("node-"+g.ID, mother.InPool(name, g.ID)))
		}
	}
	return engine.Snapshot{
		Nodes: nodes,
		Now:   now,
		Autoscaler: engine.Autoscaler{
			Running:   true,
			LastProbe: now.Add(-10 * time.Second),
			Groups:    groups,
		},
	}
}

// readableName is what the nodes of each test pool are labelled with. A pool
// absent here has no nodes, and so nothing to take a name from.
var readableName = map[string]string{"da8977ba-244f": "pool-4g"}

func assess(verdict, skipCode string) engine.NodeAssessment {
	a := engine.NodeAssessment{Node: mother.SmallNode("n")}
	switch verdict {
	case engine.VerdictSkipped:
		a.Skipped, a.SkipCode = true, skipCode
	case engine.VerdictInfeasible:
		a.Simulation = &engine.Simulation{Feasible: false}
	case engine.VerdictBlocked:
		a.Simulation = &engine.Simulation{Feasible: true}
		a.Blockers = []engine.EvictionBlocker{{Code: engine.BlockedBarePod}}
	default:
		a.Simulation = &engine.Simulation{Feasible: true}
	}
	return a
}

func TestObserveCountsNodesByVerdict(t *testing.T) {
	Observe(snapshot(), engine.Decision{
		Code: engine.CodeNoneFeasible,
		Assessments: []engine.NodeAssessment{
			assess(engine.VerdictSkipped, engine.SkipCordoned),
			assess(engine.VerdictSkipped, engine.SkipPoolAtMinimum),
			assess(engine.VerdictInfeasible, ""),
			assess(engine.VerdictBlocked, ""),
			assess(engine.VerdictDrainable, ""),
		},
	}, config(), 0.02)

	for verdict, want := range map[string]float64{
		engine.VerdictSkipped:    2,
		engine.VerdictInfeasible: 1,
		engine.VerdictBlocked:    1,
		engine.VerdictDrainable:  1,
	} {
		if got := testutil.ToFloat64(nodes.WithLabelValues(verdict)); got != want {
			t.Errorf("binpack_nodes{verdict=%q} = %v, want %v", verdict, got, want)
		}
	}
	if got := testutil.ToFloat64(drainable); got != 1 {
		t.Errorf("binpack_drainable_nodes = %v, want 1", got)
	}
	if got := testutil.ToFloat64(skipped.WithLabelValues(engine.SkipCordoned)); got != 1 {
		t.Errorf("skip code cordoned = %v, want 1", got)
	}
}

func TestEveryVerdictReportsZeroRatherThanVanishing(t *testing.T) {
	// The difference matters to an alert: "no drainable nodes" and "binpack is
	// not reporting" must not look the same. An absent series reads as the
	// second.
	Observe(snapshot(), engine.Decision{
		Code:        engine.CodeNoCandidates,
		Assessments: []engine.NodeAssessment{assess(engine.VerdictSkipped, engine.SkipCordoned)},
	}, config(), 0.01)

	// Gathered before anything reads the child by label: ToFloat64 on a
	// GaugeVec child *creates* it, so asserting through that would prove only
	// that the assertion ran.
	out := gather(t)

	if !strings.Contains(out, `binpack_nodes{verdict="drainable"} 0`) {
		t.Errorf("the drainable series is absent rather than zero:\n%s", out)
	}
	for _, verdict := range []string{
		engine.VerdictInfeasible, engine.VerdictBlocked, engine.VerdictDrainable,
	} {
		if !strings.Contains(out, `verdict="`+verdict+`"`) {
			t.Errorf("no series for verdict %q:\n%s", verdict, out)
		}
	}
}

func TestGaugesForgetWhatNoLongerApplies(t *testing.T) {
	// Prometheus has no notion of an absent series once one exists, so a skip
	// code that stops applying would otherwise keep reporting the count it had
	// when it last did — and an operator would go on believing nodes are
	// cordoned long after they were uncordoned.
	Observe(snapshot(), engine.Decision{
		Code: engine.CodeNoCandidates,
		Assessments: []engine.NodeAssessment{
			assess(engine.VerdictSkipped, engine.SkipCordoned),
			assess(engine.VerdictSkipped, engine.SkipCordoned),
		},
	}, config(), 0.01)
	if got := testutil.ToFloat64(skipped.WithLabelValues(engine.SkipCordoned)); got != 2 {
		t.Fatalf("setup: cordoned = %v, want 2", got)
	}

	Observe(snapshot(), engine.Decision{
		Code:        engine.CodeNoneFeasible,
		Assessments: []engine.NodeAssessment{assess(engine.VerdictDrainable, "")},
	}, config(), 0.01)

	// The exact series, not the bare code. Abandonment reasons share this
	// vocabulary and are seeded at zero deliberately, so a substring search
	// over the whole scrape now matches those too and would pass regardless of
	// what the gauge did.
	if strings.Contains(gather(t), `binpack_nodes_skipped{code="`+engine.SkipCordoned+`"}`) {
		t.Errorf("a skip code that no longer applies is still reported:\n%s", gather(t))
	}
}

func TestPoolsAreLabelledByNameAndForgottenWhenRemoved(t *testing.T) {
	Observe(snapshot(
		engine.NodeGroup{ID: "da8977ba-244f", MinSize: 1, MaxSize: 10, Ready: 3},
		engine.NodeGroup{ID: "unnamed-id", MinSize: 2, MaxSize: 4, Ready: 2},
	), engine.Decision{Code: engine.CodeNoneFeasible}, config(), 0.01)

	out := gather(t)
	// The human-readable name, since a provider's group ID is not something
	// anyone recognises on a dashboard.
	if !strings.Contains(out, `binpack_pool_nodes{pool="pool-4g"} 3`) {
		t.Errorf("pool not reported by name:\n%s", out)
	}
	if !strings.Contains(out, `binpack_pool_min_nodes{pool="pool-4g"} 1`) {
		t.Errorf("pool minimum missing:\n%s", out)
	}
	// Falls back to the ID rather than reporting an empty label.
	if !strings.Contains(out, `binpack_pool_nodes{pool="unnamed-id"} 2`) {
		t.Errorf("unnamed pool not reported by ID:\n%s", out)
	}

	Observe(snapshot(engine.NodeGroup{ID: "da8977ba-244f", MinSize: 1, MaxSize: 10, Ready: 3}),
		engine.Decision{Code: engine.CodeNoneFeasible}, config(), 0.01)

	if strings.Contains(gather(t), "unnamed-id") {
		t.Errorf("a deleted pool is still reported:\n%s", gather(t))
	}
}

func TestAFailedEvaluationLeavesTheGaugesAlone(t *testing.T) {
	// They describe the last evaluation that reached a conclusion. Zeroing
	// them on a failed read would turn a broken permission into what looks
	// like a cluster with nothing left to consolidate — the one state binpack
	// most needs to distinguish.
	Observe(snapshot(), engine.Decision{
		Code:        engine.CodeNoneFeasible,
		Assessments: []engine.NodeAssessment{assess(engine.VerdictDrainable, "")},
	}, config(), 0.01)

	before := testutil.ToFloat64(drainable)
	errorsBefore := testutil.ToFloat64(evaluationErrors)

	Failed()

	if got := testutil.ToFloat64(drainable); got != before {
		t.Errorf("drainable = %v after a failed read, want it unchanged at %v", got, before)
	}
	if got := testutil.ToFloat64(evaluationErrors); got != errorsBefore+1 {
		t.Errorf("errors = %v, want %v", got, errorsBefore+1)
	}
}

func TestOutcomeAndAutoscalerAreRecorded(t *testing.T) {
	Observe(snapshot(), engine.Decision{Code: engine.CodeDrain, Action: engine.Drain}, config(), 0.01)
	if got := testutil.ToFloat64(evaluations.WithLabelValues(engine.CodeDrain)); got < 1 {
		t.Errorf("no evaluation counted for code %q", engine.CodeDrain)
	}
	if got := testutil.ToFloat64(autoscalerUp); got != 1 {
		t.Errorf("binpack_autoscaler_up = %v with a live autoscaler, want 1", got)
	}

	dead := snapshot()
	dead.Autoscaler = engine.Autoscaler{}
	Observe(dead, engine.Decision{Code: engine.CodeNoAutoscaler}, config(), 0.01)
	if got := testutil.ToFloat64(autoscalerUp); got != 0 {
		t.Errorf("binpack_autoscaler_up = %v with no autoscaler, want 0", got)
	}
}

func TestEveryMetricIsPublishedUnderItsDocumentedName(t *testing.T) {
	// The names are public API from the first release: people alert on them,
	// and an alert that silently stops firing because a series was renamed is
	// worse than no alert at all.
	//
	// Asserted as an explicit list rather than by filtering the output for the
	// prefix. A filter cannot notice a name that has lost the prefix — it just
	// stops seeing it, and the check passes.
	Observe(snapshot(engine.NodeGroup{ID: "da8977ba-244f", MinSize: 1, MaxSize: 10, Ready: 1}),
		engine.Decision{
			Code:        engine.CodeNoneFeasible,
			Assessments: []engine.NodeAssessment{assess(engine.VerdictSkipped, engine.SkipCordoned)},
		}, config(), 0.01)

	all, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	published := map[string]bool{}
	for _, f := range all {
		published[f.GetName()] = true
	}

	for _, name := range []string{
		"binpack_evaluations_total",
		"binpack_evaluation_errors_total",
		"binpack_evaluation_duration_seconds",
		"binpack_last_evaluation_timestamp_seconds",
		"binpack_autoscaler_up",
		"binpack_nodes",
		"binpack_nodes_skipped",
		"binpack_drainable_nodes",
		"binpack_pool_nodes",
		"binpack_pool_min_nodes",
		"binpack_pool_max_nodes",
	} {
		if !published[name] {
			t.Errorf("%s is not published; renamed or dropped", name)
		}
	}
}

func TestNoLabelCarriesProse(t *testing.T) {
	// The engine's reasons name nodes and pods. As a label value that is
	// unbounded cardinality, which is how a monitoring system falls over.
	Observe(snapshot(), engine.Decision{
		Code:   engine.CodeNoneFeasible,
		Reason: "2 node(s) considered, none whose workload fits elsewhere",
		Assessments: []engine.NodeAssessment{
			func() engine.NodeAssessment {
				a := assess(engine.VerdictSkipped, engine.SkipBackoff)
				a.SkipReason = "in backoff until 2026-08-15T12:04:00Z after: eviction refused by default/web"
				return a
			}(),
			// A skip carrying no code at all, which must produce no series
			// rather than one labelled with the empty string.
			assess(engine.VerdictSkipped, ""),
		},
	}, config(), 0.01)

	out := gather(t)
	for _, prose := range []string{"node(s) considered", "eviction refused", "default/web", "2026-08-15"} {
		if strings.Contains(out, prose) {
			t.Errorf("prose leaked into a label: %q\n%s", prose, out)
		}
	}
	// An empty label value is a series nobody can interpret, and it is what a
	// skip with no code would produce.
	if strings.Contains(out, `=""`) {
		t.Errorf("a metric carries an empty label value:\n%s", out)
	}
}

// gather renders binpack's own series in the exposition format, exactly as a
// scrape would see them. Filtered to the binpack_ prefix so controller-runtime
// and the Go runtime collectors do not drown the assertions.
func gather(t *testing.T) string {
	t.Helper()

	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}

	var b strings.Builder
	enc := expfmt.NewEncoder(&b, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), "binpack_") {
			continue
		}
		if err := enc.Encode(f); err != nil {
			t.Fatalf("encoding %s: %v", f.GetName(), err)
		}
	}
	return b.String()
}

func TestAReplicaThatHasNotEvaluatedPublishesNothingAboutTheCluster(t *testing.T) {
	// Only the leader evaluates, but every replica serves its own metrics
	// endpoint — and during a rolling update there are always two. A standby
	// publishing a full set of zeroes is indistinguishable from a cluster in
	// serious trouble, and the documented alerts would fire depending on which
	// pod answered the scrape.
	registry := prometheus.NewRegistry()
	quiet := newGated(
		prometheus.NewGauge(prometheus.GaugeOpts{Name: "binpack_test_drainable"}),
		prometheus.NewGauge(prometheus.GaugeOpts{Name: "binpack_test_autoscaler_up"}),
	)
	registry.MustRegister(quiet)

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gathering: %v", err)
	}
	if len(families) != 0 {
		var names []string
		for _, f := range families {
			names = append(names, f.GetName())
		}
		t.Errorf("a process that has not evaluated published %v", names)
	}

	quiet.publish(func() {})

	families, err = registry.Gather()
	if err != nil {
		t.Fatalf("gathering after an evaluation: %v", err)
	}
	if len(families) != 2 {
		t.Errorf("got %d families after an evaluation, want both", len(families))
	}
}

func TestTheGateStaysShutWhileTheValuesAreWritten(t *testing.T) {
	// A scrape landing between the gate opening and the values being written
	// sees a half-written set — and on the first evaluation that is a full set
	// of zeroes, the exact reading the gate exists to prevent. Asserted from
	// inside the write, since no sequential test can observe that window from
	// outside it.
	g := newGated()

	var openDuringWrite bool
	g.publish(func() { openDuringWrite = g.evaluated.Load() })

	if openDuringWrite {
		t.Error("the gate was open while the values were still being written")
	}
	if !g.evaluated.Load() {
		t.Error("the gate never opened, so nothing would ever be published")
	}
}

func TestObservePublishesThroughTheGate(t *testing.T) {
	Observe(snapshot(), engine.Decision{
		Code:        engine.CodeNoneFeasible,
		Assessments: []engine.NodeAssessment{assess(engine.VerdictDrainable, "")},
	}, config(), 0.01)

	if !strings.Contains(gather(t), "binpack_drainable_nodes 1") {
		t.Errorf("a completed evaluation published nothing:\n%s", gather(t))
	}
}

func TestPoolNodesReportsReadyNodesNotTheTarget(t *testing.T) {
	// They differ exactly while a scale-down is in progress: the target has
	// dropped and the nodes are still there and still billed. A series called
	// "nodes" that quietly means "intent" is one nobody can trust.
	scalingDown := engine.NodeGroup{
		ID: "da8977ba-244f", MinSize: 1, MaxSize: 10, Ready: 3,
		Target: 1, HasTarget: true,
	}
	if scalingDown.Size() != 1 {
		t.Fatalf("setup: Size() = %d, want the lower target", scalingDown.Size())
	}

	Observe(snapshot(scalingDown), engine.Decision{Code: engine.CodeNoneFeasible}, config(), 0.01)

	if !strings.Contains(gather(t), `binpack_pool_nodes{pool="pool-4g"} 3`) {
		t.Errorf("pool_nodes does not report the 3 nodes that exist:\n%s", gather(t))
	}
}

func TestUnmodelledNodesAreCountedApartFromOrdinaryShortfalls(t *testing.T) {
	// "The workload does not fit" is a fact about the cluster; "binpack cannot
	// tell what the workload is" is a gap in binpack. Folding them together
	// would hide the second behind the first, which is the one that is
	// permanent until somebody changes the allowlist.
	tooSmall := assess(engine.VerdictInfeasible, "")
	tooSmall.Simulation.Blocked = &engine.Blocked{Summary: "nowhere to go"}

	unreadable := assess(engine.VerdictInfeasible, "")
	unreadable.Simulation.Blocked = &engine.Blocked{
		Summary: "no readable controller template", Unmodelled: engine.FindingNoTemplate,
	}

	Observe(snapshot(), engine.Decision{
		Code:        engine.CodeNoneFeasible,
		Assessments: []engine.NodeAssessment{tooSmall, unreadable},
	}, config(), 0.01)

	if got := testutil.ToFloat64(nodes.WithLabelValues(engine.VerdictInfeasible)); got != 2 {
		t.Errorf("infeasible = %v, want both nodes counted", got)
	}
	if got := unmodelledFor(engine.FindingNoTemplate); got != 1 {
		t.Errorf("binpack_nodes_unmodelled = %v, want only the unreadable one", got)
	}
}

// unmodelledFor is one arm of binpack_nodes_unmodelled, by cause.
func unmodelledFor(cause string) float64 {
	return testutil.ToFloat64(unmodelled.WithLabelValues(cause))
}

// simulationRefusing runs a real simulation of a candidate whose one pod is
// this one, so the cause this gauge publishes is the one the engine decided
// rather than one the test asserted into place.
func simulationRefusing(
	pod *corev1.Pod, templates map[engine.OwnerRef]*corev1.PodTemplateSpec,
) engine.NodeAssessment {
	candidate, destination := mother.LargeNode("candidate"), mother.LargeNode("destination")
	sim := engine.Simulate([]*corev1.Node{candidate, destination}, []*corev1.Pod{pod},
		templates, candidate, engine.SimConfig{ExpendablePriorityCutoff: -10})
	return engine.NodeAssessment{Node: candidate, Simulation: &sim}
}

func TestTheTwoUnmodelledCausesAreCountedApart(t *testing.T) {
	// ADR-0006 settles the controller allowlist against measurement, and this
	// gauge is the measurement. Two quite different conditions reach it: a
	// controller kind binpack has no reader for, which is a gap in binpack and
	// widens on evidence, and a webhook in this cluster that mutates pods it
	// does not mutate templates, which binpack cannot widen its way out of at
	// all. A single number that adds them together answers neither question —
	// and the second is much the commoner, so it would be the one drowning out
	// the evidence the ADR asked for.
	// An owner kind collect reads no template from, so there is no entry.
	exotic := mother.Pod("default", "shard-0", mother.OnNode("candidate"),
		mother.OwnedBy("KafkaCluster", "events"))

	// A readable template that a webhook diverged from at CREATE.
	mutated := mother.Pod("default", "web", mother.OnNode("candidate"),
		mother.WithNodeSelector("tier", "app"))
	diverging := mother.Templates(mutated)
	mother.TemplateFor(diverging, mutated) // the template lacks the selector

	Observe(snapshot(), engine.Decision{
		Code: engine.CodeNoneFeasible,
		Assessments: []engine.NodeAssessment{
			simulationRefusing(exotic, mother.Templates(exotic)),
			simulationRefusing(mutated, diverging),
		},
	}, config(), 0.01)

	if got := unmodelledFor(engine.FindingNoTemplate); got != 1 {
		t.Errorf("unreadable-template arm = %v, want 1", got)
	}
	if got := unmodelledFor(engine.FindingAdmissionDivergence); got != 1 {
		t.Errorf("admission-divergence arm = %v, want 1", got)
	}
}

func TestBothUnmodelledCausesHaveAZeroSeries(t *testing.T) {
	// Same rule as the verdicts, and the same gap it has to be tested through:
	// an arm that only appears once it has fired makes the comparison above
	// impossible on the cluster where nothing has fired yet, and an absent
	// series reads as "binpack is not reporting" rather than as zero.
	//
	// Driven from a draining tick because that is the path where seeding is
	// the only thing that writes these arms — an ordinary evaluation sets both
	// every time, so it would pass with no seeding at all. A leader that
	// restarts mid-drain opens the gate on this tick.
	unmodelled.Reset()

	Observe(snapshot(), engine.Decision{
		Code:   engine.CodeDraining,
		Reason: "a drain is in progress on node-a",
	}, config(), 0.01)

	// Spelled out rather than ranged over unmodelledCauses: a test that reads
	// the same list the code publishes from cannot notice the list shrinking,
	// which is the failure it exists to catch.
	scrape := gather(t)
	for _, cause := range []string{"unreadable-template", "admission-divergence"} {
		want := `binpack_nodes_unmodelled{cause="` + cause + `"}`
		if !strings.Contains(scrape, want) {
			t.Errorf("%s is not published:\n%s", want, scrape)
		}
	}
}

func TestEveryAbandonReasonHasAZeroSeries(t *testing.T) {
	// A counter that only appears once it has fired makes a rate() alert
	// silently useless until the first occurrence — the moment somebody needed
	// it. All three sources reach this counter, so all three must be seeded.
	scrape := gather(t)

	var want []string
	want = append(want, drain.AbandonCodes()...)
	want = append(want, engine.SkipCodes()...)
	want = append(want, engine.VerdictInfeasible, engine.VerdictBlocked)

	for _, code := range want {
		series := `binpack_drains_abandoned_total{reason="` + code + `"}`
		if !strings.Contains(scrape, series) {
			t.Errorf("%s is missing, so an alert on it cannot fire", series)
		}
	}
}

func TestBackoffDepthOutlivesTheBackoffWindow(t *testing.T) {
	// The attempt count stands until a drain of the node succeeds. Gating the
	// depth on the deadline would make it fall to zero during every retry
	// window and climb back only on the next failure, which reads as recovery
	// from the one thing worth alerting on.
	now := time.Now()
	expired := mother.LargeNode("a", mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainAttempts: "3",
		engine.AnnotationBackoffUntil:  now.Add(-time.Minute).Format(time.RFC3339),
	}))
	active := mother.LargeNode("b", mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainAttempts: "1",
		engine.AnnotationBackoffUntil:  now.Add(time.Hour).Format(time.RFC3339),
	}))

	s := snapshot()
	s.Nodes = []*corev1.Node{expired, active}
	s.Now = now
	Observe(s, engine.Decision{Code: engine.CodeNoCandidates}, config(), 0.01)

	if got := testutil.ToFloat64(drainAttemptsMax); got != 3 {
		t.Errorf("drain_attempts_max = %v, want 3 — the expired node still has three failures", got)
	}
	if got := testutil.ToFloat64(nodesInBackoff); got != 1 {
		t.Errorf("nodes_in_backoff = %v, want 1 — only one deadline is still in the future", got)
	}
}

// sumNodes is the whole of binpack_nodes, which is what a dashboard panel
// showing "how many nodes does binpack see" actually plots.
func sumNodes() float64 {
	var total float64
	for _, verdict := range []string{
		engine.VerdictSkipped, engine.VerdictInfeasible,
		engine.VerdictBlocked, engine.VerdictDrainable,
	} {
		total += testutil.ToFloat64(nodes.WithLabelValues(verdict))
	}
	return total
}

func TestAdvancingADrainDoesNotZeroTheNodeGauges(t *testing.T) {
	// An evaluation that advances a drain never runs Decide, so it carries no
	// reading of the cluster's nodes — and reset-then-recount over no
	// assessments republishes an empty cluster. For the whole duration of
	// every drain, which is minutes to tens of minutes, the gauges said
	// binpack could see nothing.
	//
	// The same reasoning Failed() already applies: the gauges describe the
	// last evaluation that reached a conclusion, and an evaluation that
	// reached none must leave them alone.
	Observe(snapshot(), engine.Decision{
		Code: engine.CodeNoneFeasible,
		Assessments: []engine.NodeAssessment{
			assess(engine.VerdictSkipped, engine.SkipCordoned),
			assess(engine.VerdictInfeasible, ""),
			assess(engine.VerdictBlocked, ""),
		},
	}, config(), 0.01)

	before := sumNodes()
	if before != 3 {
		t.Fatalf("binpack_nodes summed to %v after assessing three nodes, want 3", before)
	}

	Observe(snapshot(), engine.Decision{
		Code:   engine.CodeDraining,
		Reason: "a drain is in progress on node-a",
	}, config(), 0.01)

	if got := sumNodes(); got != before {
		t.Errorf("binpack_nodes summed to %v after a draining tick, want it unchanged at %v",
			got, before)
	}
	if got := testutil.ToFloat64(skipped.WithLabelValues(engine.SkipCordoned)); got != 1 {
		t.Errorf("binpack_nodes_skipped{code=cordoned} = %v after a draining tick, want 1", got)
	}
	if got := testutil.ToFloat64(evaluations.WithLabelValues(engine.CodeDraining)); got < 1 {
		t.Error("the draining tick was not counted as an evaluation")
	}
}

// TestADrainingTickStillSeedsTheVerdicts covers the leader that restarts
// mid-drain, whose first evaluation is a draining tick.
//
// Leaving the gauges alone opens the gate without ever having written them, so
// binpack_nodes would be served with no children at all — absent rather than
// zero, which is the reading the gate itself exists to prevent.
func TestADrainingTickStillSeedsTheVerdicts(t *testing.T) {
	// Reset stands in for the fresh process: a GaugeVec that has never counted
	// anything has no children at all.
	nodes.Reset()

	Observe(snapshot(), engine.Decision{
		Code:   engine.CodeDraining,
		Reason: "a drain is in progress on node-a",
	}, config(), 0.01)

	scrape := gather(t)
	for _, verdict := range []string{
		engine.VerdictSkipped, engine.VerdictInfeasible,
		engine.VerdictBlocked, engine.VerdictDrainable,
	} {
		series := fmt.Sprintf("binpack_nodes{verdict=%q} 0", verdict)
		if !strings.Contains(scrape, series) {
			t.Errorf("%s is missing from the scrape:\n%s", series, scrape)
		}
	}
}

// TestADryRunDrainStillPublishesTheClusterItAssessed is the other half of the
// condition, and it is the half a narrower guard would take away.
//
// In dry run a drain in progress is reported and then decided past, so the
// decision carries the draining code *and* a full set of assessments. Keying
// only on the code would republish an empty cluster for as long as dryRun
// stood — which, since the marker is never cleared, is for ever.
func TestADryRunDrainStillPublishesTheClusterItAssessed(t *testing.T) {
	Observe(snapshot(), engine.Decision{
		Code: engine.CodeDraining,
		Assessments: []engine.NodeAssessment{
			assess(engine.VerdictSkipped, engine.SkipDrainInProgress),
			assess(engine.VerdictDrainable, ""),
			assess(engine.VerdictDrainable, ""),
		},
	}, config(), 0.01)

	if got := sumNodes(); got != 3 {
		t.Errorf("binpack_nodes summed to %v, want the 3 nodes the dry run assessed", got)
	}
	if got := testutil.ToFloat64(skipped.WithLabelValues(engine.SkipDrainInProgress)); got != 1 {
		t.Errorf("binpack_nodes_skipped{code=drain-in-progress} = %v, want 1", got)
	}
}

// TestNoAutoscalerStillZeroesTheNodeGauges is the widening direction.
//
// That decision also carries no assessments, and there the zeroes are the
// answer rather than the absence of one: binpack has refused to assess
// anything, binpack_autoscaler_up says so in the same scrape, and the metrics
// reference gives that state its own alert. A guard that keyed on emptiness
// alone would freeze the last healthy reading over a cluster whose autoscaler
// has gone.
func TestNoAutoscalerStillZeroesTheNodeGauges(t *testing.T) {
	Observe(snapshot(), engine.Decision{
		Code: engine.CodeNoneFeasible,
		Assessments: []engine.NodeAssessment{
			assess(engine.VerdictDrainable, ""),
			assess(engine.VerdictDrainable, ""),
		},
	}, config(), 0.01)

	if got := sumNodes(); got != 2 {
		t.Fatalf("binpack_nodes summed to %v before the autoscaler went, want 2", got)
	}

	dead := snapshot()
	dead.Autoscaler = engine.Autoscaler{}
	Observe(dead, engine.Decision{
		Code:   engine.CodeNoAutoscaler,
		Reason: "no cluster-autoscaler status was found",
	}, config(), 0.01)

	if got := sumNodes(); got != 0 {
		t.Errorf("binpack_nodes summed to %v with no autoscaler, want 0", got)
	}
}
