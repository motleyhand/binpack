package collect

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/motleyhand/binpack/internal/engine"
)

// Reader is how binpack reads a cluster: controller-runtime's read interface,
// which is Get and List and nothing else. There is no verb here that changes
// anything, so the read path cannot mutate a cluster even by mistake. Writes
// live in the executor.
//
// One interface for both frontends, and that is the point. `binpack explain`
// satisfies it with a direct client built from a kubeconfig; the controller
// satisfies it with the manager's watch-backed cache. Both list the same
// objects and neither `collect` nor the engine translates them, so both
// frontends decide on the same cluster — a property that would otherwise rest
// on two code paths being kept in step by hand.
//
// The shared reader is only half of that, and the weaker half. The two do not
// list in the same order: the cache walks a Go map, whose order differs
// between two calls in the same process, and the direct client is ordered by
// storage key. So the guarantee is not "the same reader" but "the same inputs
// through the same function give the same answer", which holds because every
// ordering inside the engine is total and settles its ties on names rather
// than on list position. A reader handing over identical objects in a
// different order is otherwise a reader the engine can tell apart.
//
// It guarantees the inputs, not the answer. One field of Snapshot is not read
// from the cluster at all: LastDrain is the controller's own memory of when it
// last finished a drain, because a completed drain deletes the node that would
// have recorded it. So `explain` decides as though no drain had ever happened,
// and says so rather than answering silently — see the after-drain cooldown in
// its output.
type Reader = client.Reader

// Snapshot reads the cluster into the value the engine decides on.
//
// Nothing is transformed on the way through: the engine works on API types, so
// this lists objects and hands them over. The only interpretation is the
// autoscaler's status document, which is YAML inside a ConfigMap.
func Snapshot(ctx context.Context, reader Reader, now time.Time, status StatusRef) (engine.Snapshot, error) {
	s := engine.Snapshot{Now: now}

	var nodes corev1.NodeList
	if err := reader.List(ctx, &nodes); err != nil {
		return s, fmt.Errorf("listing nodes: %w", err)
	}
	for i := range nodes.Items {
		s.Nodes = append(s.Nodes, &nodes.Items[i])
	}

	var pods corev1.PodList
	if err := reader.List(ctx, &pods); err != nil {
		return s, fmt.Errorf("listing pods: %w", err)
	}
	for i := range pods.Items {
		s.Pods = append(s.Pods, &pods.Items[i])
	}

	var pdbs policyv1.PodDisruptionBudgetList
	if err := reader.List(ctx, &pdbs); err != nil {
		return s, fmt.Errorf("listing pod disruption budgets: %w", err)
	}
	for i := range pdbs.Items {
		s.PDBs = append(s.PDBs, &pdbs.Items[i])
	}

	var err error
	if s.Templates, err = templates(ctx, reader); err != nil {
		return s, err
	}

	if s.Autoscaler, err = autoscaler(ctx, reader, status); err != nil {
		return s, err
	}

	return s, nil
}

// templates reads the pod template of every controller that owns pods.
//
// binpack asks whether a *replacement* pod would fit and, without these, was
// answering about the pod that is leaving. The two diverge exactly where it
// matters most: a pod resized downward in place carries smaller requests than
// its replacement will, and nothing on the pod records that this happened. See
// ADR-0006.
//
// Kinds binpack cannot read a template for — pods created directly by an
// operator's own controller — are simply absent, and the engine refuses to
// move a pod it cannot predict rather than guessing from the running one.
func templates(ctx context.Context, reader Reader) (map[engine.OwnerRef]*corev1.PodTemplateSpec, error) {
	out := map[engine.OwnerRef]*corev1.PodTemplateSpec{}

	for _, src := range templateSources {
		list := src.List()
		if err := reader.List(ctx, list); err != nil {
			return nil, fmt.Errorf("listing %s: %w", src.Resource, err)
		}

		// ExtractList hands back pointers *into* the list's items rather than
		// copies — its allocating twin is a separate function for exactly that
		// reason — so the templates below alias the objects that were listed,
		// as they did when this read four typed slices by index. On the
		// controller path those objects belong to the shared informer cache
		// and are read-only, like everything else the engine is given.
		items, err := meta.ExtractList(list)
		if err != nil {
			return nil, fmt.Errorf("reading the %s list: %w", src.Resource, err)
		}
		for _, item := range items {
			obj, isObject := item.(client.Object)
			if !isObject {
				return nil, fmt.Errorf("reading the %s list: %T is not a Kubernetes object",
					src.Resource, item)
			}
			out[engine.OwnerRef{
				Namespace:  obj.GetNamespace(),
				APIVersion: src.APIVersion,
				Kind:       src.Kind,
				Name:       obj.GetName(),
				UID:        obj.GetUID(),
			}] = src.Template(obj)
		}
	}

	return out, nil
}

