package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/motleyhand/binpack/api/v1alpha1"
	"github.com/motleyhand/binpack/internal/controller"
	"github.com/motleyhand/binpack/internal/rbacdoc"
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
	for doc := range strings.SplitSeq(string(chart), "\n---\n") {
		if !strings.Contains(doc, "-autoscaler-status") {
			continue
		}
		found++

		// Which namespace this is, read from the document's structure rather
		// than from what its value mentions. A binding carries two: the
		// object's own, under `metadata`, and the ServiceAccount subject's,
		// under `subjects` — and the second is the release's namespace and has
		// nothing to do with where the autoscaler runs. Classifying them by
		// looking for `.Release.Namespace` in the value skipped anything that
		// mentioned it, so `{{ printf "%s-wrong" .Release.Namespace }}` on the
		// object itself was read as the subject's line and never checked at
		// all. Same section-tracking as roleBindings above.
		var section string
		var namespaces int
		for line := range strings.SplitSeq(doc, "\n") {
			if !strings.HasPrefix(line, " ") && strings.HasSuffix(line, ":") {
				section = strings.TrimSuffix(line, ":")
			}
			line = strings.TrimSpace(line)
			if section != "metadata" || !strings.HasPrefix(line, "namespace:") {
				continue
			}
			namespaces++
			value := strings.TrimSpace(strings.TrimPrefix(line, "namespace:"))
			// The whole expression, not a substring of it. Containment says
			// the helper is mentioned; what has to hold is that its value is
			// the namespace, and `printf "%s-wrong" (include …)` mentions it
			// while creating the Role somewhere else. Both objects would move
			// together, so comparing them with each other sees nothing either.
			if pipeline, ok := helmPipeline(value); !ok ||
				pipeline[0] != `include "binpack.autoscalerNamespace" .` {
				t.Errorf("the autoscaler-status objects are bound to %s rather than to "+
					"discovery.autoscalerNamespace itself: binpack reads the namespace that "+
					"setting names, and a Role anywhere else is a 403 on every evaluation",
					value)
			} else {
				// And nothing but quoting after it. A stage that transforms the
				// value moves the Role without moving what binpack reads.
				for _, stage := range pipeline[1:] {
					if stage != "quote" {
						t.Errorf("the autoscaler-status namespace passes "+
							"discovery.autoscalerNamespace through %q; whatever that "+
							"renders, it is not the namespace binpack reads", stage)
					}
				}
			}
			// Quoted, because `metadata.namespace` is a string and a bare
			// namespace name is not necessarily a YAML string. `true`, `false`
			// and `123` are all legal DNS-1123 labels and therefore legal
			// namespace names, and rendered unquoted they come back out of the
			// YAML as a bool or an int: `kubectl apply` says "cannot unmarshal
			// bool into Go struct field ObjectMeta.metadata.namespace of type
			// string" and rejects these two objects while installing the other
			// seven. That is the install-cleanly-then-403 shape again, reached
			// through Helm rather than through RBAC.
			if !strings.Contains(value, "quote") && !strings.HasPrefix(value, `"`) {
				t.Errorf("the autoscaler-status namespace is rendered unquoted (%s), so a "+
					"namespace named `true` or `123` renders as a bool or an int and the "+
					"API server refuses the object", value)
			}
		}

		// Present, not merely right when present. Every check in this loop is
		// reached by a namespace line existing, so removing it from both
		// documents left the objects agreeing with each other, the identity
		// and binding checks satisfied, and nothing following
		// discovery.autoscalerNamespace at all — which is a Role in the
		// release's namespace and a 403 on every read the autoscaler is
		// anywhere else.
		if namespaces != 1 {
			t.Errorf("an autoscaler-status object sets %d metadata namespaces, want the one "+
				"that follows discovery.autoscalerNamespace; without it the object is "+
				"created wherever the release is", namespaces)
		}
	}
	if found != 2 {
		t.Fatalf("found %d autoscaler-status objects, want the Role and its RoleBinding; "+
			"this test asserts nothing if it cannot find them", found)
	}
}

