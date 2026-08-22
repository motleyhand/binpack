package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"

	"github.com/motleyhand/binpack/api/v1alpha1"
	"github.com/motleyhand/binpack/internal/drain"
	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

const (
	explainPoolID   = "da8977ba-244f"
	explainPoolName = "pool-4g"
)

var explainNow = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func explainNode(name string, opts ...mother.NodeOption) *corev1.Node {
	return mother.SmallNode(name,
		append([]mother.NodeOption{mother.InPool(explainPoolName, explainPoolID)}, opts...)...)
}

// draining is a node binpack is part-way through emptying: cordoned, marked,
// and — because a pod has already been evicted from it — committed.
func draining(name string) *corev1.Node {
	return explainNode(name, mother.Cordoned(), mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainStarted:  explainNow.Add(-5 * time.Minute).Format(time.RFC3339),
		engine.AnnotationDrainAwaiting: engine.AwaitingSettled,
	}))
}

func explainCluster(nodes []*corev1.Node, pods []*corev1.Pod,
	pdbs ...*policyv1.PodDisruptionBudget) engine.Snapshot {
	return engine.Snapshot{
		Nodes:     nodes,
		Pods:      pods,
		PDBs:      pdbs,
		Templates: mother.Templates(pods...),
		Now:       explainNow,
		Autoscaler: engine.Autoscaler{
			Running:   true,
			LastProbe: explainNow.Add(-10 * time.Second),
			Groups: []engine.NodeGroup{
				{ID: explainPoolID, MinSize: 1, MaxSize: 10, Ready: len(nodes)},
			},
		},
	}
}

func explainConfig() engine.Config {
	return engine.Config{
		NodeGroupIDLabel: "doks.digitalocean.com/node-pool-id",
		PoolNameLabel:    explainPoolName + "-label",
		Default: engine.Policy{
			Enabled: true,
			Sim:     engine.SimConfig{ExpendablePriorityCutoff: -10},
			Evict:   engine.DefaultEvictConfig(),
			// The shipped defaults. Left at zero every drain reads as stalled
			// the instant it starts, which would make the ordinary fixtures
			// describe a cluster nobody has.
			StallTimeout:   10 * time.Minute,
			RemovalTimeout: 15 * time.Minute,
		},
	}
}

// renderedExplain runs the command's own two steps — decide, then render — so
// a test asserts on the bytes an operator would see rather than on a decision
// nothing formats.
func renderedExplain(t *testing.T, format outputFormat, s engine.Snapshot, cfg engine.Config) string {
	t.Helper()
	var buf bytes.Buffer
	if err := renderExplain(&options{output: format, out: &buf}, s, explainOutcome(s, cfg)); err != nil {
		t.Fatalf("rendering: %v", err)
	}
	return buf.String()
}

// TestExplainReportsTheDrainInProgressRatherThanASecondNode is the falsehood
// this command could tell in the window an operator is likeliest to run it.
//
// A cordoned node and a drain that has been running for twenty minutes is
// exactly when somebody asks binpack what it is doing — and the answer was the
// name of a different node, in the text output and in the JSON `action` any
// automation branches on.
func TestExplainReportsTheDrainInProgressRatherThanASecondNode(t *testing.T) {
	s := explainCluster(
		[]*corev1.Node{draining("node-a"), explainNode("node-b"), explainNode("node-c")},
		[]*corev1.Pod{mother.Pod("default", "web", mother.OnNode("node-a"))})

	text := renderedExplain(t, outputText, s, explainConfig())
	if strings.Contains(text, "would drain") {
		t.Errorf("explain announced a drain while one was in progress:\n%s", text)
	}
	if !strings.Contains(text, "node-a") {
		t.Errorf("explain did not name the node being drained:\n%s", text)
	}
	// Narrowed, not lost. Answering "which node would you pick" with "none" is
	// only honest if the reasoning survives it — the whole cluster is still
	// assessed and every node still carries a verdict.
	for _, node := range []string{"node-a", "node-b", "node-c"} {
		if !strings.Contains(text, node) {
			t.Errorf("%s is missing from the node table:\n%s", node, text)
		}
	}

	// "nothing to do" is the other falsehood available here, and it is the one
	// the branch would fall through to: binpack is not idle during a drain, it
	// is advancing one, and an operator told there is nothing to do has been
	// given the wrong reason to go looking elsewhere.
	if strings.Contains(text, "nothing to do") {
		t.Errorf("explain said there was nothing to do while advancing a drain:\n%s", text)
	}
	if !strings.Contains(text, "advances that drain") {
		t.Errorf("explain did not say what binpack will do instead:\n%s", text)
	}

	var view explainView
	if err := json.Unmarshal([]byte(renderedExplain(t, outputJSON, s, explainConfig())), &view); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}
	if view.Code != engine.CodeDraining {
		t.Errorf("json code = %q, want %q", view.Code, engine.CodeDraining)
	}
	if view.Drain == nil || view.Drain.Node != "node-a" {
		t.Errorf("json drain = %+v, want the node being drained", view.Drain)
	}
}

