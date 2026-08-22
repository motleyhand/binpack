package cli

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/motleyhand/binpack/api/v1alpha1"
	"github.com/motleyhand/binpack/internal/controller"
)

// The chart's default configuration must be one binpack accepts.
//
// A chart that renders a document the binary rejects is a crash loop nobody
// discovers until they install it, and `helm lint` cannot see it: the config
// is an opaque string as far as Helm is concerned. This checks the values the
// chart ships, which is what almost every install will actually run.
func TestTheChartsDefaultConfigurationIsValid(t *testing.T) {
	values, err := os.ReadFile("../../charts/binpack/values.yaml")
	if err != nil {
		t.Fatalf("reading the chart's values: %v", err)
	}

	// The `config:` block, rendered into the ConfigMap verbatim.
	block, ok := section(string(values), "config:")
	if !ok {
		t.Fatal("the chart has no config: block to render")
	}

	cfg, err := v1alpha1.Load([]byte(
		"apiVersion: binpack.motleyhand.com/v1alpha1\nkind: BinpackConfig\n" + block))
	if err != nil {
		t.Fatalf("the chart's default configuration is one binpack rejects: %v", err)
	}

	// Acting is opt-in. A chart that shipped dryRun: false would drain nodes
	// on install, which is not a thing anyone should be able to do by accident.
	if s := cfg.Settings(); s.DryRun != true {
		t.Error("the chart does not default to dry run")
	}
}

// section returns the indented body following a top-level key, de-indented.
func section(doc, key string) (string, bool) {
	lines := strings.Split(doc, "\n")
	var out []string
	for i, line := range lines {
		if line != key {
			continue
		}
		for _, body := range lines[i+1:] {
			if body == "" || strings.HasPrefix(body, "  ") {
				out = append(out, strings.TrimPrefix(body, "  "))
				continue
			}
			if strings.HasPrefix(body, "#") {
				continue
			}
			break
		}
		return strings.Join(out, "\n"), true
	}
	return "", false
}

// The pod must outlive the shutdown it is being asked to perform.
//
// controller-runtime cancels the runnables, waits for them to return — up to
// the graceful window binpack sets — and only then releases the lease. A pod
// SIGKILLed before that window is out is killed at the moment the handover
// would have happened, so the next leader waits out the whole lease instead,
// which is the cost releasing it exists to avoid. Kubernetes' own default is
// 30s, so this is not something a chart can leave unsaid and hope.
func TestTheChartGivesTheShutdownTimeToHappen(t *testing.T) {
	deployment, err := os.ReadFile("../../charts/binpack/templates/deployment.yaml")
	if err != nil {
		t.Fatalf("reading the chart's deployment: %v", err)
	}

	m := regexp.MustCompile(`terminationGracePeriodSeconds:\s*(\d+)`).
		FindStringSubmatch(string(deployment))
	if m == nil {
		t.Fatal("the chart sets no terminationGracePeriodSeconds, so the pod is killed on " +
			"Kubernetes' default and the shutdown has whatever is left of it")
	}
	seconds, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("terminationGracePeriodSeconds is not a number: %v", err)
	}
	grace := time.Duration(seconds) * time.Second

	if grace <= controller.DefaultGracefulShutdown {
		t.Errorf("the pod is killed after %s, inside binpack's own %s shutdown window",
			grace, controller.DefaultGracefulShutdown)
	}
	// And not so long that waiting for the old pod costs more than the lease it
	// is trying to hand over.
	if grace >= controller.DefaultLeaseDuration {
		t.Errorf("a shutdown may take %s, which is no faster than waiting out the %s lease",
			grace, controller.DefaultLeaseDuration)
	}
}

