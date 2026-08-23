package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Severity says what a finding costs and what it takes to clear.
type Severity int

const (
	// Info is the cluster working as intended. Reported so that a question
	// already answered — why that pool never shrinks — does not have to be
	// asked again, but there is nothing to do.
	Info Severity = iota

	// Warning is the cluster-autoscaler declining by policy, or a condition
	// that resolves itself. One annotation, one budget or one recovered
	// replica changes it.
	Warning

	// Blocking is the eviction API itself refusing. No configuration of
	// binpack or of the autoscaler changes the outcome: the affected pods
	// cannot be evicted at all, so their nodes cannot be removed.
	Blocking
)

func (s Severity) String() string {
	switch s {
	case Blocking:
		return "blocking"
	case Warning:
		return "warning"
	default:
		return "info"
	}
}

// Diagnostic codes that are not eviction blockers. Eviction blockers reuse the
// codes in evict.go rather than inventing parallel names for the same
// conditions, so a blocker `explain` reports and the same condition `diagnose`
// finds are recognisably one thing.
const (
	FindingNoAutoscaler           = "no-autoscaler"
	FindingNoCandidates           = "autoscaler-no-candidates"
	FindingPoolNotAutoscaled      = "pool-not-autoscaling"
	FindingPoolAtMinimum          = "pool-at-minimum"
	FindingPDBZero                = "pdb-zero-disruptions"
	FindingPDBUnhealthy           = "pdb-workload-unhealthy"
	FindingPDBSelectsNothing      = "pdb-selects-nothing"
	FindingPDBSyncFailed          = "pdb-sync-failed"
	FindingPDBUnparseableSelector = "pdb-unparseable-selector"
	FindingPriorityBelow          = "priority-below-cutoff"
	FindingAbandonedDrain         = "abandoned-drain"
	FindingNodeInBackoff          = "node-in-backoff"
	FindingNoTemplate             = "unreadable-template"
)

// Diagnosis is one class of finding: how much it matters, what it means and
// what to do about it.
//
// Prose lives here rather than on each finding because these conditions arrive
// in bulk — fifteen namespaces deploying one Helm chart produce fifteen
// identical budgets — and a report that repeats the same paragraph fifteen
// times buries the one finding that is different. Severity belongs here for
// the same reason it is not a judgement about an individual object: what a
// condition costs is a property of the condition.
type Diagnosis struct {
	Code     string
	Severity Severity
	// Summary says what the condition is and why it costs money, once.
	Summary string
	// Fix is what to change. diagnose suggests and never acts, both because a
	// cost tool should not quietly rewrite availability policy and because
	// Flux or Argo would reconcile the change away within minutes — leaving a
	// user who cannot trust what the tool told them it had done.
	Fix string
}

// Finding is one instance of a diagnosis: which object, and its specifics.
type Finding struct {
	Diagnosis
	// Subject is the object: a node, a pool, a workload, a namespace/name, or
	// "cluster".
	Subject string
	// Detail is what is true of this subject in particular — the numbers, the
	// volume name, the budget names, which node. Never repeats Summary.
	Detail string
	// FreesNothing is true when clearing this finding would not shrink the
	// cluster today, because its subject sits only on nodes the autoscaler
	// does not manage and will therefore never remove.
	//
	// Reported rather than suppressed: the finding is real, and becomes live
	// the moment autoscaling is enabled on that pool. But sending somebody to
	// annotate five workloads that would not free a single node is how a
	// diagnostic tool loses its reader.
	FreesNothing bool
}

