package executor_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/motleyhand/binpack/internal/drain"
	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/executor"
	"github.com/motleyhand/binpack/internal/mother"
)

const (
	poolID   = "pool-id"
	poolName = "pool"
)

var at = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

func drainPolicy() drain.Policy {
	return drain.Policy{
		StallTimeout: 10 * time.Minute, RemovalTimeout: 15 * time.Minute,
		BackoffInitial: 30 * time.Minute, BackoffMax: 24 * time.Hour,
	}
}

func engineConfig() engine.Config {
	return engine.Config{
		NodeGroupIDLabel: "pool-id-label",
		PoolNameLabel:    "pool-label",
		Default: engine.Policy{
			Enabled: true,
			Evict:   engine.DefaultEvictConfig(),
		},
	}
}

// node builds a pool member. Large enough that resources are never accidentally
// the reason a test fails.
func node(name string, opts ...mother.NodeOption) *corev1.Node {
	return mother.LargeNode(name, append([]mother.NodeOption{
		mother.NodeLabels(map[string]string{"pool-id-label": poolID, "pool-label": poolName}),
	}, opts...)...)
}

// marked is a node binpack has already begun draining: cordoned, with the
// markers Begin writes.
func marked(
	name string, startedAgo, progressAgo time.Duration, remaining string, opts ...mother.NodeOption,
) *corev1.Node {
	ann := map[string]string{
		engine.AnnotationDrainStarted:  at.Add(-startedAgo).Format(time.RFC3339),
		engine.AnnotationDrainProgress: at.Add(-progressAgo).Format(time.RFC3339),
	}
	if remaining != "" {
		ann[engine.AnnotationDrainPodsRemaining] = remaining
	}
	return node(name, append([]mother.NodeOption{
		mother.Cordoned(), mother.NodeAnnotations(ann),
	}, opts...)...)
}

func snapshot(nodes []*corev1.Node, pods []*corev1.Pod) engine.Snapshot {
	return engine.Snapshot{
		Nodes: nodes, Pods: pods, Templates: mother.Templates(pods...), Now: at,
		Autoscaler: engine.Autoscaler{
			Running: true, LastProbe: at.Add(-10 * time.Second),
			Groups: []engine.NodeGroup{{ID: poolID, MinSize: 1, MaxSize: 10, Ready: len(nodes)}},
		},
	}
}

func clientFor(s engine.Snapshot) client.Client {
	objs := make([]client.Object, 0, len(s.Nodes)+len(s.Pods))
	for _, n := range s.Nodes {
		objs = append(objs, n.DeepCopy())
	}
	for _, p := range s.Pods {
		pod := p.DeepCopy()
		// The fake client refuses to store a deletionTimestamp without a
		// finalizer, which a real API server is perfectly happy to hold — that
		// combination is what a wedged pod *is*. A harness constraint, so it is
		// accommodated here rather than in the mother, which models the object
		// the API server actually writes.
		if pod.DeletionTimestamp != nil && len(pod.Finalizers) == 0 {
			pod.Finalizers = []string{"binpack.test/hold"}
		}
		objs = append(objs, pod)
	}
	return fake.NewClientBuilder().WithObjects(objs...).Build()
}

func nodeFrom(t *testing.T, c client.Client, name string) *corev1.Node {
	t.Helper()
	var n corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &n); err != nil {
		t.Fatalf("re-reading node %s: %v", name, err)
	}
	return &n
}

func TestBeginMarksBeforeCordoning(t *testing.T) {
	// The order is the design. A process that stops between the two must leave
	// evidence a later evaluation can act on; cordoning first would leave a
	// cordoned node nothing knows to finish.
	n := node("a")
	c := clientFor(snapshot([]*corev1.Node{n}, nil))
	rec := &recorder{Writer: c}

	if err := executor.Begin(context.Background(), rec, n, at); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if len(rec.patches) != 2 {
		t.Fatalf("expected an annotate and a cordon, got %d writes", len(rec.patches))
	}
	if !strings.Contains(rec.patches[0], engine.AnnotationDrainStarted) {
		t.Errorf("the first write was not the marker: %s", rec.patches[0])
	}
	if !strings.Contains(rec.patches[1], "unschedulable") {
		t.Errorf("the second write was not the cordon: %s", rec.patches[1])
	}

	got := nodeFrom(t, c, "a")
	if !got.Spec.Unschedulable {
		t.Error("the node was not cordoned")
	}
	if got.Annotations[engine.AnnotationDrainStarted] == "" {
		t.Error("the drain marker was not written")
	}
}

func TestEveryEndingHandsTheNodeBack(t *testing.T) {
	// The central safety property, and the reason it is asserted rather than
	// argued: a cordoned node the autoscaler never removes is capacity that is
	// paid for and cannot be used, and if the pool is at its maximum, pods can
	// stay Pending indefinitely beside it.
	for _, tc := range []struct {
		name string
		code string
		pods []*corev1.Pod
		to   func(*engine.Snapshot)
	}{
		{
			name: "a scale-up began after the cordon",
			code: engine.SkipScaleUpInProgress,
			to:   func(s *engine.Snapshot) { s.Autoscaler.ScaleUpInProgress = true },
		},
		{
			name: "the pool reached its minimum",
			code: engine.SkipPoolAtMinimum,
			to:   func(s *engine.Snapshot) { s.Autoscaler.Groups[0].MinSize = 3 },
		},
		{
			// No skip code on this one: the verdict is what names it, and
			// publishing an empty label would put a value outside the
			// documented vocabulary into the metric.
			name: "the remaining pods no longer fit",
			code: engine.VerdictInfeasible,
			pods: []*corev1.Pod{
				mother.Pod("default", "huge", mother.OnNode("a"), mother.Requests("100m", "6Gi")),
				mother.Pod("default", "fill-b", mother.OnNode("b"), mother.Requests("100m", "6Gi")),
				mother.Pod("default", "fill-c", mother.OnNode("c"), mother.Requests("100m", "6Gi")),
			},
		},
		{
			name: "a pod is wedged past its termination deadline",
			code: drain.AbandonStuck,
			pods: []*corev1.Pod{mother.Pod("default", "wedged", mother.OnNode("a"),
				mother.Terminating(at.Add(-time.Hour), time.Minute))},
		},
		{
			name: "nothing has moved for longer than the stall timeout",
			code: drain.AbandonStalled,
			pods: []*corev1.Pod{mother.Pod("default", "idle", mother.OnNode("a"))},
		},
		{
			// The hand-over is the one ending that used to have no bound at
			// all: binpack waits for the autoscaler to take the node away, and
			// only a live autoscaler ever clears the taint that says it is
			// trying.
			name: "the autoscaler stopped without finishing the deletion it began",
			code: engine.SkipAutoscalerNotLive,
			pods: []*corev1.Pod{mother.Pod("default", "left", mother.OnNode("a"))},
			to: func(s *engine.Snapshot) {
				s.Nodes[0].Spec.Taints = append(s.Nodes[0].Spec.Taints, corev1.Taint{
					Key: engine.TaintToBeDeleted, Effect: corev1.TaintEffectNoSchedule})
				s.Autoscaler.LastProbe = at.Add(-30 * time.Minute)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Progress last seen 11 minutes ago, past the stall timeout, so
			// the cases that do not set up their own reason still end.
			nodes := []*corev1.Node{marked("a", time.Hour, 11*time.Minute, "1"), node("b"), node("c")}
			s := snapshot(nodes, tc.pods)
			if tc.to != nil {
				tc.to(&s)
			}
			c := clientFor(s)

			step, err := executor.Advance(
				context.Background(), c, s, "a", engineConfig(), drainPolicy())
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}

			if !step.Done || !step.Failed {
				t.Errorf("expected an abandoned drain, got %+v", step)
			}
			if tc.code != "" && step.Code != tc.code {
				t.Errorf("code: got %q, want %q (%s)", step.Code, tc.code, step.Reason)
			}
			if step.Reason == "" {
				t.Error("an abandoned drain must say why; this one recorded nothing")
			}

			got := nodeFrom(t, c, "a")
			if got.Spec.Unschedulable {
				t.Error("the node was left cordoned")
			}
			if got.Annotations[engine.AnnotationDrainStarted] != "" {
				t.Error("the drain marker was left behind, so binpack still thinks it is draining")
			}
			// Present is not enough: a backoff already in the past, or an
			// attempt count that never rises, would leave the candidate
			// ordering preferring the node that just failed — it now has
			// fewer pods than before.
			until, err := time.Parse(time.RFC3339, got.Annotations[engine.AnnotationBackoffUntil])
			if err != nil {
				t.Errorf("no readable backoff was recorded (%q), so the next run retries "+
					"the node it just failed", got.Annotations[engine.AnnotationBackoffUntil])
			} else if !until.After(at) {
				t.Errorf("the backoff expires at %s, which is not in the future", until)
			}
			if got.Annotations[engine.AnnotationDrainAttempts] != "1" {
				t.Errorf("attempts: got %q, want 1",
					got.Annotations[engine.AnnotationDrainAttempts])
			}
			if got.Annotations[engine.AnnotationLastFailure] != step.Reason {
				t.Errorf("the recorded failure does not match the step: %q vs %q",
					got.Annotations[engine.AnnotationLastFailure], step.Reason)
			}
		})
	}
}

func TestRevalidationRecordsTheBlockersOwnReason(t *testing.T) {
	// The recorded failure is what the operator reads, on the node and in the
	// event, and "the node is no longer drainable" is worth nothing to them.
	// Every arm of the reason exists to say the specific thing; this is the
	// one where the specific thing is a budget somebody can go and edit.
	//
	// The abandonment table above asserts that *a* reason was recorded and
	// that it matches the step. It cannot see this, because a generic
	// sentence satisfies both.
	labelled := map[string]string{"app": "web"}
	pods := []*corev1.Pod{
		mother.Pod("default", "web-1", mother.OnNode("a"), mother.PodLabels(labelled)),
		mother.Pod("default", "web-2", mother.OnNode("b"), mother.PodLabels(labelled)),
	}
	s := snapshot([]*corev1.Node{marked("a", time.Hour, time.Minute, "1"), node("b")}, pods)
	// Edited underneath the drain: the budget that allowed this one now
	// allows nothing.
	s.PDBs = []*policyv1.PodDisruptionBudget{mother.PDB("default", "web", 0, labelled)}
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if !step.Failed {
		t.Fatalf("a budget allowing nothing ends the drain, got %+v", step)
	}
	if !strings.Contains(step.Reason, "default/web") {
		t.Errorf("the reason should name the budget to edit: %q", step.Reason)
	}
	if got := nodeFrom(t, c, "a").Annotations[engine.AnnotationLastFailure]; !strings.Contains(got, "default/web") {
		t.Errorf("the node records a reason nobody can act on: %q", got)
	}
}

func TestAMarkedNodeThatIsNotCordonedIsRepaired(t *testing.T) {
	// A process that stopped between Begin's two writes. Nothing has been
	// evicted on a bad snapshot — revalidation refuses to look at an
	// uncordoned node — so this is repaired rather than abandoned.
	nodes := []*corev1.Node{node("a", mother.NodeAnnotations(map[string]string{
		engine.AnnotationDrainStarted: at.Add(-time.Minute).Format(time.RFC3339),
	})), node("b")}
	s := snapshot(nodes, nil)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if step.Code != executor.StepCordoned || step.Done {
		t.Errorf("expected the drain to be repaired and continue, got %+v", step)
	}
	if !nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the node was not cordoned")
	}
}

