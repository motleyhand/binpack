package engine_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

const (
	poolID   = "da8977ba-244f"
	poolName = "pool-4g"
)

var now = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// cluster is a working three-node autoscaling pool above its minimum, which
// every test then perturbs in exactly one way.
func cluster(nodes []*corev1.Node, pods []*corev1.Pod) engine.Snapshot {
	return engine.Snapshot{
		Nodes: nodes,
		Pods:  pods,
		// A template matching each pod: the ordinary case, where what is
		// running is what the controller would create again.
		Templates: mother.Templates(pods...),
		Now:       now,
		Autoscaler: engine.Autoscaler{
			Running: true,
			// Probed just now: a status ConfigMap outlives the autoscaler
			// that wrote it, so freshness is part of "running".
			LastProbe: now.Add(-10 * time.Second),
			Groups: []engine.NodeGroup{
				{ID: poolID, MinSize: 1, MaxSize: 10, Ready: len(nodes)},
			},
		},
	}
}

func inPool(name string, opts ...mother.NodeOption) *corev1.Node {
	return sized(name, "4Gi", append([]mother.NodeOption{mother.InPool(poolName, poolID)}, opts...)...)
}

func config() engine.Config {
	return engine.Config{
		NodeGroupIDLabel: "doks.digitalocean.com/node-pool-id",
		PoolNameLabel:    "doks.digitalocean.com/node-pool",
		Default: engine.Policy{
			Enabled: true,
			Sim:     engine.SimConfig{ExpendablePriorityCutoff: cutoff},
			Evict:   engine.DefaultEvictConfig(),
		},
	}
}

// skipReasonFor is assessmentFor for the cases that judge the sentence rather
// than the verdict, so a failure prints the sentence instead of the whole node.
func skipReasonFor(t *testing.T, d engine.Decision, node string) string {
	t.Helper()
	a := assessmentFor(d, node)
	if a == nil {
		t.Fatalf("every node must be accounted for; %s was not", node)
	}
	if !a.Skipped {
		t.Fatalf("%s was not skipped, so it has no reason to name", node)
	}
	return a.SkipReason
}

func assessmentFor(d engine.Decision, node string) *engine.NodeAssessment {
	for i := range d.Assessments {
		if d.Assessments[i].Node.Name == node {
			return &d.Assessments[i]
		}
	}
	return nil
}

func TestDrainsTheEmptiestFeasibleNode(t *testing.T) {
	nodes := []*corev1.Node{inPool("a"), inPool("b"), inPool("c")}
	pods := []*corev1.Pod{
		mother.Pod("default", "heavy", mother.OnNode("a"), mother.Requests("100m", "1Gi")),
		mother.Pod("default", "light", mother.OnNode("b"), mother.Requests("100m", "128Mi")),
	}

	d := engine.Decide(cluster(nodes, pods), config())

	if d.Action != engine.Drain {
		t.Fatalf("expected a drain, got none: %s", d.Reason)
	}
	// c is empty, so it is the cheapest to remove.
	if d.Node.Name != "c" {
		t.Errorf("drained %s, want the emptiest node c", d.Node.Name)
	}
}

// TestEmptiestIsMeasuredByWhatNodesHold closes the ordering half of the same
// question the placement simulation asks.
//
// "Emptiest" is a claim about a node, so it has to be read from what the node
// has handed out rather than from what its pods most recently asked for. A pod
// mid-downward-resize has a lowered spec and an unchanged allocation, so on
// spec alone the node holding a gigabyte of it looks like the emptiest in the
// pool and binpack tries to drain it first — while every other number binpack
// computes about that node, and everything the scheduler and the autoscaler
// see, says it is the fullest.
//
// The filler is expendable so that the ordering is the only thing under test:
// a relocatable pod mid-resize is refused outright by fit, which would make
// its node undrainable and hide which candidate was tried first behind a
// feasibility answer.
func TestEmptiestIsMeasuredByWhatNodesHold(t *testing.T) {
	nodes := []*corev1.Node{inPool("a"), inPool("b"), inPool("spare")}
	pods := []*corev1.Pod{
		mother.Pod("default", "filler", mother.OnNode("a"), mother.Priority(-100),
			mother.ResizingFrom(
				corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("128Mi"),
				},
				corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("100m"),
					corev1.ResourceMemory: resource.MustParse("1Gi"),
				},
			)),
		mother.Pod("default", "on-b", mother.OnNode("b"), mother.Requests("100m", "512Mi")),
		mother.Pod("default", "on-spare", mother.OnNode("spare"), mother.Requests("100m", "2Gi")),
	}

	d := engine.Decide(cluster(nodes, pods), config())

	if d.Action != engine.Drain {
		t.Fatalf("expected a drain, got none: %s", d.Reason)
	}
	if d.Node.Name != "b" {
		t.Errorf("drained %s, want b: a holds a gigabyte its pod's spec no longer admits to, "+
			"so ranking it emptiest reads the resize rather than the node", d.Node.Name)
	}
}

func TestRefusesWithoutAnAutoscaler(t *testing.T) {
	s := cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
	s.Autoscaler.Running = false

	d := engine.Decide(s, config())

	if d.Action != engine.None {
		t.Fatal("binpack must not drain when nothing would remove the node")
	}
	if !strings.Contains(d.Reason, "no cluster-autoscaler") {
		t.Errorf("reason should say why, got: %s", d.Reason)
	}
}

func TestWaitsAfterAScaleUp(t *testing.T) {
	// Draining straight after the cluster grew is how oscillation starts —
	// and the autoscaler pauses its own scale-down then anyway.
	s := cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
	s.Autoscaler.LastScaleUp = now.Add(-2 * time.Minute)

	cfg := config()
	cfg.Default.CooldownAfterScaleUp = 10 * time.Minute

	d := engine.Decide(s, cfg)

	if d.Action != engine.None {
		t.Fatal("expected to wait")
	}
	if !strings.Contains(d.Reason, "scaled up") {
		t.Errorf("reason should mention the scale-up, got: %s", d.Reason)
	}
}

func TestActsOnceTheCooldownHasPassed(t *testing.T) {
	s := cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
	s.Autoscaler.LastScaleUp = now.Add(-30 * time.Minute)

	cfg := config()
	cfg.Default.CooldownAfterScaleUp = 10 * time.Minute

	if d := engine.Decide(s, cfg); d.Action != engine.Drain {
		t.Fatalf("the cooldown has passed, expected a drain: %s", d.Reason)
	}
}

func TestSkipReasons(t *testing.T) {
	tests := []struct {
		name  string
		node  *corev1.Node
		want  string
		setup func(*engine.Snapshot, *engine.Config)
	}{
		{
			name: "not in an autoscaling pool",
			node: sized("static", "4Gi"), // no pool labels
			want: "not part of an autoscaling pool",
		},
		{
			name: "explicitly annotated",
			node: inPool("skipped", mother.NodeAnnotations(map[string]string{
				engine.AnnotationSkip: "true",
			})),
			want: "annotated",
		},
		{
			name: "a drain is already running",
			node: inPool("draining", mother.NodeAnnotations(map[string]string{
				engine.AnnotationDrainStarted: now.Add(-time.Minute).Format(time.RFC3339),
			})),
			want: "drain is already in progress",
		},
		{
			name: "in backoff after a failure",
			node: inPool("failed", mother.NodeAnnotations(map[string]string{
				engine.AnnotationBackoffUntil: now.Add(time.Hour).Format(time.RFC3339),
				engine.AnnotationLastFailure:  "pod stuck terminating",
			})),
			want: "in backoff",
		},
		{
			name: "already cordoned by someone else",
			node: inPool("cordoned", mother.Cordoned()),
			want: "already cordoned",
		},
		{
			name: "pool disabled",
			node: inPool("disabled"),
			want: "disabled for this pool",
			setup: func(_ *engine.Snapshot, c *engine.Config) {
				c.ByPool = map[string]engine.Policy{poolID: {Enabled: false}}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Two healthy nodes alongside, so the pool is above its minimum
			// and the subject node is the only interesting one.
			nodes := []*corev1.Node{tc.node, inPool("healthy-1"), inPool("healthy-2")}
			s := cluster(nodes, nil)
			cfg := config()
			if tc.setup != nil {
				tc.setup(&s, &cfg)
			}

			d := engine.Decide(s, cfg)

			a := assessmentFor(d, tc.node.Name)
			if a == nil {
				t.Fatalf("every node must be accounted for; %s was not", tc.node.Name)
			}
			if !a.Skipped {
				t.Fatalf("%s should have been skipped", tc.node.Name)
			}
			if !strings.Contains(a.SkipReason, tc.want) {
				t.Errorf("skip reason = %q, want it to mention %q", a.SkipReason, tc.want)
			}
			if d.Action == engine.Drain && d.Node.Name == tc.node.Name {
				t.Error("a skipped node must not be drained")
			}
		})
	}
}

func TestAnExpiredBackoffIsNotASkip(t *testing.T) {
	// Backoff is a delay, not a verdict. Nothing ever clears the annotation —
	// the executor only writes it, and the node it is written on is by
	// definition one binpack has decided not to drain, so it is never deleted
	// either. Read as "carries a backoff annotation" rather than "is inside
	// its backoff window", the node is ineligible for ever and consolidation
	// stops on it the first time a drain fails, silently: explain prints a
	// skip reason naming a timestamp in the past, and nothing else disagrees.
	// ADR-0007 makes non-permanence normative.
	//
	// Its own test rather than a row of TestSkipReasons, because that table
	// asserts every row *is* skipped and this is the opposite direction.
	expired := inPool("retryable", mother.NodeAnnotations(map[string]string{
		engine.AnnotationBackoffUntil: now.Add(-time.Minute).Format(time.RFC3339),
		engine.AnnotationLastFailure:  "pod stuck terminating",
	}))
	s := cluster([]*corev1.Node{expired, inPool("healthy-1"), inPool("healthy-2")}, nil)

	a := assessmentFor(engine.Decide(s, config()), "retryable")

	if a == nil {
		t.Fatal("every node must be accounted for; retryable was not")
	}
	if a.Skipped {
		t.Fatalf("a backoff that expired a minute ago is not a skip: %s", a.SkipReason)
	}
}