// diagnoses is the catalogue. Every code binpack can emit appears here exactly
// once, which is what makes the codes documentable and keeps two checks from
// describing the same condition two ways.
var diagnoses = map[string]Diagnosis{
	FindingNoAutoscaler: {
		Severity: Blocking,
		// Deliberately not "no cluster-autoscaler is running". Five
		// observations reach this finding — the status object missing, the
		// object present and empty, an autoscalerStatus other than Running, a
		// probe time absent or too old, and an autoscaler reporting the
		// cluster unhealthy — and binpack has established "nothing is running"
		// in none of them from a single Get. What it has established is the
		// consequence, which is the same in all five and is what the reader
		// needs. The Detail says which observation this was; a summary
		// asserting the autoscaler is gone above a detail saying binpack
		// cannot tell whether it is alive was the pair this replaces.
		Summary: "the cluster-autoscaler is not in a state to remove a node, so nothing will " +
			"remove one however empty it becomes",
		// The location is deliberately not named here. The autoscaler
		// publishes into the namespace it runs in, under the name it was
		// given — the upstream chart sets the first to whatever you install it
		// into — so a fix that says kube-system/cluster-autoscaler-status is
		// wrong on every cluster that chose otherwise, and says it about a
		// component that is working. Where binpack actually looked is a fact
		// about this run rather than about the code, and the report's closing
		// line names it.
		Fix: "read the detail above for what binpack found, then check that cluster " +
			"autoscaling is enabled for this cluster, that the status ConfigMap is being " +
			"updated, and that discovery.autoscalerNamespace and " +
			"discovery.autoscalerStatusName name where the autoscaler publishes it.",
	},
	FindingNoCandidates: {
		Severity: Info,
		Summary: "the cluster-autoscaler has looked for a node to remove and found none, " +
			"which is the state binpack exists to resolve",
		Fix: "not a fault, and not something to change. The autoscaler only removes nodes " +
			"that are already nearly empty; binpack moves work until one is.",
	},
	FindingPoolNotAutoscaled: {
		Severity: Info,
		Summary: "this pool is absent from the cluster-autoscaler's status, so it is not " +
			"autoscaled: its nodes can receive relocated work but will never be removed",
		Fix: "enable autoscaling on the pool if you expected it to shrink. Otherwise nothing " +
			"to do — binpack will use these nodes as destinations and never as candidates.",
	},
	FindingPoolAtMinimum: {
		Severity: Info,
		Summary: "this pool is at its configured minimum size, so no node will be removed " +
			"from it whatever the utilisation",
		Fix: "lower the pool's minimum if it is set higher than you need. binpack will not " +
			"drain a node out of a pool at its minimum either.",
	},
	FindingPDBZero: {
		Severity: Blocking,
		Summary: "this PodDisruptionBudget permits no voluntary disruption while its workload " +
			"is healthy, so no node running its pods can be drained by anything",
		Fix: "a budget requiring every replica to stay up can never allow a disruption, and " +
			"a single replica has no availability to protect in the first place. Run one more " +
			"replica, or express the budget so it leaves slack.",
	},
	FindingPDBUnhealthy: {
		Severity: Warning,
		Summary: "this PodDisruptionBudget permits no disruption because its workload is " +
			"currently short of replicas",
		Fix: "nothing to change in the budget: it will permit disruptions again once the " +
			"workload recovers. Find out why the replicas are unhealthy.",
	},
	FindingPDBSelectsNothing: {
		Severity: Warning,
		Summary:  "this PodDisruptionBudget selects no pods at all, so it protects nothing",
		Fix: "almost always a selector that does not match the workload's labels — a rename, " +
			"or a chart whose pod labels changed. Correct the selector, or delete the budget " +
			"if the workload is gone.",
	},
	FindingPDBSyncFailed: {
		Severity: Blocking,
		Summary: "this PodDisruptionBudget's controller could not compute it, so it is " +
			"reported as allowing no disruption whatever the budget actually says",
		Fix: "the budget itself is usually correct and editing it changes nothing. The " +
			"disruption controller records why on the DisruptionAllowed condition — read it " +
			"with kubectl get pdb -n NS NAME -o jsonpath='{.status.conditions}'. It most " +
			"often names a pod whose controlling owner it cannot resolve a replica count " +
			"from, because the owner is gone or its custom resource has no scale " +
			"subresource.",
	},
	FindingPDBUnparseableSelector: {
		Severity: Warning,
		Summary: "this PodDisruptionBudget's selector cannot be parsed, so the eviction API " +
			"skips it and it protects nothing",
		Fix: "an invalid label value in the selector, kept by an API server that grandfathers " +
			"one already stored. Nothing is blocked by it and nothing is guarded by it " +
			"either, so correct the value or delete the budget — but check first whether the " +
			"workload it was meant to cover is protected by anything else.",
	},
	BlockedPDBStale: {
		Severity: Warning,
		Summary: "this PodDisruptionBudget was edited and its controller has not caught up, " +
			"so the eviction API refuses disruptions until it does",
		Fix: "usually momentary and worth re-checking. If it persists, read the " +
			"DisruptionAllowed condition — kubectl get pdb -n NS NAME -o " +
			"jsonpath='{.status.conditions}' — because a reason of SyncFailed means the " +
			"controller is running and cannot compute this particular budget, which an " +
			"edit will not settle. Anything else that persists suggests it is not " +
			"running at all.",
	},
	BlockedMultiplePDBs: {
		Severity: Blocking,
		Summary: "these pods are selected by more than one PodDisruptionBudget, which the " +
			"eviction API refuses outright with HTTP 500 rather than a retryable 429",
		Fix: "nothing can evict such a pod — not binpack, not the autoscaler, not kubectl " +
			"drain — and every budget involved reports healthy, so this is invisible unless " +
			"you go looking. Narrow the selectors until exactly one covers each pod.",
	},
	BlockedBarePod: {
		Severity: Blocking,
		Summary: "these pods have no controller, so evicting one would delete it permanently " +
			"and nothing will try",
		// A Job is named with its caveat rather than dropped, because it is
		// the right answer for work that ends and the wrong one bare: an
		// eviction stamps DisruptionTarget and the Job controller counts a
		// deleted pod as a failure, so a Job at its backoffLimit — 0 for the
		// migrations and chart hooks this advice reaches — replaces nothing.
		// Recommending it unqualified would send the reader from one pod
		// nothing recreates to another.
		Fix: "give the pod a controller that will recreate it: a Deployment, or — for work " +
			"that ends — a Job carrying podFailurePolicy: [{action: Ignore, onPodConditions: " +
			"[{type: DisruptionTarget}]}], which Kubernetes permits only alongside " +
			"restartPolicy: Never. Without that policy an eviction spends the Job's failure " +
			"budget, and a Job that runs out creates no replacement. Otherwise accept that " +
			"the node cannot be drained while the pod is there. The autoscaler's " +
			"safe-to-evict=true annotation does override this, and means what it says: the " +
			"pod is deleted and nothing brings it back.",
	},
	BlockedMirrorPod: {
		Severity: Blocking,
		Summary: "these are static pods, created by the kubelet from an on-disk manifest, " +
			"and cannot be evicted",
		Fix: "nothing to change from inside the cluster. The node cannot be drained while a " +
			"static pod runs on it.",
	},
	BlockedLocalStorage: {
		Severity: Warning,
		Summary: "these pods use local storage, which the cluster-autoscaler will not disturb " +
			"by default in case the contents matter",
		// The narrow annotation first, because it is the one the autoscaler's
		// own FAQ recommends and the one that says only what is meant. The
		// blanket safe-to-evict also waives the bare-pod and kube-system rules
		// for the same pod, so an operator reaching for it to clear a scratch
		// directory widens rather more than they were asked to — and would
		// have no way of knowing.
		Fix: "annotate the pod cluster-autoscaler.kubernetes.io/safe-to-evict-local-volumes " +
			"with the names of the volumes whose contents need not survive, comma-separated — " +
			"a cache, a scratch directory, a rendered config. It says exactly that and nothing " +
			"more. cluster-autoscaler.kubernetes.io/safe-to-evict=true covers the whole pod " +
			"instead, including the protections that have nothing to do with storage. Neither " +
			"is needed for a tmpfs volume (emptyDir with medium: Memory), which is not local " +
			"storage and is not reported here.",
	},
	BlockedSystemPod: {
		Severity: Warning,
		Summary: "these are kube-system pods with no PodDisruptionBudget, and they are younger " +
			"than the autoscaler's grace for one, so it will not remove a node running one yet",
		// Two answers, and waiting is usually the right one: the grace runs
		// from the pod's creation, so a pod reported here is one that has just
		// started. Recommending a PodDisruptionBudget alone would ask for a
		// real availability decision — on a managed platform, about a workload
		// the operator may not even own — to clear a blockage that expires by
		// itself within the hour.
		Fix: "usually nothing. Since cluster-autoscaler 1.33 the block lifts once the pod is " +
			"older than --blocking-system-pod-distruption-timeout (one hour by default), " +
			"measured from when the pod was created, so a node reported here becomes a " +
			"candidate on its own. Give the workload a PodDisruptionBudget if you want it " +
			"moved sooner or protected properly: per the autoscaler's own documentation a " +
			"budget overrides its refusal to touch the node, and the budget then governs. If " +
			"your autoscaler is older than 1.33 it has no such grace — set " +
			"policy.autoscaler.blockingSystemPodDistruptionTimeout to 0s so binpack stops " +
			"expecting one.",
	},
	BlockedSafeToEvict: {
		Severity: Info,
		Summary:  "these pods are annotated safe-to-evict=false, so nothing will move them",
		Fix:      "deliberate — the annotation says so. Remove it if the pod can in fact move.",
	},
	FindingPriorityBelow: {
		Severity: Warning,
		Summary: "these pods sit below the cluster-autoscaler's expendable cutoff, so it " +
			"ignores them entirely — including when they are Pending",
		Fix: "if this is overprovisioning filler, its warm capacity will never be replenished " +
			"after the first burst, and nothing will report that. Raise the priority to at or " +
			"above the cutoff. If these really are throwaway pods, nothing to do.",
	},
	FindingNoTemplate: {
		Severity: Warning,
		Summary: "these pods are created by a controller binpack cannot read a pod template " +
			"from, so it cannot tell what their replacements would request and will not move them",
		Fix: "unlike everything else here this blocks binpack alone — the cluster-autoscaler and " +
			"kubectl drain are unaffected. binpack reads templates from ReplicaSets, StatefulSets, " +
			"DaemonSets and Jobs; a pod owned directly by an operator's own resource has none. " +
			"Nothing to change on your side: please report the controller, so the list can be " +
			"widened against evidence rather than guesswork.",
	},
	FindingAbandonedDrain: {
		Severity: Warning,
		Summary:  "this node is cordoned and still says binpack is draining it",
		Fix: "if binpack is running it will finish or abandon the drain by itself. If it is " +
			"not — uninstalled mid-drain, most likely — uncordon the node first, then remove " +
			"its binpack.motleyhand.com/ annotations and its " +
			"binpack.motleyhand.com/draining label. That order, because a node left cordoned " +
			"with its markers cleared reads as somebody else's cordon: binpack skips it, this " +
			"check stops naming it, and it stays cordoned and billed with nothing left to say why.",
	},
	FindingNodeInBackoff: {
		Severity: Info,
		Summary:  "binpack failed to drain this node and is waiting before trying again",
		Fix:      "it will retry on its own. The recorded reason is what to fix if it keeps failing.",
	},
}

