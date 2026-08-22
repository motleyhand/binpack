package engine

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Annotations binpack writes on nodes. Fixed keys, not configurable: one thing
// to document, one thing to grep for, and no possibility of two clusters
// disagreeing about what protects a node.
const (
	AnnotationSkip = "binpack.motleyhand.com/skip"

	// LabelDraining marks a node binpack is currently draining, so that
	// `kubectl get nodes -L` says who cordoned it. Carried alongside the
	// markers below rather than instead of them: the annotations are the
	// state, this is the signal.
	LabelDraining = "binpack.motleyhand.com/draining"

	// Drain markers. Together these are the whole recovery state of a drain in
	// progress: they live on the node rather than in process memory because
	// the failure an in-memory timer guards against and the failure that
	// destroys it are the same class of event.
	AnnotationDrainStarted       = "binpack.motleyhand.com/drain-started"
	AnnotationDrainProgress      = "binpack.motleyhand.com/drain-progress"
	AnnotationDrainPodsRemaining = "binpack.motleyhand.com/drain-pods-remaining"
	// AnnotationDrainAwaiting names the controller whose replacement pod the
	// drain is waiting to see bound, as "<owner UID>@<RFC3339>". Evicting the
	// next pod before this one has landed would put two replacements in
	// flight against a simulation that assumed one. Or AwaitingSettled, when
	// the drain has committed to an eviction and is owed no replacement for it.
	AnnotationDrainAwaiting = "binpack.motleyhand.com/drain-awaiting"

	// Backoff markers, recorded when a drain is abandoned.
	AnnotationDrainAttempts = "binpack.motleyhand.com/drain-attempts"
	AnnotationBackoffUntil  = "binpack.motleyhand.com/backoff-until"
	AnnotationLastFailure   = "binpack.motleyhand.com/last-failure"
)

// AwaitingSettled is what [AnnotationDrainAwaiting] carries when a drain owes
// itself no replacement: the previous one has already landed, the pod evicted
// was expendable and the simulation never placed one, or the eviction that will
// owe one has been committed to but not yet attempted.
//
// That last is why the sentinel is written before the eviction rather than
// after it. A marker written afterwards is lost by exactly the failures it
// exists to survive, and the eviction is not lost with it; a marker naming a
// controller cannot go first, because it claims a replacement is owed and an
// eviction refused after the write would leave the drain waiting out its stall
// timeout for a pod nobody was going to create.
//
// A third state in what began as a two-state annotation, because the two
// obvious spellings of "nothing owed" are both wrong. Absent means the drain
// has evicted nothing — [committedDrain] reads this key's presence and nothing
// else — so clearing it re-arms the preferences ADR-0009 stops asking once pods
// have moved, and since ADR-0010 the scale-up cooldown with them. Leaving the
// previous eviction's marker standing is the other way to say nothing, and it
// says something false: the drain goes on waiting for a replacement it is not
// owed, and any later pod of that controller the scheduler refuses ends it.
//
// Deliberately not of the form "<uid>@<RFC3339>". A reader parses the marker to
// find the controller it names, and a sentinel that parsed would name one.
const AwaitingSettled = "settled"

// Taints the cluster-autoscaler puts on nodes it is removing. Its constants,
// not binpack's — from cluster-autoscaler/utils/taints/taints.go — so they are
// named here once rather than spelled out at each use.
//
// Only the committed one is acted on. DeletionCandidateOfClusterAutoscaler is
// PreferNoSchedule and means the autoscaler considers the node unneeded, which
// is an opinion rather than a decision; ToBeDeletedByClusterAutoscaler is
// NoSchedule and means it has started removing it.
const (
	TaintToBeDeleted       = "ToBeDeletedByClusterAutoscaler"
	TaintDeletionCandidate = "DeletionCandidateOfClusterAutoscaler"
)

// BeingRemoved reports whether the cluster-autoscaler has committed to deleting
// this node.
//
// The node is the autoscaler's from that moment: it is draining the node
// itself, and binpack evicting alongside it is duplicated work at best. Worse,
// binpack's own bounds would eventually abandon the drain and uncordon a node
// the autoscaler is actively deleting — two components disagreeing about
// whether a node should accept pods.
func BeingRemoved(node *corev1.Node) bool {
	for _, taint := range node.Spec.Taints {
		if taint.Key == TaintToBeDeleted {
			return true
		}
	}
	return false
}

// MaxStatusAge is how stale the autoscaler's status may be before binpack
// treats it as abandoned. The autoscaler scans every ten seconds by default,
// so minutes of silence means it is gone rather than busy.
const MaxStatusAge = 5 * time.Minute

// Snapshot is the cluster as binpack sees it. Plain data: no clients, nothing
// that performs I/O, and every object read-only.
type Snapshot struct {
	Nodes []*corev1.Node
	Pods  []*corev1.Pod
	PDBs  []*policyv1.PodDisruptionBudget

	// Templates is the pod template of each controller, keyed by the owner
	// reference its pods carry. The simulation places the pod a controller
	// *would create*, not the one leaving — see the replacement doc comment —
	// and this is where that spec comes from.
	Templates map[OwnerRef]*corev1.PodTemplateSpec

	// Autoscaler is what the cluster-autoscaler reports about itself, read
	// from its status ConfigMap rather than a cloud API.
	Autoscaler Autoscaler

	// LastDrain is when binpack last completed a drain. Zero means unknown,
	// which is what a freshly started controller sees — the cooldown simply
	// does not apply, and the worst case is one drain sooner than intended.
	LastDrain time.Time

	Now time.Time
}

