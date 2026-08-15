package collect

import (
	"context"
	"fmt"
	"time"

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
// satisfies it with the manager's watch-backed cache. Neither `collect` nor
// the engine can tell which it has, which is what guarantees that what
// `explain` prints is what `run` will do — a property that would otherwise
// rest on two code paths being kept in step by hand.
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
	if s.Autoscaler, err = autoscaler(ctx, reader); err != nil {
		return s, err
	}

	return s, nil
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
