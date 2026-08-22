package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
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
			"decision function the controller runs, and says where it cannot see what the\n" +
			"controller sees.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, source, err := loadConfigOrDefaults(path, cmd.InOrStdin())
			if err != nil {
				return err
			}

			client, err := readerFor(kubeconfig, kubecontext)
			if err != nil {
				return err
			}

			snapshot, err := collect.Snapshot(cmd.Context(), client, time.Now(),
				statusRef(cfg))
			if err != nil {
				return err
			}

			// One resolution, and everything below decides against it. How
			// nodes join to pools is worked out from the cluster, so the
			// configuration alone describes a different — smaller — set of
			// pools than the one preflight just validated.
			resolved, err := engine.ResolvePools(snapshot, engineConfig(cfg))
			if err != nil {
				return err
			}

			opts.configSource = source
			opts.dryRun = cfg.Settings().DryRun
			opts.autoscalerStatus = statusRef(cfg)
			return renderExplain(opts, snapshot, explainOutcome(snapshot, resolved))
		},
	}

	cmd.Flags().StringVarP(&path, "file", "f", "", "configuration file (defaults apply when absent)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to a kubeconfig (defaults to the usual rules)")
	cmd.Flags().StringVar(&kubecontext, "context", "", "kubeconfig context to use")

	return cmd
}

// explainOutcome is everything explain reports: the decision, the nodes it was
// made about, the drain already under way if there is one, and what explain
// could not evaluate.
//
// Separately, because they answer different questions and one of them can be
// unavailable while the other is not. [engine.Decide] returns before assessing
// anything when there is no live cluster-autoscaler, which is precisely the
// condition that makes `run` hand a drain back on its next tick — so a drain
// report derived from the decision's code would go missing in the failure an
// operator is most likely to be investigating. This one is derived from the
// marker on the node, which is where the drain actually lives.
func explainOutcome(s engine.Snapshot, cfg engine.Config) explainOutput {
	out := explainOutput{Decision: engine.Decide(s, cfg), Config: cfg}
	out.Nodes = out.Decision.Assessments

	// Decide refuses before assessing anything when no autoscaler is running,
	// and that emptiness is right where it is — see [engine.Assess], which
	// says what depends on it. It is wrong here: a reader pointing binpack at
	// a cluster without an autoscaler got three sentences and no sign that it
	// had read their cluster at all, which is what a broken binary looks like.
	// The refusal is still the headline; what changes is that the arithmetic
	// underneath it survives.
	if engine.PreflightRefused(out.Decision.Code) {
		out.Nodes = engine.Assess(s, cfg)
	}

	// A control that governs the deployed binpack and that this process has no
	// input for. Snapshot.LastDrain is filled in by the controller from its own
	// memory — a completed drain deletes the node that would have recorded it —
	// so the engine reads the zero value here and the cooldown branch never
	// opens. Answering as though no drain had happened is what `run --once`
	// refuses to start rather than do, on a strictly weaker version of the same
	// condition, and explain has to at least say so.
	if where, d, set := engine.CooldownAfterDrain(cfg); set {
		out.NotEvaluated = append(out.NotEvaluated, fmt.Sprintf(
			"cooldown.afterDrain (%s sets %s) is not visible to explain: %s",
			where, d, engine.NoDrainToMeasureFrom))
	}

	// And the other reading that depends on memory this process has not got.
	// Reported only where it actually decided something: cooldown.afterScaleUp
	// is set by default, so a note on every run would be noise, and the
	// divergence only exists when the cooldown ruled a node out.
	if slices.ContainsFunc(out.Nodes, func(a engine.NodeAssessment) bool {
		return a.SkipCode == engine.SkipCooldownAfterScaleUp
	}) {
		out.NotEvaluated = append(out.NotEvaluated, fmt.Sprintf(
			"cooldown.afterScaleUp is read differently by explain: %s",
			engine.NoAutoscalerHistory))
	}

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
			drain.StateFor(s, a.Node), drain.PolicyFor(cfg, s, name))),
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
	for i := range out.Nodes {
		if out.Nodes[i].Node.Name == name {
			out.Nodes[i] = a
		}
	}
	return out
}