// TestExplainShowsTheRevalidationVerdictForTheNodeBeingDrained is the arithmetic
// the headline fix does not reach.
//
// Every step of a drain re-asks whether the node is still drainable, and
// abandons it when the answer turns. That question is asked with the drain's
// own flags — the marker and the cordon ignored, the reserve suppressed once
// pods have moved — and explain asked a different one, reporting the node as
// "skipped: a drain is already in progress on this node". Which is true, and
// tells an operator watching a stuck drain nothing whatsoever.
func TestExplainShowsTheRevalidationVerdictForTheNodeBeingDrained(t *testing.T) {
	pod := mother.Pod("default", "web", mother.OnNode("node-a"),
		mother.ControlledBy("ReplicaSet", "web"),
		mother.PodLabels(map[string]string{"app": "web"}))

	s := explainCluster(
		[]*corev1.Node{draining("node-a"), explainNode("node-b"), explainNode("node-c")},
		[]*corev1.Pod{pod},
		mother.PDB("default", "web", 0, map[string]string{"app": "web"}))

	var view explainView
	if err := json.Unmarshal([]byte(renderedExplain(t, outputJSON, s, explainConfig())), &view); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}

	var row *nodeReport
	for i := range view.Nodes {
		if view.Nodes[i].Name == "node-a" {
			row = &view.Nodes[i]
		}
	}
	if row == nil {
		t.Fatal("the node being drained is missing from the report")
	}
	if row.Verdict != engine.VerdictBlocked {
		t.Errorf("node-a verdict = %q (%s), want %q — the verdict the next drain step acts on",
			row.Verdict, row.Detail, engine.VerdictBlocked)
	}
	if len(row.Blockers) == 0 {
		t.Fatal("node-a reports no blocker, so nothing says why the drain is about to be abandoned")
	}
	if !strings.Contains(strings.Join(row.Blockers, " "), "default/web") {
		t.Errorf("blockers = %v, want the pod the budget refuses", row.Blockers)
	}

	// Marked, in both renderings. This row answers a different question from
	// every other row in the table — whether a drain already under way
	// survives, rather than whether a drain would be worth starting — and a
	// reader who cannot tell which was asked will read the wrong one.
	if !row.Draining {
		t.Error("the node being drained is not marked as such in the JSON")
	}
	for _, r := range view.Nodes {
		if r.Name != "node-a" && r.Draining {
			t.Errorf("%s is marked as being drained, and it is not", r.Name)
		}
	}
	if !strings.Contains(renderedExplain(t, outputText, s, explainConfig()), "> node-a") {
		t.Error("the node being drained carries no marker in the text output")
	}
}

// TestExplainStillPreviewsAnOrdinaryDrain is the negative half of both drain
// branches, and it is the half a wider condition takes away silently.
//
// The rows and the footer now ask "is a drain in progress?" before deciding
// what to print, and a condition that answered yes too readily would rewrite
// the ordinary preview instead: the chosen node's assessment replaced by a
// revalidation that never chose it, and its row marked as one binpack is
// already emptying.
func TestExplainStillPreviewsAnOrdinaryDrain(t *testing.T) {
	s := explainCluster(
		[]*corev1.Node{explainNode("node-a"), explainNode("node-b"), explainNode("node-c")},
		[]*corev1.Pod{mother.Pod("default", "web", mother.OnNode("node-a"))})

	text := renderedExplain(t, outputText, s, explainConfig())
	if !strings.Contains(text, "would drain") {
		t.Errorf("explain previewed no drain with nothing in progress:\n%s", text)
	}
	if strings.Contains(text, "> ") {
		t.Errorf("a node is marked as being drained with no drain in progress:\n%s", text)
	}

	var view explainView
	if err := json.Unmarshal([]byte(renderedExplain(t, outputJSON, s, explainConfig())), &view); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}
	if view.Code != engine.CodeDrain {
		t.Errorf("json code = %q, want %q", view.Code, engine.CodeDrain)
	}

	var chosen int
	for _, r := range view.Nodes {
		if r.Draining {
			t.Errorf("%s is marked as being drained with no drain in progress", r.Name)
		}
		if r.Chosen {
			chosen++
			if r.Name != view.Node {
				t.Errorf("%s is marked chosen but the decision names %s", r.Name, view.Node)
			}
		}
	}
	if chosen != 1 {
		t.Errorf("%d nodes marked chosen, want exactly the one being drained", chosen)
	}
}

