package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/motleyhand/binpack/api/v1alpha1"
	"github.com/motleyhand/binpack/internal/collect"
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
	if n := reflect.TypeFor[v1alpha1.PoolPolicy]().NumField(); n != carried {
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

// renderedExplainIn is renderedExplain with the command's own options in
// scope, for the two questions that are about the deployed binpack rather than
// about the cluster: which configuration was read, and whether it acts.
func renderedExplainIn(t *testing.T, opts *options, s engine.Snapshot, cfg engine.Config) string {
	t.Helper()
	var buf bytes.Buffer
	opts.out = &buf
	if err := renderExplain(opts, s, explainOutcome(s, cfg)); err != nil {
		t.Fatalf("rendering: %v", err)
	}
	return buf.String()
}

// crowded is a cluster in the state binpack exists for and cannot help with:
// every node holds more than any other node has room for, so every one of them
// is infeasible and every destination refuses for a real, different reason.
func crowded(n int) engine.Snapshot {
	nodes := make([]*corev1.Node, 0, n)
	pods := make([]*corev1.Pod, 0, n)
	for i := range n {
		name := fmt.Sprintf("node-%03d", i)
		nodes = append(nodes, explainNode(name))
		pods = append(pods, mother.Pod("default", fmt.Sprintf("web-%03d", i),
			mother.OnNode(name), mother.Requests("100m", "1000Mi")))
	}
	return explainCluster(nodes, pods)
}

// TestExplainStaysReadableOnALargeCluster is the output an operator gets from
// the command the install guide tells them to run first.
//
// One refusal line per destination per infeasible node is quadratic, and the
// state that produces it is not a corner — it is a cluster with nothing left
// to consolidate, which is the steady state binpack is pointed at. At a
// hundred nodes that was ten thousand lines with the answer on the last one,
// so `head` showed nothing and `less` meant paging past everything.
func TestExplainStaysReadableOnALargeCluster(t *testing.T) {
	const nodes = 100

	text := renderedExplain(t, outputText, crowded(nodes), explainConfig())
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")

	if len(lines) > 500 {
		t.Errorf("explain printed %d lines for %d nodes, want an output somebody can read",
			len(lines), nodes)
	}

	// The answer has to survive `head`, which is what a reader reaches for
	// when a command produces more than a screen.
	verdict := -1
	for i, line := range lines {
		if strings.Contains(line, "nothing to do") {
			verdict = i
			break
		}
	}
	if verdict < 0 {
		t.Fatalf("no verdict line anywhere in %d lines", len(lines))
	}
	if verdict >= 20 {
		t.Errorf("the verdict is on line %d, past anything `head` would show", verdict+1)
	}

	// Capping is only safe if the reader is told it happened. A list silently
	// truncated to its first few entries reads as the whole list, and the
	// destination somebody came here about is the one that got cut.
	if !strings.Contains(text, "more destination") {
		t.Error("the refusal lists are capped and nothing says so")
	}
}

// TestTheFullRefusalListSurvivesInJSON is the other half of the cap: the text
// output is for a person and may summarise, the JSON is for a machine and may
// not. A consumer that has to re-run the command with a different flag to see
// the data it is parsing has been given a report it cannot check.
func TestTheFullRefusalListSurvivesInJSON(t *testing.T) {
	const nodes = 12

	var view explainView
	if err := json.Unmarshal([]byte(renderedExplain(t, outputJSON, crowded(nodes), explainConfig())), &view); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}

	for _, r := range view.Nodes {
		if r.Verdict != engine.VerdictInfeasible {
			t.Fatalf("%s is %s, want every node infeasible in this cluster", r.Name, r.Verdict)
		}
		// Every other node in the cluster refused, and the JSON says so about
		// each of them.
		if len(r.Refusals) != nodes-1 {
			t.Errorf("%s carries %d refusals, want all %d destinations",
				r.Name, len(r.Refusals), nodes-1)
		}
	}
}