func TestAMarkedNodeNobodyLetsBinpackCordonIsStillBounded(t *testing.T) {
	// The repair is right for the case it was written for — a process that
	// stopped between Begin's two writes — but it is not a resting place. A
	// controller that owns spec.unschedulable, or an operator answering an
	// unexpected SchedulingDisabled with kubectl uncordon, puts the drain back
	// here every interval, and the repair on its own never consults a bound.
	nodes := []*corev1.Node{marked("a", 6*time.Hour, 6*time.Hour, "1"), node("b")}
	nodes[0].Spec.Unschedulable = false
	pods := []*corev1.Pod{mother.Pod("default", "idle", mother.OnNode("a"))}
	s := snapshot(nodes, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if !step.Failed || step.Code != drain.AbandonStalled {
		t.Errorf("expected the repair to consult the stall bound, got %+v", step)
	}
	if nodeFrom(t, c, "a").Annotations[engine.AnnotationDrainStarted] != "" {
		t.Error("the drain marker survived, so the next interval repairs the node again")
	}
}

func TestAVanishedNodeIsSuccess(t *testing.T) {
	// The cluster-autoscaler removing the node is the outcome a drain works
	// towards, so finding it gone is completion rather than an error.
	s := snapshot([]*corev1.Node{node("b"), node("c")}, nil)

	step, err := executor.Advance(context.Background(), clientFor(s), s, "a",
		engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if !step.Done || step.Failed || step.Code != executor.StepRemoved {
		t.Errorf("expected a completed drain, got %+v", step)
	}
}

func TestOnePodLeavesAtATime(t *testing.T) {
	// The simulation proves a valid assignment exists, not that the scheduler
	// will choose it. Keeping one pod in flight is what makes a wrong
	// prediction cost one eviction rather than all of them.
	pods := []*corev1.Pod{
		mother.Pod("default", "one", mother.OnNode("a")),
		mother.Pod("default", "two", mother.OnNode("a")),
		mother.Pod("default", "three", mother.OnNode("a")),
	}
	s := snapshot([]*corev1.Node{marked("a", time.Minute, time.Minute, "3"), node("b")}, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if step.Code != executor.StepEvicted || step.Done {
		t.Fatalf("expected one eviction, got %+v", step)
	}

	var left corev1.PodList
	if err := c.List(context.Background(), &left); err != nil {
		t.Fatalf("listing pods: %v", err)
	}
	if len(left.Items) != 2 {
		t.Errorf("expected exactly one pod evicted, %d of 3 remain", len(left.Items))
	}
}

func TestNothingIsEvictedWhileAPodIsStillTerminating(t *testing.T) {
	// Sequential means sequential. A second eviction while the first pod is
	// still shutting down would put two pods in flight against one simulation.
	pods := []*corev1.Pod{
		mother.Pod("default", "going", mother.OnNode("a"),
			mother.Terminating(at.Add(-time.Minute), time.Hour)),
		mother.Pod("default", "waiting", mother.OnNode("a")),
	}
	s := snapshot([]*corev1.Node{marked("a", time.Minute, time.Minute, "2"), node("b")}, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if step.Code != executor.StepWaiting {
		t.Errorf("expected the drain to wait, got %+v", step)
	}
	var left corev1.PodList
	if err := c.List(context.Background(), &left); err != nil {
		t.Fatalf("listing pods: %v", err)
	}
	if len(left.Items) != 2 {
		t.Errorf("a pod was evicted while another was terminating: %d remain", len(left.Items))
	}
}

// pending builds a pod a controller has created but the scheduler has not
// placed. unschedulable marks the ones it has tried and given up on.
func pending(name, owner string, created time.Time, unschedulable bool) *corev1.Pod {
	p := mother.Pod("default", name, mother.ControlledBy("ReplicaSet", owner))
	p.CreationTimestamp.Time = created
	p.Status.Phase = corev1.PodPending
	p.Status.Conditions = nil
	if unschedulable {
		p.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable,
		}}
	}
	return p
}

// awaitingNode is a node mid-drain that has already evicted a pod of `owner`
// and is waiting for the replacement.
func awaitingNode(name, owner string, evictedAgo time.Duration) *corev1.Node {
	n := marked(name, 20*time.Minute, evictedAgo, "1")
	n.Annotations[engine.AnnotationDrainAwaiting] = string(mother.OwnerUID(owner)) +
		"@" + at.Add(-evictedAgo).Format(time.RFC3339)
	return n
}

func TestNothingIsEvictedUntilTheReplacementIsBound(t *testing.T) {
	// "In flight" has to mean bound, not merely gone from this node. An
	// evicted pod disappears well before its replacement is placed, and
	// evicting the next one in that window would put two replacements in
	// flight against a simulation that assumed one.
	// The evicted pod's controller already has a replica elsewhere, which is
	// the ordinary case for a Deployment. Answering "landed" from that one
	// would let the drain run straight on without the replacement ever
	// existing, so the pod must also be newer than the eviction.
	sibling := pending("sibling", "web-rs", at.Add(-time.Hour), false)
	sibling.Spec.NodeName = "b"

	pods := []*corev1.Pod{
		mother.Pod("default", "stayer", mother.OnNode("a")),
		sibling,
		// Created, not yet placed, and the scheduler has not refused it.
		pending("replacement", "web-rs", at.Add(-30*time.Second), false),
	}
	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")}, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if step.Code != executor.StepWaiting {
		t.Errorf("expected the drain to wait, got %+v", step)
	}

	var left corev1.PodList
	if err := c.List(context.Background(), &left); err != nil {
		t.Fatalf("listing pods: %v", err)
	}
	if len(left.Items) != 3 {
		t.Errorf("a pod was evicted before the previous replacement landed: %d of 3 remain",
			len(left.Items))
	}
}

func TestABoundReplacementLetsTheDrainProceed(t *testing.T) {
	replacement := pending("replacement", "web-rs", at.Add(-30*time.Second), false)
	replacement.Spec.NodeName = "b"

	pods := []*corev1.Pod{mother.Pod("default", "stayer", mother.OnNode("a")), replacement}
	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")}, pods)

	step, err := executor.Advance(context.Background(), clientFor(s), s, "a",
		engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if step.Code != executor.StepEvicted {
		t.Errorf("expected the drain to proceed, got %+v", step)
	}
}

// committedDrain is the ordinary state of a drain that has done some of its
// work: one pod already evicted and its replacement bound elsewhere, one pod
// still to go. The config carries a cooldown so that window exists to fall
// inside.
func committedDrain() (engine.Snapshot, engine.Config) {
	replacement := pending("replacement", "web-rs", at.Add(-30*time.Second), false)
	replacement.Spec.NodeName = "b"

	pods := []*corev1.Pod{mother.Pod("default", "stayer", mother.OnNode("a")), replacement}
	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")}, pods)

	cfg := engineConfig()
	cfg.Default.CooldownAfterScaleUp = 10 * time.Minute
	return s, cfg
}

func TestAScaleUpElsewhereDoesNotAbandonACommittedDrain(t *testing.T) {
	// A drain that has evicted pods has spent something, and abandoning does
	// not refund it: the pods stay where they went, the consolidation is lost,
	// and the node is billed thirty minutes of backoff for an event it had no
	// part in. The trigger is not even pool-scoped — the autoscaler publishes
	// scale-up state for the whole cluster, so growth in a pool binpack cannot
	// touch used to end a drain in one it can.
	//
	// Neither arm makes the drain unsound. A scale-up adds nodes, so the pods
	// still on this one cannot fit less well than they did a minute ago; and
	// the cooldown arm reports growth that finished up to ten minutes back.
	// See ADR-0010.
	for _, tc := range []struct {
		name string
		to   func(*engine.Snapshot)
	}{
		{"the cluster is growing right now",
			func(s *engine.Snapshot) { s.Autoscaler.ScaleUpInProgress = true }},
		{"the cluster grew a minute ago",
			func(s *engine.Snapshot) { s.Autoscaler.LastScaleUp = at.Add(-time.Minute) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, cfg := committedDrain()
			tc.to(&s)
			c := clientFor(s)

			step, err := executor.Advance(context.Background(), c, s, "a", cfg, drainPolicy())
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}

			if step.Failed {
				t.Errorf("a drain that had already moved a pod was abandoned: %+v", step)
			}
			if step.Code != executor.StepEvicted {
				t.Errorf("expected the drain to carry on, got %+v", step)
			}
			if !nodeFrom(t, c, "a").Spec.Unschedulable {
				t.Error("the node was handed back, so it is accepting pods again while " +
					"the ones it already lost stay where they went")
			}
		})
	}
}

func TestAnEmptiedNodeIsNotAbandonedByAScaleUp(t *testing.T) {
	// The worst moment available. Every pod has already been moved and the
	// drain is one autoscaler pass from succeeding, so handing the node back
	// discards the whole of the work — and the same window is open for as long
	// as the removal takes, which is minutes, not seconds.
	s, cfg := committedDrain()
	// The pod that was still to go has gone too, and the node records it.
	s.Pods = s.Pods[1:]
	s.Nodes[0].Annotations[engine.AnnotationDrainPodsRemaining] = "0"
	s.Autoscaler.ScaleUpInProgress = true
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", cfg, drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if step.Failed || step.Code != executor.StepAwaitRemoval {
		t.Errorf("an emptied node was abandoned instead of waiting to be reaped: %+v", step)
	}
	if !nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("an emptied node was uncordoned, so it will fill up again before " +
			"the autoscaler gets to it")
	}
}

func TestPoolAtMinimumEndsEvenACommittedDrain(t *testing.T) {
	// The arm that must not move with the scale-up checks, asserted here
	// because this is where abandoning means something: the node is uncordoned
	// and backed off.
	//
	// At the floor the autoscaler will never remove this node, so finishing the
	// drain would strand an empty cordoned node until removalTimeout abandoned
	// it anyway — after more pods had been bounced, not fewer. That is what
	// makes it soundness rather than preference, and it is the reason the
	// narrowing above is written as one case of eligibility's switch and not as
	// a blanket rule about resuming.
	s, cfg := committedDrain()
	s.Autoscaler.Groups[0].MinSize = 3
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", cfg, drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if !step.Done || !step.Failed || step.Code != engine.SkipPoolAtMinimum {
		t.Errorf("expected the drain abandoned as %q, got %+v", engine.SkipPoolAtMinimum, step)
	}
	if nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the node was left cordoned in a pool nothing will shrink further")
	}
}

func TestADrainFinishesThroughAScaleUpThatOutlastsIt(t *testing.T) {
	// A single evaluation surviving a scale-up proves nothing about a drain,
	// because a drain is not a single evaluation: it is one eviction an
	// interval for as long as the node has pods, and the scale-up check is
	// re-asked on every one of them. A fifteen-pod node spends a quarter of an
	// hour exposed, and the cluster only has to grow once in that time.
	//
	// The scale-up starts after the first eviction, which is also the pair
	// property in one test: round zero is uncommitted and would rightly stop,
	// and every round after it has pods on other nodes that this drain put
	// there.
	var pods []*corev1.Pod
	for i := range 15 {
		pods = append(pods, mother.Pod("default", fmt.Sprintf("p%02d", i), mother.OnNode("a")))
	}
	s := snapshot([]*corev1.Node{marked("a", time.Minute, 0, ""), node("b")}, pods)
	c := clientFor(s)

	round, step := drainRounds(t, c, 3*len(pods), func(pod string, now time.Time) {
		moved := mother.Pod("default", pod+"-r", mother.OnNode("b"),
			mother.ControlledBy("ReplicaSet", pod+"-rs"))
		moved.CreationTimestamp.Time = now
		if err := c.Create(context.Background(), moved); err != nil {
			t.Fatalf("relocating %s: %v", pod, err)
		}
	}, func(round *engine.Snapshot) {
		if round.Now.After(at) {
			round.Autoscaler.ScaleUpInProgress = true
		}
	})

	if step.Failed {
		t.Fatalf("a drain in a growing cluster was abandoned at +%dm: %+v", round, step)
	}
	// One pod an evaluation, so the fifteenth evaluation is the one that finds
	// the node empty — the same round it reaches without a scale-up at all.
	if round != len(pods) || step.Code != executor.StepAwaitRemoval {
		t.Errorf("ended at +%dm with %+v; expected an empty node at +%dm",
			round, step, len(pods))
	}
}

func TestAReplacementRescheduledOntoTheDrainingNodeIsNotLanded(t *testing.T) {
	// Bound is not the same question as moved, and binpack was asking the
	// wrong one. kube-scheduler admits a pod onto a cordoned node when the pod
	// tolerates node.kubernetes.io/unschedulable:NoSchedule, so a workload
	// carrying a blanket toleration can be evicted and placed straight back
	// where it came from. Counting that as a landed replacement reads a pod
	// that never left as a successful relocation, and evicts the next one on
	// the strength of it — for ever, since the same thing happens to that one.
	//
	// The assertion is that nothing was evicted rather than that any
	// particular step was returned: more than one answer is defensible here,
	// and today it is the bounded wait the assessment already governs.
	returned := mother.Pod("default", "returned",
		mother.OnNode("a"),
		mother.ControlledBy("ReplicaSet", "web-rs"),
		mother.ToleratingEverything())
	returned.CreationTimestamp.Time = at.Add(-30 * time.Second)

	pods := []*corev1.Pod{mother.Pod("default", "stayer", mother.OnNode("a")), returned}
	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")}, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if step.Code == executor.StepEvicted {
		t.Errorf("a replacement that came back to the draining node counted as a relocation: %+v",
			step)
	}

	var left corev1.PodList
	if err := c.List(context.Background(), &left); err != nil {
		t.Fatalf("listing pods: %v", err)
	}
	if len(left.Items) != 2 {
		t.Errorf("a second pod was evicted for a replacement that never left the node: "+
			"%d of 2 remain", len(left.Items))
	}
}

func TestAnEvictionRecordsWhatItIsWaitingFor(t *testing.T) {
	pods := []*corev1.Pod{mother.Pod("default", "one", mother.OnNode("a"))}
	s := snapshot([]*corev1.Node{marked("a", time.Minute, time.Minute, "1"), node("b")}, pods)
	c := clientFor(s)

	if _, err := executor.Advance(
		context.Background(), c, s, "a", engineConfig(), drainPolicy()); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	got := nodeFrom(t, c, "a").Annotations[engine.AnnotationDrainAwaiting]
	if !strings.HasPrefix(got, string(mother.OwnerUID("one-rs"))+"@") {
		t.Errorf("the drain did not record the controller it is waiting on: %q", got)
	}
}

func TestABarePodEndsTheDrainBeforeAnythingIsEvicted(t *testing.T) {
	// Nothing would recreate it, so binpack cannot say what its replacement
	// would request — and refuses the node rather than guessing. Worth pinning
	// because it is why the drain protocol never has to wait for a replacement
	// that will never exist.
	pods := []*corev1.Pod{mother.Pod("default", "orphan", mother.OnNode("a"), mother.Bare())}
	s := snapshot([]*corev1.Node{marked("a", time.Minute, time.Minute, "1"), node("b")}, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !step.Failed || !strings.Contains(step.Reason, "orphan") {
		t.Errorf("expected the drain refused, naming the pod: %+v", step)
	}

	var left corev1.PodList
	if err := c.List(context.Background(), &left); err != nil {
		t.Fatalf("listing pods: %v", err)
	}
	if len(left.Items) != 1 {
		t.Error("a pod was evicted from a node binpack could not account for")
	}
}

func TestAReplacementTheSchedulerRefusedEndsTheDrain(t *testing.T) {
	// binpack does not place pods and cannot steer the scheduler. When the
	// replacement comes back Pending the prediction was wrong, and naming the
	// pod beats waiting for the stall timeout to report "no progress".
	// Uncordoning is also the repair: this node is where that pod can go.
	pods := []*corev1.Pod{
		mother.Pod("default", "stayer", mother.OnNode("a")),
		pending("orphan", "web-rs", at.Add(-30*time.Second), true),
	}
	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")}, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if step.Code != drain.AbandonUnschedulable || !step.Failed {
		t.Errorf("expected the drain abandoned, got %+v", step)
	}
	if !strings.Contains(step.Reason, "default/orphan") {
		t.Errorf("the reason should name the pod: %q", step.Reason)
	}
	if nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the node was left cordoned")
	}
}

func TestAReplacementTheSchedulerErroredOnIsNotRefused(t *testing.T) {
	// Only one reason is the scheduler's verdict on this pod. PodScheduled
	// goes False with reason SchedulerError whenever the scheduling cycle
	// failed for any reason that is not a rejection — kubernetes
	// pkg/scheduler/schedule_one.go, handleSchedulingFailure, which picks
	// Unschedulable only when the framework status IsRejected and
	// SchedulerError otherwise. Reading that as a refusal abandons a drain
	// that was progressing, uncordons, and puts the node behind an
	// exponential backoff over a condition the next scheduling cycle clears.
	errored := pending("replacement", "web-rs", at.Add(-30*time.Second), true)
	errored.Status.Conditions[0].Reason = corev1.PodReasonSchedulerError

	pods := []*corev1.Pod{mother.Pod("default", "stayer", mother.OnNode("a")), errored}
	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")}, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if step.Code == drain.AbandonUnschedulable || step.Failed {
		t.Errorf("abandoned a drain over a scheduler error rather than a refusal: %+v", step)
	}
	if !nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("uncordoned a node whose drain is still in flight")
	}
}

func TestOneRefusedReplacementEndsTheDrainWhateverItsSiblingsDid(t *testing.T) {
	// A controller can produce several pods after an eviction — a rollout, a
	// scale-up — and one landing does not make another one's refusal go away.
	// Listed with the bound pod first, so an implementation that returned on
	// the first match it happened to see would call this healthy.
	bound := pending("landed", "web-rs", at.Add(-30*time.Second), false)
	bound.Spec.NodeName = "b"

	pods := []*corev1.Pod{
		mother.Pod("default", "stayer", mother.OnNode("a")),
		bound,
		pending("refused", "web-rs", at.Add(-20*time.Second), true),
	}
	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")}, pods)

	step, err := executor.Advance(context.Background(), clientFor(s), s, "a",
		engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if step.Code != drain.AbandonUnschedulable {
		t.Errorf("expected the drain abandoned, got %+v", step)
	}
	if !strings.Contains(step.Reason, "default/refused") {
		t.Errorf("the reason should name the pod that could not be placed: %q", step.Reason)
	}
}

func TestAJustAdmittedReplacementIsWaitedForRatherThanAbandoned(t *testing.T) {
	// The window the gate detection must not fire in. A gating controller
	// admits a pod by removing spec.schedulingGates, and the API server keeps
	// the old status wholesale on a spec update — PrepareForUpdate is
	// `newPod.Status = oldPod.Status` — so the {PodScheduled: False, reason:
	// SchedulingGated} condition it stamped at CREATE stays standing until the
	// scheduler gets round to the pod and writes a new one.
	//
	// An evaluation landing in that window sees a healthy replacement about to
	// be placed. Reading the condition would abandon the drain and uncordon
	// at the exact moment it was about to succeed, which is worse than the
	// wait this detection replaced: the wait ended correctly, just late.
	admitted := pending("replacement", "web-rs", at.Add(-30*time.Second), false)
	mother.Gated("kueue.x-k8s.io/admission")(admitted)
	// What admission does, and all it does. The condition is left behind.
	admitted.Spec.SchedulingGates = nil

	pods := []*corev1.Pod{mother.Pod("default", "stayer", mother.OnNode("a")), admitted}
	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")}, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if step.Done || step.Failed {
		t.Errorf("abandoned a drain whose replacement had just been admitted: %+v", step)
	}
	if !nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("uncordoned a node whose drain is still in flight")
	}
}

