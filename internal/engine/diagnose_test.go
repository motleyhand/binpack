package engine_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"

	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

// diagnose runs the check against a healthy three-node pool above its minimum,
// so any finding a test sees comes from what that test set up.
func diagnose(nodes []*corev1.Node, pods []*corev1.Pod, pdbs ...*policyv1.PodDisruptionBudget) []engine.Finding {
	if nodes == nil {
		nodes = []*corev1.Node{inPool("a"), inPool("b"), inPool("c")}
	}
	s := cluster(nodes, pods)
	s.PDBs = pdbs
	return engine.Diagnose(s, config())
}

// only returns the sole finding with the given code, failing if there is not
// exactly one. Most checks are about a single condition, and asserting "exactly
// one" catches a check that fires per pod where it should fire per workload.
func only(t *testing.T, findings []engine.Finding, code string) engine.Finding {
	t.Helper()
	var matched []engine.Finding
	for _, f := range findings {
		if f.Code == code {
			matched = append(matched, f)
		}
	}
	if len(matched) != 1 {
		t.Fatalf("want exactly one %q finding, got %d in %s", code, len(matched), render(findings))
	}
	return matched[0]
}

func none(t *testing.T, findings []engine.Finding, code string) {
	t.Helper()
	for _, f := range findings {
		if f.Code == code {
			t.Fatalf("unexpected %q finding: %s — %s", code, f.Subject, f.Detail)
		}
	}
}

func render(findings []engine.Finding) string {
	var b strings.Builder
	b.WriteString("[")
	for i, f := range findings {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(f.Code + " on " + f.Subject)
	}
	b.WriteString("]")
	return b.String()
}

func TestDiagnoseFindsNothingWrongWithAHealthyCluster(t *testing.T) {
	nodes := []*corev1.Node{inPool("a"), inPool("b"), inPool("c")}
	pods := []*corev1.Pod{
		mother.Pod("default", "web", mother.OnNode("a")),
		mother.DaemonSetPod("kube-system", "cilium", mother.OnNode("a")),
	}

	if findings := diagnose(nodes, pods); len(findings) != 0 {
		t.Errorf("healthy cluster produced findings: %s", render(findings))
	}
}

func TestDiagnoseReportsAMissingAutoscalerAndSkipsPoolChecks(t *testing.T) {
	s := cluster([]*corev1.Node{inPool("a")}, nil)
	s.Autoscaler = engine.Autoscaler{}

	findings := engine.Diagnose(s, config())

	f := only(t, findings, engine.FindingNoAutoscaler)
	if f.Severity != engine.Blocking {
		t.Errorf("severity = %s, want blocking", f.Severity)
	}
	// Without a status document every pool is absent from it. Reporting each
	// one as unmanaged would bury the single answer that matters.
	none(t, findings, engine.FindingPoolNotAutoscaled)
	none(t, findings, engine.FindingPoolAtMinimum)
}

func TestDiagnoseStillReportsBudgetsWithoutAnAutoscaler(t *testing.T) {
	// A budget pinning a node is a real problem whether or not anything is
	// currently able to remove one — it becomes the next problem the moment
	// autoscaling is switched on.
	s := cluster([]*corev1.Node{inPool("a")}, nil)
	s.Autoscaler = engine.Autoscaler{}
	s.PDBs = []*policyv1.PodDisruptionBudget{mother.PDB("default", "web", 0, map[string]string{"app": "web"})}

	only(t, engine.Diagnose(s, config()), engine.FindingPDBZero)
}

func TestDiagnoseReportsAPoolAtItsMinimum(t *testing.T) {
	s := cluster([]*corev1.Node{inPool("a")}, nil)
	s.Autoscaler.Groups = []engine.NodeGroup{{ID: poolID, MinSize: 3, MaxSize: 10, Ready: 3}}

	f := only(t, engine.Diagnose(s, config()), engine.FindingPoolAtMinimum)
	if f.Subject != poolName {
		t.Errorf("subject = %q, want the human-readable pool name %q", f.Subject, poolName)
	}
	if !strings.Contains(f.Detail, "minimum 3") {
		t.Errorf("message does not state the minimum: %s", f.Detail)
	}
}

func TestDiagnoseReportsAnUnmanagedPoolOncePerPoolNotPerNode(t *testing.T) {
	nodes := []*corev1.Node{
		inPool("a"),
		sized("static-1", "8Gi", mother.InPool("pool-8g", "static-id")),
		sized("static-2", "8Gi", mother.InPool("pool-8g", "static-id")),
	}
	s := cluster(nodes, nil)
	s.Autoscaler.Groups = []engine.NodeGroup{{ID: poolID, MinSize: 1, MaxSize: 10, Ready: 1}}

	f := only(t, engine.Diagnose(s, config()), engine.FindingPoolNotAutoscaled)
	if f.Subject != "pool-8g" {
		t.Errorf("subject = %q, want the human-readable pool name", f.Subject)
	}
}

func TestDiagnoseReportsABudgetAllowingNoDisruptions(t *testing.T) {
	pdb := mother.PDB("default", "web", 0, map[string]string{"app": "web"})

	f := only(t, diagnose(nil, nil, pdb), engine.FindingPDBZero)

	if f.Severity != engine.Blocking {
		t.Errorf("severity = %s, want blocking", f.Severity)
	}
	if f.Subject != "default/web" {
		t.Errorf("subject = %q, want default/web", f.Subject)
	}
}

func TestDiagnoseSeparatesAnUnhealthyWorkloadFromAMisconfiguredBudget(t *testing.T) {
	// Zero disruptions because replicas are down is somebody else's problem
	// and resolves itself. Conflating the two would send an operator to edit a
	// budget that is behaving exactly as intended.
	pdb := mother.Healthy(mother.PDB("default", "web", 0, map[string]string{"app": "web"}), 1, 2)

	findings := diagnose(nil, nil, pdb)

	f := only(t, findings, engine.FindingPDBUnhealthy)
	if f.Severity != engine.Warning {
		t.Errorf("severity = %s, want warning", f.Severity)
	}
	none(t, findings, engine.FindingPDBZero)
}

