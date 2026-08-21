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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/leaderelection"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/motleyhand/binpack/internal/collect"
	"github.com/motleyhand/binpack/internal/drain"
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
	//
	// Asked of the acting mode, which is the only mode where it means
	// anything: what pre-emption prevents is a second node being cordoned, and
	// a dry run cordons nothing whatever it decides. It used to be asked of a
	// dry run, where "no event was recorded" passed for the property — and
	// where the silence it was really observing was binpack reporting on
	// nothing at all for as long as the marker stood.
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

	for _, e := range rec.events {
		if e.reason == ReasonDraining || e.reason == ReasonWouldDrain {
			t.Errorf("a new drain was decided on while one was already running: %+v", e)
		}
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

func TestAOneShotRunReturnsAReadFailureRatherThanHidingIt(t *testing.T) {
	// A read that fails every tick is a broken deployment, bad RBAC most
	// likely. A run that logs it and carries on looks healthy while doing
	// nothing at all.
	//
	// Immediately here, because --once has no next tick: this process is about
	// to exit and its logs go with it, and a CronJob that exits 0 having read
	// nothing is indistinguishable from one that found nothing. A controller
	// counts the same failure instead and stops on the fifth, which is
	// TestAFailureThatNeverClearsStillStopsTheController's read row.
	var log captured
	ev := newEvaluator(t, &log, &fakeRecorder{},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: collect.StatusConfigMapNamespace,
				Name:      collect.StatusConfigMapName,
			},
			Data: map[string]string{"status": "\tnot: [valid"},
		})
	ev.opts.Once = true

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

	// And on the first evaluation, not the fifth. Everything else a failed
	// evaluation can hit is worth retrying — the cluster may have moved — so
	// binpack carries on and counts. Waiting does not fix a typo, and the
	// evaluations spent waiting are ones where the misspelt override is
	// unreachable and its nodes are taking the default enabled policy.
	//
	// Asked of a controller rather than a one-shot run, which is the whole
	// point of the row: --once returns every failure, so this distinction is
	// invisible in that mode and the refusal was reachable and inert.
	ev = newEvaluator(t, &log, &fakeRecorder{}, inPool("a"), statusConfigMap())
	ev.opts.Engine, ev.opts.Once = cfg, false

	if err := ev.evaluate(context.Background()); err == nil {
		t.Error("a controller retried a configuration no retry can fix")
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

func TestTheLeaseIsSizedForBinpacksCadence(t *testing.T) {
	// controller-runtime's defaults — 15s/10s/2s — suit a controller that
	// reconciles constantly. binpack evaluates once a minute, so two five-second
	// API-server timeouts should not cost it the lease: that restart discards
	// the after-drain cooldown and the in-flight drain's completion, both of
	// which were justified on restarts being rare.
	out := managerOptions(Options{LeaderElection: true})

	if out.LeaseDuration == nil || *out.LeaseDuration != DefaultLeaseDuration {
		t.Errorf("lease duration: got %v, want %s", out.LeaseDuration, DefaultLeaseDuration)
	}
	if out.RenewDeadline == nil || *out.RenewDeadline != DefaultRenewDeadline {
		t.Errorf("renew deadline: got %v, want %s", out.RenewDeadline, DefaultRenewDeadline)
	}
	if out.RetryPeriod == nil || *out.RetryPeriod != DefaultRetryPeriod {
		t.Errorf("retry period: got %v, want %s", out.RetryPeriod, DefaultRetryPeriod)
	}

	// The whole point: the observed failure was two renewals timing out five
	// seconds apart, which the previous 10s deadline could not survive.
	if DefaultRenewDeadline <= 15*time.Second {
		t.Errorf("a renew deadline of %s still loses the lease to a brief API-server stall",
			DefaultRenewDeadline)
	}
}

func TestTheLeaseTimingsAreOverridable(t *testing.T) {
	out := managerOptions(Options{
		LeaderElection: true,
		LeaseDuration:  90 * time.Second,
		RenewDeadline:  60 * time.Second,
		RetryPeriod:    15 * time.Second,
	})

	if *out.LeaseDuration != 90*time.Second || *out.RenewDeadline != 60*time.Second ||
		*out.RetryPeriod != 15*time.Second {
		t.Errorf("got %v/%v/%v, want the values supplied",
			*out.LeaseDuration, *out.RenewDeadline, *out.RetryPeriod)
	}
}

func TestLeaseTimingsThatCannotWorkAreRefused(t *testing.T) {
	// client-go rejects these too, but from inside the manager and in terms of
	// a leader elector rather than the flags somebody set.
	for _, tc := range []struct {
		name     string
		opts     Options
		mentions string
	}{
		{
			// The dangerous one: a leader can keep acting on a lease another
			// replica has already taken.
			name: "renewing for longer than the lease lasts",
			opts: Options{LeaderElection: true,
				LeaseDuration: 30 * time.Second, RenewDeadline: 30 * time.Second},
			mentions: "renew deadline",
		},
		{
			// Nominally ordered, and still refused: retries are jittered, so
			// the last one before the deadline can land 1.2x late. This exact
			// combination passes an unjittered check and then crash-loops the
			// pod on a message about a leader elector.
			name: "a renew deadline inside the retry period's jitter",
			opts: Options{LeaderElection: true, LeaseDuration: 60 * time.Second,
				RenewDeadline: 10 * time.Second, RetryPeriod: 9 * time.Second},
			mentions: "jitter",
		},
		{
			name: "no room to retry before giving up",
			opts: Options{LeaderElection: true,
				RenewDeadline: 20 * time.Second, RetryPeriod: 20 * time.Second},
			mentions: "retry period",
		},
		{
			name: "the defaults themselves",
			opts: Options{LeaderElection: true},
		},
		{
			// A one-shot run never elects a leader, so timings it will never
			// apply are not a reason to refuse it.
			name: "timings a one-shot run will never use",
			opts: Options{Once: true, LeaseDuration: time.Second, RenewDeadline: time.Hour},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkLease(tc.opts)

			if tc.mentions == "" {
				if err != nil {
					t.Errorf("binpack's own defaults were refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted timings that cannot work")
			}
			if !strings.Contains(err.Error(), tc.mentions) {
				t.Errorf("error does not name the offending setting (%q): %v", tc.mentions, err)
			}
		})
	}
}

func TestWhatBinpackAcceptsClientGoAlsoAccepts(t *testing.T) {
	// The check exists to fail early with a message about the flags somebody
	// set. It is only worth having if it agrees with the validation it is
	// standing in front of — so this asks client-go directly rather than
	// restating its rules, which would be the same drift in a second place.
	for _, tc := range []struct {
		name                string
		lease, renew, retry time.Duration
	}{
		{"binpack's defaults", 0, 0, 0},
		{"a slower control plane", 120 * time.Second, 90 * time.Second, 20 * time.Second},
		{"controller-runtime's own defaults", 15 * time.Second, 10 * time.Second, 2 * time.Second},
		{"tight but legal", 10 * time.Second, 5 * time.Second, 4 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := Options{LeaderElection: true,
				LeaseDuration: tc.lease, RenewDeadline: tc.renew, RetryPeriod: tc.retry}

			if err := checkLease(opts); err != nil {
				t.Skipf("binpack refuses these, so client-go never sees them: %v", err)
			}

			out := managerOptions(opts)
			_, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
				LeaseDuration: *out.LeaseDuration,
				RenewDeadline: *out.RenewDeadline,
				RetryPeriod:   *out.RetryPeriod,
			})

			// It rejects the nil lock and nil callbacks too, which is not what
			// is being asked. Only a timing complaint means the two disagree.
			for _, timing := range []string{"leaseDuration", "renewDeadline", "retryPeriod"} {
				if err != nil && strings.Contains(err.Error(), timing) {
					t.Errorf("binpack accepted timings client-go refuses: %v", err)
				}
			}
		})
	}
}

