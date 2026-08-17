package controller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/go-logr/logr/funcr"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

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
//
// Note what it cannot show: whether the write ever reached the API server. The
// real recorder returns before it posts, which is why --once needs a different
// reporter and why this fake reported success while a live run wrote nothing.
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

func inPool(name string, opts ...mother.NodeOption) *corev1.Node {
	return mother.LargeNode(name, append([]mother.NodeOption{
		mother.InPool(poolName, poolID)}, opts...)...)
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
	c := fake.NewClientBuilder().WithObjects(objs...).Build()
	return &evaluator{
		reader:   c,
		writer:   c,
		reporter: broadcastReporter{recorder: rec},
		opts: Options{Engine: withDrainBounds(config()), DryRun: true,
			Interval: time.Minute, Once: true},
		log:  log.logger(),
		stop: func() {},
	}
}

// withDrainBounds gives the policy the timeouts a real one is defaulted to.
// Zero means "any elapsed time is too long", so a config without them makes
// every resumed drain abandon on its first evaluation.
func withDrainBounds(c engine.Config) engine.Config {
	c.Default.StallTimeout = 10 * time.Minute
	c.Default.RemovalTimeout = 15 * time.Minute
	return c
}

func TestTheEventSaysWhichModeBinpackIsIn(t *testing.T) {
	// The event is the one surface a cluster user reliably has on a managed
	// control plane. One saying "No action taken — dry run" while pods are
	// being evicted is worse than no event at all: it tells them the opposite
	// of what is happening.
	for _, tc := range []struct {
		name     string
		dryRun   bool
		reason   string
		mentions string
		forbids  string
	}{
		{"deciding only", true, ReasonWouldDrain, "dry run", "is draining"},
		{"acting", false, ReasonDraining, "is draining", "No action taken"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log captured
			rec := &fakeRecorder{}
			ev := newEvaluator(t, &log, rec,
				inPool("a"), inPool("b"), inPool("c"),
				mother.Pod("default", "web", mother.OnNode("a")),
				statusConfigMap(),
			)
			ev.opts.DryRun = tc.dryRun

			if err := ev.evaluate(context.Background()); err != nil {
				t.Fatalf("evaluate: %v", err)
			}
			if len(rec.events) == 0 {
				t.Fatal("no event was recorded for the chosen node")
			}

			got := rec.events[0]
			if got.reason != tc.reason {
				t.Errorf("reason = %s, want %s", got.reason, tc.reason)
			}
			if !strings.Contains(got.note, tc.mentions) {
				t.Errorf("note does not mention %q: %s", tc.mentions, got.note)
			}
			if strings.Contains(got.note, tc.forbids) {
				t.Errorf("note claims %q while in the other mode: %s", tc.forbids, got.note)
			}
		})
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

func TestDryRunChangesNothingAtAll(t *testing.T) {
	// The guarantee the whole 0.1 line is sold on, asserted rather than
	// argued. It decides everything and writes nothing: no cordon, no marker,
	// nothing but an event saying what it would have done.
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

	var nodes corev1.NodeList
	if err := ev.reader.(client.Client).List(context.Background(), &nodes); err != nil {
		t.Fatalf("listing nodes: %v", err)
	}
	for _, n := range nodes.Items {
		if n.Spec.Unschedulable {
			t.Errorf("node %s was cordoned in dry run", n.Name)
		}
		for key := range n.Annotations {
			if strings.HasPrefix(key, "binpack.motleyhand.com/") {
				t.Errorf("node %s was annotated %s in dry run", n.Name, key)
			}
		}
	}
	// And it did decide: an assertion that nothing happened is satisfied by a
	// controller that did nothing at all.
	if len(rec.events) != 1 {
		t.Errorf("got %d events, want one saying what it would have done", len(rec.events))
	}
}

func TestADrainInProgressPreemptsANewDecision(t *testing.T) {
	// Step 0 of the protocol. A drain can legitimately outlast many intervals,
	// and without this every one of those intervals would be free to cordon a
	// second node — "one node per run" quietly assumed a run was short.
	var log captured
	rec := &fakeRecorder{}
	draining := inPool("a", mother.Cordoned(), mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainStarted: time.Now().Add(-time.Minute).Format(time.RFC3339),
	}))
	ev := newEvaluator(t, &log, rec, draining, inPool("b"), inPool("c"), statusConfigMap())

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if len(rec.events) != 0 {
		t.Errorf("a new decision was reported while a drain was running: %+v", rec.events)
	}
	for _, name := range []string{"b", "c"} {
		var n corev1.Node
		if err := ev.reader.(client.Client).Get(
			context.Background(), client.ObjectKey{Name: name}, &n); err != nil {
			t.Fatalf("reading node %s: %v", name, err)
		}
		if n.Spec.Unschedulable {
			t.Errorf("node %s was cordoned while a drain was already in progress", name)
		}
	}
}

