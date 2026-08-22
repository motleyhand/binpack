package engine_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/motleyhand/binpack/internal/engine"
	"github.com/motleyhand/binpack/internal/mother"
)

// This file is about one question: which autoscaling pool is a node in?
//
// binpack answers it from a node label, and ADR-0012 settled that the answer
// is the label's *value*, compared for equality against the identifier the
// cluster-autoscaler publishes. ADR-0013 amends that: where nothing matches
// outright, binpack may derive the join instead, because every major provider
// generates the identifier *from* the pool name and so contains it. Deriving
// is only safe if it resolves the whole cluster or nothing, which is what
// almost every test below is checking.

// pool is one autoscaling pool as a test states it: the identifier the
// autoscaler publishes, and the label value the provider puts on its nodes.
type pool struct {
	id    string
	value string
	nodes int
	// ready and target default to nodes when zero, so a case says only the
	// thing it is about.
	ready  int
	target int
}

// derivable builds a cluster of the given pools, labelled with key, plus any
// extra nodes. Nothing about it matches the configured label, so the exact
// join finds nothing and the derivation is what has to answer.
func derivable(key string, pools []pool, extra ...*corev1.Node) engine.Snapshot {
	s := engine.Snapshot{Now: now, Autoscaler: engine.Autoscaler{
		Running: true, LastProbe: now.Add(-10 * time.Second)}}

	for _, p := range pools {
		for i := range p.nodes {
			s.Nodes = append(s.Nodes, mother.Node(fmt.Sprintf("%s-%d", p.value, i),
				mother.NodeLabels(map[string]string{key: p.value, "kubernetes.io/os": "linux"})))
		}
		ready, target := p.ready, p.target
		if ready == 0 {
			ready = p.nodes
		}
		if target == 0 {
			target = ready
		}
		s.Autoscaler.Groups = append(s.Autoscaler.Groups, engine.NodeGroup{
			ID: p.id, MinSize: 1, MaxSize: 10, Ready: ready, Target: target, HasTarget: true})
	}
	s.Nodes = append(s.Nodes, extra...)
	return s
}

