package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/motleyhand/binpack/api/v1alpha1"
	"github.com/motleyhand/binpack/internal/collect"
	"github.com/motleyhand/binpack/internal/engine"
)

func newExplainCommand(opts *options) *cobra.Command {
	var (
		path        string
		kubeconfig  string
		kubecontext string
	)

	cmd := &cobra.Command{
		Use:   "explain",
		Short: "Show what binpack would do, and the arithmetic behind it",
		Long: "Reads the cluster and prints the decision binpack would reach, together with\n" +
			"its reasoning for every node.\n\n" +
			"Read-only: explain never cordons, evicts or writes anything. It runs the same\n" +
			"decision function the controller runs, so what it prints is what would happen.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfigOrDefaults(path, cmd.InOrStdin())
			if err != nil {
				return err
			}

			client, err := clientFor(kubeconfig, kubecontext)
			if err != nil {
				return err
			}

			snapshot, err := collect.Snapshot(cmd.Context(), client, time.Now())
			if err != nil {
				return err
			}

			decision := engine.Decide(snapshot, engineConfig(cfg))
			return renderExplain(opts, snapshot, decision)
		},
	}

	cmd.Flags().StringVarP(&path, "file", "f", "", "configuration file (defaults apply when absent)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to a kubeconfig (defaults to the usual rules)")
	cmd.Flags().StringVar(&kubecontext, "context", "", "kubeconfig context to use")

	return cmd
}

// clientFor builds a read-only client from the usual kubeconfig rules, so
// `binpack explain` works wherever kubectl does.
func clientFor(kubeconfig, kubecontext string) (kubernetes.Interface, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if kubeconfig != "" {
		rules.ExplicitPath = kubeconfig
	}

	overrides := &clientcmd.ConfigOverrides{}
	if kubecontext != "" {
		overrides.CurrentContext = kubecontext
	}

	restCfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("building a Kubernetes client: %w", err)
	}

	return kubernetes.NewForConfig(restCfg)
}

func loadConfigOrDefaults(path string, stdin interface{ Read([]byte) (int, error) }) (*v1alpha1.Config, error) {
	if path == "" {
		// An empty document is a working configuration: pools and their
		// bounds are discovered, so there is nothing an operator must supply.
		return v1alpha1.Load(nil)
	}
	data, err := readConfigInput(path, stdin)
	if err != nil {
		return nil, err
	}
	return v1alpha1.Load(data)
}

// engineConfig translates the configuration API into what the engine needs.
// The engine never reads configuration itself.
func engineConfig(cfg *v1alpha1.Config) engine.Config {
	out := engine.Config{
		NodeGroupIDLabel: cfg.Discovery.NodeGroupIDLabel,
		PoolNameLabel:    cfg.Discovery.PoolNameLabel,
		Default:          enginePolicy(cfg.PolicyFor()),
		ByPool:           map[string]engine.Policy{},
	}
	for _, pool := range cfg.Pools {
		out.ByPool[pool.Name] = enginePolicy(cfg.PolicyFor(pool.Name))
	}
	return out
}

func enginePolicy(p v1alpha1.PoolPolicy) engine.Policy {
	return engine.Policy{
		Enabled: p.Enabled,
		Sim: engine.SimConfig{
			ExpendablePriorityCutoff: p.ExpendablePriorityCutoff,
			ReserveForLargestPod:     p.ReserveForLargestPod,
		},
		Evict:                engine.DefaultEvictConfig(),
		MaxPodsPerDrain:      p.MaxPodsPerDrain,
		CooldownAfterScaleUp: p.CooldownAfterScaleUp,
		CooldownAfterDrain:   p.CooldownAfterDrain,
		ExcludedNamespaces:   p.ExcludedNamespaces,
	}
}

// explainView is the machine-readable rendering.
type explainView struct {
	Autoscaler struct {
		Running         bool   `json:"running"`
		ScaleDownStatus string `json:"scaleDownStatus,omitempty"`
		Pools           []struct {
			ID    string `json:"id"`
			Min   int    `json:"min"`
			Max   int    `json:"max"`
			Ready int    `json:"ready"`
		} `json:"pools"`
	} `json:"autoscaler"`
	Action string       `json:"action"`
	Node   string       `json:"node,omitempty"`
	Reason string       `json:"reason,omitempty"`
	Nodes  []nodeReport `json:"nodes"`
}