func TestACompletedDrainIsCounted(t *testing.T) {
	// A successful drain is precisely the case where the evidence disappears:
	// the autoscaler removes the node and the markers go with it. Without
	// remembering the node, the completed counter would only ever increment
	// for drains that failed to finish.
	var log captured
	rec := &fakeRecorder{}
	draining := inPool("a", mother.Cordoned(), mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainStarted: time.Now().Add(-time.Minute).Format(time.RFC3339),
	}))
	ev := newEvaluator(t, &log, rec, draining, inPool("b"), inPool("c"), statusConfigMap())
	ev.opts.DryRun = false

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ev.active != "a" {
		t.Fatalf("setup: the drain was not picked up, active = %q", ev.active)
	}

	// The autoscaler removes it, taking the annotations with it.
	c := ev.reader.(client.Client)
	if err := c.Delete(context.Background(), draining); err != nil {
		t.Fatalf("deleting node a: %v", err)
	}

	before := completedDrains(t)
	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if got := completedDrains(t) - before; got != 1 {
		t.Errorf("the completed drain was counted %v times, want once", got)
	}
	if ev.active != "" {
		t.Errorf("the finished drain is still remembered as %q", ev.active)
	}

	// And it says so where somebody will find it. The event outlives the node,
	// which is the point: it is the record that the node binpack drained is
	// the node that disappeared.
	if !recorded(rec, ReasonDrained) {
		t.Errorf("no %s event was written; the only record is a log line and a counter",
			ReasonDrained)
	}
}

func TestAnAbandonedDrainSaysSoOnTheNode(t *testing.T) {
	// The sentence explaining what stopped it is the whole value here, and a
	// log line is the one place a cluster user on a managed control plane
	// cannot reach.
	var log captured
	rec := &fakeRecorder{}
	// A drain marked long ago with no progress since: stalled, so the first
	// evaluation that picks it up abandons it.
	stalled := inPool("a", mother.Cordoned(), mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainStarted:  time.Now().Add(-time.Hour).Format(time.RFC3339),
		engine.AnnotationDrainProgress: time.Now().Add(-time.Hour).Format(time.RFC3339),
	}))
	ev := newEvaluator(t, &log, rec, stalled,
		mother.Pod("default", "stuck", mother.OnNode("a")),
		inPool("b"), inPool("c"), statusConfigMap())
	ev.opts.DryRun = false

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if !recorded(rec, ReasonDrainAbandoned) {
		t.Errorf("no %s event was written for a drain that gave up", ReasonDrainAbandoned)
	}
}

func recorded(rec *fakeRecorder, reason string) bool {
	for _, e := range rec.events {
		if e.reason == reason {
			return true
		}
	}
	return false
}