func TestTheBackoffReasonNamesTheFailure(t *testing.T) {
	// Two message forms, and which one an operator gets is the difference
	// between "binpack will retry at 13:00" and knowing why it stopped. The
	// recorded failure is the only actionable half, so a node carrying one
	// must say it and a node carrying none must not trail an empty clause.
	until := now.Add(time.Hour).Format(time.RFC3339)
	recorded := inPool("recorded", mother.NodeAnnotations(map[string]string{
		engine.AnnotationBackoffUntil: until,
		engine.AnnotationLastFailure:  "pod stuck terminating",
	}))
	silent := inPool("silent", mother.NodeAnnotations(map[string]string{
		engine.AnnotationBackoffUntil: until,
	}))

	d := engine.Decide(cluster([]*corev1.Node{recorded, silent, inPool("healthy")}, nil), config())

	if got := skipReasonFor(t, d, "recorded"); !strings.Contains(got, "after: pod stuck terminating") {
		t.Errorf("the recorded failure is what makes the skip actionable: %q", got)
	}
	if got := skipReasonFor(t, d, "silent"); strings.Contains(got, "after:") {
		t.Errorf("with nothing recorded there is nothing to say after: %q", got)
	}
}

func TestAFinishedPodDoesNotProtectItsNode(t *testing.T) {
	// A Succeeded pod holds no resources and nothing removes it on its own:
	// absent a Job ttlSecondsAfterFinished or an owner deletion it sits on its
	// node until podgc crosses its terminated-pod threshold. Counting one as
	// occupying the node would make every node that has ever run a CronJob in
	// an excluded namespace permanently ineligible — and the skip renders as a
	// correct-looking sentence under a legitimate metric label, so the symptom
	// is only that binpack consolidates less of the cluster each week.
	pods := []*corev1.Pod{
		mother.Pod("kube-system", "backup-28471", mother.OnNode("a"), mother.Succeeded()),
		mother.Pod("default", "web", mother.OnNode("a")),
	}
	cfg := config()
	cfg.Default.ExcludedNamespaces = []string{"kube-system"}

	a := assessmentFor(engine.Decide(cluster(
		[]*corev1.Node{inPool("a"), inPool("b"), inPool("c")}, pods), cfg), "a")

	if a == nil {
		t.Fatal("every node must be accounted for; a was not")
	}
	if a.Skipped {
		t.Fatalf("a finished pod does not protect its node: %s", a.SkipReason)
	}
}

func TestStaleAutoscalerStatusIsNotTrusted(t *testing.T) {
	// The ConfigMap outlives the autoscaler that wrote it and keeps saying
	// Running. Without a freshness check, the one guard that stops binpack
	// draining nodes nothing will reap is defeated by a leftover object.
	s := cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
	s.Autoscaler.LastProbe = now.Add(-time.Hour)

	d := engine.Decide(s, config())

	if d.Action != engine.None {
		t.Fatal("an autoscaler silent for an hour is not running")
	}
	if !strings.Contains(d.Reason, "last reported") {
		t.Errorf("reason should say the status is stale, got: %s", d.Reason)
	}
}

func TestProviderTargetGuardsTheFloor(t *testing.T) {
	// Mid scale-down the autoscaler has already lowered its target to the
	// minimum while the departing node is still Ready. Trusting Ready would
	// approve a drain the autoscaler will not honour.
	s := cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
	s.Autoscaler.Groups[0] = engine.NodeGroup{
		ID: poolID, MinSize: 1, MaxSize: 10,
		Ready: 2, Target: 1, HasTarget: true,
	}

	d := engine.Decide(s, config())

	if d.Action != engine.None {
		t.Fatalf("the pool is already at its target minimum, got a drain of %s", d.Node.Name)
	}
	if a := assessmentFor(d, "a"); a == nil || !strings.Contains(a.SkipReason, "minimum size") {
		t.Errorf("reason should name the floor, got %+v", a)
	}
}

func TestZeroTargetIsARealTarget(t *testing.T) {
	// A pool with minSize 0 removing its last node reports target 0. Treating
	// zero as "not reported" would discard exactly the case where the
	// autoscaler has most clearly finished deciding.
	s := cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
	s.Autoscaler.Groups[0] = engine.NodeGroup{
		ID: poolID, MinSize: 0, MaxSize: 10,
		Ready: 1, Target: 0, HasTarget: true,
	}

	d := engine.Decide(s, config())

	if d.Action != engine.None {
		t.Fatalf("the autoscaler is already targeting zero, got a drain of %s", d.Node.Name)
	}
}

func TestMissingProbeTimeIsNotTrusted(t *testing.T) {
	// Absent evidence of life is not evidence of life. A document that says
	// Running but carries no probe time is one binpack cannot vouch for.
	s := cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
	s.Autoscaler.LastProbe = time.Time{}

	d := engine.Decide(s, config())

	if d.Action != engine.None {
		t.Fatal("without a probe time binpack cannot tell the autoscaler is alive")
	}
	if !strings.Contains(d.Reason, "no probe time") {
		t.Errorf("reason should say why, got: %s", d.Reason)
	}
}

func TestLiveIsTheSameJudgementEverywhere(t *testing.T) {
	// Decide and explain must not disagree about whether the autoscaler is
	// running; an earlier version printed "running" above a decision refusing
	// to act because it was not.
	s := cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
	s.Autoscaler.LastProbe = now.Add(-time.Hour)

	live, _, why := s.Autoscaler.Live(s.Now)
	d := engine.Decide(s, config())

	if live {
		t.Fatal("an autoscaler silent for an hour is not live")
	}
	if d.Reason != why {
		t.Errorf("Decide and Live must give the same reason:\n  Live:   %s\n  Decide: %s", why, d.Reason)
	}
}

func TestPoolAtMinimumIsLeftAlone(t *testing.T) {
	// At the minimum the autoscaler simply replaces whatever is drained, so
	// the drain is pure churn.
	nodes := []*corev1.Node{inPool("a"), inPool("b")}
	s := cluster(nodes, nil)
	s.Autoscaler.Groups[0].MinSize = 2

	d := engine.Decide(s, config())

	if d.Action != engine.None {
		t.Fatal("a pool at its minimum must not be drained")
	}
	if a := assessmentFor(d, "a"); a == nil || !strings.Contains(a.SkipReason, "minimum size") {
		t.Errorf("reason should name the floor, got %+v", a)
	}
}

// TestAnUnnamedNodeGroupClaimsNoNodes is the closure of the join's one
// unguarded direction: an identifier that matches by being absent.
//
// [engine.Config.GroupOf] answers "" for a node carrying no join label, which
// is every static node on every cluster — a self-managed box, a pool the
// autoscaler was never told about. The managed gate is a lookup of that answer
// in the published groups, so one group the status names with an empty string
// makes groups[""] exist and every one of those nodes pass. `name` is
// omitempty in the autoscaler's own status type, so the value is
// representable rather than hypothetical.
//
// What follows is worse than a mislabelled report. The node stops being
// reported not-autoscaled, becomes an ordinary drain candidate that no
// autoscaler will ever reap — so the cordon is stranded — and takes its policy
// from a pool it is not in, including `enabled: false`. `diagnose` stops
// listing it as static at the same moment, so the operator loses the signal
// too.
func TestAnUnnamedNodeGroupClaimsNoNodes(t *testing.T) {
	// Carrying neither label: the ordinary static node, and the one an
	// unnamed group would claim.
	s := cluster([]*corev1.Node{sized("static", "4Gi")}, nil)
	s.Autoscaler.Groups = []engine.NodeGroup{{ID: "", MinSize: 1, Ready: 1}}

	d := engine.Decide(s, config())

	a := assessmentFor(d, "static")
	if a == nil {
		t.Fatal("every node must be accounted for; static was not")
	}
	if a.SkipCode != engine.SkipNotAutoscaled {
		t.Errorf("static was assessed %q, want %q: a group published with no name "+
			"has claimed a node that is in no pool at all",
			a.SkipCode, engine.SkipNotAutoscaled)
	}
}

// TestThePoolNameIsTheSameInTheMetricAndInTheDrainEvent closes the join
// between the two reports an operator has to correlate.
//
// `binpack_pool_nodes` and `binpack diagnose` name a pool through
// [engine.PoolNames], which any one node in the pool can fill; `explain`'s
// node row and the drain Event name it through the assessment. The two agree
// only where the readable label is applied uniformly across a pool, and
// nothing enforces that: a node added by a scale-up before the operator's
// labelling automation ran carries the identifier and nothing else, and so
// does a node added by hand. The dashboard series then says `pool-4g` while
// that node's drain Event says `da8977ba-244f`, with nothing to join on —
// which is verbatim the harm [engine.NodeAssessment.PoolLabel]'s own doc
// comment says it was written to end.
//
// So what is asserted is not that the name is right but that there is one
// name: every assessment's Pool is what the resolved naming says about its
// group, and PoolLabel adds nothing to it.
func TestThePoolNameIsTheSameInTheMetricAndInTheDrainEvent(t *testing.T) {
	// Only the identifier label, which is the cluster's join and all a fresh
	// node is guaranteed to carry.
	unlabelled := sized("unlabelled", "4Gi", mother.NodeLabels(
		map[string]string{"doks.digitalocean.com/node-pool-id": poolID}))
	s := cluster([]*corev1.Node{inPool("labelled"), unlabelled}, nil)
	cfg := config()

	d := engine.Decide(s, cfg)

	names := engine.PoolNames(s, cfg)
	if len(d.Assessments) != len(s.Nodes) {
		t.Fatalf("assessed %d nodes, want all %d", len(d.Assessments), len(s.Nodes))
	}
	for _, a := range d.Assessments {
		if want := names.Label(a.Group); a.Pool != want {
			t.Errorf("%s reports pool %q while the naming resolves its group to %q, "+
				"so the metric and the drain Event have nothing to join on",
				a.Node.Name, a.Pool, want)
		}
		if a.PoolLabel() != a.Pool {
			t.Errorf("%s: PoolLabel() is %q and Pool is %q, so one assessment "+
				"names its pool two ways", a.Node.Name, a.PoolLabel(), a.Pool)
		}
	}
}

func TestInfeasibleNodeIsReportedNotDrained(t *testing.T) {
	// Everything is full, so nothing can move.
	nodes := []*corev1.Node{inPool("a"), inPool("b")}
	pods := []*corev1.Pod{
		mother.Pod("default", "big-a", mother.OnNode("a"), mother.Requests("100m", "3Gi")),
		mother.Pod("default", "big-b", mother.OnNode("b"), mother.Requests("100m", "3Gi")),
	}

	d := engine.Decide(cluster(nodes, pods), config())

	if d.Action != engine.None {
		t.Fatal("neither node's workload fits on the other")
	}
	a := assessmentFor(d, "a")
	if a == nil || a.Simulation == nil || a.Simulation.Feasible {
		t.Fatalf("the simulation should be recorded and infeasible, got %+v", a)
	}
	if a.Simulation.Blocked == nil || a.Simulation.Blocked.Pod == nil {
		t.Error("an infeasible simulation should name the pod with nowhere to go")
	}
	if !strings.Contains(d.Reason, "fits elsewhere") {
		t.Errorf("summary should say what went wrong, got: %s", d.Reason)
	}
}

