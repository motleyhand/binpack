package controller

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
// compares that with the chart.
//
// Both directions, because they fail differently. A recorded pair the chart
// does not grant is a write nobody has been given permission for. An act-gated
// pair the chart grants and nothing exercised is a permission that has outlived
// its caller, or — far more likely — a caller this test has stopped driving,
// at which point the first direction is checking a shrunken set and passing
// for it.
//
// It does not bound the *process*, and no Go test can. client-go's event
// recorder writes on the long-running path and the manager writes a Lease and
// core Events for leader election; internal/executor's package doc says so and
// docs/reference/rbac.md is the authority. What this holds is binpack's own
// code, which is what that doc's enumeration is a claim about.
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

	acting = writesOf(t, "the executor's writes",
		func(w *recordingWriter) error { return executor.Cordon(ctx, w, node) },
		func(w *recordingWriter) error {
			return executor.Annotate(ctx, w, node, map[string]string{"a": "b"})
		},
		func(w *recordingWriter) error {
			return executor.HandBack(ctx, w, node,
				map[string]string{"l": ""}, map[string]string{"a": ""})
		},
		func(w *recordingWriter) error { return executor.Evict(ctx, w, pod) },
	)

	return always, acting
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
		group = "core"
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