func TestAMarkerClearedBySomebodyElseIsNotBinpacksDrain(t *testing.T) {
	// The node is still there but nobody marked it. Continuing would be acting
	// on a drain binpack no longer has a record of starting.
	var log captured
	rec := &fakeRecorder{}
	draining := inPool("a", mother.Cordoned(), mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainStarted: time.Now().Add(-time.Minute).Format(time.RFC3339),
	}))
	ev := newEvaluator(t, &log, rec, draining, inPool("b"), statusConfigMap())
	ev.opts.DryRun = false

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	c := ev.reader.(client.Client)
	var live corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: "a"}, &live); err != nil {
		t.Fatalf("reading node a: %v", err)
	}
	live.Annotations = nil
	if err := c.Update(context.Background(), &live); err != nil {
		t.Fatalf("clearing the marker: %v", err)
	}

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	// It is free to decide afresh — and on this cluster it does. What must not
	// happen is binpack going on advancing a drain of a node nobody marked.
	if ev.active == "a" {
		t.Error("still advancing a drain of a node whose marker was cleared")
	}
}

func TestAnUnmarkedNodeIsForgottenRatherThanRemembered(t *testing.T) {
	// A name kept after the marker went would later read a node deleted for
	// some unrelated reason — a pool resize, a manual kubectl delete — as a
	// drain binpack completed.
	var log captured
	ev := newEvaluator(t, &log, &fakeRecorder{}, inPool("a"), inPool("b"), statusConfigMap())
	ev.active = "a"

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if ev.active != "" {
		t.Errorf("still remembering a drain of an unmarked node: %q", ev.active)
	}
}

func TestDryRunLeavesADrainInProgressAlone(t *testing.T) {
	// Reachable: dryRun can be switched on while a drain is running. The node
	// is cordoned and binpack has been told to change nothing, and uncordoning
	// would itself be a change — so saying so is the only honest option.
	var log captured
	rec := &fakeRecorder{}
	draining := inPool("a", mother.Cordoned(), mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainStarted: time.Now().Add(-time.Hour).Format(time.RFC3339),
	}))
	ev := newEvaluator(t, &log, rec, draining, inPool("b"), statusConfigMap())

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	var n corev1.Node
	if err := ev.reader.(client.Client).Get(
		context.Background(), client.ObjectKey{Name: "a"}, &n); err != nil {
		t.Fatalf("reading node a: %v", err)
	}
	if !n.Spec.Unschedulable || n.Annotations[engine.AnnotationDrainStarted] == "" {
		t.Error("dry run changed the node it was told to leave alone")
	}
	if !log.contains("dryRun") {
		t.Error("nothing in the log says why the drain is not moving")
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
	if ev.err != nil {
		t.Errorf("a successful pass recorded an error: %v", ev.err)
	}
}

func TestOnceCarriesItsFailurePastTheManager(t *testing.T) {
	// The manager discards a runnable's error once the stop sequence is
	// engaged, and engaging it is exactly what a one-shot run does. Returning
	// the error from Start therefore loses it — logged as "received after stop
	// sequence was engaged" — and the process exits 0 having failed. Found by
	// running --once against a real cluster with a deliberately broken config
	// and watching it succeed.
	var log captured
	ev := newEvaluator(t, &log, &fakeRecorder{}, inPool("a"), statusConfigMap())
	ev.opts.Engine.ByPool = map[string]engine.Policy{"poool-4g": {Enabled: false}}

	if err := ev.Start(context.Background()); err != nil {
		t.Fatalf("Start should not return the error, the manager would drop it: %v", err)
	}
	if ev.err == nil {
		t.Fatal("the failure was not recorded, so Run would report success")
	}
	if !strings.Contains(ev.err.Error(), "poool-4g") {
		t.Errorf("recorded error does not say what failed: %v", ev.err)
	}
}