// TestTheJoinIsDerivedFromTheLabelWhoseValuesNameThePools is the case for
// ADR-0013 existing: on EKS, GKE and AKS no label carries the identifier, and
// every one of them carries a value the identifier was built from.
//
// The identifiers are the real shapes, not invented ones — see ADR-0013 for
// where each was read from.
func TestTheJoinIsDerivedFromTheLabelWhoseValuesNameThePools(t *testing.T) {
	for _, tc := range []struct {
		name  string
		key   string
		pools []pool
	}{
		{
			// AWS publishes the Auto Scaling group's name, and EKS generates
			// that from the managed node group's name.
			name: "EKS managed node groups",
			key:  "eks.amazonaws.com/nodegroup",
			pools: []pool{
				{id: "eks-workers-a2c1d3e4-1111-2222-3333", value: "workers", nodes: 3},
				{id: "eks-system-b7f10c2d-4444-5555-6666", value: "system", nodes: 2},
			},
		},
		{
			// GCE publishes the managed instance group's full API URL, which
			// no label value could ever equal — but the pool name is in it.
			name: "GKE, where the identifier is a URL and equality is impossible",
			key:  "cloud.google.com/gke-nodepool",
			pools: []pool{
				{id: "https://www.googleapis.com/compute/v1/projects/p/zones/z/" +
					"instanceGroups/test-cluster-default-pool-a0c72690-grp",
					value: "default-pool", nodes: 3},
				{id: "https://www.googleapis.com/compute/v1/projects/p/zones/z/" +
					"instanceGroups/test-cluster-batch-pool-def45611-grp",
					value: "batch-pool", nodes: 2},
			},
		},
		{
			// Azure publishes the VM Scale Set's name, built from the agent
			// pool's.
			name: "AKS agent pools",
			key:  "kubernetes.azure.com/agentpool",
			pools: []pool{
				{id: "aks-nodepool1-33555069-vmss", value: "nodepool1", nodes: 3},
				{id: "aks-spotpool-87654321-vmss", value: "spotpool", nodes: 2},
			},
		},
		{
			// The one cluster any of this was seen on. eksctl builds an Auto
			// Scaling group through CloudFormation and the generated name
			// carries the node group's name twice.
			name: "eksctl self-managed node groups",
			key:  "alpha.eksctl.io/nodegroup-name",
			pools: []pool{
				{id: "eksctl-testing-nodegroup-testing-24-NodeGroup-iOzTWO7QftDh",
					value: "testing-24", nodes: 4},
			},
		},
		{
			// "web" sits inside both identifiers, so a rule demanding that
			// each value name exactly one pool refuses this outright. A
			// matching resolves it: "web-2" can only go one place, which
			// forces "web" to the other. Equal sizes, so nothing else can.
			name: "two pools whose names are prefixes of each other",
			key:  "eks.amazonaws.com/nodegroup",
			pools: []pool{
				{id: "eks-web-1111-aaaa", value: "web", nodes: 2},
				{id: "eks-web-2-2222-bbbb", value: "web-2", nodes: 2},
			},
		},
		{
			// Mid scale-up: the node has registered and the autoscaler has
			// not counted it ready yet. Demanding the partition equal `ready`
			// would make binpack refuse on every scale event.
			name: "a pool the cluster is still scaling up",
			key:  "eks.amazonaws.com/nodegroup",
			pools: []pool{
				{id: "eks-workers-a2c1d3e4-1111", value: "workers", nodes: 4, ready: 3, target: 4},
			},
		},
		{
			// And the other direction: the autoscaler has lowered its target
			// and the node is still there.
			name: "a pool the cluster is still scaling down",
			key:  "eks.amazonaws.com/nodegroup",
			pools: []pool{
				{id: "eks-workers-a2c1d3e4-1111", value: "workers", nodes: 4, ready: 4, target: 3},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := derivable(tc.key, tc.pools)

			cfg, err := engine.ResolvePools(s, config())
			if err != nil {
				t.Fatalf("preflight refused a cluster whose pools are derivable:\n%v", err)
			}
			if got := cfg.Mapping.Source; got != engine.MappedByName {
				t.Fatalf("mapping source is %v, want %v", got, engine.MappedByName)
			}
			if got := cfg.Mapping.Key; got != tc.key {
				t.Errorf("derived the join from %q, want %q", got, tc.key)
			}
			for _, p := range tc.pools {
				for _, node := range s.Nodes {
					if node.Labels[tc.key] != p.value {
						continue
					}
					if got := cfg.GroupOf(node); got != p.id {
						t.Errorf("%s is in group %q, want %q", node.Name, got, p.id)
					}
				}
			}
		})
	}
}

