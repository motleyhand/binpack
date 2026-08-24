package differential_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"os/exec"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/component-helpers/nodedeclaredfeatures/features/restartallcontainers"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/latest"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"

	"github.com/motleyhand/binpack/internal/fit"
	"github.com/motleyhand/binpack/internal/mother"
	"github.com/motleyhand/binpack/test/differential"
)

// check asserts the one-directional property for a single pod-node pair.
//
// The assertion is deliberately not equality. binpack is allowed to refuse a
// placement the scheduler would accept — that costs a missed consolidation,
// which the next run may find. It is never allowed to accept one the scheduler
// would refuse, because that is a drain, a Pending pod, and the scale-up this
// project exists to prevent.
func check(t *testing.T, o *differential.Oracle, name string, pod *corev1.Pod, node *corev1.Node, residents []*corev1.Pod) differential.Verdict {
	t.Helper()
	return checkIn(t, o, name, pod, node,
		[]*corev1.Node{node}, map[string][]*corev1.Pod{node.Name: residents})
}

// checkIn is check over a cluster of more than one node, for the predicates
// that are decided across nodes rather than on one.
//
// Both sides are given the same wider view, which is the point: binpack's is
// an index of the cluster's required anti-affinity built by
// fit.NewAntiAffinityDomains, and the scheduler's is the snapshot the oracle
// hands its plugins. Give either side only the candidate node and the
// disagreement this exists to find becomes unreachable — a zone-keyed term
// rejects nodes that host nothing matching it, so the node in front of you is
// the wrong place to look for it.
func checkIn(
	t *testing.T,
	o *differential.Oracle,
	name string,
	pod *corev1.Pod,
	target *corev1.Node,
	cluster []*corev1.Node,
	byNode map[string][]*corev1.Pod,
) differential.Verdict {
	t.Helper()

	residents := byNode[target.Name]

	// The same two entry points internal/engine uses, and for the same reason
	// the scheduler has two: residents are occupancies, sized from what their
	// node handed out, while the pod being placed is a demand, sized from its
	// spec. framework.NewNodeInfo and computePodResourceRequest split it
	// exactly here, so a harness that used one function for both would be
	// comparing binpack against a scheduler that does not exist.
	remaining := fit.Allocatable(target)
	for _, r := range residents {
		fit.Subtract(remaining, fit.ObservedRequests(r))
	}

	var placed []*corev1.Pod
	for _, node := range cluster {
		placed = append(placed, byNode[node.Name]...)
	}

	binpackFits, reason := fit.CanFit(pod, target, remaining, residents,
		fit.NewAntiAffinityDomains(cluster, placed))

	verdict, err := o.Accepts(pod, target, cluster, byNode)
	if err != nil {
		t.Fatalf("%s: oracle failed: %v", name, err)
	}

	if binpackFits && !verdict.Accepted {
		t.Errorf("%s: UNSOUND — binpack accepted a placement the scheduler refuses.\n"+
			"  scheduler: %s\n  pod:       %s/%s\n  node:      %s",
			name, verdict, pod.Namespace, pod.Name, target.Name)
	}

	// Not a failure, but worth seeing: every one of these is a consolidation
	// binpack declined that was actually available.
	if !binpackFits && verdict.Accepted {
		t.Logf("%s: conservative — binpack refused (%s) where the scheduler accepts", name, reason.Code)
	}

	return verdict
}

func newOracle(t *testing.T) *differential.Oracle {
	t.Helper()
	o, err := differential.NewOracle()
	if err != nil {
		t.Fatalf("building the oracle: %v", err)
	}
	return o
}