func TestActingIsOptInButPermitted(t *testing.T) {
	// dryRun: false used to be refused outright, because there was no executor
	// behind it. Now it is the mode an operator chooses, so the refusal must
	// be gone — and the default must still be the one that changes nothing.
	err := Run(context.Background(), Options{DryRun: false})

	if err != nil && strings.Contains(err.Error(), "not implemented") {
		t.Errorf("dryRun: false is still refused: %v", err)
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

func TestOnceWritesItsEventBeforeTheProcessExits(t *testing.T) {
	// The defect a live run found and no fake recorder could: the real
	// recorder returns before it posts, so a --once process was gone before
	// the write happened and its CronJob reported nothing at all. A CronJob's
	// pod logs disappear with it, which makes the event the *only* durable
	// surface that mode has.
	objects := []client.Object{
		inPool("a"), inPool("b"), inPool("c"),
		mother.Pod("default", "web", mother.OnNode("a")),
		statusConfigMap(),
	}
	c := fake.NewClientBuilder().WithObjects(objects...).Build()

	var log captured
	ev := &evaluator{
		reader: c,
		reporter: directReporter{
			writer:   c,
			instance: "binpack-test",
			now:      func() time.Time { return time.Unix(0, 0).UTC() },
		},
		opts: Options{Engine: config(), DryRun: true, Once: true},
		log:  log.logger(),
		stop: func() {},
	}

	if err := ev.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	var written eventsv1.EventList
	if err := c.List(context.Background(), &written); err != nil {
		t.Fatalf("listing events: %v", err)
	}
	if len(written.Items) != 1 {
		t.Fatalf("got %d events written, want exactly one", len(written.Items))
	}

	got := written.Items[0]
	if got.Regarding.Kind != "Node" || got.Regarding.Name == "" {
		t.Errorf("event does not point at a node: %+v", got.Regarding)
	}
	if got.Regarding.Name == "a" {
		t.Error("chose the only node running anything")
	}
	// Cluster-scoped objects have no namespace to inherit, and `kubectl
	// describe node` looks in default. Filing it anywhere else hides it.
	if got.Namespace != metav1.NamespaceDefault {
		t.Errorf("namespace = %q, want %q", got.Namespace, metav1.NamespaceDefault)
	}
	if got.ReportingController != reportingController {
		t.Errorf("reportingController = %q, want %q", got.ReportingController, reportingController)
	}
	if got.Reason != ReasonWouldDrain || got.Action != ActionConsolidate {
		t.Errorf("reason/action = %s/%s, want %s/%s",
			got.Reason, got.Action, ReasonWouldDrain, ActionConsolidate)
	}
	if !strings.Contains(got.Note, "dry run") {
		t.Errorf("the note does not say nothing was done: %q", got.Note)
	}
	if got.EventTime.IsZero() {
		t.Error("no event time, which the API requires")
	}
	// The fake client accepts names the API server rejects, so the rule is
	// asserted here rather than discovered in a cluster. A generateName of
	// "<node>." looks reasonable and is refused: the API server validates the
	// prefix itself as a subdomain, and that ends in a dot.
	if problems := validation.IsDNS1123Subdomain(got.Name); len(problems) > 0 {
		t.Errorf("event name %q would be rejected: %v", got.Name, problems)
	}
	if got.GenerateName != "" {
		t.Errorf("generateName %q: the API server validates it as a subdomain in its own right",
			got.GenerateName)
	}
}

func TestTheReporterMatchesTheExecutionModel(t *testing.T) {
	// The choice is not a preference. A long-running process wants the
	// recorder's aggregation; a process about to exit needs the write to have
	// happened. Picking the wrong one is silent in both directions.
	c := fake.NewClientBuilder().Build()

	if _, ok := reporterForClient(Options{Once: true}, c, nil).(directReporter); !ok {
		t.Error("one-shot mode uses the asynchronous reporter, and would report nothing")
	}
	if _, ok := reporterForClient(Options{Once: false}, c, &fakeRecorder{}).(broadcastReporter); !ok {
		t.Error("the controller writes an event per tick instead of aggregating")
	}
}

func TestReportingInstanceFitsWhatTheAPIAccepts(t *testing.T) {
	// Capped at 128 characters, and a rejected event is a decision nobody
	// hears about.
	if got := reportingInstance(); len(got) > 128 || got == "" {
		t.Errorf("reportingInstance() = %q (%d chars)", got, len(got))
	}
}

func TestEvaluateRefusesAnOverrideNamingAPoolThatIsNotThere(t *testing.T) {
	// The dangerous shape is `enabled: false` on a misspelt name: the operator
	// believes a pool is switched off, the override is unreachable, and its
	// nodes quietly take the default enabled policy. `explain` and `diagnose`
	// already refuse it, and of the three this is the command that will
	// eventually act on it.
	cfg := config()
	cfg.ByPool = map[string]engine.Policy{"poool-4g": {Enabled: false}}

	var log captured
	ev := newEvaluator(t, &log, &fakeRecorder{}, inPool("a"), statusConfigMap())
	ev.opts.Engine = cfg

	err := ev.evaluate(context.Background())
	if err == nil {
		t.Fatal("a typo'd pool override was accepted, and its nodes stay drainable")
	}
	if !strings.Contains(err.Error(), "poool-4g") {
		t.Errorf("error does not name the unknown pool: %v", err)
	}
}

func TestEvaluateAcceptsAnOverrideNamingAPoolThatExists(t *testing.T) {
	cfg := config()
	cfg.ByPool = map[string]engine.Policy{poolName: {Enabled: false}}

	var log captured
	ev := newEvaluator(t, &log, &fakeRecorder{}, inPool("a"), statusConfigMap())
	ev.opts.Engine = cfg

	if err := ev.evaluate(context.Background()); err != nil {
		t.Errorf("a real pool was rejected: %v", err)
	}
}

// failingReporter stands in for missing Events RBAC, or any write the API
// server refuses.
type failingReporter struct{}

func (failingReporter) emit(context.Context, *corev1.Node, string, string, string) error {
	return errors.New("events is forbidden: User cannot create resource events")
}

func TestOnceFailsWhenItCannotReport(t *testing.T) {
	// Nothing will retry: the process is exiting and its logs go with it. A
	// CronJob that exits 0 having reported nothing is indistinguishable from
	// one that found nothing to report.
	var log captured
	ev := newEvaluator(t, &log, &fakeRecorder{},
		inPool("a"), inPool("b"), inPool("c"),
		mother.Pod("default", "web", mother.OnNode("a")),
		statusConfigMap())
	ev.reporter = failingReporter{}
	ev.opts.Once = true

	err := ev.evaluate(context.Background())
	if err == nil {
		t.Fatal("a one-shot run reported nothing and called it success")
	}
	if !strings.Contains(err.Error(), "recording the decision") {
		t.Errorf("error does not say what was lost: %v", err)
	}
}

func TestTheControllerKeepsGoingWhenItCannotReport(t *testing.T) {
	// The opposite call, for the opposite reason: the next tick re-decides and
	// re-reports, and on a cluster where events are the only reachable surface
	// a crash loop would take the logs away too.
	var log captured
	ev := newEvaluator(t, &log, &fakeRecorder{},
		inPool("a"), inPool("b"), inPool("c"),
		mother.Pod("default", "web", mother.OnNode("a")),
		statusConfigMap())
	ev.reporter = failingReporter{}
	ev.opts.Once = false

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("the controller stopped over one lost event: %v", err)
	}
	if !log.contains("could not record the decision") {
		t.Errorf("the lost event was swallowed entirely:\n%s", strings.Join(log.lines, "\n"))
	}
}

func TestRunReportsAOneShotFailureTheManagerCannotCarry(t *testing.T) {
	// Run cannot be exercised without a cluster, so the rule it applies is
	// named and tested here instead. Getting it wrong is silent: binpack
	// fails, says so in its log, and exits 0.
	evaluation := errors.New("configuration overrides pools that do not exist")

	if got := outcome(nil, evaluation); !errors.Is(got, evaluation) {
		t.Errorf("outcome(nil, err) = %v, want the evaluation's failure", got)
	}
	if got := outcome(nil, nil); got != nil {
		t.Errorf("outcome(nil, nil) = %v, want success", got)
	}

	// A manager failure wins: if it could not start, whatever the evaluation
	// recorded is a consequence rather than the cause.
	managerErr := errors.New("no such host")
	got := outcome(managerErr, evaluation)
	if !errors.Is(got, managerErr) {
		t.Errorf("outcome(managerErr, _) = %v, want the manager's failure", got)
	}
	if !strings.Contains(got.Error(), "running") {
		t.Errorf("the manager's failure is unlabelled: %v", got)
	}
}

func TestControllersAreTrimmedBeforeCaching(t *testing.T) {
	// A cluster keeps ten ReplicaSet revisions per Deployment by default and
	// binpack reads one field of each. Their status and annotations — which
	// include the last-applied-configuration of the whole workload — are pure
	// memory cost in a tool whose purpose is to reduce what a cluster costs.
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "default",
			Name:        "web-rs",
			Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{...}"},
		},
		Spec: appsv1.ReplicaSetSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
			},
		},
		Status: appsv1.ReplicaSetStatus{Replicas: 3, ReadyReplicas: 3},
	}

	out, err := keepTemplateOnly(rs)
	if err != nil {
		t.Fatalf("keepTemplateOnly: %v", err)
	}
	trimmed := out.(*appsv1.ReplicaSet)

	if len(trimmed.Spec.Template.Spec.Containers) != 1 {
		t.Error("the template was dropped, which is the one thing binpack reads")
	}
	if trimmed.Status.Replicas != 0 {
		t.Error("status is still cached")
	}
	if trimmed.Annotations != nil {
		t.Error("annotations are still cached, including last-applied-configuration")
	}
}