// TestNothingIsDerivedUnlessTheWholeClusterResolves is the guard that makes
// substring matching safe rather than reckless.
//
// A substring on its own would join "linux" or "amd64" to anything they
// happen to sit inside. What rules that out is not the substring but the
// bijection around it: the label's values have to correspond one-to-one with
// the pools the autoscaler publishes, at plausible sizes, in exactly one way.
// Every row here breaks one of those and none of them may resolve — a derived
// mapping that is wrong applies one pool's floor to another pool's nodes.
func TestNothingIsDerivedUnlessTheWholeClusterResolves(t *testing.T) {
	for _, tc := range []struct {
		name string
		// why is the phrase the refusal has to carry, so that an operator
		// learns which of these they hit.
		why   string
		build func() engine.Snapshot
	}{
		{
			// The trap this whole design has to survive: a label every node
			// carries, whose value sits inside the one published identifier.
			// Two of the five nodes are static, and binpack considering a
			// static node drainable is the failure that costs a cluster.
			// Only the size window catches this.
			name: "a label the pool's nodes share with nodes outside it",
			why:  "node",
			build: func() engine.Snapshot {
				s := derivable("kubernetes.io/os", []pool{
					{id: "eks-linux-a2c1d3e4", value: "linux", nodes: 3}})
				s.Nodes = append(s.Nodes,
					mother.Node("static-0", mother.NodeLabels(map[string]string{
						"kubernetes.io/os": "linux"})),
					mother.Node("static-1", mother.NodeLabels(map[string]string{
						"kubernetes.io/os": "linux"})))
				return s
			},
		},
		{
			// One pool's nodes carry the key and another's do not, so the
			// derived scope would silently be half the cluster.
			name: "a label only some of the pools' nodes carry",
			why:  "one-to-one",
			build: func() engine.Snapshot {
				s := derivable("eks.amazonaws.com/nodegroup", []pool{
					{id: "eks-workers-a2c1d3e4", value: "workers", nodes: 3}})
				s.Autoscaler.Groups = append(s.Autoscaler.Groups, engine.NodeGroup{
					ID: "eks-system-b7f10c2d", MinSize: 1, MaxSize: 10, Ready: 2})
				s.Nodes = append(s.Nodes,
					mother.Node("sys-0"), mother.Node("sys-1"))
				return s
			},
		},
		{
			// Two values, two pools, and each value sits inside both
			// identifiers: there are two perfect matchings and binpack has no
			// way to tell which is the real one. Refusing is the only honest
			// answer, and picking either would be a coin toss reported as a
			// fact.
			name: "values that can be matched onto the pools more than one way",
			why:  "more than one way",
			build: func() engine.Snapshot {
				return derivable("eks.amazonaws.com/nodegroup", []pool{
					{id: "eks-alpha-beta-1111", value: "alpha", nodes: 2},
					{id: "eks-beta-alpha-2222", value: "beta", nodes: 2},
				})
			},
		},
		{
			// Two keys that each resolve the cluster and put a node in
			// different pools. One of them is wrong and nothing here says
			// which.
			name: "two keys that resolve the cluster differently",
			why:  "disagree",
			build: func() engine.Snapshot {
				s := derivable("eks.amazonaws.com/nodegroup", []pool{
					{id: "eks-web-1111", value: "web", nodes: 1},
					{id: "eks-api-2222", value: "api", nodes: 1},
				})
				// A second key partitioning the same two nodes the other way
				// round. Both induce a bijection; they disagree about both
				// nodes.
				s.Nodes[0].Labels["example.com/tier"] = "api"
				s.Nodes[1].Labels["example.com/tier"] = "web"
				return s
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := tc.build()

			cfg, err := engine.ResolvePools(s, config())
			if err == nil {
				t.Fatalf("preflight accepted a cluster binpack cannot map, having derived "+
					"%s=%v", cfg.Mapping.Key, cfg.Mapping.Groups)
			}
			if cfg.Mapping.Source != engine.MappedNothing {
				t.Errorf("mapping source is %v, want %v", cfg.Mapping.Source, engine.MappedNothing)
			}
			if !strings.Contains(err.Error(), tc.why) {
				t.Errorf("the refusal does not say why the near miss was rejected (%q):\n%v",
					tc.why, err)
			}
		})
	}
}

// TestTheRefusalNamesTheKeyItCameClosestOn is the hint half of this change,
// and it is what an operator gets when the derivation declines.
//
// The message #70 introduced lists the groups the autoscaler published and
// the keys the nodes carry, and leaves the reader to spot that one is inside
// the other. Naming the near miss turns that into a one-command fix.
func TestTheRefusalNamesTheKeyItCameClosestOn(t *testing.T) {
	// A cluster one node short of resolving: three nodes carry the key, the
	// pool reports two, and there is no second pool to absorb the third.
	s := derivable("alpha.eksctl.io/nodegroup-name", []pool{
		{id: "eksctl-testing-nodegroup-testing-24-NodeGroup-iOzTWO7QftDh",
			value: "testing-24", nodes: 3, ready: 2, target: 2}})

	_, err := engine.ResolvePools(s, config())
	if err == nil {
		t.Fatal("preflight accepted a cluster whose only candidate key has the wrong size")
	}

	for what, want := range map[string]string{
		"the key that came closest":     "alpha.eksctl.io/nodegroup-name",
		"the value it holds":            "testing-24",
		"the identifier it sits inside": "eksctl-testing-nodegroup-testing-24-NodeGroup-iOzTWO7QftDh",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %s (%q):\n%v", what, want, err)
		}
	}
}

