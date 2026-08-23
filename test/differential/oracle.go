// Package differential checks binpack's fit predicate against the real
// Kubernetes scheduler.
//
// binpack claims one-directional soundness: if it says a pod fits a node, the
// scheduler agrees. Every defect found while designing the predicate was a
// case where that claim held only because nobody had thought of the
// counterexample yet — which is a poor way to establish a property. This
// package establishes it by asking the scheduler.
//
// # Why this is a separate module
//
// The oracle runs the scheduler's own Filter plugins, which means depending on
// k8s.io/kubernetes. That module pins its staging dependencies at v0.0.0 and
// relies on replace directives which are not propagated to consumers, so every
// consumer must restate them — cluster-autoscaler carries about twenty-five
// such lines for exactly this reason.
//
// binpack does not want that in the module it ships. Karpenter and the
// descheduler both avoid it, building on staging helpers instead, and ADR-0006
// chose the same. Keeping the oracle in its own module means the heavy
// dependency and its replace block exist only where they are paid for: in a
// module nothing ships, whose pin can lag a Kubernetes release without
// affecting anything but the fidelity of a test.
//
// A restated replace block is also a way to lag one *silently*, since a
// replace overrides version selection unconditionally and `go mod tidy` has no
// opinion about it. TestHarnessTracksTheShippedLibraries asserts every k8s.io
// module the main module requires directly resolves to the same version here,
// so the oracle cannot end up judging a copy of internal/fit compiled against
// a library binpack does not ship.
//
// # What is compared, and what is not
//
// Only the predicates binpack actually models. Everything else it refuses
// outright, and a refusal is sound by definition — so an unmodelled constraint
// needs no oracle, only the assurance that binpack keeps refusing it.
//
// That assurance used to be this paragraph and nothing else, which is not an
// assurance. The plugin list was four literal constructor calls, so a release
// that added a Filter plugin widened what binpack was blind to without
// widening what the harness looked at, and every generated case still passed.
// TestOracleCoversEveryDefaultFilterPlugin now derives the release's default
// Filter set and fails on any member the oracle neither runs nor exempts —
// and an exemption has to name the internal/fit refusal that keeps binpack
// away from the plugin's input, and that refusal is run.
//
// Where a refusal is narrower than the plugin it stands in for, the exemption
// also carries the input that falls through it, asserted to still fall
// through. So the gap is a defect on the record rather than a sentence, and
// closing it in internal/fit fails that test until the record is deleted.
//
// # Why the oracle is asked about a cluster rather than a node
//
// [Oracle.Accepts] takes every node in the candidate cluster and everything
// running on each of them, because two of the default Filter plugins decide
// from state no single node holds. InterPodAffinity counts pods matching a
// required anti-affinity term across the whole topology domain the term names,
// so a zone-keyed term rejects every node in the zone — including nodes
// hosting nothing that matches. PodTopologySpread is the same shape one
// constraint over.
//
// Neither plugin will even construct without a Handle carrying a snapshot of
// the cluster, so while Accepts modelled one node they were unreachable: the
// harness was structurally incapable of adjudicating the one class of
// disagreement where binpack asks a node-local question and upstream asks a
// domain-wide one. That is precisely the class ADR-0006 records as the
// hardest, and it had been left to the exemption map, whose whole warning is
// that an exemption nobody can falsify turns the allowlist into a denylist.
package differential

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apiruntime "k8s.io/apimachinery/pkg/runtime"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/latest"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/interpodaffinity"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeaffinity"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodedeclaredfeatures"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodename"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeports"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeunschedulable"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/podtopologyspread"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/tainttoleration"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	schedulermetrics "k8s.io/kubernetes/pkg/scheduler/metrics"
)

// Verdict is the scheduler's answer for one pod-node pair.
type Verdict struct {
	Accepted bool
	// Plugin names which filter rejected the placement, so a disagreement
	// points at the specific predicate rather than at "the scheduler".
	Plugin string
	// Reasons is the plugin's own list, unjoined. NodeResourcesFit returns one
	// per resource dimension that ran out, and keeping them apart is what lets
	// a vacuity guard ask whether the corpus still exercises a *dimension*
	// rather than only whether the plugin still refuses something — the
	// difference between catching an ignored extended resource and not.
	Reasons []string
	Message string
}

func (v Verdict) String() string {
	if v.Accepted {
		return "accepted"
	}
	return fmt.Sprintf("rejected by %s: %s", v.Plugin, v.Message)
}

type namedPlugin struct {
	name   string
	filter fwk.FilterPlugin
	pre    fwk.PreFilterPlugin // nil when the plugin has no PreFilter phase
}