// TestExplainSaysADrainHasPassedItsBound is Codex's first finding on #64, and
// it is the case an operator is likeliest to be running this command for.
//
// Revalidation and the drain's own bounds answer different questions. A node
// whose pods still fit elsewhere revalidates drainable however long it has been
// stuck, because "could this node be emptied" is not "is this drain getting
// anywhere" — and executor.Advance asks both, abandoning on the second. So a
// row reading `drainable` under a footer saying that verdict decides whether
// the drain continues is exactly wrong for a drain about to be handed back.
func TestExplainSaysADrainHasPassedItsBound(t *testing.T) {
	stalled := explainNode("node-a", mother.Cordoned(), mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainStarted:  explainNow.Add(-2 * time.Hour).Format(time.RFC3339),
		engine.AnnotationDrainAwaiting: engine.AwaitingSettled,
		engine.AnnotationDrainProgress: explainNow.Add(-90 * time.Minute).Format(time.RFC3339),
	}))

	s := explainCluster(
		[]*corev1.Node{stalled, explainNode("node-b"), explainNode("node-c")},
		[]*corev1.Pod{mother.Pod("default", "web", mother.OnNode("node-a"))})

	cfg := explainConfig()
	cfg.Default.StallTimeout = 10 * time.Minute
	cfg.Default.RemovalTimeout = 15 * time.Minute

	text := renderedExplain(t, outputText, s, cfg)
	if !strings.Contains(text, "no progress") {
		t.Errorf("explain did not say the drain has stalled:\n%s", text)
	}
	if strings.Contains(text, "still drainable") {
		t.Errorf("explain called a stalled drain drainable and stopped there:\n%s", text)
	}
}

// TestExplainReportsADrainWhenTheAutoscalerHasGone is Codex's second finding on
// #64: the drain-in-progress gate sits below Decide's liveness check, so it
// never runs when there is no live autoscaler — and the text renderer returns
// before the node table.
//
// That is not a corner: an autoscaler that dies mid-drain is the one condition
// revalidation refuses to continue a drain through, so `run` hands the node
// back on its very next tick. explain reported the dead autoscaler and nothing
// whatsoever about the cordoned node, which is the state somebody is staring at.
func TestExplainReportsADrainWhenTheAutoscalerHasGone(t *testing.T) {
	s := explainCluster(
		[]*corev1.Node{draining("node-a"), explainNode("node-b")},
		[]*corev1.Pod{mother.Pod("default", "web", mother.OnNode("node-a"))})
	s.Autoscaler = engine.Autoscaler{}

	text := renderedExplain(t, outputText, s, explainConfig())
	if !strings.Contains(text, "node-a") {
		t.Errorf("explain hid the drain in progress behind the dead autoscaler:\n%s", text)
	}

	var view explainView
	if err := json.Unmarshal([]byte(renderedExplain(t, outputJSON, s, explainConfig())), &view); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}
	if view.Drain == nil || view.Drain.Node != "node-a" {
		t.Errorf("json drain = %+v, want the node being drained", view.Drain)
	}
}

