package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/motleyhand/binpack/internal/collect"
	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

const (
	poolID   = "da8977ba-244f"
	poolName = "pool-4g"
)

// recordedEvent is one call to the event recorder.
type recordedEvent struct {
	object         runtime.Object
	eventType      string
	reason, action string
	note           string
}

// fakeRecorder captures events instead of writing them, so a test can assert
// on what a cluster user would see in `kubectl describe node`.
type fakeRecorder struct{ events []recordedEvent }

func (r *fakeRecorder) Eventf(regarding, _ runtime.Object,
	eventType, reason, action, note string, args ...any,
) {
	r.events = append(r.events, recordedEvent{
		object: regarding, eventType: eventType,
		reason: reason, action: action,
		note: fmt.Sprintf(note, args...),
	})
}

// captured collects log lines so a test can assert the controller says
// something on every pass, which is how anyone knows it is alive.
type captured struct{ lines []string }

func (c *captured) logger() logr.Logger {
	return funcr.New(func(prefix, args string) {
		c.lines = append(c.lines, prefix+" "+args)
	}, funcr.Options{})
}

func (c *captured) contains(substr string) bool {
	for _, line := range c.lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func config() engine.Config {
	return engine.Config{
		NodeGroupIDLabel: "doks.digitalocean.com/node-pool-id",
		PoolNameLabel:    "doks.digitalocean.com/node-pool",
		Default: engine.Policy{
			Enabled: true,
			Sim:     engine.SimConfig{ExpendablePriorityCutoff: -10},
			Evict:   engine.DefaultEvictConfig(),
		},
	}
}

func inPool(name string) *corev1.Node {
	return mother.LargeNode(name, mother.InPool(poolName, poolID))
}

func statusConfigMap() client.Object {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: collect.StatusConfigMapNamespace,
			Name:      collect.StatusConfigMapName,
		},
		Data: map[string]string{"status": `
autoscalerStatus: Running
time: ` + time.Now().UTC().Format("2006-01-02 15:04:05.000000000 -0700 MST") + `
clusterWide:
  health:
    status: Healthy
    lastProbeTime: "` + time.Now().UTC().Format(time.RFC3339) + `"
  scaleDown:
    status: NoCandidates
nodeGroups:
- name: ` + poolID + `
  health:
    minSize: 1
    maxSize: 10
    cloudProviderTarget: 3
    nodeCounts:
      registered:
        ready: 3
`},
	}
}

func newEvaluator(t *testing.T, log *captured, rec *fakeRecorder, objs ...client.Object) *evaluator {
	t.Helper()
	return &evaluator{
		reader:   fake.NewClientBuilder().WithObjects(objs...).Build(),
		recorder: rec,
		opts:     Options{Engine: config(), DryRun: true, Interval: time.Minute, Once: true},
		log:      log.logger(),
		stop:     func() {},
	}
}

func TestEvaluateEmitsAnEventOnTheNodeItWouldDrain(t *testing.T) {
	// The event is the point: on a managed control plane, `kubectl describe
	// node` is the one surface a cluster user reliably has, and binpack's own
	// logs may be as unreachable as the autoscaler's.
	var log captured
	rec := &fakeRecorder{}
	ev := newEvaluator(t, &log, rec,
		inPool("a"), inPool("b"), inPool("c"),
		mother.Pod("default", "web", mother.OnNode("a")),
		statusConfigMap(),
	)

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(rec.events) != 1 {
		t.Fatalf("got %d events, want exactly one for the chosen node", len(rec.events))
	}
	got := rec.events[0]

	node, ok := got.object.(*corev1.Node)
	if !ok {
		t.Fatalf("event was recorded against %T, not the node", got.object)
	}
	if node.Name == "a" {
		t.Error("chose the only node running anything")
	}
	if got.reason != ReasonWouldDrain || got.action != ActionConsolidate {
		t.Errorf("reason/action = %s/%s, want %s/%s",
			got.reason, got.action, ReasonWouldDrain, ActionConsolidate)
	}
	if got.eventType != corev1.EventTypeNormal {
		t.Errorf("type = %s: a decision binpack made deliberately is not a warning", got.eventType)
	}
	// Nobody should have to read the code to find out whether it acted.
	if !strings.Contains(got.note, "dry run") {
		t.Errorf("the note does not say nothing was done: %q", got.note)
	}
}

func TestEvaluateSaysSomethingWhenThereIsNothingToDo(t *testing.T) {
	// A controller that goes quiet is indistinguishable from one that has
	// stopped, and this is the line that says otherwise.
	var log captured
	rec := &fakeRecorder{}
	ev := newEvaluator(t, &log, rec,
		inPool("a"),
		mother.Pod("default", "web", mother.OnNode("a")),
		statusConfigMap(),
	)

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(rec.events) != 0 {
		t.Errorf("got %d events with no decision to report", len(rec.events))
	}
	if !log.contains("nothing to do") {
		t.Errorf("said nothing at all:\n%s", strings.Join(log.lines, "\n"))
	}
}

