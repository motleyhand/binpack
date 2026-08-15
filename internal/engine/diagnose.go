package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
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
	FindingNoAutoscaler      = "no-autoscaler"
	FindingNoCandidates      = "autoscaler-no-candidates"
	FindingPoolNotAutoscaled = "pool-not-autoscaling"
	FindingPoolAtMinimum     = "pool-at-minimum"
	FindingPDBZero           = "pdb-zero-disruptions"
	FindingPDBUnhealthy      = "pdb-workload-unhealthy"
	FindingPDBSelectsNothing = "pdb-selects-nothing"
	FindingPriorityBelow     = "priority-below-cutoff"
	FindingAbandonedDrain    = "abandoned-drain"
	FindingNodeInBackoff     = "node-in-backoff"
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
		Summary: "no cluster-autoscaler is running, so nothing will remove a node however " +
			"empty it becomes",
		Fix: "check that cluster autoscaling is enabled for this cluster, and that the " +
			"cluster-autoscaler-status ConfigMap in kube-system is being updated.",
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
	BlockedPDBStale: {
		Severity: Warning,
		Summary: "this PodDisruptionBudget was edited and its controller has not caught up, " +
			"so the eviction API refuses disruptions until it does",
		Fix: "usually momentary and worth re-checking. If it persists, the disruption " +
			"controller is not running.",
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
		Fix: "give the pod a controller — a Deployment or a Job — or accept that its node " +
			"cannot be drained while it is there.",
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
		Fix: "annotate the pod cluster-autoscaler.kubernetes.io/safe-to-evict=true when the " +
			"volume holds nothing that must survive — a cache, a scratch directory, a rendered " +
			"config. That is the common case and it is a one-line change.",
	},
	BlockedSystemPod: {
		Severity: Warning,
		Summary: "these are kube-system pods with no PodDisruptionBudget, and the autoscaler " +
			"will not remove a node running one",
		Fix: "give the workload a PodDisruptionBudget. Per the autoscaler's own documentation " +
			"a budget overrides its refusal to touch the node, and the budget then governs.",
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
	FindingAbandonedDrain: {
		Severity: Warning,
		Summary:  "this node is cordoned and still carries a binpack drain marker",
		Fix: "if binpack is running it will finish or abandon the drain by itself. If it is " +
			"not — uninstalled mid-drain, most likely — uncordon the node and remove its " +
			"binpack.motleyhand.com/ annotations, or it stays cordoned and billed.",
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
	if live, why := s.Autoscaler.Live(s.Now); !live {
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
		id := node.Labels[cfg.NodeGroupIDLabel]
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
		case pdb.Status.ObservedGeneration < pdb.Generation:
			findings = append(findings, finding(BlockedPDBStale, ref,
				fmt.Sprintf("generation %d, observed %d", pdb.Generation, pdb.Status.ObservedGeneration)))

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
		if !occupies(pod) || isNodeLocal(pod) {
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

		matched := matchingPDBs(pod, s.PDBs)

		// Matched budgets are passed in so a kube-system pod that does have one
		// is not reported: a budget overrides the autoscaler's refusal to touch
		// such a node, and reporting it anyway would send an operator chasing a
		// blocker they had already cleared.
		if b := checkPod(pod, matched, policy.Evict); b != nil {
			// Not the blocker's own message: that is written for `explain`,
			// where a blocker stands alone and must explain itself. Here the
			// diagnosis has already said what the condition is, so repeating it
			// once per subject is the noise this report exists to avoid.
			g.add(pod, b.Code, blockerDetail(pod, b.Code))
		}

		if len(matched) > 1 {
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
	if live, _ := s.Autoscaler.Live(s.Now); !live {
		return nil
	}

	managed := make(map[string]bool, len(s.Autoscaler.Groups))
	for _, g := range s.Autoscaler.Groups {
		managed[g.ID] = true
	}

	out := map[string]bool{}
	for _, node := range s.Nodes {
		if !managed[node.Labels[cfg.NodeGroupIDLabel]] {
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
		// A cordoned node still carrying a drain marker. Naming it is the whole
		// value: without the marker this is a mysteriously cordoned node that
		// nobody dares uncordon.
		if started := node.Annotations[AnnotationDrainStarted]; started != "" && node.Spec.Unschedulable {
			findings = append(findings, finding(FindingAbandonedDrain, node.Name,
				"drain started "+started))
		}
	}

	for _, node := range s.Nodes {
		until, err := time.Parse(time.RFC3339, node.Annotations[AnnotationBackoffUntil])
		if err != nil || !s.Now.Before(until) {
			continue
		}
		findings = append(findings, finding(FindingNodeInBackoff, node.Name,
			fmt.Sprintf("until %s, after: %s", until.Format(time.RFC3339),
				node.Annotations[AnnotationLastFailure])))
	}

	return findings
}