// TestMirrorsUnitCases runs the scenarios the unit tests assert against the
// real scheduler. Its job is to validate the harness itself: these answers are
// known, so a failure here means the oracle is wired up wrongly rather than
// that fit is wrong.
func TestMirrorsUnitCases(t *testing.T) {
	o := newOracle(t)

	cases := []struct {
		name      string
		pod       *corev1.Pod
		node      *corev1.Node
		residents []*corev1.Pod
		// schedulerAccepts is what the real scheduler must say, asserted
		// independently of fit. Without it this suite is one-directional too,
		// and an oracle that accepted everything would pass every case here
		// AND make the generated suite vacuous — binpack's refusals would all
		// log as "conservative" and the unsound direction would be
		// unreachable. These are the answers the harness is calibrated on.
		schedulerAccepts bool
	}{
		{"ordinary pod on ordinary node", mother.Pod("default", "web"), mother.SmallNode("n"), nil, true},
		{"cordoned node", mother.Pod("default", "web"), mother.SmallNode("n", mother.Cordoned()), nil, false},
		{
			"untolerated NoSchedule taint",
			mother.Pod("default", "web"),
			mother.SmallNode("n", mother.Tainted("dedicated", "db", corev1.TaintEffectNoSchedule)),
			nil, false,
		},
		{
			"tolerated taint",
			mother.Pod("default", "web", mother.Tolerating("dedicated", corev1.TaintEffectNoSchedule)),
			mother.SmallNode("n", mother.Tainted("dedicated", "db", corev1.TaintEffectNoSchedule)),
			nil, true,
		},
		{
			"PreferNoSchedule does not block",
			mother.Pod("default", "web"),
			mother.SmallNode("n", mother.Tainted("spot", "true", corev1.TaintEffectPreferNoSchedule)),
			nil, true,
		},
		{
			"unsatisfied node selector",
			mother.Pod("default", "web", mother.WithNodeSelector("disk", "ssd")),
			mother.SmallNode("n"),
			nil, false,
		},
		{
			"satisfied node selector",
			mother.Pod("default", "web", mother.WithNodeSelector("disk", "ssd")),
			mother.SmallNode("n", mother.NodeLabels(map[string]string{"disk": "ssd"})),
			nil, true,
		},
		{
			"memory exhausted",
			mother.Pod("default", "hungry", mother.Requests("100m", "8Gi")),
			mother.SmallNode("n"),
			nil, false,
		},
		{
			"extended resource absent",
			mother.Pod("default", "trainer", mother.Requesting("nvidia.com/gpu", "1")),
			mother.LargeNode("n"),
			nil, false,
		},
		{
			"extended resource present",
			mother.Pod("default", "trainer", mother.Requesting("nvidia.com/gpu", "1")),
			mother.GPUNode("n", 2),
			nil, true,
		},
		{
			// The arithmetic that produced several defects: the scheduler
			// reserves the init container's peak, not the container sum.
			// 1200Mi peak fits a 1360Mi node; 128Mi+1200Mi would not.
			"init container peak",
			mother.Pod("default", "web",
				mother.Requests("100m", "128Mi"),
				mother.WithInitContainer("migrate", "100m", "1200Mi")),
			mother.SmallNode("n"),
			nil, true,
		},
		{
			// A native sidecar keeps running, so 1000Mi+500Mi exceeds 1360Mi.
			// If sidecars were treated as ordinary init containers this would
			// be accepted — which is what the first oracle misconfiguration
			// would have hidden.
			"native sidecar adds rather than maxes",
			mother.Pod("default", "web",
				mother.Requests("100m", "1000Mi"),
				mother.WithSidecar("proxy", "100m", "500Mi")),
			mother.SmallNode("n"),
			nil, false,
		},
		{
			// 1200Mi + 256Mi overhead exceeds 1360Mi.
			"RuntimeClass overhead counts",
			mother.Pod("default", "web",
				mother.Requests("100m", "1200Mi"),
				mother.WithOverhead("50m", "256Mi")),
			mother.SmallNode("n"),
			nil, false,
		},
		{
			// The pod requires something of the node's kubelet rather than of
			// its capacity: a RestartAllContainers restart rule needs the
			// destination to have published RestartAllContainersOnContainerExits
			// in status.declaredFeatures. An ordinary node fixture declares
			// nothing, which is what a node running an older kubelet — or one
			// with the gate off — looks like.
			"undeclared node feature",
			mother.Pod("default", "web", mother.WithRestartAllContainersRule()),
			mother.SmallNode("n"),
			nil, false,
		},
		{
			// The same pod on a node that has declared it. Paired with the row
			// above so the refusal is shown to be about the feature and not
			// about restart rules in general — a blanket refusal would satisfy
			// the first row and cost every such workload its relocations.
			"declared node feature",
			mother.Pod("default", "web", mother.WithRestartAllContainersRule()),
			mother.SmallNode("n", mother.DeclaringFeature(
				restartallcontainers.RestartAllContainersOnContainerExits)),
			nil, true,
		},
		{
			// NodeName and NodePorts refuse nothing the generator produces,
			// because fit.UnsupportedPod refuses both shapes before a
			// destination is ever considered. These two rows are where that
			// refusal is falsifiable rather than assumed: each asserts the
			// scheduler's own verdict, so an oracle that stopped running
			// either plugin fails here instead of agreeing with binpack for
			// the wrong reason.
			"pod pinned to another node",
			mother.Pod("default", "web", mother.OnNode("elsewhere")),
			mother.SmallNode("n"),
			nil, false,
		},
		{
			"host port already taken",
			mother.Pod("default", "web", mother.WithHostPort(8080)),
			mother.SmallNode("n"),
			[]*corev1.Pod{
				mother.Pod("default", "sitting", mother.OnNode("n"), mother.WithHostPort(8080)),
			},
			false,
		},
		{
			// 1360Mi node, 1000Mi resident, 600Mi candidate.
			"residents consume capacity",
			mother.Pod("default", "web", mother.Requests("100m", "600Mi")),
			mother.SmallNode("n"),
			[]*corev1.Pod{
				mother.Pod("default", "sitting", mother.Requests("100m", "1000Mi"), mother.OnNode("n")),
			},
			false,
		},
		{
			// The same 1000Mi resident, except that its spec now says 128Mi:
			// something patched the pod's requests downward in place and the
			// kubelet has not actuated it, which for a memory decrease below
			// current usage it cannot. PodInfo.CalculateResource charges
			// max(spec, actuated, allocated), so the node still holds 1000Mi
			// of it and 360Mi is all that is left.
			//
			// Read from the spec alone the node looks 1232Mi empty, which is
			// the whole finding: binpack accepts, the scheduler refuses, and
			// the relocated pod goes Pending on a node binpack has just
			// cordoned.
			"resident mid-downward-resize still occupies what it actuated",
			mother.Pod("default", "web", mother.Requests("100m", "600Mi")),
			mother.SmallNode("n"),
			[]*corev1.Pod{
				mother.Pod("default", "sitting", mother.OnNode("n"), mother.ResizingFrom(
					corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
					},
				)),
			},
			false,
		},
		{
			// Paired with the row above so the refusal is shown to be about
			// the arithmetic and not about resizing residents as a class. The
			// cheaper fix — refuse any destination hosting a pod mid-resize —
			// is sound and would satisfy the row above while costing every
			// relocation onto a node with 360Mi genuinely free. 300Mi fits
			// there, and both must say so.
			"a resizing resident does not disqualify its node",
			mother.Pod("default", "web", mother.Requests("100m", "300Mi")),
			mother.SmallNode("n"),
			[]*corev1.Pod{
				mother.Pod("default", "sitting", mother.OnNode("n"), mother.ResizingFrom(
					corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("1000Mi"),
					},
				)),
			},
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := check(t, o, tc.name, tc.pod, tc.node, tc.residents)

			if verdict.Accepted != tc.schedulerAccepts {
				t.Errorf("ORACLE MISCALIBRATED — scheduler accepted=%t, expected %t (%s).\n"+
					"Fix the harness before trusting anything the generated suite reports.",
					verdict.Accepted, tc.schedulerAccepts, verdict)
			}
		})
	}
}