// explainOutput is what the renderers are given.
type explainOutput struct {
	Decision engine.Decision
	// Config is the configuration the decision was made under, resolved
	// against this cluster. Carried rather than re-derived because the join
	// between nodes and pools is part of it and is worked out per snapshot:
	// the report has to name the configuration that produced the verdict, not
	// the one the file describes.
	Config engine.Config
	// Nodes is what explain says about each node. Normally the decision's own
	// assessments; when [engine.Decide] returned before making any, what the
	// same pass reports on its own — see [explainOutcome].
	Nodes []engine.NodeAssessment
	// Drain is the drain already under way, or nil. Never derived from the
	// decision: see [explainOutcome].
	Drain *drainReport
	// NotEvaluated names controls explain cannot evaluate, in the sentences it
	// prints for them.
	NotEvaluated []string
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
	if clientcmd.IsEmptyConfig(err) {
		// Replaced rather than wrapped, and this one only. client-go's
		// ErrEmptyConfig suggests KUBERNETES_MASTER, which predates KUBECONFIG
		// and which these loading rules never consult: the overrides above
		// carry no ClusterDefaults, so setting it produces a byte-identical
		// error. That is the first thing a stranger sees, and it sends them
		// somewhere that cannot work — leaving them no way to tell a missing
		// kubeconfig from a broken binary.
		//
		// Every other failure here reads well already (an unreachable server
		// names the address it tried), so they keep the upstream text: one
		// sentence about kubeconfigs in place of all of them would lose the
		// diagnosis rather than improve it.
		return nil, errors.New(
			"no kubeconfig found: set KUBECONFIG, or pass --kubeconfig <path> and --context <name>")
	}
	if err != nil {
		return nil, fmt.Errorf("building a Kubernetes client: %w", err)
	}
	return restCfg, nil
}

// readerFor is how a command reaches the cluster, as a variable so a test can
// run the command rather than only its renderer.
//
// The wiring between the configuration and the report has no other cover:
// which settings reach the output — and so which binpack the answer is about —
// is decided in RunE and nowhere else, and every renderer test starts from an
// options value it built itself. Since the join between nodes and pools is
// resolved there too, that wiring now decides which nodes the answer is about
// as well, which is why `diagnose` reads through this rather than calling
// [clientFor] itself.
var readerFor = clientFor

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
		NodeGroups:       cfg.NodeGroupJoin(),
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
	// Pools is how nodes were joined to those pools. Reported as data because
	// it decides *scope* — which nodes binpack considers its own — and on most
	// clusters binpack now works that out rather than being told it. A
	// consumer that cannot tell a derived scope from a configured one cannot
	// tell a change of scope from a change of cluster.
	Pools poolsView `json:"pools"`
	// Config names where the configuration came from. Reported so a verdict
	// can be checked against the settings that produced it — a command run
	// without -f answers about built-in defaults, which is a different
	// question from what the deployed binpack will do.
	Config string `json:"config"`
	// DryRun is that configuration's mode. The verdict below is a prediction
	// under one value and a plan under the other, and this is the only thing
	// in the report that says which.
	DryRun bool `json:"dryRun"`
	// NotEvaluated names controls that govern the deployed binpack and that
	// explain cannot evaluate, in the sentences it prints for them. A preview
	// that quietly answers as though a control were unset is a preview of a
	// different binpack.
	NotEvaluated []string `json:"notEvaluated,omitempty"`
	Action       string   `json:"action"`
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

// poolsView is the join, for machines.
type poolsView struct {
	// Source is the MappingSource sentence: short, and the thing to branch on.
	Source string `json:"source"`
	// Label is the node label the join reads.
	Label string `json:"label"`
	// Groups translates that label's values into published identifiers, and is
	// absent where the value is the identifier — there is nothing to say.
	Groups map[string]string `json:"groups,omitempty"`
	// AlsoAgreed are the other labels that would have produced the same join.
	// On a single-pool cluster there are usually several, and an operator who
	// cannot see that the choice was forced reads it as arbitrary.
	AlsoAgreed []string `json:"alsoAgreed,omitempty"`
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
	// Unmodelled marks a refusal binpack made because it could not read what
	// the replacement would request, rather than because the cluster is full.
	// Exactly the set binpack_nodes_unmodelled counts, and named the same way,
	// because the how-to guide sends a reader here to look for the word.
	Unmodelled bool `json:"unmodelled,omitempty"`
	// Refusals maps each destination to why it would not take the pod that
	// could not be placed. Without it, "nowhere to go" is unactionable: the
	// useful question is always which wall each node hit.
	Refusals map[string]string `json:"refusals,omitempty"`
}

func renderExplain(opts *options, s engine.Snapshot, out explainOutput) error {
	d := out.Decision
	view := buildView(s, out)
	view.Config = opts.configSource
	view.DryRun = opts.dryRun
	view.Drain = out.Drain
	view.NotEvaluated = out.NotEvaluated

	if opts.output == outputJSON {
		enc := json.NewEncoder(opts.out)
		enc.SetIndent("", "  ")
		return enc.Encode(view)
	}

	return writeExplainText(opts, s, d, view)
}