// Oracle answers whether the real scheduler would accept a placement.
type Oracle struct {
	plugins []namedPlugin
	// cluster is what the plugins read the cluster through. They captured the
	// pointer when they were constructed, so a question is asked by replacing
	// its contents rather than the object — which is also why one Oracle
	// answers one question at a time and must not be shared across goroutines.
	cluster *clusterLister
}

// clusterLister is the fwk.SharedLister the oracle's plugins see.
//
// Everything is delegated to a scheduler Snapshot built by upstream's own
// constructor from the API objects [Oracle.Accepts] is handed, so the cluster
// the plugins read is assembled by the code under comparison rather than by
// this package. The indirection exists only because plugins capture the lister
// at construction while the cluster changes per question.
type clusterLister struct {
	snapshot *cache.Snapshot
}

func (c *clusterLister) NodeInfos() fwk.NodeInfoLister           { return c.snapshot.NodeInfos() }
func (c *clusterLister) StorageInfos() fwk.StorageInfoLister     { return c.snapshot.StorageInfos() }
func (c *clusterLister) PodGroupStates() fwk.PodGroupStateLister { return c.snapshot.PodGroupStates() }

// NewOracle builds the Filter plugins corresponding to the predicates binpack
// models: NodeUnschedulable, NodeName, TaintToleration, NodeAffinity,
// NodePorts, NodeResourcesFit, PodTopologySpread, InterPodAffinity and
// NodeDeclaredFeatures, in the order the release's default profile runs them
// — so the plugin a Verdict names is the one the scheduler would have
// reported first. TestOracleCoversEveryDefaultFilterPlugin holds that claim
// to the profile, since a Verdict naming the wrong plugin is a disagreement
// report pointing at the wrong predicate.
//
// Which plugins belong here is not a judgement this constructor is trusted to
// make alone. TestOracleCoversEveryDefaultFilterPlugin derives the default
// Filter set from the pinned release and fails on any member built neither
// here nor into its exemption map. What is left out judges volumes or dynamic
// resource claims — inputs the allowlist refuses as whole classes of pod — and
// each is exempted there against the internal/fit refusal that keeps binpack
// away from it.
//
// PodTopologySpread and InterPodAffinity are the two that decide across nodes,
// and they are here because they cost almost nothing to run and answer a
// question nothing else can. Each returns Skip from PreFilter for the
// overwhelming majority of scenarios: PodTopologySpread unless the pod carries
// a DoNotSchedule constraint, InterPodAffinity unless the pod carries required
// affinity terms of its own or some node in the snapshot holds a pod with
// required anti-affinity. binpack refuses every input either plugin could
// reject today — but a refusal that nothing adjudicates is a claim, and this is
// the difference between the harness checking that claim and reprinting it.
//
// Plugin arguments come from the release's own defaulted profile rather than
// being written out here. Hand-written args reintroduce the hand-written-list
// problem one field down, and PodTopologySpread is the demonstration: its
// behaviour turns on DefaultingType, and the System default supplies two
// constraints that are ScheduleAnyway — which is why it can only ever score,
// never reject, a pod that declares no constraints of its own. Typing that
// field is guessing at somebody else's default; reading it is not.
//
// Feature gates come from the same place the real scheduler gets them:
// NewSchedulerFeaturesFromGates over the release's DefaultFeatureGate. They
// are deliberately not hand-listed.
//
// The first run of this harness reported 171 unsound placements, every one of
// them the same message — "Pod has a restartable init container and the
// SidecarContainers feature is disabled". That was the oracle, not binpack:
// zero-valued gates model a cluster where native sidecars do not exist, while
// component-helpers computes sidecar-aware requests because real clusters have
// had that feature on by default since 1.29 — Beta and Default: true there,
// and GA with LockToDefault only at 1.33 (k8s.io/kubernetes v1.36.3
// pkg/features/kube_features.go, SidecarContainers). Four releases separate
// the two dates, and getting them the wrong way round is how a 1.29 cluster
// gets modelled as sidecar-unaware.
//
// The lesson generalises past sidecars. A hand-maintained gate list is a set
// of guesses about someone else's defaults, and a wrong guess here does not
// fail loudly — it produces confident, wrong disagreement reports. Deriving
// them means the oracle tracks the pinned Kubernetes release by construction.
//
// The gate table itself is not a reliable thing to reason from either. Its
// entry for SidecarContainers carries the comment "GA in 1.33 remove in 1.36",
// and at 1.36.3 the gate is still there and still surfaced by
// NewSchedulerFeaturesFromGates — so even a list copied faithfully from
// upstream's own annotations would have been wrong about this one.
func NewOracle() (*Oracle, error) {
	ctx := context.Background()

	// The two cross-node plugins fan their counting out through the
	// framework's Parallelizer, which adds to a goroutine gauge without
	// checking that anything built one — so an unregistered scheduler panics
	// on a nil GaugeVec rather than skipping a metric nobody reads. Register
	// is idempotent and is what the real scheduler calls.
	schedulermetrics.Register()

	features := feature.NewSchedulerFeaturesFromGates(utilfeature.DefaultFeatureGate)

	// Dynamic resource allocation is switched off, and only that.
	//
	// binpack refuses any pod carrying resource claims, so DRA filtering is
	// outside the property under test — and the plugins reach for a scheduler
	// Handle to consult a DRA manager when it is on, which this oracle does
	// not have. Narrowing here matches the documented scope: compare the
	// predicates binpack models, and trust refusal for the rest.
	features.EnableDynamicResourceAllocation = false
	features.EnableDRAExtendedResource = false

	args, err := DefaultPluginArgs()
	if err != nil {
		return nil, err
	}

	o := &Oracle{cluster: &clusterLister{snapshot: cache.NewEmptySnapshot()}}

	handle, err := newHandle(ctx, o.cluster)
	if err != nil {
		return nil, err
	}

	add := func(name string, p fwk.Plugin, err error) error {
		if err != nil {
			return fmt.Errorf("constructing %s: %w", name, err)
		}
		np := namedPlugin{name: name}
		filter, ok := p.(fwk.FilterPlugin)
		if !ok {
			return fmt.Errorf("%s is not a FilterPlugin", name)
		}
		np.filter = filter
		if pre, ok := p.(fwk.PreFilterPlugin); ok {
			np.pre = pre
		}
		o.plugins = append(o.plugins, np)
		return nil
	}

	p, err := nodeunschedulable.New(ctx, args[nodeunschedulable.Name], handle, features)
	if err := add(nodeunschedulable.Name, p, err); err != nil {
		return nil, err
	}

	// NodeName is inert for anything binpack would relocate — a pod naming a
	// node bypasses the scheduler, and fit refuses those outright — so it
	// costs nothing and makes that refusal falsifiable rather than assumed.
	p, err = nodename.New(ctx, args[nodename.Name], handle, features)
	if err := add(nodename.Name, p, err); err != nil {
		return nil, err
	}

	p, err = tainttoleration.New(ctx, args[tainttoleration.Name], handle, features)
	if err := add(tainttoleration.Name, p, err); err != nil {
		return nil, err
	}

	p, err = nodeaffinity.New(ctx, args[nodeaffinity.Name], handle, features)
	if err := add(nodeaffinity.Name, p, err); err != nil {
		return nil, err
	}

	p, err = nodeports.New(ctx, args[nodeports.Name], handle, features)
	if err := add(nodeports.Name, p, err); err != nil {
		return nil, err
	}

	p, err = noderesources.NewFit(ctx, args[noderesources.Name], handle, features)
	if err := add(noderesources.Name, p, err); err != nil {
		return nil, err
	}

	p, err = podtopologyspread.New(ctx, args[podtopologyspread.Name], handle, features)
	if err := add(podtopologyspread.Name, p, err); err != nil {
		return nil, err
	}

	p, err = interpodaffinity.New(ctx, args[interpodaffinity.Name], handle, features)
	if err := add(interpodaffinity.Name, p, err); err != nil {
		return nil, err
	}

	// Beta and on by default since 1.36, and last because applyFeatureGates
	// appends it to the default profile rather than splicing it in — so a
	// verdict from any other plugin is one the scheduler would have reached
	// first. It rejects a node whose kubelet has not declared a feature the
	// pod's spec implies: new at this release, and so the demonstration of
	// why the set is derived rather than typed.
	p, err = nodedeclaredfeatures.New(ctx, args[nodedeclaredfeatures.Name], handle, features)
	if err := add(nodedeclaredfeatures.Name, p, err); err != nil {
		return nil, err
	}

	return o, nil
}

