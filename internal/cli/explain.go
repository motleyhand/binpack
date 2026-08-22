package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/motleyhand/binpack/api/v1alpha1"
	"github.com/motleyhand/binpack/internal/collect"
	"github.com/motleyhand/binpack/internal/drain"
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
			cfg, source, err := loadConfigOrDefaults(path, cmd.InOrStdin())
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

			if err := engine.CheckPools(snapshot, engineConfig(cfg)); err != nil {
				return err
			}

			opts.configSource = source
			return renderExplain(opts, snapshot, explainOutcome(snapshot, engineConfig(cfg)))
		},
	}

	cmd.Flags().StringVarP(&path, "file", "f", "", "configuration file (defaults apply when absent)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to a kubeconfig (defaults to the usual rules)")
	cmd.Flags().StringVar(&kubecontext, "context", "", "kubeconfig context to use")

	return cmd
}

// explainOutcome is everything explain reports: the decision, and — separately
// — the drain already under way, if there is one.
//
// Separately, because they answer different questions and one of them can be
// unavailable while the other is not. [engine.Decide] returns before assessing
// anything when there is no live cluster-autoscaler, which is precisely the
// condition that makes `run` hand a drain back on its next tick — so a drain
// report derived from the decision's code would go missing in the failure an
// operator is most likely to be investigating. This one is derived from the
// marker on the node, which is where the drain actually lives.
func explainOutcome(s engine.Snapshot, cfg engine.Config) explainOutput {
	out := explainOutput{Decision: engine.Decide(s, cfg)}

	name := engine.Marked(s)
	if name == "" {
		return out
	}

	// The same pair the controller reports for a drain it is not advancing,
	// through the same function, so a dry run's log line and this command
	// cannot describe one node two ways. Revalidation says whether the node
	// could still be emptied; the bound says whether this drain is getting
	// anywhere. Neither answers alone.
	a := engine.Revalidate(s, name, cfg)
	out.Drain = &drainReport{
		Node: name,
		WouldHappen: drain.WouldHappen(a, drain.Assess(
			drain.State{Node: a.Node, Pods: drain.PodsOn(s, name), Now: s.Now},
			drain.PolicyFor(cfg, s, name))),
	}

	// The marked node's own row, replaced. Selection assessed it as a node
	// binpack might newly pick, so it reports "a drain is already in progress
	// on this node" — true, and useless to somebody watching a cordoned node.
	// Revalidate asks what actually governs it: the call executor.Advance makes
	// before every eviction, with binpack's own marker and cordon ignored and
	// the reserve suppressed once pods have moved.
	//
	// Substituted here rather than inside Decide, deliberately. Those
	// assessments are also what the metrics count, and `drain-in-progress` is a
	// published binpack_nodes_skipped code: swapping the row in the engine would
	// leave it unreachable whenever there is a single marker, which is the
	// ordinary case, and would count a node already being drained towards
	// binpack_drainable_nodes.
	for i := range out.Decision.Assessments {
		if out.Decision.Assessments[i].Node.Name == name {
			out.Decision.Assessments[i] = a
		}
	}
	return out
}

// explainOutput is what the renderers are given.
type explainOutput struct {
	Decision engine.Decision
	// Drain is the drain already under way, or nil. Never derived from the
	// decision: see [explainOutcome].
	Drain *drainReport
}

// drainReport describes a drain in progress, in the words the controller uses
// for the same one.
type drainReport struct {
	Node string `json:"node"`
	// WouldHappen is what revalidation and the drain's own bound observed. Not
	// a prediction of how the drain ends — see [drain.WouldHappen].
	WouldHappen string `json:"wouldHappen"`
}

// restConfigFor resolves a connection from the usual kubeconfig rules, so the
// read-only commands work wherever kubectl does.
func restConfigFor(kubeconfig, kubecontext string) (*rest.Config, error) {
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
	return restCfg, nil
}

// clientFor builds a direct, uncached reader.
//
// Direct on purpose: a one-shot command wants one List per resource and no
// watches, where the controller wants the opposite. Both satisfy
// [collect.Reader], so the read path is shared regardless.
func clientFor(kubeconfig, kubecontext string) (collect.Reader, error) {
	restCfg, err := restConfigFor(kubeconfig, kubecontext)
	if err != nil {
		return nil, err
	}
	// The default scheme already carries every built-in type binpack reads.
	return client.New(restCfg, client.Options{})
}

