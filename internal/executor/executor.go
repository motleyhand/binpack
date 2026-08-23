// Package executor performs the only changes binpack makes to a cluster —
// cordoning a node, annotating it, handing it back, and evicting a pod — and
// sequences them into a drain.
//
// Two halves, and the split is the thing to know. executor.go is the writes
// and nothing else: how each individual change is made, and what each way of
// failing means. Keeping every write in one file means the set of things
// binpack can do to a cluster is enumerable by reading it.
//
// drain.go is the drain protocol, and that is policy. It decides what an
// evaluation does next to a node already being drained: whether to hand over
// to the cluster-autoscaler, whether a replacement is still in flight, which
// pod is evicted next, and what to call a revalidation failure. Whether a node
// should be drained at all remains [engine.Decide]'s answer, and what should
// happen next given one node's state remains [drain.Assess]'s — a pure
// function this package calls. What lives here is the part neither can be:
// carrying a step out and deciding the next one are a single unit against a
// node whose state is the annotations these writes set.
//
// The doc this replaces said "It holds no policy" and located the ordering in
// "the drain protocol", as a component living somewhere else. That was true
// when the package was four writes and the protocol had no home yet; it
// survived unchanged through all fifteen commits that have touched drain.go
// since, which is how a package doc ends up denying what the package holds.
package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Writer is the subset of a Kubernetes client the executor needs.
//
// Narrow on purpose: Patch for the node changes, and Create for the eviction
// subresource. There is no Delete here and there never should be — binpack
// removes no object, ever. A node goes away because the cluster-autoscaler
// removes it, and a pod goes away because its eviction was accepted.
type Writer interface {
	Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error
	SubResource(subResource string) client.SubResourceClient
}

// Cordon marks a node unschedulable.
//
// Patched rather than updated, and from a fresh object rather than the one
// passed in. The node handed here comes from a shared informer cache, and
// writing to it corrupts the copy every other consumer in the process is
// reading. A patch also avoids overwriting a change somebody else made between
// the read and the write, which an update carrying a whole object would.
func Cordon(ctx context.Context, w Writer, node *corev1.Node) error {
	return patchNode(ctx, w, node, `{"spec":{"unschedulable":true}}`)
}

// HandBack makes a node schedulable again and records why, in one patch.
//
// Every branch of a drain ends here or with the node deleted. A cordoned node
// the autoscaler never removes is capacity that is paid for and cannot be
// used, so this is the call that must work even when everything else has
// failed.
//
// One patch rather than an uncordon followed by a record, because the state
// between them cannot be read. A node that is schedulable and still marked is
// byte-identical to one left by a Begin that stopped between its own two
// writes, and that state's repair is to cordon and carry on — the opposite of
// what the call that produced it wanted. A merge patch carries spec and
// metadata together, so there is no reason to leave a state whose two readings
// call for opposite corrections.
func HandBack(
	ctx context.Context, w Writer, node *corev1.Node, labels, annotations map[string]string,
) error {
	patch := map[string]any{"spec": map[string]any{"unschedulable": false}}
	if meta := metadataPatch(labels, annotations); len(meta) > 0 {
		patch["metadata"] = meta
	}
	return marshalAndPatch(ctx, w, node, patch)
}

// Annotate sets annotations on a node, and removes those given an empty value.
//
// This is where a drain's recovery state lives, because it has to outlive the
// process that started it: a binpack that is restarted or loses leader
// election mid-drain must be able to find out what it was doing. See
// ADR-0007.
func Annotate(ctx context.Context, w Writer, node *corev1.Node, annotations map[string]string) error {
	return patchMeta(ctx, w, node, nil, annotations)
}

// patchMeta sets labels and annotations in one patch, removing those given an
// empty value.
//
// One patch rather than two because a merge patch on metadata carries both, and
// a drain marker that appeared without its label — or outlived it — would make
// the label a thing an operator learns not to trust.
func patchMeta(
	ctx context.Context, w Writer, node *corev1.Node, labels, annotations map[string]string,
) error {
	meta := metadataPatch(labels, annotations)
	if len(meta) == 0 {
		return nil
	}
	return marshalAndPatch(ctx, w, node, map[string]any{"metadata": meta})
}

// metadataPatch renders labels and annotations as the metadata half of a merge
// patch, empty for nothing to say.
func metadataPatch(labels, annotations map[string]string) map[string]any {
	// Marshalled rather than concatenated: an annotation value is arbitrary
	// text, and a drain's recorded failure reason is a message from the API
	// server. Building this by hand would put unescaped quotes into a patch.
	//
	// A nil value deletes the key, which is how a finished drain clears its
	// own marker.
	nullable := func(in map[string]string) map[string]*string {
		out := make(map[string]*string, len(in))
		for key, value := range in {
			if value == "" {
				out[key] = nil
				continue
			}
			out[key] = &value
		}
		return out
	}

	meta := map[string]any{}
	if len(labels) > 0 {
		meta["labels"] = nullable(labels)
	}
	if len(annotations) > 0 {
		meta["annotations"] = nullable(annotations)
	}
	return meta
}

