// Package executor performs the only changes binpack makes to a cluster:
// cordoning a node, annotating it, uncordoning it, and evicting a pod.
//
// It holds no policy. Whether a node should be drained is [engine.Decide]'s
// answer and the order things happen in is the drain protocol's; what lives
// here is how each individual change is made, and what each way of failing
// means. Keeping every write in one package means the set of things binpack
// can do to a cluster is enumerable by reading one file.
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

// Uncordon makes a node schedulable again.
//
// Every branch of a drain ends here or with the node deleted. A cordoned node
// the autoscaler never removes is capacity that is paid for and cannot be
// used, so this is the call that must work even when everything else has
// failed.
func Uncordon(ctx context.Context, w Writer, node *corev1.Node) error {
	return patchNode(ctx, w, node, `{"spec":{"unschedulable":false}}`)
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
	if len(labels) == 0 && len(annotations) == 0 {
		return nil
	}

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

	patch, err := json.Marshal(map[string]any{"metadata": meta})
	if err != nil {
		return fmt.Errorf("building the metadata patch for %s: %w", node.Name, err)
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
// function exists rather than a bare API call.
var (
	// ErrEvictionBlocked is a disruption budget refusing right now. Retryable:
	// the budget's allowance is recomputed as replicas become healthy, so the
	// same eviction may well succeed a moment later.
	ErrEvictionBlocked = errors.New("a PodDisruptionBudget currently allows no disruption")

	// ErrEvictionImpossible is the API refusing outright, which it does when a
	// pod is covered by more than one budget. Not retryable, by anyone: the
	// eviction subresource does not arbitrate between budgets and returns 500
	// rather than a retryable 429. Retrying is how a drain hangs forever.
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