// TestARefusalListIsCappedOnlyWhenItHasTo is the risk the cap creates, in
// both directions.
//
// A cap that fires on a list that fits replaces information with a summary of
// itself, which is strictly worse; one that miscounts what it dropped sends
// the reader looking for destinations that are not there. The list an operator
// came here to read is what this is about, so the tail has to be exact.
func TestARefusalListIsCappedOnlyWhenItHasTo(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nodes int
		want  string
	}{
		// Three destinations, three lines: the cap bounds the block, it is not
		// a rule that every list of some length gets summarised.
		{"a list that fits is printed whole", maxRefusalLines + 1, ""},
		{"one destination past the cap", maxRefusalLines + 2, "(and 2 more destinations refused"},
		{"a hundred past it", maxRefusalLines + 102, "(and 102 more destinations refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := renderedExplain(t, outputText, crowded(tc.nodes), explainConfig())

			if tc.want != "" {
				if !strings.Contains(text, tc.want) {
					t.Errorf("the tail does not say %q:\n%s", tc.want, text)
				}
				return
			}
			if strings.Contains(text, "more destinations refused") {
				t.Errorf("a list of %d was summarised under a cap of %d:\n%s",
					tc.nodes-1, maxRefusalLines, text)
			}
			// Every node refuses every other one, and with the list inside
			// the cap every one of those lines is printed.
			if got, want := strings.Count(text, "    - "), tc.nodes*(tc.nodes-1); got != want {
				t.Errorf("%d refusal lines printed, want all %d:\n%s", got, want, text)
			}
		})
	}
}

// TestExplainSaysWhetherTheDeployedBinpackWillAct is the one setting that
// decides whether the verdict above it is a plan or a prediction.
//
// `kubectl exec deploy/binpack -- binpack explain` reads the deployed
// configuration precisely so its answer is about the deployed binpack. dryRun
// is in that file, explain has already parsed it, and it was the one thing the
// output withheld — under a footer whose only occurrence of the words "dry
// run" was about explain itself.
func TestExplainSaysWhetherTheDeployedBinpackWillAct(t *testing.T) {
	// The workload sits on node-b, so node-a is the least loaded and the one
	// chosen — which is the node the sentences below have to name.
	s := explainCluster(
		[]*corev1.Node{explainNode("node-a"), explainNode("node-b")},
		[]*corev1.Pod{mother.Pod("default", "web", mother.OnNode("node-b"))})

	reporting := renderedExplainIn(t, &options{output: outputText, dryRun: true}, s, explainConfig())
	acting := renderedExplainIn(t, &options{output: outputText, dryRun: false}, s, explainConfig())

	if reporting == acting {
		t.Fatalf("explain describes a binpack that drains and one that does not identically:\n%s", acting)
	}

	// Each names its own mode and not the other's. Asserting only that the two
	// differ passes on any difference at all, including one that says nothing
	// about the mode.
	for _, tc := range []struct{ name, text, want, notWant string }{
		{"reporting", reporting, "dryRun: true", "dryRun: false"},
		{"acting", acting, "dryRun: false", "dryRun: true"},
	} {
		if !strings.Contains(tc.text, tc.want) {
			t.Errorf("the %s output does not name its mode (%s):\n%s", tc.name, tc.want, tc.text)
		}
		if strings.Contains(tc.text, tc.notWant) {
			t.Errorf("the %s output names the other mode (%s):\n%s", tc.name, tc.notWant, tc.text)
		}
	}

	// And what it means for the node just named, which is where the reading
	// failure was: "would drain node-a" followed by a parenthesis containing
	// the words "dry run" is a statement about the deployment to anybody
	// reading at speed, and it was a statement about the reading tool.
	if !strings.Contains(reporting, "not act on it") {
		t.Errorf("the reporting output does not say what happens to the node it named:\n%s", reporting)
	}
	if !strings.Contains(acting, "will cordon node-a and begin evicting") {
		t.Errorf("the acting output does not say what happens to the node it named:\n%s", acting)
	}
	// The parenthesis stays, with its subject made explicit. It is a true and
	// useful thing to say in both modes, and it is not the mode.
	for _, text := range []string{reporting, acting} {
		if !strings.Contains(text, "explain itself never changes anything") {
			t.Errorf("explain no longer says it is read-only:\n%s", text)
		}
	}

	// And on the verdict that is not a preview at all. A mode named only in
	// the drain footer says nothing on the two outcomes an operator meets most
	// — nothing to do, and a drain already under way — where the question
	// "will this binpack act on what it just told me" is the same question.
	idle := explainCluster(
		[]*corev1.Node{explainNode("node-a")},
		[]*corev1.Pod{mother.Pod("default", "web", mother.OnNode("node-a"))})
	for _, tc := range []struct {
		dryRun        bool
		want, notWant string
	}{
		{true, "dryRun: true", "dryRun: false"},
		{false, "dryRun: false", "dryRun: true"},
	} {
		text := renderedExplainIn(t, &options{output: outputText, dryRun: tc.dryRun}, idle, explainConfig())
		if !strings.Contains(text, "nothing to do") {
			t.Fatalf("the fixture no longer refuses:\n%s", text)
		}
		if !strings.Contains(text, tc.want) || strings.Contains(text, tc.notWant) {
			t.Errorf("a report with nothing to preview does not name its mode (%s):\n%s", tc.want, text)
		}
	}

	// The mode is a fact about the deployment, so a machine reading the same
	// report gets it too.
	var view explainView
	if err := json.Unmarshal(
		[]byte(renderedExplainIn(t, &options{output: outputJSON, dryRun: false}, s, explainConfig())),
		&view); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}
	if view.DryRun {
		t.Error("the JSON reports dry run for a binpack configured to act")
	}
}

