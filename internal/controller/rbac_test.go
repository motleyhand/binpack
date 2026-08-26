package controller

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/motleyhand/binpack/internal/collect"
	"github.com/motleyhand/binpack/internal/executor"
	"github.com/motleyhand/binpack/internal/mother"
	"github.com/motleyhand/binpack/internal/rbacdoc"
)

// TestTheChartGrantsWhatTheCodeCalls closes the corner the other three RBAC
// tests assume rather than check.
//
// Those three hold the chart and the reference page to each other, and the
// comment on one of them says it "checks the third side of an agreement the
// chart and the code already keep between them" — an agreement nothing kept.
// Both documents are hand-maintained against the code, so a verb removed from
// both stays consistent and wrong, and a verb added to the code and to neither
// is consistent and missing. Either way the suite is green and the symptom is
// the one those tests already describe: binpack holds its lease, serves its
// metrics, publishes decisions, and 403s on its first node patch.
//
// So this drives binpack's own writes — every one of them, the executor's four
// and the Event create that is the executor package doc's stated exception —
// against a writer that records what an RBAC rule would have to grant, and
// compares that with the chart. To them it adds the verbs client-go's event
// recorder issues on the long-running path, which are binpack's writes for
// RBAC's purposes even though no interface here bounds them.
//
// Both directions, because they fail differently. A recorded pair the chart
// does not grant is a write nobody has been given permission for. An act-gated
// pair the chart grants and nothing exercised is a permission that has outlived
// its caller, or — far more likely — a caller this test has stopped driving,
// at which point the first direction is checking a shrunken set and passing
// for it.
//
// It still does not bound the *process*, and no Go test can: the manager
// writes a Lease and announces itself on the core events API for leader
// election, and nothing in Go narrows that. internal/executor's package doc
// says so and docs/reference/rbac.md is the authority. What changed with the
// recorder is only which side of that line it sits on — bounding a library's
// writes and asking whether the chart permits the ones it is observed to make
// are different questions, and the second one has an answer.
func TestTheChartGrantsWhatTheCodeCalls(t *testing.T) {
	chart, err := os.ReadFile(rbacdoc.ChartPath)
	if err != nil {
		t.Fatalf("reading the chart's RBAC: %v", err)
	}

	roles, err := rbacdoc.Roles(string(chart))
	if err != nil {
		t.Fatalf("reading the chart's rules: %v", err)
	}
	// A count rather than a non-empty check, in chart_test.go's shape. The
	// chart renders thirty-six pairs; a reader that found a handful would
	// satisfy the comparisons below without checking anything, and that is the
	// failure this whole commit is about.
	if all := rbacdoc.Grants(roles); len(all) < 30 {
		t.Fatalf("the chart parsed to %d group/resource: verb pairs, which is fewer than "+
			"it has ever rendered — the reader has stopped seeing rules, and every "+
			"assertion below would pass on the remainder", len(all))
	}

	// Cluster-scoped, because every write binpack's own code makes is. Nodes
	// are cluster-scoped objects; eviction is namespaced but binpack evicts
	// from whichever namespaces the chosen node hosts; and the decision event
	// is filed under `default`, where `kubectl describe node` looks for it,
	// which is not the namespace binpack runs in and not the one the
	// autoscaler-status Role is scoped to.
	//
	// The pair alone cannot say that. Flattened across kinds, moving the
	// events rule out of the ClusterRole and into the namespaced Role beside
	// the ConfigMap grant left every comparison here green, and reporting
	// would 403 on a namespace that Role does not cover.
	clusterWide := rbacdoc.Grants(rbacdoc.OfKind(roles, "ClusterRole"))

	// What an install that has not opted in renders. The union above is the
	// right reading of "what could this chart ever grant" and the wrong one of
	// "what does a default install grant" — and dryRun defaults to true, so
	// the decision event is written by installs that render none of the act
	// rules. Checked against the union, moving its grant inside the opt-in
	// guard is invisible: the pair is still in `granted`, and the reverse
	// check below still finds it exercised.
	off, err := rbacdoc.Without(string(chart), ".Values.rbac.allowDraining")
	if err != nil {
		t.Fatalf("reading the chart without its act rules: %v", err)
	}
	ungatedRoles, err := rbacdoc.Roles(off)
	if err != nil {
		t.Fatalf("reading the chart's ungated rules: %v", err)
	}
	ungated := rbacdoc.Grants(rbacdoc.OfKind(ungatedRoles, "ClusterRole"))

	// The act grants are the difference between the two renders rather than
	// the guarded block read on its own. A block lifted out of its document
	// has no kind to carry, so reading it directly is the one way to obtain
	// these pairs that cannot tell a ClusterRole rule from a namespaced one.
	gated := map[string]bool{}
	for pair := range clusterWide {
		if !ungated[pair] {
			gated[pair] = true
		}
	}
	if len(gated) == 0 {
		t.Fatal("opting in to rbac.allowDraining adds no cluster-scoped rule, so this " +
			"asserts nothing about what acting takes")
	}

	always, acting := drivenWrites(t)

	for pair := range always {
		if !ungated[pair] {
			t.Errorf("binpack performs %q whatever dryRun is set to, and no cluster-scoped "+
				"rule the chart renders without rbac.allowDraining grants it; a default "+
				"install would 403 on that call while looking healthy", pair)
		}
	}

	// And nothing else. Every check above asks whether a permission binpack
	// needs is granted; none asks whether a permission granted is one binpack
	// needs, and those fail in opposite directions. Adding `delete` to the
	// Node read rule in the chart and the reference together left the two
	// agreeing, left internal/collect's read equality unchanged — the rule
	// still carries get, list and watch, so it still "grants read" — and was
	// never reached by the acting equality, while the service account gained
	// cluster-wide permission to delete Nodes. "binpack removes no object,
	// ever" is internal/executor's package doc, R4-003's availability argument
	// and this page's own "what binpack is never granted" section, and the
	// grant is where it is actually enforced.
	for pair := range ungated {
		if !always[pair] && !ungatedReads()[pair] {
			t.Errorf("the chart grants %q to every install and binpack neither reads nor "+
				"writes it; a permission nothing uses is one an operator has to justify, "+
				"and the reference tells them binpack is never granted it", pair)
		}
	}

	// An equality, not a subset. `acting ⊆ gated` says a mutating write stays
	// behind the opt-in — `nodes: patch` moved one line above the guard is
	// still granted, so a subset against the union accepted it, and a default
	// install would hold a verb that can cordon a node while the reference
	// page promises it holds none. `gated ⊆ acting` says the opt-in grants
	// nothing that has outlived its caller, and stops the first direction
	// quietly comparing a set this test has stopped driving.
	for pair := range acting {
		if !gated[pair] {
			t.Errorf("binpack performs %q only when it is acting, and the chart grants it "+
				"without rbac.allowDraining (or not at all). An install that has not "+
				"opted in must hold no verb that can cordon a node or evict a pod, "+
				"whatever its configuration says — docs/reference/rbac.md promises "+
				"exactly that", pair)
		}
	}
	for pair := range gated {
		if !acting[pair] {
			t.Errorf("the chart grants %q to let binpack act and nothing here exercises it. "+
				"Either the grant has outlived its caller, or this test has stopped "+
				"driving one — in which case the check above is comparing a shrunken set",
				pair)
		}
	}
}