func TestDiagnoseReportsABudgetThatSelectsNothing(t *testing.T) {
	// It blocks nothing, so calling it blocking would be a false positive.
	// It is still worth reporting, and for the opposite reason: whoever wrote
	// it believes a workload is protected when nothing is.
	pdb := mother.SelectsNothing(mother.PDB("default", "web", 0, map[string]string{"app": "gone"}))

	findings := diagnose(nil, nil, pdb)

	f := only(t, findings, engine.FindingPDBSelectsNothing)
	if f.Severity != engine.Warning {
		t.Errorf("severity = %s, want warning: it pins no node", f.Severity)
	}
	if !strings.Contains(f.Detail, "app=gone") {
		t.Errorf("detail does not show the selector that matched nothing: %s", f.Detail)
	}
	none(t, findings, engine.FindingPDBZero)
}

func TestDiagnoseReportsAStaleBudget(t *testing.T) {
	pdb := mother.Stale(mother.PDB("default", "web", 1, map[string]string{"app": "web"}))

	f := only(t, diagnose(nil, nil, pdb), engine.BlockedPDBStale)
	if !strings.Contains(f.Detail, "generation 7, observed 6") {
		t.Errorf("message does not state the generations: %s", f.Detail)
	}
}

func TestDiagnoseReportsAPodSelectedByTwoBudgets(t *testing.T) {
	// The trap: both budgets report healthy and allow disruptions, so nothing
	// looks wrong, and the eviction API refuses the pod with a 500 forever.
	pod := mother.Pod("default", "web-0",
		mother.OnNode("a"),
		mother.PodLabels(map[string]string{"app": "web", "tier": "front"}))
	byApp := mother.PDB("default", "by-app", 1, map[string]string{"app": "web"})
	byTier := mother.PDB("default", "by-tier", 1, map[string]string{"tier": "front"})

	findings := diagnose(nil, []*corev1.Pod{pod}, byTier, byApp)

	f := only(t, findings, engine.BlockedMultiplePDBs)
	if f.Severity != engine.Blocking {
		t.Errorf("severity = %s, want blocking", f.Severity)
	}
	// Sorted, so the detail does not depend on the order the budgets were
	// listed in — the same cluster must produce the same report twice.
	if !strings.Contains(f.Detail, "by-app, by-tier") {
		t.Errorf("detail does not name both budgets in order: %s", f.Detail)
	}
}

func TestDiagnoseReportsUnevictablePods(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		code string
	}{
		{"bare pod", mother.Pod("default", "orphan", mother.OnNode("a"), mother.Bare()),
			engine.BlockedBarePod},
		{"owned but not controlled", mother.Pod("default", "gc-linked", mother.OnNode("a"),
			mother.OwnedButNotControlled("ReplicaSet", "web-rs")), engine.BlockedBarePod},
		{"emptyDir", mother.Pod("default", "cache", mother.OnNode("a"), mother.WithEmptyDir("scratch")),
			engine.BlockedLocalStorage},
		{"hostPath", mother.Pod("default", "agent", mother.OnNode("a"),
			mother.WithHostPathVolume("logs", "/var/log")), engine.BlockedLocalStorage},
		{"kube-system with no budget, still inside the autoscaler's grace",
			mother.Pod("kube-system", "coredns", mother.OnNode("a"),
				mother.CreatedAt(now.Add(-5*time.Minute))), engine.BlockedSystemPod},
		{"refuses eviction", mother.Pod("default", "pinned", mother.OnNode("a"),
			mother.SafeToEvict("false")), engine.BlockedSafeToEvict},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := only(t, diagnose(nil, []*corev1.Pod{tc.pod}), tc.code)
			if f.Fix == "" {
				t.Error("no suggested fix, which is the whole point of diagnose")
			}
			// The detail says which node is being pinned. A finding an
			// operator cannot locate is one they cannot act on.
			if !strings.Contains(f.Detail, "a") {
				t.Errorf("detail does not name the node: %q", f.Detail)
			}
			// The diagnosis has already said what the condition is; repeating
			// it once per subject is the noise this report exists to avoid.
			if strings.Contains(f.Detail, f.Summary) {
				t.Errorf("detail repeats the summary: %q", f.Detail)
			}
		})
	}
}

func TestDiagnoseAcceptsAKubeSystemPodThatHasABudget(t *testing.T) {
	// Per the autoscaler's own documentation a budget overrides its refusal to
	// touch such a node. Reporting it anyway would send operators chasing a
	// blocker they had already cleared.
	pod := mother.Pod("kube-system", "coredns", mother.OnNode("a"),
		mother.PodLabels(map[string]string{"k8s-app": "kube-dns"}))
	pdb := mother.PDB("kube-system", "coredns", 1, map[string]string{"k8s-app": "kube-dns"})

	none(t, diagnose(nil, []*corev1.Pod{pod}, pdb), engine.BlockedSystemPod)
}

func TestDiagnoseIgnoresNodeLocalPods(t *testing.T) {
	// A DaemonSet pod with a hostPath volume is every cluster's cilium or
	// csi-node. It never moves and never needs to, so reporting it would bury
	// the real findings under one line per node.
	pods := []*corev1.Pod{
		mother.DaemonSetPod("kube-system", "cilium", mother.OnNode("a"),
			mother.WithHostPathVolume("cni", "/etc/cni")),
		mother.MirrorPod("kube-system", "kube-proxy-a", mother.OnNode("a")),
	}

	if findings := diagnose(nil, pods); len(findings) != 0 {
		t.Errorf("node-local pods produced findings: %s", render(findings))
	}
}