// --- PR 12 scratch tests (to be placed properly) ---

// refusingEvictions answers the eviction subresource with err while letting
// node patches through, which is what a disruption budget with no allowance
// looks like from here.
type refusingEvictions struct {
	client.Client
	err error
}

func (r refusingEvictions) SubResource(string) client.SubResourceClient {
	return refusedSubResource{r.err}
}

type refusedSubResource struct{ err error }

func (s refusedSubResource) Create(context.Context, client.Object, client.Object, ...client.SubResourceCreateOption) error {
	return s.err
}

func (s refusedSubResource) Get(context.Context, client.Object, client.Object, ...client.SubResourceGetOption) error {
	return nil
}

func (s refusedSubResource) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return nil
}

func (s refusedSubResource) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return nil
}

func (s refusedSubResource) Apply(context.Context, runtime.ApplyConfiguration, ...client.SubResourceApplyOption) error {
	return nil
}

// relocatable is a pod a drain can actually move: owned by a ReplicaSet whose
// template binpack can read. A pod with no readable controller makes its node
// infeasible, so nothing would ever be evicted from it.
func relocatable(node, name string) (*corev1.Pod, client.Object) {
	pod := mother.Pod("default", name,
		mother.OnNode(node), mother.ControlledBy("ReplicaSet", name+"-rs"))
	spec := *pod.Spec.DeepCopy()
	spec.NodeName = ""
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: pod.Namespace, Name: name + "-rs",
			UID: mother.OwnerUID(name + "-rs"),
		},
		Spec: appsv1.ReplicaSetSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: pod.Labels}, Spec: spec}},
	}
	return pod, rs
}