// TestExplainSaysHowManyPodsWouldMove is the arithmetic the command's own
// summary promises and the row it exists for did not carry.
//
// The Event describing the identical decision says how many pods move, from
// the same field. The text output dropped it, so the starred row was the only
// one in the table with an empty detail column — which reads as a rendering
// fault rather than as a deliberate silence.
func TestExplainSaysHowManyPodsWouldMove(t *testing.T) {
	// Candidates are tried least loaded first, so the chosen node is the one
	// running least — and it still has to be running something for there to
	// be any arithmetic to print.
	pods := []*corev1.Pod{
		mother.Pod("default", "web", mother.OnNode("node-a")),
		mother.Pod("default", "api", mother.OnNode("node-a")),
	}
	for _, node := range []string{"node-b", "node-c"} {
		for i := range 3 {
			pods = append(pods, mother.Pod("default",
				fmt.Sprintf("%s-%d", node, i), mother.OnNode(node)))
		}
	}
	s := explainCluster(
		[]*corev1.Node{explainNode("node-a"), explainNode("node-b"), explainNode("node-c")},
		pods)

	text := renderedExplain(t, outputText, s, explainConfig())
	if !strings.Contains(text, "would drain node-a") {
		t.Fatalf("the fixture no longer chooses the node it is about:\n%s", text)
	}

	// The sentence the Event uses, through the same function, so the two
	// surfaces cannot describe one decision two ways.
	if want := engine.RelocationSummary(2); !strings.Contains(text, want) {
		t.Errorf("explain does not say what would move (%q):\n%s", want, text)
	}
}

// TestExplainMarksAnUnmodelledRefusal is the word the how-to guide sends a
// reader here to look for.
//
// "binpack could not read what the replacement would request" and "the
// workload does not fit" are different facts calling for different responses,
// and only the metric name said which was which — so an operator scanning
// explain for the promised word found nothing and concluded the cluster had no
// modelling gaps, while the answer was on the line they were reading.
func TestExplainMarksAnUnmodelledRefusal(t *testing.T) {
	// Owned by an operator's own custom resource, so there is no template to
	// read and no way to size the replacement.
	pod := mother.Pod("default", "shard", mother.OnNode("node-a"),
		mother.ControlledBy("ShardSet", "shards"), mother.Requests("100m", "1000Mi"))
	s := explainCluster(
		[]*corev1.Node{explainNode("node-a"), explainNode("node-b")},
		[]*corev1.Pod{pod, mother.Pod("default", "web", mother.OnNode("node-b"),
			mother.Requests("100m", "1000Mi"))})

	text := renderedExplain(t, outputText, s, explainConfig())
	if !strings.Contains(text, "unmodelled") {
		t.Errorf("a refusal binpack made for want of a template is not marked as one:\n%s", text)
	}

	var view explainView
	if err := json.Unmarshal([]byte(renderedExplain(t, outputJSON, s, explainConfig())), &view); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}
	for _, r := range view.Nodes {
		// The negative half, and the one that matters: a marker on every
		// infeasible node says nothing at all. node-b is refused because a
		// 1000Mi pod does not fit on what is left, which is a fact about the
		// cluster and not a gap in binpack.
		if want := r.Name == "node-a"; r.Unmodelled != want {
			t.Errorf("%s unmodelled = %v (%s), want %v", r.Name, r.Unmodelled, r.Detail, want)
		}
	}
}