func buildView(s engine.Snapshot, out explainOutput) explainView {
	d := out.Decision
	var v explainView
	live, _, _ := s.Autoscaler.Live(s.Now)
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

	v.Pools = poolsView{
		Source:     out.Config.Mapping.Source.String(),
		Label:      out.Config.Mapping.Key,
		Groups:     out.Config.Mapping.Groups,
		AlsoAgreed: out.Config.Mapping.AlsoAgreed,
	}
	if v.Pools.Label == "" {
		v.Pools.Label = out.Config.NodeGroupIDLabel
	}

	v.Action = d.Action.String()
	v.Code = d.Code
	v.Reason = d.Reason
	if d.Node != nil {
		v.Node = d.Node.Name
	}

	for _, a := range out.Nodes {
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
			r.Unmodelled = a.Simulation.Blocked.NoTemplate
		}
	default:
		// Why this node is not being drained, which depends on what the
		// evaluation concluded rather than on the node. During a drain
		// nothing was chosen, so "another node was chosen first" would name
		// a choice that was never made.
		switch {
		case r.Draining:
			r.Detail = "its workload still fits elsewhere"
		case d.Code == engine.CodeDraining:
			r.Detail = "it could be drained, but a drain is already in progress"
		case a.Chosen:
			// The row the command exists for, and the only one in the table
			// that used to carry no detail at all — which reads as a
			// rendering fault rather than as a deliberate silence. The Event
			// describing this same decision has always said how many pods
			// move; this is that sentence, from that function.
			if a.Simulation != nil {
				r.Detail = engine.RelocationSummary(len(a.Simulation.Relocated))
			}
		case d.Node != nil:
			r.Detail = "another node was chosen first"
		default:
			// Nothing was chosen and no drain is under way, so the reason is
			// above the choice rather than about this node. The headline says
			// which; saying it again on every row would bury it.
			r.Detail = "it could be drained"
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
		p("config: %s\n", opts.configSource)
	}
	// And the mode with it, because it decides what the verdict below is. A
	// deployed binpack that reports and one that cordons reach the same
	// decision and do entirely different things with it, and this command
	// exists to answer about the deployed one.
	p("dryRun: %t — %s\n", v.DryRun, dryRunMeans(v.DryRun))
	for _, note := range v.NotEvaluated {
		p("%s\n", note)
	}

	// The same liveness check Decide uses, so this line cannot contradict the
	// decision printed below it.
	live, _, why := s.Autoscaler.Live(s.Now)

	p("\ncluster-autoscaler: ")
	if !live {
		p("unavailable")
		// The evidence, named. Everything binpack knows about the autoscaler
		// comes from this one object, and an operator whose autoscaler is
		// demonstrably running needs to see that binpack looked somewhere
		// else — which is far likelier than their autoscaler being gone.
		// Printed only here: on a cluster where the answer is yes, the object
		// binpack read to find that out answers nothing.
		if opts.autoscalerStatus.Namespace != "" {
			p(" — read %s", opts.autoscalerStatus)
		}
		p("\n")
	} else {
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
		writeJoin(p, v.Pools)
	}

	// The answer, above the node table as well as below it. On a cluster where
	// nothing is feasible the table is a row per node plus a refusal list per
	// row, and the one sentence that answers the question was printed after
	// all of it — so `head` showed nothing useful and `less` meant paging past
	// everything to reach it.
	p("\n%s\n", headline(live, why, d))

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
		detail := r.Detail
		// The word the metric and the how-to guide both use, on the line the
		// reader was sent to look at. Not a fifth verdict: that column is a
		// closed set shared with binpack_nodes, and this is a property of the
		// refusal rather than a different outcome.
		if r.Unmodelled {
			detail = "unmodelled: " + detail
		}
		p("%s %-42s %-11s %s\n", marker, name, r.Verdict, detail)
		for _, b := range r.Blockers {
			p("    - %s\n", b)
		}
		for _, line := range refusalLines(r.Refusals, maxRefusalLines) {
			p("    - %s\n", line)
		}
	}

	p("\n")
	writeVerdict(p, live, why, d, v)

	return errors.Join(errs...)
}

// maxRefusalLines is how many lines of per-destination refusal one node's row
// may carry, the last of them a tail saying how many were left out.
//
// Three, and the number comes from arithmetic rather than taste: the report is
// one such list per infeasible node, so at a hundred nodes with nothing left
// to consolidate — the ordinary state of the cluster binpack is pointed at —
// an uncapped list was ten thousand lines with the verdict on the last one.
const maxRefusalLines = 3