func TestOneGatedReplacementEndsTheDrainWhateverItsSiblingsDid(t *testing.T) {
	// The same ordering rule as the refusal above, and it needs stating
	// separately because it is the case where the two readings disagree: a
	// controller with a bound pod newer than the eviction *and* a gated one.
	// Reading "landed" there lets the drain evict the next pod while the
	// replacement for the last one is still queued for admission — which is
	// exactly the one-in-flight rule being spent on a pod that has not
	// arrived. Listed with the bound pod first, so an implementation that
	// answered from whichever it saw first would call this healthy.
	bound := pending("landed", "web-rs", at.Add(-30*time.Second), false)
	bound.Spec.NodeName = "b"

	gated := pending("gated", "web-rs", at.Add(-20*time.Second), false)
	mother.Gated("kueue.x-k8s.io/admission")(gated)

	pods := []*corev1.Pod{mother.Pod("default", "stayer", mother.OnNode("a")), bound, gated}
	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")}, pods)

	step, err := executor.Advance(context.Background(), clientFor(s), s, "a",
		engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if step.Code != drain.AbandonUnschedulable || !step.Failed {
		t.Errorf("expected the drain abandoned, got %+v", step)
	}
	if !strings.Contains(step.Reason, "default/gated") {
		t.Errorf("the reason should name the pod nothing will place: %q", step.Reason)
	}
}

func TestSomebodyElsesUnschedulablePodDoesNotEndTheDrain(t *testing.T) {
	// A workload deployed with impossible requests, while this drain happens
	// to be running. Correlating by controller rather than by time alone is
	// what keeps binpack from abandoning a healthy drain and claiming, wrongly,
	// that the pod had moved off this node.
	replacement := pending("replacement", "web-rs", at.Add(-30*time.Second), false)
	replacement.Spec.NodeName = "b"

	pods := []*corev1.Pod{
		mother.Pod("default", "stayer", mother.OnNode("a")),
		replacement,
		pending("someone-elses", "batch-rs", at.Add(-10*time.Second), true),
	}
	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")}, pods)

	step, err := executor.Advance(context.Background(), clientFor(s), s, "a",
		engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if step.Failed {
		t.Errorf("an unrelated workload ended the drain: %+v", step)
	}
}

func TestADrainWaitingForAReplacementThatNeverArrivesIsAbandoned(t *testing.T) {
	// Having a controller does not guarantee the controller produces a bound
	// pod carrying that same UID. A rollout or a scale-down supersedes the
	// ReplicaSet, so every later pod is owned by a different one; a Job at its
	// backoffLimit creates nothing at all. The wait then has nothing to end it
	// unless the drain assessment gets to run, and while it lasts binpack
	// evaluates no other node in the cluster.
	pods := []*corev1.Pod{mother.Pod("default", "stayer", mother.OnNode("a"))}
	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", 30*time.Minute), node("b")}, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if !step.Failed || step.Code != drain.AbandonStalled {
		t.Errorf("expected the stall bound to end the wait, got %+v", step)
	}
	if nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the node was left cordoned waiting for a pod that will never arrive")
	}
}

func TestAWaitForAReplacementCannotBeExtendedForEver(t *testing.T) {
	// The single-shot case above only reaches the stall bound because its
	// recorded count already matches the node's. A real drain never looks like
	// that: record runs before the eviction it accompanies, so the count on the
	// node is one higher than what the next evaluation finds. That evaluation
	// reads the departure as progress — rightly, once — and if the wait then
	// returns without lowering the count, every later evaluation reads the same
	// departure again. A stall clock that restarts every interval is not a
	// bound, and the node is wedged exactly as it was before.
	n := awaitingNode("a", "web-rs", time.Minute)
	n.Annotations[engine.AnnotationDrainPodsRemaining] = "2"

	pods := []*corev1.Pod{mother.Pod("default", "stayer", mother.OnNode("a"))}
	s := snapshot([]*corev1.Node{n, node("b")}, pods)
	c := clientFor(s)

	// One evaluation a minute for twice the stall timeout, each against the
	// node as the last one left it.
	const rounds = 20
	for i := 0; i <= rounds; i++ {
		round := s
		round.Now = at.Add(time.Duration(i) * time.Minute)
		round.Nodes = []*corev1.Node{nodeFrom(t, c, "a"), node("b")}
		round.Autoscaler.LastProbe = round.Now.Add(-10 * time.Second)

		step, err := executor.Advance(
			context.Background(), c, round, "a", engineConfig(), drainPolicy())
		if err != nil {
			t.Fatalf("Advance at +%dm: %v", i, err)
		}
		if step.Failed && step.Code == drain.AbandonStalled {
			// At the eleventh minute, not the first: the pod that left at +0
			// was real progress, so the stall clock runs from there rather
			// than from the eviction that started the wait. Bounding the
			// absence of progress is the whole of ADR-0007, and a bound that
			// fired at +1 would be a wall-clock deadline wearing its name.
			if i != 11 {
				t.Errorf("abandoned at +%dm; expected +11m, one minute past the "+
					"stall timeout measured from the last real progress", i)
			}
			return
		}
		if step.Code != executor.StepWaiting {
			t.Fatalf("at +%dm: expected the drain to wait or to end, got %+v", i, step)
		}
	}

	t.Errorf("%d evaluations over twice the stall timeout and the drain is still waiting; "+
		"the replacement never arrived and nothing ended it", rounds+1)
}

// returning builds a pod of a workload that tolerates every taint, the cordon's
// included — so the scheduler is free to put its replacement straight back onto
// the node being drained.
func returning(name, owner string, created time.Time) *corev1.Pod {
	p := mother.Pod("default", name,
		mother.OnNode("a"),
		mother.ControlledBy("ReplicaSet", owner),
		mother.ToleratingEverything())
	p.CreationTimestamp.Time = created
	return p
}

// cluster rebuilds the snapshot from the cluster as the previous round left it,
// so each evaluation reads what the last one actually wrote rather than a
// fixture frozen before the drain started.
func cluster(t *testing.T, c client.Client, now time.Time) engine.Snapshot {
	t.Helper()
	var nodes corev1.NodeList
	if err := c.List(context.Background(), &nodes); err != nil {
		t.Fatalf("listing nodes: %v", err)
	}
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods); err != nil {
		t.Fatalf("listing pods: %v", err)
	}

	ns := make([]*corev1.Node, 0, len(nodes.Items))
	for i := range nodes.Items {
		ns = append(ns, &nodes.Items[i])
	}
	ps := make([]*corev1.Pod, 0, len(pods.Items))
	for i := range pods.Items {
		ps = append(ps, &pods.Items[i])
	}

	s := snapshot(ns, ps)
	s.Now = now
	s.Autoscaler.LastProbe = now.Add(-10 * time.Second)
	return s
}

// drainRounds runs one evaluation a minute for as long as it takes, calling
// left for each pod that goes so the caller can say what the cluster does about
// it. It returns the round the drain reached an answer on and the step that
// gave it, or -1 if it never did.
//
// A loop rather than a call, and this is not stylistic. Every question about a
// bound is a question about a clock, and a clock that is reset once an interval
// looks exactly like a clock that is running if you only ever look at it once.
// The bound this file is about was shipped reachable and inert for that reason.
//
// to perturbs each round's snapshot after it is rebuilt, so a test can hold a
// cluster-wide condition true for the whole drain rather than for its first
// evaluation only. A condition that ends a drain is easy to assert once; a
// condition that must *not* end one has to be held for the drain's whole life,
// because surviving it in round zero says nothing about round nine.
func drainRounds(
	t *testing.T, c client.Client, rounds int, left func(pod string, now time.Time),
	to ...func(*engine.Snapshot),
) (int, executor.Step) {
	t.Helper()
	for i := 0; i <= rounds; i++ {
		round := cluster(t, c, at.Add(time.Duration(i)*time.Minute))
		for _, perturb := range to {
			perturb(&round)
		}
		before := map[string]bool{}
		for _, p := range round.Pods {
			before[p.Name] = true
		}

		step, err := executor.Advance(
			context.Background(), c, round, "a", engineConfig(), drainPolicy())
		if err != nil {
			t.Fatalf("Advance at +%dm: %v", i, err)
		}
		if step.Done || step.Code == executor.StepAwaitRemoval {
			return i, step
		}

		var after corev1.PodList
		if err := c.List(context.Background(), &after); err != nil {
			t.Fatalf("listing pods at +%dm: %v", i, err)
		}
		for _, p := range after.Items {
			delete(before, p.Name)
		}
		for name := range before {
			left(name, round.Now)
		}
	}
	return -1, executor.Step{}
}

func TestADrainWhosePodsKeepReturningIsEventuallyAbandoned(t *testing.T) {
	// The case above says one returning pod is not a relocation. This says what
	// has to happen when they keep returning: the node's population never
	// falls, so the drain is achieving nothing and must end.
	//
	// It did not. Every round read the returned pod as a landed replacement,
	// evicted another on the strength of it, and stamped progress on the way
	// out — so the stall clock restarted every interval and one workload's
	// lineage was evicted indefinitely, its neighbour never touched. The cost
	// is not confined to this node either: the controller short-circuits every
	// evaluation to a drain in flight, so a cluster with one wedged node
	// consolidates nowhere.
	owners := map[string]string{"w1": "w1-rs", "w2": "w2-rs"}
	pods := []*corev1.Pod{
		returning("w1", "w1-rs", at.Add(-time.Hour)),
		returning("w2", "w2-rs", at.Add(-time.Hour)),
	}
	// Three recorded against two live, because record writes the count before
	// the eviction it accompanies. That is the state every drain is in after
	// its first eviction, and a fixture where the two agree is a state no drain
	// is ever in.
	s := snapshot([]*corev1.Node{marked("a", 20*time.Minute, time.Minute, "3"), node("b")}, pods)
	c := clientFor(s)

	round, step := drainRounds(t, c, 20, func(pod string, now time.Time) {
		// The controller replaces the pod it lost, and the scheduler — which
		// admits a pod tolerating the cordon onto a cordoned node — puts the
		// replacement back where it came from.
		owner := owners[pod]
		back := returning(fmt.Sprintf("%s-%s", owner, now.Sub(at)), owner, now)
		owners[back.Name] = owner
		if err := c.Create(context.Background(), back); err != nil {
			t.Fatalf("replacing %s: %v", pod, err)
		}
	})

	// At +11m: one minute past the stall timeout, measured from the last time
	// the node's population actually fell — round zero, where three recorded
	// became two observed. Nothing after it moves a pod off the node, so the
	// clock runs from there uninterrupted. A bound that fired earlier would be
	// counting evictions rather than progress; one that never fires is the
	// defect this test exists for.
	if round != 11 || step.Code != drain.AbandonStalled {
		t.Errorf("ended at +%dm with %+v; expected the stall bound at +11m", round, step)
	}
	if nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the drain ended but left the node cordoned")
	}
}