// drainingNode is a node binpack is part-way through draining.
func drainingNode(name string) *corev1.Node {
	stamp := time.Now().Add(-time.Minute).Format(time.RFC3339)
	return inPool(name, mother.Cordoned(), mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainStarted:  stamp,
		engine.AnnotationDrainProgress: stamp,
	}))
}

// evaluationErrors reads binpack_evaluation_errors_total through the same
// registry a scrape would.
func evaluationErrors(t *testing.T) float64 {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "binpack_evaluation_errors_total" {
			continue
		}
		return f.GetMetric()[0].GetCounter().GetValue()
	}
	t.Fatal("binpack_evaluation_errors_total is not published at all")
	return 0
}

// evaluationsWithCode is binpack_evaluations_total for one outcome code.
func evaluationsWithCode(t *testing.T, code string) float64 {
	t.Helper()
	return series(t, "binpack_evaluations_total", "code", code)
}

func TestATransientEvictionRefusalDoesNotStopTheController(t *testing.T) {
	var log captured
	pod, rs := relocatable("a", "web")
	ev := newEvaluator(t, &log, &fakeRecorder{},
		drainingNode("a"), inPool("b"), inPool("c"), pod, rs, statusConfigMap())
	ev.opts.DryRun = false
	ev.opts.Once = false
	ev.writer = refusingEvictions{
		Client: ev.writer.(client.Client),
		err:    apierrors.NewTooManyRequests("", 1),
	}

	before := evaluationErrors(t)
	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("a refused eviction stopped the controller: %v", err)
	}
	if got := evaluationErrors(t) - before; got != 1 {
		t.Errorf("binpack_evaluation_errors_total moved by %v, want 1", got)
	}
}

// breakingWrites fails every node patch for as long as failing says so.
//
// The flag is a pointer because what is under test is a count of *consecutive*
// failures, and a counter that resets on every call looks exactly like one that
// accumulates if the failure is never turned off partway through.
type breakingWrites struct {
	client.Client
	failing *bool
	err     error
}

func (b breakingWrites) Patch(
	ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption,
) error {
	if *b.failing {
		return b.err
	}
	return b.Client.Patch(ctx, obj, patch, opts...)
}

// breakingReads is the same for the read at the top of an evaluation.
type breakingReads struct {
	client.Reader
	failing *bool
	err     error
}

func (b breakingReads) List(
	ctx context.Context, list client.ObjectList, opts ...client.ListOption,
) error {
	if *b.failing {
		return b.err
	}
	return b.Reader.List(ctx, list, opts...)
}