// Diagnoses returns the catalogue, ordered by code. Exported so the reference
// documentation can be checked against it rather than drifting from it, and so
// that anything consuming binpack as a library can render its own report.
func Diagnoses() []Diagnosis {
	out := make([]Diagnosis, 0, len(diagnoses))
	for code, d := range diagnoses {
		d.Code = code
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out
}

// finding builds one instance of a catalogued diagnosis.
func finding(code, subject, detail string) Finding {
	d, ok := diagnoses[code]
	if !ok {
		// Only reachable by adding a code without cataloguing it, which a test
		// asserts against. Degrading to a finding with no guidance would be
		// worse than saying so.
		d = Diagnosis{Severity: Warning, Summary: "uncatalogued diagnostic code " + code}
	}
	d.Code = code
	return Finding{Diagnosis: d, Subject: subject, Detail: detail}
}

// Diagnose reports what is preventing a cluster from shrinking.
//
// Deliberately useful without binpack installed and without a decision having
// been reached: most of what stops a cluster consolidating also stops the
// cluster-autoscaler, and several of these conditions are invisible from any
// single object. A budget permitting zero disruptions looks healthy. Two
// overlapping budgets look healthier still, and pin their pods permanently.
func Diagnose(s Snapshot, cfg Config) []Finding {
	var findings []Finding

	// The pool checks read the autoscaler's own published status, so without a
	// live autoscaler they have nothing to say — and would say it loudly,
	// since every pool is absent from a status document that does not exist.
	// The remaining checks stand alone: a budget pinning a node is worth
	// reporting whether or not anything can currently remove one.
	if live, _, why := s.Autoscaler.Live(s.Now); !live {
		findings = append(findings, finding(FindingNoAutoscaler, "cluster", why))
	} else {
		findings = append(findings, diagnoseAutoscaler(s)...)
		findings = append(findings, diagnosePools(s, cfg)...)
	}

	findings = append(findings, diagnoseBudgets(s)...)
	findings = append(findings, diagnoseWorkloads(s, cfg)...)
	findings = append(findings, diagnoseNodes(s)...)

	// Severity first, then code, so instances of one diagnosis are contiguous
	// and can be reported together. Stable within a code, preserving the
	// snapshot's order, so two runs over an unchanged cluster produce
	// byte-identical output and a diff means something changed.
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Severity != findings[j].Severity {
			return findings[i].Severity > findings[j].Severity
		}
		return findings[i].Code < findings[j].Code
	})
	return findings
}

