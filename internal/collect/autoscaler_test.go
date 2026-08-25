package collect_test

import (
	"strings"
	"testing"
	"time"

	"github.com/motleyhand/binpack/internal/collect"
	"github.com/motleyhand/binpack/internal/engine"
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

func TestFallsBackToTheDocumentTimestamp(t *testing.T) {
	// Older autoscalers may not publish a health probe time. Falling back to
	// the document's own timestamp keeps binpack usable there rather than
	// refusing to work on a cluster whose autoscaler is demonstrably fine.
	//
	// Note the format: Go's default rendering, not RFC3339 like every other
	// timestamp in the same document.
	got, err := collect.ParseAutoscalerStatus(
		"autoscalerStatus: Running\ntime: 2026-08-15 09:15:37.012080408 +0000 UTC\n")
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 8, 15, 9, 15, 37, 12080408, time.UTC)
	if !got.LastProbe.Equal(want) {
		t.Errorf("lastProbe = %s, want %s", got.LastProbe, want)
	}
}

func TestAbsentTargetIsDistinguishableFromZero(t *testing.T) {
	got, err := collect.ParseAutoscalerStatus(
		"autoscalerStatus: Running\nnodeGroups:\n- name: g\n  health:\n    minSize: 0\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.Groups[0].HasTarget {
		t.Error("a missing cloudProviderTarget must not read as a target of zero")
	}
}

// TestAGroupWithNoNameNeverReachesTheSnapshot is the collector's half of the
// engine's TestAnUnnamedNodeGroupClaimsNoNodes.
//
// `name` is omitempty in the autoscaler's own status type, so a group without
// one is representable — a provider returning an empty Id(), a status object
// written by hand or half-written, or a future release that renames the field,
// which would send every group to the empty identifier at once. Downstream,
// the identifier is what a node's join label is matched against and a node in
// no pool answers "", so an unnamed group is one that claims every static node
// in the cluster.
//
// Dropped here rather than checked at each consumer, because here there is one
// of them.
func TestAGroupWithNoNameNeverReachesTheSnapshot(t *testing.T) {
	got, err := collect.ParseAutoscalerStatus(
		"autoscalerStatus: Running\nnodeGroups:\n- health:\n    minSize: 1\n" +
			"- name: real\n  health:\n    minSize: 1\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Groups) != 1 || got.Groups[0].ID != "real" {
		t.Fatalf("groups = %+v, want only the named one: a group with no name "+
			"is one every unlabelled node matches", got.Groups)
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

// legacy is what cluster-autoscaler 1.29 and earlier publish: the free text
// ClusterAutoscalerStatus.GetReadableString() renders, wrapped in a header
// line, rather than the structured document introduced in 1.30. The
// continuation lines carry colons at arbitrary indentation, so it is not YAML
// and never was.
const legacy = `Cluster-autoscaler status at 2026-08-19 09:03:11.123456789 +0000 UTC:
Cluster-wide:
  Health:      Healthy (ready=3 unready=0 notStarted=0 longNotStarted=0 registered=3 longUnregistered=0)
               LastProbeTime:      2026-08-19 09:03:11.123456789 +0000 UTC
               LastTransitionTime: 2026-08-19 08:00:00.000000000 +0000 UTC
  ScaleUp:     NoActivity (ready=3 registered=3)
               LastProbeTime:      2026-08-19 09:03:11.123456789 +0000 UTC
               LastTransitionTime: 2026-08-19 08:00:00.000000000 +0000 UTC
  ScaleDown:   NoCandidates (candidates=0)
               LastProbeTime:      2026-08-19 09:03:11.123456789 +0000 UTC
               LastTransitionTime: 2026-08-19 08:00:00.000000000 +0000 UTC
`

func TestParseAutoscalerStatusNamesThePre130Format(t *testing.T) {
	// ADR-0004's whole mechanism is the structured status document, and it
	// arrived in cluster-autoscaler 1.30. Below that binpack cannot work at
	// all — but what the operator saw was a YAML parser complaining about a
	// line of a document they did not write, naming neither the object nor
	// the component, on a cluster where binpack is simply not applicable.
	_, err := collect.ParseAutoscalerStatus(legacy)
	if err == nil {
		t.Fatal("the pre-1.30 text format is not something binpack can read")
	}
	if !strings.Contains(err.Error(), "cluster-autoscaler 1.30") {
		t.Errorf("the error should name the version binpack needs, got: %v", err)
	}
}

func TestParseKeepsWhatTheStatusSaidAboutItself(t *testing.T) {
	// The observed status and the cluster's health were both read and both
	// thrown away — the first reduced to a bool, the second parsed into a
	// struct nothing looked at. So every refusal had to be worded from the
	// bool alone, and the one refusal that turns on health could not be made
	// at all.
	got, err := collect.ParseAutoscalerStatus(observed)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	if !got.StatusFound {
		t.Error("a parsed document is one binpack found")
	}
	if got.ObservedStatus != "Running" {
		t.Errorf("ObservedStatus = %q, want the literal value the document carried", got.ObservedStatus)
	}
	if got.HealthStatus != "Healthy" {
		t.Errorf("HealthStatus = %q, want what clusterWide.health.status said", got.HealthStatus)
	}
}

func TestParseReadsAnUnhealthyCluster(t *testing.T) {
	// The combination that passes every other guard: autoscalerStatus is
	// Running because the process is past start-up, the probe time is fresh
	// because it scans regardless — and it has stopped scaling in both
	// directions until the cluster recovers.
	got, err := collect.ParseAutoscalerStatus(
		"autoscalerStatus: Running\nclusterWide:\n  health:\n    status: Unhealthy\n")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Running {
		t.Error("the autoscaler is running; it is the cluster that is unwell")
	}
	if got.HealthStatus != engine.HealthUnhealthy {
		t.Errorf("HealthStatus = %q, want %q", got.HealthStatus, engine.HealthUnhealthy)
	}
}

func TestParseNamesTheStatusItSawWhenItIsNotRunning(t *testing.T) {
	got, err := collect.ParseAutoscalerStatus("autoscalerStatus: Initializing\n")
	if err != nil {
		t.Fatal(err)
	}
	if got.Running {
		t.Error("anything other than Running means binpack must not act")
	}
	if _, _, why := got.Live(time.Now()); !strings.Contains(why, "Initializing") {
		t.Errorf("the refusal does not name the status the autoscaler published: %s", why)
	}
}
