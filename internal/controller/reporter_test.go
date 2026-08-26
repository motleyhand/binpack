package controller

import (
	"context"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	eventsv1 "k8s.io/api/events/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"

	"github.com/motleyhand/binpack/internal/mother"
)

// recordingSink is an events.EventSink that answers every call successfully
// and remembers which one it was.
//
// The sink is the seam RBAC sees: each of its three methods is one verb on
// events.k8s.io/events, and the ClusterRole has to grant exactly the ones the
// broadcaster reaches for. Substituting it leaves the broadcaster itself
// untouched — production builds the same one, `events.NewBroadcaster(
// &events.EventSinkImpl{…})` at controller-runtime v0.24.1
// pkg/cluster/cluster.go:298 — so what this observes is the real call
// sequence with the API server replaced.
type recordingSink struct {
	mu     sync.Mutex
	called []string
	calls  chan struct{}
}

func (s *recordingSink) record(method string) {
	s.mu.Lock()
	s.called = append(s.called, method)
	s.mu.Unlock()

	// Buffered and non-blocking: the broadcaster's writes happen on goroutines
	// nothing joins, and a sink that blocked on an unread channel would wedge
	// the very path under test.
	select {
	case s.calls <- struct{}{}:
	default:
	}
}

func (s *recordingSink) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.called)
}

func (s *recordingSink) Create(_ context.Context, event *eventsv1.Event) (*eventsv1.Event, error) {
	s.record("Create")
	return event, nil
}

func (s *recordingSink) Update(_ context.Context, event *eventsv1.Event) (*eventsv1.Event, error) {
	s.record("Update")
	return event, nil
}

func (s *recordingSink) Patch(_ context.Context, event *eventsv1.Event, _ []byte) (*eventsv1.Event, error) {
	s.record("Patch")
	return event, nil
}

// TestTheEventRecorderNeverIssuesAnUpdate is what makes the ClusterRole's
// `events.k8s.io/events: update` safe to drop, and it is the reason the verb
// went after the test rather than before it.
//
// The grant was documented as the thing that lets the recorder aggregate
// repeats of an identical event into one object with a count. It is not:
// aggregation is a strategic-merge patch. client-go's only write path is
// `recordEvent` (v0.36.4, tools/events/event_broadcaster.go), which calls
// `sink.Patch` when the event has a Series and `sink.Create` otherwise or when
// the series object has gone; `EventSinkImpl.Update` exists to satisfy the
// interface and has no call site. Reading that once is not enough to remove a
// permission on somebody else's cluster, though — the next dependency bump
// could add one, and the symptom would be a 403 on a path that only runs after
// the same event has been emitted twice, which is to say not in anyone's
// smoke test.
//
// So both directions are asserted. `Update` must not be called, and `Create`
// and `Patch` both must be — the second half is what stops this passing
// vacuously if the wiring ever stops recording at all, and it is what pins the
// series path specifically. Confirmed by construction: emitting once instead
// of twice leaves Patch unreached and fails.
func TestTheEventRecorderNeverIssuesAnUpdate(t *testing.T) {
	sink := &recordingSink{calls: make(chan struct{}, 8)}
	broadcaster := events.NewBroadcaster(sink)
	t.Cleanup(broadcaster.Shutdown)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if err := broadcaster.StartRecordingToSinkWithContext(ctx); err != nil {
		t.Fatalf("starting the broadcaster: %v", err)
	}

	// The scheme controller-runtime would have used: it defaults to client-go's
	// when manager.Options names none, and managerOptions names none.
	reporter := broadcastReporter{
		recorder: broadcaster.NewRecorder(scheme.Scheme, reportingController),
	}

	// Twice, identically. The aggregation key is the type, action, reason,
	// reporting controller and instance, and the object — so two of these are
	// one series, and the second is the write that exercises the patch.
	node := mother.SmallNode("node-a")
	for range 2 {
		if err := reporter.emit(ctx, node, "Consolidate", "WouldDrain", "a note"); err != nil {
			t.Fatalf("emitting: %v", err)
		}
	}

	// Waiting on the sink rather than on a duration. The recorder hands each
	// event to a goroutine and returns, so there is nothing to join; a sleep
	// long enough to be reliable is a slow test and a sleep short enough to be
	// quick is a flaky one.
	deadline := time.After(10 * time.Second)
	for range 2 {
		select {
		case <-sink.calls:
		case <-deadline:
			t.Fatalf("only %v reached the sink in ten seconds", sink.methods())
		}
	}

	called := sink.methods()
	if slices.Contains(called, "Update") {
		t.Errorf("the recorder issued an Update (%v), so dropping the verb from the "+
			"ClusterRole would 403 on the aggregation path", called)
	}
	for _, want := range recorderMethods {
		if !slices.Contains(called, want) {
			t.Errorf("the recorder never issued a %s (%v), so this test is not "+
				"observing the write path it claims to", want, called)
		}
	}
}

// recorderMethods are the event-sink methods client-go's recorder issues on
// binpack's behalf, on the long-running path.
//
// Named here rather than inline because two tests need the same fact and only
// one of them can prove it. This file's broadcaster test is the proof: it
// drives the real recorder and observes what reaches the sink.
// TestTheChartGrantsWhatTheCodeCalls is the consumer, and it needs these
// because the chart has to grant them — the recorder writes whatever dryRun
// is set to, and nothing in Go bounds a library's writes the way an interface
// bounds binpack's own. Dropping events.k8s.io/events: patch from the chart
// and the reference together left them agreeing and left every decision after
// the first failing to aggregate.
var recorderMethods = []string{"Create", "Patch"}

// TestTheEventCreateIsBinpacksOnlyWriteOutsideTheExecutor holds the
// enumeration internal/executor's package doc tells a reviewer to trust.
//
// That doc says every write binpack's own code makes to a Node or a Pod is in
// executor.go, and R4-017's RBAC argument and R4-003's availability argument
// both rest on what a reviewer concludes from reading it: binpack removes no
// object, ever. True of executor.Writer, which has Patch and SubResource and
// no Delete. It was not true of binpack's code, because this file creates
// Events and held the whole of client.Writer to do it — Create, Update,
// Patch, Delete and DeleteAllOf — so a Delete was one line away in a file
// that enumeration does not cover, and would compile, pass, and be invisible.
//
// One method, counted rather than assumed. A verb added here has to be added
// to the executor package doc's exception too, or this fails.
//
// What it does not hold, and what the doc no longer claims: the writes the
// libraries binpack embeds make on its behalf. client-go's event recorder
// creates and patches events.k8s.io Events on the long-running path, and the
// manager writes a Lease and core Events for leader election. No Go interface
// bounds those, and a test that pretended otherwise would be the same
// over-claim in a new place. docs/reference/rbac.md enumerates them, and no
// rule the chart renders grants delete on anything — which is where "binpack
// removes no object, ever" is actually enforced.
func TestTheEventCreateIsBinpacksOnlyWriteOutsideTheExecutor(t *testing.T) {
	const permitted = 1
	if n := reflect.TypeFor[eventWriter]().NumMethod(); n != permitted {
		t.Fatalf("eventWriter has %d methods and this test asserts %d: internal/executor's "+
			"package doc names this Event create as the one write binpack's own code makes "+
			"outside that file — widen the doc with the new verb, or say here why it "+
			"needs none", n, permitted)
	}
}