// TestAValueThatIsAlreadyAnIdentifierIsUsedBeforeAnythingIsDerived keeps the
// clusters that work today working exactly as they did.
//
// Deriving is a fallback, never a preference. On DOKS — and on any cluster
// whose operator applied the label ADR-0012 asks for — the configured label's
// value *is* the identifier, and binpack must join on that rather than go
// looking for a key it likes the shape of better.
func TestAValueThatIsAlreadyAnIdentifierIsUsedBeforeAnythingIsDerived(t *testing.T) {
	// Both joins are available: the configured label holds the identifier
	// outright, and a provider label holds a value that sits inside it.
	s := engine.Snapshot{Now: now, Autoscaler: engine.Autoscaler{
		Running: true, LastProbe: now.Add(-10 * time.Second),
		Groups: []engine.NodeGroup{{ID: "eks-workers-a2c1", MinSize: 1, MaxSize: 10, Ready: 2}}}}
	for i := range 2 {
		s.Nodes = append(s.Nodes, mother.Node(fmt.Sprintf("ip-10-0-1-1%d", i),
			mother.NodeLabels(map[string]string{
				"doks.digitalocean.com/node-pool-id": "eks-workers-a2c1",
				"eks.amazonaws.com/nodegroup":        "workers",
			})))
	}

	cfg, err := engine.ResolvePools(s, config())
	if err != nil {
		t.Fatalf("preflight refused a cluster whose configured label already matches:\n%v", err)
	}
	if got := cfg.Mapping.Source; got != engine.MappedByValue {
		t.Fatalf("mapping source is %v, want %v", got, engine.MappedByValue)
	}
	if got := cfg.Mapping.Key; got != cfg.NodeGroupIDLabel {
		t.Errorf("the join reads %q, want the configured %q", got, cfg.NodeGroupIDLabel)
	}
	if cfg.Mapping.Groups != nil {
		t.Errorf("an exact join translates nothing, and this one carries %v", cfg.Mapping.Groups)
	}
}

// TestTheDerivedKeyDoesNotDependOnTheOrderTheClusterWasRead is the property a
// heuristic deciding scope has to have before it can be reported at all.
//
// Two keys resolve the real cluster this came from — the eksctl cluster name
// and the eksctl node group name — and with one pool they agree, so either is
// correct. Which one binpack *says* it used must not vary, or an operator
// diffing two runs sees a change of scope that never happened.
//
// collect.Snapshot appends nodes in whatever order the API server listed them
// and sorts nothing, so re-running one snapshot proves only that the function
// is pure. The order has to be the thing that varies.
func TestTheDerivedKeyDoesNotDependOnTheOrderTheClusterWasRead(t *testing.T) {
	build := func() engine.Snapshot {
		s := derivable("alpha.eksctl.io/nodegroup-name", []pool{
			{id: "eksctl-testing-nodegroup-testing-24-NodeGroup-iOzTWO7QftDh",
				value: "testing-24", nodes: 4}})
		for _, node := range s.Nodes {
			node.Labels["alpha.eksctl.io/cluster-name"] = "testing"
		}
		return s
	}

	first, err := engine.ResolvePools(build(), config())
	if err != nil {
		t.Fatalf("preflight refused the cluster this came from:\n%v", err)
	}
	if len(first.Mapping.AlsoAgreed) == 0 {
		t.Fatal("only one key resolves this cluster, so it cannot show that the choice " +
			"between several is stable")
	}

	for i := range 4 {
		s := build()
		slices.Reverse(s.Nodes)
		s.Nodes = append(s.Nodes[i:], s.Nodes[:i]...)

		got, err := engine.ResolvePools(s, config())
		if err != nil {
			t.Fatalf("rotation %d refused the same cluster:\n%v", i, err)
		}
		if got.Mapping.Key != first.Mapping.Key {
			t.Errorf("rotation %d derived the join from %q, but the same cluster read in "+
				"another order gave %q", i, got.Mapping.Key, first.Mapping.Key)
		}
		if !slices.Equal(got.Mapping.AlsoAgreed, first.Mapping.AlsoAgreed) {
			t.Errorf("rotation %d reported %v as also agreeing, want %v",
				i, got.Mapping.AlsoAgreed, first.Mapping.AlsoAgreed)
		}
	}
}

