package cli

import (
	"context"
	"io"
	"testing"

	"github.com/spf13/cobra"
)

func TestBinpackRunsUnderAContextASignalCanCancel(t *testing.T) {
	var got context.Context
	probe := &cobra.Command{Use: "probe", RunE: func(cmd *cobra.Command, _ []string) error {
		got = cmd.Context()
		return nil
	}}
	probe.SetArgs([]string{})
	probe.SetOut(io.Discard)
	probe.SetErr(io.Discard)

	if err := Execute(probe); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got == nil {
		t.Fatal("the command never ran, so nothing was measured")
	}
	if got.Done() == nil {
		t.Fatal("binpack runs under a context nothing can cancel, so no shutdown is " +
			"possible — graceful or otherwise")
	}
}