func TestDiagnoseReportsOverprovisioningFillerBelowTheCutoff(t *testing.T) {
	// The silent failure: the autoscaler ignores pods below its cutoff even
	// when Pending, so the warm buffer never comes back after the first burst
	// and nothing anywhere says so.
	broken := mother.PausePod("default", "filler", mother.OnNode("a"), mother.Priority(cutoff-1))

	f := only(t, diagnose(nil, []*corev1.Pod{broken}), engine.FindingPriorityBelow)
	if f.Severity != engine.Warning {
		t.Errorf("severity = %s, want warning", f.Severity)
	}

	// A correctly configured pause pod sits at or above the cutoff.
	working := mother.PausePod("default", "filler", mother.OnNode("a"), mother.Priority(cutoff))
	none(t, diagnose(nil, []*corev1.Pod{working}), engine.FindingPriorityBelow)
}

func TestDiagnoseCollapsesReplicasOfOneWorkload(t *testing.T) {
	// Twenty replicas mounting an emptyDir is one configuration mistake, and
	// twenty findings would bury the bare pod underneath them.
	var pods []*corev1.Pod
	for _, name := range []string{"web-a", "web-b", "web-c"} {
		pods = append(pods, mother.Pod("default", name, mother.OnNode("a"),
			mother.WithEmptyDir("scratch"),
			func(p *corev1.Pod) { p.OwnerReferences[0].Name = "web-rs" }))
	}
	pods = append(pods, mother.Pod("default", "orphan", mother.OnNode("a"), mother.Bare()))

	findings := diagnose(nil, pods)

	f := only(t, findings, engine.BlockedLocalStorage)
	if f.Subject != "default/ReplicaSet web-rs" {
		t.Errorf("subject = %q, want the owning workload", f.Subject)
	}
	if !strings.Contains(f.Detail, "3 pods on a") {
		t.Errorf("detail does not report the count and where: %s", f.Detail)
	}
	// The volume name is what makes the suggested annotation a judgement the
	// reader can make: "scratch" answers "is this disposable?".
	if !strings.Contains(f.Detail, "scratch") {
		t.Errorf("detail does not name the volume: %s", f.Detail)
	}
	only(t, findings, engine.BlockedBarePod)
}

func TestDiagnoseKeepsBarePodsSeparate(t *testing.T) {
	// Two bare pods have no controller to group by, and each is individually
	// something an operator has to deal with.
	pods := []*corev1.Pod{
		mother.Pod("default", "orphan-1", mother.OnNode("a"), mother.Bare()),
		mother.Pod("default", "orphan-2", mother.OnNode("a"), mother.Bare()),
	}

	var count int
	for _, f := range diagnose(nil, pods) {
		if f.Code == engine.BlockedBarePod {
			count++
		}
	}
	if count != 2 {
		t.Errorf("got %d bare-pod findings, want one per pod", count)
	}
}

func TestDiagnoseReportsAnAbandonedDrain(t *testing.T) {
	marked := inPool("a",
		mother.Cordoned(),
		mother.NodeAnnotations(map[string]string{
			engine.AnnotationDrainStarted: now.Add(-2 * time.Hour).Format(time.RFC3339),
		}))

	f := only(t, diagnose([]*corev1.Node{marked, inPool("b"), inPool("c")}, nil), engine.FindingAbandonedDrain)
	if f.Subject != "a" {
		t.Errorf("subject = %q, want the node", f.Subject)
	}

	// A marker on a schedulable node is a finished drain, not an abandoned one.
	uncordoned := inPool("a", mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainStarted: now.Add(-2 * time.Hour).Format(time.RFC3339),
	}))
	none(t, diagnose([]*corev1.Node{uncordoned, inPool("b"), inPool("c")}, nil), engine.FindingAbandonedDrain)
}

func TestDiagnoseReportsANodeInBackoff(t *testing.T) {
	backoff := func(until time.Time) *corev1.Node {
		return inPool("a", mother.NodeAnnotations(map[string]string{
			engine.AnnotationBackoffUntil: until.Format(time.RFC3339),
			engine.AnnotationLastFailure:  "eviction refused by default/web",
		}))
	}

	f := only(t, diagnose([]*corev1.Node{backoff(now.Add(10 * time.Minute)), inPool("b"), inPool("c")}, nil),
		engine.FindingNodeInBackoff)
	if !strings.Contains(f.Detail, "eviction refused by default/web") {
		t.Errorf("message drops the recorded reason, which is the only actionable part: %s", f.Detail)
	}

	// An expired backoff is not a finding: binpack will simply retry.
	none(t, diagnose([]*corev1.Node{backoff(now.Add(-time.Minute)), inPool("b"), inPool("c")}, nil), engine.FindingNodeInBackoff)
}

func TestDiagnoseReportsNoCandidatesAsInformational(t *testing.T) {
	s := cluster([]*corev1.Node{inPool("a")}, nil)
	s.Autoscaler.ScaleDownStatus = "NoCandidates"

	f := only(t, engine.Diagnose(s, config()), engine.FindingNoCandidates)
	if f.Severity != engine.Info {
		t.Errorf("severity = %s, want info: this is the autoscaler working correctly", f.Severity)
	}
}

func TestDiagnoseOrdersBlockingFindingsFirst(t *testing.T) {
	s := cluster([]*corev1.Node{inPool("a"), inPool("b"), inPool("c")}, []*corev1.Pod{
		mother.Pod("default", "orphan", mother.OnNode("a"), mother.Bare()),
	})
	s.Autoscaler.ScaleDownStatus = "NoCandidates"
	s.PDBs = []*policyv1.PodDisruptionBudget{
		mother.Stale(mother.PDB("default", "web", 1, map[string]string{"app": "web"})),
	}

	findings := engine.Diagnose(s, config())

	var got []engine.Severity
	for _, f := range findings {
		got = append(got, f.Severity)
	}
	want := []engine.Severity{engine.Blocking, engine.Warning, engine.Info}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("severities = %v, want %v (%s)", got, want, render(findings))
	}
}