// TestAConfiguredJoinIsTakenBeforeAnythingIsDerived is the escape hatch, and
// it is the answer wherever the identifier is not built from the pool name at
// all: a self-managed Auto Scaling group named by hand, or a GKE pool whose
// name the instance group truncated.
//
// It states membership and nothing else. Pool bounds still come from the
// status ConfigMap, because a minimum stated in binpack's configuration and
// enforced by the autoscaler is two numbers that can disagree — which is what
// ADR-0004's withdrawn second mode got wrong.
func TestAConfiguredJoinIsTakenBeforeAnythingIsDerived(t *testing.T) {
	s := derivable("eks.amazonaws.com/nodegroup", []pool{
		{id: "eks-workers-a2c1d3e4", value: "workers", nodes: 2},
	})
	// Nodes carry the configured label too, holding something the generated
	// identifier does not contain, so nothing could be derived from it.
	for _, node := range s.Nodes {
		node.Labels["doks.digitalocean.com/node-pool-id"] = "hand-written"
	}

	cfg := config()
	cfg.NodeGroups = map[string]string{"hand-written": "eks-workers-a2c1d3e4"}

	resolved, err := engine.ResolvePools(s, cfg)
	if err != nil {
		t.Fatalf("preflight refused a cluster whose join the configuration states:\n%v", err)
	}
	if got := resolved.Mapping.Source; got != engine.MappedByConfiguration {
		t.Fatalf("mapping source is %v, want %v", got, engine.MappedByConfiguration)
	}
	for _, node := range s.Nodes {
		if got := resolved.GroupOf(node); got != "eks-workers-a2c1d3e4" {
			t.Errorf("%s is in group %q, want the one the configuration names", node.Name, got)
		}
	}
}

// TestAConfiguredJoinLeavesEveryOtherValueMatchingOutright keeps the override
// additive.
//
// An operator naming one pool has not said anything about the others, and a
// configuration that silently took the rest out of scope would be the same
// silent narrowing this whole change exists to prevent.
func TestAConfiguredJoinLeavesEveryOtherValueMatchingOutright(t *testing.T) {
	s := engine.Snapshot{Now: now, Autoscaler: engine.Autoscaler{
		Running: true, LastProbe: now.Add(-10 * time.Second),
		Groups: []engine.NodeGroup{
			{ID: "eks-web-1111", MinSize: 1, MaxSize: 10, Ready: 1},
			{ID: "eks-api-2222", MinSize: 1, MaxSize: 10, Ready: 1},
		}}}
	s.Nodes = append(s.Nodes,
		mother.Node("web-0", mother.NodeLabels(map[string]string{
			"doks.digitalocean.com/node-pool-id": "web"})),
		mother.Node("api-0", mother.NodeLabels(map[string]string{
			"doks.digitalocean.com/node-pool-id": "eks-api-2222"})))

	cfg := config()
	cfg.NodeGroups = map[string]string{"web": "eks-web-1111"}

	resolved, err := engine.ResolvePools(s, cfg)
	if err != nil {
		t.Fatalf("preflight refused:\n%v", err)
	}
	if got := resolved.GroupOf(s.Nodes[0]); got != "eks-web-1111" {
		t.Errorf("the node the configuration names is in group %q", got)
	}
	if got := resolved.GroupOf(s.Nodes[1]); got != "eks-api-2222" {
		t.Errorf("the node the configuration says nothing about is in group %q, so stating "+
			"one pool took the other out of scope", got)
	}
}

