package collect_test

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/motleyhand/binpack/internal/collect"
	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

var now = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

// statusNamespace is where these tests put the autoscaler's status, and where
// they tell Snapshot to look. A literal rather than a constant borrowed from
// elsewhere: the point of every test below is that binpack reads the namespace
// it is given, so a fixture and a caller agreeing through a shared default
// would be agreeing about the wrong thing.
const statusNamespace = "kube-system"

// statusThere is that location as Snapshot takes it, with the name the
// autoscaler uses unless it was told otherwise.
var statusThere = collect.StatusRef{
	Namespace: statusNamespace,
	Name:      collect.StatusConfigMapName,
}

func statusConfigMap(document string) *corev1.ConfigMap {
	return statusConfigMapAt(statusThere, document)
}

func statusConfigMapIn(namespace, document string) *corev1.ConfigMap {
	return statusConfigMapAt(
		collect.StatusRef{Namespace: namespace, Name: collect.StatusConfigMapName}, document)
}

func statusConfigMapAt(at collect.StatusRef, document string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: at.Namespace,
			Name:      at.Name,
		},
		Data: map[string]string{"status": document},
	}
}

func reader(objects ...client.Object) collect.Reader {
	return fake.NewClientBuilder().WithObjects(objects...).Build()
}

func TestSnapshotReadsEveryKindTheEngineNeeds(t *testing.T) {
	r := reader(
		mother.SmallNode("a"),
		mother.LargeNode("b"),
		mother.Pod("default", "web", mother.OnNode("a")),
		mother.Pod("default", "api", mother.OnNode("b")),
		mother.PDB("default", "web", 1, map[string]string{"app": "web"}),
		statusConfigMap(observed),
	)

	s, err := collect.Snapshot(context.Background(), r, now, statusThere)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if len(s.Nodes) != 2 {
		t.Errorf("got %d nodes, want 2", len(s.Nodes))
	}
	if len(s.Pods) != 2 {
		t.Errorf("got %d pods, want 2", len(s.Pods))
	}
	if len(s.PDBs) != 1 {
		t.Errorf("got %d budgets, want 1", len(s.PDBs))
	}
	if !s.Now.Equal(now) {
		t.Errorf("Now = %s, want the clock it was given", s.Now)
	}
	if !s.Autoscaler.Running {
		t.Error("the autoscaler status was not read")
	}
}

func TestSnapshotReadsPodsInEveryNamespace(t *testing.T) {
	// A pod in any namespace occupies its node, so a read scoped to one would
	// under-count and approve a drain onto capacity that is not there.
	r := reader(
		mother.SmallNode("a"),
		mother.Pod("default", "web", mother.OnNode("a")),
		mother.Pod("kube-system", "coredns", mother.OnNode("a")),
		mother.Pod("monitoring", "prometheus", mother.OnNode("a")),
		mother.PDB("default", "one", 1, map[string]string{"app": "one"}),
		mother.PDB("kube-system", "two", 1, map[string]string{"app": "two"}),
		statusConfigMap(observed),
	)

	s, err := collect.Snapshot(context.Background(), r, now, statusThere)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if len(s.Pods) != 3 {
		t.Errorf("got %d pods, want all 3 namespaces", len(s.Pods))
	}
	if len(s.PDBs) != 2 {
		t.Errorf("got %d budgets, want both namespaces", len(s.PDBs))
	}
}

func TestSnapshotTreatsAMissingAutoscalerAsADiagnosisNotAFailure(t *testing.T) {
	// No status ConfigMap at all. binpack should be able to say "nothing here
	// will remove a drained node" clearly, rather than exiting with an error
	// that gives the reader nothing to act on.
	s, err := collect.Snapshot(context.Background(), reader(mother.SmallNode("a")), now, statusThere)
	if err != nil {
		t.Fatalf("a missing autoscaler should not be an error: %v", err)
	}

	if s.Autoscaler.Running {
		t.Error("reported a running autoscaler with no status ConfigMap")
	}
	if live, _, why := s.Autoscaler.Live(now); live || why == "" {
		t.Errorf("Live() = %v, %q; want not live, with a reason", live, why)
	}
	if len(s.Nodes) != 1 {
		t.Error("the rest of the snapshot was abandoned along with the autoscaler")
	}
}

func TestSnapshotTreatsAStatusConfigMapWithNoStatusKeyTheSameWay(t *testing.T) {
	// A ConfigMap of the right name carrying no status document tells binpack
	// nothing, and inventing a running autoscaler from it would defeat the one
	// check that stops a pointless drain.
	empty := statusConfigMap("")
	delete(empty.Data, "status")

	s, err := collect.Snapshot(context.Background(), reader(mother.SmallNode("a"), empty), now, statusThere)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if s.Autoscaler.Running {
		t.Error("reported a running autoscaler from a ConfigMap with no status")
	}
	// And it is told apart from the object not being there at all. Both mean
	// binpack will not act; only one of them is consistent with there being no
	// autoscaler, and the refusal has to be able to say which it saw.
	if !s.Autoscaler.StatusFound {
		t.Error("a ConfigMap binpack read and found empty is reported as one it never found")
	}
}