func TestDiagnoseIsReproducible(t *testing.T) {
	// Grouping and budget matching both pass through maps. Findings feed a
	// report an operator diffs between runs, so an unstable order would make
	// every run look like a change.
	pods := []*corev1.Pod{
		mother.Pod("default", "web-a", mother.OnNode("a"), mother.WithEmptyDir("scratch")),
		mother.Pod("default", "api-a", mother.OnNode("a"), mother.Bare()),
		mother.Pod("kube-system", "coredns", mother.OnNode("a")),
	}
	pdbs := []*policyv1.PodDisruptionBudget{
		mother.PDB("default", "one", 0, map[string]string{"app": "one"}),
		mother.PDB("default", "two", 0, map[string]string{"app": "two"}),
	}

	first := diagnose(nil, pods, pdbs...)
	for range 20 {
		if got := diagnose(nil, pods, pdbs...); !reflect.DeepEqual(got, first) {
			t.Fatalf("findings vary between runs:\n %s\n %s", render(first), render(got))
		}
	}
}

func TestEveryFindingCarriesItsGuidance(t *testing.T) {
	// The catalogue is a map keyed by code, so an uncatalogued code degrades to
	// a finding with no explanation and no fix rather than failing to compile.
	// This is the check that makes that impossible to ship.
	nodes := []*corev1.Node{
		inPool("a", mother.Cordoned(), mother.NodeAnnotations(map[string]string{
			engine.AnnotationDrainStarted: now.Add(-time.Hour).Format(time.RFC3339),
			engine.AnnotationBackoffUntil: now.Add(time.Hour).Format(time.RFC3339),
			engine.AnnotationLastFailure:  "eviction refused",
		})),
		sized("static", "8Gi", mother.InPool("pool-8g", "static-id")),
	}
	pods := []*corev1.Pod{
		mother.Pod("default", "orphan", mother.OnNode("a"), mother.Bare()),
		mother.Pod("default", "cache", mother.OnNode("a"), mother.WithEmptyDir("scratch")),
		mother.Pod("kube-system", "coredns", mother.OnNode("a")),
		mother.Pod("default", "pinned", mother.OnNode("a"), mother.SafeToEvict("false")),
		mother.PausePod("default", "filler", mother.OnNode("a"), mother.Priority(cutoff-1)),
		mother.Pod("default", "double", mother.OnNode("a"),
			mother.PodLabels(map[string]string{"app": "web", "tier": "front"})),
	}
	pdbs := []*policyv1.PodDisruptionBudget{
		mother.PDB("default", "by-app", 1, map[string]string{"app": "web"}),
		mother.PDB("default", "by-tier", 1, map[string]string{"tier": "front"}),
		mother.PDB("default", "zero", 0, map[string]string{"app": "zero"}),
		mother.Healthy(mother.PDB("default", "sick", 0, map[string]string{"app": "sick"}), 1, 2),
		mother.SelectsNothing(mother.PDB("default", "empty", 0, map[string]string{"app": "gone"})),
		mother.Stale(mother.PDB("default", "edited", 1, map[string]string{"app": "edited"})),
		mother.SyncFailed(mother.PDB("default", "unsyncable", 0, map[string]string{"app": "orphaned"}),
			`found no controllers for pod "orphaned-0"`),
		mother.UnparseableSelector(mother.PDB("default", "grandfathered", 1, nil)),
	}

	s := cluster(nodes, pods)
	s.PDBs = pdbs
	s.Autoscaler.ScaleDownStatus = "NoCandidates"
	s.Autoscaler.Groups = []engine.NodeGroup{
		{ID: poolID, MinSize: 1, MaxSize: 10, Ready: 1},
	}

	findings := engine.Diagnose(s, config())

	seen := map[string]bool{}
	for _, f := range findings {
		seen[f.Code] = true
		if f.Summary == "" {
			t.Errorf("%s has no summary: nothing tells the reader what it means", f.Code)
		}
		if f.Fix == "" {
			t.Errorf("%s has no fix: suggesting one is the whole purpose", f.Code)
		}
		if f.Subject == "" {
			t.Errorf("%s names no object", f.Code)
		}
		// Detail carries the specifics — numbers, a volume, a node — and the
		// diagnosis carries the prose. A detail long enough to hold a sentence
		// is one that has started repeating the summary or the fix, which is
		// exactly the duplication grouping exists to remove.
		if len(f.Detail) > 90 {
			t.Errorf("%s detail is %d chars, too long to be specifics: %q",
				f.Code, len(f.Detail), f.Detail)
		}
		for _, guidance := range []string{"safe-to-evict", "PodDisruptionBudget", "should", "Run one"} {
			if strings.Contains(f.Detail, guidance) {
				t.Errorf("%s detail contains guidance %q, which belongs in the fix: %q",
					f.Code, guidance, f.Detail)
			}
		}
		if strings.Contains(f.Summary, "uncatalogued") {
			t.Errorf("%s is emitted but absent from the catalogue", f.Code)
		}
	}

	// Every code except no-autoscaler, which needs a cluster this one is not.
	want := []string{
		engine.FindingNoCandidates, engine.FindingPoolNotAutoscaled, engine.FindingPoolAtMinimum,
		engine.FindingPDBZero, engine.FindingPDBUnhealthy, engine.FindingPDBSelectsNothing,
		engine.FindingPriorityBelow, engine.FindingAbandonedDrain, engine.FindingNodeInBackoff,
		engine.FindingPDBSyncFailed, engine.FindingPDBUnparseableSelector,
		engine.BlockedPDBStale, engine.BlockedMultiplePDBs, engine.BlockedBarePod,
		engine.BlockedLocalStorage, engine.BlockedSystemPod, engine.BlockedSafeToEvict,
	}
	for _, code := range want {
		if !seen[code] {
			t.Errorf("no %s finding, so its catalogue entry is never exercised", code)
		}
	}
}

func TestFindingsAreGroupedBySeverityThenCode(t *testing.T) {
	// The text report states each diagnosis once and lists its subjects
	// beneath, which only works if instances of one code arrive contiguously.
	pods := []*corev1.Pod{
		mother.Pod("a", "orphan", mother.OnNode("a"), mother.Bare()),
		mother.Pod("b", "cache", mother.OnNode("a"), mother.WithEmptyDir("scratch")),
		mother.Pod("c", "orphan", mother.OnNode("a"), mother.Bare()),
		mother.Pod("d", "cache", mother.OnNode("a"), mother.WithEmptyDir("scratch")),
	}

	var runs []string
	for _, f := range diagnose(nil, pods) {
		if len(runs) == 0 || runs[len(runs)-1] != f.Code {
			runs = append(runs, f.Code)
		}
	}

	seen := map[string]bool{}
	for _, code := range runs {
		if seen[code] {
			t.Errorf("code %s appears in more than one run: %v", code, runs)
		}
		seen[code] = true
	}
}