// Autoscaler describes the component that will actually remove the node.
type Autoscaler struct {
	// Running is false when no autoscaler could be found. binpack refuses to
	// act at all in that case: draining a node nothing will reap is strictly
	// worse than doing nothing.
	Running bool

	// LastScaleUp is when the cluster last grew. Mirrors the autoscaler's own
	// scale-down-delay-after-add, and needs no persistence because the
	// autoscaler publishes it.
	LastScaleUp time.Time

	// ScaleUpInProgress means the cluster is growing right now, which is the
	// clearest possible signal not to be removing nodes.
	ScaleUpInProgress bool

	// LastProbe is when the autoscaler last completed a scan. A status
	// ConfigMap outlives the autoscaler that wrote it, so freshness is what
	// distinguishes a running autoscaler from a leftover object claiming to
	// be one.
	LastProbe time.Time

	// ScaleDownStatus is what the autoscaler reports about its own
	// scale-down search — "NoCandidates" being the state binpack exists to
	// resolve. Informational: it is reported, never acted on.
	ScaleDownStatus string

	Groups []NodeGroup
}

// Live reports whether there is an autoscaler that would actually remove a
// drained node, and why not when there is not.
//
// One function so that what binpack decides and what explain prints cannot
// disagree — an earlier version had the renderer read Running directly and
// announce a healthy autoscaler above a decision refusing to act because it
// was not.
func (a Autoscaler) Live(now time.Time) (bool, string) {
	if !a.Running {
		return false, "no cluster-autoscaler is running, so a drained node would never be removed"
	}

	// A status ConfigMap outlives the autoscaler that wrote it and keeps
	// saying Running indefinitely, so freshness is what separates a live
	// autoscaler from a leftover object claiming to be one. Absent evidence
	// of life counts as absent life: a document with no probe time at all is
	// one binpack cannot vouch for.
	if a.LastProbe.IsZero() {
		return false, "the cluster-autoscaler status carries no probe time, so binpack cannot tell whether it is alive"
	}
	if since := now.Sub(a.LastProbe); since > MaxStatusAge {
		return false, fmt.Sprintf(
			"the cluster-autoscaler last reported %s ago, so it is not running; a drained node would never be removed",
			since.Round(time.Second))
	}

	return true, ""
}

// NodeGroup is one autoscaling pool, as the autoscaler reports it. Pools it
// does not manage are absent, which is how binpack knows not to touch them.
type NodeGroup struct {
	// ID matches the value of Config.NodeGroupIDLabel on a node.
	//
	// There is deliberately no Name here. The autoscaler's status carries no
	// human-readable pool name — that lives on the nodes, as a label — and a
	// field only tests could fill in is one every consumer silently falls back
	// from. Use [PoolNames] instead.
	ID      string
	MinSize int
	MaxSize int
	Ready   int

	// Target is what the autoscaler has asked the provider for. It can differ
	// from Ready mid-transition: after a scale-down lowers the target to the
	// minimum but before the node disappears, Ready still exceeds MinSize.
	//
	// HasTarget distinguishes a reported zero from an absent value. Zero is a
	// legitimate target — a pool with minSize 0 removing its last node — so
	// treating it as "not reported" would discard exactly the case where the
	// autoscaler has most clearly finished deciding.
	Target    int
	HasTarget bool
}

// Size is the group size to compare against its floor.
//
// The smaller of intent and reality, because either being at the minimum
// means a drained node will simply be replaced. Preferring the smaller value
// costs a missed consolidation the next run may find; preferring the larger
// costs a pointless drain.
func (g NodeGroup) Size() int {
	if g.HasTarget && g.Target < g.Ready {
		return g.Target
	}
	return g.Ready
}

// Policy is a fully resolved policy for one pool.
type Policy struct {
	Enabled              bool
	Sim                  SimConfig
	Evict                EvictConfig
	MaxPodsPerDrain      int
	CooldownAfterScaleUp time.Duration
	CooldownAfterDrain   time.Duration

	// StallTimeout and RemovalTimeout bound a drain in progress.
	// BackoffInitial and BackoffMax say how long a node is left alone after
	// one fails. The engine reads none of the four — it decides, and all of
	// them describe what happens long after it has — but a policy resolved
	// per pool has to carry them, or the executor would have to resolve pools
	// a second time and could disagree.
	StallTimeout       time.Duration
	RemovalTimeout     time.Duration
	BackoffInitial     time.Duration
	BackoffMax         time.Duration
	ExcludedNamespaces []string
}

// Config is what the engine needs to decide. The CLI and controller build it
// from api/v1alpha1; the engine never reads configuration itself.
type Config struct {
	NodeGroupIDLabel string
	PoolNameLabel    string

	Default Policy
	ByPool  map[string]Policy
}

