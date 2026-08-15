// Package cli implements binpack's command-line interface.
//
// Commands here are thin frontends. Decision logic belongs in internal/engine,
// and anything holding a Kubernetes client belongs in internal/collect.
package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

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
		newExplainCommand(opts),
		newVersionCommand(opts),
	)

	return cmd
}