func TestDiagnoseMarksFindingsOnNodesNothingWillRemove(t *testing.T) {
	// A blocker on a static pool's node is real but costs nothing today: that
	// node was never going away. Suppressing it would be wrong — it goes live
	// the moment autoscaling is enabled — but sending someone to annotate five
	// workloads that free nothing is how a diagnostic tool loses its reader.
	nodes := []*corev1.Node{
		inPool("auto-1"), inPool("auto-2"), inPool("auto-3"),
		sized("static-1", "8Gi", mother.InPool("pool-8g", "static-id")),
	}
	pods := []*corev1.Pod{
		mother.Pod("default", "on-static", mother.OnNode("static-1"), mother.Bare()),
		mother.Pod("default", "on-auto", mother.OnNode("auto-1"), mother.Bare()),
	}

	bySubject := map[string]engine.Finding{}
	for _, f := range diagnose(nodes, pods) {
		bySubject[f.Subject] = f
	}

	if f := bySubject["default/on-static"]; !f.FreesNothing {
		t.Errorf("a blocker on a static pool's node is not marked: %+v", f)
	}
	if f := bySubject["default/on-auto"]; f.FreesNothing {
		t.Errorf("a blocker on an autoscaled node is wrongly marked: %+v", f)
	}
}

func TestDiagnoseMarksNothingWithoutAnAutoscaler(t *testing.T) {
	// Every pool is absent from a status document that does not exist, so
	// "not autoscaled" would be true of the whole cluster and mean nothing.
	s := cluster([]*corev1.Node{inPool("a")}, []*corev1.Pod{
		mother.Pod("default", "orphan", mother.OnNode("a"), mother.Bare()),
	})
	s.Autoscaler = engine.Autoscaler{}

	for _, f := range engine.Diagnose(s, config()) {
		if f.FreesNothing {
			t.Errorf("%s on %s marked as freeing nothing with no autoscaler to compare against",
				f.Code, f.Subject)
		}
	}
}

func TestDiagnoseFindsFillerThatCannotBeScheduled(t *testing.T) {
	// The failure this diagnosis exists for. A pause pod below the cutoff is
	// evicted by a burst and then stays Pending forever, because the
	// autoscaler will not scale up for a pod it considers expendable — and
	// nothing else in the cluster reports it. A check that required the pod to
	// be on a node was silent in exactly this case.
	pods := []*corev1.Pod{
		mother.PausePod("default", "filler-a", mother.Priority(cutoff-1), mother.Pending()),
		mother.PausePod("default", "filler-b", mother.Priority(cutoff-1), mother.Pending()),
	}
	for _, p := range pods {
		p.OwnerReferences[0].Name = "filler-rs"
	}

	f := only(t, diagnose(nil, pods), engine.FindingPriorityBelow)

	if !strings.Contains(f.Detail, "2 pending") {
		t.Errorf("detail does not report the unscheduled pods: %q", f.Detail)
	}
	// Nothing will place them, so this is a live problem, not a dormant one.
	if f.FreesNothing {
		t.Error("an unscheduled pod was treated as sitting on a node nothing will remove")
	}
}

func TestDiagnoseReportsPendingAndScheduledPodsTogether(t *testing.T) {
	pods := []*corev1.Pod{
		mother.PausePod("default", "filler-a", mother.Priority(cutoff-1), mother.OnNode("a")),
		mother.PausePod("default", "filler-b", mother.Priority(cutoff-1), mother.Pending()),
	}
	for _, p := range pods {
		p.OwnerReferences[0].Name = "filler-rs"
	}

	f := only(t, diagnose(nil, pods), engine.FindingPriorityBelow)

	if !strings.Contains(f.Detail, "a") || !strings.Contains(f.Detail, "1 pending") {
		t.Errorf("detail loses either the node or the pending pod: %q", f.Detail)
	}
}

func TestDiagnoseIgnoresUnscheduledPodsForEverythingElse(t *testing.T) {
	// A Pending pod holds no node open, so it cannot be why one is still
	// there. Only the priority check has anything to say about it.
	pods := []*corev1.Pod{
		mother.Pod("default", "orphan", mother.Bare(), mother.Pending()),
		mother.Pod("default", "cache", mother.WithEmptyDir("scratch"), mother.Pending()),
		mother.Pod("kube-system", "coredns", mother.Pending()),
	}

	if findings := diagnose(nil, pods); len(findings) != 0 {
		t.Errorf("unscheduled pods produced findings: %s", render(findings))
	}
}

func TestDiagnoseTreatsABudgetTemporarilyOutOfSlackAsTransient(t *testing.T) {
	// Three replicas, maxUnavailable: 1, so desiredHealthy is 2. One replica
	// is down: currentHealthy equals desiredHealthy exactly, the disruption
	// controller reports zero allowed, and the budget is fine again the moment
	// the third recovers. Nothing about it needs editing.
	pdb := mother.Replicas(
		mother.Healthy(mother.PDB("default", "web", 0, map[string]string{"app": "web"}), 2, 2), 3)

	findings := diagnose(nil, nil, pdb)

	f := only(t, findings, engine.FindingPDBUnhealthy)
	if !strings.Contains(f.Detail, "2 of 3 pods healthy") {
		t.Errorf("detail does not show the missing replica: %q", f.Detail)
	}
	none(t, findings, engine.FindingPDBZero)
}