func TestSnapshotNamesTheObjectAndTheVersionForAPre130Status(t *testing.T) {
	// A cluster on cluster-autoscaler 1.29 or older is one binpack cannot work
	// with at all, and what the operator used to get was a YAML parser
	// complaining about line 4 of a document they did not write — naming
	// neither the component, nor the version, nor the object.
	_, err := collect.Snapshot(context.Background(),
		reader(statusConfigMap(legacy)), now, statusThere)

	if err == nil {
		t.Fatal("the pre-1.30 free-text status is not something binpack can read")
	}
	if !strings.Contains(err.Error(), "cluster-autoscaler 1.30") {
		t.Errorf("the error does not say what binpack needs: %v", err)
	}
	if !strings.Contains(err.Error(), statusThere.String()) {
		t.Errorf("the error does not say which object it read: %v", err)
	}
}

func TestSnapshotFailsLoudlyOnAnUnparseableStatus(t *testing.T) {
	// Distinct from a missing one. An autoscaler that is publishing something
	// binpack cannot read is a change in somebody else's schema, and guessing
	// at it is how a confidently wrong decision gets made.
	_, err := collect.Snapshot(context.Background(),
		reader(statusConfigMap("\tnot: [valid")), now, statusThere)

	if err == nil {
		t.Fatal("an unparseable autoscaler status was accepted")
	}
}

func TestPodsOnSelectsByNode(t *testing.T) {
	s, err := collect.Snapshot(context.Background(), reader(
		mother.SmallNode("a"),
		mother.SmallNode("b"),
		mother.Pod("default", "here", mother.OnNode("a")),
		mother.Pod("default", "there", mother.OnNode("b")),
		mother.Pod("default", "unscheduled"),
	), now, statusThere)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	on := collect.PodsOn(s, mother.SmallNode("a"))
	if len(on) != 1 || on[0].Name != "here" {
		t.Errorf("PodsOn(a) = %v, want just the pod bound to a", names(on))
	}
}

func names(pods []*corev1.Pod) []string {
	out := make([]string, len(pods))
	for i, p := range pods {
		out[i] = p.Name
	}
	return out
}

// Compile-time proof that a full client satisfies the read interface. The two
// implementations in production are a direct client and the manager's cache,
// and neither collect nor the engine can tell them apart — which is what makes
// `explain` a truthful preview of `run`.
var _ collect.Reader = client.Client(nil)

func template(image string) corev1.PodTemplateSpec {
	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": image}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app", Image: image}}},
	}
}

func TestSnapshotReadsATemplateForEveryOwningKind(t *testing.T) {
	// binpack places the pod a controller *would create*, so a kind whose
	// template goes unread is a kind whose pods binpack refuses to move. All
	// four are owners a pod can name.
	//
	// Written out rather than derived from [collect.TemplateSources], although
	// everything else that needs the set now derives from it. A test that
	// reads the declaration in order to check the declaration cannot fail:
	// drop a kind and it ranges over one fewer, agrees with itself, and passes.
	// So this stays a hand-written expectation, and it is what catches a row
	// whose apiVersion is wrong — the one field the chart cannot check, since
	// RBAC grants a group and this keys on a group *and version*.
	r := reader(
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "web-rs", UID: "rs-uid"},
			Spec:       appsv1.ReplicaSetSpec{Template: template("web")},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "db"},
			Spec:       appsv1.StatefulSetSpec{Template: template("db")},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "cilium"},
			Spec:       appsv1.DaemonSetSpec{Template: template("cilium")},
		},
		&batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "backfill"},
			Spec:       batchv1.JobSpec{Template: template("backfill")},
		},
		statusConfigMap(observed),
	)

	s, err := collect.Snapshot(context.Background(), r, now, statusThere)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	for _, want := range []struct {
		ref   engine.OwnerRef
		image string
	}{
		{engine.OwnerRef{Namespace: "default", APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-rs", UID: "rs-uid"}, "web"},
		{engine.OwnerRef{Namespace: "default", APIVersion: "apps/v1", Kind: "StatefulSet", Name: "db"}, "db"},
		{engine.OwnerRef{Namespace: "kube-system", APIVersion: "apps/v1", Kind: "DaemonSet", Name: "cilium"}, "cilium"},
		{engine.OwnerRef{Namespace: "default", APIVersion: "batch/v1", Kind: "Job", Name: "backfill"}, "backfill"},
	} {
		got, ok := s.Templates[want.ref]
		if !ok {
			t.Errorf("no template for %s %s/%s", want.ref.Kind, want.ref.Namespace, want.ref.Name)
			continue
		}
		if got.Spec.Containers[0].Image != want.image {
			t.Errorf("%s template holds %q, want %q",
				want.ref.Kind, got.Spec.Containers[0].Image, want.image)
		}
	}
	if len(s.Templates) != 4 {
		t.Errorf("got %d templates, want one per controller", len(s.Templates))
	}
}

func TestTemplatesAreKeyedByNamespaceSoNamesCanRepeat(t *testing.T) {
	// "web-rs" in two namespaces is two different workloads. Keying on name
	// alone would hand one namespace's pods the other's template — a silent
	// wrong answer of exactly the kind reading templates exists to prevent.
	// The key carries apiVersion and UID for the same reason: a custom
	// resource sharing a kind and name, or a controller recreated under one.
	r := reader(
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "staging", Name: "web-rs"},
			Spec:       appsv1.ReplicaSetSpec{Template: template("staging-image")},
		},
		&appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Namespace: "production", Name: "web-rs"},
			Spec:       appsv1.ReplicaSetSpec{Template: template("production-image")},
		},
		statusConfigMap(observed),
	)

	s, err := collect.Snapshot(context.Background(), r, now, statusThere)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	for _, ns := range []string{"staging", "production"} {
		ref := engine.OwnerRef{Namespace: ns, APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web-rs"}
		got, ok := s.Templates[ref]
		if !ok {
			t.Fatalf("no template for %s/web-rs", ns)
		}
		if want := ns + "-image"; got.Spec.Containers[0].Image != want {
			t.Errorf("%s got %q, want %q", ns, got.Spec.Containers[0].Image, want)
		}
	}
}

