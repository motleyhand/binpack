package engine_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"

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

	live, why := s.Autoscaler.Live(s.Now)
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
	if a := assessmentFor(d, "a"); a == nil || !strings.Contains(a.SkipReason, "payments") {
		t.Errorf("reason should name the namespace, got %+v", a)
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

func TestCheckPoolsNamesEveryUnknownPoolInAStableOrder(t *testing.T) {
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

	first := engine.CheckPools(s, cfg)
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
		if got := engine.CheckPools(s, cfg); got.Error() != first.Error() {
			t.Fatalf("the message varies between runs:\n %v\n %v", first, got)
		}
	}
}

func TestCheckPoolsAcceptsAPoolKnownOnlyToTheAutoscaler(t *testing.T) {
	// A pool scaled to zero has no nodes to carry its label, but the
	// autoscaler still reports it. Rejecting the override then would take
	// binpack down over a pool that is merely empty.
	s := cluster(nil, nil)
	s.Autoscaler.Groups = []engine.NodeGroup{
		{ID: poolID, MinSize: 0, MaxSize: 10, Ready: 0},
	}
	cfg := config()
	cfg.ByPool = map[string]engine.Policy{poolID: {Enabled: false}}

	if err := engine.CheckPools(s, cfg); err != nil {
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
	}

	seen := map[string]bool{}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := engine.Decide(tc.build(), tc.cfg)

			var codes []string
			for _, a := range d.Assessments {
				if a.Skipped {
					codes = append(codes, a.SkipCode)
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
	for _, code := range []string{
		engine.SkipNotAutoscaled, engine.SkipScaleUpInProgress,
		engine.SkipCooldownAfterScaleUp, engine.SkipCooldownAfterDrain,
		engine.SkipPoolAtMinimum, engine.SkipAnnotated, engine.SkipDrainInProgress,
		engine.SkipBackoff, engine.SkipCordoned, engine.SkipBeingRemoved,
	} {
		if !seen[code] {
			t.Errorf("no case reaches %q", code)
		}
	}
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

// TestTheSummaryDoesNotDependOnMapOrder pins the half of counting by code that
// the tables above cannot reach.
//
// Which of two equally common codes is named is arbitrary and deliberately not
// asserted; that the same cluster gives the same answer twice is not. The
// counting is a map, so dropping the tie-break would leave a sentence that
// changes between evaluations with nothing in the cluster changing — and the
// operator reading it comparing two log lines that disagree.
func TestTheSummaryDoesNotDependOnMapOrder(t *testing.T) {
	nodes := []*corev1.Node{
		backingOff("a", now.Add(time.Hour), ""),
		backingOff("b", now.Add(45*time.Minute), ""),
		inPool("c", mother.Cordoned()),
		inPool("d", mother.Cordoned()),
	}
	s, cfg := cluster(nodes, nil), config()

	first := engine.Decide(s, cfg).Reason
	if !strings.Contains(first, "2 of 4") {
		t.Fatalf("neither tied code covers the nodes it should, got: %s", first)
	}
	for i := 0; i < 32; i++ {
		if again := engine.Decide(s, cfg).Reason; again != first {
			t.Fatalf("the same cluster summarised two ways:\n%s\n%s", first, again)
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