func TestDiagnoseStillReportsABudgetThatCanNeverAllowADisruption(t *testing.T) {
	tests := []struct {
		name string
		pdb  *policyv1.PodDisruptionBudget
	}{
		{
			// The classic trap: one replica, minAvailable 1. Every pod it
			// selects is healthy and it can still never permit a disruption.
			"single replica at minAvailable 1",
			mother.Replicas(
				mother.Healthy(mother.PDB("default", "solo", 0, map[string]string{"app": "solo"}), 1, 1), 1),
		},
		{
			// minAvailable above the replica count. currentHealthy is below
			// desiredHealthy, which looks transient — but every selected pod
			// is already healthy, so nothing is coming to fix it.
			"minAvailable above the replica count",
			mother.Replicas(
				mother.Healthy(mother.PDB("default", "over", 0, map[string]string{"app": "over"}), 2, 5), 2),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			findings := diagnose(nil, nil, tc.pdb)
			only(t, findings, engine.FindingPDBZero)
			none(t, findings, engine.FindingPDBUnhealthy)
		})
	}
}

func TestDiagnoseDoesNotExcuseAFindingBecauseItsOtherPodsAreOnAStaticPool(t *testing.T) {
	// One filler pod on a pool nothing will remove, one Pending. Judging the
	// workload only by the nodes it occupies would call the whole finding
	// dormant — and the Pending pod is the live half, so the gate would let a
	// real broken buffer through.
	nodes := []*corev1.Node{
		inPool("auto-1"), inPool("auto-2"), inPool("auto-3"),
		sized("static-1", "8Gi", mother.InPool("pool-8g", "static-id")),
	}
	pods := []*corev1.Pod{
		mother.PausePod("default", "filler-a", mother.Priority(cutoff-1), mother.OnNode("static-1")),
		mother.PausePod("default", "filler-b", mother.Priority(cutoff-1), mother.Pending()),
	}
	for _, p := range pods {
		p.OwnerReferences[0].Name = "filler-rs"
	}

	f := only(t, diagnose(nodes, pods), engine.FindingPriorityBelow)

	if f.FreesNothing {
		t.Error("marked dormant despite an unscheduled pod that nothing will place")
	}
}

func TestDiagnoseReportsAPodWhoseTemplateCannotBeRead(t *testing.T) {
	// A pod binpack will never move, which nothing else in the cluster reports:
	// the autoscaler and kubectl drain are perfectly happy with it. Without
	// this the node simply never becomes a candidate and nobody can find out
	// why without reading binpack's source.
	exotic := mother.Pod("default", "shard-0", mother.OnNode("a"),
		mother.OwnedBy("KafkaCluster", "events"))

	findings := diagnose(nil, []*corev1.Pod{exotic})

	f := only(t, findings, engine.FindingNoTemplate)
	if f.Severity != engine.Warning {
		t.Errorf("severity = %s: it blocks binpack, not the cluster", f.Severity)
	}
	if !strings.Contains(f.Detail, "KafkaCluster events") {
		t.Errorf("detail does not name the controller to report: %q", f.Detail)
	}
	// A bare pod is already its own finding; saying it twice helps nobody.
	none(t, diagnose(nil, []*corev1.Pod{
		mother.Pod("default", "orphan", mother.OnNode("a"), mother.Bare()),
	}), engine.FindingNoTemplate)
}

func TestACordonBinpackLeftBehindIsReportedEvenWithoutItsMarkers(t *testing.T) {
	// The half-cleaned state: the annotations are gone and the cordon is not.
	// It is what the documented hand-back leaves behind between its first
	// command and its last, and nothing else in binpack can see it — the
	// engine skips it as `cordoned`, which is the same bucket as a node a
	// human cordoned. The label is the only thing left that says whose it is.
	labelled := inPool("a", mother.Cordoned(),
		mother.NodeLabels(map[string]string{engine.LabelDraining: "true"}))

	f := only(t, diagnose([]*corev1.Node{labelled, inPool("b"), inPool("c")}, nil),
		engine.FindingAbandonedDrain)
	if f.Subject != "a" {
		t.Errorf("subject = %q, want the node", f.Subject)
	}

	// A labelled node that is *not* cordoned is the other half-state, left by
	// a hand-back that got as far as uncordoning. That one has a reader
	// already: the next evaluation cordons it and resumes.
	schedulable := inPool("a", mother.NodeLabels(map[string]string{engine.LabelDraining: "true"}))
	none(t, diagnose([]*corev1.Node{schedulable, inPool("b"), inPool("c")}, nil),
		engine.FindingAbandonedDrain)
}

func TestADrainInFlightIsReportedOnceAndAsItself(t *testing.T) {
	// Begin writes the label and the markers in one patch, so every drain in
	// flight carries both keys the check now reads. Ungated, the label arm
	// fires on all of them — a second copy of a finding that is already noise
	// on a healthy cluster. Gated on the absence of the marker, it names only
	// the state nothing else can see.
	//
	// Both directions are asserted here: exactly one finding rather than two,
	// and the one that fired is the marker arm, which is the arm that can say
	// when the drain began.
	started := now.Add(-2 * time.Hour).Format(time.RFC3339)
	inFlight := inPool("a", mother.Cordoned(),
		mother.NodeLabels(map[string]string{engine.LabelDraining: "true"}),
		mother.NodeAnnotations(map[string]string{engine.AnnotationDrainStarted: started}))

	f := only(t, diagnose([]*corev1.Node{inFlight, inPool("b"), inPool("c")}, nil),
		engine.FindingAbandonedDrain)
	if !strings.Contains(f.Detail, started) {
		t.Errorf("detail = %q, want the marker arm's, naming when the drain began", f.Detail)
	}
}

func TestTheAbandonedDrainFixNamesEveryMarkerBinpackWrote(t *testing.T) {
	// The fix is the one instruction reached by an operator who uninstalled
	// binpack mid-drain, and it is followed literally. `kubectl annotate` does
	// not remove a label, so a fix naming only the annotations leaves the node
	// back in service still advertising a drain that ended — which is what the
	// label was added to make impossible.
	var fix string
	for _, d := range engine.Diagnoses() {
		if d.Code == engine.FindingAbandonedDrain {
			fix = d.Fix
		}
	}
	if fix == "" {
		t.Fatalf("no %s diagnosis in the catalogue", engine.FindingAbandonedDrain)
	}
	for _, key := range []string{"binpack.motleyhand.com/", engine.LabelDraining} {
		if !strings.Contains(fix, key) {
			t.Errorf("the fix does not name %s, so following it exactly leaves it behind: %s", key, fix)
		}
	}
}