func TestSnapshotReadsTheStatusFromTheNamespaceItIsGiven(t *testing.T) {
	// The cluster-autoscaler writes its status into the namespace it was given
	// with --namespace, and the upstream Helm chart sets that to whatever
	// namespace you install it into. A binpack that only ever looked in
	// kube-system reported "no cluster-autoscaler is running" about a healthy
	// one running in `autoscaler` — and then refused to act on the strength of
	// it. A confident, blocking claim about a component that is demonstrably
	// fine is worse than saying nothing.
	r := reader(mother.SmallNode("a"), statusConfigMapIn("autoscaler", observed))

	s, err := collect.Snapshot(context.Background(), r, now,
		collect.StatusRef{Namespace: "autoscaler", Name: collect.StatusConfigMapName})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if !s.Autoscaler.Running {
		t.Error("the autoscaler's own status says Running and binpack did not read it")
	}
	if len(s.Autoscaler.Groups) != 1 {
		t.Errorf("got %d node groups, want the one the status publishes",
			len(s.Autoscaler.Groups))
	}
}

func TestSnapshotReadsNoNamespaceButTheOneItIsGiven(t *testing.T) {
	// Why binpack is told where to look rather than searching every namespace
	// for the name: a cluster can hold more than one of these, and the one
	// this was found on did — a years-old status ConfigMap left in `default`
	// beside the live one in `autoscaler`.
	//
	// So the two documents here disagree, and binpack is pointed at the one
	// that is worse news. An implementation that searched and kept whichever
	// answer looked healthiest would pass every other test in this file and
	// fail this one, which is the whole point of writing it that way round.
	r := reader(mother.SmallNode("a"),
		statusConfigMapIn("default", "autoscalerStatus: Unhealthy\n"),
		statusConfigMapIn("autoscaler", observed))

	s, err := collect.Snapshot(context.Background(), r, now,
		collect.StatusRef{Namespace: "default", Name: collect.StatusConfigMapName})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if s.Autoscaler.Running {
		t.Error("read a status ConfigMap from a namespace binpack was not pointed at")
	}
}

func TestSnapshotRefusesToLookNowhereAtAll(t *testing.T) {
	// An empty namespace or name is a caller that lost the configured value,
	// not a cluster without an autoscaler — but a Get for a namespaced object
	// with no namespace simply finds nothing, and a name selector matching
	// nothing finds nothing too, so both are indistinguishable downstream.
	// That is exactly the bug these parameters exist to fix, so it fails here
	// rather than being reported as a healthy autoscaler's absence.
	for _, tc := range []struct {
		name string
		at   collect.StatusRef
		want string
	}{
		{"no namespace", collect.StatusRef{Name: collect.StatusConfigMapName}, "namespace"},
		{"no name", collect.StatusRef{Namespace: statusNamespace}, "name"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := collect.Snapshot(context.Background(),
				reader(statusConfigMap(observed)), now, tc.at)

			if err == nil {
				t.Fatalf("%+v was read as a cluster with no autoscaler", tc.at)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not say what is missing: %v", err)
			}
		})
	}
}

func TestAutoscalerHonoursConfiguredLocation(t *testing.T) {
	// Both halves of where the status lives are autoscaler flags: --namespace
	// and --status-config-map-name. binpack reads the namespace it is told to
	// read; the name was a constant, so an autoscaler that renamed its status
	// object was reported as no autoscaler at all — a claim about the
	// operator's cluster that binpack had not established, on a cluster where
	// it would otherwise have worked.
	r := reader(
		mother.SmallNode("a"),
		statusConfigMapAt(collect.StatusRef{Namespace: "cluster-autoscaler", Name: "my-ca-status"}, observed),
	)

	s, err := collect.Snapshot(context.Background(), r, now,
		collect.StatusRef{Namespace: "cluster-autoscaler", Name: "my-ca-status"})
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if !s.Autoscaler.Running {
		t.Error("binpack looked where it was told and found a running autoscaler; it should say so")
	}
}