// completedDrains reads the counter through the same registry a scrape would.
// A direct handle would pass against a series nobody publishes; this cannot.
func completedDrains(t *testing.T) float64 {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "binpack_drains_completed_total" {
			continue
		}
		return f.GetMetric()[0].GetCounter().GetValue()
	}
	t.Fatal("binpack_drains_completed_total is not published at all")
	return 0
}

func TestTheLoggedCountAgreesWithTheReasonBesideIt(t *testing.T) {
	// Seen in a live cluster: `"reason":"2 node(s) considered, none whose
	// workload fits elsewhere","nodesConsidered":4`. One line, one word, two
	// counts — the field counted every node looked at, the sentence counted
	// only those that reached a simulation. Nodes ruled out beforehand were
	// never candidates, so the sentence had it right.
	var log captured
	// Two nodes binpack will not consider — one annotated, one cordoned by
	// somebody else — beside two it will.
	skipped := inPool("skip-me", mother.NodeAnnotations(
		map[string]string{engine.AnnotationSkip: "true"}))
	ev := newEvaluator(t, &log, &fakeRecorder{},
		skipped, inPool("cordoned", mother.Cordoned()), inPool("a"), inPool("b"),
		// Enough on both candidates that neither can take the other's pods.
		mother.Pod("default", "big-a", mother.OnNode("a"), mother.Requests("100m", "3Gi")),
		mother.Pod("default", "big-b", mother.OnNode("b"), mother.Requests("100m", "3Gi")),
		statusConfigMap(),
	)

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	var line string
	for _, l := range log.lines {
		if strings.Contains(l, "nothing to do") {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no decision was logged: %v", log.lines)
	}

	// The sentence's own number, whatever it is, must be the field's number.
	m := regexp.MustCompile(`(\d+) node\(s\) considered`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("the reason did not report a count: %s", line)
	}
	if !strings.Contains(line, `"nodesConsidered"=`+m[1]) {
		t.Errorf("the reason says %s considered, the field disagrees: %s", m[1], line)
	}
	if !strings.Contains(line, `"nodesSkipped"=2`) {
		t.Errorf("the two nodes ruled out before simulation are not accounted for: %s", line)
	}
}

func TestACompletedDrainStartsTheCooldown(t *testing.T) {
	// cooldown.afterDrain was configurable, documented, and measured from a
	// field nothing ever set — so it could never fire, and binpack would drain
	// a node and pick another on the very next evaluation.
	var log captured
	rec := &fakeRecorder{}
	draining := inPool("a", mother.Cordoned(), mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainStarted: time.Now().Add(-time.Minute).Format(time.RFC3339),
	}))
	ev := newEvaluator(t, &log, rec, draining, inPool("b"), inPool("c"), statusConfigMap())
	ev.opts.DryRun = false
	ev.opts.Engine.Default.CooldownAfterDrain = 30 * time.Minute

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ev.active != "a" {
		t.Fatalf("setup: the drain was not picked up, active = %q", ev.active)
	}

	// The autoscaler removes the node, which completes the drain.
	if err := ev.reader.(client.Client).Delete(context.Background(), draining); err != nil {
		t.Fatalf("deleting node a: %v", err)
	}
	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if ev.lastDrain.IsZero() {
		t.Fatal("the completed drain was not recorded, so the cooldown measures from nothing")
	}

	// The next evaluation must decline, and say why in the documented terms.
	log.lines = nil
	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if !log.contains("letting the cluster settle") {
		t.Errorf("no cooldown after a completed drain: %v", log.lines)
	}
}

