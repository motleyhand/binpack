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
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/motleyhand/binpack/api/v1alpha1"
	"github.com/motleyhand/binpack/internal/collect"
)

// Execute runs binpack under a context a signal cancels.
//
// Here rather than in main because the context is the whole of binpack's
// graceful shutdown, and main is the one place nothing can test. ADR-0002 chose
// controller-runtime partly so that a drain in progress is not abandoned
// mid-eviction, and the manager does deliver that — but only once something
// cancels the context it was started with. Executing without one left that
// unreachable rather than merely unused: cobra fills in context.Background,
// whose Done channel is nil, so the manager's stop procedure can never be
// selected, LeaderElectionReleaseOnCancel was dead configuration, and every
// rolling update made the next leader wait out the whole lease rather than take
// a released one.
//
// controller-runtime's handler rather than signal.NotifyContext, for what it
// does with a second signal: it exits immediately, so an operator whose
// shutdown is taking longer than they expected can still stop it with a second
// ^C. NotifyContext would swallow that one too. The read-only commands are
// unaffected either way — they finish long before a signal matters.
func Execute(root *cobra.Command) error {
	return root.ExecuteContext(ctrl.SetupSignalHandler())
}

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
	if exit, ok := errors.AsType[*ExitError](err); ok {
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

	// autoscalerStatus is where this configuration says the cluster-autoscaler
	// publishes its status, and therefore the one object every verdict about
	// the autoscaler rests on. Carried for the same reason as the two above:
	// it says what the answer is an answer about. A refusal to act on the
	// autoscaler's account is a claim binpack makes from a single Get, and
	// without naming where that Get went the reader has no way to tell a
	// cluster with no autoscaler from a binpack pointed somewhere else.
	//
	// Both halves, because both are configurable and either can be wrong.
	autoscalerStatus collect.StatusRef

	// dryRun is that configuration's mode, and it belongs beside the source
	// for the same reason: both say what the verdict is a verdict about. It is
	// the one setting that decides whether "would drain node-a" describes
	// something about to happen or something that never will, and the engine
	// does not read it — so a command that prints a decision has to carry it
	// separately or not at all.
	dryRun bool
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

// statusRef is where this configuration says the cluster-autoscaler publishes
// its status.
//
// One function so that the object binpack reads and the object it names in a
// report cannot come apart — the whole point of naming it is that the reader
// can go and look at the same one.
func statusRef(cfg *v1alpha1.Config) collect.StatusRef {
	return collect.StatusRef{
		Namespace: cfg.Discovery.AutoscalerNamespace,
		Name:      cfg.Discovery.AutoscalerStatusName,
	}
}