// PolicyFor resolves the policy for a pool, matching on either identifier.
func (c Config) PolicyFor(names ...string) Policy {
	for _, name := range names {
		if name == "" {
			continue
		}
		if p, ok := c.ByPool[name]; ok {
			return p
		}
	}
	return c.Default
}

// NoDrainToMeasureFrom is why an after-drain cooldown cannot be enforced by a
// process that did not perform the drain.
//
// One clause with two readers. `run --once` refuses to start on it, and
// `binpack explain` discloses it, because they are the same fact: a completed
// drain deletes the node that would otherwise have recorded it, so the
// timestamp lives in the running controller's memory and nowhere else. Two
// accounts of one fact drift, and the operator meeting both is the same person
// reading the same configuration.
const NoDrainToMeasureFrom = "a completed drain leaves nothing in the cluster to measure from"

// CooldownAfterDrain names the first place a non-zero cooldown.afterDrain is
// configured, if there is one.
//
// The default policy first, then pools in name order, so the answer does not
// depend on map iteration.
func CooldownAfterDrain(cfg Config) (where string, d time.Duration, set bool) {
	if d := cfg.Default.CooldownAfterDrain; d > 0 {
		return "the default policy", d, true
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.ByPool)) {
		if d := cfg.ByPool[name].CooldownAfterDrain; d > 0 {
			return "pool " + name, d, true
		}
	}
	return "", 0, false
}

// Action is what binpack has decided to do.
type Action int

const (
	// None: nothing to do, and Decision.Reason says why.
	None Action = iota
	// Drain the named node.
	Drain
)

func (a Action) String() string {
	if a == Drain {
		return "drain"
	}
	return "none"
}

// SkipCodes is every reason a node can be ruled out. Enumerable because these
// are metric label values: a counter that only appears once it has fired makes
// a rate() alert silently useless until the first occurrence.
func SkipCodes() []string {
	return []string{
		SkipNotAutoscaled, SkipPoolDisabled, SkipScaleUpInProgress,
		SkipCooldownAfterScaleUp, SkipCooldownAfterDrain, SkipPoolAtMinimum,
		SkipAnnotated, SkipDrainInProgress, SkipGone, SkipUncordoned,
		SkipBackoff, SkipCordoned, SkipProtectedPod, SkipTooManyPods,
	}
}

// Verdicts: what binpack concluded about one node, as a bounded value.
//
// The prose says why in a sentence an operator can act on; these name the
// outcome so it can be counted, alerted on and matched against without anyone
// parsing English. Stable, like the diagnostic codes and the metric names.
const (
	VerdictSkipped    = "skipped"
	VerdictInfeasible = "infeasible"
	VerdictBlocked    = "blocked"
	VerdictDrainable  = "drainable"
)

// Skip codes: why a node was ruled out. One per branch of the eligibility
// check, set beside the prose so the two cannot drift apart.
const (
	SkipNotAutoscaled        = "not-autoscaled"
	SkipPoolDisabled         = "pool-disabled"
	SkipScaleUpInProgress    = "scale-up-in-progress"
	SkipCooldownAfterScaleUp = "cooldown-after-scale-up"
	SkipCooldownAfterDrain   = "cooldown-after-drain"
	SkipPoolAtMinimum        = "pool-at-minimum"
	SkipAnnotated            = "annotated-skip"
	SkipDrainInProgress      = "drain-in-progress"
	// SkipGone: the node is no longer in the cluster, which for a drain in
	// progress is success rather than a skip.
	SkipGone = "gone"
	// SkipUncordoned: a drain is marked on the node but the node is
	// schedulable, so it is still accepting pods.
	SkipUncordoned = "uncordoned"
	// SkipBeingRemoved: the cluster-autoscaler has committed to deleting the
	// node and is draining it itself.
	SkipBeingRemoved = "being-removed"
	SkipBackoff      = "backoff"
	SkipCordoned     = "cordoned"
	SkipProtectedPod = "protected-pod"
	SkipTooManyPods  = "too-many-pods"
)

// Decision codes: the outcome of a whole evaluation.
const (
	CodeDrain = "drain"
	// CodeDraining: a drain is already under way, so this evaluation advanced
	// it rather than deciding afresh.
	CodeDraining     = "draining"
	CodeNoAutoscaler = "no-autoscaler"
	// CodeNoCandidates: every node was ruled out before any simulation ran.
	CodeNoCandidates = "no-candidates"
	// CodeNoneFeasible: nodes were simulated and none could be emptied.
	CodeNoneFeasible = "none-feasible"
)

// Decision is the engine's answer, carrying the arithmetic that produced it.
//
// Every field exists so `binpack explain` can show its working. A decision
// that cannot be explained is one nobody should act on.
type Decision struct {
	Action Action
	// Code names the outcome, from the bounded set above. Reason explains it;
	// this is what a metric or an alert can key on.
	Code string
	// Node is set when Action is Drain.
	Node *corev1.Node
	// Reason explains a None, in a sentence an operator can act on.
	Reason string
	// Assessments covers every node considered, in the order considered,
	// including those ruled out before any simulation ran.
	Assessments []NodeAssessment
}