// TestAPoolWithNoReadyNodeIsStillNoEvidenceEitherWay preserves the narrowness
// ADR-0012 chose. Deriving must not widen what binpack is willing to refuse
// over: with nothing published that has a node in it, there is nothing to
// match against and no question to answer.
func TestAPoolWithNoReadyNodeIsStillNoEvidenceEitherWay(t *testing.T) {
	s := derivable("eks.amazonaws.com/nodegroup", []pool{
		{id: "eks-workers-a2c1d3e4", value: "workers", nodes: 2}})
	s.Autoscaler.Groups[0].Ready, s.Autoscaler.Groups[0].Target = 0, 0

	cfg, err := engine.ResolvePools(s, config())
	if err != nil {
		t.Fatalf("preflight refused a cluster it has no evidence about:\n%v", err)
	}
	if got := cfg.Mapping.Source; got != engine.MappedNothing {
		t.Errorf("mapping source is %v, want %v", got, engine.MappedNothing)
	}
}

// TestADerivedJoinPutsTheNodesInTheirPool is the whole point, asserted
// through the decision rather than through the mapping: a cluster that
// reported every node as "not part of an autoscaling pool" is now assessed.
func TestADerivedJoinPutsTheNodesInTheirPool(t *testing.T) {
	s := derivable("eks.amazonaws.com/nodegroup", []pool{
		{id: "eks-workers-a2c1d3e4", value: "workers", nodes: 3}})

	cfg, err := engine.ResolvePools(s, config())
	if err != nil {
		t.Fatalf("preflight refused:\n%v", err)
	}
	for _, a := range engine.Assess(s, cfg) {
		if a.SkipCode == engine.SkipNotAutoscaled {
			t.Errorf("%s is still outside every pool after the join was derived", a.Node.Name)
		}
		if a.Group != "eks-workers-a2c1d3e4" {
			t.Errorf("%s was assessed in group %q", a.Node.Name, a.Group)
		}
	}
}

// TestAPoolThatReportsNoTargetIsSizedByWhatIsReady closes the half of the
// size window that has no second number to fall back on.
//
// cloudProviderTarget is a pointer upstream and NodeGroup.HasTarget
// distinguishes an absent one from a reported zero. Treating an absent target
// as zero widens the window to [0, ready] — which accepts a label carried by
// one node of a three-node pool, and that is over-capture again, arriving
// through arithmetic rather than through a substring.
func TestAPoolThatReportsNoTargetIsSizedByWhatIsReady(t *testing.T) {
	// One value, one pool, so the partition count matches and the size window
	// is the only thing left to refuse on: the pool reports three ready nodes
	// and two of them carry the label.
	s := derivable("eks.amazonaws.com/nodegroup", []pool{
		{id: "eks-workers-a2c1d3e4", value: "workers", nodes: 2, ready: 3}},
		mother.Node("unlabelled"))
	s.Autoscaler.Groups[0].Target, s.Autoscaler.Groups[0].HasTarget = 0, false

	if _, err := engine.ResolvePools(s, config()); err == nil {
		t.Fatal("a label two of a pool's three nodes carry was accepted as naming it")
	}
}

// TestTheKeyReportedIsTheFirstInSortedOrder pins the tie-break, because
// several keys resolving a cluster identically is the ordinary case rather
// than the exotic one and something has to choose between them.
//
// Sorted, and the rest reported as agreeing. Any stable rule would be
// deterministic; this one is also independent of how many labels the nodes
// happen to carry and of what the API server returned first, which a rule
// like "the most specific-looking key" would not be.
func TestTheKeyReportedIsTheFirstInSortedOrder(t *testing.T) {
	s := derivable("alpha.eksctl.io/nodegroup-name", []pool{
		{id: "eksctl-testing-nodegroup-testing-24-NodeGroup-iOzTWO7QftDh",
			value: "testing-24", nodes: 4}})
	for _, node := range s.Nodes {
		node.Labels["alpha.eksctl.io/cluster-name"] = "testing"
	}

	cfg, err := engine.ResolvePools(s, config())
	if err != nil {
		t.Fatalf("preflight refused:\n%v", err)
	}
	if got, want := cfg.Mapping.Key, "alpha.eksctl.io/cluster-name"; got != want {
		t.Errorf("reported %q as the key it matched on, want the first in sorted order, %q",
			got, want)
	}
	if got, want := cfg.Mapping.AlsoAgreed,
		[]string{"alpha.eksctl.io/nodegroup-name"}; !slices.Equal(got, want) {
		t.Errorf("reported %v as also agreeing, want %v", got, want)
	}
}