// drivenWrites runs every write binpack's own code makes, returning what each
// would need granted — split by whether an install has to opt in to reach it.
//
// Every call site, named once. The executor's four writes plus the reporter's
// Event create are the whole of it — that is internal/executor's package doc's
// claim, and internal/controller's eventWriter method count is what stops a
// sixth appearing here unnoticed.
//
// The split is a fact about the controller rather than about the calls: an
// evaluation reports its decision on the node whatever dryRun says — that is
// what dry run is *for*, and evaluate does it on the frozen-drain path too —
// while advance, which is where every executor write is reached, is only ever
// entered when binpack is acting. It is the same line the chart draws with
// rbac.allowDraining, which is why the two can be compared at all.
func drivenWrites(t *testing.T) (always, acting map[string]bool) {
	t.Helper()

	ctx := context.Background()
	node := mother.SmallNode("node-a")
	pod := mother.Pod("default", "web", mother.OnNode(node.Name))

	always = writesOf(t, "the decision event",
		func(w *recordingWriter) error {
			r := directReporter{writer: w, instance: "test",
				now: func() time.Time { return time.Unix(0, 0) }}
			return r.emit(ctx, node, "Reason", "Action", "a note")
		})

	// The other reporter, which is the one an ordinary install runs. It hands
	// events to client-go's broadcaster, and no interface here bounds what
	// that writes — but an install still has to be permitted to make those
	// requests, and this test is where the chart is asked whether it is.
	//
	// The verbs come from recorderMethods, which the broadcaster test in
	// reporter_test.go proves by driving the real recorder and observing its
	// sink. Named there rather than repeated here: a fact one test can prove
	// and another needs should be written down once, and only the test that
	// drives the recorder can prove it. Dropping events.k8s.io/events: patch
	// from the chart and the reference together left the two agreeing while
	// every decision after the first failed to aggregate.
	for _, method := range recorderMethods {
		always["events.k8s.io/events: "+strings.ToLower(method)] = true
	}

	driven := map[string]func(w *recordingWriter) error{
		"Cordon":   func(w *recordingWriter) error { return executor.Cordon(ctx, w, node) },
		"Annotate": func(w *recordingWriter) error { return executor.Annotate(ctx, w, node, map[string]string{"a": "b"}) },
		"HandBack": func(w *recordingWriter) error {
			return executor.HandBack(ctx, w, node,
				map[string]string{"l": ""}, map[string]string{"a": ""})
		},
		"Evict": func(w *recordingWriter) error { return executor.Evict(ctx, w, pod) },
	}
	requireEveryWriteIsDriven(t, driven)

	calls := make([]func(*recordingWriter) error, 0, len(driven))
	for _, call := range driven {
		calls = append(calls, call)
	}
	acting = writesOf(t, "the executor's writes", calls...)
	requireEverySubresourceIsReached(t, acting)

	return always, acting
}