func TestEvictionBlockersPreventADrain(t *testing.T) {
	// The workload fits, but a disruption budget will not let it move.
	labelled := map[string]string{"app": "web"}
	// b is annotated skip so it is a destination but not a candidate,
	// leaving a as the only node under consideration. A skipped node is
	// still somewhere pods can go — it is only excluded from being drained.
	nodes := []*corev1.Node{
		inPool("a"),
		inPool("b", mother.NodeAnnotations(map[string]string{engine.AnnotationSkip: "true"})),
	}
	pods := []*corev1.Pod{
		mother.Pod("default", "web-1", mother.OnNode("a"),
			mother.PodLabels(labelled), mother.Requests("100m", "128Mi")),
	}
	s := cluster(nodes, pods)
	s.PDBs = []*policyv1.PodDisruptionBudget{mother.PDB("default", "web", 0, labelled)}

	d := engine.Decide(s, config())

	if d.Action != engine.None {
		t.Fatal("a pod that cannot be evicted must stop the drain")
	}
	a := assessmentFor(d, "a")
	if a == nil || len(a.Blockers) == 0 {
		t.Fatalf("the blocker should be recorded against the node, got %+v", a)
	}
	if a.Simulation == nil || !a.Simulation.Feasible {
		t.Error("the simulation succeeded; it is eviction that failed, and explain must show both")
	}
}

func TestTriesEveryEligibleNode(t *testing.T) {
	// The emptiest node is blocked by a budget, so binpack should move on
	// rather than give up — ordering is a preference, not a filter.
	labelled := map[string]string{"app": "stuck"}
	nodes := []*corev1.Node{inPool("blocked"), inPool("free"), inPool("spare")}
	pods := []*corev1.Pod{
		mother.Pod("default", "stuck-1", mother.OnNode("blocked"),
			mother.PodLabels(labelled), mother.Requests("100m", "128Mi")),
		mother.Pod("default", "movable", mother.OnNode("free"), mother.Requests("100m", "256Mi")),
	}
	s := cluster(nodes, pods)
	s.PDBs = []*policyv1.PodDisruptionBudget{mother.PDB("default", "stuck", 0, labelled)}

	d := engine.Decide(s, config())

	if d.Action != engine.Drain {
		t.Fatalf("another node was drainable: %s", d.Reason)
	}
	if d.Node.Name == "blocked" {
		t.Fatal("drained the node whose pod cannot be evicted")
	}
}

func TestMaxPodsPerDrainCapsBlastRadius(t *testing.T) {
	// roomy is a destination but not a candidate, so busy is the only node
	// the cap can apply to.
	nodes := []*corev1.Node{
		inPool("busy"),
		inPool("roomy", mother.NodeAnnotations(map[string]string{engine.AnnotationSkip: "true"})),
	}
	var pods []*corev1.Pod
	for _, name := range []string{"p1", "p2", "p3"} {
		pods = append(pods, mother.Pod("default", name,
			mother.OnNode("busy"), mother.Requests("50m", "64Mi")))
	}

	cfg := config()
	cfg.Default.MaxPodsPerDrain = 2

	d := engine.Decide(cluster(nodes, pods), cfg)

	if d.Action != engine.None {
		t.Fatalf("three pods exceeds the cap of two, got a drain of %s", d.Node.Name)
	}
	if a := assessmentFor(d, "busy"); a == nil || !strings.Contains(a.SkipReason, "above the limit") {
		t.Errorf("reason should name the cap, got %+v", a)
	}
}

func TestExcludedNamespaceProtectsItsNode(t *testing.T) {
	// The pods are protected, not ignored: removing them from the arithmetic
	// while leaving them on the node would be unsound.
	nodes := []*corev1.Node{inPool("a"), inPool("b")}
	pods := []*corev1.Pod{
		mother.Pod("payments", "ledger", mother.OnNode("a"), mother.Requests("100m", "128Mi")),
	}

	cfg := config()
	cfg.Default.ExcludedNamespaces = []string{"payments"}

	d := engine.Decide(cluster(nodes, pods), cfg)

	if d.Action == engine.Drain && d.Node.Name == "a" {
		t.Fatal("a node hosting an excluded namespace must not be drained")
	}
	a := assessmentFor(d, "a")
	if a == nil || !strings.Contains(a.SkipReason, "payments") {
		t.Fatalf("reason should name the namespace, got %+v", a)
	}
	// The sentence is for a human and the code is the public thing: it is
	// published as binpack_nodes_skipped{code="protected-pod"} and documented
	// in docs/reference/metrics.md. Pinning only the prose leaves this branch
	// free to reuse a neighbouring code — the sentence still names the
	// namespace, the series stops existing, and any alert keyed on it never
	// fires again.
	if a.SkipCode != engine.SkipProtectedPod {
		t.Errorf("skip code = %q, want %q", a.SkipCode, engine.SkipProtectedPod)
	}
}

const otherPoolID = "758e416b-b3f9"

func inOtherPool(name string, opts ...mother.NodeOption) *corev1.Node {
	return sized(name, "4Gi", append([]mother.NodeOption{mother.InPool("pool-8g", otherPoolID)}, opts...)...)
}

// twoPools is a snapshot with both pools autoscaling and above their minimums.
func twoPools(nodes []*corev1.Node, pods []*corev1.Pod) engine.Snapshot {
	s := cluster(nodes, pods)
	s.Autoscaler.Groups = []engine.NodeGroup{
		{ID: poolID, MinSize: 1, MaxSize: 10, Ready: 2},
		{ID: otherPoolID, MinSize: 1, MaxSize: 10, Ready: 2},
	}
	return s
}

func TestCooldownIsResolvedPerPool(t *testing.T) {
	// A pool that wants an hour of quiet after a scale-up must not hold up a
	// pool that wants none. Reading only the global default would make both
	// directions of override silently ineffective.
	nodes := []*corev1.Node{
		inPool("cautious-1"), inPool("cautious-2"),
		inOtherPool("eager-1"), inOtherPool("eager-2"),
	}
	s := twoPools(nodes, nil)
	s.Autoscaler.LastScaleUp = now.Add(-5 * time.Minute)

	cfg := config()
	cfg.ByPool = map[string]engine.Policy{
		poolID: {Enabled: true, CooldownAfterScaleUp: time.Hour,
			Sim: cfg.Default.Sim, Evict: cfg.Default.Evict},
	}

	d := engine.Decide(s, cfg)

	if d.Action != engine.Drain {
		t.Fatalf("the pool without a cooldown should still be drainable: %s", d.Reason)
	}
	if !strings.HasPrefix(d.Node.Name, "eager") {
		t.Errorf("drained %s, want a node from the pool with no cooldown", d.Node.Name)
	}

	a := assessmentFor(d, "cautious-1")
	if a == nil || !a.Skipped || !strings.Contains(a.SkipReason, "scaled up") {
		t.Errorf("the cautious pool should be waiting, got %+v", a)
	}
}

func TestExcludedNamespacesAreResolvedPerPool(t *testing.T) {
	nodes := []*corev1.Node{
		inPool("a-1"), inPool("a-2"),
		inOtherPool("b-1"), inOtherPool("b-2"),
	}
	pods := []*corev1.Pod{
		mother.Pod("payments", "ledger", mother.OnNode("a-1"), mother.Requests("100m", "128Mi")),
		mother.Pod("payments", "billing", mother.OnNode("b-1"), mother.Requests("100m", "128Mi")),
	}

	t.Run("a pool override can add an exclusion the default lacks", func(t *testing.T) {
		cfg := config()
		cfg.ByPool = map[string]engine.Policy{
			poolID: {Enabled: true, ExcludedNamespaces: []string{"payments"},
				Sim: cfg.Default.Sim, Evict: cfg.Default.Evict},
		}

		d := engine.Decide(twoPools(nodes, pods), cfg)

		if a := assessmentFor(d, "a-1"); a == nil || !strings.Contains(a.SkipReason, "payments") {
			t.Errorf("the override should protect a-1, got %+v", a)
		}
		// The other pool has no such exclusion, so its node is untouched by it.
		if a := assessmentFor(d, "b-1"); a != nil && strings.Contains(a.SkipReason, "payments") {
			t.Errorf("b-1 is in a pool with no exclusion and must not be protected, got %+v", a)
		}
	})

	t.Run("a pool override can clear a global exclusion", func(t *testing.T) {
		cfg := config()
		cfg.Default.ExcludedNamespaces = []string{"payments"}
		cfg.ByPool = map[string]engine.Policy{
			otherPoolID: {Enabled: true, ExcludedNamespaces: nil,
				Sim: cfg.Default.Sim, Evict: cfg.Default.Evict},
		}

		d := engine.Decide(twoPools(nodes, pods), cfg)

		if a := assessmentFor(d, "a-1"); a == nil || !strings.Contains(a.SkipReason, "payments") {
			t.Errorf("the global exclusion should still protect a-1, got %+v", a)
		}
		if a := assessmentFor(d, "b-1"); a == nil {
			t.Fatal("b-1 missing from the assessments")
		} else if strings.Contains(a.SkipReason, "payments") {
			t.Errorf("the override cleared the exclusion for this pool, got %+v", a)
		}
	})
}

func TestNothingEligibleStillExplainsItself(t *testing.T) {
	// "No node was eligible" on its own tells an operator nothing. The
	// commonest reason is what they need.
	nodes := []*corev1.Node{inPool("a"), inPool("b")}
	s := cluster(nodes, nil)
	s.Autoscaler.Groups[0].MinSize = 2

	d := engine.Decide(s, config())

	if !strings.Contains(d.Reason, "minimum size") {
		t.Errorf("reason should name the commonest obstacle, got: %s", d.Reason)
	}
}

func TestEveryNodeIsAccountedFor(t *testing.T) {
	// explain has to describe the whole cluster, not only the interesting
	// part. A node missing from the assessments is a node an operator cannot
	// ask about.
	nodes := []*corev1.Node{
		inPool("a"), inPool("b"),
		sized("static", "4Gi"),
		inPool("skipped", mother.NodeAnnotations(map[string]string{engine.AnnotationSkip: "true"})),
	}

	d := engine.Decide(cluster(nodes, nil), config())

	for _, node := range nodes {
		if assessmentFor(d, node.Name) == nil {
			t.Errorf("%s is missing from the assessments", node.Name)
		}
	}
}