// DeployedConfig is where the Helm chart mounts binpack's configuration.
//
// Used as the default for -f so that `kubectl exec deploy/binpack -- binpack
// explain` answers about the binpack running beside it, rather than about one
// configured with defaults. Those are different questions, and explain exists
// to answer the first.
const DeployedConfig = "/etc/binpack/config.yaml"

// deployedConfig is the path actually consulted, so a test can exercise the
// branch that matters most: the one that makes `kubectl exec ... -- binpack
// explain` answer about the deployed configuration. Without a seam it only
// runs on a machine that happens to have that file, which is no machine any
// test runs on.
var deployedConfig = DeployedConfig

// loadConfigOrDefaults resolves the configuration and says where it came from.
//
// The source is returned rather than inferred by the caller because every
// command reports it. A tool that prints a verdict without saying what it read
// is one whose output cannot be checked — and a configuration that silently
// failed to arrive looks exactly like one that arrived and said nothing.
func loadConfigOrDefaults(
	path string, stdin interface{ Read([]byte) (int, error) },
) (*v1alpha1.Config, string, error) {
	if path == "" {
		// Read first and ask questions after, rather than probing with Stat.
		// Two reasons: a file that exists at the probe and not at the read is
		// a gap this cannot have, and — the one that matters — only "no such
		// file" may fall back. A configuration that is present but unreadable
		// is a broken deployment, and reporting "built-in defaults" for it
		// would put the authority of the reported source behind an answer
		// about settings nobody chose.
		data, err := os.ReadFile(deployedConfig)
		switch {
		case err == nil:
			cfg, err := v1alpha1.Load(data)
			return cfg, deployedConfig, err
		case !errors.Is(err, fs.ErrNotExist):
			return nil, "", fmt.Errorf("reading %s: %w", deployedConfig, err)
		}

		// An empty document is a working configuration: pools and their
		// bounds are discovered, so there is nothing an operator must supply.
		cfg, err := v1alpha1.Load(nil)
		return cfg, "built-in defaults", err
	}

	data, err := readConfigInput(path, stdin)
	if err != nil {
		return nil, "", err
	}
	cfg, err := v1alpha1.Load(data)
	return cfg, path, err
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
		StallTimeout:         p.StallTimeout,
		RemovalTimeout:       p.RemovalTimeout,
		BackoffInitial:       p.BackoffInitial,
		BackoffMax:           p.BackoffMax,
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
	// Config names where the configuration came from. Reported so a verdict
	// can be checked against the settings that produced it — a command run
	// without -f answers about built-in defaults, which is a different
	// question from what the deployed binpack will do.
	Config string `json:"config"`
	Action string `json:"action"`
	// Code names the outcome, from the engine's bounded set.
	Code   string `json:"code"`
	Node   string `json:"node,omitempty"`
	Reason string `json:"reason,omitempty"`
	// Drain is the drain already under way, absent when there is none. It is
	// reported whatever the decision was, including the one that assesses
	// nothing because no autoscaler is running.
	Drain *drainReport `json:"drain,omitempty"`
	Nodes []nodeReport `json:"nodes"`
}

type nodeReport struct {
	Name   string `json:"name"`
	Pool   string `json:"pool,omitempty"`
	Chosen bool   `json:"chosen,omitempty"`
	// Draining marks the node binpack is part-way through emptying. Its
	// verdict is the one revalidation reached, not the one selection would —
	// they are different questions and this says which was asked.
	Draining bool   `json:"draining,omitempty"`
	Verdict  string `json:"verdict"`
	// Code names why a node was skipped, from the engine's bounded set. The
	// detail says it in prose; this is what a consumer can branch on without
	// matching a sentence that may be reworded.
	Code      string   `json:"code,omitempty"`
	Detail    string   `json:"detail,omitempty"`
	Relocates int      `json:"relocates,omitempty"`
	Blockers  []string `json:"blockers,omitempty"`
	// Refusals maps each destination to why it would not take the pod that
	// could not be placed. Without it, "nowhere to go" is unactionable: the
	// useful question is always which wall each node hit.
	Refusals map[string]string `json:"refusals,omitempty"`
}