func diagnoseAutoscaler(s Snapshot) []Finding {
	if s.Autoscaler.ScaleDownStatus == "NoCandidates" {
		return []Finding{finding(FindingNoCandidates, "cluster",
			"scale-down status: NoCandidates")}
	}
	return nil
}

func diagnosePools(s Snapshot, cfg Config) []Finding {
	var findings []Finding
	names := PoolNames(s, cfg)

	// A pool sitting on its own floor explains a cluster that will not shrink
	// more completely than anything else here, and is trivially checkable.
	for _, g := range s.Autoscaler.Groups {
		if g.MinSize > 0 && g.Size() <= g.MinSize {
			findings = append(findings, finding(FindingPoolAtMinimum, poolLabel(names[g.ID], g.ID),
				fmt.Sprintf("%d node(s), minimum %d", g.Size(), g.MinSize)))
		}
	}

	// A pool absent from the status is one the autoscaler does not manage.
	managed := make(map[string]bool, len(s.Autoscaler.Groups))
	for _, g := range s.Autoscaler.Groups {
		managed[g.ID] = true
	}

	counts := map[string]int{}
	var order []string
	for _, node := range s.Nodes {
		id := cfg.GroupOf(node)
		if id == "" || managed[id] {
			continue
		}
		if _, seen := counts[id]; !seen {
			order = append(order, id)
		}
		counts[id]++
	}
	for _, id := range order {
		findings = append(findings, finding(FindingPoolNotAutoscaled, poolLabel(names[id], id),
			fmt.Sprintf("%d node(s)", counts[id])))
	}

	return findings
}

