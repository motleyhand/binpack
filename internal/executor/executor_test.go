package executor_test

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/motleyhand/binpack/internal/executor"
	"github.com/motleyhand/binpack/internal/mother"
)

func ctx() context.Context { return context.Background() }

func withNode(t *testing.T, node *corev1.Node) client.Client {
	t.Helper()
	return fake.NewClientBuilder().WithObjects(node).Build()
}

func reread(t *testing.T, c client.Client, name string) *corev1.Node {
	t.Helper()
	var node corev1.Node
	if err := c.Get(ctx(), client.ObjectKey{Name: name}, &node); err != nil {
		t.Fatalf("re-reading node: %v", err)
	}
	return &node
}

func TestCordonAndHandBack(t *testing.T) {
	node := mother.SmallNode("a")
	c := withNode(t, node)

	if err := executor.Cordon(ctx(), c, node); err != nil {
		t.Fatalf("Cordon: %v", err)
	}
	if !reread(t, c, "a").Spec.Unschedulable {
		t.Error("the node is still schedulable after a cordon")
	}

	// The cordon and the record travel together, so the hand-back is asserted
	// on both halves: a node that came back schedulable but kept its marker is
	// the half-state the single patch exists to make unreachable.
	err := executor.HandBack(ctx(), c, node,
		map[string]string{"binpack.motleyhand.com/draining": ""},
		map[string]string{"binpack.motleyhand.com/last-failure": "because"})
	if err != nil {
		t.Fatalf("HandBack: %v", err)
	}

	after := reread(t, c, "a")
	if after.Spec.Unschedulable {
		t.Error("the node is still cordoned after a hand-back")
	}
	if after.Annotations["binpack.motleyhand.com/last-failure"] != "because" {
		t.Errorf("the hand-back recorded nothing: %v", after.Annotations)
	}
}

