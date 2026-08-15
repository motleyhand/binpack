// Command binpack consolidates nodes that a managed cluster-autoscaler will
// not remove on its own.
package main

import (
	"fmt"
	"os"

	"github.com/motleyhand/binpack/internal/cli"
)

func main() {
	if err := cli.NewRootCommand(os.Stdout).Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "binpack:", err)
		// Not every non-nil error is a failure: `diagnose --fail-on` reports
		// its verdict through the exit status, and a CI job has to be able to
		// tell that from a command that could not run at all.
		os.Exit(cli.ExitCodeFor(err))
	}
}
