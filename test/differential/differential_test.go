package differential_test

import (
	"fmt"
	"math/rand"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

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

	remaining := fit.Allocatable(node)
	for _, r := range residents {
		fit.Subtract(remaining, fit.EffectiveRequests(r))
	}

	binpackFits, reason := fit.CanFit(pod, node, remaining, residents)

	verdict, err := o.Accepts(pod, node, residents)
	if err != nil {
		t.Fatalf("%s: oracle failed: %v", name, err)
	}

	if binpackFits && !verdict.Accepted {
		t.Errorf("%s: UNSOUND — binpack accepted a placement the scheduler refuses.\n"+
			"  scheduler: %s\n  pod:       %s/%s\n  node:      %s",
			name, verdict, pod.Namespace, pod.Name, node.Name)
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
			// 1360Mi node, 1000Mi resident, 600Mi candidate.
			"residents consume capacity",
			mother.Pod("default", "web", mother.Requests("100m", "600Mi")),
			mother.SmallNode("n"),
			[]*corev1.Pod{
				mother.Pod("default", "sitting", mother.Requests("100m", "1000Mi"), mother.OnNode("n")),
			},
			false,
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

// TestGeneratedStates is where the value is. It does not require anyone to
// have thought of the failure first, which is how every defect in this
// predicate was found so far.
func TestGeneratedStates(t *testing.T) {
	o := newOracle(t)

	const cases = 3000
	rng := rand.New(rand.NewSource(20260815))

	accepted, refused, oracleRefused := 0, 0, 0
	for i := range cases {
		node, pod, residents := randomScenario(rng, i)

		remaining := fit.Allocatable(node)
		for _, r := range residents {
			fit.Subtract(remaining, fit.EffectiveRequests(r))
		}
		if ok, _ := fit.CanFit(pod, node, remaining, residents); ok {
			accepted++
		} else {
			refused++
		}

		if v := check(t, o, fmt.Sprintf("generated/%d", i), pod, node, residents); !v.Accepted {
			oracleRefused++
		}
	}

	// Two ways this suite could pass while proving nothing, both worth
	// failing on rather than discovering later.

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

	t.Logf("%d scenarios: binpack accepted %d, refused %d; scheduler refused %d",
		cases, accepted, refused, oracleRefused)
}

func randomScenario(rng *rand.Rand, i int) (*corev1.Node, *corev1.Pod, []*corev1.Pod) {
	name := fmt.Sprintf("n%d", i)

	var nodeOpts []mother.NodeOption
	switch rng.Intn(6) {
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
	}

	var node *corev1.Node
	switch rng.Intn(3) {
	case 0:
		node = mother.SmallNode(name, nodeOpts...)
	case 1:
		node = mother.LargeNode(name, nodeOpts...)
	default:
		node = mother.GPUNode(name, int64(rng.Intn(3)), nodeOpts...)
	}

	podOpts := []mother.PodOption{
		mother.Requests(
			fmt.Sprintf("%dm", 50+rng.Intn(2000)),
			fmt.Sprintf("%dMi", 32+rng.Intn(3000)),
		),
	}
	switch rng.Intn(7) {
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

	verdict, err := o.Accepts(pod, node, residents)
	if err != nil {
		t.Fatal(err)
	}
	if verdict.Accepted {
		t.Fatal("the scheduler should refuse a third pod on a node with two slots; the oracle is wrong")
	}

	check(t, o, "pod slots exhausted", pod, node, residents)
}