// TestExplainSaysTheAfterDrainCooldownIsNotVisibleToIt is a control that is
// configured, reported as configured, and — on this command — silently inert.
//
// The completion timestamp lives in the controller's memory, so explain has no
// LastDrain at all and the engine reads the zero value as "no recent drain".
// binpack already grades that condition as start-refusing for a strictly
// weaker case: `run --once` will not start with a cooldown it cannot honour.
// explain has to at least say so.
func TestExplainSaysTheAfterDrainCooldownIsNotVisibleToIt(t *testing.T) {
	s := explainCluster(
		[]*corev1.Node{explainNode("node-a"), explainNode("node-b")},
		[]*corev1.Pod{mother.Pod("default", "web", mother.OnNode("node-a"))})

	cfg := explainConfig()
	cfg.Default.CooldownAfterDrain = 15 * time.Minute

	text := renderedExplain(t, outputText, s, cfg)
	if !strings.Contains(text, "cooldown.afterDrain") {
		t.Errorf("explain previewed a drain without saying it cannot see the cooldown:\n%s", text)
	}
	// The clause `run --once` refuses on, shared rather than restated, so the
	// two cannot come to describe the same fact differently.
	//
	// Spelled out rather than taken from the constant. A test that builds its
	// expectation the way the code builds the value agrees with any rewording,
	// including one that leaves the two surfaces saying different things —
	// which is the whole of what the sharing is for. The controller's own
	// refusal asserts the same sentence, so moving it fails on both.
	const why = "a completed drain leaves nothing in the cluster to measure from"
	if !strings.Contains(text, why) {
		t.Errorf("explain does not say why the cooldown is invisible to it:\n%s", text)
	}
	if engine.NoDrainToMeasureFrom != why {
		t.Errorf("the shared clause is now %q; `run --once` says something else",
			engine.NoDrainToMeasureFrom)
	}

	// The negative half. An unconditional line says nothing about the
	// configuration, and a caveat printed where it cannot apply is noise that
	// teaches a reader to skip it.
	off := renderedExplain(t, outputText, s, explainConfig())
	if strings.Contains(off, "cooldown.afterDrain") {
		t.Errorf("explain warned about a cooldown nobody configured:\n%s", off)
	}

	var view explainView
	if err := json.Unmarshal([]byte(renderedExplain(t, outputJSON, s, cfg)), &view); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}
	if len(view.NotEvaluated) != 1 {
		t.Errorf("notEvaluated = %v, want the one control explain could not evaluate", view.NotEvaluated)
	}
}