// NodeAssessment is what binpack concluded about one node.
type NodeAssessment struct {
	Node  *corev1.Node
	Group string
	Pool  string

	// Skipped is set when the node was ruled out before simulation, with
	// SkipReason saying why in prose and SkipCode naming it.
	Skipped    bool
	SkipReason string
	SkipCode   string

	// Simulation and Blockers are populated only for nodes that reached
	// those stages.
	Simulation *Simulation
	Blockers   []EvictionBlocker

	// Chosen marks the node binpack decided to drain.
	Chosen bool
}

// Verdict names what binpack concluded about this node.
//
// Derived rather than stored: it is a reading of the other fields, and a
// stored copy is a copy that can disagree with them.
func (a NodeAssessment) Verdict() string {
	switch {
	case a.Skipped:
		return VerdictSkipped
	case len(a.Blockers) > 0:
		return VerdictBlocked
	case a.Simulation != nil && !a.Simulation.Feasible:
		return VerdictInfeasible
	default:
		return VerdictDrainable
	}
}

// Decide runs the whole procedure and returns one action.
//
// Deliberately one node per run. Iterative beats clever: the next run observes
// fresh state, which is both safer and far simpler to reason about than
// planning a multi-node consolidation against a cluster that is changing
// underneath the plan.
func Decide(s Snapshot, cfg Config) Decision {
	if live, why := s.Autoscaler.Live(s.Now); !live {
		return Decision{Code: CodeNoAutoscaler, Reason: why}
	}

	assessments, chosen := assess(s, cfg)

	// Step 0 of the drain protocol: a drain already under way pre-empts a new
	// decision, however good the candidate below it looks.
	//
	// Below the assessments rather than above them, and the difference is the
	// whole value of the branch. Returning at the top of this function is the
	// cheap way to write "one node per run" and it would hand `binpack explain`
	// an empty node table — losing the arithmetic the command exists to show,
	// in precisely the window that made the gate necessary. So every node is
	// still assessed and still reported; what a drain in progress takes away is
	// the choice, not the reasoning.
	//
	// The marked node's own row is the one this cannot answer: it is assessed
	// here as a node binpack might select, and what governs it is whether the
	// drain already under way survives another step. That question is
	// [Revalidate]'s, and it is asked with the drain's own flags.
	if name := Marked(s); name != "" {
		return Decision{
			Code:        CodeDraining,
			Node:        nodeNamed(s, name),
			Reason:      "a drain is in progress on " + name,
			Assessments: assessments,
		}
	}

	if chosen >= 0 {
		assessments[chosen].Chosen = true
		return Decision{
			Action:      Drain,
			Code:        CodeDrain,
			Node:        assessments[chosen].Node,
			Assessments: assessments,
		}
	}

	return Decision{
		Code:        outcomeCode(assessments),
		Reason:      summarise(assessments),
		Assessments: assessments,
	}
}

// Assess is what [Decide] concluded about every node, without the decision.
//
// Exported for `binpack explain`, which has one path Decide cannot serve. With
// no live cluster-autoscaler Decide refuses before assessing anything, and
// that emptiness is load-bearing where it is: it is what stops a dead
// autoscaler zeroing the node gauges over a cluster nobody looked at, and what
// keeps a refusal off nodes the decision was never about. But a reader
// pointing binpack at a cluster that has no autoscaler — kind, minikube, or a
// managed cluster with autoscaling switched off — was shown five lines and no
// evidence that binpack had read anything, which is indistinguishable from a
// binary that does not work.
//
// So explain asks this instead and says plainly that nothing will act on it.
// No node is marked Chosen: choosing is Decide's, and a choice reported by a
// binpack that will not act is a plan nobody is going to carry out.
func Assess(s Snapshot, cfg Config) []NodeAssessment {
	assessments, _ := assess(s, cfg)
	return assessments
}

// assess runs the eligibility pass over every node and simulates each
// candidate, returning the assessments and the index of the first node that
// could be drained, or -1.
func assess(s Snapshot, cfg Config) ([]NodeAssessment, int) {
	candidates, assessments := eligible(s, cfg)

	// Least loaded first. Not a filter — every eligible node is tried in
	// turn — but a node doing less work is cheaper to empty and likelier to
	// succeed, so it is worth trying first.
	sort.SliceStable(candidates, func(i, j int) bool {
		return workloadOn(s, candidates[i].Node) < workloadOn(s, candidates[j].Node)
	})

	// Every candidate is assessed, not just those up to the first success.
	// Stopping early would be cheaper, but `explain` has to describe the whole
	// cluster: a node missing from the assessments is a node an operator
	// cannot ask about, and "why not that one?" is the question they will ask.
	chosen := -1

	for i := range candidates {
		a := candidates[i]
		// Never committed: this is the decision, and nothing has been evicted
		// on the strength of it yet.
		if drainable(s, a, cfg.PolicyFor(a.Group, a.Pool), false) && chosen < 0 {
			chosen = len(assessments)
		}
		assessments = append(assessments, *a)
	}

	return assessments, chosen
}

