package collect

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
func Snapshot(ctx context.Context, reader Reader, now time.Time) (engine.Snapshot, error) {
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

	if s.Autoscaler, err = autoscaler(ctx, reader); err != nil {
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

	var replicaSets appsv1.ReplicaSetList
	if err := reader.List(ctx, &replicaSets); err != nil {
		return nil, fmt.Errorf("listing replicasets: %w", err)
	}
	for i := range replicaSets.Items {
		rs := &replicaSets.Items[i]
		out[engine.OwnerRef{Namespace: rs.Namespace, APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID}] = &rs.Spec.Template
	}

	var statefulSets appsv1.StatefulSetList
	if err := reader.List(ctx, &statefulSets); err != nil {
		return nil, fmt.Errorf("listing statefulsets: %w", err)
	}
	for i := range statefulSets.Items {
		sts := &statefulSets.Items[i]
		out[engine.OwnerRef{Namespace: sts.Namespace, APIVersion: "apps/v1", Kind: "StatefulSet", Name: sts.Name, UID: sts.UID}] = &sts.Spec.Template
	}

	// DaemonSet pods are node-local and never relocated, so their templates are
	// never consulted for placement. They are read anyway because a DaemonSet
	// is one of the kinds a pod can name as its controller, and an absent entry
	// is indistinguishable from a kind binpack cannot read.
	var daemonSets appsv1.DaemonSetList
	if err := reader.List(ctx, &daemonSets); err != nil {
		return nil, fmt.Errorf("listing daemonsets: %w", err)
	}
	for i := range daemonSets.Items {
		ds := &daemonSets.Items[i]
		out[engine.OwnerRef{Namespace: ds.Namespace, APIVersion: "apps/v1", Kind: "DaemonSet", Name: ds.Name, UID: ds.UID}] = &ds.Spec.Template
	}

	var jobs batchv1.JobList
	if err := reader.List(ctx, &jobs); err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		out[engine.OwnerRef{Namespace: job.Namespace, APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID}] = &job.Spec.Template
	}

	return out, nil
}

// autoscaler reads the cluster-autoscaler's published status.
//
// A missing ConfigMap yields a not-running autoscaler rather than an error.
// That is a diagnosis, not a failure: binpack should report "nothing here will
// remove a drained node" clearly rather than exiting with a stack trace.
func autoscaler(ctx context.Context, reader Reader) (engine.Autoscaler, error) {
	var cm corev1.ConfigMap
	err := reader.Get(ctx, client.ObjectKey{
		Namespace: StatusConfigMapNamespace,
		Name:      StatusConfigMapName,
	}, &cm)

	switch {
	case apierrors.IsNotFound(err):
		return engine.Autoscaler{}, nil
	case err != nil:
		return engine.Autoscaler{}, fmt.Errorf("reading %s/%s: %w",
			StatusConfigMapNamespace, StatusConfigMapName, err)
	}

	document, ok := cm.Data[statusKey]
	if !ok {
		return engine.Autoscaler{}, nil
	}

	return ParseAutoscalerStatus(document)
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