func TestResolvePoolsNamesEveryUnknownPoolInAStableOrder(t *testing.T) {
	// The names come from a map. An error that reorders itself between runs
	// is one nobody can diff, match against a log, or assert on.
	s := cluster([]*corev1.Node{inPool("a")}, nil)
	cfg := config()
	cfg.ByPool = map[string]engine.Policy{
		"zeta-pool":  {Enabled: false},
		"alpha-pool": {Enabled: false},
		"mid-pool":   {Enabled: false},
		poolName:     {Enabled: false},
	}

	_, first := engine.ResolvePools(s, cfg)
	if first == nil {
		t.Fatal("three unknown pools were accepted")
	}
	if !strings.Contains(first.Error(), "alpha-pool, mid-pool, zeta-pool") {
		t.Errorf("unknown pools are not listed in sorted order: %v", first)
	}
	// The one that does exist must not be reported.
	if strings.Contains(first.Error(), poolName) {
		t.Errorf("a pool that exists was reported as unknown: %v", first)
	}

	for range 20 {
		if _, got := engine.ResolvePools(s, cfg); got.Error() != first.Error() {
			t.Fatalf("the message varies between runs:\n %v\n %v", first, got)
		}
	}
}

func TestResolvePoolsAcceptsAPoolKnownOnlyToTheAutoscaler(t *testing.T) {
	// A pool scaled to zero has no nodes to carry its label, but the
	// autoscaler still reports it. Rejecting the override then would take
	// binpack down over a pool that is merely empty.
	s := cluster(nil, nil)
	s.Autoscaler.Groups = []engine.NodeGroup{
		{ID: poolID, MinSize: 0, MaxSize: 10, Ready: 0},
	}
	cfg := config()
	cfg.ByPool = map[string]engine.Policy{poolID: {Enabled: false}}

	if _, err := engine.ResolvePools(s, cfg); err != nil {
		t.Errorf("an empty but autoscaled pool was rejected: %v", err)
	}
}

func TestEverySkipReasonCarriesItsOwnCode(t *testing.T) {
	// The prose says why in a sentence; the code is what a metric and an alert
	// key on. Two branches sharing a code is invisible in the prose and makes
	// the metric lie — "12 nodes in backoff" when none are.
	// The cooldowns only apply when configured, and the shared config leaves
	// them off so that other tests are not silently governed by a clock.
	withCooldowns := func() engine.Config {
		c := config()
		c.Default.CooldownAfterScaleUp = 10 * time.Minute
		c.Default.CooldownAfterDrain = 10 * time.Minute
		return c
	}
	disabledPool := func() engine.Config {
		c := config()
		c.ByPool = map[string]engine.Policy{poolName: {Enabled: false}}
		return c
	}
	excluding := func(namespace string) engine.Config {
		c := config()
		c.Default.ExcludedNamespaces = []string{namespace}
		return c
	}
	cappedAt := func(pods int) engine.Config {
		c := config()
		c.Default.MaxPodsPerDrain = pods
		return c
	}

	tests := []struct {
		name  string
		build func() engine.Snapshot
		cfg   engine.Config
		want  string
	}{
		{"not autoscaled", func() engine.Snapshot {
			s := cluster([]*corev1.Node{sized("loner", "4Gi")}, nil)
			return s
		}, config(), engine.SkipNotAutoscaled},

		{"scaling up right now", func() engine.Snapshot {
			s := cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
			s.Autoscaler.ScaleUpInProgress = true
			return s
		}, config(), engine.SkipScaleUpInProgress},

		{"cooldown after scale-up", func() engine.Snapshot {
			s := cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
			s.Autoscaler.LastScaleUp = now.Add(-time.Minute)
			return s
		}, withCooldowns(), engine.SkipCooldownAfterScaleUp},

		{"cooldown after drain", func() engine.Snapshot {
			s := cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
			s.LastDrain = now.Add(-time.Minute)
			return s
		}, withCooldowns(), engine.SkipCooldownAfterDrain},

		{"pool at its minimum", func() engine.Snapshot {
			s := cluster([]*corev1.Node{inPool("a")}, nil)
			s.Autoscaler.Groups = []engine.NodeGroup{
				{ID: poolID, MinSize: 3, MaxSize: 10, Ready: 3},
			}
			return s
		}, config(), engine.SkipPoolAtMinimum},

		{"annotated skip", func() engine.Snapshot {
			return cluster([]*corev1.Node{
				inPool("a", mother.NodeAnnotations(map[string]string{engine.AnnotationSkip: "true"})),
				inPool("b"),
			}, nil)
		}, config(), engine.SkipAnnotated},

		{"drain in progress", func() engine.Snapshot {
			return cluster([]*corev1.Node{
				inPool("a", mother.NodeAnnotations(map[string]string{
					engine.AnnotationDrainStarted: now.Format(time.RFC3339)})),
				inPool("b"),
			}, nil)
		}, config(), engine.SkipDrainInProgress},

		{"backoff", func() engine.Snapshot {
			return cluster([]*corev1.Node{
				inPool("a", mother.NodeAnnotations(map[string]string{
					engine.AnnotationBackoffUntil: now.Add(time.Hour).Format(time.RFC3339),
					engine.AnnotationLastFailure:  "eviction refused",
				})),
				inPool("b"),
			}, nil)
		}, config(), engine.SkipBackoff},

		{"cordoned", func() engine.Snapshot {
			return cluster([]*corev1.Node{inPool("a", mother.Cordoned()), inPool("b")}, nil)
		}, config(), engine.SkipCordoned},

		{"the autoscaler is already deleting this node", func() engine.Snapshot {
			return cluster([]*corev1.Node{
				inPool("a", mother.Tainted(
					engine.TaintToBeDeleted, "1786971071", corev1.TaintEffectNoSchedule)),
				inPool("b"),
			}, nil)
		}, config(), engine.SkipBeingRemoved},

		{"the pool is switched off", func() engine.Snapshot {
			return cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
		}, disabledPool(), engine.SkipPoolDisabled},

		{"a pod binpack must not evict", func() engine.Snapshot {
			return cluster([]*corev1.Node{inPool("a"), inPool("b")}, []*corev1.Pod{
				mother.Pod("payments", "ledger", mother.OnNode("a"),
					mother.Requests("100m", "128Mi")),
			})
		}, excluding("payments"), engine.SkipProtectedPod},

		{"more pods than the blast radius allows", func() engine.Snapshot {
			var pods []*corev1.Pod
			for _, name := range []string{"p1", "p2", "p3"} {
				pods = append(pods, mother.Pod("default", name,
					mother.OnNode("a"), mother.Requests("50m", "64Mi")))
			}
			return cluster([]*corev1.Node{inPool("a"), inPool("b")}, pods)
		}, cappedAt(2), engine.SkipTooManyPods},
	}

	seen, produced := map[string]bool{}, map[string]bool{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := engine.Decide(tc.build(), tc.cfg)

			var codes []string
			for _, a := range d.Assessments {
				if a.Skipped {
					codes = append(codes, a.SkipCode)
					produced[a.SkipCode] = true
					if a.SkipReason == "" {
						t.Errorf("%s: a skip code with no prose to explain it", a.SkipCode)
					}
				}
			}
			if !slices.Contains(codes, tc.want) {
				t.Errorf("skip codes = %v, want one of them to be %q", codes, tc.want)
			}
			seen[tc.want] = true
		})
	}

	// Every code the engine can emit must be reachable, or it is documentation
	// for a state that cannot happen.
	//
	// Enumerated from [engine.SkipCodes] rather than listed here, because a
	// list is a record of what somebody remembered to type rather than of what
	// the vocabulary holds. The hand-written one this replaced named ten of
	// sixteen, and the six it omitted included protected-pod — published as
	// binpack_nodes_skipped{code="protected-pod"}, documented in
	// docs/reference/metrics.md, and asserted by nothing at all. Every test of
	// that branch pinned the sentence, which is explicitly not the public half.
	enumerated := map[string]bool{}
	for _, code := range engine.SkipCodes() {
		enumerated[code] = true

		why, elsewhere := decidedElsewhere[code]
		switch {
		case elsewhere && seen[code]:
			// A false record reads as coverage, so it fails as loudly as a
			// gap does. The fix is to move the code into the table above.
			t.Errorf("a case reaches %q, but the record says Decide cannot:\n  %s", code, why)
		case !elsewhere && !seen[code]:
			t.Errorf("no case reaches %q", code)
		}
	}

	// And the record is checked against something that emits, not only against
	// itself. Every entry claims Revalidate reaches the code instead, and until
	// now that claim was a sentence: a value added to SkipCodes(), to
	// docs/reference/metrics.md and to this map satisfied all three at once,
	// so a misspelling or a branch never written could be documented and
	// pre-initialised as a public label for ever with nothing producing it.
	//
	// The fixtures are vocabulary_test.go's, driven rather than described.
	// The record and [engine.SelectionSkipCodes] have to name the same three.
	// The package's set is what metrics and the reference read; this one
	// carries the reasons and the driven check, and a package that quietly
	// widened its set would otherwise leave both green.
	for _, code := range engine.SkipCodes() {
		_, excused := decidedElsewhere[code]
		if reachable := slices.Contains(engine.SelectionSkipCodes(), code); reachable == excused {
			t.Errorf("SelectionSkipCodes() %s %q and this record %s it; the two describe "+
				"one fact and disagree", map[bool]string{true: "includes", false: "omits"}[reachable],
				code, map[bool]string{true: "excuses", false: "does not excuse"}[excused])
		}
	}

	produces := revalidateProduces(t)
	for code, why := range decidedElsewhere {
		if !produces[code] {
			t.Errorf("the record says Revalidate reaches %q instead, and no fixture there "+
				"produces it:\n  %s\nA code nothing emits is documentation for a state "+
				"that cannot happen, which is what this loop exists to refuse", code, why)
		}
	}

	// And the other direction, which the loop above cannot give: a code the
	// engine emits that the enumerator has stopped listing is invisible to
	// every check keyed on the enumerator, this one included — it simply
	// stops being visited.
	//
	// [TestEverySkipCodeDecideProducesIsEnumerated] asks the same question of
	// its own smaller table, and the metrics reference guard asks it of the
	// documentation. Neither reaches every branch this table does: deleting
	// SkipProtectedPod from [engine.SkipCodes] and from
	// docs/reference/metrics.md left the whole suite green with the branch
	// still emitting "protected-pod", so the series was no longer
	// pre-initialised, no longer documented, and still produced.
	for code := range produced {
		if !enumerated[code] {
			t.Errorf("Decide emits %q and SkipCodes() does not enumerate it, so it is "+
				"published as binpack_nodes_skipped{code=%q} without being "+
				"pre-initialised or held to the reference", code, code)
		}
	}
}