// binpack's roles must never be bound to the namespace's default ServiceAccount.
//
// `serviceAccount.create: false` with `serviceAccount.name` left empty is the
// `helm create` scaffold's fallback, and it reads as safe — `default` is the
// account a pod gets when it names none. That is exactly what makes binding to
// it wrong: every other pod in the release namespace that names no account of
// its own inherits whatever binpack was granted, which under
// `rbac.allowDraining` is cluster-wide `nodes: patch` and `pods/eviction:
// create`. binpack itself runs perfectly well as `default`, so nothing reports
// the over-grant; the install works, wrongly.
//
// The name reaches all three bindings and the Deployment through one helper,
// so there is no half of it the chart could get right. The helper has to
// refuse, the way _validate.tpl refuses the other combination that installs
// cleanly and then does the wrong thing.
//
// Read from the templates rather than rendered, because `make check` may not
// have Helm: CONTRIBUTING asks for Go and golangci-lint, and
// hack/check-workflows.py holds the release job to whatever the build job
// installs, so shelling out to `helm template` would either skip in CI or make
// Helm a release-time dependency. The render itself is asserted in the chart
// CI job, which has Helm.
func TestTheChartNeverBindsTheDefaultServiceAccount(t *testing.T) {
	bindings := roleBindings(t)
	// Three: the ClusterRoleBinding, the autoscaler-status RoleBinding in
	// kube-system, and the leader-election RoleBinding. A parse that found
	// none would satisfy every assertion below without checking anything.
	if len(bindings) < 3 {
		t.Fatalf("found %d subjects in the chart's bindings, want the three it renders: "+
			"the parse has lost the structure it is checking", len(bindings))
	}

	const helper = `include "binpack.serviceAccountName"`
	for _, b := range bindings {
		if !strings.Contains(b.subject, helper) {
			t.Errorf("the %s at rbac.yaml:%d binds %s, which does not come from %s; "+
				"a guard on one helper covers only the bindings that use it",
				b.kind, b.line, b.subject, helper)
		}
	}

	managed, external := serviceAccountNameArms(t)

	// The arm taken when the operator manages the ServiceAccount themselves.
	if refusal := refusalIn(external); refusal == "" {
		for _, b := range bindings {
			t.Errorf("the %s at rbac.yaml:%d binds the %q ServiceAccount when "+
				"serviceAccount.create is false and serviceAccount.name is unset",
				b.kind, b.line, literalIn(external))
		}
	} else {
		// Named values, not prose: the operator has to be told which one to
		// set and when, and a reworded refusal that stopped saying so would
		// otherwise still pass.
		for _, value := range []string{"serviceAccount.name", "serviceAccount.create"} {
			if !strings.Contains(refusal, value) {
				t.Errorf("the refusal does not name %s, so it does not say what to set: %q",
					value, refusal)
			}
		}
	}

	// And the ordinary install must still render. A refusal that fired on the
	// chart's own defaults would break every install there is.
	if refusal := refusalIn(managed); refusal != "" {
		t.Errorf("the chart refuses to render when it creates the ServiceAccount itself: %q", refusal)
	}
	if !strings.Contains(managed, `include "binpack.fullname"`) {
		t.Errorf("a created ServiceAccount is no longer named after the release: %s", oneLine(managed))
	}
}

// roleBinding is one subject of one binding, as the template writes it.
type roleBinding struct {
	kind    string // ClusterRoleBinding or RoleBinding
	line    int    // in rbac.yaml, so a failure can be read against the source
	subject string // the expression the subject's name comes from
}

// roleBindings reads every binding subject out of the chart's RBAC template.
func roleBindings(t *testing.T) []roleBinding {
	t.Helper()

	rbac, err := os.ReadFile("../../charts/binpack/templates/rbac.yaml")
	if err != nil {
		t.Fatalf("reading the chart's RBAC: %v", err)
	}

	var bindings []roleBinding
	var kind, section string
	for i, line := range strings.Split(string(rbac), "\n") {
		switch {
		case strings.HasPrefix(line, "---"):
			kind, section = "", ""
		case strings.HasPrefix(line, "kind: "):
			kind, section = strings.TrimPrefix(line, "kind: "), ""
		case !strings.HasPrefix(line, " ") && strings.HasSuffix(line, ":"):
			section = strings.TrimSuffix(line, ":")
		case section == "subjects" && strings.HasPrefix(line, "    name: ") &&
			strings.HasSuffix(kind, "Binding"):
			bindings = append(bindings, roleBinding{
				kind:    kind,
				line:    i + 1,
				subject: strings.TrimPrefix(line, "    name: "),
			})
		}
	}
	return bindings
}

// serviceAccountNameArms returns the two arms of binpack.serviceAccountName:
// what it names when the chart creates the ServiceAccount, and what it names
// when the operator manages it elsewhere.
func serviceAccountNameArms(t *testing.T) (managed, external string) {
	t.Helper()

	helpers, err := os.ReadFile("../../charts/binpack/templates/_helpers.tpl")
	if err != nil {
		t.Fatalf("reading the chart's helpers: %v", err)
	}

	const opening = `{{- define "binpack.serviceAccountName" -}}`
	start := strings.Index(string(helpers), opening)
	if start < 0 {
		t.Fatal("the chart has no binpack.serviceAccountName helper, though its bindings name one")
	}
	body := string(helpers)[start+len(opening):]
	if next := strings.Index(body, `{{- define "`); next >= 0 {
		body = body[:next]
	}

	if !strings.Contains(body, ".Values.serviceAccount.create") {
		t.Fatalf("binpack.serviceAccountName no longer branches on serviceAccount.create: %s", oneLine(body))
	}
	arms := strings.SplitN(body, "{{- else -}}", 2)
	if len(arms) != 2 {
		t.Fatalf("binpack.serviceAccountName is no longer one if/else, so this test cannot "+
			"tell its arms apart: %s", oneLine(body))
	}
	return arms[0], arms[1]
}

