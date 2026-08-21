package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"

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
		},
	}
}

// renderedExplain runs the command's own two steps — decide, then render — so
// a test asserts on the bytes an operator would see rather than on a decision
// nothing formats.
func renderedExplain(t *testing.T, format outputFormat, s engine.Snapshot, cfg engine.Config) string {
	t.Helper()
	var buf bytes.Buffer
	if err := renderExplain(&options{output: format, out: &buf}, s, explainDecision(s, cfg)); err != nil {
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
	if view.Node != "node-a" {
		t.Errorf("json node = %q, want the node being drained", view.Node)
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