// breaking wires both wrappers onto an evaluator and returns the switch.
func breaking(ev *evaluator, err error) *bool {
	failing := new(bool)
	c := ev.writer.(client.Client)
	ev.writer = breakingWrites{Client: c, failing: failing, err: err}
	ev.reader = breakingReads{Reader: c, failing: failing, err: err}
	return failing
}

func TestAFailureThatNeverClearsStillStopsTheController(t *testing.T) {
	for _, tc := range []struct {
		name string
		only func(*evaluator, *bool)
	}{
		{"a write nothing will accept", func(ev *evaluator, _ *bool) {
			ev.reader = ev.reader.(breakingReads).Reader
		}},
		{"a read nothing will answer", func(ev *evaluator, _ *bool) {
			ev.writer = ev.writer.(breakingWrites).Client
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log captured
			pod, rs := relocatable("a", "web")
			ev := newEvaluator(t, &log, &fakeRecorder{},
				drainingNode("a"), inPool("b"), inPool("c"), pod, rs, statusConfigMap())
			ev.opts.DryRun, ev.opts.Once = false, false
			failing := breaking(ev, apierrors.NewForbidden(
				schema.GroupResource{Resource: "nodes"}, "a", errors.New("no")))
			*failing = true
			tc.only(ev, failing)

			for i := 1; i < maxConsecutiveFailures; i++ {
				if err := ev.evaluate(context.Background()); err != nil {
					t.Fatalf("failure %d of %d stopped the controller: %v",
						i, maxConsecutiveFailures, err)
				}
			}
			if err := ev.evaluate(context.Background()); err == nil {
				t.Fatalf("%d consecutive failures were retried quietly for ever",
					maxConsecutiveFailures)
			}
		})
	}
}

func TestAFailureThatClearsIsNotCountedTowardsTheNext(t *testing.T) {
	var log captured
	pod, rs := relocatable("a", "web")
	ev := newEvaluator(t, &log, &fakeRecorder{},
		drainingNode("a"), inPool("b"), inPool("c"), pod, rs, statusConfigMap())
	ev.opts.DryRun, ev.opts.Once = false, false
	failing := breaking(ev, apierrors.NewServiceUnavailable("etcd leader changed"))

	for i := 0; i < 4*maxConsecutiveFailures; i++ {
		*failing = i%2 == 0
		if err := ev.evaluate(context.Background()); err != nil {
			t.Fatalf("round %d stopped the controller over a cluster that keeps recovering: %v",
				i, err)
		}
	}
}

func TestShutdownHandsTheLeaseOverRatherThanWaitingItOut(t *testing.T) {
	opts := managerOptions(Options{LeaderElection: true, MetricsAddress: "0", ProbeAddress: "0"})
	if !opts.LeaderElectionReleaseOnCancel {
		t.Error("a rolling update would wait out the lease instead of handing over")
	}
	if opts.LeaderElectionID != LeaderElectionID {
		t.Errorf("lease ID = %q, want the documented %q", opts.LeaderElectionID, LeaderElectionID)
	}

	var log captured
	pod, rs := relocatable("a", "web")
	ev := newEvaluator(t, &log, &fakeRecorder{},
		drainingNode("a"), inPool("b"), pod, rs, statusConfigMap())
	ev.opts.DryRun, ev.opts.Once = false, false
	failing := breaking(ev, context.Canceled)
	*failing = true

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	before := evaluationErrors(t)
	done := make(chan error, 1)
	go func() { done <- ev.Start(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a shutdown was reported as a failure: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the evaluator held the manager past the graceful window")
	}
	if got := evaluationErrors(t) - before; got != 0 {
		t.Errorf("a shutdown moved binpack_evaluation_errors_total by %v", got)
	}
}

// nodesReported sums binpack_nodes across every verdict: how many nodes the
// last evaluation actually assessed.
func nodesReported(t *testing.T) float64 {
	t.Helper()
	return series(t, "binpack_nodes", "", "")
}

// nodesWithVerdict is binpack_nodes for one verdict.
func nodesWithVerdict(t *testing.T, verdict string) float64 {
	t.Helper()
	return series(t, "binpack_nodes", "verdict", verdict)
}

// series sums one binpack_ metric family, keeping only the children carrying a
// given label value — or all of them when the label name is empty.
//
// Gauge and counter values are added because exactly one of them is ever set
// and the protobuf getters are nil-safe on the other, which lets the callers
// below read a gauge and a counter through one function. Written against the
// gathered families rather than against prometheus/client_model's types on
// purpose: naming that package here would promote it from an indirect
// dependency to a direct one, for a test helper, and CI checks tidiness.
func series(t *testing.T, family, label, value string) float64 {
	t.Helper()
	families, err := ctrlmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}

	var total float64
	for _, f := range families {
		if f.GetName() != family {
			continue
		}
		for _, m := range f.GetMetric() {
			matched := label == ""
			for _, l := range m.GetLabel() {
				if l.GetName() == label && l.GetValue() == value {
					matched = true
				}
			}
			if matched {
				total += m.GetGauge().GetValue() + m.GetCounter().GetValue()
			}
		}
	}
	return total
}