// reachedByGenerator names the Filter plugins the generated corpus has to keep
// making refuse.
//
// The guards this sits beside are two scalars: at least one binpack acceptance
// and at least one oracle refusal. The second is satisfied more than a thousand
// times over by cordons and taints alone, so an oracle that stopped
// discriminating in the dimension the project is actually at risk in —
// resource arithmetic — would leave it green. A guard passing for the wrong
// reason reads as evidence, which is worse than no guard, because the next
// person takes the green as coverage of the thing it never looked at.
var reachedByGenerator = []string{
	names.NodeUnschedulable,
	names.TaintToleration,
	names.NodeAffinity,
	names.NodeResourcesFit,
}

// notReachedByGenerator is the other half, and it is the half that has to be
// written down. A plugin refusing nothing is either a corpus that stopped
// probing it or a generator that never could, and only one of those is a
// defect — so each entry says which, and names where the plugin is adjudicated
// instead. The count is asserted to be zero, so a generator that starts
// reaching one of these fails here until the record catches up.
var notReachedByGenerator = map[string]string{
	names.NodeName: "the generator never sets spec.nodeName on the candidate, and " +
		"fit.UnsupportedPod refuses a pod that carries one, so the two can only " +
		"ever agree. Exercised by TestMirrorsUnitCases' \"pod pinned to another node\".",
	names.NodePorts: "the generator emits no hostPort, and fit.UnsupportedPod refuses " +
		"a pod that declares one. Exercised by TestMirrorsUnitCases' \"host port " +
		"already taken\".",
	names.NodeDeclaredFeatures: "two of the five features in this release's registry infer " +
		"a requirement at scheduling time rather than only on update: a container " +
		"carrying a RestartAllContainers restart rule, which the generator does not " +
		"emit and TestMirrorsUnitCases' \"undeclared node feature\" does; and host " +
		"networking with hostUsers false, which fit.UnsupportedPod refuses for the " +
		"hostPort reason before a destination is considered " +
		"(k8s.io/component-helpers v0.36.3 nodedeclaredfeatures/features/registry.go, " +
		"AllFeatures).",
	names.PodTopologySpread: "the generator emits no DoNotSchedule spread constraint, " +
		"which is the only kind this plugin's Filter can reject on — the release's " +
		"System defaults are ScheduleAnyway and reach Score only. fit.UnsupportedPod " +
		"refuses a pod declaring the hard kind.",
	names.InterPodAffinity: "the generator builds one node at a time, and the " +
		"disagreement worth having here is between nodes. Exercised by " +
		"TestZoneWideAntiAffinityIsAdjudicated.",
}

// mustShortfall names the resource dimensions NodeResourcesFit has to be seen
// refusing on, in the plugin's own words.
//
// This is the guard the per-plugin one cannot be. NodeResourcesFit answers for
// every dimension at once, so it keeps refusing thousands of scenarios on
// memory while a single dimension goes quiet — which is exactly what
// NodeResourcesFitArgs.IgnoredResources does to an extended resource, and what
// a generator drifting away from a boundary does to `pods`. The strings are
// upstream's InsufficientResource.Reason values (k8s.io/kubernetes v1.36.3
// pkg/scheduler/framework/plugins/noderesources/fit.go, fitsRequest); note
// that the pod-ceiling one is "Too many pods" rather than the "Insufficient"
// form every other dimension uses.
var mustShortfall = []string{
	"Insufficient cpu",
	"Insufficient memory",
	"Insufficient nvidia.com/gpu",
	"Too many pods",
}