// TestExplainStillDescribesEveryNodeWithoutAnAutoscaler is the first thing a
// stranger sees when they point binpack at kind, minikube or k3d.
//
// Decide refuses before assessing anything when no autoscaler is running, and
// explain returned at the same point: five lines, no node table, nothing to
// show it had read the cluster at all. That is indistinguishable from a binary
// that does not work, and it is the moment the project's reception turns on.
func TestExplainStillDescribesEveryNodeWithoutAnAutoscaler(t *testing.T) {
	s := explainCluster(
		[]*corev1.Node{explainNode("node-a"), explainNode("node-b"), explainNode("node-c")},
		[]*corev1.Pod{mother.Pod("default", "web", mother.OnNode("node-a"))})
	s.Autoscaler = engine.Autoscaler{}

	text := renderedExplain(t, outputText, s, explainConfig())

	for _, node := range []string{"node-a", "node-b", "node-c"} {
		if !strings.Contains(text, node) {
			t.Errorf("%s is missing from the report:\n%s", node, text)
		}
	}
	// The refusal is still the headline. Describing the cluster underneath it
	// must not read as a plan binpack is about to carry out.
	if !strings.Contains(text, "binpack will not act") {
		t.Errorf("explain no longer says binpack will not act:\n%s", text)
	}

	var view explainView
	if err := json.Unmarshal([]byte(renderedExplain(t, outputJSON, s, explainConfig())), &view); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}
	if len(view.Nodes) != 3 {
		t.Errorf("json nodes = %d, want every node in the cluster", len(view.Nodes))
	}

	// An autoscaler restarting is the same branch with the pools still
	// published, so the rows carry real verdicts — and none of them may claim
	// a choice, because no choice was made. "another node was chosen first"
	// on every drainable row is what a report that assumes a decision says.
	stale := explainCluster(
		[]*corev1.Node{explainNode("node-a"), explainNode("node-b"), explainNode("node-c")},
		[]*corev1.Pod{mother.Pod("default", "web", mother.OnNode("node-a"))})
	stale.Autoscaler.LastProbe = explainNow.Add(-30 * time.Minute)

	restarting := renderedExplain(t, outputText, stale, explainConfig())
	if strings.Contains(restarting, "chosen first") {
		t.Errorf("a report that chose nothing says another node was chosen:\n%s", restarting)
	}
	if !strings.Contains(restarting, engine.VerdictDrainable) {
		t.Errorf("a restarting autoscaler takes the arithmetic away with it:\n%s", restarting)
	}
	// Assessed, and not chosen — the assertion the fixture above cannot make,
	// because with no pools published there is no candidate to choose. A
	// drainable node marked `*` under a headline saying binpack will not act
	// is the report contradicting itself on the row somebody is reading.
	if strings.Contains(restarting, "* ") {
		t.Errorf("a binpack that will not act marked a node as chosen:\n%s", restarting)
	}
	var restartingView explainView
	if err := json.Unmarshal([]byte(renderedExplain(t, outputJSON, stale, explainConfig())),
		&restartingView); err != nil {
		t.Fatalf("decoding the JSON output: %v", err)
	}
	for _, r := range restartingView.Nodes {
		if r.Chosen {
			t.Errorf("%s is marked chosen by a binpack that will not act", r.Name)
		}
	}
	// Assessed, not chosen. Nothing will happen, and a report that marks a
	// node the way a preview does would say otherwise.
	for _, r := range view.Nodes {
		if r.Chosen {
			t.Errorf("%s is marked chosen by a binpack that will not act", r.Name)
		}
	}
}

// TestAMissingKubeconfigNamesTheFlagThatFixesIt is the first interaction a
// stranger has with binpack, and it was advice that provably does not work.
//
// client-go's ErrEmptyConfig carries a suggestion from before KUBECONFIG
// existed. binpack builds its loading rules without ClusterDefaults, so
// KUBERNETES_MASTER is not consulted on this path at all: setting it produces
// a byte-identical error, and the reader is left with no evidence that the
// problem is a kubeconfig rather than a broken binary.
func TestAMissingKubeconfigNamesTheFlagThatFixesIt(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "there-is-no-kubeconfig"))

	_, err := restConfigFor("", "")
	if err == nil {
		t.Fatal("resolving a connection with no kubeconfig anywhere succeeded")
	}
	if strings.Contains(err.Error(), "KUBERNETES_MASTER") {
		t.Errorf("the error tells the reader to set a variable binpack never reads: %v", err)
	}
	if !strings.Contains(err.Error(), "--kubeconfig") || !strings.Contains(err.Error(), "KUBECONFIG") {
		t.Errorf("the error names neither of the two things that would fix it: %v", err)
	}
}