func TestEnginePolicyCarriesEveryResolvedPolicyField(t *testing.T) {
	// enginePolicy is the only translation from a resolved configuration into
	// what the engine and executor read, and every cluster command goes
	// through it. A field it drops is one the schema accepts, the defaults
	// fill in, validation enforces and `config validate` prints back — and
	// that nothing acts on. The whole suite stays green either way, because
	// the engine tests start from an already-built engine.Config and the
	// config tests stop at the resolved PoolPolicy: this is the seam between
	// them, and nothing else stands on it.
	//
	// Distinct values, so a field wired to its neighbour fails as loudly as
	// one wired to nothing.
	resolved := v1alpha1.PoolPolicy{
		Enabled:                  true,
		ExpendablePriorityCutoff: -7,
		ReserveForLargestPod:     true,
		MaxPodsPerDrain:          3,
		StallTimeout:             11 * time.Minute,
		RemovalTimeout:           17 * time.Minute,
		BackoffInitial:           5 * time.Minute,
		BackoffMax:               72 * time.Hour,
		CooldownAfterScaleUp:     13 * time.Minute,
		CooldownAfterDrain:       19 * time.Minute,
		ExcludedNamespaces:       []string{"kube-system"},
	}

	// Counted from the struct, not taken on trust. Every field below is
	// carried; nothing is deliberately dropped. engine.Policy.Evict is the
	// one member with no counterpart here, and it comes from
	// engine.DefaultEvictConfig() rather than from the configuration at all.
	//
	// A Fatal rather than an Error: once the counts disagree the assertions
	// below are answering a question about a different struct, and a
	// twelfth field asserted by nothing is exactly what this test exists to
	// prevent.
	const carried = 11
	if n := reflect.TypeOf(v1alpha1.PoolPolicy{}).NumField(); n != carried {
		t.Fatalf("PoolPolicy has %d fields and this test asserts %d: "+
			"carry the new one into engine.Policy and assert it here, or say "+
			"here why it is not carried", n, carried)
	}

	got := enginePolicy(resolved)

	for _, tc := range []struct {
		field     string
		got, want any
	}{
		{"Enabled", got.Enabled, resolved.Enabled},
		{"Sim.ExpendablePriorityCutoff", got.Sim.ExpendablePriorityCutoff, resolved.ExpendablePriorityCutoff},
		{"Sim.ReserveForLargestPod", got.Sim.ReserveForLargestPod, resolved.ReserveForLargestPod},
		{"MaxPodsPerDrain", got.MaxPodsPerDrain, resolved.MaxPodsPerDrain},
		{"StallTimeout", got.StallTimeout, resolved.StallTimeout},
		{"RemovalTimeout", got.RemovalTimeout, resolved.RemovalTimeout},
		{"BackoffInitial", got.BackoffInitial, resolved.BackoffInitial},
		{"BackoffMax", got.BackoffMax, resolved.BackoffMax},
		{"CooldownAfterScaleUp", got.CooldownAfterScaleUp, resolved.CooldownAfterScaleUp},
		{"CooldownAfterDrain", got.CooldownAfterDrain, resolved.CooldownAfterDrain},
		{"ExcludedNamespaces", got.ExcludedNamespaces, resolved.ExcludedNamespaces},
	} {
		if !reflect.DeepEqual(tc.got, tc.want) {
			t.Errorf("engine.Policy.%s: got %v, want the configured %v",
				tc.field, tc.got, tc.want)
		}
	}

	// The two booleans are indistinguishable by value while both are true, so
	// a crossed pair reads as correct above. One case with them differing
	// pins which is which.
	crossed := enginePolicy(v1alpha1.PoolPolicy{Enabled: true})
	if !crossed.Enabled || crossed.Sim.ReserveForLargestPod {
		t.Errorf("the two booleans are crossed: Enabled=%v, ReserveForLargestPod=%v, want true and false",
			crossed.Enabled, crossed.Sim.ReserveForLargestPod)
	}
}

func TestAnUnconfiguredBackoffStillWaitsTheDocumentedDefault(t *testing.T) {
	// The other direction of the same seam. Carrying the two fields through
	// replaced a pair of constants in internal/drain with the defaults in
	// api/v1alpha1, and the case with nothing configured is the one that used
	// to be right by coincidence: the constants happened to equal the
	// defaults, and nothing held them together.
	//
	// Literal durations rather than the Default* constants, because asserting
	// a constant against itself passes however far the documented promise
	// moves. These two are what docs/reference/configuration.md tells an
	// operator they get when they set nothing.
	//
	// Through the whole route — enginePolicy, then drain.PolicyFor, then the
	// arithmetic — because every one of those was a place the value could
	// have stopped, and asserting the field alone would not notice.
	node := explainNode("a")
	policy := drain.PolicyFor(
		engineConfig(new(v1alpha1.Config)),
		explainCluster([]*corev1.Node{node}, nil),
		node.Name)

	for _, tc := range []struct {
		name     string
		attempts string
		want     time.Duration
	}{
		{"the first failure", "", 30 * time.Minute},
		{"the twentieth, long past the cap", "19", 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failed := explainNode("a", mother.NodeAnnotations(
				map[string]string{engine.AnnotationDrainAttempts: tc.attempts}))

			_, until := drain.Backoff(failed, explainNow, policy)

			if got := until.Sub(explainNow); got != tc.want {
				t.Errorf("an unconfigured install waits %s after %s, want the documented %s",
					got, tc.name, tc.want)
			}
		})
	}
}
