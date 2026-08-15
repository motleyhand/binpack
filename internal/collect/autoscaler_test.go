package collect_test

import (
	"testing"
	"time"

	"github.com/motleyhand/binpack/internal/collect"
)

// observed is the status document from a real DOKS cluster.
//
// Using a real document rather than a hand-written one matters: it is somebody
// else's schema, and the point of the test is that binpack reads what the
// autoscaler actually writes.
//
// It is deliberately NOT trimmed to the fields binpack currently reads. An
// earlier version was, and the trimming removed clusterWide.health's
// timestamps — which later became load-bearing for the staleness check, at
// which point the "real" fixture was quietly a fiction.
const observed = `
autoscalerStatus: Running
clusterWide:
  health:
    lastProbeTime: "2026-08-15T09:15:37Z"
    lastTransitionTime: "2026-08-15T04:33:33Z"
    status: Healthy
  scaleDown:
    lastProbeTime: "2026-08-15T09:15:37Z"
    status: NoCandidates
  scaleUp:
    lastProbeTime: "2026-08-15T09:15:37Z"
    lastTransitionTime: "2026-08-15T04:33:33Z"
    status: NoActivity
nodeGroups:
- health:
    cloudProviderTarget: 2
    maxSize: 24
    minSize: 0
    nodeCounts:
      registered:
        ready: 2
        total: 2
    status: Healthy
  name: da8977ba-244f-4cfe-9ea1-834a39370f6d
  scaleDown:
    status: NoCandidates
time: 2026-08-15 09:15:37.012080408 +0000 UTC
`

func TestParseObservedStatus(t *testing.T) {
	got, err := collect.ParseAutoscalerStatus(observed)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	if !got.Running {
		t.Error("autoscalerStatus Running should mean running")
	}
	if got.ScaleUpInProgress {
		t.Error("NoActivity is not a scale-up in progress")
	}
	if got.ScaleDownStatus != "NoCandidates" {
		t.Errorf("scaleDownStatus = %q", got.ScaleDownStatus)
	}

	want := time.Date(2026, 8, 15, 4, 33, 33, 0, time.UTC)
	if !got.LastScaleUp.Equal(want) {
		t.Errorf("lastScaleUp = %s, want %s", got.LastScaleUp, want)
	}

	want2 := time.Date(2026, 8, 15, 9, 15, 37, 0, time.UTC)
	if !got.LastProbe.Equal(want2) {
		t.Errorf("lastProbe = %s, want %s", got.LastProbe, want2)
	}

	if len(got.Groups) != 1 {
		t.Fatalf("groups = %d, want 1", len(got.Groups))
	}
	g := got.Groups[0]
	if g.Target != 2 {
		t.Errorf("cloudProviderTarget = %d, want 2", g.Target)
	}
	// The name is the node group identifier, matching the value of
	// doks.digitalocean.com/node-pool-id on the node.
	if g.ID != "da8977ba-244f-4cfe-9ea1-834a39370f6d" {
		t.Errorf("group ID = %q", g.ID)
	}
	if g.MinSize != 0 || g.MaxSize != 24 || g.Ready != 2 {
		t.Errorf("bounds = min %d, max %d, ready %d; want 0/24/2", g.MinSize, g.MaxSize, g.Ready)
	}
}

func TestOnlyAutoscalingPoolsAppear(t *testing.T) {
	// The observed cluster has two pools and the autoscaler reports one. A
	// pool absent from the document is one nothing will ever remove nodes
	// from, which is how binpack learns not to touch it — no configuration,
	// and nothing to drift.
	got, err := collect.ParseAutoscalerStatus(observed)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 1 {
		t.Fatalf("only the autoscaling pool should appear, got %d", len(got.Groups))
	}
}

func TestNonRunningAutoscalerIsNotRunning(t *testing.T) {
	got, err := collect.ParseAutoscalerStatus("autoscalerStatus: Unhealthy\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.Running {
		t.Error("anything other than Running means binpack must not act")
	}
}

func TestScaleUpInProgress(t *testing.T) {
	got, err := collect.ParseAutoscalerStatus(
		"autoscalerStatus: Running\nclusterWide:\n  scaleUp:\n    status: InProgress\n")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ScaleUpInProgress {
		t.Error("a scale-up in progress is the clearest signal not to remove nodes")
	}
}

func TestUnknownFieldsAreIgnored(t *testing.T) {
	// Somebody else's schema, and it grows. A new field must not break
	// binpack on a cluster that upgraded before binpack did.
	got, err := collect.ParseAutoscalerStatus(
		"autoscalerStatus: Running\nsomethingNew:\n  invented: true\n")
	if err != nil {
		t.Fatalf("an unrecognised field must not fail the parse: %v", err)
	}
	if !got.Running {
		t.Error("the rest of the document should still be read")
	}
}