// TestAnUnreadableConnectionErrorIsLeftAlone is the negative half. Only the
// empty-configuration error carries bad advice; every other failure on this
// path — an unreachable server, a bad context — reads well already, and
// replacing all of them with one sentence about kubeconfigs would lose the
// diagnosis rather than improve it.
func TestAnUnreadableConnectionErrorIsLeftAlone(t *testing.T) {
	_, err := restConfigFor("", "no-such-context")
	if err == nil {
		t.Fatal("resolving a connection for a context that does not exist succeeded")
	}
	if strings.Contains(err.Error(), "--kubeconfig") {
		t.Errorf("a context failure was reported as a missing kubeconfig: %v", err)
	}
}

// dryRunLimit is the half of the dry-run claim that was missing, in the words
// all four places have to use.
//
// Shared as a literal rather than interpolated, because two of the four are
// not Go: the guard below reads each file as text, which is the only thing a
// YAML comment and a markdown paragraph have in common with a doc comment.
const dryRunLimit = "nothing is drained"

// dryRunClaimSources are every place binpack tells an operator what dry run
// proves. Four rather than three: the Go comment, the `run --help` text and
// the how-to guide are the ones a reader might go looking for, and
// values.yaml is the one every installer actually opens and edits.
var dryRunClaimSources = []string{
	"../../internal/controller/controller.go",
	"run.go",
	"../../docs/how-to/let-binpack-drain-nodes.md",
	"../../charts/binpack/values.yaml",
}

// TestTheDryRunClaimIsScopedToOneEvaluation guards a promise that is true of
// an evaluation and false of a week of them.
//
// Decide is the same function on the same inputs in both modes, so which node
// binpack would pick is exactly what dry run tells you. What it cannot tell
// you is what follows, because nothing follows: no drain starts, so the
// cluster never consolidates, LastDrain stays zero, and cooldown.afterDrain,
// per-node backoff and the one-drain-at-a-time gate can never fire. A week of
// dry-run events is a repeated *first* choice, not the sequence that enabling
// produces — and it is the sequence the adoption gate rests on.
//
// Checked in all four places at once because the failure mode is fixing three
// of them: the claim reads naturally in each register, and the one nobody
// greps for is the YAML comment.
func TestTheDryRunClaimIsScopedToOneEvaluation(t *testing.T) {
	for _, source := range dryRunClaimSources {
		t.Run(filepath.Base(source), func(t *testing.T) {
			data, err := os.ReadFile(source)
			if err != nil {
				t.Fatalf("reading %s: %v", source, err)
			}
			// Whitespace and the characters that only ever wrap it — comment
			// markers, string continuations — flattened away first. The claim
			// is prose, and a paragraph re-wrapped by an editor is the same
			// paragraph; a guard that failed on it would be a guard about
			// line breaks.
			text := flattened(string(data))

			// The half that is true, and that this must not throw away with
			// the other one: an operator has to be told the preview is real.
			if !strings.Contains(text, "identical") {
				t.Errorf("%s no longer says the decisions are identical in both modes, "+
					"which is the half of the claim that holds", source)
			}

			// The half that is not. Both phrasings assert totality over a
			// span, which is where the promise stops being true.
			for _, claim := range []string{"what it would have done", "what it will do"} {
				if strings.Contains(text, claim) {
					t.Errorf("%s still promises %q: dry run performs none of binpack's own "+
						"consolidation, so it cannot show what follows the first choice",
						source, claim)
				}
			}

			// And the limit itself, in the same words everywhere, so that four
			// passages cannot drift into four different accounts of one fact.
			if !strings.Contains(text, dryRunLimit) {
				t.Errorf("%s does not say what dry run cannot show (%q)", source, dryRunLimit)
			}
		})
	}
}

// flattened reduces a source file to the prose in it, so a substring search
// asks about the sentence rather than about where the lines happen to break.
func flattened(text string) string {
	// The escape first, because a Go string literal wraps a sentence with two
	// characters that are not whitespace at all.
	text = strings.ReplaceAll(text, `\n`, " ")
	return regexp.MustCompile(`[\s/#+"]+`).ReplaceAllString(text, " ")
}

