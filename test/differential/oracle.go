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
package differential

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeaffinity"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodedeclaredfeatures"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodename"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeports"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodeunschedulable"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/tainttoleration"
)

// Verdict is the scheduler's answer for one pod-node pair.
type Verdict struct {
	Accepted bool
	// Plugin names which filter rejected the placement, so a disagreement
	// points at the specific predicate rather than at "the scheduler".
	Plugin  string
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
}

// NewOracle builds the Filter plugins corresponding to the predicates binpack
// models: NodeUnschedulable, NodeName, TaintToleration, NodeAffinity,
// NodePorts, NodeResourcesFit and NodeDeclaredFeatures, in the order the
// release's default profile runs them — so the plugin a Verdict names is the
// one the scheduler would have reported first.
//
// Which plugins belong here is not a judgement this constructor is trusted to
// make alone. TestOracleCoversEveryDefaultFilterPlugin derives the default
// Filter set from the pinned release and fails on any member built neither
// here nor into its exemption map. The seven built here are the ones that
// decide from the pod and the node alone; the rest want a scheduler Handle, a
// snapshot of the whole cluster, or a DRA manager, and are exempted there
// against the internal/fit refusal that keeps binpack away from their inputs.
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
// had that feature on by default since 1.33.
//
// The lesson generalises past sidecars. A hand-maintained gate list is a set
// of guesses about someone else's defaults, and a wrong guess here does not
// fail loudly — it produces confident, wrong disagreement reports. Deriving
// them means the oracle tracks the pinned Kubernetes release by construction.
func NewOracle() (*Oracle, error) {
	ctx := context.Background()
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

	o := &Oracle{}

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

	p, err := nodeunschedulable.New(ctx, nil, nil, features)
	if err := add(nodeunschedulable.Name, p, err); err != nil {
		return nil, err
	}

	// NodeName is inert for anything binpack would relocate — a pod naming a
	// node bypasses the scheduler, and fit refuses those outright — so it
	// costs nothing and makes that refusal falsifiable rather than assumed.
	p, err = nodename.New(ctx, nil, nil, features)
	if err := add(nodename.Name, p, err); err != nil {
		return nil, err
	}

	p, err = tainttoleration.New(ctx, nil, nil, features)
	if err := add(tainttoleration.Name, p, err); err != nil {
		return nil, err
	}

	p, err = nodeaffinity.New(ctx, &config.NodeAffinityArgs{}, nil, features)
	if err := add(nodeaffinity.Name, p, err); err != nil {
		return nil, err
	}

	p, err = nodeports.New(ctx, nil, nil, features)
	if err := add(nodeports.Name, p, err); err != nil {
		return nil, err
	}

	p, err = noderesources.NewFit(ctx, &config.NodeResourcesFitArgs{
		ScoringStrategy: &config.ScoringStrategy{Type: config.LeastAllocated},
	}, nil, features)
	if err := add(noderesources.Name, p, err); err != nil {
		return nil, err
	}

	// Beta and on by default since 1.36. It rejects a node whose kubelet has
	// not declared a feature the pod's spec implies — new at this release,
	// and so the demonstration of why the set is derived rather than typed.
	p, err = nodedeclaredfeatures.New(ctx, nil, nil, features)
	if err := add(nodedeclaredfeatures.Name, p, err); err != nil {
		return nil, err
	}

	return o, nil
}

// Accepts reports whether the scheduler would allow pod onto node, given the
// pods already there.
//
// Capacity is derived the way the scheduler derives it — from the node's
// allocatable minus the residents' requests — rather than being supplied, so
// the oracle shares no arithmetic with the code under test.
func (o *Oracle) Accepts(pod *corev1.Pod, node *corev1.Node, residents []*corev1.Pod) (Verdict, error) {
	ctx := context.Background()

	nodeInfo := framework.NewNodeInfo(residents...)
	nodeInfo.SetNode(node)

	state := framework.NewCycleState()
	nodes := []fwk.NodeInfo{nodeInfo}

	for _, p := range o.plugins {
		if p.pre != nil {
			_, status := p.pre.PreFilter(ctx, state, pod, nodes)
			if status.IsSkip() {
				continue
			}
			if !status.IsSuccess() {
				return Verdict{Plugin: p.name, Message: status.Message()}, nil
			}
		}
		if status := p.filter.Filter(ctx, state, pod, nodeInfo); !status.IsSuccess() {
			return Verdict{Plugin: p.name, Message: status.Message()}, nil
		}
	}

	return Verdict{Accepted: true}, nil
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