func TestANodeThatNeverGetsEmptierEndsTheDrain(t *testing.T) {
	// The other way a node's population fails to fall, and the one the fix
	// above does not cover: every replacement does relocate, so the drain is
	// entitled to proceed to the next eviction — and pods of some other
	// tolerating workload keep arriving to take their place.
	//
	// An accepted eviction was progress in its own right, which makes the stall
	// bound unreachable here however correct it is: the eviction is an event,
	// it happens every round, and it restamps the marker every round. Progress
	// has to be read from the node's state, because state is the only kind of
	// progress that cannot be manufactured by trying.
	relocate := func(t *testing.T, c client.Client, gone string, now time.Time) {
		t.Helper()
		moved := mother.Pod("default", gone+"-r", mother.OnNode("b"),
			mother.ControlledBy("ReplicaSet", gone+"-rs"))
		moved.CreationTimestamp.Time = now
		if err := c.Create(context.Background(), moved); err != nil {
			t.Fatalf("relocating %s: %v", gone, err)
		}
	}
	arrive := func(t *testing.T, c client.Client, now time.Time, n int) {
		t.Helper()
		for k := range n {
			name := fmt.Sprintf("arrival-%d-%d", int(now.Sub(at).Minutes()), k)
			if err := c.Create(context.Background(), returning(name, name+"-rs", now)); err != nil {
				t.Fatalf("scheduling %s onto the draining node: %v", name, err)
			}
		}
	}

	for _, tc := range []struct {
		name string
		fill func(t *testing.T, c client.Client, gone string, now time.Time)
	}{
		{
			// The population holds exactly still.
			name: "one arrives for each that leaves",
			fill: func(t *testing.T, c client.Client, gone string, now time.Time) {
				relocate(t, c, gone, now)
				arrive(t, c, now, 1)
			},
		},
		{
			// The same thing less tidily, and worth its own case because it
			// defeats the obvious repair. A count that simply followed the live
			// population would read every fall back to two as a fresh
			// departure, and refresh the stall clock on it — half as often as
			// an eviction did, and just as endlessly.
			name: "they arrive in pairs and leave again",
			fill: func(t *testing.T, c client.Client, gone string, now time.Time) {
				relocate(t, c, gone, now)
				if int(now.Sub(at).Minutes())%2 == 0 {
					arrive(t, c, now, 2)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pods := []*corev1.Pod{
				returning("w1", "w1-rs", at.Add(-time.Hour)),
				returning("w2", "w2-rs", at.Add(-time.Hour)),
			}
			s := snapshot(
				[]*corev1.Node{marked("a", 20*time.Minute, time.Minute, "3"), node("b")}, pods)
			c := clientFor(s)

			round, step := drainRounds(t, c, 20, func(gone string, now time.Time) {
				tc.fill(t, c, gone, now)
			})

			if round != 11 || step.Code != drain.AbandonStalled {
				t.Errorf("ended at +%dm with %+v; expected the stall bound at +11m, since the "+
					"node has emptied by not one pod in that time", round, step)
			}
		})
	}
}

func TestADrainLongerThanTheStallTimeoutIsNotAbandonedWhileItIsWorking(t *testing.T) {
	// The other half of the bound, and the half that is easy to break while
	// tightening the first. A node with more pods on it than the stall timeout
	// has intervals takes longer than the stall timeout to drain — and must not
	// be abandoned for that, because it is working the whole time.
	//
	// What makes it work is the recorded population, and that is a baseline
	// rather than a claim: `fewer` is the live count measured against it, so a
	// drain that never records one can never detect the fall it is waiting for.
	// Recording it only once progress has been seen is circular, and the circle
	// closes on the first eviction of every drain, since a node comes out of
	// Begin carrying no count at all. This drain relocates a pod a minute and
	// every replacement lands elsewhere; nothing about it is stalled.
	var pods []*corev1.Pod
	for i := range 15 {
		pods = append(pods, mother.Pod("default", fmt.Sprintf("p%02d", i), mother.OnNode("a")))
	}
	// No pods-remaining marker, because Begin writes none: it records when the
	// drain started and that it is progressing, and the count arrives with the
	// first evaluation that looks at the node.
	s := snapshot([]*corev1.Node{marked("a", time.Minute, 0, ""), node("b")}, pods)
	c := clientFor(s)

	round, step := drainRounds(t, c, 3*len(pods), func(pod string, now time.Time) {
		// Scheduled somewhere else, which is what a relocation is, and gone
		// from this node inside the interval — the ordinary case, since
		// terminationGracePeriodSeconds defaults to well under a minute.
		moved := mother.Pod("default", pod+"-r", mother.OnNode("b"),
			mother.ControlledBy("ReplicaSet", pod+"-rs"))
		moved.CreationTimestamp.Time = now
		if err := c.Create(context.Background(), moved); err != nil {
			t.Fatalf("relocating %s: %v", pod, err)
		}
	})

	if step.Failed {
		t.Fatalf("a drain that had relocated a pod a minute was abandoned at +%dm: %+v",
			round, step)
	}
	// One pod an evaluation, so the fifteenth evaluation is the one that finds
	// the node empty.
	if round != len(pods) || step.Code != executor.StepAwaitRemoval {
		t.Errorf("ended at +%dm with %+v; expected an empty node at +%dm",
			round, step, len(pods))
	}
}

func TestACompletedPodIsNotAnInFlightEviction(t *testing.T) {
	// A Succeeded pod held by a finalizer is not occupying the node, so the
	// assessment does not count it. Treating it as in flight would wait on it
	// every evaluation while evictable pods sat there, until the drain was
	// abandoned as stalled.
	done := mother.Pod("default", "finished", mother.OnNode("a"),
		mother.Terminating(at.Add(-time.Hour), time.Minute), mother.Succeeded())

	pods := []*corev1.Pod{done, mother.Pod("default", "movable", mother.OnNode("a"))}
	s := snapshot([]*corev1.Node{marked("a", time.Minute, time.Minute, "2"), node("b")}, pods)

	step, err := executor.Advance(context.Background(), clientFor(s), s, "a",
		engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if step.Code != executor.StepEvicted {
		t.Errorf("expected the evictable pod to be evicted, got %+v", step)
	}
}

func TestAnEmptyNodeWaitsForTheAutoscaler(t *testing.T) {
	s := snapshot([]*corev1.Node{marked("a", 5*time.Minute, time.Minute, "1"), node("b")}, nil)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if step.Code != executor.StepAwaitRemoval || step.Done {
		t.Errorf("expected the drain to await removal, got %+v", step)
	}
	// One pod became zero, which is progress, so the markers move.
	if got := nodeFrom(t, c, "a").Annotations[engine.AnnotationDrainPodsRemaining]; got != "0" {
		t.Errorf("pods-remaining: got %q, want 0", got)
	}
}

func TestTheProgressMarkerDoesNotMoveWithoutProgress(t *testing.T) {
	// The removal clock runs from the last progress, so refreshing the marker
	// on an evaluation that observed nothing would push the deadline out by an
	// interval every interval, and removalTimeout would never fire.
	//
	// An empty node already recorded as empty is that case: nothing left to
	// move, nothing shutting down, and the autoscaler yet to act.
	stamp := at.Add(-5 * time.Minute).Format(time.RFC3339)
	s := snapshot([]*corev1.Node{marked("a", 20*time.Minute, 5*time.Minute, "0"), node("b")}, nil)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if step.Code != executor.StepAwaitRemoval {
		t.Fatalf("expected the drain to await removal, got %+v", step)
	}

	if got := nodeFrom(t, c, "a").Annotations[engine.AnnotationDrainProgress]; got != stamp {
		t.Errorf("the progress marker moved without progress: got %q, want %q", got, stamp)
	}
}

// recorder captures the patches a call makes, in order, so a test can assert
// on sequence rather than only on the final state.
type recorder struct {
	executor.Writer
	patches []string
}

func (r *recorder) Patch(
	ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption,
) error {
	data, err := patch.Data(obj)
	if err != nil {
		return err
	}
	r.patches = append(r.patches, string(data))
	return r.Writer.Patch(ctx, obj, patch, opts...)
}

func TestTheDrainingLabelAppearsAndGoesWithTheMarkers(t *testing.T) {
	// `kubectl get nodes` shows a cordoned node as SchedulingDisabled and says
	// nothing about who did it. The label says binpack — and it has to go when
	// the drain does, or it becomes a thing an operator learns not to trust.
	n := node("a")
	c := clientFor(snapshot([]*corev1.Node{n, node("b")}, nil))

	if err := executor.Begin(context.Background(), c, n, at); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if got := nodeFrom(t, c, "a").Labels[engine.LabelDraining]; got != "true" {
		t.Errorf("the draining label was not set: %q", got)
	}

	if _, err := executor.Abandon(
		context.Background(), c, n, "test", "because", at, drainPolicy()); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	after := nodeFrom(t, c, "a")
	if _, present := after.Labels[engine.LabelDraining]; present {
		t.Errorf("the draining label outlived the drain: %v", after.Labels)
	}
	if after.Annotations[engine.AnnotationDrainStarted] != "" {
		t.Error("the markers outlived the drain")
	}
}

func TestTheLabelAndTheMarkersMoveInOneWrite(t *testing.T) {
	// Separately would let a node exist marked-but-unlabelled, or the reverse,
	// for as long as it takes the second write to land — or forever, if it
	// fails. A merge patch on metadata carries both, so there is no reason to
	// take that risk.
	n := node("a")
	c := clientFor(snapshot([]*corev1.Node{n, node("b")}, nil))
	rec := &recorder{Writer: c}

	if err := executor.Begin(context.Background(), rec, n, at); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	if len(rec.patches) != 2 {
		t.Fatalf("expected one metadata patch and one cordon, got %d", len(rec.patches))
	}
	if !strings.Contains(rec.patches[0], engine.LabelDraining) ||
		!strings.Contains(rec.patches[0], engine.AnnotationDrainStarted) {
		t.Errorf("the label and the marker were not written together: %s", rec.patches[0])
	}
}

func TestOtherLabelsAreLeftAlone(t *testing.T) {
	// A node carries labels from its provider and from anything else in the
	// cluster. Clobbering those would be its own incident.
	n := node("a")
	c := clientFor(snapshot([]*corev1.Node{n, node("b")}, nil))

	if err := executor.Begin(context.Background(), c, n, at); err != nil {
		t.Fatalf("Begin: %v", err)
	}
	if _, err := executor.Abandon(
		context.Background(), c, n, "test", "because", at, drainPolicy()); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	if got := nodeFrom(t, c, "a").Labels["pool-label"]; got != poolName {
		t.Errorf("a label binpack does not own was lost: %q", got)
	}
}

// beingRemoved adds the taint the cluster-autoscaler sets once it has
// committed to deleting a node.
func beingRemoved() mother.NodeOption {
	return mother.Tainted(engine.TaintToBeDeleted, "1786971071", corev1.TaintEffectNoSchedule)
}

func TestADrainTheAutoscalerHasTakenOverIsNotAbandoned(t *testing.T) {
	// Abandoning uncordons, and uncordoning a node another component is
	// actively deleting is two controllers disagreeing about whether it
	// accepts pods. The drain is also, in every sense that matters, going
	// fine — so there is nothing to abandon.
	n := marked("a", time.Hour, time.Hour, "3")
	n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{
		Key: engine.TaintToBeDeleted, Effect: corev1.TaintEffectNoSchedule})

	pods := []*corev1.Pod{mother.Pod("default", "still-here", mother.OnNode("a"))}
	s := snapshot([]*corev1.Node{n, node("b")}, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if step.Code != executor.StepHandedOver {
		t.Errorf("got %+v, want the drain handed over", step)
	}
	if step.Failed || step.Done {
		t.Error("a handover is neither a failure nor the end of the drain")
	}

	after := nodeFrom(t, c, "a")
	if !after.Spec.Unschedulable {
		t.Error("binpack uncordoned a node the autoscaler is deleting")
	}
	if after.Annotations[engine.AnnotationDrainStarted] == "" {
		t.Error("the markers were cleared, so the completion will go unrecorded")
	}
	if after.Annotations[engine.AnnotationBackoffUntil] != "" {
		t.Error("a handover was recorded as a failed drain")
	}

	var left corev1.PodList
	if err := c.List(context.Background(), &left); err != nil {
		t.Fatalf("listing pods: %v", err)
	}
	if len(left.Items) != 1 {
		t.Error("binpack evicted alongside the autoscaler")
	}
}

func TestAHandoverKeepsTheStallClockFromRunning(t *testing.T) {
	// If the autoscaler changes its mind and drops its taint, the drain comes
	// back to binpack. Without this it would come back with a stall clock that
	// had been running throughout, and be abandoned on the spot.
	n := marked("a", time.Hour, time.Hour, "3")
	n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{
		Key: engine.TaintToBeDeleted, Effect: corev1.TaintEffectNoSchedule})
	s := snapshot([]*corev1.Node{n, node("b")}, nil)
	c := clientFor(s)

	if _, err := executor.Advance(
		context.Background(), c, s, "a", engineConfig(), drainPolicy()); err != nil {
		t.Fatalf("Advance: %v", err)
	}

	got := nodeFrom(t, c, "a").Annotations[engine.AnnotationDrainProgress]
	if got != at.UTC().Format(time.RFC3339) {
		t.Errorf("progress was not recorded during the handover: %q", got)
	}
}

func TestANodeTheAutoscalerIsRemovingIsNeverChosen(t *testing.T) {
	// It is not necessarily cordoned — the autoscaler uses a taint — so the
	// cordon check does not cover this. Choosing it would start a drain of a
	// node that is already going away.
	s := snapshot([]*corev1.Node{node("a", beingRemoved()), node("b"), node("c")}, nil)

	d := engine.Decide(s, engineConfig())

	for _, a := range d.Assessments {
		if a.Node.Name != "a" {
			continue
		}
		if !a.Skipped || a.SkipCode != engine.SkipBeingRemoved {
			t.Errorf("got skipped=%v code=%q, want %q", a.Skipped, a.SkipCode, engine.SkipBeingRemoved)
		}
	}
	if d.Node != nil && d.Node.Name == "a" {
		t.Error("binpack chose a node the autoscaler is already removing")
	}
}

func TestASoftDeletionCandidateDoesNotStopADrain(t *testing.T) {
	// PreferNoSchedule, and it means the autoscaler considers the node
	// unneeded — an opinion, not a decision. Treating it as a handover would
	// stall every drain binpack starts, since a node it is emptying is exactly
	// one the autoscaler starts calling unneeded.
	n := marked("a", time.Minute, time.Minute, "1")
	n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{
		Key: engine.TaintDeletionCandidate, Effect: corev1.TaintEffectPreferNoSchedule})

	pods := []*corev1.Pod{mother.Pod("default", "movable", mother.OnNode("a"))}
	s := snapshot([]*corev1.Node{n, node("b")}, pods)

	step, err := executor.Advance(context.Background(), clientFor(s), s, "a",
		engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if step.Code != executor.StepEvicted {
		t.Errorf("got %+v, want the drain to carry on", step)
	}
}

func TestTheHandoverSurvivesEveryOtherReasonToStop(t *testing.T) {
	// eligibility reports one reason, and a node can satisfy several. The
	// first of these is not a corner case: the autoscaler deleting a node is
	// frequently what brings the pool to its minimum, so a handover that lost
	// to pool-at-minimum would uncordon the node mid-deletion nearly every
	// time it mattered.
	for _, tc := range []struct {
		name string
		to   func(*engine.Snapshot)
	}{
		{"the pool reached its minimum, because of this very deletion",
			func(s *engine.Snapshot) { s.Autoscaler.Groups[0].MinSize = 2 }},
		{"a scale-up began elsewhere",
			func(s *engine.Snapshot) { s.Autoscaler.ScaleUpInProgress = true }},
		{"an operator annotated the node",
			func(s *engine.Snapshot) {
				s.Nodes[0].Annotations[engine.AnnotationSkip] = "true"
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := marked("a", time.Hour, time.Hour, "1")
			n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{
				Key: engine.TaintToBeDeleted, Effect: corev1.TaintEffectNoSchedule})

			s := snapshot([]*corev1.Node{n, node("b")},
				[]*corev1.Pod{mother.Pod("default", "left", mother.OnNode("a"))})
			tc.to(&s)
			c := clientFor(s)

			step, err := executor.Advance(
				context.Background(), c, s, "a", engineConfig(), drainPolicy())
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}

			if step.Code != executor.StepHandedOver {
				t.Errorf("got %+v, want the handover to win", step)
			}
			if nodeFrom(t, c, "a").Spec.Unschedulable == false {
				t.Error("binpack uncordoned a node the autoscaler is deleting")
			}
		})
	}
}

func TestAdvanceAbandonsAHandOverTheAutoscalerNeverFinishes(t *testing.T) {
	// The taint is only ever cleared by a live autoscaler — on a failed
	// scale-down, on the batch rollback, or by cleanUpIfRequired at process
	// start. So an autoscaler that stops between tainting the node and
	// deleting it leaves a taint nothing in the cluster will remove, and
	// binpack refreshing the progress marker against it for ever leaves the
	// node cordoned and stops consolidation everywhere else.
	n := marked("a", 2*time.Hour, 2*time.Hour, "1")
	// Valued as the autoscaler values it: the Unix second at which it
	// committed to the deletion.
	n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{
		Key:    engine.TaintToBeDeleted,
		Value:  strconv.FormatInt(at.Add(-2*time.Hour).Unix(), 10),
		Effect: corev1.TaintEffectNoSchedule,
	})

	s := snapshot([]*corev1.Node{n, node("b")},
		[]*corev1.Pod{mother.Pod("default", "left", mother.OnNode("a"))})
	// Still claiming to be running — a status ConfigMap outlives the process
	// that wrote it — but thirty minutes since it last completed a scan,
	// against a five-minute MaxStatusAge.
	s.Autoscaler.LastProbe = at.Add(-30 * time.Minute)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if !step.Done || !step.Failed {
		t.Errorf("got %+v, want the hand-over ended and recorded as a failure", step)
	}
	// The reason revalidation already computed, rather than a code invented
	// here: what ended this drain is that nothing would remove the node, which
	// is the same fact under the same name whether a drain was in flight or
	// not.
	if step.Code != engine.SkipAutoscalerNotLive {
		t.Errorf("abandon code = %q, want %q", step.Code, engine.SkipAutoscalerNotLive)
	}

	after := nodeFrom(t, c, "a")
	if after.Spec.Unschedulable {
		t.Error("the node is still cordoned, so the capacity is still paid for and unusable")
	}
	if after.Annotations[engine.AnnotationDrainStarted] != "" {
		t.Error("the drain markers survived, so binpack will short-circuit to this node again")
	}
	if after.Annotations[engine.AnnotationBackoffUntil] == "" {
		t.Error("no backoff was recorded, so the node is the emptiest candidate again immediately")
	}
}