func TestAnAbandonedDrainDoesNotStartTheCooldown(t *testing.T) {
	// Backoff is already recorded on the node that failed. A cluster-wide
	// cooldown as well would punish every other node for one node's failure,
	// and the two exist precisely to be different instruments.
	var log captured
	stalled := inPool("a", mother.Cordoned(), mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainStarted:  time.Now().Add(-time.Hour).Format(time.RFC3339),
		engine.AnnotationDrainProgress: time.Now().Add(-time.Hour).Format(time.RFC3339),
	}))
	ev := newEvaluator(t, &log, &fakeRecorder{}, stalled,
		mother.Pod("default", "stuck", mother.OnNode("a")),
		inPool("b"), inPool("c"), statusConfigMap())
	ev.opts.DryRun = false
	ev.opts.Engine.Default.CooldownAfterDrain = 30 * time.Minute

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if !ev.lastDrain.IsZero() {
		t.Error("an abandoned drain started a cluster-wide cooldown")
	}
}

func TestOnceRefusesACooldownItCannotHonour(t *testing.T) {
	// Every --once run is a new process, and a completed drain deletes the
	// node that would have recorded it — so the cooldown measures from a value
	// that is always zero. A safety control that is configured, reported as
	// configured, and silently inert is worse than one that is absent.
	for _, tc := range []struct {
		name    string
		refused bool
		to      func(*Options)
	}{
		{
			name: "acting on a schedule, with a cooldown configured",
			// The whole point: this is the combination that cannot work.
			refused: true,
			to:      func(o *Options) { o.Engine.Default.CooldownAfterDrain = 15 * time.Minute },
		},
		{
			name:    "a pool sets one, even though the default does not",
			refused: true,
			to: func(o *Options) {
				o.Engine.ByPool = map[string]engine.Policy{
					"gpu": {CooldownAfterDrain: 15 * time.Minute},
				}
			},
		},
		{
			name: "the cooldown is switched off deliberately",
			to:   func(o *Options) { o.Engine.Default.CooldownAfterDrain = 0 },
		},
		{
			// Nothing changes, so there is nothing to settle after.
			name: "deciding only",
			to: func(o *Options) {
				o.DryRun = true
				o.Engine.Default.CooldownAfterDrain = 15 * time.Minute
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{Once: true, DryRun: false, Engine: config()}
			tc.to(&opts)

			err := Run(context.Background(), opts)
			refused := err != nil && strings.Contains(err.Error(), "cooldown.afterDrain")

			if refused != tc.refused {
				t.Errorf("refused = %v, want %v (err: %v)", refused, tc.refused, err)
			}
			if tc.refused && !strings.Contains(err.Error(), "Deployment") {
				t.Errorf("the refusal does not say how to resolve it: %v", err)
			}
		})
	}
}

func TestADeploymentIsNotRefusedItsCooldown(t *testing.T) {
	// The mode that can honour it must not be caught by the guard: binpack
	// running as a Deployment holds the timestamp for as long as it runs.
	err := Run(context.Background(), Options{
		Once: false, DryRun: false,
		Engine: func() engine.Config {
			c := config()
			c.Default.CooldownAfterDrain = 15 * time.Minute
			return c
		}(),
	})

	if err != nil && strings.Contains(err.Error(), "cooldown.afterDrain") {
		t.Errorf("a Deployment was refused a cooldown it can honour: %v", err)
	}
}
