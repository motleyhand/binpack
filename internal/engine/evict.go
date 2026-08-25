package engine

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// safeToEvict is the cluster-autoscaler's own annotation. binpack honours it
// because the autoscaler does: a node it will not drain is a node it will not
// remove, whatever binpack thinks.
const safeToEvict = "cluster-autoscaler.kubernetes.io/safe-to-evict"

// safeToEvictLocalVolumes is the autoscaler's narrower escape hatch: a
// comma-separated list of the pod's own volume names whose contents are
// disposable. cluster-autoscaler utils/drain/drain.go,
// SafeToEvictLocalVolumesKey.
//
// Narrower is the point. safeToEvict=true also waives the bare-pod and
// kube-system rules for the same pod, so an operator told to reach for it
// because a scratch directory blocks a drain widens rather more than they were
// asked to — which is why the autoscaler's own FAQ offers this one first.
const safeToEvictLocalVolumes = "cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes"

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
	// running a kube-system pod that is not a DaemonSet or mirror pod — for
	// BlockingSystemPodDistruptionTimeout after that pod was created, after
	// which it evicts the pod and removes the node anyway.
	SkipNodesWithSystemPods bool

	// SkipNodesWithLocalStorage mirrors --skip-nodes-with-local-storage,
	// which defaults to true. Under it a pod with a disk-backed emptyDir or a
	// hostPath volume blocks removal, unless the volume is named in
	// safeToEvictLocalVolumes or the whole pod is annotated safeToEvict.
	SkipNodesWithLocalStorage bool

	// BlockingSystemPodDistruptionTimeout mirrors
	// --blocking-system-pod-distruption-timeout, whose misspelling is
	// upstream's and is kept here so the two can be grepped together. It is
	// how long after a kube-system pod's creation the autoscaler waits before
	// evicting it anyway, and it defaults to an hour.
	//
	// The one field here whose default is not true of every autoscaler
	// binpack supports. The grace arrived in cluster-autoscaler 1.33 and the
	// stated floor is 1.30, so zero means the blocker never expires — what
	// 1.30 to 1.32 do, and deliberately not what zero means upstream. See
	// api/v1alpha1.Autoscaler for why that reading is the safe one.
	BlockingSystemPodDistruptionTimeout time.Duration
}

// DefaultEvictConfig mirrors the cluster-autoscaler's own defaults at the
// version binpack pins, which is what an autoscaler nobody has reconfigured is
// running.
//
// Not more than that, and it used to claim more: the two skip flags are
// ordinary pflags on the autoscaler binary and nothing restricts who sets
// them. They are unexposed only where the platform runs the autoscaler in a
// control plane the operator cannot reach — DOKS, LKE, Vultr, Scaleway. AKS
// exposes both through its cluster-autoscaler profile and ships
// skip-nodes-with-local-storage=false; on EKS, kOps, Rancher and every
// self-hosted install the flags are whatever the manifest says. Where the
// cluster differs, policy.autoscaler is how an operator says so.
func DefaultEvictConfig() EvictConfig {
	return EvictConfig{
		SkipNodesWithSystemPods:             true,
		SkipNodesWithLocalStorage:           true,
		BlockingSystemPodDistruptionTimeout: time.Hour,
	}
}

// Eviction refusal codes. Stable, since metrics and diagnose key on them.
const (
	BlockedBarePod        = "bare-pod"
	BlockedSafeToEvict    = "safe-to-evict-false"
	BlockedLocalStorage   = "local-storage"
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

	// Transient marks a blocker that resolves without anybody doing anything,
	// so a drain already under way should wait for it rather than end over it.
	//
	// Read only by the executor, and only mid-drain: at selection a blocker of
	// any kind is a reason to choose a different candidate, where refusing
	// costs nothing at all.
	//
	// Deliberately opt-in, and deliberately a small set. False is the safe
	// default — a durable blocker classed transient turns an abandonment that
	// is immediate and explains itself into a wait that only the drain's stall
	// timeout ends — so a blocker added later is durable until somebody argues
	// otherwise, rather than transient by omission.
	Transient bool
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
	now time.Time,
) []EvictionBlocker {
	var blockers []EvictionBlocker

	// Demand carries a representative pod as well as a count, so a shortfall
	// can name a workload rather than only a budget.
	demand := make(map[*policyv1.PodDisruptionBudget]*budgetDemand)

	for _, pod := range evicting {
		// Matched once and reused: a budget both exempts a kube-system pod
		// from the blanket rule and governs whether it can actually go.
		matched := matchingPDBs(pod, pdbs)

		if b := checkPod(pod, matched, cfg, now); b != nil {
			blockers = append(blockers, *b)
			continue
		}

		// Everything below is disruption-budget arithmetic, and the eviction
		// API does none of it for such a pod.
		if pdbsIgnored(pod) {
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
			pdb := matched[0]
			// An unready pod may be evictable without consuming the budget at
			// all, in which case counting it would block a drain Kubernetes
			// would have allowed.
			if evictableWhileUnready(pod, pdb) {
				continue
			}
			d, ok := demand[pdb]
			if !ok {
				d = &budgetDemand{first: pod}
				demand[pdb] = d
			}
			d.count++
		}
	}

	return append(blockers, checkAllowances(demand)...)
}