func TestALiveHandOverIsWaitedForHoweverLongItTakes(t *testing.T) {
	// The other side of the bound, and it needs a loop: a hand-over is
	// refreshed once an interval, and a clock that is reset once an interval
	// looks exactly like a clock that is running if you only ever look once.
	// A bound that read the taint's age, or any other elapsed time, would end
	// this drain part-way through — and uncordoning a node the autoscaler is
	// genuinely mid-delete is the one thing the hand-over exists to prevent.
	n := marked("a", time.Hour, time.Hour, "1")
	n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{
		Key:    engine.TaintToBeDeleted,
		Value:  strconv.FormatInt(at.Add(-time.Hour).Unix(), 10),
		Effect: corev1.TaintEffectNoSchedule,
	})

	s := snapshot([]*corev1.Node{n, node("b")},
		[]*corev1.Pod{mother.Pod("default", "left", mother.OnNode("a"))})
	c := clientFor(s)

	// Well past both the stall and the removal timeout, with the autoscaler
	// reporting in throughout.
	round, step := drainRounds(t, c, 40, func(pod string, _ time.Time) {
		t.Errorf("binpack evicted %s alongside the autoscaler", pod)
	})
	if round != -1 {
		t.Fatalf("the drain ended at +%dm with %+v; a live autoscaler is waited for", round, step)
	}

	after := nodeFrom(t, c, "a")
	if !after.Spec.Unschedulable {
		t.Error("binpack uncordoned a node the autoscaler is deleting")
	}
	if after.Annotations[engine.AnnotationBackoffUntil] != "" {
		t.Error("a hand-over in progress was recorded as a failed drain")
	}
}

func TestAHandOverForAPoolTheAutoscalerNoLongerManagesIsEnded(t *testing.T) {
	// The other way a hand-over is never finished, and this one survives a
	// restart: the autoscaler's start-up clean-up only visits nodes in the
	// groups it currently manages, so a node whose pool has left that set —
	// deleted, dropped by auto-discovery, or failing a provider lookup — keeps
	// its taint however healthy the autoscaler is. Its status is fresh
	// throughout, which is why the bound cannot be freshness alone.
	n := marked("a", time.Hour, time.Hour, "1")
	n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{
		Key:    engine.TaintToBeDeleted,
		Value:  strconv.FormatInt(at.Add(-time.Hour).Unix(), 10),
		Effect: corev1.TaintEffectNoSchedule,
	})

	s := snapshot([]*corev1.Node{n, node("b")},
		[]*corev1.Pod{mother.Pod("default", "left", mother.OnNode("a"))})
	s.Autoscaler.Groups = []engine.NodeGroup{
		{ID: "a-pool-that-is-not-this-one", MinSize: 1, MaxSize: 10, Ready: 2},
	}
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if !step.Done || !step.Failed {
		t.Errorf("got %+v, want the hand-over ended and recorded as a failure", step)
	}
	if step.Code != engine.SkipNotAutoscaled {
		t.Errorf("abandon code = %q, want %q", step.Code, engine.SkipNotAutoscaled)
	}
	if nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the node is still cordoned, waiting on an autoscaler that does not manage it")
	}
}

// expendable is a pod the autoscaler would terminate without ceremony, and
// which binpack therefore simulates no destination for. Its controller is the
// load-bearing part: a bare pod is refused before anything is evicted, so the
// only expendable pod that reaches an eviction is one something will recreate.
func expendable(name string, opts ...mother.PodOption) *corev1.Pod {
	return mother.Pod("default", name,
		append([]mother.PodOption{mother.OnNode("a"), mother.Priority(-100)}, opts...)...)
}

// consolidating is engineConfig with the autoscaler's own expendable cutoff
// stated rather than left at the zero value, so the fixtures below say which
// rule they are testing.
func consolidating() engine.Config {
	cfg := engineConfig()
	cfg.Default.Sim.ExpendablePriorityCutoff = -10
	return cfg
}

func TestAnExpendablePodsEvictionRecordsNoReplacementWait(t *testing.T) {
	// An expendable pod needs no destination — that is the whole of what the
	// class means, and Simulate places none for it. Recording a wait for its
	// replacement adds exactly the thing the contract says binpack adds
	// nothing of: the drain now requires a pod that was deliberately never
	// placed to be placeable, and the autoscaler will not scale up for a
	// sub-cutoff pod, so nothing ever resolves it.
	pods := []*corev1.Pod{expendable("filler")}
	s := snapshot([]*corev1.Node{marked("a", time.Minute, time.Minute, "1"), node("b")}, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", consolidating(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if step.Code != executor.StepEvicted {
		t.Fatalf("expected the expendable pod evicted, got %+v", step)
	}

	// Settled, not absent. The marker's presence is the only record that this
	// drain has begun, so omitting the write leaves the previous eviction's
	// marker standing and clearing it re-arms the preferences ADR-0009 stops
	// asking once pods have moved.
	got := nodeFrom(t, c, "a").Annotations[engine.AnnotationDrainAwaiting]
	if got != engine.AwaitingSettled {
		t.Errorf("the drain recorded a wait for a pod it never placed: %q", got)
	}
}

func TestAnUnschedulableExpendableReplacementDoesNotEndTheDrain(t *testing.T) {
	// The harm in full, and it lands at the worst possible moment. nextToEvict
	// takes the relocatable pods first, so the expendable one is the last
	// eviction of the drain — by which point every pod that needed a
	// destination has already been given one. Its replacement then comes back
	// Unschedulable, which is an ordinary outcome in a cluster binpack has
	// just tightened by a node's worth of pods, and binpack throws away a
	// drain that had finished: node uncordoned, thirty minutes of backoff, and
	// a replacement-unschedulable abandonment counted against an operation
	// that succeeded.
	pods := []*corev1.Pod{
		mother.Pod("default", "web", mother.OnNode("a")),
		expendable("filler"),
	}
	s := snapshot([]*corev1.Node{marked("a", time.Minute, 0, ""), node("b")}, pods)
	c := clientFor(s)

	round, step := drainRounds(t, c, 6, func(pod string, now time.Time) {
		// The relocatable pod's replacement lands where the simulation said it
		// would; the expendable one's cannot be placed, because nothing
		// reserved it room and nothing will make any.
		replacement := pending(pod+"-2", pod+"-rs", now, pod == "filler")
		if pod != "filler" {
			replacement.Spec.NodeName = "b"
		}
		if err := c.Create(context.Background(), replacement); err != nil {
			t.Fatalf("replacing %s: %v", pod, err)
		}
	})

	if step.Failed {
		t.Fatalf("a finished drain was abandoned at +%dm: %+v", round, step)
	}
	if step.Code != executor.StepAwaitRemoval {
		t.Errorf("ended at +%dm with %+v; expected the emptied node awaiting removal", round, step)
	}
	if !nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the node was uncordoned, so the cluster-autoscaler will not remove it")
	}
}

func TestAnUnrelatedUnschedulablePodDoesNotAbandonAnEmptiedNode(t *testing.T) {
	// The marker means "this drain owes one replacement", but its lifetime was
	// "until the next eviction" — and on an emptied node there is no next
	// eviction. So it stays live, and every later evaluation re-asks the same
	// question of the whole pod list. Any new pod of that ReplicaSet the
	// scheduler cannot place then abandons the drain and uncordons a node the
	// autoscaler was about to reap, recording a failure that names a pod which
	// never ran there.
	//
	// Reached in three rounds rather than built by hand, because the defect is
	// in what the second round leaves behind: a fixture that wrote the marker
	// itself would be asserting the test's own idea of the state.
	pods := []*corev1.Pod{mother.Pod("default", "web", mother.OnNode("a"))}
	s := snapshot([]*corev1.Node{marked("a", time.Minute, 0, ""), node("b")}, pods)
	c := clientFor(s)

	round, step := drainRounds(t, c, 6, func(pod string, now time.Time) {
		moved := pending(pod+"-2", pod+"-rs", now, false)
		moved.Spec.NodeName = "b"
		if err := c.Create(context.Background(), moved); err != nil {
			t.Fatalf("relocating %s: %v", pod, err)
		}
	})
	if step.Code != executor.StepAwaitRemoval {
		t.Fatalf("the drain did not reach an emptied node: +%dm %+v", round, step)
	}

	// An HPA scale-up, a rollout, a replica recreated after a node failure —
	// during a period when the cluster is by construction near-full. Nothing
	// to do with this drain, and not on this node.
	if err := c.Create(context.Background(),
		pending("scaled-up", "web-rs", at.Add(time.Duration(round+1)*time.Minute), true)); err != nil {
		t.Fatalf("scaling up: %v", err)
	}

	after := cluster(t, c, at.Add(time.Duration(round+1)*time.Minute))
	step, err := executor.Advance(
		context.Background(), c, after, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if step.Failed || step.Code != executor.StepAwaitRemoval {
		t.Errorf("an unrelated pod ended a drain that had emptied its node: %+v", step)
	}
	if !nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the node was uncordoned while the cluster-autoscaler was removing it")
	}
}

func TestAnExpendablePodThatComesBackIsNotEvictedAgainEveryRound(t *testing.T) {
	// Settling the marker takes the drain past the awaiting block, and the
	// awaiting block is where the other half of this was being answered:
	// replacementFor refuses to read a pod bound to the draining node as a
	// landed replacement, so a workload tolerating the cordon left the drain
	// waiting rather than evicting again. An expendable pod owes no
	// replacement, so nothing sends it through that block any more — and a
	// pod that tolerates the cordon is exactly the pod whose replacement the
	// scheduler puts straight back where it came from.
	//
	// The drain ends either way, at the same minute, on the same bound. What
	// differs is how much of the workload is destroyed on the way there, and
	// evicting the same lineage once an interval for eleven minutes is the
	// churn a bound is supposed to stop rather than merely outlast.
	pods := []*corev1.Pod{
		expendable("filler", mother.ControlledBy("ReplicaSet", "filler-rs"),
			mother.ToleratingEverything()),
	}
	s := snapshot([]*corev1.Node{marked("a", 20*time.Minute, time.Minute, "2"), node("b")}, pods)
	c := clientFor(s)

	evicted := 0
	round, step := drainRounds(t, c, 20, func(pod string, now time.Time) {
		evicted++
		back := expendable(fmt.Sprintf("filler-%s", now.Sub(at)),
			mother.ControlledBy("ReplicaSet", "filler-rs"), mother.ToleratingEverything())
		back.CreationTimestamp.Time = now
		if err := c.Create(context.Background(), back); err != nil {
			t.Fatalf("replacing %s: %v", pod, err)
		}
	})

	// One. The first eviction is the one that finds out; every one after it is
	// binpack repeating an experiment whose answer is already on the node.
	if evicted != 1 {
		t.Errorf("the workload was evicted %d times before the drain ended", evicted)
	}
	// And it must still end, on the bound that measures the absence of
	// progress rather than on anything counting evictions.
	if round != 11 || step.Code != drain.AbandonStalled {
		t.Errorf("ended at +%dm with %+v; expected the stall bound at +11m", round, step)
	}
	if nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the drain ended but left the node cordoned")
	}
}

func TestAnArrivalOnTheCordonedNodeIsWaitedForRatherThanCalledUnaccounted(t *testing.T) {
	// The pod that arrived after the cordon is the only thing left to move,
	// and the case above says what happens then: binpack waits, because a
	// workload that keeps coming back may simply stop and because "no
	// progress" describes the node better than "pods binpack could not
	// account for".
	//
	// What decides which of those two an operator is told is the filter, and
	// it has to be the same filter the assessment uses. A CNI DaemonSet pod
	// is on every node there is, so a residency test that keeps it hands the
	// abandonment a pod on every cluster there is — and the drain ends one
	// evaluation short with a sentence sending its operator to look for a
	// workload binpack models perfectly well.
	pods := []*corev1.Pod{
		mother.DaemonSetPod("kube-system", "cni",
			mother.OnNode("a"), mother.CreatedAt(at.Add(-time.Hour))),
		returning("late", "web-rs", at.Add(-5*time.Minute)),
	}
	s := snapshot([]*corev1.Node{marked("a", 20*time.Minute, time.Minute, "1"), node("b")}, pods)
	c := clientFor(s)

	step, err := executor.Advance(
		context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if step.Code != executor.StepWaiting || step.Done || step.Failed {
		t.Errorf("a DaemonSet pod ended the drain: %+v", step)
	}
	// And it is counted the same way, so the sentence names the arrival alone.
	if !strings.Contains(step.Reason, "1 pods came back") {
		t.Errorf("reason counted more than the arrival: %q", step.Reason)
	}
	if !nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the drain was handed back over a pod the simulation accounted for")
	}
}

func TestAnUnaccountedPodIsOneTheSimulationCouldNotName(t *testing.T) {
	// The case above is the DaemonSet one; these are the other three ways a
	// node carries a pod the simulation declines to name, and the point of
	// running them all is that the residency test they pass is a test about
	// age. Every one of them predates the drain, so whichever of them a node
	// happens to run is the one that would end it.
	//
	// A mirror pod dies with the node as a DaemonSet pod does. A Succeeded or
	// Failed pod holds nothing and never leaves on its own. The simulation
	// accounts for all three by having nothing to do with them, which is what
	// makes the abandonment's sentence a claim about binpack's allowlist
	// rather than about the node's population.
	for _, tc := range []struct {
		name string
		pod  *corev1.Pod
	}{
		{"mirror", mother.MirrorPod("kube-system", "kube-apiserver", mother.OnNode("a"))},
		{"succeeded", mother.Pod("default", "job", mother.OnNode("a"), mother.Succeeded())},
		{"failed", mother.Pod("default", "gone", mother.OnNode("a"), mother.Failed())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pods := []*corev1.Pod{tc.pod, returning("late", "web-rs", at.Add(-5*time.Minute))}
			s := snapshot(
				[]*corev1.Node{marked("a", 20*time.Minute, time.Minute, "1"), node("b")}, pods)
			c := clientFor(s)

			step, err := executor.Advance(
				context.Background(), c, s, "a", engineConfig(), drainPolicy())
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}

			if step.Code == drain.AbandonUnaccounted {
				t.Errorf("a %s pod was reported as one the simulation could not name: %+v",
					tc.name, step)
			}
			if step.Code != executor.StepWaiting || step.Done || step.Failed {
				t.Errorf("the drain did not wait for the arrival: %+v", step)
			}
		})
	}
}

