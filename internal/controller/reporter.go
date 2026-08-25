package controller

import (
	"context"
	"fmt"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	eventsv1 "k8s.io/api/events/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// reportingController identifies binpack in the events it writes. Fixed, like
// the annotations and the lease: it is what people filter on.
const reportingController = "binpack"

// reporter puts a decision where a cluster user will find it.
//
// Two implementations, because the two execution models want incompatible
// things from an event and neither can serve both.
//
// The controller runs for weeks and re-decides every minute, so it wants the
// event recorder's aggregation: events sharing a key collapse into a single
// object carrying a count and a first and last timestamp, and a decision that
// holds for an hour reads as one line saying so. It pays for that with
// asynchrony — the recorder hands each event to a goroutine and returns, and
// nothing waits for the write.
//
// The key is the type, action, reason, reporting controller and instance, and
// the object the event is about — and not the note
// (k8s.io/client-go@v0.36.4, tools/events/event_broadcaster.go, getKey). So
// "identical" is a weaker condition than it sounds, and a note is written
// once per series and never refreshed: every later event of the series is
// dropped in favour of bumping the count. Both [report] and [refusal] are
// written to that constraint, which is why neither note carries anything that
// moves.
//
// A --once run exits in milliseconds, and there fire-and-forget means
// forgotten: the process is gone before that goroutine posts anything, so the
// CronJob reports nothing at all. It writes its event synchronously instead,
// accepting one object per invocation in exchange for the write having
// actually happened.
//
// Found by running --once against a real cluster and finding no event. A fake
// recorder cannot show this, because the entire defect is in what happens
// after the call returns — which is also why the synchronous path is the one
// that can be tested.
type reporter interface {
	emit(ctx context.Context, node *corev1.Node, reason, action, note string) error
}

// broadcastReporter is the long-running controller's reporter.
type broadcastReporter struct{ recorder events.EventRecorder }

func (r broadcastReporter) emit(
	_ context.Context, node *corev1.Node, reason, action, note string,
) error {
	// Asynchronous, so an event can be lost if the process stops between the
	// call and the write. Harmless here: the next tick re-decides and
	// re-reports, and the recorder will aggregate it into the same object.
	r.recorder.Eventf(node, nil, corev1.EventTypeNormal, reason, action, "%s", note)
	return nil
}

// eventWriter is the one write binpack makes outside internal/executor.
//
// One method, and the narrowing is the point. internal/executor's package doc
// tells a reviewer that reading executor.go enumerates what binpack can do to
// a cluster, and executor.Writer holds Patch and the eviction subresource and
// no Delete — which is what "binpack removes no object, ever" is read off. A
// field of type client.Writer here held Create, Update, Patch, Delete and
// DeleteAllOf, so that conclusion was correct about the executor and wrong
// about the process: a Delete was one line away, in a file the enumeration
// does not cover, and would have compiled and passed.
//
// The narrowing costs nothing — mgr.GetClient() satisfies this unchanged —
// and it makes the exception the executor's doc now states an enforced one
// rather than a promise. TestTheEventWriteIsTheOnlyOtherThingBinpackWrites
// counts the methods.
type eventWriter interface {
	Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error
}

// directReporter writes the event itself and waits for the API server to
// accept it, for a process that will not be alive to retry.
type directReporter struct {
	writer   eventWriter
	instance string
	now      func() time.Time
}

func (r directReporter) emit(
	ctx context.Context, node *corev1.Node, reason, action, note string,
) error {
	now := r.now()
	return r.writer.Create(ctx, &eventsv1.Event{
		ObjectMeta: metav1.ObjectMeta{
			// A node is cluster-scoped and so has no namespace to inherit. The
			// event recorder files events about such objects under default,
			// and that is where `kubectl describe node` looks for them.
			Namespace: metav1.NamespaceDefault,
			// A name rather than a generateName, and the recorder's own scheme:
			// the object name with a hex nanosecond suffix. generateName is
			// tempting and is rejected — the API server validates the prefix
			// itself as an RFC 1123 subdomain, and "<node>." ends in a dot.
			Name: fmt.Sprintf("%s.%x", node.Name, now.UnixNano()),
		},
		EventTime:           metav1.MicroTime{Time: now},
		ReportingController: reportingController,
		ReportingInstance:   r.instance,
		Action:              action,
		Reason:              reason,
		Regarding: corev1.ObjectReference{
			APIVersion: "v1",
			Kind:       "Node",
			Name:       node.Name,
			UID:        node.UID,
		},
		Note: note,
		Type: corev1.EventTypeNormal,
	})
}

// reportingInstance names this process within the controller, the way the
// event recorder does. Truncated because the API caps it at 128 characters and
// a rejected event is a decision nobody hears about.
func reportingInstance() string {
	const max = 128

	host, err := os.Hostname()
	if err != nil || host == "" {
		return reportingController
	}
	instance := reportingController + "-" + host
	if len(instance) > max {
		return instance[:max]
	}
	return instance
}