// TestGeneratedStates is where the value is. It does not require anyone to
// have thought of the failure first, which is how every defect in this
// predicate was found so far.
func TestGeneratedStates(t *testing.T) {
	o := newOracle(t)

	const cases = 3000
	rng := rand.New(rand.NewSource(20260815))

	accepted, refused, oracleRefused := 0, 0, 0
	byPlugin := map[string]int{}
	byShortfall := map[string]int{}
	for i := range cases {
		node, pod, residents := randomScenario(rng, i)

		remaining := fit.Allocatable(node)
		for _, r := range residents {
			fit.Subtract(remaining, fit.ObservedRequests(r))
		}
		if ok, _ := fit.CanFit(pod, node, remaining, residents,
			fit.NewAntiAffinityDomains([]*corev1.Node{node}, residents)); ok {
			accepted++
		} else {
			refused++
		}

		v := check(t, o, fmt.Sprintf("generated/%d", i), pod, node, residents)
		if v.Accepted {
			continue
		}
		oracleRefused++
		byPlugin[v.Plugin]++
		if v.Plugin == names.NodeResourcesFit {
			for _, reason := range v.Reasons {
				byShortfall[reason]++
			}
		}
	}

	// Ways this suite could pass while proving nothing, all worth failing on
	// rather than discovering later.

	// If binpack accepts none of the corpus, the unsound direction is never
	// exercised: it is only reachable through an acceptance.
	if accepted == 0 {
		t.Fatalf("generated %d scenarios and binpack accepted none — the corpus cannot test the property", cases)
	}

	// If the oracle refuses none, it may simply be accepting everything, in
	// which case no acceptance by binpack could ever be caught. That is the
	// failure mode a one-directional check cannot see from the inside.
	if oracleRefused == 0 {
		t.Fatalf("the scheduler refused none of %d scenarios — the oracle is not discriminating, so this suite proves nothing", cases)
	}

	// And the same question asked per plugin and per resource dimension,
	// because the two above are answered by cordons and taints alone.
	for _, name := range o.Modelled() {
		_, unreachable := notReachedByGenerator[name]
		switch {
		case unreachable && slices.Contains(reachedByGenerator, name):
			t.Errorf("both lists claim %s, which cannot both be true", name)
		case !unreachable && !slices.Contains(reachedByGenerator, name):
			t.Errorf("the oracle runs %s and neither list accounts for it, so nobody has "+
				"said whether this corpus is supposed to exercise it", name)
		}
	}
	for _, name := range reachedByGenerator {
		if byPlugin[name] == 0 {
			t.Errorf("%s refused none of %d generated scenarios. Either the corpus has "+
				"drifted away from what it decides, or it has stopped deciding — and "+
				"the aggregate guards above cannot tell you which, because the other "+
				"plugins go on refusing thousands of cases either way.", name, cases)
		}
	}
	for name, why := range notReachedByGenerator {
		if byPlugin[name] != 0 {
			t.Errorf("%s refused %d generated scenarios, but the record says it cannot be "+
				"reached:\n  %s\nUpdate the record — a false one reads as coverage.",
				name, byPlugin[name], why)
		}
	}
	for _, reason := range mustShortfall {
		if byShortfall[reason] == 0 {
			t.Errorf("no generated scenario was refused for %q. NodeResourcesFit goes on "+
				"refusing on the other dimensions, so nothing else here notices that "+
				"this one has stopped being tested.", reason)
		}
	}

	t.Logf("%d scenarios: binpack accepted %d, refused %d; scheduler refused %d",
		cases, accepted, refused, oracleRefused)
	t.Logf("scheduler refusals by plugin: %v", byPlugin)
	t.Logf("NodeResourcesFit refusals by dimension: %v", byShortfall)
}