func poolLabel(name, id string) string {
	if name == "" {
		return id
	}
	return name
}

func diagnoseBudgets(s Snapshot) []Finding {
	var findings []Finding

	for _, pdb := range s.PDBs {
		ref := pdb.Namespace + "/" + pdb.Name

		switch {
		// The arms are ordered by what they read. This one reads the spec,
		// which is current whatever the status is doing, so it comes before
		// every question about whether the status can be believed.
		case unparseableSelector(pdb):
			// And before the sync failure in particular, because a budget
			// nothing can parse is one the disruption controller cannot sync
			// either — getPodsForPdb parses the same selector — so it always
			// arrives carrying one. Naming the field to edit beats relaying a
			// parse error back.
			findings = append(findings, finding(FindingPDBUnparseableSelector, ref,
				"invalid selector: "+selectorSummary(pdb)))

		// Then the one question about the status as a whole, before any arm
		// that reads a field of it — the condition included. Nothing clears a
		// condition when a spec is edited, since only the disruption
		// controller writes status and it writes on sync, so a budget just
		// corrected still carries the reason its previous sync failed with.
		// Reporting that as a current failure would use a stale message to
		// make a blocking claim binpack cannot yet support: whether the edit
		// worked is exactly what the pending sync decides.
		//
		// Which is upstream's own precedence. checkAndDecrement compares
		// observedGeneration against generation and returns 429 before it
		// reads the condition at all, and the "check whether sync is failed
		// first" comment beside that condition orders it ahead of the
		// counters, not ahead of this.
		case pdb.Status.ObservedGeneration < pdb.Generation:
			findings = append(findings, finding(BlockedPDBStale, ref,
				fmt.Sprintf("generation %d, observed %d", pdb.Generation, pdb.Status.ObservedGeneration)))

		case syncFailed(pdb) != nil:
			// Now the status can be believed, and this is the only field of it
			// that is not misleading: failSafe forces disruptionsAllowed to
			// zero and leaves currentHealthy, desiredHealthy and expectedPods
			// at whatever the last successful sync wrote. Read by the counters
			// alone, a budget whose controller is failing is indistinguishable
			// from a one-replica minAvailable: 1 — and from a budget selecting
			// nothing, when it has never synced at all and every counter is
			// still zero. So this comes before all three.
			//
			// The reason discriminates rather than the presence of a
			// condition, which every computed budget also carries: the
			// disruption controller rewrites it to SufficientPods or
			// InsufficientPods on each successful sync (component-helpers
			// apps/poddisruptionbudget, UpdateDisruptionAllowedCondition).
			findings = append(findings, finding(FindingPDBSyncFailed, ref, syncFailed(pdb).Message))

		case pdb.Status.DisruptionsAllowed > 0:
			// Healthy: it permits the disruption a drain needs.

		case pdb.Status.ExpectedPods == 0:
			// Zero because it selects nothing. It pins no node — and protects
			// nothing either, which is the more useful thing to say: whoever
			// wrote it believes a workload is covered when it is not.
			findings = append(findings, finding(FindingPDBSelectsNothing, ref,
				"selector matches no pods: "+selectorSummary(pdb)))

		case pdb.Status.CurrentHealthy < pdb.Status.ExpectedPods:
			// Zero because a replica is missing rather than because the budget
			// is wrong. A different problem, with a different owner.
			//
			// Compared against ExpectedPods, not DesiredHealthy. A budget that
			// has temporarily lost exactly its slack — three replicas,
			// maxUnavailable 1, one of them down — reports currentHealthy
			// equal to desiredHealthy and zero disruptions allowed, and is
			// fine again the moment the third recovers. Testing only against
			// desiredHealthy called that a permanent misconfiguration and sent
			// the reader off to edit a correct budget.
			//
			// It also gets the converse right: minAvailable set above the
			// replica count leaves currentHealthy below desiredHealthy but
			// equal to expectedPods, and that block really is permanent.
			findings = append(findings, finding(FindingPDBUnhealthy, ref,
				fmt.Sprintf("%d of %d pods healthy, %d required",
					pdb.Status.CurrentHealthy, pdb.Status.ExpectedPods, pdb.Status.DesiredHealthy)))

		default:
			findings = append(findings, finding(FindingPDBZero, ref,
				fmt.Sprintf("%d of %d pods healthy, %d required (%s)",
					pdb.Status.CurrentHealthy, pdb.Status.ExpectedPods, pdb.Status.DesiredHealthy,
					budgetSpec(pdb))))
		}
	}

	return findings
}