// TemplateSource is one controller kind binpack reads pod templates from.
//
// The set is closed, small, and — from here on — written down once. It used to
// be written in five places: the four list-and-key blocks this replaces, the
// cache's per-kind restrictions and its trim in internal/controller, and the
// fixture in internal/mother that decides whether a test can observe a kind at
// all. Only one of the five failed loudly when they disagreed. A kind missing
// from the chart's RBAC produces a Forbidden on every evaluation; a kind
// missing from the cache restrictions gets an unrestricted informer, one
// missing from the trim is cached with its whole status, and one missing from
// the fixture leaves every test asserting the behaviour from before the change
// and still passing — which is the worst of the four, because it makes the
// work look finished.
//
// Which four kinds, and why only these, is explained where operators read it:
// docs/reference/rbac.md, docs/reference/diagnostics.md and ADR-0006. Those
// stay prose — they answer "why" and a generated list would not — and the
// tests beside this file hold them to what the slice says.
type TemplateSource struct {
	// APIVersion and Kind are the strings an ownerReference carries, so they
	// are what a pod names its controller by and what the engine keys its
	// template map on. Stated here rather than read back off a listed object,
	// because a typed List leaves TypeMeta empty: the scheme knows the kind,
	// the decoded object does not carry it.
	APIVersion string
	Kind       string

	// Resource is the plural the API server serves the kind under, which is
	// the name an RBAC rule grants. Stated rather than derived from Kind:
	// pluralisation belongs to the API server, and a rule that lowercases and
	// appends an "s" is right for these four by luck rather than by contract.
	Resource string

	// List returns an empty list to read the kind into.
	List func() client.ObjectList

	// Object returns an empty object of the kind. controller-runtime's cache
	// takes one as the key its per-kind restrictions hang off.
	Object func() client.Object

	// Template returns the one field binpack reads — the pod template a
	// replacement would be built from — as a pointer into the object.
	Template func(client.Object) *corev1.PodTemplateSpec

	// ClearStatus empties the object's status if it is of this kind, and
	// reports whether it was. Status is the type-specific half of what the
	// cache drops before storing a controller; the rest is metadata and is
	// dropped the same way for every kind.
	ClearStatus func(client.Object) bool
}

// Group is the API group the kind belongs to, as an RBAC rule names it.
//
// The core group's apiVersion is a bare version with no slash in it, so
// splitting on the separator and taking the first half would report "v1" as a
// group for anything that ever moves there.
func (s TemplateSource) Group() string {
	group, _, qualified := strings.Cut(s.APIVersion, "/")
	if !qualified {
		return ""
	}
	return group
}

// TemplateSources is every controller kind binpack can read a pod template
// from.
//
// Enumerable on purpose, and for the same reason [engine.SkipCodes] is: this
// is a closed set that several other things have to agree with, and a set
// nothing can range over is one they agree with only by inspection.
func TemplateSources() []TemplateSource {
	return slices.Clone(templateSources)
}