func TestTheNoAutoscalerFindingDoesNotContradictItsOwnDetail(t *testing.T) {
	// The detail is Live's sentence, and the summary was a fixed claim above
	// it — so a document with no probe time printed "no cluster-autoscaler is
	// running" over "binpack cannot tell whether it is alive", which is one
	// report saying both that it knows and that it does not. Five
	// observations reach this finding and the summary has to hold for all of
	// them, because only the detail knows which one this was.
	for _, tc := range []struct {
		name       string
		autoscaler engine.Autoscaler
		detail     string
	}{
		{"nothing where binpack looked", engine.Autoscaler{}, "no cluster-autoscaler status"},
		{
			name:       "the object is there and empty",
			autoscaler: engine.Autoscaler{StatusFound: true},
			detail:     "carries no autoscalerStatus",
		},
		{
			name:       "a status the autoscaler named itself",
			autoscaler: engine.Autoscaler{StatusFound: true, ObservedStatus: "Initializing"},
			detail:     "Initializing",
		},
		{
			name:       "running, with no probe time",
			autoscaler: engine.Autoscaler{StatusFound: true, ObservedStatus: "Running", Running: true},
			detail:     "no probe time",
		},
		{
			name: "running, reporting the cluster unhealthy",
			autoscaler: engine.Autoscaler{
				StatusFound: true, ObservedStatus: "Running", Running: true,
				LastProbe: now.Add(-10 * time.Second), HealthStatus: engine.HealthUnhealthy,
			},
			detail: "unhealthy",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := cluster([]*corev1.Node{inPool("a")}, nil)
			s.Autoscaler = tc.autoscaler

			f := only(t, engine.Diagnose(s, config()), engine.FindingNoAutoscaler)

			if !strings.Contains(f.Detail, tc.detail) {
				t.Errorf("detail should name what was observed (%q), got: %s", tc.detail, f.Detail)
			}
			if strings.Contains(f.Summary, "no cluster-autoscaler is running") {
				t.Errorf("the summary asserts something binpack established in none of these "+
					"cases from one read:\n  %s\n  %s", f.Summary, f.Detail)
			}
		})
	}
}

// TestDiagnoseDoesNotReportASystemPodTheAutoscalerWouldAlreadyEvict is the
// diagnose half of the wiring TestTheSystemPodGraceIsMeasuredAgainstTheSnapshotsClock
// holds for the decision path. Diagnose calls checkPod itself and has its own
// clock to pass, so the two can be got right and wrong independently.
//
// The direction matters here more than in explain: this finding used to tell
// an operator to give a kube-system workload a PodDisruptionBudget, which on a
// managed platform may be a workload they do not own — a real availability
// decision, asked for to clear a blockage that expires by itself.
func TestDiagnoseDoesNotReportASystemPodTheAutoscalerWouldAlreadyEvict(t *testing.T) {
	old := mother.Pod("kube-system", "metrics-server", mother.OnNode("a"),
		mother.CreatedAt(now.Add(-2*time.Hour)))
	none(t, diagnose(nil, []*corev1.Pod{old}), engine.BlockedSystemPod)

	// And still reports one the autoscaler is still waiting on, or the check
	// would be satisfied by not looking.
	fresh := mother.Pod("kube-system", "konnectivity-agent", mother.OnNode("a"),
		mother.CreatedAt(now.Add(-5*time.Minute)))
	only(t, diagnose(nil, []*corev1.Pod{fresh}), engine.BlockedSystemPod)
}

// TestDiagnoseDoesNotBlameABudgetWhoseControllerFailedToSync separates a
// budget that is wrong from one nobody could compute.
//
// When trySync fails the disruption controller calls failSafe, which forces
// disruptionsAllowed to zero and records the reason on the DisruptionAllowed
// condition — and touches nothing else, so currentHealthy, desiredHealthy and
// expectedPods keep whatever the last successful sync wrote. Read without the
// condition, such a budget is indistinguishable from a one-replica
// minAvailable: 1, and binpack told the operator to run another replica. That
// changes nothing, and the operator's next conclusion is that binpack is
// wrong.
func TestDiagnoseDoesNotBlameABudgetWhoseControllerFailedToSync(t *testing.T) {
	// The commonest cause, and the one whose message says the most: a pod
	// whose controlling owner exposes no scale subresource, or has been
	// deleted out from under it.
	const message = `found no controllers for pod "web-0"`

	labelled := map[string]string{"app": "web"}
	budget := func() *policyv1.PodDisruptionBudget {
		return mother.PDB("default", "web", 0, labelled)
	}

	// One subtest per arm the sync-failed check has to precede, because the
	// stale counters can be left in any of these shapes and each one has its
	// own wrong answer. Testing a single shape pins the check's existence and
	// not its position, and position is the whole of it.
	misread := []struct {
		name    string
		pdb     *policyv1.PodDisruptionBudget
		instead string
	}{
		{"a budget whose last sync found every replica up",
			mother.Replicas(mother.Healthy(budget(), 3, 2), 3), engine.FindingPDBZero},
		{"a budget whose last sync found one missing",
			mother.Replicas(mother.Healthy(budget(), 2, 2), 3), engine.FindingPDBUnhealthy},
		// Before the first successful sync every counter is zero, which is
		// also exactly what a selector matching no pods looks like.
		{"a budget that has never synced at all",
			mother.SelectsNothing(budget()), engine.FindingPDBSelectsNothing},
	}

	for _, tc := range misread {
		t.Run(tc.name, func(t *testing.T) {
			findings := diagnose(nil, nil, mother.SyncFailed(tc.pdb, message))

			f := only(t, findings, engine.FindingPDBSyncFailed)
			if f.Severity != engine.Blocking {
				t.Errorf("severity = %s, want blocking: nothing can be evicted meanwhile", f.Severity)
			}
			if !strings.Contains(f.Detail, "found no controllers") {
				t.Errorf("detail does not carry the controller's own reason: %s", f.Detail)
			}
			none(t, findings, tc.instead)
		})
	}

	// The control, and the direction that must not move: the reason does the
	// discriminating, not the presence of a condition. An ordinary tight
	// budget carries one too — InsufficientPods, written by the same helper on
	// every successful sync — and must still be blamed.
	t.Run("an ordinary zero-allowance budget is still blamed", func(t *testing.T) {
		pdb := mother.Insufficient(budget())

		findings := diagnose(nil, nil, pdb)

		only(t, findings, engine.FindingPDBZero)
		none(t, findings, engine.FindingPDBSyncFailed)
	})
}