// stalePDB is a budget covering the drain's remaining work whose controller has
// not yet observed the current spec. The eviction API refuses every disruption
// in that state, whatever the recorded allowance says.
func stalePDB() *policyv1.PodDisruptionBudget {
	return mother.Stale(mother.PDB("default", "web", 1, map[string]string{"app": "web"}))
}

// covered is an ordinary pod on the draining node, labelled so that a budget
// can select it.
func covered(name, app string) *corev1.Pod {
	return mother.Pod("default", name, mother.OnNode("a"),
		mother.PodLabels(map[string]string{"app": app}))
}

func TestABudgetWhoseControllerIsCatchingUpDoesNotEndACommittedDrain(t *testing.T) {
	// A budget whose observedGeneration is behind its generation has not been
	// recomputed yet; disruptionsAllowed is stale rather than zero-because-the
	// -application-is-degraded. The disruption controller resyncs on the very
	// update that bumped the generation, so the condition lasts one sync — and
	// the write that causes it is a Helm upgrade, an Argo sync or a kubectl
	// edit touching any budget in the namespace, which is a routine event on a
	// cluster with continuous delivery.
	//
	// Ending the drain over it uncordons a node that has already relocated
	// pods, keeps the relocations, buys nothing with them, and files thirty
	// minutes of backoff under a reason that has stopped being true before
	// anybody reads it.
	replacement := pending("replacement", "web-rs", at.Add(-30*time.Second), false)
	replacement.Spec.NodeName = "b"

	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")},
		[]*corev1.Pod{covered("stayer", "web"), replacement})
	s.PDBs = []*policyv1.PodDisruptionBudget{stalePDB()}
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if step.Done || step.Failed {
		t.Errorf("a budget waiting for its own controller ended the drain: %+v", step)
	}
	if step.Code != executor.StepWaiting {
		t.Errorf("expected the drain to wait, got %+v", step)
	}
	if !nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the node was uncordoned by a blocker that pauses rather than ends a drain")
	}
}

func TestABudgetThatNeverCatchesUpStillEndsTheDrain(t *testing.T) {
	// The other half of pausing, and the failure this change can introduce: a
	// blocker classed transient that turns out to be durable converts a
	// bounded abandon into a wait, and a wait is only safe while something
	// ends it.
	//
	// So the blocker is held true for the drain's whole life rather than for
	// its first evaluation. A pause re-derives the same answer every interval,
	// which is indistinguishable from progress if you only ever look once.
	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")},
		[]*corev1.Pod{covered("stayer", "web")})
	c := clientFor(s)

	stale := stalePDB()
	round, step := drainRounds(t, c, 20,
		func(pod string, now time.Time) {
			t.Errorf("%s was evicted while every eviction was being refused", pod)
		},
		func(round *engine.Snapshot) {
			round.PDBs = []*policyv1.PodDisruptionBudget{stale}
		})

	// At +10m: the last progress is the marker awaitingNode left a minute
	// before round zero, and a paused drain moves nothing, so the clock runs
	// from there uninterrupted.
	if round != 10 || step.Code != drain.AbandonStalled {
		t.Errorf("ended at +%dm with %+v; expected the stall bound at +10m", round, step)
	}
	if nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the drain ended but left the node cordoned")
	}
}

func TestOnlyABlockerThatLiftsItselfPausesADrain(t *testing.T) {
	// The transient set is one member wide on purpose, and the cost of
	// widening it is asymmetric. A blocker that really does lift costs one
	// evaluation to wait for. A durable one classed transient converts an
	// abandonment that is immediate and names its cause into a cordoned node
	// that ends eleven minutes later reporting "no progress" — the same
	// outcome, later, described worse.
	//
	// So the others are asserted to end the drain rather than left to end it
	// by not having been thought about. Marking any of them transient makes a
	// row here fail, which is the whole point of writing them down.
	tests := []struct {
		name  string
		pods  []*corev1.Pod
		pdbs  []*policyv1.PodDisruptionBudget
		pause bool
	}{
		{
			name:  "a budget waiting for its own controller",
			pods:  []*corev1.Pod{covered("stayer", "web")},
			pdbs:  []*policyv1.PodDisruptionBudget{stalePDB()},
			pause: true,
		},
		{
			// An allowance of zero is the application saying it cannot spare a
			// replica. It may resolve as replicas become healthy, and it may
			// equally be a budget that can never have slack — minAvailable at
			// 100%, a workload that is not coming back — and nothing in the
			// object distinguishes the two.
			name: "a budget with no disruption to spare",
			pods: []*corev1.Pod{covered("stayer", "web")},
			pdbs: []*policyv1.PodDisruptionBudget{
				mother.PDB("default", "web", 0, map[string]string{"app": "web"})},
		},
		{
			// Two budgets over one pod. The eviction subresource does not
			// arbitrate between them and answers 500, so nothing evicts that
			// pod: not binpack, not kubectl drain, not the autoscaler.
			name: "a pod under two budgets",
			pods: []*corev1.Pod{covered("stayer", "web")},
			pdbs: []*policyv1.PodDisruptionBudget{
				mother.PDB("default", "web", 1, map[string]string{"app": "web"}),
				mother.PDB("default", "web-also", 1, map[string]string{"app": "web"})},
		},
		{
			// Every blocker, not any. A node held cordoned waiting for a
			// condition that lifted a minute ago, alongside one that never
			// will, is the worst of both answers.
			name: "one of each",
			pods: []*corev1.Pod{covered("stayer", "web"), covered("other", "api")},
			pdbs: []*policyv1.PodDisruptionBudget{stalePDB(),
				mother.PDB("default", "api", 0, map[string]string{"app": "api"})},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot(
				[]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")}, tc.pods)
			s.PDBs = tc.pdbs
			c := clientFor(s)

			step, err := executor.Advance(
				context.Background(), c, s, "a", engineConfig(), drainPolicy())
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}

			cordoned := nodeFrom(t, c, "a").Spec.Unschedulable
			if tc.pause {
				if step.Done || step.Failed || !cordoned {
					t.Errorf("expected the drain to pause and the node to stay cordoned, "+
						"got %+v (cordoned %v)", step, cordoned)
				}
				return
			}
			if !step.Done || !step.Failed || cordoned {
				t.Errorf("expected the drain to end and the node to be handed back, "+
					"got %+v (cordoned %v)", step, cordoned)
			}
		})
	}
}

func TestARefusedReplacementEndsTheDrainEvenWhileABudgetIsCatchingUp(t *testing.T) {
	// Pausing is for a refusal that lifts on its own. A replacement the
	// scheduler has already given up on is not that, and it outranks the
	// pause for the reason the awaiting block gives: uncordoning is that
	// pod's repair, because this node is where it can go.
	//
	// Before the pause existed every non-drainable verdict uncordoned, so a
	// refused replacement always got its node back whatever else was wrong.
	// The pause is the first verdict that does not, which is what makes this
	// a hole the branch opened rather than one it inherited — a Pending pod
	// held down for as long as a budget takes to catch up, and up to the
	// stall timeout if the disruption controller is the thing that is broken.
	refused := pending("replacement", "web-rs", at.Add(-30*time.Second), true)

	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")},
		[]*corev1.Pod{covered("stayer", "web"), refused})
	s.PDBs = []*policyv1.PodDisruptionBudget{stalePDB()}
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if step.Code != drain.AbandonUnschedulable || !step.Done || !step.Failed {
		t.Errorf("expected the refused replacement to end the drain, got %+v", step)
	}
	if !strings.Contains(step.Reason, "replacement") {
		t.Errorf("the reason should name the pod that cannot be placed, got %q", step.Reason)
	}
	if nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the node stayed cordoned, so the pod that needs it still cannot be placed")
	}
}

func TestAGatedReplacementEndsTheDrainEvenWhileABudgetIsCatchingUp(t *testing.T) {
	// The same hole, reached by the state next door. The pause branch and the
	// awaiting block below it ask the same question of the same helper, so a
	// replacement state that ends the drain in one and waits in the other
	// leaves a node cordoned for whichever of the two conditions happens to be
	// checked first — and here that is a pod nothing is going to place held
	// down behind a budget that will catch up in one sync.
	//
	// Its own test rather than a row on the one above, because the two
	// branches are separate call sites and only crossing the state with the
	// blocker separates them: with no blocker the awaiting block answers, and
	// this branch could return anything at all without a test noticing.
	gated := pending("replacement", "web-rs", at.Add(-30*time.Second), false)
	mother.Gated("kueue.x-k8s.io/admission")(gated)

	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")},
		[]*corev1.Pod{covered("stayer", "web"), gated})
	s.PDBs = []*policyv1.PodDisruptionBudget{stalePDB()}
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if step.Code != drain.AbandonUnschedulable || !step.Done || !step.Failed {
		t.Errorf("expected the gated replacement to end the drain, got %+v", step)
	}
	if !strings.Contains(step.Reason, "scheduling gate") {
		t.Errorf("the reason should say the pod is gated, got %q", step.Reason)
	}
	if nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the node stayed cordoned behind a replacement nothing will place")
	}
}

func TestAPausedDrainSettlesALandedReplacement(t *testing.T) {
	// Two branches answer above the awaiting block — the repair and the pause
	// — and a replacement that landed on the evaluation either of them
	// answered was never recorded as settled. The marker went on naming the
	// controller, so the next evaluation went on asking the whole pod list
	// about it, and any later pod of that ReplicaSet the scheduler could not
	// place — an HPA scale-up into a cluster binpack has just tightened, a
	// rollout, a scale-out waiting for a node — ended a healthy half-finished
	// drain under `replacement-unschedulable`, naming a pod that never ran on
	// this node and firing the series that exists to detect the scale-up
	// binpack is meant to prevent.
	//
	// Two rounds, because one says nothing. The settle is a write, and what it
	// buys is what the *next* evaluation does not do.
	//
	// The rows that end the drain are the ranking, asserted in the same table
	// on purpose: a replacement nothing will place outranks the pause, and
	// resolving the replacement above the branch switch must not quietly
	// reorder that.
	landed := func() *corev1.Pod {
		p := pending("replacement", "web-rs", at.Add(-30*time.Second), false)
		p.Spec.NodeName = "b"
		return p
	}
	gated := func() *corev1.Pod {
		p := pending("replacement", "web-rs", at.Add(-30*time.Second), false)
		mother.Gated("kueue.x-k8s.io/admission")(p)
		return p
	}

	for _, tc := range []struct {
		name string
		// replacement is what became of the pod this drain is owed.
		replacement *corev1.Pod
		// catchingUp puts a budget waiting for its own controller in the
		// snapshot, which is what makes the pause branch answer.
		catchingUp bool
		// uncordoned makes the node marked-but-schedulable, which is what
		// makes the repair branch answer.
		uncordoned bool
		// ends: the drain is over on round one, and what the marker says
		// afterwards is [Abandon]'s business rather than this test's.
		ends bool
	}{
		{
			name:        "paused while the replacement lands",
			replacement: landed(),
			catchingUp:  true,
		},
		{
			name:        "repaired while the replacement lands",
			replacement: landed(),
			uncordoned:  true,
		},
		{
			name:        "a refused replacement still outranks the pause",
			replacement: pending("replacement", "web-rs", at.Add(-30*time.Second), true),
			catchingUp:  true,
			ends:        true,
		},
		{
			name:        "a gated replacement still outranks the pause",
			replacement: gated(),
			catchingUp:  true,
			ends:        true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := []*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")}
			if tc.uncordoned {
				nodes[0].Spec.Unschedulable = false
			}
			s := snapshot(nodes, []*corev1.Pod{covered("stayer", "web"), tc.replacement})
			if tc.catchingUp {
				s.PDBs = []*policyv1.PodDisruptionBudget{stalePDB()}
			}
			c := clientFor(s)

			step, err := executor.Advance(
				context.Background(), c, s, "a", engineConfig(), drainPolicy())
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}

			if tc.ends {
				if step.Code != drain.AbandonUnschedulable || !step.Failed {
					t.Errorf("a replacement nothing will place stopped outranking "+
						"the pause: %+v", step)
				}
				return
			}

			if step.Failed {
				t.Fatalf("the drain ended over a replacement that had landed: %+v", step)
			}
			if got := nodeFrom(t, c, "a").Annotations[engine.AnnotationDrainAwaiting]; got !=
				engine.AwaitingSettled {
				t.Errorf("the marker still names a controller that owes this drain "+
					"nothing: %q", got)
			}

			// The ReplicaSet gains a pod the scheduler cannot place. It was
			// never on this node and this drain did not cause it, so nothing
			// about it is the drain's business.
			if err := c.Create(context.Background(),
				pending("scaled-up", "web-rs", at.Add(30*time.Second), true)); err != nil {
				t.Fatalf("creating the scale-up pod: %v", err)
			}

			round := cluster(t, c, at.Add(time.Minute))
			if tc.catchingUp {
				round.PDBs = []*policyv1.PodDisruptionBudget{stalePDB()}
			}
			step, err = executor.Advance(
				context.Background(), c, round, "a", engineConfig(), drainPolicy())
			if err != nil {
				t.Fatalf("Advance, round two: %v", err)
			}

			if step.Failed {
				t.Errorf("a pod that never ran on this node ended a healthy drain: %+v", step)
			}
			if !nodeFrom(t, c, "a").Spec.Unschedulable {
				t.Error("the node was handed back, so every pod it has already " +
					"lost bought nothing")
			}
		})
	}
}

// refusingEvictions answers every eviction with err while letting the node
// patches through, so a test can assert what the drain did about the refusal
// rather than only that it stopped.
type refusingEvictions struct {
	executor.Writer
	err error
}

func (r refusingEvictions) SubResource(string) client.SubResourceClient {
	return subResource{r.err}
}

func TestAPermanentEvictionRefusalAbandonsRatherThanRetries(t *testing.T) {
	// The mirror of the case above. A pod covered by two budgets is refused
	// with HTTP 500 rather than a retryable 429, because the eviction
	// subresource does not arbitrate between budgets — so nothing evicts that
	// pod: not binpack, not kubectl drain, not the autoscaler.
	//
	// Reachable only as a race, since CheckEvictable otherwise refuses the
	// candidate first: the second budget is created between the assessment and
	// the eviction. Failing the reconcile leaves the node cordoned until the
	// next evaluation reaches the same conclusion through a different route,
	// so the conclusion is reached here instead, under the same code that
	// route would have produced.
	pods := []*corev1.Pod{
		mother.Pod("default", "one", mother.OnNode("a")),
		mother.Pod("default", "two", mother.OnNode("a")),
	}
	s := snapshot([]*corev1.Node{marked("a", time.Minute, time.Minute, "2"), node("b")}, pods)
	c := clientFor(s)
	w := refusingEvictions{Writer: c, err: apierrors.NewInternalError(errors.New(
		"This pod has more than one PodDisruptionBudget, which the eviction subresource does not support."))}

	step, err := executor.Advance(context.Background(), w, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if !step.Done || !step.Failed {
		t.Errorf("an eviction nothing can accept did not end the drain: %+v", step)
	}
	if step.Code != engine.VerdictBlocked {
		t.Errorf("code = %q, want %q — the same code revalidation reaches an interval later",
			step.Code, engine.VerdictBlocked)
	}
	if nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the drain ended but left the node cordoned")
	}
}