// syncFailed returns the condition saying the disruption controller could not
// compute this budget, or nil.
//
// policyv1's own constants rather than the strings: the condition type and the
// reason are both exported by k8s.io/api, and a literal here would silently
// stop matching if either were ever renamed.
func syncFailed(pdb *policyv1.PodDisruptionBudget) *metav1.Condition {
	c := meta.FindStatusCondition(pdb.Status.Conditions, policyv1.DisruptionAllowedCondition)
	if c == nil || c.Status != metav1.ConditionFalse || c.Reason != policyv1.SyncFailedReason {
		return nil
	}
	return c
}

// unparseableSelector reports whether no client can turn this budget's
// selector into a label query — the state the eviction subresource responds to
// by skipping the budget entirely.
//
// A null selector is not one of these: LabelSelectorAsSelector answers it with
// labels.Nothing and no error, which is the "matches no pods" that policy/v1
// specifies and a separate diagnosis already covers.
func unparseableSelector(pdb *policyv1.PodDisruptionBudget) bool {
	_, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
	return err != nil
}

// budgetSpec renders the field an operator would edit, since the numbers alone
// do not say which knob produced them.
func budgetSpec(pdb *policyv1.PodDisruptionBudget) string {
	switch {
	case pdb.Spec.MinAvailable != nil:
		return "minAvailable: " + pdb.Spec.MinAvailable.String()
	case pdb.Spec.MaxUnavailable != nil:
		return "maxUnavailable: " + pdb.Spec.MaxUnavailable.String()
	default:
		return "no minAvailable or maxUnavailable"
	}
}