// refusalIn returns the message a template arm refuses with, or "" if it
// renders a name instead.
func refusalIn(arm string) string {
	return firstGroup(regexp.MustCompile(`(?:required|fail)\s+"([^"]*)"`), arm)
}

// literalIn returns the name an arm falls back to when the value it prefers is
// empty.
func literalIn(arm string) string {
	return firstGroup(regexp.MustCompile(`default\s+"([^"]*)"`), arm)
}

// oneLine folds a template fragment onto one line, so a failure reads as one
// failure rather than as a stray block of chart.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func firstGroup(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// The Role granting the autoscaler's status must be bound where binpack will
// look for it.
//
// This is the half of a configurable namespace that installs cleanly and then
// does not work. binpack reads `discovery.autoscalerNamespace`; the Role is
// namespaced, and RBAC in the wrong namespace produces a 403 on the very first
// read — which the controller counts as a failed evaluation and the one-shot
// commands report as a failure to read the cluster. Neither says "your Role is
// in kube-system and your autoscaler is not".
//
// So the two must come from one value. Asserted against the chart's text
// because `helm lint` cannot see it either: to Helm, the config is an opaque
// string and the Role's namespace is a different key entirely.
func TestTheChartBindsTheAutoscalerStatusRoleWhereBinpackReads(t *testing.T) {
	chart, err := os.ReadFile("../../charts/binpack/templates/rbac.yaml")
	if err != nil {
		t.Fatalf("reading the chart's RBAC: %v", err)
	}

	// Every object named for the autoscaler's status: the Role and the
	// RoleBinding, both namespaced, and both wrong in the same way if either
	// is pinned.
	var found int
	for _, doc := range strings.Split(string(chart), "\n---\n") {
		if !strings.Contains(doc, "-autoscaler-status") {
			continue
		}
		found++
		for _, line := range strings.Split(doc, "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "namespace:") {
				continue
			}
			value := strings.TrimSpace(strings.TrimPrefix(line, "namespace:"))
			// The ServiceAccount subject is in the release's namespace and
			// has nothing to do with where the autoscaler runs.
			if strings.Contains(value, ".Release.Namespace") {
				continue
			}
			if !strings.Contains(value, "autoscalerNamespace") {
				t.Errorf("the autoscaler-status objects are bound to %s, which does not "+
					"move with discovery.autoscalerNamespace: binpack would read a "+
					"namespace it has no Role in and 403 on every evaluation", value)
			}
		}
	}
	if found != 2 {
		t.Fatalf("found %d autoscaler-status objects, want the Role and its RoleBinding; "+
			"this test asserts nothing if it cannot find them", found)
	}
}

// The chart's default namespace and the binary's must be the same one.
//
// They are two defaults for one question — the chart renders a config document
// and the binary fills in what the document omits — and a disagreement between
// them is invisible until the day somebody deletes the line from their values.
func TestTheChartAndTheBinaryDefaultToTheSameAutoscalerNamespace(t *testing.T) {
	values, err := os.ReadFile("../../charts/binpack/values.yaml")
	if err != nil {
		t.Fatalf("reading the chart's values: %v", err)
	}
	block, ok := section(string(values), "config:")
	if !ok {
		t.Fatal("the chart has no config: block to render")
	}
	cfg, err := v1alpha1.Load([]byte(
		"apiVersion: binpack.motleyhand.com/v1alpha1\nkind: BinpackConfig\n" + block))
	if err != nil {
		t.Fatalf("the chart's default configuration is one binpack rejects: %v", err)
	}

	if cfg.Discovery.AutoscalerNamespace != v1alpha1.DefaultAutoscalerNamespace {
		t.Errorf("the chart installs with autoscalerNamespace %q and the binary defaults "+
			"to %q", cfg.Discovery.AutoscalerNamespace, v1alpha1.DefaultAutoscalerNamespace)
	}

	// And that the field is visible in the file operators actually read. It
	// is not an ordinary tunable: the Role's namespace is derived from it, so
	// somebody whose autoscaler is not in kube-system has to change it and
	// cannot discover that from a document it is absent from. Defaulting
	// would otherwise hide it completely, since an omitted field renders no
	// line into the ConfigMap either.
	if !strings.Contains(block, "autoscalerNamespace") {
		t.Error("the chart's config: block does not mention autoscalerNamespace, so " +
			"nothing an operator reads says binpack looks in one namespace or which")
	}
}
