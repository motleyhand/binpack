package engine

import (
	"fmt"
	"sort"
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

// Snapshot is the cluster as binpack sees it. Plain data: no clients, nothing
// that performs I/O, and every object read-only.
type Snapshot struct {
	Nodes []*corev1.Node
	Pods  []*corev1.Pod
	PDBs  []*policyv1.PodDisruptionBudget

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

	Groups []NodeGroup
}

// NodeGroup is one autoscaling pool, as the autoscaler reports it. Pools it
// does not manage are absent, which is how binpack knows not to touch them.
type NodeGroup struct {
	// ID matches the value of Config.NodeGroupIDLabel on a node.
	ID      string
	Name    string
	MinSize int
	MaxSize int
	Ready   int
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

// Decision is the engine's answer, carrying the arithmetic that produced it.
//
// Every field exists so `binpack explain` can show its working. A decision
// that cannot be explained is one nobody should act on.
type Decision struct {
	Action Action
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
	// SkipReason saying why.
	Skipped    bool
	SkipReason string

	// Simulation and Blockers are populated only for nodes that reached
	// those stages.
	Simulation *Simulation
	Blockers   []EvictionBlocker

	// Chosen marks the node binpack decided to drain.
	Chosen bool
}

// Decide runs the whole procedure and returns one action.
//
// Deliberately one node per run. Iterative beats clever: the next run observes
// fresh state, which is both safer and far simpler to reason about than
// planning a multi-node consolidation against a cluster that is changing
// underneath the plan.
func Decide(s Snapshot, cfg Config) Decision {
	if !s.Autoscaler.Running {
		return Decision{Reason: "no cluster-autoscaler is running, so a drained node would never be removed"}
	}

	if wait, ok := cooling(s, cfg); ok {
		return Decision{Reason: wait}
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

		sim := Simulate(s.Nodes, s.Pods, a.Node, policy.Sim)
		a.Simulation = &sim
		if !sim.Feasible {
			assessments = append(assessments, *a)
			continue
		}

		if policy.MaxPodsPerDrain > 0 && len(sim.Relocated) > policy.MaxPodsPerDrain {
			a.Skipped = true
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
			Node:        assessments[chosen].Node,
			Assessments: assessments,
		}
	}

	return Decision{
		Reason:      summarise(assessments),
		Assessments: assessments,
	}
}

// cooling reports whether recent activity means binpack should wait.
func cooling(s Snapshot, cfg Config) (string, bool) {
	// The autoscaler pauses all scale-down after any scale-up, so acting
	// inside that window achieves nothing anyway — and draining straight
	// after the cluster grew is how oscillation starts.
	if d := cfg.Default.CooldownAfterScaleUp; d > 0 && !s.Autoscaler.LastScaleUp.IsZero() {
		if since := s.Now.Sub(s.Autoscaler.LastScaleUp); since < d {
			return fmt.Sprintf("the cluster scaled up %s ago; waiting %s before considering a drain",
				since.Round(time.Second), d), true
		}
	}

	if d := cfg.Default.CooldownAfterDrain; d > 0 && !s.LastDrain.IsZero() {
		if since := s.Now.Sub(s.LastDrain); since < d {
			return fmt.Sprintf("a drain completed %s ago; letting the cluster settle for %s",
				since.Round(time.Second), d), true
		}
	}

	return "", false
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

		group, managed := groups[a.Group]
		switch {
		case !managed:
			// Absent from the autoscaler's status means it does not manage
			// this pool, so nothing would ever remove the node.
			a.Skipped, a.SkipReason = true, "not part of an autoscaling pool"

		case !cfg.PolicyFor(a.Group, a.Pool).Enabled:
			a.Skipped, a.SkipReason = true, "binpack is disabled for this pool"

		case group.Ready <= group.MinSize:
			a.Skipped, a.SkipReason = true, fmt.Sprintf(
				"pool %s is at its minimum size (%d)", displayPool(a), group.MinSize)

		case node.Annotations[AnnotationSkip] == "true":
			a.Skipped, a.SkipReason = true, "annotated "+AnnotationSkip

		case node.Annotations[AnnotationDrainStarted] != "":
			a.Skipped, a.SkipReason = true, "a drain is already in progress on this node"

		case backoffActive(node, s.Now):
			a.Skipped, a.SkipReason = true, backoffReason(node)

		case node.Spec.Unschedulable:
			// Already cordoned by someone else. Draining it would not be
			// binpack's to finish, and it is not accepting work anyway.
			a.Skipped, a.SkipReason = true, "already cordoned"

		case protected[node.Name] != "":
			a.Skipped, a.SkipReason = true, protected[node.Name]
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
func protectedNamespaces(s Snapshot, cfg Config) map[string]string {
	out := make(map[string]string)
	for _, pod := range s.Pods {
		if pod.Spec.NodeName == "" || !occupies(pod) {
			continue
		}
		policy := cfg.Default
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

	switch {
	case considered == 0:
		return "no node was eligible to consider"
	case blocked > 0 && infeasible > 0:
		return fmt.Sprintf("%d node(s) considered: %d could not be emptied, %d had pods that cannot be evicted",
			considered, infeasible, blocked)
	case blocked > 0:
		return fmt.Sprintf("%d node(s) considered, all with pods that cannot be evicted", considered)
	default:
		return fmt.Sprintf("%d node(s) considered, none whose workload fits elsewhere", considered)
	}
}