func checkPod(pod *corev1.Pod, matched []*policyv1.PodDisruptionBudget, cfg EvictConfig,
	now time.Time,
) *EvictionBlocker {
	// An explicit refusal beats everything, including the exemptions below.
	if pod.Annotations[safeToEvict] == "false" {
		return &EvictionBlocker{Pod: pod, Code: BlockedSafeToEvict,
			Message: fmt.Sprintf("%s is annotated %s=false", podRef(pod), safeToEvict)}
	}

	// An explicit permission likewise beats the heuristics below: it is how an
	// operator says a scratch volume is disposable.
	permitted := pod.Annotations[safeToEvict] == "true"

	// No mirror-pod branch, deliberately. A static pod is node-local — see
	// [NodeBound] — so it is neither relocated nor evicted, and both callers
	// of this function filter node-local pods out before reaching it. The
	// branch that used to sit here declared one Blocking, and it could not
	// fire: a node carrying nothing but a static pod is drained without
	// comment, by binpack and by the cluster-autoscaler alike.

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
	//
	// And only until the autoscaler's own patience runs out, which for a
	// steady-state cluster is a blocker that has already expired: the timeout
	// runs from the pod's creation, so the coredns replica that has been up
	// for a fortnight is one the autoscaler will evict rather than route
	// around.
	if cfg.SkipNodesWithSystemPods && pod.Namespace == metav1.NamespaceSystem &&
		!permitted && len(matched) == 0 && !pastSystemPodGrace(pod, cfg, now) {
		return &EvictionBlocker{Pod: pod, Code: BlockedSystemPod,
			Message: fmt.Sprintf(
				"%s is a kube-system pod with no PodDisruptionBudget, so the autoscaler will not remove its node yet",
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
func checkAllowances(demand map[*policyv1.PodDisruptionBudget]*budgetDemand) []EvictionBlocker {
	var blockers []EvictionBlocker

	for pdb, d := range demand {
		wanted := d.count
		// A budget whose controller has not caught up with its spec is
		// refused by the eviction API outright, whatever its recorded
		// allowance says — see checkAndDecrement in the eviction subresource.
		// Trusting a stale number is how a drain gets approved and then
		// rejected mid-flight.
		if pdb.Status.ObservedGeneration < pdb.Generation {
			blockers = append(blockers, EvictionBlocker{
				Pod:  d.first,
				Code: BlockedPDBStale,
				PDB:  pdb.Namespace + "/" + pdb.Name,
				// Transient by definition, and the only blocker that is. The
				// disruption controller resyncs the budget on the very update
				// that bumped its generation, so the gap closes in one sync —
				// well inside an evaluation interval. Nothing is wrong with
				// the application; binpack is looking at the budget between
				// the write and the recomputation.
				Transient: true,
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
			Pod:  d.first,
			Code: BlockedPDBInsufficint,
			PDB:  pdb.Namespace + "/" + pdb.Name,
			Message: fmt.Sprintf(
				"%s/%s allows %d disruption(s) but the drain needs %d",
				pdb.Namespace, pdb.Name, allowed, wanted),
		})
	}

	// Sorted, because the first of these is the one an operator is actually
	// sent to: [CheckEvictable] appends them after the per-pod blockers, and
	// what reads Blockers[0] is the last-failure annotation, the sentence in
	// the DrainAbandoned event, the pause reason and `binpack explain`.
	//
	// The walk above is over a map, and the runtime reseeds a map walk on
	// every range statement — so a node under two short budgets was blamed on
	// a different one from one evaluation to the next, with nothing about the
	// cluster having changed, and `explain` could name a third while
	// previewing the same node. Total, because one budget yields at most one
	// blocker: the branches above are exclusive and each ends the iteration.
	slices.SortFunc(blockers, func(a, b EvictionBlocker) int {
		return cmp.Compare(a.PDB, b.PDB)
	})

	return blockers
}

// budgetDemand is how many evictions a drain needs from one budget, and a pod
// to name when reporting a shortfall.
type budgetDemand struct {
	count int
	first *corev1.Pod
}

// evictableWhileUnready reports whether an unready pod can be evicted without
// drawing on its budget's allowance.
//
// The eviction subresource skips the budget entirely for a pod that is not
// Ready, in two cases: when the budget sets AlwaysAllow, and — under the
// default policy — when the application it guards is currently healthy.
// Evicting an already-broken replica disrupts nothing, so it is not charged
// for.
//
// Counting such a pod would block a drain Kubernetes would have permitted: a
// CrashLoopBackOff pod under a zero-allowance budget is evictable, and
// refusing to move it is exactly the kind of stuck node binpack exists to
// clear.
func evictableWhileUnready(pod *corev1.Pod, pdb *policyv1.PodDisruptionBudget) bool {
	if isPodReady(pod) {
		return false
	}
	// An unset policy is IfHealthyBudget: policy/v1 says the empty value
	// "corresponds to the IfHealthyBudget policy", which is also what the
	// eviction subresource does with it.
	policy := policyv1.IfHealthyBudget
	if declared := pdb.Spec.UnhealthyPodEvictionPolicy; declared != nil {
		policy = *declared
	}

	switch policy {
	case policyv1.AlwaysAllow:
		return true
	case policyv1.IfHealthyBudget:
		// Free only while the guarded application is meeting its budget.
		return pdb.Status.CurrentHealthy >= pdb.Status.DesiredHealthy && pdb.Status.DesiredHealthy > 0
	default:
		// A closed set, deliberately, and deliberately not what a current API
		// server does with a value it does not know — its own handler falls
		// through to the IfHealthyBudget arm. policy/v1 asks clients for the
		// opposite: "Additional policies may be added in the future. Clients
		// making eviction decisions should disallow eviction of unhealthy
		// pods if they encounter an unrecognized policy in this field."
		//
		// Validation means such a value can only come from an API server
		// newer than the k8s.io/api binpack was built against, so the policy
		// binpack has never seen is by construction one whose rule it cannot
		// know. Excusing the pod would under-count the demand the API server
		// will actually meet, approve the drain, and leave the node
		// half-emptied on the first 429 — the one unsound direction available
		// here. Charging costs a consolidation.
		return false
	}
}

// pdbsIgnored reports whether the eviction subresource would delete this pod
// without consulting a disruption budget at all.
//
// Mirrors canIgnorePDB in kubernetes pkg/registry/core/pod/storage/eviction.go.
// The gate sits above every budget in Create: getPodDisruptionBudgets, the
// more-than-one refusal and checkAndDecrement are all downstream of it, so for
// such a pod no budget is read, none is charged, and the two-budget HTTP 500
// cannot happen.
//
// Pending is the arm that changes binpack's answer, and it is a pod bound to a
// node whose containers have not started — an image still pulling, a volume
// still attaching. Which is to say the wedged pod an operator most wants
// consolidated away, and the node binpack used to refuse for ever.
//
// The other three arms are already filtered out before CheckEvictable sees
// them: Occupies drops Succeeded and Failed, and Classify calls a terminating
// pod node-bound. They are mirrored anyway rather than trimmed to what is
// currently reachable, because the predicate is named for upstream's and a
// caller that stopped filtering would otherwise get the arithmetic silently
// wrong.
func pdbsIgnored(pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodPending, corev1.PodSucceeded, corev1.PodFailed:
		return true
	}
	return pod.DeletionTimestamp != nil
}

func isPodReady(pod *corev1.Pod) bool {
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// matchingPDBs returns every budget selecting pod, ordered by name.
//
// By name rather than in the order they were listed, because two of the answers
// built from this list name budgets rather than counting them: the
// more-than-one refusal names the first two of them, and diagnose lists them
// all. The list arrives from a watch-backed cache in the controller and from a
// live client in `binpack explain`, so an order inherited from it is an order
// the two frontends do not share. Settled here rather than at each of those
// sites, so there is one answer to which budget gets named.
//
// The name alone is total: every budget here is in the pod's own namespace,
// which is the first thing the loop below checks.
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
			// Skipped, because the eviction subresource skips it: "This
			// object has an invalid selector, it does not match the pod",
			// getPodDisruptionBudgets in the same file as canIgnorePDB. The
			// disruption controller cannot read it either, so such a budget
			// has never had a healthy status computed for it.
			//
			// This used to append, on the grounds that a budget nobody can
			// reason about should be assumed to apply. That reasoning holds
			// for an allowance and inverts for a match: a matched set larger
			// than the API server's is what produces the more-than-one
			// refusal, which binpack reports as permanent and unfixable by
			// retry. So the cautious-looking reading was the one that refused
			// a node the API would have drained.
			//
			// The object is not dropped, only disarmed — diagnose reports it
			// as pdb-unparseable-selector.
			continue
		}
		if selector.Matches(labels.Set(pod.Labels)) {
			matched = append(matched, pdb)
		}
	}

	slices.SortFunc(matched, func(a, b *policyv1.PodDisruptionBudget) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return matched
}

// pastSystemPodGrace reports whether the autoscaler would by now evict a
// blocking kube-system pod rather than leave its node alone.
//
// Mirrors isBspPassedDisruptionTimeout in cluster-autoscaler
// simulator/drainability/rules/system/rule.go, including its guard on an unset
// creation timestamp: an object that has not been through an API server has no
// age, and reading the zero time as an instant would hand every such pod the
// grace at once.
//
// A timeout of zero is no grace at all, which is what the autoscaler did
// before 1.33 — the rule and its flag arrived there, and binpack supports 1.30
// upward. Not upstream's reading of zero, which would expire the blocker
// immediately; that behaviour is already spelled SkipNodesWithSystemPods
// false, whereas an autoscaler with no grace is otherwise unsayable. It also
// means a caller that fills in the two booleans and forgets this field gets
// the strict answer rather than no rule at all.
func pastSystemPodGrace(pod *corev1.Pod, cfg EvictConfig, now time.Time) bool {
	if cfg.BlockingSystemPodDistruptionTimeout <= 0 || pod.CreationTimestamp.IsZero() {
		return false
	}
	return now.After(pod.CreationTimestamp.Add(cfg.BlockingSystemPodDistruptionTimeout))
}

// firstLocalStorage names a volume whose presence stops the autoscaler
// removing the node, mirroring drain.HasBlockingLocalStorage and isLocalVolume
// in cluster-autoscaler utils/drain/drain.go.
//
// Two exclusions, and binpack had neither. A memory-medium emptyDir is tmpfs —
// RAM with a filesystem over it, nothing of which reaches the node's disk — so
// upstream excludes it by name, while binpack blocked on it and thereby
// refused every node of every service-mesh cluster, since Istio and Linkerd
// inject exactly that volume into every pod they mesh. And the operator may
// name individual volumes as disposable, which is the autoscaler's documented
// answer and a narrower one than safeToEvict.
func firstLocalStorage(pod *corev1.Pod) (string, bool) {
	disposable := nonBlockingVolumes(pod)

	for _, v := range pod.Spec.Volumes {
		if disposable[v.Name] {
			continue
		}
		switch {
		case v.EmptyDir != nil:
			// tmpfs. It is charged to the pod's memory and it dies with the
			// pod, so there is nothing on the node to lose.
			if v.EmptyDir.Medium == corev1.StorageMediumMemory {
				continue
			}
			return v.Name + " (emptyDir)", true
		case v.HostPath != nil:
			return v.Name + " (hostPath)", true
		}
	}
	return "", false
}

// nonBlockingVolumes reads the volume names an operator has declared
// disposable, mirroring getNonBlockingVolumes upstream.
//
// Split on commas and nothing else, because upstream splits on commas and
// nothing else: a value written "cache, data" exempts a volume named " data"
// there, which is to say it exempts nothing. Trimming here would be the more
// forgiving choice and the wrong one — binpack's job is to predict what the
// autoscaler will do with this pod, and being right about a typo the
// autoscaler is wrong about is how a drain gets approved and then stalls.
func nonBlockingVolumes(pod *corev1.Pod) map[string]bool {
	value := pod.Annotations[safeToEvictLocalVolumes]
	if value == "" {
		return nil
	}
	out := make(map[string]bool)
	for _, name := range strings.Split(value, ",") {
		out[name] = true
	}
	return out
}

func podRef(pod *corev1.Pod) string { return pod.Namespace + "/" + pod.Name }