func randomScenario(rng *rand.Rand, i int) (*corev1.Node, *corev1.Pod, []*corev1.Pod) {
	name := fmt.Sprintf("n%d", i)

	var nodeOpts []mother.NodeOption
	switch rng.Intn(7) {
	case 0:
		nodeOpts = append(nodeOpts, mother.Cordoned())
	case 1:
		nodeOpts = append(nodeOpts, mother.Tainted("dedicated", "db", corev1.TaintEffectNoSchedule))
	case 2:
		nodeOpts = append(nodeOpts, mother.Tainted("drain", "soon", corev1.TaintEffectNoExecute))
	case 3:
		nodeOpts = append(nodeOpts, mother.Tainted("spot", "true", corev1.TaintEffectPreferNoSchedule))
	case 4:
		nodeOpts = append(nodeOpts, mother.NodeLabels(map[string]string{"disk": "ssd"}))
	case 5:
		// Numeric, so a Gt toleration has something to compare against.
		nodeOpts = append(nodeOpts, mother.Tainted("shard", "5", corev1.TaintEffectNoSchedule))
	}

	var node *corev1.Node
	switch rng.Intn(4) {
	case 0:
		node = mother.SmallNode(name, nodeOpts...)
	case 1:
		node = mother.LargeNode(name, nodeOpts...)
	case 2:
		// The only branch whose pod ceiling the resident count below can
		// reach, and the reason it is here. Every other node mother advertises
		// 110 slots against at most three residents, so `pods` was a dimension
		// the corpus could not exercise however many scenarios it generated:
		// the synthetic pods:1 that fit.EffectiveRequests adds could be
		// deleted outright and not one of the three thousand decisions would
		// move. A dimension nothing reaches is a dimension nothing is testing.
		node = mother.CappedNode(name, nodeOpts...)
	default:
		node = mother.GPUNode(name, int64(rng.Intn(3)), nodeOpts...)
	}

	podOpts := []mother.PodOption{
		mother.Requests(
			fmt.Sprintf("%dm", 50+rng.Intn(2000)),
			fmt.Sprintf("%dMi", 32+rng.Intn(3000)),
		),
	}
	switch rng.Intn(8) {
	case 0:
		podOpts = append(podOpts, mother.Tolerating("dedicated", corev1.TaintEffectNoSchedule))
	case 1:
		podOpts = append(podOpts, mother.WithNodeSelector("disk", "ssd"))
	case 2:
		podOpts = append(podOpts, mother.WithInitContainer("init",
			fmt.Sprintf("%dm", 50+rng.Intn(2000)), fmt.Sprintf("%dMi", 32+rng.Intn(3000))))
	case 3:
		podOpts = append(podOpts, mother.WithSidecar("proxy",
			fmt.Sprintf("%dm", 10+rng.Intn(500)), fmt.Sprintf("%dMi", 16+rng.Intn(512))))
	case 4:
		podOpts = append(podOpts, mother.WithOverhead("25m", fmt.Sprintf("%dMi", 32+rng.Intn(256))))
	case 5:
		podOpts = append(podOpts, mother.Requesting("nvidia.com/gpu", fmt.Sprintf("%d", 1+rng.Intn(2))))
	case 6:
		podOpts = append(podOpts, mother.ToleratingGt("shard", "3"))
	}

	pod := mother.Pod("default", fmt.Sprintf("p%d", i), podOpts...)

	var residents []*corev1.Pod
	for r := range rng.Intn(4) {
		residents = append(residents, mother.Pod("default", fmt.Sprintf("r%d-%d", i, r),
			mother.OnNode(name),
			mother.Requests(
				fmt.Sprintf("%dm", 50+rng.Intn(500)),
				fmt.Sprintf("%dMi", 32+rng.Intn(700)),
			),
		))
	}

	return node, pod, residents
}

// TestPodSlotsAreEnforced pins the property that motivated synthesising a
// pods:1 request: a node at its pod ceiling must refuse, however much CPU and
// memory it has free. The scheduler enforces this; a naive request-subtraction
// model does not.
func TestPodSlotsAreEnforced(t *testing.T) {
	o := newOracle(t)

	node := mother.LargeNode("n", mother.Allocatable(corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("32"),
		corev1.ResourceMemory: resource.MustParse("128Gi"),
		corev1.ResourcePods:   resource.MustParse("2"),
	}))
	residents := []*corev1.Pod{
		mother.Pod("default", "a", mother.OnNode("n"), mother.Requests("10m", "16Mi")),
		mother.Pod("default", "b", mother.OnNode("n"), mother.Requests("10m", "16Mi")),
	}
	pod := mother.Pod("default", "third", mother.Requests("10m", "16Mi"))

	verdict, err := o.Accepts(pod, node,
		[]*corev1.Node{node}, map[string][]*corev1.Pod{node.Name: residents})
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Accepted {
		t.Fatal("the scheduler should refuse a third pod on a node with two slots; the oracle is wrong")
	}

	check(t, o, "pod slots exhausted", pod, node, residents)
}

// TestZoneWideAntiAffinityIsAdjudicated is the case the harness could not
// express while its oracle modelled one node.
//
// Upstream counts pods matching a required anti-affinity term across the whole
// topology domain the term names. At kubernetes.io/hostname that domain is one
// node and asking a candidate about its own residents gets the right answer;
// at any wider key it does not, and it is wrong in the accepting direction —
// binpack approves a node holding nothing that matches, the eviction has
// already happened, and the replacement is Pending. That is the disagreement
// the fit predicate's index of the cluster's terms exists to close, and until
// Accepts took a cluster nothing here could tell whether it had.
//
// Both rows assert the scheduler's own verdict, independently of fit. The
// second is not decoration: refusing every node in a zone that holds any
// anti-affinity at all would satisfy the first row and cost the cluster every
// consolidation, so the pair is what says the refusal is about the match.
func TestZoneWideAntiAffinityIsAdjudicated(t *testing.T) {
	o := newOracle(t)

	const zone = corev1.LabelTopologyZone
	inZone := mother.NodeLabels(map[string]string{zone: "a"})

	// Node b holds the pod whose anti-affinity spans the zone; node c is in
	// the same zone and holds nothing at all.
	b, c := mother.LargeNode("b", inZone), mother.LargeNode("c", inZone)
	cluster := []*corev1.Node{b, c}
	byNode := map[string][]*corev1.Pod{
		"b": {mother.Pod("default", "sitting", mother.OnNode("b"),
			mother.WithRequiredAntiAffinityAt(zone, "app", "web"))},
	}

	cases := []struct {
		name             string
		pod              *corev1.Pod
		schedulerAccepts bool
	}{
		{
			"matching pod refused a node in the domain, not just the node holding it",
			mother.Pod("default", "web", mother.PodLabels(map[string]string{"app": "web"})),
			false,
		},
		{
			"a pod the term does not select is placed there",
			mother.Pod("default", "api", mother.PodLabels(map[string]string{"app": "api"})),
			true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			verdict := checkIn(t, o, tc.name, tc.pod, c, cluster, byNode)

			if verdict.Accepted != tc.schedulerAccepts {
				t.Fatalf("ORACLE MISCALIBRATED — scheduler accepted=%t, expected %t (%s).\n"+
					"Fix the harness before trusting anything it reports about topology.",
					verdict.Accepted, tc.schedulerAccepts, verdict)
			}
			if !tc.schedulerAccepts && verdict.Plugin != names.InterPodAffinity {
				t.Fatalf("the scheduler refused through %s rather than %s, so this case is "+
					"no longer about anti-affinity: %s",
					verdict.Plugin, names.InterPodAffinity, verdict)
			}
		})
	}
}

