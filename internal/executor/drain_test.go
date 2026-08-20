package executor_test

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	return drain.Policy{StallTimeout: 10 * time.Minute, RemovalTimeout: 15 * time.Minute}
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
func marked(name string, startedAgo, progressAgo time.Duration, remaining string) *corev1.Node {
	ann := map[string]string{
		engine.AnnotationDrainStarted:  at.Add(-startedAgo).Format(time.RFC3339),
		engine.AnnotationDrainProgress: at.Add(-progressAgo).Format(time.RFC3339),
	}
	if remaining != "" {
		ann[engine.AnnotationDrainPodsRemaining] = remaining
	}
	return node(name, mother.Cordoned(), mother.NodeAnnotations(ann))
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

func TestACompletedPodIsNotAnInFlightEviction(t *testing.T) {
	// A Succeeded pod held by a finalizer is not occupying the node, so the
	// assessment does not count it. Treating it as in flight would wait on it
	// every evaluation while evictable pods sat there, until the drain was
	// abandoned as stalled.
	done := mother.Pod("default", "finished", mother.OnNode("a"),
		mother.Terminating(at.Add(-time.Hour), time.Minute))
	done.Status.Phase = corev1.PodSucceeded

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
		context.Background(), c, n, "test", "because", at); err != nil {
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
		context.Background(), c, n, "test", "because", at); err != nil {
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
		{"the autoscaler's status went stale",
			func(s *engine.Snapshot) { s.Autoscaler.LastProbe = at.Add(-time.Hour) }},
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