// decidedElsewhere is the other half of the reachability guard, and it is the
// half that has to be written down.
//
// A skip code Decide never emits is either a branch that has stopped firing or
// a value only [engine.Revalidate] can reach, and only one of those is a
// defect. So each entry says which, and names the test that adjudicates the
// code instead — deleting it from the enumerator to quiet the loop would take
// a published label value out of the vocabulary the reference is checked
// against, which is the failure the enumerator exists to prevent.
//
// All three here are Revalidate's because the two entry points ask different
// questions: Decide chooses among the nodes it can see, while Revalidate
// re-asks about one named node whose drain is already under way — so "gone",
// "uncordoned" and "no autoscaler to finish this" are states only the second
// can be in.
var decidedElsewhere = map[string]string{
	engine.SkipGone: "Decide assesses the nodes the snapshot carries, so a node it " +
		"can see is by construction not gone. Revalidate answers for a node that " +
		"left between the decision and this evaluation, which is the outcome a " +
		"drain is working towards. Exercised by TestRevalidateTreatsAMissingNodeAsGone.",
	engine.SkipUncordoned: "eligibility reaches this branch only when resuming, and " +
		"Decide passes resuming false — a node it has not marked cannot be a marked " +
		"node that is not cordoned. Exercised by " +
		"TestRevalidateRefusesAMarkedNodeThatIsNotCordoned.",
	engine.SkipAutoscalerNotLive: "Decide refuses above the assessments when " +
		"Autoscaler.Live says no, and returns a decision code rather than a per-node " +
		"skip — that emptiness is what stops a dead autoscaler zeroing the node " +
		"gauges. Only a drain already in flight needs the per-node answer. Exercised " +
		"by TestRevalidateStopsADrainTheClusterHasOvertaken and " +
		"TestEverySkipCodeRevalidateProducesIsEnumerated.",
}

func TestEveryDecisionCarriesACode(t *testing.T) {
	// Decide has several returns and the code is set at each. A new one that
	// forgets leaves a metric labelled with the empty string.
	noAutoscaler := cluster([]*corev1.Node{inPool("a")}, nil)
	noAutoscaler.Autoscaler = engine.Autoscaler{}

	allSkipped := cluster([]*corev1.Node{inPool("a", mother.Cordoned())}, nil)

	feasible := cluster([]*corev1.Node{inPool("a"), inPool("b"), inPool("c")},
		[]*corev1.Pod{mother.Pod("default", "web", mother.OnNode("a"))})

	// Two nodes, each too full to take the other's workload — so every
	// candidate is simulated and every one fails, which is the state
	// none-feasible names and no-candidates does not.
	tooBig := cluster([]*corev1.Node{inPool("a"), inPool("b")}, []*corev1.Pod{
		mother.Pod("default", "big-a", mother.OnNode("a"), mother.Requests("100m", "3Gi")),
		mother.Pod("default", "big-b", mother.OnNode("b"), mother.Requests("100m", "3Gi")),
	})

	for _, tc := range []struct {
		name string
		s    engine.Snapshot
		want string
	}{
		{"no autoscaler", noAutoscaler, engine.CodeNoAutoscaler},
		{"nothing eligible", allSkipped, engine.CodeNoCandidates},
		{"nothing fits", tooBig, engine.CodeNoneFeasible},
		{"a drain", feasible, engine.CodeDrain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := engine.Decide(tc.s, config())
			if d.Code != tc.want {
				t.Errorf("code = %q, want %q (reason: %s)", d.Code, tc.want, d.Reason)
			}
		})
	}
}

// TestAnOrdinaryTaintIsNotTheAutoscalers is the negative half, and it is the
// one the suite could not fail.
//
// BeingRemoved scans the whole taint list for a single key, so it can be wrong
// in two directions and neither is loud. Not recognising the autoscaler's taint
// has binpack evicting alongside a deletion already under way. Recognising
// everything else has every dedicated, spot or GPU pool reported as
// being-removed — and, on a node binpack is draining, handed to a wait for an
// autoscaler that was never coming.
func TestAnOrdinaryTaintIsNotTheAutoscalers(t *testing.T) {
	s := cluster([]*corev1.Node{
		inPool("a", mother.Tainted("dedicated", "db", corev1.TaintEffectNoSchedule)),
		inPool("b"),
	}, nil)

	a := assessmentFor(engine.Decide(s, config()), "a")
	if a == nil {
		t.Fatal("every node must be accounted for; a was not")
	}
	if a.SkipCode == engine.SkipBeingRemoved {
		t.Errorf("a taint that is not the autoscaler's was read as one: %s", a.SkipReason)
	}
	if a.Skipped {
		t.Errorf("an ordinary taint is not a reason to skip a node: %s", a.SkipReason)
	}
}

// TestDecideReportsADrainInProgressRatherThanChoosingASecondNode is the whole
// of "one node per run", asked of the function that is supposed to enforce it.
//
// The rule used to live above Decide, in the controller's loop, so the only
// other caller — `binpack explain` — did not have it. During a drain explain
// therefore announced a drain of a *different* node, in the one window an
// operator is most likely to be looking at a cordoned node and asking what
// binpack is doing to it.
func TestDecideReportsADrainInProgressRatherThanChoosingASecondNode(t *testing.T) {
	s := cluster([]*corev1.Node{
		inPool("a", mother.Cordoned(), mother.NodeAnnotations(map[string]string{
			engine.AnnotationDrainStarted: now.Add(-5 * time.Minute).Format(time.RFC3339),
		})),
		inPool("b"), inPool("c"),
	}, []*corev1.Pod{mother.Pod("default", "web", mother.OnNode("a"))})

	d := engine.Decide(s, config())

	if d.Action != engine.None {
		t.Errorf("action = %s naming %s, want none: a drain is already running on a",
			d.Action, d.Node.Name)
	}
	if d.Code != engine.CodeDraining {
		t.Errorf("code = %q, want %q", d.Code, engine.CodeDraining)
	}
	switch {
	case d.Node == nil:
		t.Error("the decision must name the node being drained, it named none")
	case d.Node.Name != "a":
		t.Errorf("the decision named %s, want the node being drained, a", d.Node.Name)
	}
}

// TestADrainInProgressStillAccountsForEveryNode is the half the gate could
// silently take away.
//
// Returning early from Decide would be the cheap way to write the rule and it
// would leave explain with an empty node table — losing the arithmetic the
// command exists to show, in the same window that made the gate necessary. So
// the gate sits below the assessments, not above them.
func TestADrainInProgressStillAccountsForEveryNode(t *testing.T) {
	nodes := []*corev1.Node{
		inPool("a", mother.Cordoned(), mother.NodeAnnotations(map[string]string{
			engine.AnnotationDrainStarted: now.Add(-5 * time.Minute).Format(time.RFC3339),
		})),
		inPool("b"), inPool("c"),
	}

	d := engine.Decide(cluster(nodes, nil), config())

	for _, node := range nodes {
		if assessmentFor(d, node.Name) == nil {
			t.Errorf("%s is missing from the assessments", node.Name)
		}
	}
	if a := assessmentFor(d, "b"); a != nil && a.Chosen {
		t.Error("b is marked chosen; a drain in progress must choose nothing")
	}
}

// backingOff is a node binpack has failed to drain, waiting until when.
//
// Each caller passes its own expiry because a shared one would hide the whole
// defect below: the sentences only collide when the timestamps do.
func backingOff(name string, until time.Time, attempts string) *corev1.Node {
	annotations := map[string]string{
		engine.AnnotationBackoffUntil: until.Format(time.RFC3339),
		engine.AnnotationLastFailure:  "pod stuck terminating",
	}
	if attempts != "" {
		annotations[engine.AnnotationDrainAttempts] = attempts
	}
	return inPool(name, mother.NodeAnnotations(annotations))
}

// TestTheSummaryCountsSkipsByCodeNotBySentence pins the sentence an operator
// reads before anything else.
//
// It is Decision.Reason, and the `reason` key of the controller's "nothing to
// do" log line. Taking the mode over the rendered prose rather than the code
// beside it is wrong in two directions at once. Five skip reasons interpolate
// per-node data — a backoff expiry, a pod reference, a pool's size — so each
// of their nodes contributes a distinct string counted once: a unanimous wall
// reads as one node's sentence "most commonly", and a fragmented majority
// loses outright to any smaller group whose prose happens to be identical.
//
// Both directions are here because fixing one is not fixing the other: a mode
// over codes with no count still cannot say the cluster was unanimous, and a
// count over sentences would count the wrong thing accurately.
func TestTheSummaryCountsSkipsByCodeNotBySentence(t *testing.T) {
	for _, tc := range []struct {
		name            string
		nodes           []*corev1.Node
		want, doNotWant string
	}{
		{
			// The count is the claim. Without it the sentence cannot
			// distinguish "every node" from "one of them".
			name: "a wall every node hit",
			nodes: []*corev1.Node{
				backingOff("a", now.Add(time.Hour), ""),
				backingOff("b", now.Add(45*time.Minute), ""),
				backingOff("c", now.Add(30*time.Minute), ""),
			},
			want:      "3 of 3",
			doNotWant: "already cordoned",
		},
		{
			// The count and the sentence have to be about one code. Ruled out
			// first is a node the modal code does not cover, so a
			// representative taken from the front pairs three nodes' count
			// with a fourth node's wall.
			name: "a reason from the modal code, not from whichever node came first",
			nodes: []*corev1.Node{
				inPool("e", mother.Cordoned()),
				backingOff("a", now.Add(time.Hour), ""),
				backingOff("b", now.Add(45*time.Minute), ""),
				backingOff("c", now.Add(30*time.Minute), ""),
			},
			want:      "3 of 4",
			doNotWant: "already cordoned",
		},
		{
			// The sharper failure: four nodes share a code and lose to two
			// that share a string, so the summary names a minority reason as
			// the typical one.
			name: "a fragmented majority against a smaller identical minority",
			nodes: []*corev1.Node{
				backingOff("a", now.Add(time.Hour), ""),
				backingOff("b", now.Add(45*time.Minute), ""),
				backingOff("c", now.Add(30*time.Minute), ""),
				backingOff("d", now.Add(15*time.Minute), ""),
				inPool("e", mother.Cordoned()),
				inPool("f", mother.Cordoned()),
			},
			want:      "4 of 6",
			doNotWant: "already cordoned",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := engine.Decide(cluster(tc.nodes, nil), config())

			if d.Action != engine.None {
				t.Fatalf("every node was ruled out, expected no drain: %s", d.Reason)
			}
			if !strings.Contains(d.Reason, tc.want) {
				t.Errorf("the summary does not count the nodes the modal code covers "+
					"(want %q), got: %s", tc.want, d.Reason)
			}
			if !strings.Contains(d.Reason, "in backoff") {
				t.Errorf("the summary names a reason other than the modal code's, got: %s",
					d.Reason)
			}
			if strings.Contains(d.Reason, tc.doNotWant) {
				t.Errorf("the summary names %q, which fewer nodes hit than the modal code, "+
					"got: %s", tc.doNotWant, d.Reason)
			}
		})
	}
}

