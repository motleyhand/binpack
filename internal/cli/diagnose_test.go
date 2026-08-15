package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/motleyhand/binpack/internal/engine"
)

func renderFindings(t *testing.T, format outputFormat, findings []engine.Finding) string {
	t.Helper()
	var buf bytes.Buffer
	if err := renderDiagnose(&options{output: format, out: &buf}, findings); err != nil {
		t.Fatalf("rendering: %v", err)
	}
	return buf.String()
}

var sample = []engine.Finding{
	{
		Diagnosis: engine.Diagnosis{
			Severity: engine.Blocking,
			Code:     engine.FindingPDBZero,
			Summary:  "permits no voluntary disruption while healthy",
			Fix:      "run a second replica, or relax the budget",
		},
		Subject: "default/web",
		Detail:  "1 of 1 pods healthy, 1 required (minAvailable: 1)",
	},
	{
		Diagnosis: engine.Diagnosis{
			Severity: engine.Blocking,
			Code:     engine.FindingPDBZero,
			Summary:  "permits no voluntary disruption while healthy",
			Fix:      "run a second replica, or relax the budget",
		},
		Subject: "other/api",
		Detail:  "2 of 2 pods healthy, 2 required (minAvailable: 2)",
	},
	{
		Diagnosis: engine.Diagnosis{
			Severity: engine.Warning,
			Code:     engine.FindingPriorityBelow,
			Summary:  "sits below the autoscaler's expendable cutoff",
			Fix:      "raise the priority to at or above the cutoff",
		},
		Subject: "default/ReplicaSet filler",
		Detail:  "priority -1000 (overprovisioning), cutoff -10",
	},
	{
		Diagnosis: engine.Diagnosis{
			Severity: engine.Info,
			Code:     engine.FindingPoolAtMinimum,
			Summary:  "is at its configured minimum size",
		},
		Subject: "pool-4g",
		Detail:  "3 node(s), minimum 3",
	},
}