// exemption is one internal/fit refusal that keeps binpack away from the
// inputs a Filter plugin judges, and so justifies the oracle not running it.
type exemption struct {
	// why names the predicate and says what it refuses. Prose, because the
	// interesting half of an exemption is the half no compiler can check:
	// what the plugin decides, and why binpack never puts it in a position
	// to decide it.
	why string
	// refused is that predicate, applied to an input the plugin would judge.
	// The test runs it, so the exemption holds only while the refusal it
	// claims is still there — delete the check in internal/fit and this map
	// stops agreeing rather than staying reassuring.
	//
	// A witness, not a proof of closure. It shows the refusal exists, not
	// that it covers every input the plugin sees.
	refused func() fit.Reason
	// gap is an input this plugin rejects and internal/fit does not, for an
	// exemption whose refusal is narrower than the plugin it stands in for.
	//
	// It is not a limitation binpack has accepted. It is an unsound
	// acceptance — binpack approving a destination the scheduler refuses —
	// and the reason it is written here rather than left in prose is that
	// prose cannot stop a green coverage test from reading as coverage. The
	// test asserts the gap is still open, which means one cannot be declared
	// where none exists, and that closing it in internal/fit fails here. The
	// record deletes itself when the defect is fixed instead of rotting into
	// a false statement about a package that has moved on.
	gap func() fit.Reason
}

// exempt records, for each default Filter plugin the oracle does not run, the
// internal/fit refusals it rests on.
//
// This map is the dangerous half of the coverage test below. Prose alone would
// make it the place to put any plugin that is inconvenient to construct, and
// an exemption nobody can falsify turns the allowlist into a denylist — the
// one shape ADR-0006 says binpack's must never take. So each entry carries a
// predicate and an input the test runs, and each entry is a limitation of
// binpack. Read a new one as if you were adding one, because you are.
var exempt = map[string][]exemption{
	names.VolumeRestrictions: {{
		why: "fit.UnsupportedPod refuses any pod carrying a volume it cannot prove " +
			"node-independent, and volumes are the whole of what this plugin filters " +
			"on: it rejects a node where a resident already holds the incoming pod's " +
			"read-write-once volume.",
		refused: func() fit.Reason {
			return fit.UnsupportedPod(mother.Pod("default", "web", mother.WithPVC("data")))
		},
	}},
	names.NodeVolumeLimits: {{
		why: "fit.UnsupportedPod refuses any pod carrying a volume it cannot prove " +
			"node-independent, which is every volume that could count against a CSI " +
			"driver's per-node attachment limit.",
		refused: func() fit.Reason {
			return fit.UnsupportedPod(mother.Pod("default", "web",
				mother.WithInlineCSIVolume("data", "csi.example.com")))
		},
	}},
	names.VolumeBinding: {{
		why: "fit.UnsupportedPod refuses any pod carrying a volume it cannot prove " +
			"node-independent, so binpack never asks whether a claim could bind to a " +
			"volume this node can reach.",
		refused: func() fit.Reason {
			return fit.UnsupportedPod(mother.Pod("default", "web", mother.WithPVC("data")))
		},
	}},
	names.VolumeZone: {{
		why: "fit.UnsupportedPod refuses any pod carrying a volume it cannot prove " +
			"node-independent, so binpack never asks whether the node's zone matches " +
			"the zone affinity of a volume already bound to one.",
		refused: func() fit.Reason {
			return fit.UnsupportedPod(mother.Pod("default", "web", mother.WithPVC("data")))
		},
	}},
	names.DynamicResources: {{
		why: "fit.UnsupportedPod refuses any pod carrying resource claims, which are " +
			"the only pods this plugin filters on. The oracle also clears the DRA " +
			"gates, since the plugin reads device availability through a Handle it " +
			"has no manager for.",
		refused: func() fit.Reason {
			return fit.UnsupportedPod(mother.Pod("default", "web", mother.WithResourceClaim("gpu")))
		},
	}},
}