// drainable simulates a node and reports whether its pods could go elsewhere,
// filling in the assessment with what it found.
//
// Shared by [Decide] and [Revalidate] rather than written twice. Selection and
// mid-drain revalidation asking the question differently is the failure that
// matters here: binpack would cordon a node on one basis and evict its pods on
// another, with nothing to reveal the disagreement.
func drainable(s Snapshot, a *NodeAssessment, policy Policy, committed bool) bool {
	// Once pods have started leaving, only the questions whose answers make a
	// drain *unsound* are re-asked. The rest are preferences about whether it
	// was a good idea, and re-asking those after the work has begun does not
	// undo the work — it abandons a half-drained node and leaves the cluster
	// worse than either finishing or never starting. See ADR-0009.
	sim := policy.Sim
	if committed {
		sim.ReserveForLargestPod = false
	}

	simulation := Simulate(s.Nodes, s.Pods, s.Templates, a.Node, sim)
	a.Simulation = &simulation
	if !simulation.Feasible {
		return false
	}

	if !committed && policy.MaxPodsPerDrain > 0 && len(simulation.Relocated) > policy.MaxPodsPerDrain {
		a.Skipped = true
		a.SkipCode = SkipTooManyPods
		a.SkipReason = fmt.Sprintf("would relocate %d pods, above the limit of %d",
			len(simulation.Relocated), policy.MaxPodsPerDrain)
		return false
	}

	evicting := append(append([]*corev1.Pod{}, podsOf(simulation.Relocated)...), simulation.Evicted...)
	if blockers := CheckEvictable(evicting, s.PDBs, policy.Evict); len(blockers) > 0 {
		a.Blockers = blockers
		return false
	}

	return true
}

// Revalidate re-asks, of one named node, the question that selected it.
//
// A drain is not one decision followed by a batch of evictions: the cluster
// keeps moving underneath it, and a drain can legitimately outlast many
// evaluation intervals. The snapshot that chose this node is stale by the time
// anything is evicted — the scheduler may have bound new pods to it between
// the decision and the cordon, pods whose fit, evictability and PDB demand
// were never assessed.
//
// So this runs the whole procedure again against the node as it is now, using
// the same code that selected it, and ignoring only binpack's own marker and
// cordon. The [NodeAssessment.Verdict] is drainable or it is not; anything
// else means the drain must stop, and SkipReason or Blockers say why in terms
// an operator can act on.
//
// A node absent from the snapshot returns skipped rather than an error: the
// cluster-autoscaler removing it is the outcome a drain is working towards.
func Revalidate(s Snapshot, name string, cfg Config) NodeAssessment {
	node := nodeNamed(s, name)
	if node == nil {
		return NodeAssessment{
			Node:       &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}},
			Skipped:    true,
			SkipCode:   SkipGone,
			SkipReason: "the node is no longer in the cluster",
		}
	}

	// Checked here as well as per-node: an autoscaler that has died mid-drain
	// means nothing will ever remove this node, so continuing to empty it
	// would strand the cordon.
	if live, why := s.Autoscaler.Live(s.Now); !live {
		a := NodeAssessment{Node: node, Group: node.Labels[cfg.NodeGroupIDLabel],
			Pool: node.Labels[cfg.PoolNameLabel]}
		a.Skipped, a.SkipCode, a.SkipReason = true, SkipNotAutoscaled, why
		return a
	}

	a := eligibility(s, cfg, groupsByID(s), protectedNamespaces(s, cfg), node, true)
	if a.Skipped {
		return a
	}

	// The same question eligibility asked above, through the same reading, so
	// the two halves of the drain cannot disagree about whether it has begun.
	drainable(s, &a, cfg.PolicyFor(a.Group, a.Pool), committedDrain(node))
	return a
}

// outcomeCode distinguishes a cluster where nothing was even eligible from one
// where candidates were simulated and none worked. They call for entirely
// different responses — the first is configuration, the second is capacity —
// and the prose already draws the line, so the code must too.
func outcomeCode(assessments []NodeAssessment) string {
	for _, a := range assessments {
		if !a.Skipped {
			return CodeNoneFeasible
		}
	}
	return CodeNoCandidates
}