func TestDiagnoseTextStatesEachDiagnosisOnceWithItsSubjectsBeneath(t *testing.T) {
	out := renderFindings(t, outputText, sample)

	// The whole point of grouping: fifteen namespaces deploying one Helm chart
	// is one mistake to explain, not fifteen.
	if n := strings.Count(out, "run a second replica"); n != 1 {
		t.Errorf("the shared fix appears %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "blocking · pdb-zero-disruptions (2)") {
		t.Errorf("heading does not lead with severity, code and count:\n%s", out)
	}
	// Both subjects still appear, each with its own numbers.
	for _, want := range []string{"default/web", "other/api", "minAvailable: 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("output drops %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "2 blocking, 1 warning, 1 informational") {
		t.Errorf("summary counts missing or wrong:\n%s", out)
	}
}

func TestDiagnoseTextOmitsAnEmptyFix(t *testing.T) {
	out := renderFindings(t, outputText, sample[3:])

	if strings.Contains(out, "fix:") {
		t.Errorf("printed an empty fix line:\n%s", out)
	}
}

func TestDiagnoseTextSaysWhatCleanMeans(t *testing.T) {
	out := renderFindings(t, outputText, nil)

	// "nothing found" must not be read as "nothing to save". The two questions
	// are independent, and conflating them is how a user concludes the tool
	// has nothing to offer them.
	if !strings.Contains(out, "binpack explain") {
		t.Errorf("clean report does not point at the other half of the question:\n%s", out)
	}
}

func TestDiagnoseJSONIsAnArrayEvenWhenEmpty(t *testing.T) {
	// A healthy cluster should not need special handling downstream, which a
	// null would force on every consumer.
	out := renderFindings(t, outputJSON, nil)

	if strings.TrimSpace(out) != "[]" {
		t.Errorf("empty report = %q, want []", strings.TrimSpace(out))
	}
}

func TestDiagnoseJSONCarriesEveryField(t *testing.T) {
	var got []findingView
	if err := json.Unmarshal([]byte(renderFindings(t, outputJSON, sample)), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	if len(got) != len(sample) {
		t.Fatalf("got %d findings, want %d", len(got), len(sample))
	}
	want := findingView{
		Severity: "blocking",
		Code:     engine.FindingPDBZero,
		Subject:  "default/web",
		Detail:   "1 of 1 pods healthy, 1 required (minAvailable: 1)",
		Summary:  "permits no voluntary disruption while healthy",
		Fix:      "run a second replica, or relax the budget",
	}
	if got[0] != want {
		t.Errorf("first finding = %+v, want %+v", got[0], want)
	}
	// Flat records, with the shared prose repeated: a consumer asking for
	// every blocking finding should not have to join two structures.
	if got[1].Summary != want.Summary {
		t.Errorf("second instance dropped its summary: %+v", got[1])
	}
	if got[3].Fix != "" {
		t.Errorf("an absent fix should be omitted, got %q", got[3].Fix)
	}
}

func TestDiagnoseIsRegisteredAndReadOnly(t *testing.T) {
	var buf bytes.Buffer
	cmd := NewRootCommand(&buf)
	cmd.SetArgs([]string{"help", "diagnose"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("diagnose is not registered: %v", err)
	}

	out := buf.String()
	// The promise not to remediate is load-bearing, and stated where a user
	// looking for a remediation flag will read it.
	if !strings.Contains(out, "never makes them") {
		t.Errorf("help does not state that diagnose only suggests:\n%s", out)
	}
	for _, flag := range []string{"--file", "--kubeconfig", "--context", "--output"} {
		if !strings.Contains(out, flag) {
			t.Errorf("help does not mention %s:\n%s", flag, out)
		}
	}
}

func TestDiagnoseTextStatesTheStaticCaveatOncePerGroup(t *testing.T) {
	group := []engine.Finding{
		{Diagnosis: engine.Diagnosis{Severity: engine.Blocking, Code: engine.BlockedBarePod,
			Summary: "no controller"}, Subject: "a/one", FreesNothing: true},
		{Diagnosis: engine.Diagnosis{Severity: engine.Blocking, Code: engine.BlockedBarePod,
			Summary: "no controller"}, Subject: "a/two", FreesNothing: true},
	}

	out := renderFindings(t, outputText, group)

	if n := strings.Count(out, "not autoscaled"); n != 1 {
		t.Errorf("the caveat appears %d times, want once for the whole group:\n%s", n, out)
	}
	if strings.Contains(out, "frees nothing today") {
		t.Errorf("per-line markers alongside the group note:\n%s", out)
	}
}

func TestDiagnoseTextMarksTheStaticOnesInAMixedGroup(t *testing.T) {
	group := []engine.Finding{
		{Diagnosis: engine.Diagnosis{Severity: engine.Blocking, Code: engine.BlockedBarePod,
			Summary: "no controller"}, Subject: "a/static", FreesNothing: true},
		{Diagnosis: engine.Diagnosis{Severity: engine.Blocking, Code: engine.BlockedBarePod,
			Summary: "no controller"}, Subject: "a/live"},
	}

	out := renderFindings(t, outputText, group)

	if strings.Contains(out, "none of these") {
		t.Errorf("group-wide note on a mixed group:\n%s", out)
	}
	if n := strings.Count(out, "frees nothing today"); n != 1 {
		t.Errorf("marked %d lines, want only the static one:\n%s", n, out)
	}
}

func exitCodeFor(t *testing.T, findings []engine.Finding, threshold failThreshold, static bool) int {
	t.Helper()
	err := exitFor(findings, threshold, static)
	if err == nil {
		return 0
	}
	return ExitCodeFor(err)
}

func TestDiagnoseExitsZeroByDefaultWhateverItFinds(t *testing.T) {
	// Reporting is the job; failing a build is an opt-in. A diagnostic that
	// broke pipelines the day it was installed would be uninstalled the same
	// day.
	if code := exitCodeFor(t, sample, failNever, false); code != 0 {
		t.Errorf("exit = %d with --fail-on never, want 0", code)
	}
}

func TestDiagnoseFailOnIsAThreshold(t *testing.T) {
	tests := []struct {
		threshold failThreshold
		findings  []engine.Finding
		want      int
	}{
		{failBlocking, sample, ExitFindings},
		{failWarning, sample, ExitFindings},
		// Blocking findings only. A threshold fails at the lower setting too;
		// an exact-match check would let these through.
		{failBlocking, sample[:2], ExitFindings},
		{failWarning, sample[:2], ExitFindings},
		// Warnings and info only: blocking passes, warning fails.
		{failBlocking, sample[2:], 0},
		{failWarning, sample[2:], ExitFindings},
		// Info only: neither threshold fires, since info is the cluster
		// working as intended.
		{failBlocking, sample[3:], 0},
		{failWarning, sample[3:], 0},
		{failWarning, nil, 0},
	}

	for _, tc := range tests {
		got := exitCodeFor(t, tc.findings, tc.threshold, false)
		if got != tc.want {
			t.Errorf("--fail-on %s over %d finding(s): exit = %d, want %d",
				tc.threshold, len(tc.findings), got, tc.want)
		}
	}
}

func TestDiagnoseDoesNotFailForFindingsThatFreeNothing(t *testing.T) {
	// A gate that fails today over a node that was never going away is one a
	// team turns off, and then it catches nothing at all.
	onStatic := []engine.Finding{{
		Diagnosis:    engine.Diagnosis{Severity: engine.Blocking, Code: engine.BlockedBarePod},
		Subject:      "a/orphan",
		FreesNothing: true,
	}}

	if code := exitCodeFor(t, onStatic, failBlocking, false); code != 0 {
		t.Errorf("exit = %d, want 0: nothing will remove that node anyway", code)
	}
	if code := exitCodeFor(t, onStatic, failBlocking, true); code != ExitFindings {
		t.Errorf("exit = %d with --fail-on-static-pools, want %d", code, ExitFindings)
	}
}

func TestDiagnoseSaysWhatItDidNotCount(t *testing.T) {
	mixed := []engine.Finding{
		{Diagnosis: engine.Diagnosis{Severity: engine.Blocking, Code: engine.BlockedBarePod},
			Subject: "a/live"},
		{Diagnosis: engine.Diagnosis{Severity: engine.Blocking, Code: engine.BlockedBarePod},
			Subject: "a/static", FreesNothing: true},
	}

	err := exitFor(mixed, failBlocking, false)
	if err == nil {
		t.Fatal("no failure for a live blocking finding")
	}
	// Otherwise a count that disagrees with the report printed above it is
	// merely puzzling.
	if !strings.Contains(err.Error(), "1 finding(s) at or above blocking") {
		t.Errorf("message does not give the counted total: %q", err)
	}
	if !strings.Contains(err.Error(), "--fail-on-static-pools") {
		t.Errorf("message does not say how to include the rest: %q", err)
	}
}

func TestExitCodeForKeepsOrdinaryFailuresAtOne(t *testing.T) {
	// A CI job has to tell "your cluster has blockers" from "diagnose could
	// not reach the cluster", and every other command already exits 1.
	if code := ExitCodeFor(errors.New("connection refused")); code != 1 {
		t.Errorf("exit = %d for an ordinary error, want 1", code)
	}
	if ExitFindings == 1 {
		t.Error("the findings status is indistinguishable from a runtime failure")
	}
}

func TestDiagnoseRejectsAnUnknownFailOnBeforeReadingTheCluster(t *testing.T) {
	var buf bytes.Buffer
	cmd := NewRootCommand(&buf)
	// An unusable kubeconfig, so this both stays hermetic and proves the order:
	// were the flag checked after the cluster read, the failure below would be
	// about the kubeconfig instead.
	cmd.SetArgs([]string{"diagnose", "--fail-on", "everything",
		"--kubeconfig", filepath.Join(t.TempDir(), "no-such-kubeconfig")})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("accepted an unknown --fail-on value")
	}
	if !strings.Contains(err.Error(), "invalid --fail-on") {
		t.Errorf("error does not name the flag: %v", err)
	}
	if code := ExitCodeFor(err); code != 1 {
		t.Errorf("exit = %d for a bad flag, want 1: it is a usage error, not a verdict", code)
	}
}