func TestEvaluateReturnsAReadFailureRatherThanHidingIt(t *testing.T) {
	// A read that fails every tick is a broken deployment, bad RBAC most
	// likely. A controller that logs it and carries on looks healthy while
	// doing nothing at all.
	var log captured
	ev := newEvaluator(t, &log, &fakeRecorder{},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: collect.StatusConfigMapNamespace,
				Name:      collect.StatusConfigMapName,
			},
			Data: map[string]string{"status": "\tnot: [valid"},
		})

	err := ev.evaluate(context.Background())
	if err == nil {
		t.Fatal("an unreadable cluster was treated as an ordinary tick")
	}
	if !strings.Contains(err.Error(), "reading the cluster") {
		t.Errorf("error does not say what failed: %v", err)
	}
}

func TestOnceStopsTheManagerAfterASinglePass(t *testing.T) {
	var stopped bool
	var log captured
	ev := newEvaluator(t, &log, &fakeRecorder{}, inPool("a"), statusConfigMap())
	ev.stop = func() { stopped = true }

	if err := ev.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if !stopped {
		t.Error("one-shot mode left the manager running, so the process would never exit")
	}
}

func TestRunRefusesToPretendItCanAct(t *testing.T) {
	// Someone who sets dryRun: false has decided binpack should act. Running
	// anyway and logging "would drain" would leave them believing it is acting
	// for as long as it takes them to check a node.
	err := Run(context.Background(), Options{DryRun: false})

	if err == nil {
		t.Fatal("dryRun: false was accepted by a build with no executor")
	}
	for _, want := range []string{"dryRun", "not implemented"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}

func TestTheEvaluatorRunsBehindTheLease(t *testing.T) {
	// Everything binpack does that matters happens in the evaluator, so if it
	// ran outside leader election the lease would protect nothing.
	if !(&evaluator{}).NeedLeaderElection() {
		t.Error("the evaluator would run on every replica at once")
	}
}

func TestTheCacheDoesNotWatchEveryConfigMapInTheCluster(t *testing.T) {
	// binpack reads exactly one ConfigMap. An unrestricted cache would watch
	// and hold every ConfigMap in the cluster to serve that one Get — Helm
	// release data, certificate bundles, all of it, permanently, by a tool
	// whose purpose is to reduce what the cluster costs.
	opts := cacheOptions()

	byConfigMap, ok := opts.ByObject[&corev1.ConfigMap{}]
	if !ok {
		// Map keys are pointers, so look it up by type rather than identity.
		for obj, cfg := range opts.ByObject {
			if _, isConfigMap := obj.(*corev1.ConfigMap); isConfigMap {
				byConfigMap, ok = cfg, true
			}
		}
	}
	if !ok {
		t.Fatal("ConfigMaps are cached without restriction")
	}

	scoped, inKubeSystem := byConfigMap.Namespaces[collect.StatusConfigMapNamespace]
	if !inKubeSystem {
		t.Fatalf("the ConfigMap cache is not scoped to %s", collect.StatusConfigMapNamespace)
	}
	if len(byConfigMap.Namespaces) != 1 {
		t.Errorf("cached ConfigMaps in %d namespaces, want only the autoscaler's",
			len(byConfigMap.Namespaces))
	}
	if scoped.FieldSelector == nil ||
		!strings.Contains(scoped.FieldSelector.String(), collect.StatusConfigMapName) {
		t.Errorf("the cache is not narrowed to %s: %v",
			collect.StatusConfigMapName, scoped.FieldSelector)
	}
}

func TestManagedFieldsAreDroppedBeforeCaching(t *testing.T) {
	// Never read by binpack, and a substantial fraction of a pod's stored size
	// on a large cluster.
	for _, obj := range []client.Object{
		mother.Pod("default", "web"), mother.SmallNode("a"),
		&policyv1.PodDisruptionBudget{},
	} {
		obj.SetManagedFields([]metav1.ManagedFieldsEntry{{Manager: "kubectl"}})

		if _, err := dropManagedFields(obj); err != nil {
			t.Fatalf("dropManagedFields: %v", err)
		}
		if len(obj.GetManagedFields()) != 0 {
			t.Errorf("%T kept its managed fields", obj)
		}
	}
}

func TestOnceRunsWithoutServersOrLeaderElection(t *testing.T) {
	// None of them mean anything to a process about to exit, and a CronJob
	// contending for a lease would block rather than run.
	opts := managerOptions(Options{
		Once: true, LeaderElection: true,
		MetricsAddress: ":8080", ProbeAddress: ":8081",
	})

	if opts.LeaderElection {
		t.Error("one-shot mode would wait for a lease it does not need")
	}
	if opts.Metrics.BindAddress != "0" || opts.HealthProbeBindAddress != "0" {
		t.Errorf("one-shot mode binds servers nothing will ever reach: metrics=%q probes=%q",
			opts.Metrics.BindAddress, opts.HealthProbeBindAddress)
	}
}

func TestTheLeaseIsReleasedOnShutdown(t *testing.T) {
	// The failure leader election guards against — two binpacks draining at
	// once — is worst during a rolling update, when the old and new pods are
	// both alive. Holding the lease to expiry makes that window minutes long.
	opts := managerOptions(Options{LeaderElection: true, MetricsAddress: "0", ProbeAddress: "0"})

	if !opts.LeaderElectionReleaseOnCancel {
		t.Error("a rolling update would wait out the lease instead of handing over")
	}
	if opts.LeaderElectionID != LeaderElectionID {
		t.Errorf("lease ID = %q, want the documented %q", opts.LeaderElectionID, LeaderElectionID)
	}
}