// executorSource is where internal/executor's package doc says every write to
// a Node or a Pod lives.
const executorSource = "../executor/executor.go"

// requireEveryWriteIsDriven holds the list above to the file it is a list of.
//
// It was hand-maintained, which is the shape this whole pull request is about:
// a new acting path calling Writer.Patch on another resource leaves the
// interface unchanged, the recorder satisfied and `acting` exactly as it was,
// so every RBAC comparison passes while that path 403s. The list is checked
// against the exported functions executor.go declares that take a Writer —
// which is what its package doc promises the file enumerates, so the promise
// and the list now fail together or not at all.
func requireEveryWriteIsDriven(t *testing.T, driven map[string]func(*recordingWriter) error) {
	t.Helper()

	source, err := parser.ParseFile(token.NewFileSet(), executorSource, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", executorSource, err)
	}

	declared := map[string]bool{}
	for _, decl := range source.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || !fn.Name.IsExported() {
			continue
		}
		for _, param := range fn.Type.Params.List {
			if ident, ok := param.Type.(*ast.Ident); ok && ident.Name == "Writer" {
				declared[fn.Name.Name] = true
			}
		}
	}
	if len(declared) == 0 {
		t.Fatalf("no exported function in %s takes a Writer; either the writes moved or "+
			"this parse has stopped seeing them, and either way nothing below is checked",
			executorSource)
	}

	for name := range declared {
		if driven[name] == nil {
			t.Errorf("%s declares %s, which takes a Writer and so performs a write, and "+
				"nothing here drives it; the chart is never asked whether it grants what "+
				"that call needs", executorSource, name)
		}
	}
	for name := range driven {
		if !declared[name] {
			t.Errorf("this drives %s and %s no longer declares it; the list has outlived "+
				"the file it is a list of", name, executorSource)
		}
	}
}