func TestDryRunStillReportsOnTheRestOfTheCluster(t *testing.T) {
	var log captured
	rec := &fakeRecorder{}
	ev := newEvaluator(t, &log, rec,
		drainingNode("a"), inPool("b"), inPool("c"), statusConfigMap())

	drainingBefore := evaluationsWithCode(t, engine.CodeDraining)

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	// Counted as what it was. Before the drain-in-progress gate moved into the
	// engine this evaluation ended in a decision to drain a second node, so it
	// was counted `drain` — an outcome nothing acted on, in the mode whose
	// whole purpose is saying what would happen.
	if got := evaluationsWithCode(t, engine.CodeDraining) - drainingBefore; got != 1 {
		t.Errorf("binpack_evaluations_total{code=draining} moved by %v, want 1", got)
	}

	var stranded bool
	for _, e := range rec.events {
		n, ok := e.object.(*corev1.Node)
		if !ok {
			continue
		}
		if n.Name == "a" && e.reason == ReasonWouldAdvanceDrain {
			stranded = true
		}
	}
	if !stranded {
		t.Errorf("nothing said what would happen to the drain binpack is not advancing: %+v",
			rec.events)
	}
	if got := nodesReported(t); got != 3 {
		t.Errorf("binpack_nodes covers %v nodes, want all 3", got)
	}
	// Assessed, not merely counted. A drain in progress stops binpack choosing
	// a second node, so nothing here names b or c any more — but the whole of
	// what dry run is for is that the rest of the cluster is still evaluated,
	// and three nodes counted would be satisfied by three skips. Reaching
	// drainable means the simulation ran for both.
	if got := nodesWithVerdict(t, engine.VerdictDrainable); got != 2 {
		t.Errorf("binpack_nodes{verdict=drainable} = %v, want b and c both assessed", got)
	}

	var nodes corev1.NodeList
	if err := ev.reader.(client.Client).List(context.Background(), &nodes); err != nil {
		t.Fatalf("listing nodes: %v", err)
	}
	for _, n := range nodes.Items {
		if n.Name != "a" && (n.Spec.Unschedulable || len(n.Annotations) > 0) {
			t.Errorf("dry run wrote to node %s while carrying on past the drain", n.Name)
		}
	}
}

func TestDryRunForgetsADrainWhoseNodeHasGone(t *testing.T) {
	// The autoscaler finished a drain that started before dry run was switched
	// on. Nothing is stranded and there is nothing to report on, and a node
	// binpack goes on remembering is one it writes an event about every
	// evaluation for ever — on an object that is not there.
	var log captured
	rec := &fakeRecorder{}
	ev := newEvaluator(t, &log, rec, inPool("b"), inPool("c"), statusConfigMap())
	ev.active = "a"

	if err := ev.evaluate(context.Background()); err != nil {
		t.Fatalf("evaluate: %v", err)
	}

	if ev.active != "" {
		t.Errorf("still remembering a drain of a node that has gone: %q", ev.active)
	}
	for _, e := range rec.events {
		if e.reason == ReasonWouldAdvanceDrain {
			t.Errorf("reported on a node that is no longer in the cluster: %+v", e)
		}
	}
}