// TestTheSummaryDoesNotDependOnHowTheClusterWasListed pins the half of
// counting by code that the tables above cannot reach: the sentence must be a
// function of the cluster and of nothing else.
//
// Two things that are not the cluster can reach it. The counting is a map, so
// without a tie-break two equally common codes swap places between
// evaluations. And the reason is one node's, so without an order the
// representative is whichever node came first — which is whatever
// `reader.List` returned, since collect.Snapshot appends nodes in list order
// and sorts nothing. Either one produces a `reason` log line that changes with
// nothing in the cluster changing, which is the defect this PR is about
// wearing a different hat.
//
// Which of two tied codes wins is arbitrary and deliberately not asserted.
// That the same cluster answers the same way is.
func TestTheSummaryDoesNotDependOnHowTheClusterWasListed(t *testing.T) {
	// Distinct expiries, so every backoff node renders a different sentence
	// and only the choice between them can vary.
	backoff := []*corev1.Node{
		backingOff("a", now.Add(time.Hour), ""),
		backingOff("b", now.Add(45*time.Minute), ""),
		backingOff("c", now.Add(30*time.Minute), ""),
	}
	cordoned := []*corev1.Node{inPool("d", mother.Cordoned()), inPool("e", mother.Cordoned())}
	cfg := config()

	orders := map[string][]*corev1.Node{
		"as listed":            {backoff[0], backoff[1], backoff[2], cordoned[0], cordoned[1]},
		"reversed":             {cordoned[1], cordoned[0], backoff[2], backoff[1], backoff[0]},
		"interleaved":          {cordoned[0], backoff[2], cordoned[1], backoff[0], backoff[1]},
		"modal node not first": {cordoned[0], cordoned[1], backoff[1], backoff[2], backoff[0]},
	}

	var first, firstName string
	for name, nodes := range orders {
		reason := engine.Decide(cluster(nodes, nil), cfg).Reason
		if !strings.Contains(reason, "3 of 5") {
			t.Fatalf("%s: the modal code does not cover the nodes it should, got: %s",
				name, reason)
		}
		if first == "" {
			first, firstName = reason, name
			continue
		}
		if reason != first {
			t.Errorf("the same cluster summarised two ways, by list order alone:\n"+
				"%s: %s\n%s: %s", firstName, first, name, reason)
		}
	}

	// Which representative, not only that it is stable. Ordering by the
	// sentence instead of by the node would also be deterministic and would
	// pass everything above — and for backoff expiries the smallest string is
	// node c at 12:30, the node closest to recovering and the least
	// representative of the three. Node a's is the answer that comes from an
	// identity rather than from a spelling.
	if !strings.Contains(first, now.Add(time.Hour).Format(time.RFC3339)) {
		t.Errorf("the representative is not the winning code's first node by name, so it "+
			"was chosen by how its sentence reads: %s", first)
	}

	// And the map half, which needs repetition rather than reordering: two
	// codes covering the same number of nodes must not swap between runs.
	tied := cluster([]*corev1.Node{backoff[0], backoff[1], cordoned[0], cordoned[1]}, nil)
	stable := engine.Decide(tied, cfg).Reason
	if !strings.Contains(stable, "2 of 4") {
		t.Fatalf("neither tied code covers the nodes it should, got: %s", stable)
	}
	for range 32 {
		if again := engine.Decide(tied, cfg).Reason; again != stable {
			t.Fatalf("the same cluster summarised two ways:\n%s\n%s", stable, again)
		}
	}
}

// TestBackoffStateNamesTheAttemptCount closes the gap between the metric an
// alert fires on and the two surfaces an operator then reads.
//
// `binpack_drain_attempts_max` climbing is the alert; ADR-0007 says explain
// and diagnose are where the node behind it is named. Both name the node and
// withhold the number, so with more than one node in backoff neither says
// which one the alert was about.
func TestBackoffStateNamesTheAttemptCount(t *testing.T) {
	for _, tc := range []struct {
		name, attempts, want, doNotWant string
	}{
		// The number the doubling is computed from, and the only thing that
		// tells a first failure from a seventh.
		{name: "several attempts", attempts: "3", want: "after 3 attempts"},
		// Written as prose an operator reads, so it has to agree with itself.
		{name: "one attempt", attempts: "1", want: "after 1 attempt", doNotWant: "1 attempts"},
		// Absent or unreadable is not zero attempts, it is no answer — and a
		// clause claiming none would be worse than no clause.
		{name: "nothing recorded", attempts: "", doNotWant: "attempt"},
		// A node in backoff has failed at least once, so a recorded zero is a
		// corrupt value like any other and "after 0 attempts" is a sentence
		// that contradicts the one it is attached to.
		{name: "zero recorded", attempts: "0", doNotWant: "attempt"},
		{name: "unreadable", attempts: "seven", doNotWant: "attempt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := backingOff("failed", now.Add(time.Hour), tc.attempts)
			nodes := []*corev1.Node{node, inPool("healthy-1"), inPool("healthy-2")}
			s := cluster(nodes, nil)

			a := assessmentFor(engine.Decide(s, config()), node.Name)
			if a == nil {
				t.Fatalf("every node must be accounted for; %s was not", node.Name)
			}
			detail := only(t, engine.Diagnose(s, config()), engine.FindingNodeInBackoff).Detail

			// Both, because they are one fact rendered twice: explain reads
			// the skip reason and diagnose reads the finding, and an operator
			// meeting both is the same person.
			for surface, got := range map[string]string{
				"the skip reason":            a.SkipReason,
				"the node-in-backoff detail": detail,
			} {
				if tc.want != "" && !strings.Contains(got, tc.want) {
					t.Errorf("%s does not carry the recorded attempt count (want %q), got: %s",
						surface, tc.want, got)
				}
				if tc.doNotWant != "" && strings.Contains(got, tc.doNotWant) {
					t.Errorf("%s says %q on a count it does not have, got: %s",
						surface, tc.doNotWant, got)
				}
			}
		})
	}
}

// eksNodeGroupLabel is the label AWS puts on a managed node group's nodes.
//
// It stands in for every provider label that looks like the answer and is not:
// its value is the EKS node group's name, while the identifier the
// cluster-autoscaler publishes for the same nodes is the Auto Scaling group's.
// The key is present, the values do not match, and binpack sees no pool at all.
const eksNodeGroupLabel = "eks.amazonaws.com/nodegroup"

// eksNode is a node from a cluster binpack was never pointed at correctly: it
// carries a pool label, but not the one the configuration names.
func eksNode(name string, opts ...mother.NodeOption) *corev1.Node {
	return sized(name, "16Gi", append([]mother.NodeOption{
		mother.NodeLabels(map[string]string{
			eksNodeGroupLabel:        "workers",
			"kubernetes.io/hostname": name,
		}),
	}, opts...)...)
}

// mismatched is the cluster the whole of this section is about: a live
// autoscaler reporting two groups that have nodes in them, and not one node
// carrying a value either group would recognise.
func mismatched(nodes ...*corev1.Node) engine.Snapshot {
	s := cluster(nodes, nil)
	s.Autoscaler.Groups = []engine.NodeGroup{
		{ID: "asg-b", MinSize: 1, MaxSize: 10, Ready: len(nodes)},
		{ID: "asg-a", MinSize: 1, MaxSize: 10, Ready: len(nodes)},
	}
	return s
}

// TestALabelThatMatchesNoNodeIsReportedAsSuch is the preflight the reference
// has promised since the first release and nothing implemented.
//
// binpack maps a node to a pool through one label, and the match is on the
// label's *value*: `nodeGroups[].name` in the cluster-autoscaler's status is
// the cloud provider's own identifier for the group, so a node has to carry
// that identifier for discovery to work at all. Where it does not, every node
// falls out of scope and binpack says "not part of an autoscaling pool" about
// a cluster of nothing but autoscaled nodes — printed directly beneath its own
// list of healthy pools, with the count from the summary making the wrong
// answer sound more certain rather than less.
//
// So the error has to carry four things the operator cannot get anywhere else:
// which setting is wrong, what it is set to, what the autoscaler actually
// published, and what the nodes actually carry. And it has to carry the
// remedy, because this check converts a cluster that ran badly into one that
// does not run — a trade only worth making if the message ends the problem.
func TestALabelThatMatchesNoNodeIsReportedAsSuch(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() engine.Snapshot
	}{
		{
			// The advertised first-run path everywhere but one provider: the
			// chart's defaults against nodes carrying a pool label of their
			// own, which looks like the answer and is not.
			name: "no node carries the configured key at all",
			build: func() engine.Snapshot {
				return mismatched(eksNode("ip-10-0-1-11"), eksNode("ip-10-0-1-12"))
			},
		},
		{
			// The half a key-level check would let through, and the half the
			// whole finding turns on: the key is exactly where it should be,
			// and its value answers to no group the autoscaler published. A
			// pool rebuilt under a new identifier reaches this, and so does a
			// copied configuration.
			name: "the key is there holding a value no group answers to",
			build: func() engine.Snapshot {
				return mismatched(
					eksNode("ip-10-0-1-11", mother.NodeLabels(map[string]string{
						"doks.digitalocean.com/node-pool-id": "a-pool-that-was-replaced",
					})),
					eksNode("ip-10-0-1-12"),
				)
			},
		},
		{
			// `name` is omitempty upstream, so a group with no name at all is
			// representable — and an unnamed group matches every unlabelled
			// node, which would switch this check off rather than fail it.
			name: "the status carries a group with no name beside a real one",
			build: func() engine.Snapshot {
				s := mismatched(eksNode("ip-10-0-1-11"), sized("unlabelled", "16Gi"))
				s.Autoscaler.Groups = append(s.Autoscaler.Groups,
					engine.NodeGroup{MinSize: 1, MaxSize: 10, Ready: 1})
				return s
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg := tc.build(), config()

			_, err := engine.ResolvePools(s, cfg)
			if err == nil {
				t.Fatal("a cluster where no node carries a recognised value passed preflight")
			}

			for what, want := range map[string]string{
				"a group the autoscaler published": "asg-a",
				"the other group it published":     "asg-b",
				"the label key the nodes do carry": eksNodeGroupLabel,
				"the remedy, as a command to run":  "kubectl label nodes",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error does not name %s (%q):\n%v", what, want, err)
				}
			}

			// The setting and the value it holds, in one line rather than
			// merely both somewhere in the message. Naming the setting is
			// what the remedy needs; naming the value is what tells an
			// operator whether they are looking at the default or at
			// something they set. Separated, neither says which is which.
			if !sameLine(err.Error(), "nodeGroupIDLabel", cfg.NodeGroupIDLabel) {
				t.Errorf("the error does not say which key discovery.nodeGroupIDLabel "+
					"currently names:\n%v", err)
			}

			// A template is not a remedy. The command has to carry a name the
			// autoscaler actually published, or the operator is back at the
			// ConfigMap working out what to substitute.
			if !strings.Contains(err.Error(), engine.NodeGroupLabelSuggestion+"=asg-a") {
				t.Errorf("the command binpack prints is not runnable as printed:\n%v", err)
			}

		})
	}
}