// requireEverySubresourceIsReached holds the writes *inside* those functions,
// which the audit above cannot.
//
// Driving each entry point once proves the entry points are all driven and
// nothing about what each one does: a conditional
// `w.SubResource("status").Patch(…)` added inside Cordon leaves the exported
// signatures unchanged, is never reached by a fixture that does not take that
// branch, and leaves `acting` exactly as it was while the production path
// 403s.
//
// Subresources specifically, because they are the part of an RBAC pair that
// static text can answer: the resource comes from the object's type and the
// verb from the method, but a subresource is a string literal at the call
// site. Every one executor.go names has to appear in what the recorder saw.
func requireEverySubresourceIsReached(t *testing.T, recorded map[string]bool) {
	t.Helper()

	source, err := parser.ParseFile(token.NewFileSet(), executorSource, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", executorSource, err)
	}

	// The method as well as the subresource. Keyed on the name alone, a
	// `SubResource("eviction").Patch(…)` added behind an unexercised branch was
	// reported as reached by the existing `pods/eviction: create` — a different
	// verb on the same subresource is a different RBAC pair, and the chart was
	// never asked for it.
	named := map[string]string{}
	ast.Inspect(source, func(n ast.Node) bool {
		outer, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		method, ok := outer.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		inner, ok := method.X.(*ast.CallExpr)
		if !ok || len(inner.Args) != 1 {
			return true
		}
		if fn, ok := inner.Fun.(*ast.SelectorExpr); !ok || fn.Sel.Name != "SubResource" {
			return true
		}

		lit, ok := inner.Args[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			t.Errorf("%s calls SubResource with something other than a literal, so this "+
				"cannot say which subresource it writes", executorSource)
			return true
		}
		if value, err := strconv.Unquote(lit.Value); err == nil {
			named[value+": "+strings.ToLower(method.Sel.Name)] = method.Sel.Name
		}
		return true
	})

	if len(named) == 0 {
		t.Fatalf("no SubResource call found in %s; either the eviction write moved or this "+
			"parse has stopped seeing it", executorSource)
	}

	requireEveryWriteIsUnconditional(t, source)

	for call, method := range named {
		var reached bool
		for pair := range recorded {
			if strings.HasSuffix(pair, "/"+call) {
				reached = true
			}
		}
		if !reached {
			t.Errorf("%s calls %s on a subresource and nothing here reaches it (%q); the "+
				"chart is never asked whether it grants that verb, and the branch holding "+
				"the call 403s in production", executorSource, method, call)
		}
	}
}

// writesOf drives calls against one recorder and returns what they would need
// granted. A call only errors here when the recorder cannot name the object it
// was handed, and its message says which type that was.
func writesOf(t *testing.T, what string, calls ...func(*recordingWriter) error) map[string]bool {
	t.Helper()

	w := &recordingWriter{pairs: map[string]bool{}}
	for i, call := range calls {
		if err := call(w); err != nil {
			t.Fatalf("%s, call %d: %v", what, i+1, err)
		}
	}
	return w.pairs
}

// recordingWriter answers binpack's writes by recording what each would need
// granted, in the `group/resource: verb` spelling [rbacdoc.Grants] produces.
//
// It records at the client boundary and derives the resource from the object
// through the scheme, rather than being told per call site what each call is
// for. A recorder carrying its own description of Cordon and Evict would be a
// second implementation of the executor's call shapes, agreeing with the chart
// about a set of writes neither is any longer a reading of.
//
// It satisfies executor.Writer and controller.eventWriter, which is what makes
// the boundary the real one: a verb added to either interface has to be
// answered here before this compiles.
type recordingWriter struct{ pairs map[string]bool }

func (w *recordingWriter) Patch(
	_ context.Context, obj client.Object, _ client.Patch, _ ...client.PatchOption,
) error {
	return w.record(obj, "", "patch")
}

func (w *recordingWriter) Create(
	_ context.Context, obj client.Object, _ ...client.CreateOption,
) error {
	return w.record(obj, "", "create")
}

func (w *recordingWriter) SubResource(name string) client.SubResourceClient {
	return recordingSubResource{w, name}
}

// record names the rule one call would need, deriving group and resource from
// the object's own kind.
//
// UnsafeGuessKindToResource is upstream's own kind-to-resource guess and is
// how a client without a discovery document names a resource. "Unsafe" is
// about irregular plurals in third-party APIs; every kind binpack writes is a
// core one.
func (w *recordingWriter) record(obj client.Object, subresource, verb string) error {
	kinds, _, err := scheme.Scheme.ObjectKinds(obj)
	if err != nil || len(kinds) == 0 {
		return fmt.Errorf("binpack writes a %T the scheme does not recognise, so this test "+
			"cannot say what granting it would take: %w", obj, err)
	}

	gvr, _ := meta.UnsafeGuessKindToResource(kinds[0])
	resource := gvr.Resource
	if subresource != "" {
		resource += "/" + subresource
	}
	group := kinds[0].Group
	if group == "" {
		group = rbacdoc.CoreGroup
	}

	w.pairs[group+"/"+resource+": "+verb] = true
	return nil
}

