package collect_test

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/motleyhand/binpack/internal/collect"
	"github.com/motleyhand/binpack/internal/mother"
)

var now = time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)

func statusConfigMap(document string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: collect.StatusConfigMapNamespace,
			Name:      collect.StatusConfigMapName,
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

	s, err := collect.Snapshot(context.Background(), r, now)
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

	s, err := collect.Snapshot(context.Background(), r, now)
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
	s, err := collect.Snapshot(context.Background(), reader(mother.SmallNode("a")), now)
	if err != nil {
		t.Fatalf("a missing autoscaler should not be an error: %v", err)
	}

	if s.Autoscaler.Running {
		t.Error("reported a running autoscaler with no status ConfigMap")
	}
	if live, why := s.Autoscaler.Live(now); live || why == "" {
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

	s, err := collect.Snapshot(context.Background(), reader(mother.SmallNode("a"), empty), now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if s.Autoscaler.Running {
		t.Error("reported a running autoscaler from a ConfigMap with no status")
	}
}

func TestSnapshotFailsLoudlyOnAnUnparseableStatus(t *testing.T) {
	// Distinct from a missing one. An autoscaler that is publishing something
	// binpack cannot read is a change in somebody else's schema, and guessing
	// at it is how a confidently wrong decision gets made.
	_, err := collect.Snapshot(context.Background(),
		reader(statusConfigMap("\tnot: [valid")), now)

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
	), now)
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