// sized is a pool member with its memory stated outright. The fixtures below
// turn on how much room is left in the cluster rather than on how many pods
// there are, so the archetype's "large enough that resources are never the
// reason" is the wrong default for them.
func sized(name, memory string, opts ...mother.NodeOption) *corev1.Node {
	return node(name, append([]mother.NodeOption{
		mother.Allocatable(corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse(memory),
			corev1.ResourcePods:   resource.MustParse("110"),
		}),
	}, opts...)...)
}

// journal records every write in the order it was made, patches and evictions
// together.
//
// Ordering is not visible in the state a successful call leaves behind: the
// same annotations are on the node either way. It is only visible in what
// survives an interruption, so it is asserted directly.
type journal struct {
	executor.Writer
	writes []string
}

func (j *journal) Patch(
	ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption,
) error {
	data, err := patch.Data(obj)
	if err != nil {
		return err
	}
	j.writes = append(j.writes, "patch "+string(data))
	return j.Writer.Patch(ctx, obj, patch, opts...)
}

func (j *journal) SubResource(name string) client.SubResourceClient {
	return evictions{SubResourceClient: j.Writer.SubResource(name), journal: j}
}

type evictions struct {
	client.SubResourceClient
	journal *journal
}

func (e evictions) Create(
	ctx context.Context, obj client.Object, sub client.Object, opts ...client.SubResourceCreateOption,
) error {
	e.journal.writes = append(e.journal.writes, "evict "+obj.GetName())
	return e.SubResourceClient.Create(ctx, obj, sub, opts...)
}

// interrupted stops answering after keep node patches, which is what a lost
// lease, an OOM kill or an API server that has started refusing looks like from
// inside one evaluation. Counted rather than matched on content, so it puts the
// break at a position in the sequence rather than at a particular annotation.
type interrupted struct {
	executor.Writer
	keep int
	seen int
}

func (i *interrupted) Patch(
	ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption,
) error {
	i.seen++
	if i.seen > i.keep {
		return apierrors.NewInternalError(errors.New("simulated API-server failure"))
	}
	return i.Writer.Patch(ctx, obj, patch, opts...)
}

// commitment is a drain one eviction from finishing whose cluster tightens
// underneath it: a big workload is deployed while the drain is in flight, and
// the reserve — which asks that a pod of every maximal relocatable shape in the
// cluster could still be placed — stops being satisfiable.
//
// That is the shape the reserve is dangerous in. It is a preference, so
// ADR-0009 stops asking it once pods have moved; a drain that has forgotten it
// evicted anything is asked it again, and abandons a node it had half emptied.
func commitment() (engine.Snapshot, engine.Config) {
	pods := []*corev1.Pod{
		mother.Pod("default", "one", mother.OnNode("a"), mother.Requests("100m", "1Gi")),
		mother.Pod("default", "two", mother.OnNode("a"), mother.Requests("100m", "512Mi")),
	}
	s := snapshot([]*corev1.Node{
		marked("a", 20*time.Minute, 5*time.Minute, "3"),
		sized("b", "2048Mi"), sized("c", "2048Mi"), sized("d", "4352Mi"),
	}, pods)

	cfg := engineConfig()
	cfg.Default.Sim.ReserveForLargestPod = true
	return s, cfg
}

// tightens deploys the workload that makes the reserve unsatisfiable, and puts
// the replacement for the evicted pod where the scheduler would have put it.
func tightens(t *testing.T, c client.Client) {
	t.Helper()
	back := mother.Pod("default", "one-back", mother.OnNode("c"),
		mother.ControlledBy("ReplicaSet", "one-rs"), mother.Requests("100m", "1Gi"))
	back.CreationTimestamp.Time = at.Add(30 * time.Second)

	for _, pod := range []*corev1.Pod{
		back,
		mother.Pod("default", "big", mother.OnNode("d"), mother.Requests("100m", "4Gi")),
	} {
		if err := c.Create(context.Background(), pod); err != nil {
			t.Fatalf("creating %s: %v", pod.Name, err)
		}
	}
}

func TestTheDrainCommitsBeforeItEvicts(t *testing.T) {
	// The commitment marker is the only record that a drain has disrupted
	// anything, and it was written afterwards, in a patch of its own. Between
	// the accepted eviction and that write the node is indistinguishable from
	// one that has evicted nothing — and the window is entered once per
	// eviction, so a busy drain enters it dozens of times.
	//
	// Written first instead, and in the same patch as the progress markers,
	// because there is no useful state between those two either: a node
	// carrying fresh progress and no commitment says both that an eviction has
	// just happened and that none ever has.
	//
	// Both arms because the write that follows the eviction is conditional,
	// and a condition nothing exercises on its false side is a condition
	// nothing holds to.
	for _, tc := range []struct {
		name   string
		pods   []*corev1.Pod
		cfg    engine.Config
		writes int
	}{
		{
			// The marker is upgraded afterwards to name the controller that
			// owes the replacement, which is a fact only the accepted eviction
			// establishes.
			name:   "the eviction owes a replacement",
			pods:   []*corev1.Pod{mother.Pod("default", "one", mother.OnNode("a"))},
			cfg:    engineConfig(),
			writes: 3,
		},
		{
			// Nothing is owed for an expendable pod, so the commitment already
			// says everything there is to say and the second write would
			// repeat it.
			name:   "the eviction owes nothing",
			pods:   []*corev1.Pod{expendable("filler")},
			cfg:    consolidating(),
			writes: 2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := snapshot([]*corev1.Node{
				marked("a", 20*time.Minute, 5*time.Minute, "2"), node("b")}, tc.pods)
			w := &journal{Writer: clientFor(s)}

			step, err := executor.Advance(
				context.Background(), w, s, "a", tc.cfg, drainPolicy())
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if step.Code != executor.StepEvicted {
				t.Fatalf("expected an eviction, got %+v", step)
			}

			if len(w.writes) != tc.writes {
				t.Fatalf("expected %d writes, got %d: %v", tc.writes, len(w.writes), w.writes)
			}
			for _, key := range []string{
				engine.AnnotationDrainAwaiting,
				engine.AnnotationDrainProgress,
				engine.AnnotationDrainPodsRemaining,
			} {
				if !strings.Contains(w.writes[0], key) {
					t.Errorf("the drain's first write did not carry %s: %s", key, w.writes[0])
				}
			}
			if !strings.HasPrefix(w.writes[1], "evict ") {
				t.Errorf("the eviction did not follow the commitment: %v", w.writes)
			}
		})
	}
}

func TestACommittedDrainStaysCommittedWhenTheMarkerWriteFails(t *testing.T) {
	// The eviction is irreversible and the write that records it is not
	// retried, so one failed patch used to leave a disrupted pod on a node
	// carrying no marker. Since the commitment gates which questions
	// revalidation re-asks, that is not a re-applied preference any more: the
	// reserve comes back, and the pod that just left has consumed the room it
	// wants, so the next evaluation abandons a drain that was working.
	//
	// Every place the evaluation can stop, not only the last one. The invariant
	// is that no interruption leaves a pod gone from a node that reads as
	// untouched, and an evaluation that stops before the eviction satisfies it
	// by having disrupted nothing — which is the arm that fails if the
	// commitment travels with the markers but still follows the eviction.
	for _, keep := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("%d writes reach the API server", keep), func(t *testing.T) {
			s, reserving := commitment()
			c := clientFor(s)

			_, _ = executor.Advance(context.Background(),
				&interrupted{Writer: c, keep: keep}, s, "a", engineConfig(), drainPolicy())

			var left corev1.PodList
			if err := c.List(context.Background(), &left); err != nil {
				t.Fatalf("listing pods: %v", err)
			}
			evicted := len(left.Items) < len(s.Pods)
			committed := nodeFrom(t, c, "a").Annotations[engine.AnnotationDrainAwaiting] != ""

			if evicted && !committed {
				t.Fatal("a drain that has evicted a pod is carrying no record of it")
			}
			if !evicted {
				return
			}

			tightens(t, c)
			step, err := executor.Advance(context.Background(), c,
				cluster(t, c, at.Add(time.Minute)), "a", reserving, drainPolicy())
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if step.Failed {
				t.Errorf("a preference abandoned a drain that had already evicted: %+v", step)
			}
		})
	}
}

func TestARefusedEvictionLeavesTheDrainOwingNoReplacement(t *testing.T) {
	// The other half of writing the marker first, and the reason the value
	// written is the settled sentinel rather than the controller's identity.
	//
	// A disruption budget refusing the eviction is the routine failure here,
	// and it lands *after* the commitment. A marker naming a controller would
	// then claim a replacement is owed for a pod that never left, and nothing
	// creates one — so the drain waits, makes no progress, and ends at the
	// stall bound having relocated everything it was asked to. The sentinel
	// owes nothing, so the next evaluation simply tries the eviction again.
	//
	// Run to its ending rather than checked once, because "waits this round"
	// and "waits for ever" look identical from a single call, and it is the
	// ending that differs.
	s, _ := commitment()
	c := clientFor(s)

	budget := refusingEvictions{Writer: c, err: apierrors.NewTooManyRequests("nope", 1)}
	if _, err := executor.Advance(
		context.Background(), budget, s, "a", engineConfig(), drainPolicy()); err == nil {
		t.Fatal("expected the refused eviction to end the evaluation")
	}

	round, step := drainRounds(t, c, 20, func(pod string, now time.Time) {
		back := mother.Pod("default", pod+"-back", mother.OnNode("d"),
			mother.ControlledBy("ReplicaSet", pod+"-rs"), mother.Requests("100m", "128Mi"))
		back.CreationTimestamp.Time = now
		if err := c.Create(context.Background(), back); err != nil {
			t.Fatalf("replacing %s: %v", pod, err)
		}
	})

	if step.Code != executor.StepAwaitRemoval {
		t.Errorf("the drain ended at +%dm with %+v; expected it to finish", round, step)
	}
}

func TestALostReplacementWaitCostsTheWaitAndNotTheDrain(t *testing.T) {
	// What the residual window actually costs, measured rather than argued.
	//
	// The commitment survives a failed marker write; the wait for the
	// replacement does not, because the value naming the controller is the one
	// written after the eviction. So the next evaluation evicts again with a
	// replacement still unbound — and an unbound pod sits on no node, so the
	// simulation approving that eviction has reserved nothing for it. Two
	// replacements against room proved for one is the risk sequential eviction
	// exists to remove, and it is why this window is worth stating rather than
	// waving at.
	//
	// It is bounded, though, and narrower than the window it replaces, where
	// the same failed patch lost the commitment as well. Both halves are
	// asserted: neither follows from the other, and the second is the reason
	// the first is acceptable.
	s, _ := commitment()
	c := clientFor(s)

	if _, err := executor.Advance(context.Background(),
		&interrupted{Writer: c, keep: 1}, s, "a", engineConfig(), drainPolicy()); err == nil {
		t.Fatal("expected the lost marker write to end the evaluation")
	}

	// Unbound, which is precisely the state the lost wait would have waited
	// for, and which the simulation cannot see.
	unbound := pending("one-back", "one-rs", at.Add(30*time.Second), false)
	if err := c.Create(context.Background(), unbound); err != nil {
		t.Fatalf("creating the replacement: %v", err)
	}

	next, err := executor.Advance(context.Background(), c,
		cluster(t, c, at.Add(time.Minute)), "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if next.Code != executor.StepEvicted {
		t.Errorf("the lost write should cost the wait, not the eviction: %+v", next)
	}

	// The second eviction's own marker was written, so its replacement is
	// waited for as usual. Bind it and let the drain reach its ending: a lost
	// wait must not leave the node cordoned with nothing to finish it.
	landed := pending("two-back", "two-rs", at.Add(90*time.Second), false)
	landed.Spec.NodeName = "d"
	if err := c.Create(context.Background(), landed); err != nil {
		t.Fatalf("binding the second replacement: %v", err)
	}

	last, err := executor.Advance(context.Background(), c,
		cluster(t, c, at.Add(2*time.Minute)), "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if last.Code != executor.StepAwaitRemoval {
		t.Errorf("a drain that lost a replacement wait was left unfinished: %+v", last)
	}
}

func TestALostAwaitingMarkerDoesNotReapplyTheReserve(t *testing.T) {
	// The same thing asked of the engine rather than of the annotation, and
	// asked of both outcomes of the write. Whether the patch that carries the
	// marker reaches the API server is not something a drain can control; what
	// it can control is whether the answer to "has this drain evicted
	// anything" depends on it.
	for _, tc := range []struct {
		name string
		keep int
	}{
		{"the marker write landed", 99},
		{"the marker write was lost", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, reserving := commitment()
			c := clientFor(s)

			_, _ = executor.Advance(context.Background(),
				&interrupted{Writer: c, keep: tc.keep}, s, "a", engineConfig(), drainPolicy())

			tightens(t, c)
			got := engine.Revalidate(cluster(t, c, at.Add(time.Minute)), "a", reserving)

			if got.Verdict() != engine.VerdictDrainable {
				t.Errorf("the reserve re-armed on a drain that had already evicted: %q",
					got.Verdict())
			}
		})
	}
}

func TestBeginDoesNotCommitTheDrain(t *testing.T) {
	// The cheap version of the fix, and the trap in it. Committing in Begin
	// leaves every test green while reversing what ADR-0009 chose: a node that
	// has cordoned and evicted nothing would read as committed, and the
	// preferences that decide whether a drain should *start* would never be
	// asked of it.
	//
	// Asserted against a node Begin actually wrote, because the fixtures that
	// pin the uncommitted side elsewhere build their nodes by hand and would
	// go on passing.
	s, reserving := commitment()
	fresh := node("a")
	s.Nodes[0] = fresh
	c := clientFor(s)

	if err := executor.Begin(context.Background(), c, fresh, at); err != nil {
		t.Fatalf("Begin: %v", err)
	}

	after := nodeFrom(t, c, "a")
	if got := after.Annotations[engine.AnnotationDrainAwaiting]; got != "" {
		t.Errorf("a drain that has evicted nothing recorded a commitment: %q", got)
	}

	tightens(t, c)
	if got := engine.Revalidate(cluster(t, c, at.Add(time.Minute)), "a", reserving); got.Verdict() ==
		engine.VerdictDrainable {
		t.Error("the reserve should still refuse a drain that has evicted nothing")
	}
}