// TestTheRefusalDoesNotDependOnTheOrderTheStatusListedPools is the other half
// of the determinism property, and the one a fixture with a single pool
// cannot show.
//
// collect.ParseAutoscalerStatus appends groups in the order the document
// listed them, which is the autoscaler's. A refusal that named a different
// pool on each run is one nobody can diff against the last one, and #69's
// review established that re-running a single snapshot tests purity rather
// than determinism — the order has to be the thing that varies.
func TestTheRefusalDoesNotDependOnTheOrderTheStatusListedPools(t *testing.T) {
	build := func() engine.Snapshot {
		// Both identifiers contain both values and neither pool is the size
		// either value is carried by, so the sentence naming "the pool it
		// names" has two pools to choose from and iteration order is all that
		// separates them.
		return derivable("eks.amazonaws.com/nodegroup", []pool{
			{id: "eks-alpha-beta-1111", value: "alpha", nodes: 3, ready: 2},
			{id: "eks-beta-alpha-2222", value: "beta", nodes: 1, ready: 2},
		})
	}

	_, first := engine.ResolvePools(build(), config())
	if first == nil {
		t.Fatal("a cluster with two possible matchings was accepted")
	}
	for i := range 4 {
		s := build()
		slices.Reverse(s.Autoscaler.Groups)
		slices.Reverse(s.Nodes)
		s.Nodes = append(s.Nodes[i:], s.Nodes[:i]...)

		_, got := engine.ResolvePools(s, config())
		if got == nil {
			t.Fatalf("permutation %d accepted the same cluster", i)
		}
		if got.Error() != first.Error() {
			t.Errorf("permutation %d words the refusal differently:\n %v\n %v", i, first, got)
		}
	}
}

// TestAPoolOverrideMayNameTheLabelValueUnderADerivedJoin is the one place a
// derived join changes what an operator has to write, and the failure is
// silent in the dangerous direction.
//
// With equality, the value of discovery.nodeGroupIDLabel *is* the identifier,
// so `pools[].name` matched either way. Derived, they are two different
// strings — `workers` and `eks-workers-…` — and an operator writes the one
// they can see on their nodes. A policy that did not match would leave a pool
// somebody believes they switched off still being considered for a drain.
func TestAPoolOverrideMayNameTheLabelValueUnderADerivedJoin(t *testing.T) {
	s := derivable("eks.amazonaws.com/nodegroup", []pool{
		{id: "eks-workers-a2c1d3e4", value: "workers", nodes: 2}})

	for _, name := range []string{"workers", "eks-workers-a2c1d3e4"} {
		t.Run(name, func(t *testing.T) {
			cfg := config()
			cfg.ByPool = map[string]engine.Policy{name: {Enabled: false}}

			resolved, err := engine.ResolvePools(s, cfg)
			if err != nil {
				t.Fatalf("preflight rejected an override naming a pool that is there:\n%v", err)
			}
			for _, node := range s.Nodes {
				if resolved.PolicyForNode(node).Enabled {
					t.Errorf("%s took the default policy, so an override written as %q "+
						"did nothing", node.Name, name)
				}
			}
		})
	}
}

