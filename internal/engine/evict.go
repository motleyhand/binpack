package engine

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// safeToEvict is the cluster-autoscaler's own annotation. binpack honours it
// because the autoscaler does: a node it will not drain is a node it will not
// remove, whatever binpack thinks.
const safeToEvict = "cluster-autoscaler.kubernetes.io/safe-to-evict"

// EvictConfig describes what binpack believes about the cluster-autoscaler,
// since the autoscaler is what ultimately removes the node. Every field
// mirrors one of its flags, and the defaults mirror the autoscaler's.
//
// Getting these wrong does not make binpack unsafe, it makes it wrong: it
// would drain a node the autoscaler then declines to remove, which the drain
// verification catches and backs off from — a wasted drain rather than a
// scale-up.
type EvictConfig struct {
	// SkipNodesWithSystemPods mirrors --skip-nodes-with-system-pods, which
	// defaults to true. Under it the autoscaler will not remove a node
	// running a kube-system pod that is not a DaemonSet or mirror pod.
	SkipNodesWithSystemPods bool

	// SkipNodesWithLocalStorage mirrors --skip-nodes-with-local-storage,
	// which defaults to true. Under it a pod with an emptyDir or hostPath
	// volume blocks removal unless annotated safe-to-evict.
	SkipNodesWithLocalStorage bool
}

// DefaultEvictConfig mirrors the cluster-autoscaler's own defaults, which is
// what a managed platform will be running since neither flag is exposed.
func DefaultEvictConfig() EvictConfig {
	return EvictConfig{
		SkipNodesWithSystemPods:   true,
		SkipNodesWithLocalStorage: true,
	}
}

// Eviction refusal codes. Stable, since metrics and diagnose key on them.
const (
	BlockedBarePod        = "bare-pod"
	BlockedSafeToEvict    = "safe-to-evict-false"
	BlockedLocalStorage   = "local-storage"
	BlockedMirrorPod      = "mirror-pod"
	BlockedSystemPod      = "system-pod"
	BlockedMultiplePDBs   = "multiple-pdbs"
	BlockedPDBInsufficint = "pdb-insufficient"
	BlockedPDBStale       = "pdb-stale"
)

// EvictionBlocker is one reason a drain would not complete.
type EvictionBlocker struct {
	// Pod is the pod that cannot be evicted. For a PDB shortfall it is the
	// first such pod; PDB names the budget involved.
	Pod  *corev1.Pod
	Code string
	// PDB is set when the blocker is a disruption budget, so diagnose can
	// point at the object to change.
	PDB     string
	Message string
}

// CheckEvictable predicts whether every pod that must leave a node actually
// can, before anything is cordoned.
//
// The alternative — evict and find out — is what the descheduler does, and it
// is why a failed drain there leaves a half-emptied node. Prediction is what
// lets binpack decline a drain it cannot finish.
//
// evicting is the set of pods that would be evicted: relocatable and
// expendable ones, not node-local pods. allPods is the whole cluster, needed
// because a disruption budget's allowance is a property of every pod it
// matches, not only those on this node.
func CheckEvictable(
	evicting []*corev1.Pod,
	pdbs []*policyv1.PodDisruptionBudget,
	cfg EvictConfig,
) []EvictionBlocker {
	var blockers []EvictionBlocker

	demand := make(map[*policyv1.PodDisruptionBudget]int)

	for _, pod := range evicting {
		// Matched once and reused: a budget both exempts a kube-system pod
		// from the blanket rule and governs whether it can actually go.
		matched := matchingPDBs(pod, pdbs)

		if b := checkPod(pod, matched, cfg); b != nil {
			blockers = append(blockers, *b)
			continue
		}

		// The eviction subresource does not arbitrate between budgets. It
		// returns HTTP 500 — not a retryable 429 — so such a pod cannot be
		// evicted by anything: not binpack, not kubectl drain, not the
		// autoscaler. Both budgets meanwhile report healthy, which is what
		// makes this so hard to spot by hand.
		if len(matched) > 1 {
			blockers = append(blockers, EvictionBlocker{
				Pod: pod, Code: BlockedMultiplePDBs,
				PDB: matched[0].Name,
				Message: fmt.Sprintf(
					"%s is selected by %d PodDisruptionBudgets (%s, %s), which the eviction API refuses outright",
					podRef(pod), len(matched), matched[0].Name, matched[1].Name),
			})
			continue
		}
		if len(matched) == 1 {
			demand[matched[0]]++
		}
	}

	return append(blockers, checkAllowances(demand)...)
}