// TestOracleCoversEveryDefaultFilterPlugin derives the Filter plugins the
// pinned release enables by default, and asserts the oracle accounts for every
// one of them.
//
// The set the oracle builds used to be four literal constructor calls, which
// is the practice this package's own doc comment argues against one level
// down. For feature gates a hand-written list produced 171 confident, wrong
// reports — and that is the benign failure, because it was loud. A
// hand-written plugin list fails the other way round: a placement the
// scheduler refuses through a plugin the oracle never runs is accepted on both
// sides, so check's only failing condition cannot fire, and the harness reads
// green exactly where it is blind. Neither vacuity guard in TestGeneratedStates
// can see it either, since the modelled plugins refuse thousands of scenarios
// on their own.
//
// Accounted for means one of two things:
//
//   - the oracle constructs it, so the scheduler's own code answers; or
//   - exempt names the internal/fit refusals that keep binpack away from the
//     inputs it judges, and this test runs them.
//
// Anything else fails, naming the plugin. That is the whole point: the next
// release that adds a Filter plugin has to be classified by somebody, rather
// than quietly joining the set of things nobody has thought about.
//
// Accounted for is not the same as sound, and this test is careful not to be
// read as saying so. An exemption whose refusal is narrower than the plugin it
// stands in for carries a gap: an input binpack accepts and the plugin rejects,
// asserted here to be still open and logged on every green run. None does
// today — InterPodAffinity's was closed by the topology-domain index, and the
// field is kept because the next narrow exemption should not have to rebuild
// the mechanism. Green means every default Filter plugin has been looked at and
// its status written down — not that binpack agrees with all of them.
func TestOracleCoversEveryDefaultFilterPlugin(t *testing.T) {
	cfg, err := latest.Default()
	if err != nil {
		t.Fatalf("reading the release's default scheduler configuration: %v", err)
	}
	if len(cfg.Profiles) != 1 {
		t.Fatalf("expected exactly one default profile, got %d", len(cfg.Profiles))
	}
	profile := cfg.Profiles[0]

	// Defaulted args, from the same source as the plugin names and the same
	// place the oracle takes them. Passing hand-written args would reintroduce
	// the problem one field down.
	args, err := differential.DefaultPluginArgs()
	if err != nil {
		t.Fatalf("reading the release's default plugin arguments: %v", err)
	}

	enabled := make([]string, 0, len(profile.Plugins.MultiPoint.Enabled))
	for _, entry := range profile.Plugins.MultiPoint.Enabled {
		enabled = append(enabled, entry.Name)
	}

	registry := plugins.NewInTreeRegistry()
	modelled := newOracle(t).Modelled()

	// A Verdict names the plugin the scheduler would have reported first,
	// which is only true while the oracle runs its plugins in profile order.
	// Nothing else notices if that stops being so: every answer stays correct
	// and every disagreement report starts pointing at the wrong predicate.
	if before := profileOrder(enabled, modelled); before != nil {
		t.Errorf("the oracle runs %s before %s; this release's default profile runs them "+
			"the other way round, so a Verdict would name a plugin the scheduler "+
			"would not have reached first.", before[0], before[1])
	}

	// An exemption that no longer describes anything reads as coverage, so
	// both ways of going stale are failures rather than untidiness.
	for name := range exempt {
		switch {
		case !slices.Contains(enabled, name):
			t.Errorf("exempt names %s, which this release's default profile does not "+
				"enable — drop the exemption", name)
		case slices.Contains(modelled, name):
			t.Errorf("exempt names %s, which the oracle now runs — drop the exemption", name)
		case len(exempt[name]) == 0:
			t.Errorf("exempt names %s with no refusal behind it, which is not an exemption", name)
		}
	}

	for _, name := range enabled {
		t.Run(name, func(t *testing.T) {
			if slices.Contains(modelled, name) {
				return
			}

			if reasons, ok := exempt[name]; ok {
				for _, e := range reasons {
					reason := e.refused()
					switch {
					case reason.Empty():
						t.Errorf("%s is exempt because:\n  %s\n"+
							"but that predicate accepted the input it names, so the "+
							"exemption rests on a refusal that is no longer there.",
							name, e.why)
					case reason.Code != fit.ReasonUnsupportedPod && reason.Code != fit.ReasonUnsupportedNode:
						t.Errorf("%s is exempt because:\n  %s\n"+
							"but that predicate answered %s rather than declining to "+
							"model the input, and a refusal binpack makes for some "+
							"other reason does not stand in for a plugin it never runs.",
							name, e.why, reason.Code)
					}

					if e.gap == nil {
						continue
					}
					if closed := e.gap(); !closed.Empty() {
						t.Errorf("%s records a gap internal/fit now closes (%s: %s).\n"+
							"Delete the gap and narrow the exemption's reason — the "+
							"record has done its job and is now a false statement.",
							name, closed.Code, closed.Message)
						continue
					}
					// Passing here is not the same as being covered, and the
					// difference is worth printing: CI runs this package with
					// -v, so the open gap is in the log of every green run.
					t.Logf("%s is exempt with a gap still open: binpack accepts an "+
						"input this plugin refuses. Not a limitation, a defect.", name)
				}
				return
			}

			// Neither modelled nor exempt. Build it to find out whether it
			// filters at all: most of the default profile does not, and a
			// plugin nobody can construct is a plugin nobody has classified,
			// which is the state this test exists to end.
			factory, ok := registry[name]
			if !ok {
				t.Fatalf("%s is enabled by default but absent from the in-tree registry", name)
			}
			p, err := factory(t.Context(), args[name], schedulerHandle(t))
			if err != nil {
				t.Fatalf("cannot classify %s: constructing it failed: %v\n"+
					"Build it in NewOracle, or exempt it naming the internal/fit "+
					"refusal that keeps binpack away from its input.", name, err)
			}
			if _, filters := p.(fwk.FilterPlugin); !filters {
				return
			}
			t.Errorf("%s filters placements and the oracle neither runs it nor exempts "+
				"it, so the harness cannot disagree with the scheduler about anything "+
				"it decides.\nBuild it in NewOracle, or exempt it naming the "+
				"internal/fit refusal that keeps binpack away from its input.", name)
		})
	}
}

