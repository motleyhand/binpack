package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/expfmt"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	corev1 "k8s.io/api/core/v1"

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

	if strings.Contains(gather(t), engine.SkipCordoned) {
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
		Summary: "no readable controller template", NoTemplate: true,
	}

	Observe(snapshot(), engine.Decision{
		Code:        engine.CodeNoneFeasible,
		Assessments: []engine.NodeAssessment{tooSmall, unreadable},
	}, config(), 0.01)

	if got := testutil.ToFloat64(nodes.WithLabelValues(engine.VerdictInfeasible)); got != 2 {
		t.Errorf("infeasible = %v, want both nodes counted", got)
	}
	if got := testutil.ToFloat64(unmodelled); got != 1 {
		t.Errorf("binpack_nodes_unmodelled = %v, want only the unreadable one", got)
	}
}