// DefaultPluginArgs returns the release's own defaulted arguments for every
// plugin its default profile configures, keyed by plugin name.
//
// Exported because the coverage test builds the plugins the oracle does not
// run, and it has to build them with the same arguments for the same reason
// the oracle does.
func DefaultPluginArgs() (map[string]apiruntime.Object, error) {
	cfg, err := latest.Default()
	if err != nil {
		return nil, fmt.Errorf("reading the release's default scheduler configuration: %w", err)
	}
	if len(cfg.Profiles) != 1 {
		return nil, fmt.Errorf("expected exactly one default profile, got %d", len(cfg.Profiles))
	}
	args := make(map[string]apiruntime.Object, len(cfg.Profiles[0].PluginConfig))
	for _, pc := range cfg.Profiles[0].PluginConfig {
		args[pc.Name] = pc.Args
	}
	return args, nil
}

// newHandle builds the least a default Filter plugin needs to construct: a
// snapshot lister, an informer factory and a client.
//
// The factory's informers are never started, and that is a limitation worth
// naming rather than a saving. InterPodAffinity reads namespace labels through
// it to resolve a term's namespaceSelector, and an unstarted lister answers
// "not found" — so a fixture using namespaceSelector would be judged against a
// cluster where no namespace carries any label. No fixture does; one that did
// would need the factory started and its caches synced.
func newHandle(ctx context.Context, lister fwk.SharedLister) (fwk.Handle, error) {
	client := fake.NewClientset()
	h, err := frameworkruntime.NewFramework(ctx, nil, nil,
		frameworkruntime.WithSnapshotSharedLister(lister),
		frameworkruntime.WithInformerFactory(informers.NewSharedInformerFactory(client, 0)),
		frameworkruntime.WithClientSet(client),
	)
	if err != nil {
		return nil, fmt.Errorf("building a scheduler handle: %w", err)
	}
	return h, nil
}