func selectorSummary(pdb *policyv1.PodDisruptionBudget) string {
	if pdb.Spec.Selector == nil {
		return "none"
	}
	keys := make([]string, 0, len(pdb.Spec.Selector.MatchLabels))
	for k, v := range pdb.Spec.Selector.MatchLabels {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	if len(pdb.Spec.Selector.MatchExpressions) > 0 {
		keys = append(keys, fmt.Sprintf("+%d expression(s)", len(pdb.Spec.Selector.MatchExpressions)))
	}
	if len(keys) == 0 {
		return "empty"
	}
	return strings.Join(keys, ",")
}

// diagnoseWorkloads reports per-pod conditions, collapsed by workload.
//
// Collapsing matters: a twenty-replica Deployment mounting an emptyDir is one
// configuration mistake, and twenty identical findings would bury the single
// bare pod underneath them.
func diagnoseWorkloads(s Snapshot, cfg Config) []Finding {
	// The default policy, not a per-pool one. The expendable cutoff is a
	// single cluster-autoscaler flag for the whole cluster, and a workload can
	// span pools, so resolving it per pool would report a threshold that does
	// not exist.
	policy := cfg.Default
	g := newGrouped(staticNodes(s, cfg))

	for _, pod := range s.Pods {
		if !Occupies(pod) || isNodeLocal(pod) {
			continue
		}

		// Checked before the pod is required to be on a node, because a pod
		// below the cutoff that is *Pending* is the failure this diagnosis
		// exists for: the autoscaler ignores such pods when deciding to scale
		// up, so overprovisioning filler evicted by a burst never comes back
		// and no other signal reports it. Requiring an assignment first made
		// the check silent in exactly the case it documents.
		//
		// Narrow by construction: the cutoff defaults to -10 and an unclassed
		// pod sits at 0, so only a deliberate negative priority lands here.
		if pod.Spec.PriorityClassName != "" && podPriority(pod) < policy.Sim.ExpendablePriorityCutoff {
			g.add(pod, FindingPriorityBelow, fmt.Sprintf("priority %d (%s), cutoff %d",
				podPriority(pod), pod.Spec.PriorityClassName, policy.Sim.ExpendablePriorityCutoff))
		}

		// Everything below is about a pod holding a node open, which an
		// unscheduled pod is not.
		if pod.Spec.NodeName == "" {
			continue
		}

		// A pod whose replacement binpack cannot predict is one it will never
		// move, and nothing else in the cluster reports that: every other tool
		// is perfectly happy to drain the node.
		// Guarded on having a controller at all: a bare pod is already its own
		// finding, and reporting it twice would say the same thing in two
		// vocabularies.
		if owner := metav1.GetControllerOf(pod); owner != nil {
			if _, ok := replacement(pod, s.Templates); !ok {
				g.add(pod, FindingNoTemplate, "owned by "+owner.Kind+" "+owner.Name)
			}
		}

		matched := matchingPDBs(pod, s.PDBs)

		// Matched budgets are passed in so a kube-system pod that does have one
		// is not reported: a budget overrides the autoscaler's refusal to touch
		// such a node, and reporting it anyway would send an operator chasing a
		// blocker they had already cleared.
		if b := checkPod(pod, matched, policy.Evict, s.Now); b != nil {
			// Not the blocker's own message: that is written for `explain`,
			// where a blocker stands alone and must explain itself. Here the
			// diagnosis has already said what the condition is, so repeating it
			// once per subject is the noise this report exists to avoid.
			g.add(pod, b.Code, blockerDetail(pod, b.Code))
		}

		// The same gate CheckEvictable applies, so the report does not send an
		// operator to rewrite selectors over a pod the API server would delete
		// without reading either budget.
		if len(matched) > 1 && !pdbsIgnored(pod) {
			names := make([]string, len(matched))
			for i, p := range matched {
				names[i] = p.Name
			}
			// Sorted, so the report does not depend on the order budgets were
			// listed in — an unchanged cluster must produce an unchanged report.
			sort.Strings(names)
			g.add(pod, BlockedMultiplePDBs, "selected by "+strings.Join(names, ", "))
		}
	}

	return g.findings()
}

// blockerDetail is what varies between subjects for a given eviction blocker.
// Most vary in nothing but which pod they are, which the subject already says.
func blockerDetail(pod *corev1.Pod, code string) string {
	if code != BlockedLocalStorage {
		return ""
	}
	// Naming the volume is what makes the suggested annotation a judgement the
	// reader can actually make: "wasm-cache" answers "is this disposable?" and
	// "data" does not.
	volume, _ := firstLocalStorage(pod)
	return volume
}

// grouped collapses identical findings across pods of one workload, keeping
// first-seen order so output is reproducible between runs.
type grouped struct {
	order  []string
	byKey  map[string]*groupedFinding
	static map[string]bool
}

// staticNodes is the set of nodes whose pool the cluster-autoscaler does not
// manage. Nothing will ever remove those nodes, so an eviction blocker sitting
// on one costs nothing today — and telling somebody to go and annotate five
// workloads that would not free a single node is worse than saying nothing.
//
// Empty when there is no live autoscaler, where every pool is absent from a
// status document that does not exist and the distinction is meaningless.
func staticNodes(s Snapshot, cfg Config) map[string]bool {
	if live, _, _ := s.Autoscaler.Live(s.Now); !live {
		return nil
	}

	managed := make(map[string]bool, len(s.Autoscaler.Groups))
	for _, g := range s.Autoscaler.Groups {
		managed[g.ID] = true
	}

	out := map[string]bool{}
	for _, node := range s.Nodes {
		if !managed[cfg.GroupOf(node)] {
			out[node.Name] = true
		}
	}
	return out
}

type groupedFinding struct {
	finding Finding
	pods    int
	// nodes is which nodes the workload is pinning, in first-seen order.
	// The point of the whole report: a finding an operator cannot locate is
	// one they cannot weigh against the cost of the node it is holding open.
	nodes []string
	seen  map[string]bool
	// pending counts pods the scheduler has not placed. They pin no node, and
	// for an expendable pod being unplaced is itself the finding.
	pending int
}

// where says how many pods are involved and on which nodes, which is what
// turns a finding into something an operator can locate and cost.
func (f *groupedFinding) where() string {
	var parts []string

	if scheduled := f.pods - f.pending; scheduled > 0 {
		location := f.nodes[0]
		if len(f.nodes) > 1 {
			location = fmt.Sprintf("%d nodes", len(f.nodes))
		}
		if scheduled > 1 {
			location = fmt.Sprintf("%d pods on %s", scheduled, location)
		}
		parts = append(parts, location)
	}
	if f.pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", f.pending))
	}

	return strings.Join(parts, ", ")
}