// TestExplainAnswersAboutTheDeployedBinpack runs the command rather than its
// renderer, because the step between them has no other cover.
//
// `kubectl exec deploy/binpack -- binpack explain` exists to answer about the
// binpack running beside it. That only holds if the configuration it reads
// actually reaches the report: every other test in this file builds the
// renderer's options by hand, so a field never carried out of RunE would leave
// all of them green while the command answered about a binpack nobody
// configured.
func TestExplainAnswersAboutTheDeployedBinpack(t *testing.T) {
	for _, tc := range []struct {
		name, document, want, notWant string
	}{
		{"a deployment that acts", "dryRun: false\n", "dryRun: false", "dryRun: true"},
		{"a deployment that reports", "dryRun: true\n", "dryRun: true", "dryRun: false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mounted := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(mounted, []byte(
				"apiVersion: binpack.motleyhand.com/v1alpha1\nkind: BinpackConfig\n"+tc.document),
				0o600); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func(orig string) func() {
				return func() { deployedConfig = orig }
			}(deployedConfig))
			deployedConfig = mounted

			t.Cleanup(func(orig func(string, string) (collect.Reader, error)) func() {
				return func() { readerFor = orig }
			}(readerFor))
			readerFor = func(string, string) (collect.Reader, error) {
				return fake.NewClientBuilder().
					WithObjects(explainNode("node-a"), explainNode("node-b")).Build(), nil
			}

			var out bytes.Buffer
			cmd := NewRootCommand(&out)
			cmd.SetArgs([]string{"explain"})
			if err := cmd.Execute(); err != nil {
				t.Fatalf("explain: %v", err)
			}

			text := out.String()
			if !strings.Contains(text, mounted) {
				t.Errorf("the report does not name the configuration it read:\n%s", text)
			}
			if !strings.Contains(text, tc.want) {
				t.Errorf("the report does not say the deployment %s:\n%s", tc.want, text)
			}
			if strings.Contains(text, tc.notWant) {
				t.Errorf("the report says %s about a deployment that does not:\n%s", tc.notWant, text)
			}
			// And it read the cluster, on a fixture with no autoscaler at all
			// — which is the one path where the node table survives a refusal
			// reached above it.
			for _, node := range []string{"node-a", "node-b"} {
				if !strings.Contains(text, node) {
					t.Errorf("%s is missing from the report:\n%s", node, text)
				}
			}
		})
	}
}

// TestExplainSaysWhereItLookedForTheAutoscaler is the same obligation as
// diagnose's closing line, on the command an operator reaches for first.
//
// "cluster-autoscaler: unavailable" is a claim about the cluster, and binpack
// has read exactly one object to make it. On a cluster whose autoscaler runs
// outside the namespace binpack was pointed at, that claim is false and there
// is nothing in the output to suggest where to look — so naming the object is
// what turns an assertion into something checkable.
func TestExplainSaysWhereItLookedForTheAutoscaler(t *testing.T) {
	s := explainCluster([]*corev1.Node{explainNode("a")}, nil)
	s.Autoscaler = engine.Autoscaler{}

	out := renderedExplainIn(t, &options{output: outputText, autoscalerNamespace: "autoscaler"},
		s, explainConfig())

	if !strings.Contains(out, "autoscaler/cluster-autoscaler-status") {
		t.Errorf("explain calls the autoscaler unavailable without saying what it read "+
			"to decide that:\n%s", out)
	}
}

func TestExplainSaysNothingAboutWhereItLookedWhenTheAutoscalerIsThere(t *testing.T) {
	// The reverse: the object is worth naming as the evidence behind a
	// refusal, and is noise on a cluster where the answer was yes.
	out := renderedExplainIn(t, &options{output: outputText, autoscalerNamespace: "autoscaler"},
		explainCluster([]*corev1.Node{explainNode("a")}, nil), explainConfig())

	if strings.Contains(out, "cluster-autoscaler-status") {
		t.Errorf("explain names the object it read on a cluster whose autoscaler is "+
			"running, where it answers nothing:\n%s", out)
	}
}

