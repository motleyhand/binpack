package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/motleyhand/binpack/internal/collect"
	"github.com/motleyhand/binpack/internal/engine"
)

// failThreshold is the severity at which `diagnose` starts reporting failure
// through its exit status.
//
// A threshold rather than a set: asking to fail on warnings and pass on
// blockers is not a thing anyone means.
type failThreshold string

const (
	failNever    failThreshold = "never"
	failBlocking failThreshold = "blocking"
	failWarning  failThreshold = "warning"
)

// severity resolves the threshold, and reports whether it fails at all.
//
// There is deliberately no "info" level. Info findings are the cluster working
// as intended — a pool at its minimum, an autoscaler reporting NoCandidates —
// so a job configured that way would be red on a perfectly healthy cluster,
// and a check that is always red is a check nobody reads.
func (f failThreshold) severity() (engine.Severity, bool) {
	switch f {
	case failBlocking:
		return engine.Blocking, true
	case failWarning:
		return engine.Warning, true
	default:
		return engine.Info, false
	}
}

func (f failThreshold) valid() bool {
	return f == failNever || f == failBlocking || f == failWarning
}

func newDiagnoseCommand(opts *options) *cobra.Command {
	var (
		path        string
		kubeconfig  string
		kubecontext string
		failOn      string
		failStatic  bool
	)

	cmd := &cobra.Command{
		Use:   "diagnose",
		Short: "Report what is stopping this cluster from shrinking",
		Long: "Finds the configuration preventing nodes from being removed, and says what to change.\n\n" +
			"Useful whether or not you run binpack: most of what blocks consolidation blocks the\n" +
			"cluster-autoscaler too, and several of these conditions are invisible from any single\n" +
			"object — a disruption budget permitting zero disruptions looks perfectly healthy.\n\n" +
			"Read-only. diagnose suggests changes and never makes them, both because a cost tool\n" +
			"should not quietly rewrite availability policy and because Flux or Argo would\n" +
			"reconcile the change away within minutes.\n\n" +
			"Exits 0 by default whatever it finds. Pass --fail-on to use it as a CI gate:\n" +
			"a report at or above the threshold exits 2, while 1 stays reserved for diagnose\n" +
			"failing to run at all — a job needs to tell those apart.",
		Args: cobra.NoArgs,
		PreRunE: func(_ *cobra.Command, _ []string) error {
			// Checked before the cluster is contacted, so a typo fails in
			// milliseconds rather than after a full read.
			if !failThreshold(failOn).valid() {
				return fmt.Errorf("invalid --fail-on %q: want %q, %q or %q",
					failOn, failNever, failBlocking, failWarning)
			}
			return nil
		},
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
			// Validated even though diagnose reads only the default policy, so
			// that a typo is not accepted here and rejected by explain.
			if err := engine.CheckPools(snapshot, engineConfig(cfg)); err != nil {
				return err
			}

			opts.configSource = source
			findings := engine.Diagnose(snapshot, engineConfig(cfg))
			if err := renderDiagnose(opts, findings); err != nil {
				return err
			}
			return exitFor(findings, failThreshold(failOn), failStatic)
		},
	}

	cmd.Flags().StringVarP(&path, "file", "f", "", "configuration file (defaults apply when absent)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to a kubeconfig (defaults to the usual rules)")
	cmd.Flags().StringVar(&kubecontext, "context", "", "kubeconfig context to use")
	cmd.Flags().StringVar(&failOn, "fail-on", string(failNever),
		"exit non-zero when findings reach this severity: never, blocking or warning")
	cmd.Flags().BoolVar(&failStatic, "fail-on-static-pools", false,
		"also count findings on pools the cluster-autoscaler does not manage, "+
			"which cannot free a node today and are excluded from --fail-on by default")

	return cmd
}