func newGrouped(static map[string]bool) *grouped {
	return &grouped{byKey: map[string]*groupedFinding{}, static: static}
}

func (g *grouped) add(pod *corev1.Pod, code, detail string) {
	// Grouped by controller, so replicas of one workload collapse while two
	// bare pods — which have no controller, and are each separately something
	// an operator has to deal with — stay apart.
	subject, key := podRef(pod), podRef(pod)+"/"+code
	if owner := metav1.GetControllerOf(pod); owner != nil {
		subject = fmt.Sprintf("%s/%s %s", pod.Namespace, owner.Kind, owner.Name)
		key = subject + "/" + code
	}

	entry, ok := g.byKey[key]
	if !ok {
		entry = &groupedFinding{finding: finding(code, subject, detail), seen: map[string]bool{}}
		g.byKey[key] = entry
		g.order = append(g.order, key)
	}
	entry.pods++
	node := pod.Spec.NodeName
	if node == "" {
		entry.pending++
		return
	}
	if !entry.seen[node] {
		entry.seen[node] = true
		entry.nodes = append(entry.nodes, node)
	}
}

// allStatic reports whether a finding sits entirely on nodes nothing will ever
// remove. An unscheduled pod is never static: it is on no node at all, and for
// expendable filler that is the live problem rather than a dormant one.
func (g *grouped) allStatic(f *groupedFinding) bool {
	if f.pending > 0 || len(f.nodes) == 0 {
		return false
	}
	for _, node := range f.nodes {
		if !g.static[node] {
			return false
		}
	}
	return true
}

func (g *grouped) findings() []Finding {
	out := make([]Finding, 0, len(g.order))
	for _, key := range g.order {
		entry := g.byKey[key]
		f := entry.finding

		if where := entry.where(); f.Detail == "" {
			f.Detail = where
		} else {
			f.Detail += ", " + where
		}
		f.FreesNothing = g.allStatic(entry)
		out = append(out, f)
	}
	return out
}

func diagnoseNodes(s Snapshot) []Finding {
	var findings []Finding

	for _, node := range s.Nodes {
		if !node.Spec.Unschedulable {
			continue
		}
		// A cordoned node still carrying a drain marker. Naming it is the whole
		// value: without the marker this is a mysteriously cordoned node that
		// nobody dares uncordon.
		if started := node.Annotations[AnnotationDrainStarted]; started != "" {
			findings = append(findings, finding(FindingAbandonedDrain, node.Name,
				"drain started "+started))
			continue
		}
		// The marker gone and the cordon still there is the worse half of the
		// same state, and until now the only one nothing could see. It is what
		// a hand-back leaves between clearing the markers and uncordoning, and
		// what a policy controller stripping binpack.motleyhand.com/
		// annotations leaves for good: [eligibility] then skips the node as
		// SkipCordoned, which is the bucket for a node somebody else cordoned,
		// and every surface that could name it has gone quiet.
		//
		// The label is a sound second key because Begin and Abandon write and
		// clear it in the same patch as the markers, so it is never left
		// behind by binpack itself — only by a repair that stopped halfway.
		// Reading it here is also the only reader it has ever had.
		//
		// Gated on the marker's absence rather than added beside it, because
		// every drain in flight carries both: ungated this would report each
		// of them twice, and the finding is already noise on a busy cluster.
		if node.Labels[LabelDraining] == "true" {
			findings = append(findings, finding(FindingAbandonedDrain, node.Name,
				"cordoned by binpack, drain markers removed"))
		}
	}

	for _, node := range s.Nodes {
		until, err := time.Parse(time.RFC3339, node.Annotations[AnnotationBackoffUntil])
		if err != nil || !s.Now.Before(until) {
			continue
		}
		// The attempt count, in the same words the skip reason uses: explain
		// reads one and diagnose the other, and the operator meeting both is
		// the same person reading about the same node.
		findings = append(findings, finding(FindingNodeInBackoff, node.Name,
			fmt.Sprintf("until %s%s, after: %s", until.Format(time.RFC3339),
				attemptClause(node), node.Annotations[AnnotationLastFailure])))
	}

	return findings
}
