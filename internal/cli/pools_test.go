package cli

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/motleyhand/binpack/api/v1alpha1"
	"github.com/motleyhand/binpack/internal/collect"
	"github.com/motleyhand/binpack/internal/mother"
)

// The two tests here are the call-site half of ADR-0013, and they have no
// other cover.
//
// [engine.ResolvePools] returns the Config to decide against, because the join
// between nodes and pools is worked out from the snapshot rather than read
// from configuration. A command that resolved and then went on passing
// engineConfig(cfg) would satisfy every test of the resolver itself and still
// report every node as outside every pool — the silent narrowing of scope
// ADR-0012 exists to prevent. A mutation sweep over the resolver cannot reach
// a defect in where the resolver is called, so each frontend is asked
// directly, through the command rather than its renderer.

// derivedObjects is a cluster no version of binpack before ADR-0013 could see
// a single pool on: an EKS cluster whose autoscaler publishes an Auto Scaling
// group name generated from the managed node group's, and where no label on
// any node equals it.
func derivedObjects(extra ...client.Object) []client.Object {
	objs := []client.Object{&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: v1alpha1.DefaultAutoscalerNamespace,
			Name:      v1alpha1.DefaultAutoscalerStatusName,
		},
		Data: map[string]string{"status": `
autoscalerStatus: Running
time: ` + time.Now().UTC().Format("2006-01-02 15:04:05.000000000 -0700 MST") + `
clusterWide:
  health:
    status: Healthy
    lastProbeTime: "` + time.Now().UTC().Format(time.RFC3339) + `"
  scaleDown:
    status: NoCandidates
nodeGroups:
- name: eks-workers-a2c1d3e4-1111
  health:
    minSize: 1
    maxSize: 10
    cloudProviderTarget: 3
    nodeCounts:
      registered:
        ready: 3
`}}}
	for i := range 3 {
		objs = append(objs, mother.LargeNode(fmt.Sprintf("ip-10-0-1-1%d", i),
			mother.NodeLabels(map[string]string{"eks.amazonaws.com/nodegroup": "workers"})))
	}
	return append(objs, extra...)
}

// readingCluster points the commands at a fake cluster for the duration of one
// test. Both read through readerFor, which is what makes this possible and is
// why `diagnose` does not call clientFor itself.
func readingCluster(t *testing.T, objs ...client.Object) {
	t.Helper()
	t.Cleanup(func(orig func(string, string) (collect.Reader, error)) func() {
		return func() { readerFor = orig }
	}(readerFor))
	built := fake.NewClientBuilder().WithObjects(objs...).Build()
	readerFor = func(string, string) (collect.Reader, error) { return built, nil }
}

// TestExplainDecidesAgainstTheJoinItResolved asks the report the question an
// operator would: which pool is this node in?
func TestExplainDecidesAgainstTheJoinItResolved(t *testing.T) {
	readingCluster(t, derivedObjects()...)

	var out bytes.Buffer
	cmd := NewRootCommand(&out)
	cmd.SetArgs([]string{"explain"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("explain refused a cluster whose join it can derive: %v", err)
	}

	text := out.String()
	if strings.Contains(text, "not part of an autoscaling pool") {
		t.Errorf("explain still reports nodes as outside every pool:\n%s", text)
	}
	// Named twice on purpose: the pool list is printed from the status
	// document whatever binpack concluded, so the identifier appearing at all
	// proves nothing. The line that says how nodes were matched to it is what
	// only a resolved configuration produces.
	if !strings.Contains(text, "pools matched by eks.amazonaws.com/nodegroup") {
		t.Errorf("explain does not say how it matched nodes to pools:\n%s", text)
	}
}

// TestDiagnoseGatesAgainstTheJoinItResolved asks the question a CI job would.
//
// diagnose's silence under a mismatch is narrower than explain's and worth
// reproducing exactly: staticNodes returns every node, so every finding
// diagnoseWorkloads groups is marked as freeing nothing and --fail-on skips
// it. A gate on a real blocker therefore passes. Here the blocker is a bare
// pod, which is blocking, and the gate has to fail.
func TestDiagnoseGatesAgainstTheJoinItResolved(t *testing.T) {
	readingCluster(t, derivedObjects(mother.Pod("default", "legacy", mother.Bare(), mother.OnNode("ip-10-0-1-10")))...)

	var out bytes.Buffer
	cmd := NewRootCommand(&out)
	cmd.SetArgs([]string{"diagnose", "--fail-on", "blocking"})

	err := cmd.Execute()
	if err == nil {
		t.Fatalf("the gate passed on a cluster with a bare pod on a pool node:\n%s",
			out.String())
	}
	if code := ExitCodeFor(err); code != ExitFindings {
		t.Errorf("exit = %d, want %d — the gate failed for some reason other than the "+
			"finding:\n%v", code, ExitFindings, err)
	}
}
