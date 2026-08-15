package cli

import (
	"bytes"
	"encoding/json"
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