// refusalLines renders a node's per-destination refusals, capped.
//
// The tail is explicit and says where the rest are, because a list silently
// truncated to its first entries reads as the whole list — and the destination
// an operator came here about is exactly the one a silent cap would drop.
// `--output json` carries every entry, uncapped, since a machine reading the
// report has no trouble with the volume and no way to ask for more.
func refusalLines(refusals map[string]string, limit int) []string {
	names := sortedKeys(refusals)

	out := make([]string, 0, min(len(names), limit))
	for _, dest := range names {
		if len(names) > limit && len(out) == limit-1 {
			out = append(out, fmt.Sprintf(
				"(and %d more destinations refused; --output json lists them all)",
				len(names)-len(out)))
			break
		}
		out = append(out, dest+": "+refusals[dest])
	}
	return out
}

// dryRunMeans says what the mode does, in the register `binpack config
// validate` already uses for the same setting.
func dryRunMeans(dryRun bool) string {
	if dryRun {
		return "binpack reports what it decides and changes nothing"
	}
	return "binpack acts on what it decides"
}

// headline is the answer in one line. See [writeVerdict], which says it again
// with everything that follows from it.
func headline(live bool, why string, d engine.Decision) string {
	switch {
	case !live:
		return "binpack will not act: " + why
	case d.Action == engine.Drain:
		return "would drain " + d.Node.Name
	case d.Code == engine.CodeDraining:
		return "a drain is in progress on " + d.Node.Name
	default:
		return "nothing to do: " + d.Reason
	}
}

// writeVerdict says what binpack would do, and what the deployed
// configuration will do about it.
func writeVerdict(p func(string, ...any), live bool, why string, d engine.Decision, v explainView) {
	switch {
	case !live:
		p("binpack will not act: %s\n", why)
		// Said rather than left to the node table above, which reads as a
		// preview otherwise. Not "what it would decide if one were running"
		// either, which would be a second wrong answer: the pools come from
		// the same status document, so what binpack could determine without
		// one is exactly what the rows say and no more.
		//
		// Unless there is one. An autoscaler reporting the cluster unhealthy
		// has published its whole status, so the rows are complete and the
		// sentence about determining things without an autoscaler is simply
		// untrue — the refusal is about what that autoscaler is doing, not
		// about what binpack could read.
		if d.Code == engine.CodeAutoscalerUnhealthy {
			p("the table above is binpack's full reading of the cluster; nothing acts on it\n")
			p("until the autoscaler reports the cluster healthy again\n")
		} else {
			p("the table above is what binpack could determine without one\n")
		}
		// Still said, because this is the condition that ends a drain rather
		// than one that merely stops a new one starting: revalidation refuses
		// to continue through a dead autoscaler, so the next evaluation hands
		// the node back.
		if v.Drain != nil {
			p("\n")
			writeDrain(p, v.Drain)
		}

	case d.Action == engine.Drain:
		chosen := ""
		for _, r := range v.Nodes {
			if r.Chosen {
				chosen = r.Detail
			}
		}
		p("would drain %s: %s\n", d.Node.Name, chosen)
		// The subject of every sentence here is the deployed binpack, which is
		// what the parenthesis below used to be read as. "would drain node-a"
		// followed by the words "dry run" is a statement about the deployment
		// to anybody reading at speed, and it was a statement about the
		// reading tool.
		if v.DryRun {
			p("the configuration sets dryRun: true, so binpack will report this decision and not act on it\n")
		} else {
			p("the configuration sets dryRun: false, so binpack will cordon %s and begin evicting\n",
				d.Node.Name)
		}
		p("(explain itself never changes anything)\n")

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
}

// writeDrain says which node is being drained and what the two halves of the
// question observed about it. Nothing when no drain is under way.
//
// It does not say where the node's row is, because a reader may be looking at
// a report whose table says nothing about this node: without a live
// cluster-autoscaler every row is a node ruled out before it was simulated.
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

// writeJoin says how nodes were matched to those pools, and says nothing at
// all where the join is the one the configuration describes.
//
// Which nodes binpack considers its own is the most consequential thing it
// decides before deciding anything, and on most clusters it now works that
// out rather than being told. Printing it every run would bury it; printing
// it only when there is something to disclose is what makes it readable —
// and the derivation refusing prints far more than this, in the preflight
// error, so silence here never means the question went unanswered.
func writeJoin(p func(string, ...any), v poolsView) {
	if len(v.Groups) == 0 {
		return
	}
	p("  pools matched by %s, %s\n", v.Label, v.Source)
	for _, value := range slices.Sorted(maps.Keys(v.Groups)) {
		p("    %s=%s is %s\n", v.Label, value, v.Groups[value])
	}
	if len(v.AlsoAgreed) > 0 {
		// So that the choice of key does not read as arbitrary. On a
		// single-pool cluster several labels usually resolve it identically,
		// and an operator seeing only one named cannot tell whether another
		// would have given a different answer.
		p("    %s also produce this mapping\n", strings.Join(v.AlsoAgreed, ", "))
	}
}