// TestDiagnoseReportsAnUnparseableSelectorRatherThanDroppingIt keeps the
// object visible now that it no longer drives a refusal.
//
// The eviction subresource skips such a budget, so binpack must too — but
// silently skipping it would lose the one thing worth saying about it, which
// is that somebody believes a workload is protected and no API server agrees.
func TestDiagnoseReportsAnUnparseableSelectorRatherThanDroppingIt(t *testing.T) {
	pdb := mother.UnparseableSelector(mother.PDB("default", "grandfathered", 1, nil))

	findings := diagnose(nil, nil, pdb)

	f := only(t, findings, engine.FindingPDBUnparseableSelector)
	if f.Severity != engine.Warning {
		t.Errorf("severity = %s, want warning: it blocks nothing", f.Severity)
	}
	if f.Subject != "default/grandfathered" {
		t.Errorf("subject = %q, want default/grandfathered", f.Subject)
	}
	// The value is the whole of the fix, and it is the one thing a reader
	// cannot get from the code: kubectl shows the object without saying which
	// part of it no client can read.
	if !strings.Contains(f.Detail, "Not A Valid Label Value!") {
		t.Errorf("detail does not show the selector that will not parse: %s", f.Detail)
	}

	// And it outranks the staleness an edit creates, unlike every other budget
	// finding. This one reads the spec, which is current whatever the status
	// is doing — and the pending sync will not settle it, since that sync
	// fails on the same unparseable selector.
	t.Run("an edit does not demote it to staleness", func(t *testing.T) {
		findings := diagnose(nil, nil,
			mother.Stale(mother.UnparseableSelector(mother.PDB("default", "grandfathered", 1, nil))))

		only(t, findings, engine.FindingPDBUnparseableSelector)
		none(t, findings, engine.BlockedPDBStale)
	})
}

// TestDiagnoseDoesNotSendAnOperatorAfterAStartingPodsBudgets is the report's
// half of the canIgnorePDB gate.
//
// diagnoseWorkloads counts matched budgets itself rather than going through
// CheckEvictable, so the two can disagree — and the disagreement is the
// expensive direction: multiple-pdbs is documented as unfixable by anything,
// so the report would send somebody to rewrite selectors over a pod the API
// server deletes without reading either budget.
func TestDiagnoseDoesNotSendAnOperatorAfterAStartingPodsBudgets(t *testing.T) {
	labelled := map[string]string{"app": "web", "tier": "front"}
	pdbs := []*policyv1.PodDisruptionBudget{
		mother.PDB("default", "by-app", 1, map[string]string{"app": "web"}),
		mother.PDB("default", "by-tier", 1, map[string]string{"tier": "front"}),
	}

	starting := mother.Pod("default", "web-0", mother.OnNode("a"),
		mother.PodLabels(labelled), mother.Starting())

	none(t, diagnose(nil, []*corev1.Pod{starting}, pdbs...), engine.BlockedMultiplePDBs)

	// The control: a running pod under the same pair is still reported, or
	// the gate has silenced the finding rather than narrowed it.
	running := mother.Pod("default", "web-0", mother.OnNode("a"), mother.PodLabels(labelled))

	only(t, diagnose(nil, []*corev1.Pod{running}, pdbs...), engine.BlockedMultiplePDBs)
}

// TestDiagnoseReportsAnEditedBudgetAsStaleEvenWhenItsLastSyncFailed settles
// which of two status-derived findings wins while the status is behind the
// spec, and the answer is neither of them: the staleness does.
//
// Nothing clears a condition when a spec is edited — only the disruption
// controller writes status, and it writes on sync — so a budget that was
// failing and has just been corrected still carries the old SyncFailed
// condition and its old message, with observedGeneration behind generation
// until the next sync. That window is the state an operator creates by acting
// on pdb-sync-failed, so it is common rather than exotic, and during it binpack
// does not yet know whether the edit worked.
//
// This is upstream's own precedence: checkAndDecrement compares
// observedGeneration against generation and returns 429 before it reads the
// condition at all. The rule is uniform — a status behind its spec is
// untrustworthy in every field, the condition included, which is the same
// reasoning that makes the counters untrustworthy under a sync failure.
func TestDiagnoseReportsAnEditedBudgetAsStaleEvenWhenItsLastSyncFailed(t *testing.T) {
	labelled := map[string]string{"app": "web"}
	failing := func() *policyv1.PodDisruptionBudget {
		return mother.SyncFailed(mother.PDB("default", "web", 0, labelled),
			`found no controllers for pod "web-0"`)
	}

	t.Run("an edit outranks the condition it has not yet cleared", func(t *testing.T) {
		findings := diagnose(nil, nil, mother.Stale(failing()))

		f := only(t, findings, engine.BlockedPDBStale)
		if f.Severity != engine.Warning {
			t.Errorf("severity = %s, want warning: binpack does not yet know the edit failed", f.Severity)
		}
		none(t, findings, engine.FindingPDBSyncFailed)
	})

	// The control: the edit is the discriminator, not the condition. A sync
	// failure the controller has caught up with is still reported as one.
	t.Run("a settled budget still reports the sync failure", func(t *testing.T) {
		findings := diagnose(nil, nil, failing())

		only(t, findings, engine.FindingPDBSyncFailed)
		none(t, findings, engine.BlockedPDBStale)
	})
}