// recordingSubResource records the subresource half, which is where eviction
// lives: a create on pods/eviction rather than a delete on the pod.
type recordingSubResource struct {
	w    *recordingWriter
	name string
}

func (s recordingSubResource) Create(
	_ context.Context, obj client.Object, _ client.Object, _ ...client.SubResourceCreateOption,
) error {
	return s.w.record(obj, s.name, "create")
}

func (s recordingSubResource) Get(
	_ context.Context, obj client.Object, _ client.Object, _ ...client.SubResourceGetOption,
) error {
	return s.w.record(obj, s.name, "get")
}

func (s recordingSubResource) Update(
	_ context.Context, obj client.Object, _ ...client.SubResourceUpdateOption,
) error {
	return s.w.record(obj, s.name, "update")
}

func (s recordingSubResource) Patch(
	_ context.Context, obj client.Object, _ client.Patch, _ ...client.SubResourcePatchOption,
) error {
	return s.w.record(obj, s.name, "patch")
}

func (s recordingSubResource) Apply(
	_ context.Context, _ runtime.ApplyConfiguration, _ ...client.SubResourceApplyOption,
) error {
	return fmt.Errorf("binpack applies nothing, and a server-side apply is a patch this " +
		"test has stopped naming")
}

// The two interfaces this has to satisfy for the recording to be at the
// boundary rather than beside it.
var (
	_ executor.Writer = (*recordingWriter)(nil)
	_ eventWriter     = (*recordingWriter)(nil)
)

// ungatedReads is every read binpack performs, as the pairs a rule granting it
// would carry.
//
// Derived from the same declaration internal/collect's read equality derives
// from, so the two cannot drift: the controller kinds come from
// collect.TemplateSources, and the three the snapshot reads directly are
// written out for the reason that test writes them out — this has to name them
// in order to say the ClusterRole holds nothing else, and naming them is what
// makes the assertion cover the whole role rather than the API groups the
// controller kinds happen to occupy today.
//
// get, list and watch together, and no other verb. binpack's cache lists and
// then watches and `explain` gets, so those three are what a read costs; a
// fourth verb on a read rule is not a wider read, it is a different
// permission.
func ungatedReads() map[string]bool {
	resources := []string{
		rbacdoc.CoreGroup + "/nodes", rbacdoc.CoreGroup + "/pods",
		"policy/poddisruptionbudgets",
	}
	for _, src := range collect.TemplateSources() {
		group := src.Group()
		if group == "" {
			group = rbacdoc.CoreGroup
		}
		resources = append(resources, group+"/"+src.Resource)
	}

	out := map[string]bool{}
	for _, resource := range resources {
		for _, verb := range []string{"get", "list", "watch"} {
			out[resource+": "+verb] = true
		}
	}
	return out
}