// TestWhichHalfOfAStatedJoinIsReportedDoesNotDependOnNodeOrder closes the one
// place the join's *account* could vary while the join itself did not.
//
// A configuration may state one pool and leave another matching outright. One
// node matching is enough to settle that the join works, so the first
// implementation stopped there — and which node that was decided whether
// binpack said the join came from the configuration or from the label's value.
// collect.Snapshot appends nodes in the order the API server listed them.
func TestWhichHalfOfAStatedJoinIsReportedDoesNotDependOnNodeOrder(t *testing.T) {
	build := func() engine.Snapshot {
		s := engine.Snapshot{Now: now, Autoscaler: engine.Autoscaler{
			Running: true, LastProbe: now.Add(-10 * time.Second),
			Groups: []engine.NodeGroup{
				{ID: "eks-web-1111", MinSize: 1, MaxSize: 10, Ready: 1},
				{ID: "eks-api-2222", MinSize: 1, MaxSize: 10, Ready: 1},
			}}}
		s.Nodes = append(s.Nodes,
			// Joined by the configuration below.
			mother.Node("web-0", mother.NodeLabels(map[string]string{
				"doks.digitalocean.com/node-pool-id": "web"})),
			// Joined by carrying the identifier outright.
			mother.Node("api-0", mother.NodeLabels(map[string]string{
				"doks.digitalocean.com/node-pool-id": "eks-api-2222"})))
		return s
	}
	cfg := config()
	cfg.NodeGroups = map[string]string{"web": "eks-web-1111"}

	first, err := engine.ResolvePools(build(), cfg)
	if err != nil {
		t.Fatalf("preflight refused:\n%v", err)
	}
	if got := first.Mapping.Source; got != engine.MappedByConfiguration {
		t.Fatalf("mapping source is %v, want %v", got, engine.MappedByConfiguration)
	}

	s := build()
	slices.Reverse(s.Nodes)
	reversed, err := engine.ResolvePools(s, cfg)
	if err != nil {
		t.Fatalf("preflight refused the same cluster read backwards:\n%v", err)
	}
	if got := reversed.Mapping.Source; got != first.Mapping.Source {
		t.Errorf("the same cluster read backwards reports the join as %v, not %v",
			got, first.Mapping.Source)
	}
}

// TestAStatedJoinMustNameAPoolTheAutoscalerPublishes gives the escape hatch
// the same guard the pool overrides have had since ADR-0012.
//
// `pools` entries are checked because a misspelt name installs an unreachable
// map entry and its nodes quietly take the default policy. A misspelt
// `discovery.nodeGroups` entry is worse: it says nothing, the nodes it meant
// to place stay outside every pool, and binpack may then derive a mapping
// instead — so the operator's own statement about their cluster is the thing
// that gets ignored.
func TestAStatedJoinMustNameAPoolTheAutoscalerPublishes(t *testing.T) {
	s := derivable("eks.amazonaws.com/nodegroup", []pool{
		{id: "eks-workers-a2c1d3e4", value: "workers", nodes: 2}})

	cfg := config()
	cfg.NodeGroups = map[string]string{"workers": "eks-worker-a2c1d3e4"}

	_, err := engine.ResolvePools(s, cfg)
	if err == nil {
		t.Fatal("a stated join naming a pool that is not there was accepted")
	}
	for what, want := range map[string]string{
		"the group it named":      "eks-worker-a2c1d3e4",
		"the setting it is under": "discovery.nodeGroups",
		"a pool that does exist":  "eks-workers-a2c1d3e4",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s (%q):\n%v", what, want, err)
		}
	}
}

// TestAStatedJoinMayNameAPoolScaledToZero keeps that check as narrow as every
// other one here.
//
// A pool with no nodes still appears in the status document, and refusing
// over one would take binpack down for a pool that is merely empty — which is
// the false positive TestResolvePoolsAcceptsAPoolKnownOnlyToTheAutoscaler
// exists to prevent for the overrides.
func TestAStatedJoinMayNameAPoolScaledToZero(t *testing.T) {
	s := derivable("eks.amazonaws.com/nodegroup", []pool{
		{id: "eks-workers-a2c1d3e4", value: "workers", nodes: 2}})
	s.Autoscaler.Groups = append(s.Autoscaler.Groups,
		engine.NodeGroup{ID: "eks-batch-b7f1", MinSize: 0, MaxSize: 10, Ready: 0})

	cfg := config()
	cfg.NodeGroups = map[string]string{"batch": "eks-batch-b7f1"}

	if _, err := engine.ResolvePools(s, cfg); err != nil {
		t.Errorf("a stated join naming an empty but published pool was rejected:\n%v", err)
	}
}