// sameLine reports whether one line of text carries every one of the given
// substrings, so an assertion about a sentence is not satisfied by two
// unrelated ones.
func sameLine(text string, want ...string) bool {
	for line := range strings.SplitSeq(text, "\n") {
		found := true
		for _, w := range want {
			found = found && strings.Contains(line, w)
		}
		if found {
			return true
		}
	}
	return false
}

// TestWithoutPreflightAMismatchReadsAsAFactAboutTheCluster is the state the
// check above pre-empts, kept as its own test so that a fixture which stopped
// reproducing it cannot leave the assertions on the message passing over a
// cluster binpack would have been perfectly happy with.
//
// Every node is skipped as `not-autoscaled` — a code that means "the
// autoscaler does not manage this node", said about a cluster of nothing but
// autoscaled nodes. Nothing in it is a question, and nothing names the label.
func TestWithoutPreflightAMismatchReadsAsAFactAboutTheCluster(t *testing.T) {
	s := mismatched(eksNode("ip-10-0-1-11"), eksNode("ip-10-0-1-12"))

	d := engine.Decide(s, config())

	if d.Action != engine.None {
		t.Fatalf("a cluster binpack can map no node in produced a drain: %s", d.Reason)
	}
	if len(d.Assessments) != 2 {
		t.Fatalf("expected both nodes to be assessed, got %d", len(d.Assessments))
	}
	for i := range d.Assessments {
		if got := d.Assessments[i].SkipCode; got != engine.SkipNotAutoscaled {
			t.Fatalf("%s is not out of scope, so this cluster no longer reproduces the "+
				"silent failure preflight exists to catch: %s",
				d.Assessments[i].Node.Name, got)
		}
	}
	if strings.Contains(d.Reason, "nodeGroupIDLabel") {
		t.Errorf("the summary names the configured label, so the mismatch is no longer "+
			"silent and this test is asserting nothing: %s", d.Reason)
	}
}

// TestPreflightAcceptsEveryClusterTheLabelCouldStillBeWorkingOn covers the
// negative of each half of the condition separately, because refusing to run
// is the expensive direction.
//
// binpack refuses only where it has positive evidence that the mapping is
// broken: an autoscaler it can vouch for, groups with nodes in them, and no
// node carrying a value any of those groups would answer to. Drop any one of
// those and what is left is a question binpack cannot answer, not an answer of
// "misconfigured" — and a preflight that stops a cluster over a question is a
// worse failure than the silence it replaced.
func TestPreflightAcceptsEveryClusterTheLabelCouldStillBeWorkingOn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func() engine.Snapshot
	}{
		{
			// The risk this check carries, and the case it must not fire on:
			// an operator halfway through applying the label. One node
			// matching is proof the mapping works, and the rest is arithmetic
			// binpack does every run anyway.
			name: "one node has been relabelled and the rest have not",
			build: func() engine.Snapshot {
				return mismatched(
					eksNode("ip-10-0-1-11", mother.NodeLabels(map[string]string{
						"doks.digitalocean.com/node-pool-id": "asg-a",
					})),
					eksNode("ip-10-0-1-12"),
				)
			},
		},
		{
			// A pool scaled to zero has no node to carry its label, so no
			// node can match and nothing is wrong. Refusing here would take
			// binpack down over a pool that is merely empty — the same
			// false positive TestResolvePoolsAcceptsAPoolKnownOnlyToTheAutoscaler
			// guards on the other check.
			name: "every reported group is scaled to zero",
			build: func() engine.Snapshot {
				s := mismatched(eksNode("ip-10-0-1-11"))
				for i := range s.Autoscaler.Groups {
					s.Autoscaler.Groups[i].Ready = 0
				}
				return s
			},
		},
		{
			// Nothing is published, so there is no value to match against.
			// This is a cluster with no autoscaling at all, which binpack
			// already reports as such.
			name: "the autoscaler reports no groups",
			build: func() engine.Snapshot {
				s := mismatched(eksNode("ip-10-0-1-11"))
				s.Autoscaler.Groups = nil
				return s
			},
		},
		{
			// The groups came from a status document, and a status document
			// nothing is updating says nothing about the cluster now. Live
			// already refuses to act on it, and it names the real problem;
			// a label error over the top of it would send the operator to
			// relabel nodes over an autoscaler that has died.
			name: "no autoscaler is running",
			build: func() engine.Snapshot {
				s := mismatched(eksNode("ip-10-0-1-11"))
				s.Autoscaler.Running = false
				return s
			},
		},
		{
			name: "the autoscaler status is stale",
			build: func() engine.Snapshot {
				s := mismatched(eksNode("ip-10-0-1-11"))
				s.Autoscaler.LastProbe = now.Add(-24 * time.Hour)
				return s
			},
		},
		{
			// There is nothing to report. The error's whole content is the
			// keys the nodes carry, and with no nodes there are none — so it
			// would name a configured key, a published group and an empty
			// list, which is a sentence about a cluster binpack cannot see
			// rather than one it has diagnosed.
			name: "the cluster has no nodes at all",
			build: func() engine.Snapshot {
				s := mismatched()
				// Ready set by hand: mismatched derives it from the node
				// count, so leaving it would satisfy the scaled-to-zero
				// branch too and this case would prove nothing about the
				// one it is here for.
				for i := range s.Autoscaler.Groups {
					s.Autoscaler.Groups[i].Ready = 3
				}
				return s
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := engine.ResolvePools(tc.build(), config()); err != nil {
				t.Errorf("preflight refused a cluster it cannot show is misconfigured:\n%v", err)
			}
		})
	}
}

// TestThePreflightErrorDoesNotDependOnHowTheClusterWasListed pins the same
// property the summary needed, for the same reason.
//
// collect.Snapshot appends nodes in the order the API server listed them and
// sorts nothing, and the group order is the status document's. Both reach this
// error directly: it names every group and every label key it saw. Without an
// order of its own it would reword itself between evaluations with nothing in
// the cluster changing, which is the one thing an operator diffing two runs
// cannot cope with.
//
// Re-running one snapshot cannot see this — it would only prove the function
// is pure. The inputs have to be permuted.
func TestThePreflightErrorDoesNotDependOnHowTheClusterWasListed(t *testing.T) {
	// Distinct keys per node, so the key list is a genuine merge across nodes
	// rather than one node's map repeated.
	a := sized("a", "16Gi", mother.NodeLabels(map[string]string{
		eksNodeGroupLabel: "workers", "kubernetes.io/hostname": "a"}))
	b := sized("b", "16Gi", mother.NodeLabels(map[string]string{
		"topology.kubernetes.io/zone": "eu-west-1a", "kubernetes.io/hostname": "b"}))
	c := sized("c", "16Gi", mother.NodeLabels(map[string]string{
		"node.kubernetes.io/instance-type": "m5.large"}))

	groups := []engine.NodeGroup{
		{ID: "asg-c", MinSize: 1, MaxSize: 10, Ready: 1},
		{ID: "asg-a", MinSize: 1, MaxSize: 10, Ready: 1},
		{ID: "asg-b", MinSize: 1, MaxSize: 10, Ready: 1},
	}
	orders := map[string]struct {
		nodes  []*corev1.Node
		groups []engine.NodeGroup
	}{
		"as listed":     {[]*corev1.Node{a, b, c}, groups},
		"nodes rotated": {[]*corev1.Node{c, a, b}, groups},
		"both reversed": {[]*corev1.Node{c, b, a}, []engine.NodeGroup{groups[2], groups[1], groups[0]}},
		"interleaved":   {[]*corev1.Node{b, c, a}, []engine.NodeGroup{groups[1], groups[2], groups[0]}},
	}

	var first, firstName string
	for name, order := range orders {
		s := cluster(order.nodes, nil)
		s.Autoscaler.Groups = order.groups

		_, err := engine.ResolvePools(s, config())
		if err == nil {
			t.Fatalf("%s: no node carries the configured label and preflight passed", name)
		}
		if first == "" {
			first, firstName = err.Error(), name
			continue
		}
		if err.Error() != first {
			t.Errorf("the same cluster produced two errors, by list order alone:\n"+
				"%s: %s\n%s: %s", firstName, first, name, err)
		}
	}

	// Which order, not only that there is one. Written out rather than sorted
	// here, so a test composing its expectation the way the code does cannot
	// agree with whatever the code starts doing.
	for what, want := range map[string]string{
		"the published groups": "asg-a, asg-b, asg-c",
		"the labels the nodes carry": "eks.amazonaws.com/nodegroup, kubernetes.io/hostname, " +
			"node.kubernetes.io/instance-type, topology.kubernetes.io/zone",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("%s are not listed in sorted order (want %q):\n%s", what, want, first)
		}
	}
}

// TestChosenCandidateDoesNotDependOnInputOrder is the same defect one level
// up: the candidate ordering is least-loaded-first, and a homogeneous pool
// running one workload — the modal binpack cluster — ties routinely.
//
// A tie broken by list order means `explain` names one node and the controller
// drains another, so the command whose whole purpose is to preview a run
// previews a different one. It also means an operator cannot use `explain`
// after the fact to explain what the controller did.
func TestChosenCandidateDoesNotDependOnInputOrder(t *testing.T) {
	a := inPool("a")
	b := inPool("b")
	spare := inPool("spare")
	pods := []*corev1.Pod{
		mother.Pod("default", "on-a", mother.OnNode("a"), mother.Requests("100m", "512Mi")),
		mother.Pod("default", "on-b", mother.OnNode("b"), mother.Requests("100m", "512Mi")),
		mother.Pod("default", "on-spare", mother.OnNode("spare"), mother.Requests("100m", "1Gi")),
	}

	for _, nodes := range [][]*corev1.Node{
		{a, b, spare},
		{b, a, spare},
		{spare, b, a},
	} {
		listed := []string{nodes[0].Name, nodes[1].Name, nodes[2].Name}
		d := engine.Decide(cluster(nodes, pods), config())

		if d.Action != engine.Drain {
			t.Fatalf("listed %v: expected a drain, got none: %s", listed, d.Reason)
		}
		// a and b carry identical workloads, so the choice between them is
		// arbitrary — but it has to be the same arbitrary choice every time,
		// and the name is the only key both frontends see identically.
		if d.Node.Name != "a" {
			t.Errorf("listed %v: drained %s, want a", listed, d.Node.Name)
		}
	}
}