// requireEveryWriteIsUnconditional holds the property that makes driving each
// entry point once sufficient.
//
// The audits above find the writes a subresource literal names, and a direct
// `w.Patch(…)` on another resource names nothing static text can read: the
// resource comes from the object's type. So the guard is structural instead —
// every Writer call in executor.go runs on its function's unconditional path,
// which is what makes "drove the function" and "reached the write" the same
// statement.
//
// A write that genuinely needs a branch is not forbidden; it has to become its
// own function, which the exported-function audit then requires to be driven.
// That is also what internal/executor's package doc already promises the file
// is: an enumeration of the writes, one per name.
func requireEveryWriteIsUnconditional(t *testing.T, source *ast.File) {
	t.Helper()

	writerNames = map[string]bool{}
	for _, decl := range source.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		for _, param := range fn.Type.Params.List {
			if ident, ok := param.Type.(*ast.Ident); !ok || ident.Name != "Writer" {
				continue
			}
			for _, name := range param.Names {
				writerNames[name.Name] = true
			}
		}
	}
	if len(writerNames) == 0 {
		t.Fatalf("no function in %s takes a Writer, so this cannot recognise a write",
			executorSource)
	}

	var found int
	var walk func(n ast.Node, conditional bool)
	walk = func(n ast.Node, conditional bool) {
		if n == nil {
			return
		}

		switch n := n.(type) {
		case *ast.IfStmt:
			// The init runs whatever the condition decides — `if err :=
			// w.Patch(…); err != nil` always makes the call — so only the
			// bodies are conditional.
			walk(n.Init, conditional)
			walk(n.Cond, conditional)
			walk(n.Body, true)
			walk(n.Else, true)
			return
		case *ast.ForStmt:
			walk(n.Init, conditional)
			walk(n.Cond, conditional)
			walk(n.Body, true)
			return
		case *ast.RangeStmt:
			walk(n.X, conditional)
			walk(n.Body, true)
			return
		case *ast.SwitchStmt:
			walk(n.Init, conditional)
			walk(n.Tag, conditional)
			walk(n.Body, true)
			return
		case *ast.TypeSwitchStmt, *ast.SelectStmt:
			for _, child := range children(n) {
				walk(child, true)
			}
			return
		case *ast.BinaryExpr:
			// `&&` and `||` do not evaluate their right operand when the left
			// decides the answer, so a write there is as conditional as one in
			// a branch body — `if annotated && w.Patch(…) != nil` reaches the
			// patch only for annotated nodes. The condition of an if was
			// walked with its caller's state, which read the whole of it as
			// always evaluated.
			if n.Op == token.LAND || n.Op == token.LOR {
				walk(n.X, conditional)
				walk(n.Y, true)
				return
			}
		case *ast.CallExpr:
			if writesThroughWriter(n) {
				found++
				if conditional {
					t.Errorf("%s makes a write inside a conditional; driving the function "+
						"it is in does not reach it, so the chart is never asked whether "+
						"it grants what that call needs — give the write its own function, "+
						"which the audit above then requires to be driven", executorSource)
				}
			}
		}

		// A block's statements are not independent: once a preceding one can
		// return, everything after it is reached only when that branch was not
		// taken. `if skip { return nil }` followed by a write made the write
		// look unconditional while a fixture taking the early return never
		// reached it.
		if block, ok := n.(*ast.BlockStmt); ok {
			mayHaveReturned := conditional
			for _, statement := range block.List {
				walk(statement, mayHaveReturned)
				if !mayHaveReturned && terminates(statement) {
					mayHaveReturned = true
				}
			}
			return
		}

		for _, child := range children(n) {
			walk(child, conditional)
		}
	}
	walk(source, false)

	if found == 0 {
		t.Fatalf("no Writer call found in %s; either the writes moved or this parse has "+
			"stopped seeing them", executorSource)
	}
}

// terminates reports whether a statement can end the function early.
//
// Conservatively: anything containing a return or a panic. A branch that can
// leave makes everything after it conditional, whether or not it does.
func terminates(statement ast.Stmt) bool {
	var leaves bool
	ast.Inspect(statement, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ReturnStmt:
			leaves = true
		case *ast.CallExpr:
			if fn, ok := n.Fun.(*ast.Ident); ok && fn.Name == "panic" {
				leaves = true
			}
		}
		return !leaves
	})
	return leaves
}

// children is one node's immediate children, in source order.
func children(n ast.Node) []ast.Node {
	var out []ast.Node
	first := true
	ast.Inspect(n, func(child ast.Node) bool {
		if first {
			first = false
			return true
		}
		if child != nil {
			out = append(out, child)
		}
		return false
	})
	return out
}

// writerNames are the parameter names the Writer is bound to in executor.go,
// read from the signatures rather than assumed.
var writerNames map[string]bool

// writesThroughWriter reports whether a call is one made on a Writer.
func writesThroughWriter(call *ast.CallExpr) bool {
	fn, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch fn.Sel.Name {
	case "Patch", "Create", "Update":
	default:
		return false
	}
	// Either `<writer>.Patch(…)` or `<writer>.SubResource("x").Create(…)`, and
	// the receiver is found by what it *is* rather than what it is called: the
	// spelling `w` is this file's convention and not a rule, so a new entry
	// point taking `writer Writer` slipped past with its conditional write
	// unexamined.
	if ident, ok := fn.X.(*ast.Ident); ok {
		return writerNames[ident.Name]
	}
	inner, ok := fn.X.(*ast.CallExpr)
	if !ok {
		return false
	}
	sub, ok := inner.Fun.(*ast.SelectorExpr)
	return ok && sub.Sel.Name == "SubResource"
}