func checkPod(pod *corev1.Pod, matched []*policyv1.PodDisruptionBudget, cfg EvictConfig) *EvictionBlocker {
	// An explicit refusal beats everything, including the exemptions below.
	if pod.Annotations[safeToEvict] == "false" {
		return &EvictionBlocker{Pod: pod, Code: BlockedSafeToEvict,
			Message: fmt.Sprintf("%s is annotated %s=false", podRef(pod), safeToEvict)}
	}

	// An explicit permission likewise beats the heuristics below: it is how an
	// operator says a scratch volume is disposable.
	permitted := pod.Annotations[safeToEvict] == "true"

	if _, mirror := pod.Annotations[corev1.MirrorPodAnnotationKey]; mirror {
		return &EvictionBlocker{Pod: pod, Code: BlockedMirrorPod,
			Message: fmt.Sprintf("%s is a static pod managed by the kubelet and cannot be evicted", podRef(pod))}
	}

	// A pod with no *controlling* owner is not recreated after eviction, so
	// evicting it destroys it. The autoscaler refuses for the same reason.
	//
	// Counting any owner reference would be wrong: references exist for
	// garbage collection too, and one with Controller unset means nothing is
	// responsible for replacing the pod.
	if metav1.GetControllerOf(pod) == nil && !permitted {
		return &EvictionBlocker{Pod: pod, Code: BlockedBarePod,
			Message: fmt.Sprintf("%s has no controller, so evicting it would delete it permanently", podRef(pod))}
	}

	if cfg.SkipNodesWithLocalStorage && !permitted {
		if volume, ok := firstLocalStorage(pod); ok {
			return &EvictionBlocker{Pod: pod, Code: BlockedLocalStorage,
				Message: fmt.Sprintf(
					"%s uses local storage (%s); annotate it %s=true if the contents are disposable",
					podRef(pod), volume, safeToEvict)}
		}
	}

	// A kube-system pod blocks node removal only when no budget covers it.
	// Per the autoscaler's own documentation, configuring a PDB for such a
	// pod "overrides the default strategy of not touching the node" — the
	// budget then governs, and its allowance is checked below like any other.
	if cfg.SkipNodesWithSystemPods && pod.Namespace == metav1.NamespaceSystem &&
		!permitted && len(matched) == 0 {
		return &EvictionBlocker{Pod: pod, Code: BlockedSystemPod,
			Message: fmt.Sprintf(
				"%s is a kube-system pod with no PodDisruptionBudget, so the autoscaler will not remove its node",
				podRef(pod))}
	}

	return nil
}

// checkAllowances verifies each budget can absorb the drain's demand.
//
// Demand is aggregated across the whole drain rather than checked per pod: a
// drain is many evictions drawing on the same allowances, so two pods of one
// Deployment on a node with disruptionsAllowed of 1 pass a per-pod check and
// then half-drain it.
func checkAllowances(demand map[*policyv1.PodDisruptionBudget]int) []EvictionBlocker {
	var blockers []EvictionBlocker

	for pdb, wanted := range demand {
		// A budget whose controller has not caught up with its spec is
		// refused by the eviction API outright, whatever its recorded
		// allowance says — see checkAndDecrement in the eviction subresource.
		// Trusting a stale number is how a drain gets approved and then
		// rejected mid-flight.
		if pdb.Status.ObservedGeneration < pdb.Generation {
			blockers = append(blockers, EvictionBlocker{
				Code: BlockedPDBStale,
				PDB:  pdb.Namespace + "/" + pdb.Name,
				Message: fmt.Sprintf(
					"%s/%s was edited and its controller has not caught up (generation %d, observed %d), so evictions are refused until it does",
					pdb.Namespace, pdb.Name, pdb.Generation, pdb.Status.ObservedGeneration),
			})
			continue
		}

		allowed := int(pdb.Status.DisruptionsAllowed)
		if wanted <= allowed {
			continue
		}
		blockers = append(blockers, EvictionBlocker{
			Code: BlockedPDBInsufficint,
			PDB:  pdb.Namespace + "/" + pdb.Name,
			Message: fmt.Sprintf(
				"%s/%s allows %d disruption(s) but the drain needs %d",
				pdb.Namespace, pdb.Name, allowed, wanted),
		})
	}

	return blockers
}

// matchingPDBs returns every budget selecting pod, in declaration order.
func matchingPDBs(pod *corev1.Pod, pdbs []*policyv1.PodDisruptionBudget) []*policyv1.PodDisruptionBudget {
	var matched []*policyv1.PodDisruptionBudget

	for _, pdb := range pdbs {
		if pdb.Namespace != pod.Namespace {
			continue
		}
		// A null selector matches no pods; an empty one matches every pod in
		// the namespace. Treating them alike would either miss a real budget
		// or invent one that does not apply.
		if pdb.Spec.Selector == nil {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err != nil {
			// An unparseable selector is a budget nobody can reason about.
			// Assuming it does not apply would be the unsafe direction.
			matched = append(matched, pdb)
			continue
		}
		if selector.Matches(labels.Set(pod.Labels)) {
			matched = append(matched, pdb)
		}
	}

	return matched
}

func firstLocalStorage(pod *corev1.Pod) (string, bool) {
	for _, v := range pod.Spec.Volumes {
		switch {
		case v.EmptyDir != nil:
			return v.Name + " (emptyDir)", true
		case v.HostPath != nil:
			return v.Name + " (hostPath)", true
		}
	}
	return "", false
}

func podRef(pod *corev1.Pod) string { return pod.Namespace + "/" + pod.Name }