// helmPipeline splits a field whose whole value is one Helm action into the
// stages of its pipeline, and reports whether it is one.
//
// A field written as anything other than a single action — a literal, or an
// action with text around it — is not a pipeline and the caller has to say so
// in its own terms.
func helmPipeline(value string) ([]string, bool) {
	body, ok := strings.CutPrefix(strings.TrimSpace(value), "{{")
	if !ok {
		return nil, false
	}
	body, ok = strings.CutSuffix(body, "}}")
	if !ok {
		return nil, false
	}

	var stages []string
	for _, stage := range strings.Split(strings.Trim(body, "-"), "|") {
		stages = append(stages, strings.TrimSpace(stage))
	}
	return stages, len(stages) > 0
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

// Every `kubectl -n <ns>` example in the documentation must name a namespace
// the reader will actually have.
//
// There are only two such namespaces, and they arrive by different routes.
// binpack's own lives wherever the install put it, which the install how-to
// fixes with `--namespace`; the cluster-autoscaler's is discovered rather than
// chosen, and defaults to the one `v1alpha1.DefaultAutoscalerNamespace` names.
// Anything else is a namespace nobody was told to create.
//
// The failure this catches is quiet in the worst way. A wrong namespace does
// not produce a wrong answer, it produces `Error from server (NotFound):
// deployments.apps "binpack" not found` — which reads as "binpack is not
// installed" rather than "look in the other namespace", so the reader stops
// trusting the install rather than the command. The one place this mattered
// most was the reference's only documented way to ask the *deployed* binpack
// what it decided; a reader whose copy-paste failed there falls back to
// running the command on their laptop, which answers about built-in defaults
// — the exact substitution that section exists to warn against.
func TestEveryDocumentedNamespaceIsOneAnInstallCreates(t *testing.T) {
	install, err := os.ReadFile("../../docs/how-to/install-binpack.md")
	if err != nil {
		t.Fatalf("reading the install how-to: %v", err)
	}

	// The namespaces the how-to actually installs into.
	installed := map[string]bool{}
	for _, m := range regexp.MustCompile(
		`--namespace\s+([A-Za-z0-9-]+)`).FindAllStringSubmatch(string(install), -1) {
		installed[m[1]] = true
	}
	if len(installed) == 0 {
		t.Fatal("found no --namespace in the install how-to; this test asserts " +
			"nothing if it cannot read the namespaces it is comparing against")
	}

	// Plus the autoscaler's, which no install command creates because binpack
	// does not install it — it reads one object out of it.
	installed[v1alpha1.DefaultAutoscalerNamespace] = true

	// The flag is read from anywhere in the command, not just straight after
	// `kubectl`. Both positions are valid and both are used here — the docs
	// carry `kubectl -n kube-system get deploy` and `kubectl get pdb -n NS`
	// — so a pattern anchored to the first position inspects about half the
	// examples while reading as though it inspected all of them.
	flag := regexp.MustCompile(`(?:^|\s)(?:-n|--namespace)[=\s]+(\S+)`)
	// A namespace is an RFC 1123 label. Anything else in that position is a
	// placeholder — `NS`, `<ns>`, `<discovery.autoscalerNamespace>` — and is
	// skipped rather than reported, by a rule rather than a list of spellings,
	// so a new placeholder convention needs no change here. The distinction is
	// safe in the direction that matters: the defect this test exists for was
	// `binpack` for `binpack-system`, and both are legal labels.
	namespace := regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

	checked, skipped := 0, 0
	// README.md is walked too: it is the highest-traffic surface of the lot and
	// carries the same kind of example.
	roots := []string{"../../docs", "../../README.md"}
	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for i, line := range joinContinuations(strings.Split(string(body), "\n")) {
			cmd := strings.Index(line, "kubectl ")
			if cmd < 0 {
				continue
			}
			// Only as far as the first pipe: `kubectl describe node <n> | sed
			// -n '...'` is one line with two `-n` flags on it, and the second
			// belongs to sed.
			run := line[cmd:]
			if pipe := strings.Index(run, "|"); pipe >= 0 {
				run = run[:pipe]
			}
			for _, m := range flag.FindAllStringSubmatch(run, -1) {
				// Markup travels with the token when the command is written
				// inline rather than fenced — "`kubectl edit deploy -n
				// kube-system`" ends the namespace with a backtick. Angle
				// brackets are deliberately not trimmed: `<ns>` is a
				// placeholder and must stay one.
				ns := strings.Trim(m[1], "`'\",.;)")
				// A placeholder is skipped and a typo is reported, and telling
				// them apart by "is this a legal namespace" got that backwards:
				// `Binpack_system` is not a legal label, so it read as a
				// placeholder and a reader copying it gets an error from
				// kubectl before any resource is addressed. Placeholders are
				// recognised by their shape instead — angle brackets, a shell
				// variable, or the page's own sentinel.
				if placeholder(ns) {
					skipped++
					continue
				}
				if len(ns) > 63 || !namespace.MatchString(ns) {
					t.Errorf("%s:%d uses namespace %q, which kubectl refuses: a namespace is "+
						"a DNS label, at most 63 characters of lowercase alphanumerics and "+
						"dashes: %s", path, i+1, ns, strings.TrimSpace(line))
					continue
				}
				checked++
				if !installed[ns] {
					t.Errorf("%s:%d uses namespace %q, which no install command in "+
						"docs/how-to/install-binpack.md creates: %s",
						path, i+1, ns, strings.TrimSpace(line))
				}
			}
		}
		return nil
	}
	for _, root := range roots {
		if err := filepath.WalkDir(root, walk); err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	// Both counts, because the two ways this stops asserting anything are the
	// pattern matching nothing at all and every match being written off as a
	// placeholder.
	if checked == 0 {
		t.Fatalf("found no real namespace in a `kubectl` example under docs/ or the "+
			"README (%d placeholders skipped); this test asserts nothing if its "+
			"pattern has stopped matching", skipped)
	}
}

// TestEachNamespacedRoleGrantsWhatItsOwnNamespaceIsFor completes the promise
// the binding test above makes.
//
// That test holds the autoscaler-status Role and RoleBinding to the namespace
// binpack reads the status from. It says nothing about what that Role grants —
// and the two are one promise, because a Role bound in the right namespace
// granting nothing binpack needs there fails exactly as a Role bound in the
// wrong one does: a 403 on every evaluation, from an install that came up
// clean.
//
// The gap is invisible to the RBAC comparisons in capability_doc_test.go,
// which key on group, resource and verb. Those keys are right for a
// ClusterRole, whose grants apply everywhere. They cannot express a namespaced
// one, and this chart renders two into two different namespaces: the
// autoscaler's, which the operator names through discovery.autoscalerNamespace,
// and binpack's own. Moving the ConfigMap read from the first Role to the
// second left every test in this repository green, while any install whose
// release namespace differs from the autoscaler's — the ordinary case, since
// the autoscaler publishes into the namespace it runs in — reads a namespace
// it holds no grant for.
//
// The namespaces themselves cannot be compared here: the chart writes both as
// Helm expressions, and rendering them means running Helm. The names can, and
// they are what the objects are actually distinguished by.
func TestEachNamespacedRoleGrantsWhatItsOwnNamespaceIsFor(t *testing.T) {
	chart, err := os.ReadFile(rbacdoc.ChartPath)
	if err != nil {
		t.Fatalf("reading the chart's RBAC: %v", err)
	}
	roles, err := rbacdoc.Roles(string(chart))
	if err != nil {
		t.Fatalf("reading the chart's rules: %v", err)
	}

	namespaced := rbacdoc.OfKind(roles, "Role")
	if len(namespaced) != 2 {
		t.Fatalf("found %d namespaced Roles, want the autoscaler-status one and the "+
			"leader-election one; this test asserts nothing if it cannot find them",
			len(namespaced))
	}

	grants := map[string]map[string]bool{}
	for _, role := range namespaced {
		grants[role.Metadata.Name] = rbacdoc.Grants([]rbacdoc.Role{role})
	}

	// Disjoint, which is the general form and needs no list. Two Roles in two
	// namespaces granting the same pair means one of them grants it somewhere
	// nobody asked for it.
	for name, mine := range grants {
		for other, theirs := range grants {
			if name == other {
				continue
			}
			for pair := range mine {
				if theirs[pair] {
					t.Errorf("%s and %s both grant %q, and they are created in different "+
						"namespaces; one of them is granting it where binpack does not "+
						"read", name, other, pair)
				}
			}
		}
	}

	// Neither may be empty. A Role that grants nothing is still an object the
	// chart creates and binds, so it satisfies disjointness and every count
	// while authorising nothing — which is how the rules of one could be moved
	// wholesale into the other with only the anchors below noticing.
	for name, mine := range grants {
		if len(mine) == 0 {
			t.Errorf("%s grants nothing at all; an install creates it, binds it, and "+
				"binpack holds no permission in the namespace it is created in", name)
		}
	}

	// And an anchor per Role, because disjointness alone is satisfied by the
	// two swapping their rules wholesale — and one anchor alone by everything
	// moving into the anchored one.
	//
	// Read means get, list and watch together, for the reason
	// internal/collect's grantsRead gives: binpack's cache lists and then
	// watches, and `explain` gets, so a rule short of any of the three is a
	// rule that fails at runtime rather than a narrower version of the same
	// permission.
	grantsExactly(t, grants, "-autoscaler-status",
		// binpack reads exactly one kind in the namespace
		// discovery.autoscalerNamespace names: the cluster-autoscaler's status
		// ConfigMap, which ADR-0004 makes the whole basis of running without
		// cloud credentials.
		rbacdoc.CoreGroup+"/configmaps: get", rbacdoc.CoreGroup+"/configmaps: list", rbacdoc.CoreGroup+"/configmaps: watch")

	grantsExactly(t, grants, "-leader-election",
		// The Lease is held in binpack's own namespace, and client-go's
		// LeaseLock gets it, creates it and updates it (k8s.io/client-go
		// v0.36.4, tools/leaderelection/resourcelock/leaselock.go). Granted
		// anywhere else, a replica cannot acquire leadership and the process
		// never starts reconciling.
		"coordination.k8s.io/leases: get",
		"coordination.k8s.io/leases: create",
		"coordination.k8s.io/leases: update",
		// list, watch and patch are granted and not exercised. That is a
		// position docs/reference/rbac.md takes and defends — this Role is
		// namespaced to binpack's own namespace and nothing else there holds a
		// Lease — so they belong in the expected set rather than being read as
		// surplus. Narrowing the grant is a decision for that page to change
		// first.
		"coordination.k8s.io/leases: list",
		"coordination.k8s.io/leases: watch",
		"coordination.k8s.io/leases: patch",
		// And the events leader election announces itself with, which go to
		// the *core* group through client-go's legacy recorder — unlike
		// binpack's own decision events, which are events.k8s.io and
		// cluster-wide. They are about a Lease in this namespace, so they
		// belong to this Role: anchoring only the Lease verbs left the core
		// event rule free to move into the autoscaler-status Role, which is
		// nonempty, disjoint and in the wrong namespace.
		rbacdoc.CoreGroup+"/events: create",
		rbacdoc.CoreGroup+"/events: patch")
}

// grantsExactly asserts that the namespaced Role whose name carries the given
// suffix grants these pairs, only these, and that no other Role grants them.
//
// Three halves, and each fails differently. Present in the right Role is what
// the runtime needs. Absent from every other is what stops a rule drifting
// into a Role created in a namespace binpack never reads. And nothing beyond
// them is the direction the rest of this file had to learn twice: every other
// check asks whether a permission binpack needs is granted, and none of them
// has an opinion about one it does not — `delete` added to the ConfigMap rule
// in the chart and the reference together satisfied the per-identity
// comparison, disjointness and every minimum anchor at once.
func grantsExactly(t *testing.T, grants map[string]map[string]bool, suffix string, pairs ...string) {
	t.Helper()

	var role string
	for name := range grants {
		if strings.HasSuffix(name, suffix) {
			role = name
		}
	}
	if role == "" {
		t.Fatalf("the chart renders no namespaced Role named %q, so this asserts nothing "+
			"about where %v are granted", suffix, pairs)
	}

	want := map[string]bool{}
	for _, pair := range pairs {
		want[pair] = true

		if !grants[role][pair] {
			t.Errorf("%s does not grant %q, and it is the Role created in the namespace "+
				"binpack needs it in", role, pair)
		}
		for name, other := range grants {
			if name != role && other[pair] {
				t.Errorf("%s grants %q, and it is not the Role created in the namespace "+
					"binpack needs it in; the permission would be held somewhere binpack "+
					"never issues that request", name, pair)
			}
		}
	}

	for pair := range grants[role] {
		if !want[pair] {
			t.Errorf("%s grants %q, which binpack never issues in that namespace; a "+
				"permission nothing uses is one an operator has to justify, and this Role "+
				"is scoped to a namespace they may not own", role, pair)
		}
	}
}

// TestEveryRoleTheChartRendersIsBoundToItself is the step between granting a
// permission and holding it.
//
// A Role grants nothing until a binding names it, and a binding naming a role
// that does not exist installs cleanly — the API server does not resolve
// roleRef at admission, so the failure is a 403 at the first request rather
// than a rejected manifest. Every other assertion in this file is about what
// the Roles grant, and all of them stay true when a binding points somewhere
// else: the grant is still written, and nothing reaches it.
//
// Both halves of roleRef. The name is the one that drifts; the kind is the one
// that is silently wrong, since a RoleBinding may reference either a Role or a
// ClusterRole and `kind: ClusterRole` with a Role's name resolves to nothing.
func TestEveryRoleTheChartRendersIsBoundToItself(t *testing.T) {
	chart, err := os.ReadFile(rbacdoc.ChartPath)
	if err != nil {
		t.Fatalf("reading the chart's RBAC: %v", err)
	}
	roles, err := rbacdoc.Roles(string(chart))
	if err != nil {
		t.Fatalf("reading the chart's rules: %v", err)
	}
	bindings, err := rbacdoc.Bindings(string(chart))
	if err != nil {
		t.Fatalf("reading the chart's bindings: %v", err)
	}
	if len(bindings) != len(roles) {
		t.Fatalf("the chart renders %d roles and %d bindings; every role it creates has to "+
			"be bound or it grants nothing, and every binding has to name one",
			len(roles), len(bindings))
	}

	// C4: every object the chart renders has to be a distinct one. Duplicate
	// manifests keep the counts balanced, collapse into the same map entries
	// and dedupe through every grant comparison, and Helm then asks the API
	// server to create the same cluster-scoped object twice and the install
	// fails outright.
	identities := map[string]bool{}
	for _, role := range roles {
		identity(t, identities, role.Kind, role.Metadata.Namespace, role.Metadata.Name)

		// A ClusterRole is cluster-scoped and the API server refuses one
		// carrying a namespace. Nothing else here would notice: every rule it
		// holds is still written, still compared and still correct, and the
		// object is never created.
		if role.Kind == "ClusterRole" && role.Metadata.Namespace != "" {
			t.Errorf("the ClusterRole %s sets namespace %s; it is cluster-scoped, the API "+
				"server refuses the object, and the install comes up with none of the "+
				"cluster-wide reads binpack needs", role.Metadata.Name, role.Metadata.Namespace)
		}
	}

	bound := map[string]bool{}
	for _, b := range bindings {
		identity(t, identities, b.Kind, b.Metadata.Namespace, b.Metadata.Name)

		// A ClusterRoleBinding is cluster-scoped and the API server refuses one
		// carrying a namespace. Skipping the namespace check for anything that
		// is not a RoleBinding read a namespace on it as "nothing to check".
		if b.Kind == "ClusterRoleBinding" && b.Metadata.Namespace != "" {
			t.Errorf("the ClusterRoleBinding %s sets namespace %s; it is cluster-scoped, "+
				"the API server refuses the object, and the install comes up without any "+
				"cluster-wide permission", b.Metadata.Name, b.Metadata.Namespace)
		}
		bound[b.RoleRef.Kind+"/"+b.RoleRef.Name] = true

		// Kubernetes rejects a binding whose roleRef names another API group,
		// and nothing else here would notice: the kind and name would still be
		// the pair this test expects. Helm validates neither, and roleRef is
		// immutable, so the failure lands at install with no upgrade that
		// repairs it.
		if b.RoleRef.APIGroup != rbacdoc.RBACAPIGroup {
			t.Errorf("the %s %s has roleRef.apiGroup %q, and Kubernetes accepts only %q; "+
				"the API server refuses the object and the install is missing a binding",
				b.Kind, b.Metadata.Name, b.RoleRef.APIGroup, rbacdoc.RBACAPIGroup)
		}

		// And the binding's own kind against the role's. A RoleBinding naming
		// a ClusterRole is legal and installs cleanly, and grants that
		// ClusterRole's rules inside one namespace only — so the
		// ClusterRoleBinding turned into a RoleBinding leaves the counts
		// matching, the ClusterRole marked bound, and binpack without the
		// cluster-wide Node and Pod read this whole file is about.
		if want := rbacdoc.BindingKindFor(b.RoleRef.Kind); b.Kind != want {
			t.Errorf("the %s %s references a %s, which is made effective by a %s; as "+
				"written it grants %s's rules somewhere narrower than they are meant to "+
				"apply, or not at all", b.Kind, b.Metadata.Name, b.RoleRef.Kind, want,
				b.RoleRef.Name)
		}

		// A RoleBinding is only effective in its own namespace, so one in a
		// different namespace from the Role it names binds nothing.
		if b.Kind == "RoleBinding" {
			for _, role := range roles {
				if role.Metadata.Name == b.RoleRef.Name &&
					role.Metadata.Namespace != b.Metadata.Namespace {
					t.Errorf("the RoleBinding %s is created in %s and the Role it names is "+
						"created in %s; a RoleBinding grants nothing outside its own "+
						"namespace", b.Metadata.Name, b.Metadata.Namespace,
						role.Metadata.Namespace)
				}
			}
		}
	}

	for _, role := range roles {
		if !bound[role.Kind+"/"+role.Metadata.Name] {
			t.Errorf("the chart renders %s %s and no binding names it as roleRef; every "+
				"rule in it is granted to nobody, and the install comes up clean",
				role.Kind, role.Metadata.Name)
		}
	}

	// And who it grants to. A binding naming the right role and the wrong
	// principal is as inert as one naming the wrong role, and reads the same in
	// every assertion about what the role grants: the subject namespace pointed
	// at the autoscaler's is a valid binding for a ServiceAccount that does not
	// exist there, and binpack's own account holds nothing.
	//
	// The namespace is compared with the leader-election Role's rather than
	// against a literal, because that one is already pinned to the namespace
	// the deployment elects in — which is the release namespace, where the
	// ServiceAccount and the pod both are. Pinning it twice to the same
	// expression is what makes it a chain rather than two guesses.
	elections := rbacdoc.OfIdentity(roles, "Role-leader-election")
	if len(elections) != 1 {
		t.Fatalf("found %d leader-election Roles, want one; without it there is nothing to "+
			"compare the bindings' subject namespace against", len(elections))
	}
	release := elections[0].Metadata.Namespace
	account := deploymentServiceAccount(t)

	// And the account the chart creates, which this chain never reached: it
	// compared the bindings with the pod and stopped there, so renaming the
	// ServiceAccount manifest left both of those agreeing with each other
	// about a name nothing creates. A default install then has a suffixed
	// account, a Deployment naming the unsuffixed one, and pods that cannot
	// start.
	created, createdNamespace := renderedServiceAccount(t)
	if created != account {
		t.Errorf("the chart creates the ServiceAccount %s and the Deployment runs as %s; "+
			"one of them names an account that does not exist", created, account)
	}
	if createdNamespace != "" && createdNamespace != release {
		t.Errorf("the chart creates the ServiceAccount in %s and its bindings name it in "+
			"%s", createdNamespace, release)
	}

	for _, b := range bindings {
		// Per binding, not in aggregate. A total equal to the binding count is
		// satisfied by one binding with none and another with two, and the
		// binding with none grants its whole Role to nobody.
		if len(b.Subjects) != 1 {
			t.Errorf("the %s %s has %d subjects, want the one ServiceAccount binpack runs "+
				"as; with none it grants its Role to nobody, and this check would be "+
				"satisfied by another binding carrying the difference",
				b.Kind, b.Metadata.Name, len(b.Subjects))
		}

		for _, subject := range b.Subjects {
			if subject.Kind != "ServiceAccount" {
				t.Errorf("the %s %s binds a %s; binpack runs as a ServiceAccount and a "+
					"binding to anything else grants it nothing",
					b.Kind, b.Metadata.Name, subject.Kind)
			}
			// Kind-specific, and empty for a ServiceAccount because it is a
			// core object. A subject that acquires the RBAC group decodes
			// unchanged in every other field and the API server refuses the
			// binding.
			if want := rbacdoc.SubjectAPIGroup(subject.Kind); subject.APIGroup != want {
				t.Errorf("the %s %s binds a %s whose apiGroup is %q, and Kubernetes "+
					"requires %q for that kind; the API server refuses the object",
					b.Kind, b.Metadata.Name, subject.Kind, subject.APIGroup, want)
			}
			if subject.Namespace != release {
				t.Errorf("the %s %s binds a ServiceAccount in %s, and binpack's runs in "+
					"%s; the binding is valid and names an account that is not the one "+
					"the chart creates or the pod uses",
					b.Kind, b.Metadata.Name, subject.Namespace, release)
			}
			// And the account itself. The older raw-template check requires
			// the name expression to *contain* the helper, which a suffix
			// satisfies: `…serviceAccountName" . }}-other` on all three
			// subjects left the suite green while the Deployment went on
			// running as the unsuffixed account and held none of these
			// permissions. Compared with the Deployment's own expression, so
			// the two have to move together.
			if subject.Name != account {
				t.Errorf("the %s %s binds the ServiceAccount %s and the Deployment runs as "+
					"%s; the binding is valid, names an account that is not the pod's, and "+
					"grants binpack nothing", b.Kind, b.Metadata.Name, subject.Name, account)
			}
		}
	}
}

// identity records one object's kind, namespace and name, and fails if the
// chart has rendered that combination already.
func identity(t *testing.T, seen map[string]bool, kind, namespace, name string) {
	t.Helper()

	key := kind + " " + namespace + "/" + name
	if seen[key] {
		t.Errorf("the chart renders %s twice; the counts balance, every comparison here "+
			"dedupes it, and Helm asks the API server to create one object twice", key)
	}
	seen[key] = true
}

// placeholder reports whether a namespace token stands for one rather than
// being one.
//
// Three shapes, and no more: `<namespace>`, a shell variable, and the bare
// sentinel the pages already use. Anything else that is not a legal namespace
// is a typo, and reporting it is the whole point — an example nobody can run
// is worse than no example.
func placeholder(ns string) bool {
	switch {
	// An opening bracket is enough: `<that namespace>` is a placeholder with a
	// space in it, and the flag pattern above stops at the space, so the token
	// reaching here is `<that`. Requiring the closing bracket would report a
	// real placeholder as a typo.
	case strings.HasPrefix(ns, "<"):
		return true
	case strings.HasPrefix(ns, "$"):
		return true
	case ns == "NS":
		return true
	default:
		return false
	}
}

// joinContinuations folds a backslash-continued shell command onto the line it
// starts on, leaving the lines it consumed blank so the numbering still points
// at the source.
//
// The scan below looks at one line at a time and only at lines carrying
// `kubectl `, so a command written across two — `kubectl get deploy binpack \`
// and then `--namespace binpack` — had its flags on a line the scan never
// examined. The other examples kept the "did this check anything" counter
// above zero, so the wrong namespace stayed green and a reader copying it gets
// a NotFound.
func joinContinuations(lines []string) []string {
	out := make([]string, len(lines))
	for i := 0; i < len(lines); i++ {
		start, joined := i, lines[i]
		for i+1 < len(lines) {
			trimmed := strings.TrimRight(joined, " \t")
			if !strings.HasSuffix(trimmed, `\`) {
				break
			}
			joined = strings.TrimSuffix(trimmed, `\`) + " " + strings.TrimSpace(lines[i+1])
			i++
		}
		// The lines a command was folded from stay empty, so a failure still
		// reports the line the command starts on.
		out[start] = joined
	}
	return out
}

// renderedServiceAccount is the name and namespace expressions of the account
// the chart creates, rendered the way rbacdoc renders one.
func renderedServiceAccount(t *testing.T) (name, namespace string) {
	t.Helper()

	manifest, err := os.ReadFile("../../charts/binpack/templates/serviceaccount.yaml")
	if err != nil {
		t.Fatalf("reading the chart's ServiceAccount: %v", err)
	}

	var section string
	for line := range strings.SplitSeq(string(manifest), "\n") {
		if !strings.HasPrefix(line, " ") && strings.HasSuffix(line, ":") {
			section = strings.TrimSuffix(line, ":")
		}
		if section != "metadata" {
			continue
		}
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "name:"):
			name = rbacdoc.Placeholder(strings.TrimPrefix(trimmed, "name:"))
		case strings.HasPrefix(trimmed, "namespace:"):
			namespace = rbacdoc.Placeholder(strings.TrimPrefix(trimmed, "namespace:"))
		}
	}

	if name == "" {
		t.Fatal("the chart's ServiceAccount manifest names no account, so nothing here can " +
			"say whether the Deployment and the bindings agree with it")
	}
	return name, namespace
}

// deploymentServiceAccount is the account expression the pod runs as, rendered
// the way rbacdoc renders one.
func deploymentServiceAccount(t *testing.T) string {
	t.Helper()

	deployment, err := os.ReadFile("../../charts/binpack/templates/deployment.yaml")
	if err != nil {
		t.Fatalf("reading the chart's deployment: %v", err)
	}

	// The field, not the first line mentioning it. A comment carrying the old
	// expression above a changed field satisfied a textual search while the
	// rendered pod named an account the chart never creates — so a commented
	// line is skipped, and finding more than one real field is a failure
	// rather than a choice between them.
	const field = "serviceAccountName:"

	var found []string
	for line := range strings.SplitSeq(string(deployment), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		value, ok := strings.CutPrefix(trimmed, field)
		if !ok {
			continue
		}
		found = append(found, rbacdoc.Placeholder(value))
	}

	switch len(found) {
	case 1:
		return found[0]
	case 0:
		t.Fatalf("the Deployment no longer sets %s, so it runs as the namespace's default "+
			"account and every binding here grants binpack nothing", field)
	default:
		t.Fatalf("the Deployment sets %s %d times (%v); which account the pod runs as is "+
			"then a question about ordering, and this would answer it by position",
			field, len(found), found)
	}
	return ""
}

// TestRBACCreateFalseRendersNoRBACAtAll holds what the setting is for.
//
// docs/reference/rbac.md and the chart both offer rbac.create: false to an
// operator who manages RBAC themselves, and what it promises is that the chart
// adds nothing — they grant what they choose to grant. rbacdoc.Roles unions
// every branch, so a Role or binding moved outside that guard is invisible to
// every assertion about what the chart grants: it is still granted, now to
// everybody, and an operator who chose to manage RBAC elsewhere silently
// receives chart-managed permissions as well.
//
// The chart CI job renders with rbac.create=false and checks only that
// templating succeeds, which an object rendered outside the guard also does.
func TestRBACCreateFalseRendersNoRBACAtAll(t *testing.T) {
	chart, err := os.ReadFile(rbacdoc.ChartPath)
	if err != nil {
		t.Fatalf("reading the chart's RBAC: %v", err)
	}

	off, err := rbacdoc.Without(string(chart), ".Values.rbac.create")
	if err != nil {
		t.Fatalf("reading the chart without rbac.create: %v", err)
	}

	// Read from the rendered documents rather than from the readers refusing
	// them. Both do refuse an empty document, so `err != nil` would pass today
	// — and would go on passing if they ever stopped, since the assertion
	// would then be about a contract nothing here states.
	//
	// Decoded rather than matched as text: `kind: "Role"` is the same object
	// to Kubernetes and to every other reader here, and to a substring it is
	// not there at all.
	kinds, err := rbacdoc.Kinds(off)
	if err != nil {
		t.Fatalf("reading what rbac.create: false renders: %v", err)
	}
	for _, kind := range kinds {
		if strings.HasSuffix(kind, "Role") || strings.HasSuffix(kind, "RoleBinding") {
			t.Errorf("rbac.create: false still renders a %s; the chart promises to add "+
				"nothing, and an operator who chose to manage RBAC elsewhere would "+
				"receive it anyway — and their install fails if the object already "+
				"exists", kind)
		}
	}

	// And the ordinary install must still render them, or the guard has
	// swallowed the wrong block and this passes for the wrong reason.
	roles, err := rbacdoc.Roles(string(chart))
	if err != nil {
		t.Fatalf("the chart renders no roles with rbac.create on: %v", err)
	}
	if len(roles) < 3 {
		t.Errorf("the chart renders %d roles with rbac.create on, and it has always "+
			"rendered the ClusterRole and two namespaced Roles; this test would pass "+
			"against a chart that had lost them", len(roles))
	}
}

// TestTheLeaderElectionRoleIsWhereTheLeaseIsTaken is the leader-election half
// of the promise TestTheChartBindsTheAutoscalerStatusRoleWhereBinpackReads
// makes for the autoscaler's status.
//
// The two namespaced Roles are anchored to what each grants, and that says
// nothing about where either is created. binpack takes its Lease in the
// namespace the deployment names with --leader-election-namespace; move the
// Role and its binding anywhere else and both stay nonempty, disjoint and
// correctly anchored while the process cannot acquire leadership and never
// starts reconciling.
//
// The namespaces are Helm expressions, so this compares expressions rather
// than namespaces: rbacdoc renders each action to a scalar derived from its own
// text, which answers "are these two the same expression" and never "which
// namespace is it". That is the question here — the flag and the Role have to
// move together, whatever they move to.
func TestTheLeaderElectionRoleIsWhereTheLeaseIsTaken(t *testing.T) {
	chart, err := os.ReadFile(rbacdoc.ChartPath)
	if err != nil {
		t.Fatalf("reading the chart's RBAC: %v", err)
	}
	roles, err := rbacdoc.Roles(string(chart))
	if err != nil {
		t.Fatalf("reading the chart's rules: %v", err)
	}

	elections := rbacdoc.OfIdentity(roles, "Role-leader-election")
	if len(elections) != 1 {
		t.Fatalf("found %d leader-election Roles, want one; this asserts nothing without it",
			len(elections))
	}

	deployment, err := os.ReadFile("../../charts/binpack/templates/deployment.yaml")
	if err != nil {
		t.Fatalf("reading the chart's deployment: %v", err)
	}
	flag := leaderElectionNamespaceFlag(t, string(deployment))

	if got := elections[0].Metadata.Namespace; got != flag {
		t.Errorf("the leader-election Role is created in %s and the deployment takes its "+
			"Lease in %s; binpack would hold no permission where it elects, and a replica "+
			"could never acquire leadership", got, flag)
	}

	// And the two namespaced Roles must not be in the same place, or the
	// anchoring above is describing a distinction the chart no longer draws.
	status := rbacdoc.OfIdentity(roles, "Role-autoscaler-status")
	if len(status) == 1 && status[0].Metadata.Namespace == elections[0].Metadata.Namespace {
		t.Errorf("both namespaced Roles are created in %s; the autoscaler publishes into "+
			"the namespace it runs in and binpack elects in its own, so a chart that puts "+
			"them together is wrong about one of them", elections[0].Metadata.Namespace)
	}
}

// leaderElectionNamespaceFlag is the namespace expression the deployment passes
// to --leader-election-namespace, rendered the way rbacdoc renders one.
func leaderElectionNamespaceFlag(t *testing.T, deployment string) string {
	t.Helper()

	const flag = "--leader-election-namespace="
	return rbacdoc.Placeholder(theOnly(t, deployment, flag,
		"the deployment no longer passes "+flag+", so this cannot say where the Lease is "+
			"taken; if the flag has gone, so has the reason for the leader-election Role"))
}

// theOnly is the value following a prefix, from the one uncommented line that
// carries it.
//
// Commented lines are skipped and two matches are a failure, for the reason
// the ServiceAccount field has the same treatment: a comment retaining the old
// expression above a changed line satisfied a textual search while the
// rendered object used the other one. The two helpers had that fix applied to
// one of them.
func theOnly(t *testing.T, document, prefix, absent string) string {
	t.Helper()

	var found []string
	for line := range strings.SplitSeq(document, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if _, value, ok := strings.Cut(trimmed, prefix); ok {
			found = append(found, value)
		}
	}

	switch len(found) {
	case 1:
		return found[0]
	case 0:
		t.Fatal(absent)
	default:
		t.Fatalf("%q appears %d times (%v); which one takes effect is then a question "+
			"about ordering, and this would answer it by position", prefix, len(found), found)
	}
	return ""
}

// TestTheLeaderElectionGrantsGoWhenLeaderElectionDoes holds the offer
// docs/reference/rbac.md makes: that Leases are only for leader election and
// may be omitted by anyone running a single replica.
//
// rbacdoc.Roles is deliberately the union of every branch, so a guard removed
// from around these rules is invisible to every assertion about what they
// grant — they are still granted, just to everybody. An operator who set
// leaderElection.enabled=false to avoid the permission would hold it anyway,
// and the page would be wrong about a permission rather than about a default.
func TestTheLeaderElectionGrantsGoWhenLeaderElectionDoes(t *testing.T) {
	chart, err := os.ReadFile(rbacdoc.ChartPath)
	if err != nil {
		t.Fatalf("reading the chart's RBAC: %v", err)
	}

	off, err := rbacdoc.Without(string(chart), ".Values.leaderElection.enabled")
	if err != nil {
		t.Fatalf("reading the chart without leader election: %v", err)
	}
	without, err := rbacdoc.Roles(off)
	if err != nil {
		t.Fatalf("reading the rules an install without leader election renders: %v", err)
	}

	granted := rbacdoc.Grants(without)
	for pair := range granted {
		if strings.HasPrefix(pair, "coordination.k8s.io/leases: ") {
			t.Errorf("an install with leaderElection.enabled=false is still granted %q; "+
				"the reference tells an operator they may omit Leases entirely, and the "+
				"chart no longer lets them", pair)
		}
	}
	if len(rbacdoc.OfIdentity(without, "Role-leader-election")) != 0 {
		t.Error("an install with leaderElection.enabled=false still renders the " +
			"leader-election Role, so the guard no longer covers the whole object")
	}

	// And the rules must come back when it is on, or this passes by the guard
	// having swallowed the wrong block.
	on, err := rbacdoc.Roles(string(chart))
	if err != nil {
		t.Fatalf("reading the chart's rules: %v", err)
	}
	if len(rbacdoc.Grants(on)) <= len(granted) {
		t.Errorf("turning leader election off removed nothing (%d pairs either way), so "+
			"this test is not observing the guard it names", len(granted))
	}
}

// TestEachFeatureGuardStandsOnItsOwn holds a relationship the readers here
// deliberately flatten.
//
// rbacdoc.Roles unions every branch and rbacdoc.Without models a guard by
// deleting its block, so neither can say that two values must both be true.
// Nesting the act rules inside another feature's guard is invisible to both:
// the union still holds them, the difference between the two renders still
// reports them as gated on rbac.allowDraining, and an install that opted in to
// acting and out of the other feature renders neither `nodes: patch` nor
// `pods/eviction: create` and 403s on its first drain.
//
// So the nesting is read from the template rather than inferred from what the
// branches contain. Both features hang off rbac.create and off nothing else,
// which is what the reference promises: acting is a decision about acting, and
// leader election is a decision about replicas.
func TestEachFeatureGuardStandsOnItsOwn(t *testing.T) {
	chart, err := os.ReadFile(rbacdoc.ChartPath)
	if err != nil {
		t.Fatalf("reading the chart's RBAC: %v", err)
	}

	for _, feature := range []struct {
		guard    string
		within   []string
		contains []string
	}{
		// rbac.create holds the whole file, so the two features are inside it
		// by design. They hold rules and nothing else: a construct inside one
		// of them can suppress what it guards, which is the same harm as a
		// guard around it and is not visible from outside the block.
		{".Values.rbac.create", nil,
			[]string{".Values.rbac.allowDraining", ".Values.leaderElection.enabled"}},
		{".Values.rbac.allowDraining", []string{".Values.rbac.create"}, nil},
		{".Values.leaderElection.enabled", []string{".Values.rbac.create"}, nil},
	} {
		around, err := rbacdoc.GuardsAround(string(chart), feature.guard)
		if err != nil {
			t.Errorf("reading what encloses %s: %v", feature.guard, err)
			continue
		}
		if !slices.Equal(around, feature.within) {
			t.Errorf("the block gated on %s is nested inside %v, and it should be inside "+
				"%v; an install that set %s and cleared one of the others would render "+
				"none of its rules while every comparison went on reading them as granted",
				feature.guard, around, feature.within, feature.guard)
		}

		inside, err := rbacdoc.GuardsWithin(string(chart), feature.guard)
		if err != nil {
			t.Errorf("reading what %s contains: %v", feature.guard, err)
			continue
		}
		if !slices.Equal(inside, feature.contains) {
			t.Errorf("the block gated on %s contains %v, and it should contain %v; "+
				"whatever else is in there can suppress the rules this feature is "+
				"supposed to grant, and every comparison reads them as granted anyway",
				feature.guard, inside, feature.contains)
		}
	}
}

// TestTheChartAgreesWithItselfAboutItsOptions holds three joins between a
// template and the value that is supposed to control it.
//
// Each is a place where two halves of one decision are written in different
// files, and where the RBAC guards check one half and take the other on trust:
// the Role a feature needs is checked against the guard that renders it, and
// nothing checked that the process is told about the same guard, or that the
// helper both objects agree on still reads what it claims to.
func TestTheChartAgreesWithItselfAboutItsOptions(t *testing.T) {
	deployment, err := os.ReadFile("../../charts/binpack/templates/deployment.yaml")
	if err != nil {
		t.Fatalf("reading the chart's deployment: %v", err)
	}

	// The flag and the guard are one decision. Hard-coded, an install with
	// leaderElection.enabled=false renders no Lease permissions — which the
	// guard test above confirms — and starts a process that tries to take one
	// anyway, then 403s for as long as it runs.
	const flag = "--leader-election="
	value := theOnly(t, string(deployment), flag,
		"the deployment no longer passes "+flag+", so whether the process elects is "+
			"decided somewhere this cannot see")
	if got, want := rbacdoc.Placeholder(value), rbacdoc.Placeholder(".Values.leaderElection.enabled"); got != want {
		t.Errorf("the deployment passes %s%s and the RBAC is gated on "+
			".Values.leaderElection.enabled; one of them decides whether binpack elects "+
			"and the other decides whether it may", flag, strings.TrimSpace(value))
	}

	// The helper both autoscaler-status objects are held to has to read the
	// setting they are held to it *for*. Returning the default unconditionally
	// leaves them agreeing with each other, this pipeline check satisfied, and
	// the rendered config still telling binpack to read somewhere else.
	helpers, err := os.ReadFile("../../charts/binpack/templates/_helpers.tpl")
	if err != nil {
		t.Fatalf("reading the chart's helpers: %v", err)
	}
	body, ok := helperBody(string(helpers), "binpack.autoscalerNamespace")
	if !ok {
		t.Fatal("the chart no longer defines binpack.autoscalerNamespace, and both " +
			"autoscaler-status objects are checked against it by name")
	}
	for _, key := range []string{"discovery", "autoscalerNamespace", ".Values.config"} {
		if !strings.Contains(body, key) {
			t.Errorf("binpack.autoscalerNamespace does not read %s: the Role would be "+
				"created wherever the helper decides and binpack would read the namespace "+
				"config.discovery.autoscalerNamespace names, which is a 403 on every "+
				"evaluation", key)
		}
	}

	// And the ServiceAccount the chart creates is created only when the
	// operator asked it to. Rendered unguarded, an install naming an external
	// account gets a chart-managed one as well — which fails if it exists, and
	// which Helm owns and deletes on uninstall if it does not.
	account, err := os.ReadFile("../../charts/binpack/templates/serviceaccount.yaml")
	if err != nil {
		t.Fatalf("reading the chart's ServiceAccount: %v", err)
	}
	off, err := rbacdoc.Without(string(account), ".Values.serviceAccount.create")
	if err != nil {
		t.Fatalf("reading the ServiceAccount without its guard: %v", err)
	}
	if kinds, err := rbacdoc.Kinds(off); err != nil {
		t.Errorf("reading what serviceAccount.create: false renders: %v", err)
	} else if len(kinds) > 0 {
		t.Errorf("serviceAccount.create: false still renders %v; an operator who named an "+
			"external account receives a chart-managed one too", kinds)
	}
}

// helperBody is the body of one named Helm template.
func helperBody(helpers, name string) (string, bool) {
	_, rest, ok := strings.Cut(helpers, `{{- define "`+name+`" -}}`)
	if !ok {
		return "", false
	}
	body, _, ok := strings.Cut(rest, "{{- end ")
	return body, ok
}
