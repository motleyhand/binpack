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
		os.Exit(1)
	}
}