// Marked names the node binpack is part-way through draining, or "" for none.
//
// "One node per run" is the whole of the drain protocol's step 0, and it used
// to live above this package, in the controller's loop — so the other caller of
// [Decide], `binpack explain`, did not have it and went on choosing a second
// node while the first was still being emptied. A rule enforced above a shared
// function is a rule only one of its callers obeys.
//
// Sorted, so two markers left by some earlier confusion produce the same answer
// every evaluation rather than whichever the cache listed first. One drain at a
// time is the invariant; picking deterministically means the second marker is
// resolved rather than alternated with.
func Marked(s Snapshot) string {
	var names []string
	for _, node := range s.Nodes {
		if node.Annotations[AnnotationDrainStarted] != "" {
			names = append(names, node.Name)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// nodeNamed finds one node in a snapshot, or nil.
func nodeNamed(s Snapshot, name string) *corev1.Node {
	for _, node := range s.Nodes {
		if node.Name == name {
			return node
		}
	}
	return nil
}

// committedDrain reports whether the drain on this node has begun disrupting
// workload.
//
// Read from the node rather than passed in, so nothing can disagree with the
// node about what has already happened: the marker is written immediately
// before the first eviction and cleared only when the drain ends, naming the
// replacement being waited for or, as [AwaitingSettled], recording that none is
// owed. Its presence is the signal and its value is not, which is why "nothing
// owed" has a spelling of its own. See ADR-0009.
//
// Before rather than after, so a drain whose first eviction was refused reads
// as committed having disrupted nothing. That is the direction to be wrong in:
// it withdraws two preferences and leaves every question of soundness asked,
// where the other direction hands back a node that has already been half
// emptied.
func committedDrain(node *corev1.Node) bool {
	return node.Annotations[AnnotationDrainAwaiting] != ""
}

// cooling reports whether recent activity means this node should be left
// alone, according to its own pool's policy.
//
// Per node rather than cluster-wide, because the cooldowns are per-pool
// configuration: a pool that wants an hour of quiet after a scale-up must not
// hold up a pool that wants none. The underlying autoscaler behaviour is
// cluster-wide, so this can only ever be more conservative than reality —
// which is the safe direction.
// cooling reports whether recent activity means a node should be left alone,
// with a code naming which of the three reasons applies. They are worth
// telling apart: scaling up right now is the cluster disagreeing with binpack,
// while a cooldown is binpack disagreeing with itself a few minutes ago.
func cooling(s Snapshot, policy Policy) (code, reason string, cooling bool) {
	// Growing right now is the clearest possible signal not to be removing
	// nodes, and it does not depend on any configured duration.
	if s.Autoscaler.ScaleUpInProgress {
		return SkipScaleUpInProgress, "the cluster is scaling up right now", true
	}

	// Draining straight after the cluster grew is how oscillation starts, and
	// the autoscaler pauses its own scale-down then anyway.
	if d := policy.CooldownAfterScaleUp; d > 0 && !s.Autoscaler.LastScaleUp.IsZero() {
		if since := s.Now.Sub(s.Autoscaler.LastScaleUp); since < d {
			return SkipCooldownAfterScaleUp, fmt.Sprintf(
				"the cluster scaled up %s ago; waiting %s before considering a drain",
				since.Round(time.Second), d), true
		}
	}

	if d := policy.CooldownAfterDrain; d > 0 && !s.LastDrain.IsZero() {
		if since := s.Now.Sub(s.LastDrain); since < d {
			return SkipCooldownAfterDrain, fmt.Sprintf(
				"a drain completed %s ago; letting the cluster settle for %s",
				since.Round(time.Second), d), true
		}
	}

	return "", "", false
}

// eligible splits nodes into those worth simulating and those ruled out, with
// a reason recorded for every exclusion so explain can account for the whole
// cluster rather than only the interesting part.
func eligible(s Snapshot, cfg Config) (candidates []*NodeAssessment, ruledOut []NodeAssessment) {
	groups := groupsByID(s)
	protected := protectedNamespaces(s, cfg)

	for _, node := range s.Nodes {
		a := eligibility(s, cfg, groups, protected, node, false)
		if a.Skipped {
			ruledOut = append(ruledOut, a)
			continue
		}
		candidates = append(candidates, &a)
	}

	return candidates, ruledOut
}

func groupsByID(s Snapshot) map[string]NodeGroup {
	groups := make(map[string]NodeGroup, len(s.Autoscaler.Groups))
	for _, g := range s.Autoscaler.Groups {
		groups[g.ID] = g
	}
	return groups
}

// eligibility rules a node in or out before any simulation runs.
//
// resuming is set when the caller is binpack revisiting a drain it started
// itself, and suppresses the two checks binpack is the cause of: its own drain
// marker, and the cordon it applied.
//
// Once that drain has evicted something, the cooldowns are suppressed too —
// a different rule, for a different reason. The marker and the cordon are
// binpack's own answers read back, so re-asking them is wrong at any point in
// a drain. A cooldown is a real observation about the cluster; it is just an
// observation about whether a drain should *start*, and this one has started.
// Re-asking it afterwards cannot undo the evictions it disapproves of. What it
// does is hand back a half-drained node, leaving the pods that already moved
// where they went and billing the node thirty minutes of backoff for a
// cluster-wide event it had no part in. See ADR-0010.
//
// Every other check still applies, and that is the point of re-running them: a
// pool that has since reached its minimum, an operator annotating the node
// mid-drain, or an autoscaler that has stopped managing the pool all mean the
// drain should stop, however much of it is already done.
func eligibility(
	s Snapshot, cfg Config,
	groups map[string]NodeGroup, protected map[string]string,
	node *corev1.Node, resuming bool,
) NodeAssessment {
	a := NodeAssessment{
		Node:  node,
		Group: node.Labels[cfg.NodeGroupIDLabel],
		Pool:  node.Labels[cfg.PoolNameLabel],
	}

	policy := cfg.PolicyFor(a.Group, a.Pool)
	group, managed := groups[a.Group]
	coolCode, cooldown, cooling := cooling(s, policy)

	// Committedness is binpack resuming its own drain, not a property of the
	// node that every caller should honour. Selection rules a node with a
	// drain in flight out further down this same switch, and without the
	// resuming half it would reach that case instead — reporting a different
	// question's answer to explain and diagnose, by an accident of case order
	// rather than a decision.
	committed := resuming && committedDrain(node)

	switch {
	case !managed:
		// Absent from the autoscaler's status means it does not manage
		// this pool, so nothing would ever remove the node.
		a.Skipped, a.SkipCode, a.SkipReason = true, SkipNotAutoscaled, "not part of an autoscaling pool"

	case !policy.Enabled:
		a.Skipped, a.SkipCode, a.SkipReason = true, SkipPoolDisabled, "binpack is disabled for this pool"

	case cooling && !committed:
		a.Skipped, a.SkipCode, a.SkipReason = true, coolCode, cooldown

	case group.Size() <= group.MinSize:
		a.Skipped, a.SkipCode = true, SkipPoolAtMinimum
		a.SkipReason = fmt.Sprintf(
			"pool %s is at its minimum size (%d)", displayPool(a), group.MinSize)

	case node.Annotations[AnnotationSkip] == "true":
		a.Skipped, a.SkipCode, a.SkipReason = true, SkipAnnotated, "annotated "+AnnotationSkip

	case !resuming && node.Annotations[AnnotationDrainStarted] != "":
		a.Skipped, a.SkipCode, a.SkipReason = true, SkipDrainInProgress, "a drain is already in progress on this node"

	case backoffActive(node, s.Now):
		a.Skipped, a.SkipCode, a.SkipReason = true, SkipBackoff, backoffReason(node)

	case resuming && !node.Spec.Unschedulable:
		// Marked but schedulable: either the controller stopped between
		// writing the marker and cordoning, or someone uncordoned a drain in
		// flight. Either way the node is still accepting pods, which is the
		// race the cordon exists to close — evicting from it could relocate
		// work whose fit and PDB demand were never assessed.
		//
		// Refused rather than tolerated, because a pure function cannot fix
		// it. The caller's move is to cordon and let the next evaluation
		// revalidate against a fresh snapshot; proceeding on this one would
		// be reasoning about a node whose pod set can still grow.
		a.Skipped, a.SkipCode = true, SkipUncordoned
		a.SkipReason = "a drain is marked on this node but it is not cordoned, so it is still accepting pods"

	case BeingRemoved(node):
		// Taints rather than spec.unschedulable, so the cordon check below
		// does not catch this. Skipped whether or not binpack started the
		// drain: once the autoscaler is removing a node, there is nothing left
		// for binpack to decide about it.
		a.Skipped, a.SkipCode, a.SkipReason = true, SkipBeingRemoved,
			"the cluster-autoscaler is already removing this node"

	case !resuming && node.Spec.Unschedulable:
		// Already cordoned by someone else. Draining it would not be
		// binpack's to finish, and it is not accepting work anyway.
		a.Skipped, a.SkipCode, a.SkipReason = true, SkipCordoned, "already cordoned"

	case protected[node.Name] != "":
		a.Skipped, a.SkipCode, a.SkipReason = true, SkipProtectedPod, protected[node.Name]
	}

	return a
}

// protectedNamespaces marks nodes hosting a pod binpack must not evict.
//
// The pods are protected, not ignored: removing them from the arithmetic
// while leaving them on the node would be unsound.
//
// The exclusion list is resolved from the pod's own node's pool, not from the
// global default. A pool may widen the list or clear it, and reading the
// default would make both overrides silently ineffective.
func protectedNamespaces(s Snapshot, cfg Config) map[string]string {
	byName := make(map[string]*corev1.Node, len(s.Nodes))
	for _, node := range s.Nodes {
		byName[node.Name] = node
	}

	out := make(map[string]string)
	for _, pod := range s.Pods {
		if pod.Spec.NodeName == "" || !Occupies(pod) {
			continue
		}
		node, known := byName[pod.Spec.NodeName]
		if !known {
			continue
		}
		policy := cfg.PolicyFor(node.Labels[cfg.NodeGroupIDLabel], node.Labels[cfg.PoolNameLabel])
		for _, ns := range policy.ExcludedNamespaces {
			if pod.Namespace != ns {
				continue
			}
			if Classify(pod, policy.Sim.ExpendablePriorityCutoff) == NodeLocal {
				continue
			}
			out[pod.Spec.NodeName] = fmt.Sprintf("hosts %s, in the excluded namespace %s",
				podRef(pod), ns)
		}
	}
	return out
}

func backoffActive(node *corev1.Node, now time.Time) bool {
	until, err := time.Parse(time.RFC3339, node.Annotations[AnnotationBackoffUntil])
	return err == nil && now.Before(until)
}

func backoffReason(node *corev1.Node) string {
	reason := node.Annotations[AnnotationLastFailure]
	until := node.Annotations[AnnotationBackoffUntil]
	if reason == "" {
		return "in backoff until " + until
	}
	return fmt.Sprintf("in backoff until %s after: %s", until, reason)
}

// workloadOn measures how much real work a node is doing, for ordering.
//
// DaemonSet pods are excluded: their footprint is roughly constant per node,
// so counting them would measure node count rather than workload.
func workloadOn(s Snapshot, node *corev1.Node) int64 {
	var total int64
	for _, pod := range s.Pods {
		if pod.Spec.NodeName != node.Name || !Occupies(pod) {
			continue
		}
		if isNodeLocal(pod) {
			continue
		}
		total += memoryOf(pod)
	}
	return total
}

func podsOf(placements []Placement) []*corev1.Pod {
	out := make([]*corev1.Pod, len(placements))
	for i, p := range placements {
		out[i] = p.Pod
	}
	return out
}

func displayPool(a NodeAssessment) string {
	if a.Pool != "" {
		return a.Pool
	}
	return a.Group
}

// commonestSkip returns the most frequent skip reason, so a cluster where
// nothing was eligible still explains itself rather than shrugging.
func commonestSkip(assessments []NodeAssessment) (string, int) {
	counts := make(map[string]int)
	for _, a := range assessments {
		if a.Skipped {
			counts[a.SkipReason]++
		}
	}
	var best string
	var bestN int
	for reason, n := range counts {
		if n > bestN || (n == bestN && reason < best) {
			best, bestN = reason, n
		}
	}
	return best, bestN
}

// summarise turns a set of rejections into one sentence. "Nothing to do" is
// not an answer; an operator wants to know which wall was hit.
// Considered counts the nodes that were actually assessed as candidates,
// which is fewer than the nodes looked at: those ruled out before any
// simulation ran were never candidates.
//
// Exported because the log line and the prose have to agree. They used the
// same word for two different counts — "4 nodes considered" beside a reason
// reading "2 node(s) considered" — and of the two, the one that excludes
// skipped nodes is the useful answer.
func Considered(assessments []NodeAssessment) int {
	n := 0
	for _, a := range assessments {
		if !a.Skipped {
			n++
		}
	}
	return n
}

func summarise(assessments []NodeAssessment) string {
	considered := Considered(assessments)
	var infeasible, blocked int
	for _, a := range assessments {
		switch {
		case a.Blockers != nil:
			blocked++
		case a.Simulation != nil && !a.Simulation.Feasible:
			infeasible++
		}
	}

	if considered == 0 {
		if reason, n := commonestSkip(assessments); reason != "" {
			if n == len(assessments) {
				return fmt.Sprintf("no node was eligible: %s", reason)
			}
			return fmt.Sprintf("no node was eligible; most commonly: %s", reason)
		}
		return "no node was eligible to consider"
	}

	switch {
	case blocked > 0 && infeasible > 0:
		return fmt.Sprintf("%d node(s) considered: %d could not be emptied, %d had pods that cannot be evicted",
			considered, infeasible, blocked)
	case blocked > 0:
		return fmt.Sprintf("%d node(s) considered, all with pods that cannot be evicted", considered)
	default:
		return fmt.Sprintf("%d node(s) considered, none whose workload fits elsewhere", considered)
	}
}

// PoolNames maps each autoscaling group's identifier to the human-readable
// pool name its nodes carry.
//
// The two names come from different places: the identifier from the
// cluster-autoscaler's status, the readable name from a node label. Anything
// shown to a person wants the second — nobody recognises a provider UUID on a
// dashboard or in an alert — and anything matching against the autoscaler
// wants the first.
//
// A pool with no nodes has nothing to take a name from and is absent here, so
// callers fall back to the identifier.
func PoolNames(s Snapshot, cfg Config) map[string]string {
	names := map[string]string{}
	for _, node := range s.Nodes {
		id := node.Labels[cfg.NodeGroupIDLabel]
		name := node.Labels[cfg.PoolNameLabel]
		if id != "" && name != "" {
			names[id] = name
		}
	}
	return names
}

// CheckPools rejects per-pool overrides naming a pool that is not there.
//
// Pools are discovered, never declared, so an override adjusts something that
// exists. A misspelt name otherwise installs an unreachable map entry and its
// nodes quietly take the default policy — which is actively dangerous for
// `enabled: false`, where an operator believes they have switched a pool off
// and binpack goes on considering it drainable.
//
// Checked against the resolved configuration rather than the document, so what
// is validated is what the engine will actually consult. Every frontend calls
// it: a configuration `explain` refuses must not be one `run` accepts, and the
// controller is the one that will eventually act on it.
func CheckPools(s Snapshot, cfg Config) error {
	if len(cfg.ByPool) == 0 {
		return nil
	}

	known := map[string]bool{}
	for _, g := range s.Autoscaler.Groups {
		known[g.ID] = true
	}
	for _, node := range s.Nodes {
		if name := node.Labels[cfg.PoolNameLabel]; name != "" {
			known[name] = true
		}
		if id := node.Labels[cfg.NodeGroupIDLabel]; id != "" {
			known[id] = true
		}
	}

	var unknown []string
	for name := range cfg.ByPool {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}

	// Sorted: the names come from a map, and an error that reorders itself
	// between runs is one nobody can diff or match against a log.
	sort.Strings(unknown)
	return fmt.Errorf(
		"configuration overrides pools that do not exist in this cluster: %s\n"+
			"pools are discovered, not declared, so an override must name one that is there;\n"+
			"check for a typo, or remove the entry if the pool is gone",
		strings.Join(unknown, ", "))
}
