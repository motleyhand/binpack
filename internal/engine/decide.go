package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
)

// Annotations binpack writes on nodes. Fixed keys, not configurable: one thing
// to document, one thing to grep for, and no possibility of two clusters
// disagreeing about what protects a node.
const (
	AnnotationSkip         = "binpack.motleyhand.com/skip"
	AnnotationDrainStarted = "binpack.motleyhand.com/drain-started"
	AnnotationBackoffUntil = "binpack.motleyhand.com/backoff-until"
	AnnotationLastFailure  = "binpack.motleyhand.com/last-failure"
)

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
	ExcludedNamespaces   []string
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
	SkipBackoff              = "backoff"
	SkipCordoned             = "cordoned"
	SkipProtectedPod         = "protected-pod"
	SkipTooManyPods          = "too-many-pods"
)

// Decision codes: the outcome of a whole evaluation.
const (
	CodeDrain        = "drain"
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
		policy := cfg.PolicyFor(a.Group, a.Pool)

		sim := Simulate(s.Nodes, s.Pods, s.Templates, a.Node, policy.Sim)
		a.Simulation = &sim
		if !sim.Feasible {
			assessments = append(assessments, *a)
			continue
		}

		if policy.MaxPodsPerDrain > 0 && len(sim.Relocated) > policy.MaxPodsPerDrain {
			a.Skipped = true
			a.SkipCode = SkipTooManyPods
			a.SkipReason = fmt.Sprintf("would relocate %d pods, above the limit of %d",
				len(sim.Relocated), policy.MaxPodsPerDrain)
			assessments = append(assessments, *a)
			continue
		}

		evicting := append(append([]*corev1.Pod{}, podsOf(sim.Relocated)...), sim.Evicted...)
		if blockers := CheckEvictable(evicting, s.PDBs, policy.Evict); len(blockers) > 0 {
			a.Blockers = blockers
			assessments = append(assessments, *a)
			continue
		}

		if chosen < 0 {
			chosen = len(assessments)
		}
		assessments = append(assessments, *a)
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
	groups := make(map[string]NodeGroup, len(s.Autoscaler.Groups))
	for _, g := range s.Autoscaler.Groups {
		groups[g.ID] = g
	}

	protected := protectedNamespaces(s, cfg)

	for _, node := range s.Nodes {
		a := NodeAssessment{
			Node:  node,
			Group: node.Labels[cfg.NodeGroupIDLabel],
			Pool:  node.Labels[cfg.PoolNameLabel],
		}

		policy := cfg.PolicyFor(a.Group, a.Pool)
		group, managed := groups[a.Group]
		coolCode, cooldown, cooling := cooling(s, policy)

		switch {
		case !managed:
			// Absent from the autoscaler's status means it does not manage
			// this pool, so nothing would ever remove the node.
			a.Skipped, a.SkipCode, a.SkipReason = true, SkipNotAutoscaled, "not part of an autoscaling pool"

		case !policy.Enabled:
			a.Skipped, a.SkipCode, a.SkipReason = true, SkipPoolDisabled, "binpack is disabled for this pool"

		case cooling:
			a.Skipped, a.SkipCode, a.SkipReason = true, coolCode, cooldown

		case group.Size() <= group.MinSize:
			a.Skipped, a.SkipCode = true, SkipPoolAtMinimum
			a.SkipReason = fmt.Sprintf(
				"pool %s is at its minimum size (%d)", displayPool(a), group.MinSize)

		case node.Annotations[AnnotationSkip] == "true":
			a.Skipped, a.SkipCode, a.SkipReason = true, SkipAnnotated, "annotated "+AnnotationSkip

		case node.Annotations[AnnotationDrainStarted] != "":
			a.Skipped, a.SkipCode, a.SkipReason = true, SkipDrainInProgress, "a drain is already in progress on this node"

		case backoffActive(node, s.Now):
			a.Skipped, a.SkipCode, a.SkipReason = true, SkipBackoff, backoffReason(node)

		case node.Spec.Unschedulable:
			// Already cordoned by someone else. Draining it would not be
			// binpack's to finish, and it is not accepting work anyway.
			a.Skipped, a.SkipCode, a.SkipReason = true, SkipCordoned, "already cordoned"

		case protected[node.Name] != "":
			a.Skipped, a.SkipCode, a.SkipReason = true, SkipProtectedPod, protected[node.Name]
		}

		if a.Skipped {
			ruledOut = append(ruledOut, a)
			continue
		}
		candidates = append(candidates, &a)
	}

	return candidates, ruledOut
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
		if pod.Spec.NodeName == "" || !occupies(pod) {
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
		if pod.Spec.NodeName != node.Name || !occupies(pod) {
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
func summarise(assessments []NodeAssessment) string {
	var considered, infeasible, blocked int
	for _, a := range assessments {
		switch {
		case a.Blockers != nil:
			blocked++
			considered++
		case a.Simulation != nil && !a.Simulation.Feasible:
			infeasible++
			considered++
		case !a.Skipped:
			considered++
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