type nodeReport struct {
	Name      string   `json:"name"`
	Pool      string   `json:"pool,omitempty"`
	Chosen    bool     `json:"chosen,omitempty"`
	Verdict   string   `json:"verdict"`
	Detail    string   `json:"detail,omitempty"`
	Relocates int      `json:"relocates,omitempty"`
	Blockers  []string `json:"blockers,omitempty"`
	// Refusals maps each destination to why it would not take the pod that
	// could not be placed. Without it, "nowhere to go" is unactionable: the
	// useful question is always which wall each node hit.
	Refusals map[string]string `json:"refusals,omitempty"`
}

func renderExplain(opts *options, s engine.Snapshot, d engine.Decision) error {
	view := buildView(s, d)

	if opts.output == outputJSON {
		enc := json.NewEncoder(opts.out)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}

	return writeExplainText(opts, s, d, view)
}

func buildView(s engine.Snapshot, d engine.Decision) explainView {
	var v explainView
	v.Autoscaler.Running = s.Autoscaler.Running
	v.Autoscaler.ScaleDownStatus = s.Autoscaler.ScaleDownStatus
	for _, g := range s.Autoscaler.Groups {
		v.Autoscaler.Pools = append(v.Autoscaler.Pools, struct {
			ID    string `json:"id"`
			Min   int    `json:"min"`
			Max   int    `json:"max"`
			Ready int    `json:"ready"`
		}{g.ID, g.MinSize, g.MaxSize, g.Ready})
	}

	v.Action = d.Action.String()
	v.Reason = d.Reason
	if d.Node != nil {
		v.Node = d.Node.Name
	}

	for _, a := range d.Assessments {
		v.Nodes = append(v.Nodes, reportFor(a))
	}
	return v
}

func reportFor(a engine.NodeAssessment) nodeReport {
	r := nodeReport{Name: a.Node.Name, Pool: a.Pool, Chosen: a.Chosen}

	switch {
	case a.Skipped:
		r.Verdict, r.Detail = "skipped", a.SkipReason
	case len(a.Blockers) > 0:
		r.Verdict = "blocked"
		r.Detail = "its workload fits elsewhere, but some pods cannot be evicted"
		for _, b := range a.Blockers {
			r.Blockers = append(r.Blockers, b.Message)
		}
	case a.Simulation != nil && !a.Simulation.Feasible:
		r.Verdict = "infeasible"
		if a.Simulation.Blocked != nil {
			r.Detail = a.Simulation.Blocked.Summary
			r.Refusals = a.Simulation.Blocked.PerNode
		}
	case a.Chosen:
		r.Verdict = "drainable"
		r.Relocates = len(a.Simulation.Relocated)
	default:
		r.Verdict = "drainable"
		r.Detail = "another node was chosen first"
		if a.Simulation != nil {
			r.Relocates = len(a.Simulation.Relocated)
		}
	}
	return r
}

func writeExplainText(opts *options, s engine.Snapshot, d engine.Decision, v explainView) error {
	var errs []error
	p := func(format string, args ...any) {
		if _, err := fmt.Fprintf(opts.out, format, args...); err != nil {
			errs = append(errs, err)
		}
	}

	p("cluster-autoscaler: ")
	if !s.Autoscaler.Running {
		p("not found\n\n")
		p("binpack will not act: a drained node would never be removed.\n")
		return errors.Join(errs...)
	}
	p("running")
	if s.Autoscaler.ScaleDownStatus != "" {
		// NoCandidates is the autoscaler stating binpack's reason for
		// existing in its own words.
		p(", scale-down: %s", s.Autoscaler.ScaleDownStatus)
	}
	p("\n")

	for _, pool := range v.Autoscaler.Pools {
		p("  pool %s: %d ready (min %d, max %d)\n", pool.ID, pool.Ready, pool.Min, pool.Max)
	}
	if len(v.Autoscaler.Pools) == 0 {
		p("  no autoscaling pools reported\n")
	}

	p("\nnodes\n")
	for _, r := range v.Nodes {
		marker := " "
		if r.Chosen {
			marker = "*"
		}
		name := r.Name
		if r.Pool != "" {
			name = fmt.Sprintf("%s (%s)", r.Name, r.Pool)
		}
		p("%s %-42s %-11s %s\n", marker, name, r.Verdict, r.Detail)
		for _, b := range r.Blockers {
			p("    - %s\n", b)
		}
		for _, dest := range sortedKeys(r.Refusals) {
			p("    - %s: %s\n", dest, r.Refusals[dest])
		}
	}

	p("\n")
	if d.Action == engine.Drain {
		p("would drain %s\n", d.Node.Name)
		p("(dry run: explain never changes anything)\n")
	} else {
		p("nothing to do: %s\n", d.Reason)
	}

	return errors.Join(errs...)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