// The four kinds, and everything that differs between them.
//
// DaemonSets earn their row despite nothing ever asking for their template:
// their pods are node-local, never relocated, and [engine.NodeBound]
// recognises them by owner kind before a template is looked up. They are read
// because a DaemonSet is one of the kinds a pod can name as its controller,
// and an absent entry is indistinguishable from a kind binpack genuinely
// cannot read — the distinction the whole refusal path turns on. Dropping the
// row would cost nothing today and would make that correct only for as long
// as the engine's shortcut stays where it is.
var templateSources = []TemplateSource{
	{
		APIVersion: "apps/v1", Kind: "ReplicaSet", Resource: "replicasets",
		List:     func() client.ObjectList { return &appsv1.ReplicaSetList{} },
		Object:   func() client.Object { return &appsv1.ReplicaSet{} },
		Template: func(o client.Object) *corev1.PodTemplateSpec { return &o.(*appsv1.ReplicaSet).Spec.Template },
		ClearStatus: func(o client.Object) bool {
			obj, ok := o.(*appsv1.ReplicaSet)
			if ok {
				obj.Status = appsv1.ReplicaSetStatus{}
			}
			return ok
		},
	},
	{
		APIVersion: "apps/v1", Kind: "StatefulSet", Resource: "statefulsets",
		List:     func() client.ObjectList { return &appsv1.StatefulSetList{} },
		Object:   func() client.Object { return &appsv1.StatefulSet{} },
		Template: func(o client.Object) *corev1.PodTemplateSpec { return &o.(*appsv1.StatefulSet).Spec.Template },
		ClearStatus: func(o client.Object) bool {
			obj, ok := o.(*appsv1.StatefulSet)
			if ok {
				obj.Status = appsv1.StatefulSetStatus{}
			}
			return ok
		},
	},
	{
		APIVersion: "apps/v1", Kind: "DaemonSet", Resource: "daemonsets",
		List:     func() client.ObjectList { return &appsv1.DaemonSetList{} },
		Object:   func() client.Object { return &appsv1.DaemonSet{} },
		Template: func(o client.Object) *corev1.PodTemplateSpec { return &o.(*appsv1.DaemonSet).Spec.Template },
		ClearStatus: func(o client.Object) bool {
			obj, ok := o.(*appsv1.DaemonSet)
			if ok {
				obj.Status = appsv1.DaemonSetStatus{}
			}
			return ok
		},
	},
	{
		APIVersion: "batch/v1", Kind: "Job", Resource: "jobs",
		List:     func() client.ObjectList { return &batchv1.JobList{} },
		Object:   func() client.Object { return &batchv1.Job{} },
		Template: func(o client.Object) *corev1.PodTemplateSpec { return &o.(*batchv1.Job).Spec.Template },
		ClearStatus: func(o client.Object) bool {
			obj, ok := o.(*batchv1.Job)
			if ok {
				obj.Status = batchv1.JobStatus{}
			}
			return ok
		},
	},
}

// autoscaler reads the cluster-autoscaler's published status from the one
// place the operator says it is published.
//
// A missing ConfigMap yields a not-running autoscaler rather than an error.
// That is a diagnosis, not a failure: binpack should report "nothing here will
// remove a drained node" clearly rather than exiting with a stack trace. What
// it must not do is state that as a cluster fact, so the two ways of finding
// nothing are told apart on the way out — the object was not there, or it was
// there and said nothing. See [engine.Autoscaler.Live].
//
// An empty namespace or name is the one thing here that is not a diagnosis. It
// means a caller lost the configured value, and a Get for a namespaced object
// without a namespace finds nothing — so it would arrive downstream wearing
// the same clothes as a cluster that genuinely has no autoscaler, which is
// precisely the confident-and-wrong report these parameters exist to end.
func autoscaler(ctx context.Context, reader Reader, status StatusRef) (engine.Autoscaler, error) {
	switch {
	case status.Namespace == "":
		return engine.Autoscaler{}, fmt.Errorf(
			"no namespace to read the cluster-autoscaler status from: set discovery.autoscalerNamespace")
	case status.Name == "":
		return engine.Autoscaler{}, fmt.Errorf(
			"no name to read the cluster-autoscaler status by: set discovery.autoscalerStatusName")
	}

	var cm corev1.ConfigMap
	err := reader.Get(ctx, client.ObjectKey{
		Namespace: status.Namespace,
		Name:      status.Name,
	}, &cm)

	switch {
	case apierrors.IsNotFound(err):
		return engine.Autoscaler{}, nil
	case err != nil:
		return engine.Autoscaler{}, fmt.Errorf("reading %s: %w", status, err)
	}

	document, ok := cm.Data[statusKey]
	if !ok {
		// The object exists, so binpack has read the operator's cluster and
		// found the autoscaler's own status empty. Distinct from not finding
		// the object, and the refusal says which.
		return engine.Autoscaler{StatusFound: true}, nil
	}

	a, err := ParseAutoscalerStatus(document)
	if err != nil {
		return engine.Autoscaler{}, fmt.Errorf("reading %s: %w", status, err)
	}
	return a, nil
}

// PodsOn returns the pods assigned to a node, for rendering.
func PodsOn(s engine.Snapshot, node *corev1.Node) []*corev1.Pod {
	var out []*corev1.Pod
	for _, pod := range s.Pods {
		if pod.Spec.NodeName == node.Name {
			out = append(out, pod)
		}
	}
	return out
}