func TestDecideRefusesWhenTheAutoscalerReportsAnUnhealthyCluster(t *testing.T) {
	// autoscalerStatus is Running whenever the process is past start-up — it
	// is the only value the constant set admits besides Initializing — so it
	// says nothing about whether the autoscaler is doing anything. Health is
	// the field that does: below --ok-total-unready-count the autoscaler logs
	// "Cluster is not ready for autoscaling" and returns before any scale-up
	// or scale-down, while still refreshing the probe time. Both of binpack's
	// existing guards pass, and the drain it approves is one nothing will
	// reap — during an incident, which is when the churn is least welcome.
	s := cluster([]*corev1.Node{inPool("a"), inPool("b")}, nil)
	s.Autoscaler.HealthStatus = "Unhealthy"

	d := engine.Decide(s, config())

	if d.Action != engine.None {
		t.Fatalf("an autoscaler reporting an unhealthy cluster will not reap a node, got a drain of %s", d.Node.Name)
	}
	if d.Code != engine.CodeAutoscalerUnhealthy {
		t.Errorf("code = %q, want %q", d.Code, engine.CodeAutoscalerUnhealthy)
	}
	if !strings.Contains(d.Reason, "unhealthy") {
		t.Errorf("reason should say the autoscaler reports the cluster unhealthy, got: %s", d.Reason)
	}
}

func TestLiveNamesWhatTheStatusSaid(t *testing.T) {
	// One sentence covered four different observations, and asserted the one
	// thing binpack had not established in three of them: that no autoscaler
	// is running. A ConfigMap nobody wrote, a ConfigMap with nothing in it and
	// an autoscaler mid-start are different facts about the reader's cluster,
	// and only the first is even consistent with the sentence.
	for _, tc := range []struct {
		name       string
		autoscaler engine.Autoscaler
		want       string
		notWant    string
	}{
		{
			name:       "nothing was found where binpack looked",
			autoscaler: engine.Autoscaler{},
			want:       "no cluster-autoscaler status",
		},
		{
			name:       "the object is there but says nothing",
			autoscaler: engine.Autoscaler{StatusFound: true},
			want:       "carries no autoscalerStatus",
			notWant:    "no cluster-autoscaler is running",
		},
		{
			name:       "the autoscaler named a status of its own",
			autoscaler: engine.Autoscaler{StatusFound: true, ObservedStatus: "Initializing"},
			want:       "Initializing",
			notWant:    "no cluster-autoscaler is running",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, _, why := tc.autoscaler.Live(now)

			if live {
				t.Fatalf("Live() = true for %+v", tc.autoscaler)
			}
			if !strings.Contains(why, tc.want) {
				t.Errorf("reason should contain %q, got: %s", tc.want, why)
			}
			if tc.notWant != "" && strings.Contains(why, tc.notWant) {
				t.Errorf("reason asserts %q, which binpack did not establish: %s", tc.notWant, why)
			}
		})
	}
}

func TestCoolingIgnoresAutoscalerRestart(t *testing.T) {
	// The autoscaler carries scaleUp.lastTransitionTime in process memory and
	// seeds it empty, so its first scan after a restart finds the condition
	// changed and stamps the transition with that scan's own probe time. A
	// managed control plane restarts the autoscaler for its own reasons, and
	// binpack read every one of those as a cluster that had just grown.
	nodes := []*corev1.Node{inPool("a"), inPool("b")}
	cfg := config()
	cfg.Default.CooldownAfterScaleUp = 10 * time.Minute

	restarted := cluster(nodes, nil)
	restarted.Autoscaler.LastScaleUp = restarted.Autoscaler.LastProbe

	if d := engine.Decide(restarted, cfg); d.Action != engine.Drain {
		t.Errorf("a restart is not a scale-up, got %s: %s", d.Code, d.Reason)
	}

	// The same document one scan later, with a transition that predates
	// everything binpack has seen this autoscaler publish: a real scale-up.
	grew := cluster(nodes, nil)
	grew.Autoscaler.LastScaleUp = grew.Autoscaler.LastProbe.Add(-time.Second)

	d := engine.Decide(grew, cfg)
	if d.Action != engine.None {
		t.Fatalf("a scale-up a second before the last scan is still a scale-up, got a drain of %s", d.Node.Name)
	}
	if a := assessmentFor(d, "a"); a == nil || a.SkipCode != engine.SkipCooldownAfterScaleUp {
		t.Errorf("skip code = %+v, want %s", a, engine.SkipCooldownAfterScaleUp)
	}
}

func TestCoolingReadsTheScaleUpStampAgainstWhatBinpackHasSeen(t *testing.T) {
	// One scan after a restart the phantom stamp is just an old timestamp,
	// and nothing in the document says otherwise — so the single-document
	// reading credits it and the cluster stands down for a scale-up that did
	// not happen. What the controller carries across evaluations is what
	// settles it.
	nodes := []*corev1.Node{inPool("a"), inPool("b")}
	cfg := config()
	cfg.Default.CooldownAfterScaleUp = 10 * time.Minute

	for _, tc := range []struct {
		name       string
		autoscaler func(*engine.Autoscaler)
		want       engine.Action
	}{
		{
			name: "the stamp is pinned to the first scan binpack ever saw",
			autoscaler: func(a *engine.Autoscaler) {
				a.LastScaleUp = a.LastProbe.Add(-time.Minute)
				a.EarliestProbe = a.LastScaleUp
			},
			want: engine.Drain,
		},
		{
			name: "the stamp predates everything binpack has seen",
			autoscaler: func(a *engine.Autoscaler) {
				a.LastScaleUp = a.LastProbe.Add(-time.Minute)
				a.EarliestProbe = a.LastProbe.Add(-30 * time.Second)
			},
			want: engine.None,
		},
		{
			name: "binpack watched this one grow",
			autoscaler: func(a *engine.Autoscaler) {
				a.LastScaleUp = a.LastProbe.Add(-time.Minute)
				a.EarliestProbe = a.LastProbe.Add(-2 * time.Hour)
				a.WatchedScaleUp = a.LastScaleUp
			},
			want: engine.None,
		},
		{
			name: "an older scale-up binpack watched does not vouch for a later stamp",
			autoscaler: func(a *engine.Autoscaler) {
				a.LastScaleUp = a.LastProbe.Add(-time.Minute)
				a.EarliestProbe = a.LastProbe.Add(-2 * time.Hour)
				a.WatchedScaleUp = a.LastProbe.Add(-time.Hour)
			},
			want: engine.Drain,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := cluster(nodes, nil)
			tc.autoscaler(&s.Autoscaler)

			if d := engine.Decide(s, cfg); d.Action != tc.want {
				t.Errorf("action = %s, want %s (%s: %s)", d.Action, tc.want, d.Code, d.Reason)
			}
		})
	}
}

// TestDrainableEvictConfig is the engine half of making the autoscaler's two
// skip flags configurable.
//
// It passes without any change to the engine, and that is the finding rather
// than a reason to skip it: EvictConfig has always had the field, drainable
// has always read it, and nothing an operator can write ever reached it — so
// on a cluster whose autoscaler runs --skip-nodes-with-local-storage=false
// binpack modelled the opposite policy and refused every node hosting an
// emptyDir pod, with no way to say otherwise. The half that fails today is in
// api/v1alpha1 and internal/cli, where the document meets this struct.
//
// b is annotated skip so it is a destination and not a candidate, leaving a as
// the only node under consideration.
func TestDrainableEvictConfig(t *testing.T) {
	nodes := []*corev1.Node{
		inPool("a"),
		inPool("b", mother.NodeAnnotations(map[string]string{engine.AnnotationSkip: "true"})),
	}
	pods := []*corev1.Pod{
		mother.Pod("default", "cache", mother.OnNode("a"),
			mother.WithEmptyDir("scratch"), mother.Requests("100m", "128Mi")),
	}

	t.Run("the autoscaler's default, which binpack has always assumed", func(t *testing.T) {
		d := engine.Decide(cluster(nodes, pods), config())

		if d.Action != engine.None {
			t.Fatalf("expected no drain, got %s of %s", d.Action, d.Node.Name)
		}
		a := assessmentFor(d, "a")
		if a == nil || len(a.Blockers) != 1 || a.Blockers[0].Code != engine.BlockedLocalStorage {
			t.Fatalf("want a single %s blocker on a, got %+v", engine.BlockedLocalStorage, a)
		}
	})

	t.Run("an autoscaler told every emptyDir is scratch", func(t *testing.T) {
		cfg := config()
		cfg.Default.Evict.SkipNodesWithLocalStorage = false

		d := engine.Decide(cluster(nodes, pods), cfg)

		if d.Action != engine.Drain || d.Node.Name != "a" {
			t.Fatalf("expected a to be drainable, got %s of %s: %s", d.Action, d.Node.Name, d.Reason)
		}
	})
}

// TestTheSystemPodGraceIsMeasuredAgainstTheSnapshotsClock holds the wiring
// rather than the rule.
//
// CheckEvictable gained a clock for this, and a clock is the kind of argument
// that can be threaded correctly in the unit test and dropped at the one call
// site that matters: a zero time reaches every pod as one created after the
// evaluation, so every kube-system pod blocks again and the whole rule quietly
// reverts. Nothing else in the suite notices, because everything else asks
// CheckEvictable directly.
//
// b is annotated skip so it is a destination and not a candidate, leaving a as
// the only node under consideration.
func TestTheSystemPodGraceIsMeasuredAgainstTheSnapshotsClock(t *testing.T) {
	nodes := []*corev1.Node{
		inPool("a"),
		inPool("b", mother.NodeAnnotations(map[string]string{engine.AnnotationSkip: "true"})),
	}
	pods := []*corev1.Pod{
		mother.Pod("kube-system", "metrics-server", mother.OnNode("a"),
			mother.CreatedAt(now.Add(-2*time.Hour)), mother.Requests("100m", "128Mi")),
	}

	d := engine.Decide(cluster(nodes, pods), config())

	if d.Action != engine.Drain || d.Node.Name != "a" {
		t.Fatalf("expected a to be drainable, got %s of %s: %s — the pod is two hours old, "+
			"so the autoscaler would evict it and take the node",
			d.Action, d.Node.Name, d.Reason)
	}
}