func marshalAndPatch(
	ctx context.Context, w Writer, node *corev1.Node, document map[string]any,
) error {
	patch, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("building the patch for %s: %w", node.Name, err)
	}
	return patchNode(ctx, w, node, string(patch))
}

func patchNode(ctx context.Context, w Writer, node *corev1.Node, patch string) error {
	// A bare object carrying only the name: nothing is read back from the
	// cache copy, and nothing written through to it.
	target := &corev1.Node{}
	target.Name = node.Name

	if err := w.Patch(ctx, target, client.RawPatch(client.Merge.Type(), []byte(patch))); err != nil {
		return fmt.Errorf("patching node %s: %w", node.Name, err)
	}
	return nil
}

// Eviction outcomes. The distinction between them is the whole reason this
// function exists rather than a bare API call, and [Advance] is where it is
// spent: a refusal nothing can lift ends the drain there, rather than travelling
// out of the executor as an error like every other failure.
var (
	// ErrEvictionBlocked is a disruption budget refusing right now. Retryable:
	// the budget's allowance is recomputed as replicas become healthy, so the
	// same eviction may well succeed a moment later. Returned to the caller
	// rather than retried here, because one step per evaluation is the shape of
	// this whole package: the controller ends the evaluation on it and re-reads
	// the cluster on the next interval, which is the retry — against a fresh
	// snapshot rather than the stale one that produced the refusal. It used to
	// end the process instead, which made the pod's restart backoff, rather
	// than the interval, the clock every drain ran on.
	ErrEvictionBlocked = errors.New("a PodDisruptionBudget currently allows no disruption")

	// ErrEvictionImpossible is the API refusing outright, which it does when a
	// pod is covered by more than one budget. Not retryable, by anyone: the
	// eviction subresource does not arbitrate between budgets and returns 500
	// rather than a retryable 429. Retrying is how a drain hangs forever, and
	// returning it would end an evaluation over a condition no evaluation can
	// fix. So the drain is handed back instead — the same conclusion the next
	// revalidation reaches through CheckEvictable, without the trip through
	// the caller's error path first.
	ErrEvictionImpossible = errors.New("the eviction API refuses this pod outright")
)

// multiplePDBs is the eviction subresource's own wording for the one 500 that
// is permanent, from pkg/registry/core/pod/storage/eviction.go.
//
// Matching on a message is unpleasant and is the only signal there is: the
// status carries no reason or cause distinguishing it, just code 500. So the
// choice is which way to be wrong when the wording changes, and this errs
// towards *retrying*. A permanent refusal treated as transient makes a drain
// stall, which the progress bound already catches and backs off from; a
// transient failure treated as permanent abandons a drain that would have
// worked, and etcd hiccups and webhook timeouts are far commoner than a pod
// under two budgets.
//
// That asymmetry is what the match is for, and it is now paid in behaviour
// rather than only argued: a match here ends the drain on the spot. Upstream
// rewording costs whatever a non-match costs today — the error leaves the
// executor, and revalidation abandons the node under the same code on a later
// evaluation. A false match costs a drain that would have worked.
const multiplePDBs = "more than one PodDisruptionBudget"

// Evict asks the API server to remove a pod, respecting disruption budgets.
//
// A create against the eviction subresource, not a delete. The difference is
// the point: a delete ignores PodDisruptionBudgets entirely, and binpack must
// never be the thing that took an application below its declared availability.
//
// A pod that is already gone counts as success. It is what was wanted, and a
// drain that failed because something else finished its work first would be a
// strange thing to report.
func Evict(ctx context.Context, w Writer, pod *corev1.Pod) error {
	target := &corev1.Pod{}
	target.Name, target.Namespace = pod.Name, pod.Namespace

	eviction := &policyv1.Eviction{ObjectMeta: metav1.ObjectMeta{
		Name: pod.Name, Namespace: pod.Namespace,
	}}

	err := w.SubResource("eviction").Create(ctx, target, eviction)
	switch {
	case err == nil, apierrors.IsNotFound(err):
		return nil
	case apierrors.IsTooManyRequests(err):
		return fmt.Errorf("%s/%s: %w", pod.Namespace, pod.Name, ErrEvictionBlocked)
	case apierrors.IsInternalError(err) && strings.Contains(err.Error(), multiplePDBs):
		return fmt.Errorf("%s/%s: %w", pod.Namespace, pod.Name, ErrEvictionImpossible)
	default:
		return fmt.Errorf("evicting %s/%s: %w", pod.Namespace, pod.Name, err)
	}
}
