package engine

import (
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

// This file answers one question — which autoscaling pool is a node in? — and
// it is the only place that answers it. Everything else asks through
// [Config.GroupOf].
//
// The question is harder than it looks because the two halves come from
// different places and were named by different people. The cluster-autoscaler
// publishes `nodeGroups[].name`, which is whatever the cloud provider's
// `Id()` returns: a node pool ID on DigitalOcean, an Auto Scaling group name
// on AWS, a VM Scale Set name on Azure, a full instance-group URL on GCE. A
// node carries labels, which are the provider's or the operator's. Joining
// them is the whole of pool discovery, and getting it wrong applies one
// pool's floor to another pool's nodes — a confidently wrong decision of
// exactly the kind internal/fit exists to prevent.
//
// Three ways the join can be established, tried in this order. See ADR-0013.

// MappingSource says how binpack established the join between nodes and the
// identifiers the cluster-autoscaler publishes.
//
// The order is the precedence order, and it runs from most stated to least:
// something an operator chose beats something binpack worked out.
type MappingSource int

const (
	// MappedNothing is no join at all. Either there was no evidence to
	// reason from — no live autoscaler, no published pool with a node in it —
	// or every candidate was rejected, in which case preflight refuses.
	MappedNothing MappingSource = iota

	// MappedByValue is the join ADR-0012 describes and the only one there
	// was: some node's `discovery.nodeGroupIDLabel` value *is* an identifier
	// the autoscaler published. DOKS out of the box, and any cluster whose
	// operator applied the label themselves.
	MappedByValue

	// MappedByConfiguration is a join `discovery.nodeGroups` states outright.
	// Membership only — pool bounds still come from the status ConfigMap.
	MappedByConfiguration

	// MappedByName is derived: a label whose values the identifiers were
	// generated from. Attempted only when neither of the above matched
	// anything, and refused unless it claims every published pool at once.
	MappedByName
)

func (s MappingSource) String() string {
	switch s {
	case MappedByValue:
		return "the configured label's value is the identifier"
	case MappedByConfiguration:
		return "discovery.nodeGroups states it"
	case MappedByName:
		return "derived from the pool names the identifiers were built from"
	default:
		return "no pool mapping"
	}
}

// PoolMapping is the join binpack will use, and the account of how it got
// there.
//
// The account is not decoration. A heuristic that decides *scope* has to be
// reported or it is indistinguishable from binpack quietly changing its mind
// about which nodes it manages, so `binpack explain` names the key and says
// why it chose it, and a refusal says what it tried.
type PoolMapping struct {
	Source MappingSource

	// Key is the node label the join reads. Empty only when Source is
	// MappedNothing.
	Key string

	// Groups translates a value of Key into the identifier the autoscaler
	// published. Nil for MappedByValue, where the value is the identifier;
	// a value absent from a non-nil map falls back to that same equality, so
	// stating one pool never takes the others out of scope.
	Groups map[string]string

	// AlsoAgreed are the other label keys that induce exactly the same join,
	// sorted. On a single-pool cluster there are usually several — the eksctl
	// cluster name resolves as well as the node group name — and reporting
	// them is what stops the choice between them looking arbitrary.
	AlsoAgreed []string

	// Rejected are the candidate keys that came close and the reason each
	// was turned down, sorted by key. Only keys with a value inside some
	// published identifier appear: every other label is a near miss in no
	// sense at all, and listing them would bury the one that matters.
	Rejected []Rejection
}

// Rejection is one candidate key the derivation turned down.
type Rejection struct {
	Key string
	// Why is a sentence completing "…, but ", written for the operator who
	// has to decide what to do about it.
	Why string
}

// GroupOf is the identifier the cluster-autoscaler publishes for the pool a
// node belongs to, or "" for a node in no pool binpack can see.
//
// Every caller goes through here. The alternative — reading
// `node.Labels[cfg.NodeGroupIDLabel]` at each site, as the code did while
// equality was the only join — is nine places that have to be changed
// together, and the one that is missed reports a node as unmanaged rather
// than failing.
func (c Config) GroupOf(node *corev1.Node) string {
	value := node.Labels[c.mappingKey()]
	if value == "" {
		return ""
	}
	if id, ok := c.Mapping.Groups[value]; ok {
		return id
	}
	// No translation for this value, so the value is the identifier. That is
	// the ADR-0012 join, and it is what an unresolved Config does for every
	// value — which is why forgetting to use the Config [ResolvePools]
	// returns costs the derivation rather than producing a wrong answer.
	return value
}

// mappingKey is the label the join reads: whatever was derived, falling back
// to the configured key.
func (c Config) mappingKey() string {
	if c.Mapping.Key != "" {
		return c.Mapping.Key
	}
	return c.NodeGroupIDLabel
}

// PolicyForNode resolves the policy governing one node's pool, and is the
// only way to do that. Nothing else may resolve a node's policy: three call
// sites once passed `(a.Group, a.Pool)` instead, which was the same call until
// a derived join made a node's group and its label value two different
// strings, and then silently stopped honouring an override named after the
// label — including `enabled: false`, which is the one an operator most
// expects to be obeyed.
//
// Four names, because a `pools` entry may be written as any of them and an
// operator should not have to know which one binpack matches on: the
// identifier the autoscaler publishes, the value of whichever label the join
// reads, the value of the label `discovery.nodeGroupIDLabel` configures, and
// the human-readable pool name. [ResolvePools] accepts all four when it
// validates an override, and accepting a name there while ignoring it here is
// worse than refusing it.
//
// Where the join is plain equality the middle two are the same string and this
// is the two-name lookup it has always been.
func (c Config) PolicyForNode(node *corev1.Node) Policy {
	return c.policyFor(c.GroupOf(node), node.Labels[c.mappingKey()],
		node.Labels[c.NodeGroupIDLabel], node.Labels[c.PoolNameLabel])
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
// callers fall back to the identifier — through [PoolNaming.Label], which is
// that fallback written once.
func PoolNames(s Snapshot, cfg Config) PoolNaming {
	names := PoolNaming{}
	for _, node := range s.Nodes {
		id := cfg.GroupOf(node)
		name := node.Labels[cfg.PoolNameLabel]
		if id != "" && name != "" {
			names[id] = name
		}
	}
	return names
}

// PoolNaming resolves a node group's identifier to what a person should see.
//
// A type with a method rather than a bare map, so that the fallback every
// caller needs has one implementation. It had three, plus two callers that
// skipped it — see [NodeAssessment.PoolLabel], which answers the same question
// for a caller holding an assessment.
type PoolNaming map[string]string

// Label is the readable name for a group, or the identifier where the cluster
// carries no readable name for it.
func (n PoolNaming) Label(id string) string {
	if name := n[id]; name != "" {
		return name
	}
	return id
}

// NodeGroupLabelSuggestion is the label binpack recommends where nothing a
// provider already writes carries the autoscaler's identifier for a pool.
//
// Under binpack's own API group deliberately: a key in a provider's namespace
// is one the provider may start writing itself, and a value it then overwrites
// is a mapping that breaks with no configuration having changed.
const NodeGroupLabelSuggestion = "binpack.motleyhand.com/node-group"

// liveGroups are the published groups binpack can reason about: named, and
// with a node in them.
//
// A group the status names with an empty string would be matched by every
// unlabelled node, which turns every check here off rather than failing it —
// `name` is omitempty upstream, so the value is representable. A group with
// no ready node has nothing to carry its identifier and is not evidence of
// anything, in either direction.
//
// Sorted by identifier: the status document's order is the autoscaler's, and
// everything downstream of this either reports it or iterates it.
func liveGroups(a Autoscaler) []NodeGroup {
	var out []NodeGroup
	for _, g := range a.Groups {
		if g.ID != "" && g.Ready > 0 {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, func(x, y NodeGroup) int { return strings.Compare(x.ID, y.ID) })
	return out
}

// resolveMapping establishes the join, in precedence order.
//
// Configured first, because an operator who has stated the answer has settled
// it. Then equality, which is ADR-0012's join and the only one until now.
// Only if neither reaches a single node does binpack derive one — so DOKS and
// every hand-labelled cluster take exactly the path they took before, and the
// derivation cannot change what any working cluster does.
func resolveMapping(s Snapshot, cfg Config) PoolMapping {
	if live, _, _ := s.Autoscaler.Live(s.Now); !live || len(s.Nodes) == 0 {
		return PoolMapping{}
	}
	groups := liveGroups(s.Autoscaler)
	if len(groups) == 0 {
		return PoolMapping{}
	}

	published := map[string]bool{}
	for _, g := range groups {
		published[g.ID] = true
	}

	// Configured and equality are one pass: the configured table translates
	// the values it names and equality answers for the rest, so a document
	// naming one pool leaves the others exactly as they were.
	stated := PoolMapping{Source: MappedByValue, Key: cfg.NodeGroupIDLabel}
	if len(cfg.NodeGroups) > 0 {
		stated.Groups = cfg.NodeGroups
	}
	// Every node rather than the first that matches, even though one match is
	// enough to settle *whether* the join works. What it does not settle is
	// which half of it did: with an entry in discovery.nodeGroups and another
	// value that matches outright, the answer would be whichever node the API
	// server happened to list first, and collect.Snapshot sorts nothing.
	matched, byConfiguration := false, false
	for _, node := range s.Nodes {
		if !published[stated.groupOf(node)] {
			continue
		}
		matched = true
		if _, named := cfg.NodeGroups[node.Labels[cfg.NodeGroupIDLabel]]; named {
			byConfiguration = true
		}
	}
	if matched {
		// One node matching is proof the join works, so a cluster halfway
		// through being relabelled resolves rather than refusing.
		if byConfiguration {
			stated.Source = MappedByConfiguration
		}
		return stated
	}

	return derive(s, groups)
}

// groupOf is [Config.GroupOf] against a mapping that is not in a Config yet.
func (m PoolMapping) groupOf(node *corev1.Node) string {
	value := node.Labels[m.Key]
	if value == "" {
		return ""
	}
	if id, ok := m.Groups[value]; ok {
		return id
	}
	return value
}

// derive works the join out from the labels the nodes already carry.
//
// The insight it rests on is that a provider *generates* the identifier from
// the pool name, so the pool label's value is a substring of it:
// `workers` inside `eks-workers-a2c1d3e4-…`, `nodepool1` inside
// `aks-nodepool1-33555069-vmss`, `default-pool` inside the instance-group URL
// GCE publishes — which is the case equality cannot reach at all, since no
// label value may contain `/` or `:`.
//
// A substring on its own is reckless: `linux` and `amd64` sit inside plenty
// of things. What makes it safe is the bijection around it. A candidate key
// has to partition the nodes into exactly as many groups as the autoscaler
// publishes, at sizes those pools could plausibly have, matched onto them
// one-to-one in exactly one way — and every key that manages it has to agree
// about every node. Nothing resolves unless *every published pool* is claimed
// at once, so the failure mode is a refusal rather than a silent change of
// scope. That is the objection ADR-0012 raised against inferring the join, and
// it is what this answers.
//
// The claim is about pools and not about nodes, and the difference is not
// pedantry. Most clusters hold nodes in no autoscaling pool at all — a
// self-managed box, a pool the autoscaler was never told about — and none of
// them carries a provider's pool label. Requiring every node to contribute a
// value would refuse those clusters outright, in favour of the silent
// mismatch ADR-0012 exists to end. So a node outside every pool stays outside
// every pool, which under-scopes rather than over-scopes: the floors binpack
// enforces come from the autoscaler's own status and not from counting nodes,
// so a node the mapping omits is one binpack never drains and no arithmetic
// depends on.
//
// What that leaves unguarded, said plainly: a node hand-labelled with a
// pool's own name is counted as being in that pool. No label-based join can
// tell otherwise — ADR-0012's equality join has exactly the same hole — and
// closing it needs a cloud API, which ADR-0004 rules out.
func derive(s Snapshot, groups []NodeGroup) PoolMapping {
	out := PoolMapping{Source: MappedNothing}

	keys := map[string]bool{}
	for _, node := range s.Nodes {
		for key := range node.Labels {
			keys[key] = true
		}
	}

	var winners []PoolMapping
	for _, key := range slices.Sorted(maps.Keys(keys)) {
		mapping, why := match(s, groups, key)
		switch {
		case why == "":
			winners = append(winners, mapping)
		case why != skipSilently:
			out.Rejected = append(out.Rejected, Rejection{Key: key, Why: why})
		}
	}
	if len(winners) == 0 {
		return out
	}

	// Keys in sorted order, so the one reported does not depend on the order
	// the cluster was read — collect.Snapshot appends nodes as the API server
	// listed them and sorts nothing.
	first := winners[0]
	for _, other := range winners[1:] {
		for _, node := range s.Nodes {
			if first.groupOf(node) == other.groupOf(node) {
				continue
			}
			// Two keys that each resolve the cluster and put a node in
			// different pools. One is wrong and nothing here says which, so
			// this is the same coin toss as an ambiguous matching and gets
			// the same answer.
			out.Rejected = append(out.Rejected, Rejection{Key: first.Key, Why: fmt.Sprintf(
				"%s and %s disagree about %s", first.Key, other.Key, node.Name)})
			return out
		}
		first.AlsoAgreed = append(first.AlsoAgreed, other.Key)
	}
	return first
}

// skipSilently marks a candidate key that is a near miss in no sense at all —
// no value of it sits inside any published identifier — so it is not worth an
// operator's attention. Every cluster has dozens.
const skipSilently = "\x00"

// match tests one candidate key, returning either the join it induces or the
// reason it does not.
func match(s Snapshot, groups []NodeGroup, key string) (PoolMapping, string) {
	// Values sorted, and members counted rather than collected: the size is
	// all the matching needs, and a sorted value order makes the matching
	// itself independent of node order.
	sizes := map[string]int{}
	for _, node := range s.Nodes {
		if value, ok := node.Labels[key]; ok && value != "" {
			sizes[value]++
		}
	}
	values := slices.Sorted(maps.Keys(sizes))

	// Candidate edges. A value may join a group when it sits inside that
	// group's identifier *and* the number of nodes carrying it is a size
	// that group could have.
	edges := map[string][]string{}
	inside := false
	for _, value := range values {
		for _, g := range groups {
			if !strings.Contains(g.ID, value) {
				continue
			}
			inside = true
			if plausible(g, sizes[value]) {
				edges[value] = append(edges[value], g.ID)
			}
		}
	}
	if !inside {
		return PoolMapping{}, skipSilently
	}

	// Checked after the substring test rather than before, so that a key
	// which is genuinely close — the provider's own pool label on a cluster
	// with a pool binpack cannot see — is reported rather than skipped.
	if len(values) != len(groups) {
		return PoolMapping{}, fmt.Sprintf(
			"its %d value(s) do not correspond one-to-one with the %d pool(s) the "+
				"cluster-autoscaler publishes", len(values), len(groups))
	}

	pairs, ok := matching(values, edges)
	if !ok {
		// Distinguish the two ways this fails, because they call for
		// different things: a value inside nothing is a naming question, and
		// a value inside something at the wrong size is a counting one.
		for _, value := range values {
			if len(edges[value]) > 0 {
				continue
			}
			for _, g := range groups {
				if strings.Contains(g.ID, value) {
					lo, hi := bounds(g)
					return PoolMapping{}, fmt.Sprintf(
						"%d node(s) carry %s=%s while the pool it names, %s, is a pool of "+
							"%s", sizes[value], key, value, g.ID, span(lo, hi))
				}
			}
		}
		return PoolMapping{}, "its values cannot be matched onto those pools one-to-one"
	}
	// A perfect matching that is not the only one is a coin toss, and binpack
	// would report the result as a fact. Forced by two pools each of whose
	// identifiers contains the other's name; see ADR-0013.
	for value, id := range pairs {
		if _, second := matching(values, without(edges, value, id)); second {
			return PoolMapping{}, "its values can be matched onto those pools in more than one way"
		}
	}

	return PoolMapping{Source: MappedByName, Key: key, Groups: pairs}, ""
}

// plausible reports whether a pool of the reported shape could hold n nodes.
//
// Between what the autoscaler counts ready and what it has asked the provider
// for, inclusive, because the two differ in both directions while a pool is
// changing size: a node that has registered but is not counted ready yet
// during a scale-up, and one the target has already excluded during a
// scale-down. Demanding equality with either would make binpack refuse on
// every scale event, which is the opposite of working out of the box.
//
// Deliberately not [NodeGroup.Size], which answers a different question and
// answers it conservatively — it takes the *smaller* of intent and reality,
// because either being at the floor means a drained node comes straight back.
// Using it here collapses the scale-up half of the window to a point and
// refuses exactly the case this exists to allow.
func plausible(g NodeGroup, n int) bool {
	lo, hi := bounds(g)
	return lo <= n && n <= hi
}

// bounds is the window, as two numbers, for [plausible] and for saying why a
// key was rejected. One function, because a refusal that quotes different
// numbers from the ones it judged on sends an operator after the wrong thing.
func bounds(g NodeGroup) (lo, hi int) {
	lo, hi = g.Ready, g.Ready
	if g.HasTarget {
		lo, hi = min(lo, g.Target), max(hi, g.Target)
	}
	return lo, hi
}

// span renders a size window for a person: "3 nodes", or "3 to 4 nodes" while
// the pool is changing size.
func span(lo, hi int) string {
	if lo == hi {
		return fmt.Sprintf("%d node(s)", lo)
	}
	return fmt.Sprintf("%d to %d node(s)", lo, hi)
}

// matching finds a perfect matching of values onto groups by augmenting
// paths. The graph has one vertex per pool, so anything cleverer than this
// would be slower to read for no measurable gain.
//
// Values are taken in sorted order and each value's edges are in the sorted
// group order [liveGroups] produced, so the matching returned is the same one
// for the same cluster however it was read.
func matching(values []string, edges map[string][]string) (map[string]string, bool) {
	byGroup := map[string]string{}

	var assign func(value string, seen map[string]bool) bool
	assign = func(value string, seen map[string]bool) bool {
		for _, id := range edges[value] {
			if seen[id] {
				continue
			}
			seen[id] = true
			if held, taken := byGroup[id]; !taken || assign(held, seen) {
				byGroup[id] = value
				return true
			}
		}
		return false
	}

	for _, value := range values {
		if !assign(value, map[string]bool{}) {
			return nil, false
		}
	}

	out := make(map[string]string, len(byGroup))
	for id, value := range byGroup {
		out[value] = id
	}
	return out, true
}

// without copies the edge set with one edge removed, for the uniqueness test.
func without(edges map[string][]string, value, id string) map[string][]string {
	out := make(map[string][]string, len(edges))
	for v, ids := range edges {
		if v != value {
			out[v] = ids
			continue
		}
		out[v] = slices.DeleteFunc(slices.Clone(ids), func(g string) bool { return g == id })
	}
	return out
}

// checkNodeGroupLabel rejects a cluster where nothing maps a node to any pool
// the autoscaler reports.
//
// binpack matches a node to a pool through one label, and by default the
// match is on that label's *value*: `nodeGroups[].name` in the status
// ConfigMap is the cloud provider's own identifier for the group, so a node
// has to carry that identifier. Where none does and nothing can be derived,
// every node falls out of scope and binpack reports "not part of an
// autoscaling pool" about a cluster of nothing but autoscaled nodes — printed
// directly beneath its own list of healthy pools, with a count from the
// summary making the wrong answer sound more certain. Nothing about that
// state tells an operator which of the two halves is wrong. `diagnose` goes
// quiet in one particular way too: every node is static, so every finding
// diagnoseWorkloads groups frees nothing and `--fail-on` skips it. Budget and
// pool findings are unaffected, which is narrower than it first reads — see
// ADR-0012.
//
// Refused only on positive evidence, because refusing is the expensive
// direction. A status document nothing is updating says nothing about the
// cluster now, and Live already names that as the problem; a group with no
// ready node has nothing to carry its identifier; and one node matching is
// proof the mapping works, so a cluster halfway through being relabelled goes
// on running.
func checkNodeGroupLabel(s Snapshot, cfg Config) error {
	if cfg.Mapping.Source != MappedNothing {
		return nil
	}
	// With no nodes there is no evidence and nothing to report: the error's
	// content is what the nodes do carry, and an empty list of that is a
	// sentence about a cluster binpack cannot see rather than one it has
	// diagnosed.
	if live, _, _ := s.Autoscaler.Live(s.Now); !live || len(s.Nodes) == 0 {
		return nil
	}
	// Nothing published has a node in it, so nothing could have matched and
	// there is no evidence either way. It also leaves `published` non-empty
	// wherever this returns an error, which the remedy below relies on.
	groups := liveGroups(s.Autoscaler)
	if len(groups) == 0 {
		return nil
	}

	var published []string
	for _, g := range groups {
		published = append(published, g.ID)
	}
	// Every named group, not only the live ones: a pool scaled to zero is not
	// evidence of a mismatch, but an operator reading the list wants to see
	// the pool they are looking for in it.
	for _, g := range s.Autoscaler.Groups {
		if g.ID != "" && g.Ready == 0 {
			published = append(published, g.ID)
		}
	}

	keys := map[string]bool{}
	for _, node := range s.Nodes {
		for key := range node.Labels {
			keys[key] = true
		}
	}

	// Both lists sorted, for the reason the summary sentence needed one:
	// collect.Snapshot appends nodes in the order the API server listed them
	// and sorts nothing, and the group order is the status document's. An
	// unchanged cluster would otherwise word its refusal differently on every
	// run, and a refusal nobody can diff against the last one is one nobody
	// can tell they have already read.
	sort.Strings(published)
	present := slices.Sorted(maps.Keys(keys))

	return fmt.Errorf(
		"no node carries a pool identifier the cluster-autoscaler recognises, so binpack\n"+
			"can see no autoscaling pool at all;\n"+
			"discovery.nodeGroupIDLabel is %q, and a node belongs to a pool\n"+
			"when that label's value equals a group name the autoscaler publishes\n"+
			"  the autoscaler publishes: %s\n"+
			"  the nodes carry the labels: %s\n"+
			"%s"+
			"if one of those labels already holds a group name, set discovery.nodeGroupIDLabel\n"+
			"to it; otherwise label each pool's nodes yourself and set it to that:\n"+
			"  kubectl label nodes <node>... %s=%s",
		cfg.NodeGroupIDLabel,
		strings.Join(published, ", "),
		strings.Join(present, ", "),
		nearMisses(cfg.Mapping.Rejected),
		NodeGroupLabelSuggestion, published[0])
}

// nearMisses is the paragraph naming what the derivation tried.
//
// binpack can also work the join out from a label whose values the identifiers
// were built from, and where that declines the operator is one `kubectl label`
// away either way — but only if they are told which key came close and what
// was wrong with it. Empty when nothing came close, so the common message is
// the one ADR-0012 settled on.
func nearMisses(rejected []Rejection) string {
	if len(rejected) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("binpack also tried to work the mapping out from the labels the nodes " +
		"already carry,\nbecause a provider usually builds the identifier out of the pool " +
		"name — but it has\nto resolve every pool or none, and here it could not:\n")
	for _, r := range rejected {
		fmt.Fprintf(&b, "  %s, but %s\n", r.Key, r.Why)
	}
	return b.String()
}

// checkStatedJoin rejects a discovery.nodeGroups entry naming a pool the
// autoscaler does not publish.
//
// The same reasoning as the pool-override check below, one step earlier and
// one degree worse. A misspelt override installs an unreachable map entry and
// its nodes quietly take the default policy; a misspelt join says nothing at
// all, leaves the nodes it meant to place outside every pool, and lets the
// derivation answer instead — so the operator's own statement about their
// cluster is the thing that gets ignored, which is the failure this whole
// field exists to let them avoid.
//
// Every named group rather than the live ones, because a pool scaled to zero
// is still published and refusing over one would take binpack down for a pool
// that is merely empty. Skipped entirely without a live autoscaler, for the
// reason [checkNodeGroupLabel] gives: a status document nothing is updating
// says nothing about the cluster now.
func checkStatedJoin(s Snapshot, cfg Config) error {
	if len(cfg.NodeGroups) == 0 {
		return nil
	}
	if live, _, _ := s.Autoscaler.Live(s.Now); !live {
		return nil
	}

	var published []string
	known := map[string]bool{}
	for _, g := range s.Autoscaler.Groups {
		if g.ID != "" {
			published = append(published, g.ID)
			known[g.ID] = true
		}
	}
	// Nothing published, so there is nothing to check against and no evidence
	// either way — the same narrowness every other refusal here is held to.
	if len(published) == 0 {
		return nil
	}

	var unknown []string
	for value, id := range cfg.NodeGroups {
		if !known[id] {
			unknown = append(unknown, fmt.Sprintf("%s=%s is stated as %s", cfg.NodeGroupIDLabel,
				value, id))
		}
	}
	if len(unknown) == 0 {
		return nil
	}

	// Sorted, on both sides: the entries come from a map and the groups from
	// the status document's own order, and an error that reorders itself
	// between runs is one nobody can diff against the last one.
	sort.Strings(unknown)
	sort.Strings(published)
	return fmt.Errorf(
		"discovery.nodeGroups states a join to a node group the cluster-autoscaler does not\n"+
			"publish, so the nodes it names would stay outside every pool:\n"+
			"  %s\n"+
			"  the autoscaler publishes: %s\n"+
			"node groups are discovered, not declared, so a stated join must name one that is\n"+
			"there; check for a typo, or remove the entry if the pool is gone",
		strings.Join(unknown, "\n  "), strings.Join(published, ", "))
}

// ResolvePools is the preflight every frontend runs before it decides
// anything: it establishes how nodes join to pools, and rejects a
// configuration binpack will not be able to resolve.
//
// It returns the Config to use. That is why it is not called CheckPools any
// more: the join is derived per snapshot rather than configured, so a caller
// that discarded the result would be deciding against a different cluster
// from the one it validated. Discarding it costs the derivation and never
// produces a wrong mapping — [Config.GroupOf] falls back to equality — but it
// silently narrows scope, which is the failure ADR-0012 exists to prevent.
//
// Three ways the configuration fails, and the order is load-bearing: the
// mapping first, because everything else is downstream of there being any
// pool at all; then the join an operator stated, because a pool override
// naming a pool that only that join would have produced should be reported as
// the join's problem rather than the override's; then the overrides. See
// [checkNodeGroupLabel], [checkStatedJoin], and below.
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
func ResolvePools(s Snapshot, cfg Config) (Config, error) {
	cfg.Mapping = resolveMapping(s, cfg)

	if err := checkNodeGroupLabel(s, cfg); err != nil {
		return cfg, err
	}
	if err := checkStatedJoin(s, cfg); err != nil {
		return cfg, err
	}
	if len(cfg.ByPool) == 0 {
		return cfg, nil
	}

	known := map[string]bool{}
	for _, g := range s.Autoscaler.Groups {
		known[g.ID] = true
	}
	for _, node := range s.Nodes {
		if name := node.Labels[cfg.PoolNameLabel]; name != "" {
			known[name] = true
		}
		// Both the label the join reads and the configured one, because they
		// differ once a join is derived and an operator may reasonably have
		// written either.
		for _, key := range []string{cfg.mappingKey(), cfg.NodeGroupIDLabel} {
			if id := node.Labels[key]; id != "" {
				known[id] = true
			}
		}
	}

	var unknown []string
	for name := range cfg.ByPool {
		if !known[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return cfg, nil
	}

	// Sorted: the names come from a map, and an error that reorders itself
	// between runs is one nobody can diff or match against a log.
	sort.Strings(unknown)
	return cfg, fmt.Errorf(
		"configuration overrides pools that do not exist in this cluster: %s\n"+
			"pools are discovered, not declared, so an override must name one that is there;\n"+
			"check for a typo, or remove the entry if the pool is gone",
		strings.Join(unknown, ", "))
}