// profileOrder reports the first pair the oracle runs out of profile order, as
// {earlier-in-the-oracle, earlier-in-the-profile}, or nil when it agrees.
func profileOrder(enabled, modelled []string) []string {
	for i := 1; i < len(modelled); i++ {
		prev, cur := slices.Index(enabled, modelled[i-1]), slices.Index(enabled, modelled[i])
		if prev >= 0 && cur >= 0 && prev > cur {
			return []string{modelled[i-1], modelled[i]}
		}
	}
	return nil
}

// schedulerHandle builds the least a default plugin needs to construct: a
// snapshot lister, an informer factory and a client. Nothing here is asked a
// question — the handle exists so that a plugin the oracle does not run can
// still be classified, rather than being exempted because it panicked.
func schedulerHandle(t *testing.T) fwk.Handle {
	t.Helper()

	client := fake.NewClientset()
	h, err := frameworkruntime.NewFramework(t.Context(), nil, nil,
		frameworkruntime.WithSnapshotSharedLister(cache.NewEmptySnapshot()),
		frameworkruntime.WithInformerFactory(informers.NewSharedInformerFactory(client, 0)),
		frameworkruntime.WithClientSet(client),
	)
	if err != nil {
		t.Fatalf("building a scheduler handle: %v", err)
	}
	return h
}

// TestHarnessTracksTheShippedLibraries asserts that every k8s.io module the
// main module requires directly resolves, in this module's build, to the
// version the main module names.
//
// This module's replace block is thirty literal versions long, because
// k8s.io/kubernetes pins its own staging dependencies at v0.0.0 and leaves
// every consumer to restate them. A Go replace overrides version selection
// unconditionally and `go mod tidy` has no opinion about the version one
// names, so a dependency bump in the main module leaves the block untouched,
// tidy stays silent, the build succeeds — and the oracle goes on comparing the
// scheduler against a copy of internal/fit compiled against a library binpack
// no longer ships. Tidiness catches a stale *require*; nothing was catching a
// stale *replace*.
//
// The consequence is specific rather than theoretical. internal/fit hands the
// whole effective-request computation to k8s.io/component-helpers'
// resource.PodRequests, and how much behaviour turns on that function's
// options is the subject of internal/fit's own two entry points. A green run
// reported about code that is not shipping is worse than no run at all,
// because the strongest correctness claim in the project would go on being
// made about a previous release with nothing saying so.
//
// Both halves are read rather than assumed. The requirements come from
// `go mod edit -json` over the main module's go.mod, which is the only thing
// that knows which of them are direct; the resolved version comes from
// `go list -m` in this module, which is the only thing that accounts for a
// replace.
func TestHarnessTracksTheShippedLibraries(t *testing.T) {
	var shipped struct {
		Require []struct {
			Path     string
			Version  string
			Indirect bool
		}
	}
	out, err := goList(t, "mod", "edit", "-json", "../../go.mod")
	if err != nil {
		t.Fatalf("reading the main module's go.mod: %v", err)
	}
	if err := json.Unmarshal(out, &shipped); err != nil {
		t.Fatalf("parsing the main module's go.mod: %v", err)
	}

	checked := 0
	for _, r := range shipped.Require {
		if r.Indirect || !strings.HasPrefix(r.Path, "k8s.io/") {
			continue
		}
		checked++

		// The template collapses both cases deliberately: inside `with` the
		// dot is the replacement module, so this is the version the compiler
		// will actually use either way.
		out, err := goList(t, "list", "-m",
			"-f", "{{with .Replace}}{{.Version}}{{else}}{{.Version}}{{end}}", r.Path)
		if err != nil {
			t.Errorf("binpack requires %s %s and this module cannot resolve it at all:\n  %v\n"+
				"Add it, so the oracle is compiled against what binpack ships.",
				r.Path, r.Version, err)
			continue
		}
		if got := strings.TrimSpace(string(out)); got != r.Version {
			t.Errorf("binpack ships %s %s and the harness builds against %s.\n"+
				"The replace directive in this module's go.mod overrides version "+
				"selection, so `go mod tidy` will not report this and the build will "+
				"not fail — the oracle simply stops judging the code that ships.",
				r.Path, r.Version, got)
		}
	}

	if checked == 0 {
		t.Fatal("the main module requires no k8s.io module directly, which cannot be " +
			"true while internal/fit exists — this test has stopped reading anything")
	}
	t.Logf("%d k8s.io modules tracked between the two go.mod files", checked)
}

// goList runs the go command in this module's directory and returns its
// stdout, with the tool's own stderr in the error — a version mismatch
// reported without go's explanation of why it could not resolve something is
// most of a diagnosis missing.
func goList(t *testing.T, args ...string) ([]byte, error) {
	t.Helper()

	cmd := exec.Command("go", args...)
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil, fmt.Errorf("go %s: %w: %s", strings.Join(args, " "), err, exit.Stderr)
	}
	if err != nil {
		return nil, fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}