// exitFor decides whether a report should fail the command.
//
// Findings on pools nothing will ever remove are excluded by default. They are
// real, and they go live the moment that pool autoscales — but a gate that
// fails today over a node that was never going away is one a team turns off.
func exitFor(findings []engine.Finding, threshold failThreshold, countStatic bool) error {
	minimum, fails := threshold.severity()
	if !fails {
		return nil
	}

	var counted, excluded int
	for _, f := range findings {
		if f.Severity < minimum {
			continue
		}
		if f.FreesNothing && !countStatic {
			excluded++
			continue
		}
		counted++
	}
	if counted == 0 {
		return nil
	}

	message := fmt.Sprintf("%d finding(s) at or above %s", counted, minimum)
	if excluded > 0 {
		// Stated, so a count that disagrees with the report above it is
		// explained rather than merely puzzling.
		message += fmt.Sprintf(" (%d more on pools that are not autoscaled were not counted; "+
			"pass --fail-on-static-pools to include them)", excluded)
	}
	return &ExitError{Code: ExitFindings, Message: message}
}

// findingView is the machine-readable rendering. Codes are the stable part of
// the contract; the prose around them may be reworded.
type findingView struct {
	Severity string `json:"severity"`
	Code     string `json:"code"`
	Subject  string `json:"subject"`
	Detail   string `json:"detail,omitempty"`
	Summary  string `json:"summary"`
	Fix      string `json:"fix,omitempty"`
	// FreesNothing marks a finding that is real but would not shrink the
	// cluster today, its subject sitting only on nodes nothing will remove.
	FreesNothing bool `json:"freesNothing,omitempty"`
}

func renderDiagnose(opts *options, findings []engine.Finding) error {
	if opts.output == outputJSON {
		// One flat record per finding, repeating the shared prose: a JSON
		// consumer filters and joins for itself, and a grouped document would
		// make the common case — "give me every blocking finding" — harder.
		// An empty slice rather than nil, so a healthy cluster yields [] and
		// needs no special handling downstream.
		views := make([]findingView, 0, len(findings))
		for _, f := range findings {
			views = append(views, findingView{
				Severity:     f.Severity.String(),
				Code:         f.Code,
				Subject:      f.Subject,
				Detail:       f.Detail,
				Summary:      f.Summary,
				Fix:          f.Fix,
				FreesNothing: f.FreesNothing,
			})
		}
		enc := json.NewEncoder(opts.out)
		enc.SetIndent("", "  ")
		return enc.Encode(views)
	}

	return renderDiagnoseText(opts, findings)
}

func renderDiagnoseText(opts *options, findings []engine.Finding) error {
	var errs []error
	p := func(format string, args ...any) {
		if _, err := fmt.Fprintf(opts.out, format, args...); err != nil {
			errs = append(errs, err)
		}
	}

	// Text only. diagnose's JSON is a flat array of findings, documented as
	// such, and wrapping it in an object to carry one string would break every
	// consumer for a field they can determine from how they invoked it. explain
	// reports it in both because its JSON is already a document.
	if opts.configSource != "" {
		p("config: %s\n\n", opts.configSource)
	}

	if len(findings) == 0 {
		p("nothing found: no configuration in this cluster is stopping a node being removed.\n\n")
		p("that does not mean the cluster is as small as it could be — run `binpack explain`\n")
		p("to see whether any node's workload would fit elsewhere.\n")
		return errors.Join(errs...)
	}

	var blocking, warning, spokenFor int
	var noAutoscaler bool
	for _, f := range findings {
		switch f.Severity {
		case engine.Blocking:
			blocking++
			if classFooterSpeaksFor(f.Code) {
				spokenFor++
			}
		case engine.Warning:
			warning++
		}
		if f.Code == engine.FindingNoAutoscaler {
			noAutoscaler = true
		}
	}

	// Findings arrive grouped by severity then code, so one pass reports each
	// diagnosis once and lists its subjects underneath. Fifteen namespaces
	// deploying one Helm chart is one mistake, not fifteen.
	for _, group := range groupByCode(findings) {
		first := group[0]
		p("%s · %s", first.Severity, first.Code)
		if len(group) > 1 {
			p(" (%d)", len(group))
		}
		p("\n  %s\n", first.Summary)
		if first.Fix != "" {
			p("  fix: %s\n", first.Fix)
		}
		// Said once when it applies throughout, rather than once per line: the
		// caveat is about the whole group, and repeating it is the noise this
		// report exists to remove.
		if allFreeNothing(group) {
			p("  note: none of these would free a node today — their pools are not autoscaled.\n")
		}
		p("\n")

		width := subjectWidth(group)
		for _, f := range group {
			p("    %-*s", width, f.Subject)
			if f.Detail != "" {
				p("  %s", f.Detail)
			}
			if f.FreesNothing && !allFreeNothing(group) {
				p("  (frees nothing today)")
			}
			p("\n")
		}
		p("\n")
	}

	p("%d blocking, %d warning, %d informational\n",
		blocking, warning, len(findings)-blocking-warning)
	// Counted together above, because the counts line and --fail-on are about
	// severity; closed over separately here, because the two closing lines are
	// about what a finding is.
	if spokenFor > 0 {
		p("\n" + blockingClassFooter)
	}
	if noAutoscaler {
		p("\n" + noAutoscalerFooter)
	}

	return errors.Join(errs...)
}

