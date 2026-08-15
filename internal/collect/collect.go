package collect

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/motleyhand/binpack/internal/engine"
)

// Reader is the subset of a Kubernetes client binpack uses to read a cluster.
//
// Deliberately narrow. Everything here is a list or a get; there is no verb in
// this interface that changes anything, so the read path cannot mutate a
// cluster even by mistake. Writes live in the executor.
type Reader interface {
	kubernetes.Interface
}

// Snapshot reads the cluster into the value the engine decides on.
//
// Nothing is transformed on the way through: the engine works on API types, so
// this lists objects and hands them over. The only interpretation is the
// autoscaler's status document, which is YAML inside a ConfigMap.
func Snapshot(ctx context.Context, client Reader, now time.Time) (engine.Snapshot, error) {
	s := engine.Snapshot{Now: now}

	nodes, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return s, fmt.Errorf("listing nodes: %w", err)
	}
	for i := range nodes.Items {
		s.Nodes = append(s.Nodes, &nodes.Items[i])
	}

	pods, err := client.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return s, fmt.Errorf("listing pods: %w", err)
	}
	for i := range pods.Items {
		s.Pods = append(s.Pods, &pods.Items[i])
	}

	pdbs, err := client.PolicyV1().PodDisruptionBudgets(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return s, fmt.Errorf("listing pod disruption budgets: %w", err)
	}
	for i := range pdbs.Items {
		s.PDBs = append(s.PDBs, &pdbs.Items[i])
	}

	s.Autoscaler, err = autoscaler(ctx, client)
	if err != nil {
		return s, err
	}

	return s, nil
}

// autoscaler reads the cluster-autoscaler's published status.
//
// A missing ConfigMap yields a not-running autoscaler rather than an error.
// That is a diagnosis, not a failure: binpack should report "nothing here will
// remove a drained node" clearly rather than exiting with a stack trace.
func autoscaler(ctx context.Context, client Reader) (engine.Autoscaler, error) {
	cm, err := client.CoreV1().
		ConfigMaps(StatusConfigMapNamespace).
		Get(ctx, StatusConfigMapName, metav1.GetOptions{})

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