func TestWhatAFrozenDrainIsToldAboutItself(t *testing.T) {
	// The sentence an operator reads on a node binpack has stopped advancing,
	// and the one place a dry run can mislead: every row here is something
	// revalidation or the assessment observed, never a prediction of which
	// ending Advance would reach. Two of the conditions below do not have one
	// ending — a node the autoscaler is already removing is handed over rather
	// than back, and one marked but uncordoned is repaired — so a sentence
	// naming an ending would be wrong for them and right for the rest, which
	// is the worst way to be wrong.
	stalled := drain.Assessment{Action: drain.Abandon,
		Code: drain.AbandonStalled, Reason: "no pod has left for 11m0s"}
	moving := drain.Assessment{Action: drain.Continue, Remaining: 3}

	for _, tc := range []struct {
		name       string
		assessment engine.NodeAssessment
		drain      drain.Assessment
		want       string
	}{
		{"the cluster moved underneath it",
			engine.NodeAssessment{Skipped: true, SkipReason: "pool pool-4g is at its minimum size (1)"},
			moving, "The cluster has moved underneath it: pool pool-4g is at its minimum size (1)."},
		{"a pod can no longer be evicted",
			engine.NodeAssessment{Blockers: []engine.EvictionBlocker{
				{Message: "default/web is covered by two PodDisruptionBudgets"}}},
			moving, "A pod on it can no longer be evicted: default/web is covered by two " +
				"PodDisruptionBudgets."},
		{"the pods no longer fit",
			engine.NodeAssessment{Simulation: &engine.Simulation{Feasible: false}},
			moving, "The pods still on it no longer fit anywhere else."},
		// The bound, which is what a frozen drain does not have — and the row
		// that says a drain nobody is advancing has run past it.
		{"past its bound", engine.NodeAssessment{}, stalled,
			"It has passed its bound and would be handed back: no pod has left for 11m0s (stalled)."},
		{"still going", engine.NodeAssessment{}, moving,
			"It is within its bounds, with 3 pods left to move."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := wouldHappen(tc.assessment, tc.drain); got != tc.want {
				t.Errorf("wouldHappen() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestALostFrozenDrainEventFailsAOneShotRunAndOnlyAOneShotRun(t *testing.T) {
	// A frozen drain's event is the whole output of that evaluation: nothing is
	// written, nothing is advanced, and the note on the node is the only thing
	// that says a cordoned node is being left cordoned deliberately. In --once
	// that makes it the only durable record there is — the process is exiting
	// and its logs go with it — and the decision that follows writes nothing at
	// all when there is nothing to drain, so the run would exit 0 having
	// reported none of it.
	//
	// The same rule the decision's own report has drawn since it was written,
	// and both directions matter: a controller must not crash-loop over one
	// lost event, because on a cluster where events are the only reachable
	// surface that takes the logs away too.
	for _, tc := range []struct {
		name string
		once bool
		want bool
	}{
		{"a one-shot run", true, true},
		{"a controller", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The marked node and nothing else, which is the half of this
			// the decision cannot cover for. With another candidate present
			// the decision that follows writes its own event and fails on
			// that, so the frozen drain's lost event is invisible; with none,
			// report logs "nothing to do", writes nothing, and returns nil.
			var log captured
			ev := newEvaluator(t, &log, &fakeRecorder{},
				drainingNode("a"), statusConfigMap())
			ev.reporter = failingReporter{}
			ev.opts.Once = tc.once

			err := ev.evaluate(context.Background())
			if tc.want && err == nil {
				t.Fatal("a one-shot run reported nothing about the drain it left alone " +
					"and called it success")
			}
			if !tc.want && err != nil {
				t.Fatalf("a controller stopped over one lost event: %v", err)
			}
			if tc.want && !strings.Contains(err.Error(), "the drain of a") {
				t.Errorf("the error does not say what was lost: %v", err)
			}
			if !tc.want && !log.contains("could not record what would happen") {
				t.Errorf("the lost event was swallowed entirely:\n%s",
					strings.Join(log.lines, "\n"))
			}
		})
	}
}