// derivedCluster is a cluster binpack can only see because it worked the join
// out: nothing carries the configured label, and the provider's own pool label
// holds a value the published identifier was built from.
func derivedCluster() engine.Snapshot {
	s := engine.Snapshot{
		Now: explainNow,
		Autoscaler: engine.Autoscaler{
			Running:   true,
			LastProbe: explainNow.Add(-10 * time.Second),
			Groups: []engine.NodeGroup{
				{ID: "eks-workers-a2c1d3e4-1111", MinSize: 1, MaxSize: 10, Ready: 2},
			},
		},
	}
	for i := range 2 {
		s.Nodes = append(s.Nodes, mother.SmallNode(fmt.Sprintf("ip-10-0-1-1%d", i),
			mother.NodeLabels(map[string]string{"eks.amazonaws.com/nodegroup": "workers"})))
	}
	return s
}

// TestExplainNamesTheJoinItDerivedAndWhy is the reporting obligation that
// makes deriving acceptable at all.
//
// A heuristic that decides which nodes binpack manages has to be visible, or
// it is indistinguishable from binpack quietly changing its mind about scope.
// So the key, the fact that it was derived rather than configured, and what
// each value resolved to all have to reach the operator.
func TestExplainNamesTheJoinItDerivedAndWhy(t *testing.T) {
	s := derivedCluster()
	cfg, err := engine.ResolvePools(s, explainConfig())
	if err != nil {
		t.Fatalf("preflight refused a derivable cluster:\n%v", err)
	}

	text := renderedExplain(t, outputText, s, cfg)
	for what, want := range map[string]string{
		"the key it read":                 "eks.amazonaws.com/nodegroup",
		"the value that key holds":        "workers",
		"the pool that value resolved to": "eks-workers-a2c1d3e4-1111",
		"that it was derived":             "derived",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("explain does not report %s (%q):\n%s", what, want, text)
		}
	}
}

// TestExplainSaysNothingAboutTheJoinWhenItIsTheConfiguredOne keeps the report
// quiet on the clusters where there is nothing to disclose.
//
// The value being the identifier is what the configuration says binpack does,
// so restating it every run is noise — and noise is what stops the derived
// case standing out.
func TestExplainSaysNothingAboutTheJoinWhenItIsTheConfiguredOne(t *testing.T) {
	s := explainCluster([]*corev1.Node{explainNode("node-a")}, nil)
	cfg, err := engine.ResolvePools(s, explainConfig())
	if err != nil {
		t.Fatalf("preflight refused an ordinary cluster:\n%v", err)
	}

	if text := renderedExplain(t, outputText, s, cfg); strings.Contains(text, "derived") {
		t.Errorf("explain reports a derivation on a cluster that matched outright:\n%s", text)
	}
}

// TestTheJsonReportCarriesTheJoin, because a consumer branching on scope
// needs it as data rather than as a sentence.
func TestTheJsonReportCarriesTheJoin(t *testing.T) {
	s := derivedCluster()
	cfg, err := engine.ResolvePools(s, explainConfig())
	if err != nil {
		t.Fatalf("preflight refused a derivable cluster:\n%v", err)
	}

	var view struct {
		Pools struct {
			Source string            `json:"source"`
			Label  string            `json:"label"`
			Groups map[string]string `json:"groups,omitempty"`
		} `json:"pools"`
	}
	raw := renderedExplain(t, outputJSON, s, cfg)
	if err := json.Unmarshal([]byte(raw), &view); err != nil {
		t.Fatalf("decoding: %v\n%s", err, raw)
	}
	if view.Pools.Label != "eks.amazonaws.com/nodegroup" {
		t.Errorf("pools.label = %q", view.Pools.Label)
	}
	if view.Pools.Source == "" {
		t.Errorf("pools.source is empty, so a consumer cannot tell a derived scope "+
			"from a configured one:\n%s", raw)
	}
	if got := view.Pools.Groups["workers"]; got != "eks-workers-a2c1d3e4-1111" {
		t.Errorf("pools.groups[workers] = %q", got)
	}
}