// blockingClassFooter is the report's closing sentence about the blocking
// findings in it.
//
// It used to say the eviction API was refusing and that no setting changed the
// outcome, which was false of half the codes the command can print and printed
// a few lines under the fixes that disprove it. The eviction API deletes a bare
// pod without complaint — binpack and the autoscaler decline by their own
// policy — and `kubectl drain --force` clears one. What the class does have in
// common is where the answer is: on the object.
const blockingClassFooter = "blocking findings hold a node open until the object itself changes — a budget\n" +
	"rewritten, a controller added, a manifest removed. Clearing them may be enough on\n" +
	"its own.\n"

// noAutoscalerFooter is the closing line for the one blocking code the
// sentence above is not about.
//
// Every claim here is conditioned on something the reader can check, because
// binpack cannot check it: this finding comes from the autoscaler's own status
// ConfigMap, and an autoscaler running with status reporting turned off is
// indistinguishable from none at all. A first version said flatly that acting
// on the rest of the report frees no node until this clears, which is false in
// exactly that case — the budget above gets fixed and the running autoscaler
// removes the node — so the sentence sent operators past a remedy that works,
// under a caveat two lines later saying it might. The condition and the caveat
// have to be the same sentence.
const noAutoscalerFooter = "no-autoscaler is the exception: it is not about an object, and binpack learns it\n" +
	"only from the cluster-autoscaler's own status ConfigMap — an autoscaler running\n" +
	"with status reporting turned off looks the same from here. Check which you have\n" +
	"before deciding what the rest of this report is worth: if one is running, fixing\n" +
	"a blocker above can still free a node, and if none is, nothing above will until\n" +
	"one does.\n"

// classFooterSpeaksFor reports whether the sentence above is a sentence about
// this code.
//
// no-autoscaler is blocking — nothing removes a node without an autoscaler,
// which is as blocking as it gets — but it is a precondition rather than
// anything true of an object, and its remedy is a cluster setting. It gets its
// own closing line rather than the class definition being stretched until it
// covers both, which would leave the definition describing neither.
func classFooterSpeaksFor(code string) bool {
	return code != engine.FindingNoAutoscaler
}

func allFreeNothing(group []engine.Finding) bool {
	for _, f := range group {
		if !f.FreesNothing {
			return false
		}
	}
	return true
}

// groupByCode splits a severity-then-code ordered slice into runs of one code.
func groupByCode(findings []engine.Finding) [][]engine.Finding {
	var groups [][]engine.Finding
	for i, f := range findings {
		if i > 0 && findings[i-1].Code == f.Code {
			groups[len(groups)-1] = append(groups[len(groups)-1], f)
			continue
		}
		groups = append(groups, []engine.Finding{f})
	}
	return groups
}

// subjectWidth aligns details into a column, capped so that one very long
// namespace does not push every other line off the screen.
func subjectWidth(group []engine.Finding) int {
	const max = 52
	width := 0
	for _, f := range group {
		if n := len(f.Subject); n > width && n <= max {
			width = n
		}
	}
	return width
}