func TestMaxPodsPerDrainStopsApplyingOnceTheDrainHasCommitted(t *testing.T) {
	// The boundary the commitment moves, stated rather than discovered. A cap
	// on how many pods one drain may relocate is a question about whether the
	// drain should start, so it stops being asked once the drain has committed
	// — and the drain now commits when it attempts an eviction rather than when
	// one is accepted. A budget refusing the first eviction therefore takes the
	// cap out of force an evaluation earlier than it used to.
	//
	// The conservative direction of the same trade the reserve gets: a drain
	// that has disrupted nothing reads as committed, which disables two
	// preferences and nothing else. The other direction is a drain that has
	// disrupted something reading as untouched, and that ends it.
	//
	// Both arms, because the cap moving one evaluation earlier and the cap
	// never applying at all look identical from the second arm alone — and the
	// second is what writing the marker in Begin would produce.
	pods := []*corev1.Pod{
		mother.Pod("default", "one", mother.OnNode("a")),
		mother.Pod("default", "two", mother.OnNode("a")),
	}
	s := snapshot([]*corev1.Node{marked("a", 20*time.Minute, 5*time.Minute, "3"), node("b")}, pods)
	cfg := engineConfig()
	cfg.Default.MaxPodsPerDrain = 2

	for _, tc := range []struct {
		name      string
		attempted bool
		capped    bool
	}{
		{"before the first eviction is attempted", false, true},
		{"after a budget refused the first eviction", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := clientFor(s)
			if tc.attempted {
				budget := refusingEvictions{
					Writer: c, err: apierrors.NewTooManyRequests("nope", 1)}
				if _, err := executor.Advance(
					context.Background(), budget, s, "a", cfg, drainPolicy()); err == nil {
					t.Fatal("expected the refused eviction to end the evaluation")
				}
			}

			// A workload that tolerates the cordon lands on the node, which is
			// the only way a drain's relocatable set grows once it is under
			// way. Without it the cap answers the same at every evaluation and
			// the arms cannot differ.
			if err := c.Create(context.Background(),
				returning("late", "late-rs", at.Add(30*time.Second))); err != nil {
				t.Fatalf("creating the arrival: %v", err)
			}

			step, err := executor.Advance(context.Background(), c,
				cluster(t, c, at.Add(time.Minute)), "a", cfg, drainPolicy())
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}
			if step.Failed != tc.capped {
				t.Errorf("cap applied = %t, want %t: %+v", step.Failed, tc.capped, step)
			}
		})
	}
}

// refusingMetadata answers every patch carrying metadata with err, so a test
// can stop a hand-back at the point where it records why it happened. A bare
// spec patch still goes through, which is what makes the two orderings of an
// abandon distinguishable.
type refusingMetadata struct {
	executor.Writer
	err error
}

func (r refusingMetadata) Patch(
	ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption,
) error {
	data, err := patch.Data(obj)
	if err != nil {
		return err
	}
	if strings.Contains(string(data), "metadata") {
		return r.err
	}
	return r.Writer.Patch(ctx, obj, patch, opts...)
}

// stuck is a drain the cluster has overtaken: the pod left on the node no
// longer fits anywhere, so revalidation ends it.
func stuck() engine.Snapshot {
	pods := []*corev1.Pod{
		mother.Pod("default", "stuck", mother.OnNode("a"), mother.Requests("100m", "3Gi")),
		mother.Pod("default", "big", mother.OnNode("b"), mother.Requests("100m", "3Gi")),
	}
	return snapshot([]*corev1.Node{
		marked("a", 20*time.Minute, time.Minute, "1"), sized("b", "4096Mi"),
	}, pods)
}

func TestAnAbandonThatCannotRecordItselfDoesNotResume(t *testing.T) {
	// A hand-back that uncordons and then fails to record itself leaves the
	// node in the state Begin leaves between its own two writes — marked and
	// schedulable — and that state has a repair, which is to cordon and carry
	// on. So the drain the cluster had just decided to abandon resumes, and
	// the diagnosis goes with it: no reason on the node, no backoff, and
	// nothing to stop the same node being chosen again immediately.
	//
	// The two halves have no useful intermediate, so they travel together.
	s := stuck()
	c := clientFor(s)
	w := refusingMetadata{Writer: c, err: apierrors.NewInternalError(
		errors.New("simulated API-server failure"))}

	if _, err := executor.Advance(
		context.Background(), w, s, "a", engineConfig(), drainPolicy()); err == nil {
		t.Fatal("expected the refused patch to end the evaluation")
	}

	step, err := executor.Advance(context.Background(), c,
		cluster(t, c, at.Add(time.Minute)), "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if step.Code == executor.StepCordoned {
		t.Errorf("the abandoned drain resumed: %+v", step)
	}

	after := nodeFrom(t, c, "a")
	if after.Spec.Unschedulable {
		t.Error("the drain ended but left the node cordoned")
	}
	for _, key := range []string{
		engine.AnnotationBackoffUntil,
		engine.AnnotationLastFailure,
		engine.AnnotationDrainAttempts,
	} {
		if after.Annotations[key] == "" {
			t.Errorf("the abandonment left no %s behind: %v", key, after.Annotations)
		}
	}
}

func TestTheHandBackAndItsRecordMoveInOneWrite(t *testing.T) {
	// Separately would let the node be schedulable-but-marked for as long as
	// it takes the second write to land, or forever if it fails — and that is
	// the one half-state binpack cannot read, because it is byte-identical to
	// the one a half-finished Begin leaves and calls for the opposite repair.
	// A merge patch carries spec and metadata together, so there is no reason
	// to take that risk.
	s := stuck()
	c := clientFor(s)
	rec := &recorder{Writer: c}

	if _, err := executor.Abandon(context.Background(), rec,
		s.Nodes[0], "test", "because", at, drainPolicy()); err != nil {
		t.Fatalf("Abandon: %v", err)
	}

	if len(rec.patches) != 1 {
		t.Fatalf("expected one patch, got %d: %v", len(rec.patches), rec.patches)
	}
	if !strings.Contains(rec.patches[0], "unschedulable") ||
		!strings.Contains(rec.patches[0], engine.AnnotationBackoffUntil) {
		t.Errorf("the hand-back and its record were not written together: %s", rec.patches[0])
	}
}

// TestTheHardestPodToPlaceIsEvictedFirst is the eviction order's stated
// purpose, asked of a node whose binding dimension is not memory.
//
// Evictions are one per interval with a full revalidation between them, so a
// drain spans minutes and the cluster moves underneath it. The order exists so
// that a prediction which turns out to be wrong costs one eviction rather than
// all of them — which requires the pod likeliest to lose its destination to go
// first. Ordering by memory put a 7-core pod behind every 2Gi one, so the
// riskiest eviction was scheduled last, after the cheap ones had already been
// spent.
func TestTheHardestPodToPlaceIsEvictedFirst(t *testing.T) {
	// Wide enough that all four pods have somewhere to go, so nothing about
	// this test turns on the drain being tight.
	roomy := mother.Allocatable(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("16"),
		corev1.ResourceMemory: resource.MustParse("16Gi"),
		corev1.ResourcePods:   resource.MustParse("110"),
	})
	pods := []*corev1.Pod{
		mother.Pod("default", "web-a", mother.OnNode("a"), mother.Requests("500m", "2Gi")),
		mother.Pod("default", "web-b", mother.OnNode("a"), mother.Requests("500m", "2Gi")),
		mother.Pod("default", "web-c", mother.OnNode("a"), mother.Requests("500m", "2Gi")),
		mother.Pod("default", "hog", mother.OnNode("a"), mother.Requests("7", "100Mi")),
	}
	s := snapshot([]*corev1.Node{marked("a", time.Minute, 0, "", roomy), node("b", roomy)}, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if step.Code != executor.StepEvicted {
		t.Fatalf("expected an eviction, got %+v", step)
	}

	var after corev1.PodList
	if err := c.List(context.Background(), &after); err != nil {
		t.Fatalf("listing pods: %v", err)
	}
	remaining := map[string]bool{}
	for _, p := range after.Items {
		remaining[p.Name] = true
	}
	if remaining["hog"] {
		t.Errorf("evicted a 2Gi pod first; the 7-core one is the hard placement, and it is still on the node")
	}
}

// TestAnEmptiedNodeWaitsThroughAScaleUpButNotThroughADeadAutoscaler pairs the
// extension with the thing that still ends it.
//
// The cluster-autoscaler gates all scale-down on its own
// scale-down-delay-after-add, cluster-wide, so a deploy anywhere can hold an
// emptied node past removalTimeout. binpack waits through that rather than
// uncordoning a node the autoscaler was about to delete. What it must not do
// is wait through it for ever, and the failure that makes a wait meaningless
// is the autoscaler being gone — nothing else would ever remove the node, and
// a scale-up it published before it stopped would otherwise keep the drain
// alive on the strength of a process that no longer exists.
//
// That question is answered where every other wait answers it: revalidation
// reads the published status, reports not-autoscaled, and the executor hands
// the node back on the verdict before the assessment is consulted at all.
func TestAnEmptiedNodeWaitsThroughAScaleUpButNotThroughADeadAutoscaler(t *testing.T) {
	policy := drainPolicy()
	policy.ScaleUpPause = 10 * time.Minute

	for _, tc := range []struct {
		name string
		// probeAgo is how long since the autoscaler last completed a scan,
		// against a five-minute MaxStatusAge. A status ConfigMap outlives the
		// process that wrote it, so freshness is what tells them apart.
		probeAgo time.Duration
		// scaleUpAgo ages the transition stamp. Past the pause it defers
		// nothing on its own, which is what leaves the flag as the only thing
		// that can decide the case.
		scaleUpAgo time.Duration
		inProgress bool
		wantCode   string
		wantEnded  bool
	}{
		{
			name:       "a live autoscaler inside its own post-scale-up pause is waited for",
			probeAgo:   10 * time.Second,
			scaleUpAgo: 2 * time.Minute,
			wantCode:   executor.StepAwaitRemoval,
		},
		{
			// The stamp has aged out, so only the flag can defer this — which
			// makes it the case that proves the flag reaches the assessment at
			// all. A slow scale-up is precisely the shape that gets here: the
			// stamp names when the growth began and stops moving, so it ages
			// past the pause while the growth is still going on.
			name:       "a scale-up still in progress is waited for past the stamp's pause",
			probeAgo:   10 * time.Second,
			scaleUpAgo: 30 * time.Minute,
			inProgress: true,
			wantCode:   executor.StepAwaitRemoval,
		},
		{
			// The control for the one above, and the reason it is not vacuous.
			name:       "the same aged stamp with no scale-up under way hands the node back",
			probeAgo:   10 * time.Second,
			scaleUpAgo: 30 * time.Minute,
			wantCode:   drain.AbandonNotRemoved,
			wantEnded:  true,
		},
		{
			name:       "an autoscaler that has stopped ends the wait, scale-up or no",
			probeAgo:   30 * time.Minute,
			scaleUpAgo: 2 * time.Minute,
			wantCode:   engine.SkipAutoscalerNotLive,
			wantEnded:  true,
		},
		{
			// The deferral binpack cannot time out of on its own. A timestamp
			// ages; a flag does not, so a status document frozen mid-scale-up
			// by an autoscaler that then died reads InProgress for ever — the
			// unbounded wait in its purest form, and the reason this pair is
			// tested together rather than the deferral alone. Nothing new
			// bounds it, because the bound is the same one: the document has
			// stopped being refreshed, so it has stopped vouching for the
			// process that wrote it.
			name:       "an InProgress scale-up frozen by a dead autoscaler ends the wait too",
			probeAgo:   30 * time.Minute,
			scaleUpAgo: 2 * time.Minute,
			inProgress: true,
			wantCode:   engine.SkipAutoscalerNotLive,
			wantEnded:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Empty, and sixteen minutes past a fifteen-minute bound. Settled
			// rather than bare: a node binpack emptied got that way by
			// evicting, so the drain is committed and the cooldowns have
			// stopped applying to it.
			n := marked("a", time.Hour, 16*time.Minute, "0", mother.NodeAnnotations(
				map[string]string{engine.AnnotationDrainAwaiting: engine.AwaitingSettled}))

			s := snapshot([]*corev1.Node{n, node("b")}, nil)
			s.Autoscaler.LastProbe = at.Add(-tc.probeAgo)
			s.Autoscaler.LastScaleUp = at.Add(-tc.scaleUpAgo)
			s.Autoscaler.ScaleUpInProgress = tc.inProgress

			c := clientFor(s)
			step, err := executor.Advance(
				context.Background(), c, s, "a", engineConfig(), policy)
			if err != nil {
				t.Fatalf("Advance: %v", err)
			}

			if step.Code != tc.wantCode {
				t.Errorf("step code = %q, want %q", step.Code, tc.wantCode)
			}
			if step.Done != tc.wantEnded || step.Failed != tc.wantEnded {
				t.Errorf("got %+v, want ended=%v", step, tc.wantEnded)
			}
			if after := nodeFrom(t, c, "a"); after.Spec.Unschedulable == tc.wantEnded {
				t.Errorf("cordoned = %v, want %v", after.Spec.Unschedulable, !tc.wantEnded)
			}
			if tc.wantEnded {
				return
			}
			// The pause defers the abandonment; it does not restart the clock.
			// Restamping here would hold the removal bound at zero for as long
			// as the cluster kept growing, so the node would never be handed
			// back once the gate opened — the wait made unbounded by the very
			// thing meant to keep it honest.
			if got := nodeFrom(t, c, "a").Annotations[engine.AnnotationDrainProgress]; got !=
				at.Add(-16*time.Minute).Format(time.RFC3339) {
				t.Errorf("progress marker = %q, want it left where it was", got)
			}
		})
	}
}

func TestAdvanceAbandonsWhenTheReplacementIsSchedulingGated(t *testing.T) {
	// An admission webhook that attaches a scheduling gate at CREATE — Kueue,
	// or any quota or policy gate — re-gates the replacement the controller
	// makes for an evicted pod. The gate is on neither of the two objects
	// binpack builds a replacement from: not the stored template, since it is
	// injected at CREATE, and not the running pod, since the API server
	// forbids spec.nodeName while a gate remains. So the allowlist's
	// scheduling-gate refusal never sees it and the eviction happens.
	//
	// What arrives then is a pod in neither state this drain can act on. The
	// API server stamps {PodScheduled, False, SchedulingGated}, the scheduler
	// never dequeues it, and nothing ever writes Unschedulable — so it is not
	// refused, and having no node it has not landed either. Waiting for it is
	// waiting on a third-party controller that publishes nothing binpack can
	// read, which is the one shape CLAUDE.md says must never bound a wait.
	// Detect it positively and hand the node back: it is where that pod could
	// have stayed.
	gated := pending("gated", "web-rs", at.Add(-30*time.Second), false)
	mother.Gated("kueue.x-k8s.io/admission")(gated)

	pods := []*corev1.Pod{mother.Pod("default", "stayer", mother.OnNode("a")), gated}
	s := snapshot([]*corev1.Node{awaitingNode("a", "web-rs", time.Minute), node("b")}, pods)
	c := clientFor(s)

	step, err := executor.Advance(context.Background(), c, s, "a", engineConfig(), drainPolicy())
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	if !step.Done || !step.Failed {
		t.Errorf("expected the drain abandoned, got %+v", step)
	}
	if !strings.Contains(step.Reason, "default/gated") {
		t.Errorf("the reason should name the pod: %q", step.Reason)
	}
	if nodeFrom(t, c, "a").Spec.Unschedulable {
		t.Error("the node was left cordoned")
	}
}