// Accepts reports whether the scheduler would allow pod onto target, given the
// cluster it sits in.
//
// cluster is every node the placement is judged against and byNode is what is
// running on each of them, keyed by node name. Both are needed in full because
// the topology plugins count across a domain rather than a node: passing only
// target would answer a narrower question than the scheduler's, in the
// accepting direction.
//
// Capacity is derived the way the scheduler derives it — from each node's
// allocatable minus its residents' requests — rather than being supplied, so
// the oracle shares no arithmetic with the code under test.
func (o *Oracle) Accepts(
	pod *corev1.Pod,
	target *corev1.Node,
	cluster []*corev1.Node,
	byNode map[string][]*corev1.Pod,
) (Verdict, error) {
	ctx := context.Background()

	placed, err := flatten(target, cluster, byNode)
	if err != nil {
		return Verdict{}, err
	}

	// Built by upstream's own constructor, which is also what decides which
	// nodes carry pods with required anti-affinity — the index the topology
	// plugins ask for first.
	o.cluster.snapshot = cache.NewSnapshot(placed, cluster)

	nodeInfo, err := o.cluster.snapshot.NodeInfos().Get(target.Name)
	if err != nil {
		return Verdict{}, fmt.Errorf("node %s is missing from the snapshot: %w", target.Name, err)
	}
	nodes, err := o.cluster.snapshot.NodeInfos().List()
	if err != nil {
		return Verdict{}, fmt.Errorf("listing the snapshot's nodes: %w", err)
	}

	state := framework.NewCycleState()

	for _, p := range o.plugins {
		if p.pre != nil {
			_, status := p.pre.PreFilter(ctx, state, pod, nodes)
			if status.IsSkip() {
				continue
			}
			if !status.IsSuccess() {
				return refused(p.name, status), nil
			}
		}
		if status := p.filter.Filter(ctx, state, pod, nodeInfo); !status.IsSuccess() {
			return refused(p.name, status), nil
		}
	}

	return Verdict{Accepted: true}, nil
}

func refused(plugin string, status *fwk.Status) Verdict {
	return Verdict{Plugin: plugin, Reasons: status.Reasons(), Message: status.Message()}
}

// flatten checks the cluster describes a state the snapshot can represent, and
// returns every placed pod in it.
//
// The checks are not defensive tidiness. cache.NewSnapshot files a pod onto the
// node its spec.NodeName names and drops it silently otherwise, so a fixture
// that lists a resident under a node without binding it there would produce an
// empty node and an oracle that cheerfully accepts — a harness failing in the
// one direction it exists to catch, quietly.
func flatten(
	target *corev1.Node,
	cluster []*corev1.Node,
	byNode map[string][]*corev1.Pod,
) ([]*corev1.Pod, error) {
	known := make(map[string]bool, len(cluster))
	for _, n := range cluster {
		known[n.Name] = true
	}
	if !known[target.Name] {
		return nil, fmt.Errorf("target node %s is not among the %d nodes it is asked about",
			target.Name, len(cluster))
	}

	for name := range byNode {
		if !known[name] {
			return nil, fmt.Errorf("pods are listed under node %s, which is not in the cluster", name)
		}
	}

	// Walked in cluster order rather than map order, so that two runs over the
	// same scenario build the same snapshot. Nothing a Filter plugin decides
	// turns on the order pods were added in, but a harness whose input varies
	// run to run is one whose red is hard to reproduce.
	var placed []*corev1.Pod
	for _, n := range cluster {
		for _, p := range byNode[n.Name] {
			if p.Spec.NodeName != n.Name {
				return nil, fmt.Errorf("pod %s/%s is listed under node %s but is bound to %q",
					p.Namespace, p.Name, n.Name, p.Spec.NodeName)
			}
			placed = append(placed, p)
		}
	}
	return placed, nil
}

// Modelled returns the names of the Filter plugins this oracle runs, in the
// order it runs them.
func (o *Oracle) Modelled() []string {
	names := make([]string, 0, len(o.plugins))
	for _, p := range o.plugins {
		names = append(names, p.name)
	}
	return names
}
