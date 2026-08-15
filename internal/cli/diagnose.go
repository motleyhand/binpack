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

func newDiagnoseCommand(opts *options) *cobra.Command {
	var (
		path        string
		kubeconfig  string
		kubecontext string
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
			"reconcile the change away within minutes.",
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
			// Validated even though diagnose reads only the default policy, so
			// that a typo is not accepted here and rejected by explain.
			if err := checkPoolsExist(cfg, snapshot); err != nil {
				return err
			}

			return renderDiagnose(opts, engine.Diagnose(snapshot, engineConfig(cfg)))
		},
	}

	cmd.Flags().StringVarP(&path, "file", "f", "", "configuration file (defaults apply when absent)")
	cmd.Flags().StringVar(&kubeconfig, "kubeconfig", "", "path to a kubeconfig (defaults to the usual rules)")
	cmd.Flags().StringVar(&kubecontext, "context", "", "kubeconfig context to use")

	return cmd
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

	if len(findings) == 0 {
		p("nothing found: no configuration in this cluster is stopping a node being removed.\n\n")
		p("that does not mean the cluster is as small as it could be — run `binpack explain`\n")
		p("to see whether any node's workload would fit elsewhere.\n")
		return errors.Join(errs...)
	}

	var blocking, warning int
	for _, f := range findings {
		switch f.Severity {
		case engine.Blocking:
			blocking++
		case engine.Warning:
			warning++
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
	if blocking > 0 {
		p("\nblocking findings are the eviction API refusing outright: no setting of binpack's\n")
		p("or the autoscaler's changes them. Clearing those may be enough on its own.\n")
	}

	return errors.Join(errs...)
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
