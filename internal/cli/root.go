// Package cli implements binpack's command-line interface.
//
// Commands here are thin frontends. Decision logic belongs in internal/engine,
// and anything holding a Kubernetes client belongs in internal/collect.
package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// ExitFindings is the status `binpack diagnose` exits with when a report
// crosses its --fail-on threshold.
//
// Deliberately not 1. A CI job needs to tell "diagnose ran and your cluster has
// blockers" from "diagnose could not reach the cluster", and 1 already means
// the latter for every other command.
const ExitFindings = 2

// ExitError is a command reporting a result through its exit status rather
// than failing. It is not an error in the usual sense: the command did what it
// was asked, and the status is the answer.
type ExitError struct {
	Code    int
	Message string
}

func (e *ExitError) Error() string { return e.Message }

// ExitCodeFor is the status a failed Execute should exit with. Anything that
// is not an [ExitError] is an ordinary failure, and stays 1.
func ExitCodeFor(err error) int {
	var exit *ExitError
	if errors.As(err, &exit) {
		return exit.Code
	}
	return 1
}

// outputFormat selects how a command renders its result.
type outputFormat string

const (
	outputText outputFormat = "text"
	outputJSON outputFormat = "json"
)

func (o outputFormat) valid() bool {
	return o == outputText || o == outputJSON
}

// options holds flags shared by every command.
type options struct {
	output outputFormat
	out    io.Writer

	// configSource names where the configuration came from, and is reported
	// with every verdict. A command that answers about a binpack configured
	// differently from the one running is answering a different question, and
	// nothing else in the output reveals which one it answered.
	configSource string
}

// NewRootCommand builds the command tree. out receives normal output, which
// lets tests capture it without touching os.Stdout.
func NewRootCommand(out io.Writer) *cobra.Command {
	opts := &options{output: outputText, out: out}

	cmd := &cobra.Command{
		Use:   "binpack",
		Short: "Consolidate nodes a managed autoscaler will not",
		Long: "binpack drains a node only when it can show that every one of that node's pods\n" +
			"would be schedulable elsewhere, then lets the cluster-autoscaler reap it.",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			if !opts.output.valid() {
				return fmt.Errorf("invalid --output %q: want %q or %q", opts.output, outputText, outputJSON)
			}
			return nil
		},
	}

	cmd.SetOut(out)
	cmd.PersistentFlags().StringVar((*string)(&opts.output), "output", string(outputText),
		"output format: text or json")

	cmd.AddCommand(
		newConfigCommand(opts),
		newDiagnoseCommand(opts),
		newExplainCommand(opts),
		newRunCommand(opts),
		newVersionCommand(opts),
	)

	return cmd
}