func renderExplain(opts *options, s engine.Snapshot, out explainOutput) error {
	d := out.Decision
	view := buildView(s, d)
	view.Config = opts.configSource
	view.Drain = out.Drain

	if opts.output == outputJSON {
		enc := json.NewEncoder(opts.out)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}

	return writeExplainText(opts, s, d, view)
}

func buildView(s engine.Snapshot, d engine.Decision) explainView {
	var v explainView
	live, _ := s.Autoscaler.Live(s.Now)
	v.Autoscaler.Running = live
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
	v.Code = d.Code
	v.Reason = d.Reason
	if d.Node != nil {
		v.Node = d.Node.Name
	}

	for _, a := range d.Assessments {
		v.Nodes = append(v.Nodes, reportFor(a, d))
	}
	return v
}

func reportFor(a engine.NodeAssessment, d engine.Decision) nodeReport {
	// The verdict comes from the engine rather than being recomputed here.
	// Two implementations of "what happened to this node" is one more than
	// can be kept in step, and the metrics read the same one.
	r := nodeReport{
		Name: a.Node.Name, Pool: a.Pool, Chosen: a.Chosen,
		Verdict: a.Verdict(), Code: a.SkipCode,
		Draining: d.Code == engine.CodeDraining && d.Node != nil && a.Node.Name == d.Node.Name,
	}

	switch r.Verdict {
	case engine.VerdictSkipped:
		r.Detail = a.SkipReason
	case engine.VerdictBlocked:
		r.Detail = "its workload fits elsewhere, but some pods cannot be evicted"
		for _, b := range a.Blockers {
			r.Blockers = append(r.Blockers, b.Message)
		}
	case engine.VerdictInfeasible:
		if a.Simulation.Blocked != nil {
			r.Detail = a.Simulation.Blocked.Summary
			r.Refusals = a.Simulation.Blocked.PerNode
		}
	default:
		// Why this node is not being drained, which during a drain is a
		// different sentence: nothing was chosen, so "another node was chosen
		// first" would name a choice that was never made.
		switch {
		case r.Draining:
			r.Detail = "its workload still fits elsewhere"
		case d.Code == engine.CodeDraining:
			r.Detail = "it could be drained, but a drain is already in progress"
		case !a.Chosen:
			r.Detail = "another node was chosen first"
		}
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

	// First, because everything below it is conditional on this. A reader who
	// skips it can still check the verdict against the settings; a reader who
	// never sees it cannot.
	if opts.configSource != "" {
		p("config: %s\n\n", opts.configSource)
	}

	// The same liveness check Decide uses, so this line cannot contradict the
	// decision printed below it.
	live, why := s.Autoscaler.Live(s.Now)

	p("cluster-autoscaler: ")
	if !live {
		p("unavailable\n\n")
		p("binpack will not act: %s\n", why)
		// Still said, because this is the condition that ends a drain rather
		// than one that merely stops a new one starting: revalidation refuses
		// to continue through a dead autoscaler, so the next evaluation hands
		// the node back. Returning here reported the dead autoscaler and
		// nothing at all about the cordoned node somebody is looking at.
		if v.Drain != nil {
			p("\n")
			writeDrain(p, v.Drain)
		}
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
		switch {
		case r.Chosen:
			marker = "*"
		case r.Draining:
			marker = ">"
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
	switch {
	case d.Action == engine.Drain:
		p("would drain %s\n", d.Node.Name)
		p("(dry run: explain never changes anything)\n")

	case d.Code == engine.CodeDraining:
		// Narrowed rather than lost. What binpack will do next is advance this
		// drain, and the honest answer to "which node would you pick" is that
		// it is not picking one — so the preview that remains is the node table
		// above, and the row that matters is the one being emptied.
		writeDrain(p, v.Drain)
		p("binpack advances that drain rather than choosing another node; its row is\n")
		p("marked > above.\n")

	default:
		p("nothing to do: %s\n", d.Reason)
	}

	return errors.Join(errs...)
}

// writeDrain says which node is being drained and what the two halves of the
// question observed about it. Nothing when no drain is under way.
//
// It does not say where the node's row is, because one caller prints no node
// table: without a live autoscaler the engine assesses nothing, and this is
// still the moment to say a node is cordoned.
func writeDrain(p func(string, ...any), r *drainReport) {
	if r == nil {
		return
	}
	p("a drain is in progress on %s\n", r.Node)
	p("%s\n", r.WouldHappen)
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
