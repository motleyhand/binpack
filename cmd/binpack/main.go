// Command binpack consolidates nodes that a managed cluster-autoscaler will
// not remove on its own.
package main

import (
	"fmt"
	"os"

	"github.com/motleyhand/binpack/internal/cli"
)

func main() {
	// Through cli.Execute, which supplies the context a signal cancels. Calling
	// the command's own Execute would leave binpack unable to shut down at all.
	if err := cli.Execute(cli.NewRootCommand(os.Stdout)); err != nil {
		fmt.Fprintln(os.Stderr, "binpack:", err)
		// Not every non-nil error is a failure: `diagnose --fail-on` reports
		// its verdict through the exit status, and a CI job has to be able to
		// tell that from a command that could not run at all.
		os.Exit(cli.ExitCodeFor(err))
	}
}