func TestTheCachedObjectIsNeverWrittenThrough(t *testing.T) {
	// The node handed in comes from a shared informer cache, and writing to it
	// corrupts the copy every other consumer in the process is reading. This
	// cannot be linted, so it is asserted: the argument must come back
	// unchanged from every call that changes the cluster.
	node := mother.SmallNode("a", mother.NodeAnnotations(map[string]string{"keep": "me"}))
	before := node.DeepCopy()
	c := withNode(t, node)

	for name, call := range map[string]func() error{
		"Cordon":   func() error { return executor.Cordon(ctx(), c, node) },
		"HandBack": func() error { return executor.HandBack(ctx(), c, node, nil, nil) },
		"Annotate": func() error {
			return executor.Annotate(ctx(), c, node, map[string]string{"binpack.motleyhand.com/skip": "true"})
		},
	} {
		if err := call(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if diff := node.Spec.Unschedulable != before.Spec.Unschedulable; diff {
			t.Errorf("%s wrote spec.unschedulable back onto the cached object", name)
		}
		if len(node.Annotations) != len(before.Annotations) {
			t.Errorf("%s wrote annotations back onto the cached object: %v", name, node.Annotations)
		}
	}
}

func TestAnnotateAddsRemovesAndLeavesOthersAlone(t *testing.T) {
	// A merge patch, so an annotation binpack does not name must survive. A
	// node carries labels and annotations from its provider and from anything
	// else running in the cluster, and clobbering those would be its own
	// incident.
	node := mother.SmallNode("a", mother.NodeAnnotations(map[string]string{
		"doks.digitalocean.com/managed": "true",
		"binpack.motleyhand.com/skip":   "true",
	}))
	c := withNode(t, node)

	err := executor.Annotate(ctx(), c, node, map[string]string{
		"binpack.motleyhand.com/drain-started": "2026-08-16T10:00:00Z",
		"binpack.motleyhand.com/skip":          "", // removed
	})
	if err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	got := reread(t, c, "a").Annotations
	if got["binpack.motleyhand.com/drain-started"] != "2026-08-16T10:00:00Z" {
		t.Errorf("the drain marker was not written: %v", got)
	}
	if _, present := got["binpack.motleyhand.com/skip"]; present {
		t.Errorf("an empty value should remove the annotation: %v", got)
	}
	if got["doks.digitalocean.com/managed"] != "true" {
		t.Errorf("an annotation binpack does not own was lost: %v", got)
	}
}

func TestAnnotateEscapesWhatItIsGiven(t *testing.T) {
	// A recorded failure reason is a message from the API server, and those
	// contain quotes. Building the patch by hand would produce invalid JSON
	// exactly when a drain has already gone wrong.
	node := mother.SmallNode("a")
	c := withNode(t, node)

	reason := `evicting "web": pods "web" is forbidden: unexpected {"a":1}`
	if err := executor.Annotate(ctx(), c, node,
		map[string]string{"binpack.motleyhand.com/last-failure": reason}); err != nil {
		t.Fatalf("Annotate: %v", err)
	}

	if got := reread(t, c, "a").Annotations["binpack.motleyhand.com/last-failure"]; got != reason {
		t.Errorf("the reason did not survive the patch:\n got %q\nwant %q", got, reason)
	}
}

// evictor stands in for the API server's answers to an eviction, which is the
// part of this worth testing: the outcomes differ in whether retrying helps.
type evictor struct{ err error }

func (e evictor) Patch(context.Context, client.Object, client.Patch, ...client.PatchOption) error {
	return nil
}
func (e evictor) SubResource(string) client.SubResourceClient { return subResource(e) }

type subResource struct{ err error }

func (s subResource) Create(context.Context, client.Object, client.Object, ...client.SubResourceCreateOption) error {
	return s.err
}
func (s subResource) Get(context.Context, client.Object, client.Object, ...client.SubResourceGetOption) error {
	return nil
}
func (s subResource) Update(context.Context, client.Object, ...client.SubResourceUpdateOption) error {
	return nil
}
func (s subResource) Patch(context.Context, client.Object, client.Patch, ...client.SubResourcePatchOption) error {
	return nil
}
func (s subResource) Apply(context.Context, runtime.ApplyConfiguration, ...client.SubResourceApplyOption) error {
	return nil
}

func TestEvictDistinguishesItsFailures(t *testing.T) {
	pod := mother.Pod("default", "web")
	gr := schema.GroupResource{Resource: "pods"}

	for _, tc := range []struct {
		name string
		from error
		want error
	}{
		{"accepted", nil, nil},
		// Already gone is what was wanted. A drain that failed because
		// something else finished its work first would be a strange report.
		{"already gone", apierrors.NewNotFound(gr, "web"), nil},
		// A budget refusing right now. Its allowance is recomputed as replicas
		// become healthy, so the same eviction may succeed a moment later.
		{"budget exhausted", apierrors.NewTooManyRequests("nope", 1), executor.ErrEvictionBlocked},
		// Two budgets covering one pod, in the eviction subresource's own
		// words. The API does not arbitrate between them and returns 500
		// rather than a retryable 429 — retrying is how a drain hangs forever.
		{"refused outright", apierrors.NewInternalError(errors.New(
			"This pod has more than one PodDisruptionBudget, which the eviction subresource does not support.")),
			executor.ErrEvictionImpossible},
		// Also a 500, and nothing to do with budgets. Abandoning a drain over
		// an etcd hiccup or a webhook timeout would give up on one that works
		// a moment later, and those are far commoner than the case above.
		{"transient server error", apierrors.NewInternalError(errors.New("etcdserver: request timed out")), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := executor.Evict(ctx(), evictor{tc.from}, pod)
			switch {
			case tc.name == "transient server error":
				if errors.Is(err, executor.ErrEvictionImpossible) {
					t.Errorf("a transient 500 was classified as permanent: %v", err)
				}
				if !apierrors.IsInternalError(err) {
					t.Errorf("the original error should still be inspectable, got %v", err)
				}
			case tc.want == nil && err != nil:
				t.Errorf("wanted success, got %v", err)
			case tc.want != nil && !errors.Is(err, tc.want):
				t.Errorf("got %v, want it to wrap %v", err, tc.want)
			}
		})
	}
}

func TestEvictSurfacesAnythingItDoesNotRecognise(t *testing.T) {
	// A forbidden eviction is a missing permission, not a busy budget, and
	// treating it as retryable would spin forever against a role that will
	// never change on its own.
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Resource: "pods"}, "web", errors.New("no"))

	err := executor.Evict(ctx(), evictor{forbidden}, mother.Pod("default", "web"))

	if errors.Is(err, executor.ErrEvictionBlocked) || errors.Is(err, executor.ErrEvictionImpossible) {
		t.Errorf("a forbidden eviction was classified as a budget outcome: %v", err)
	}
	if !apierrors.IsForbidden(err) {
		t.Errorf("the original error should still be inspectable, got %v", err)
	}
}
