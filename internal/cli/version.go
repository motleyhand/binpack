package cli

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/motleyhand/binpack/internal/version"
)

func newVersionCommand(opts *options) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build information",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			return writeVersion(opts)
		},
	}
}

func writeVersion(opts *options) error {
	info := version.Get()

	if opts.output == outputJSON {
		enc := json.NewEncoder(opts.out)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}

	_, err := fmt.Fprintf(opts.out,
		"binpack %s\n%-10s %s\n%-10s %s\n%-10s %s\n%-10s %s\n",
		info.Version,
		"commit:", info.Commit,
		"built:", info.Date,
		"go:", info.GoVersion,
		"platform:", info.Platform,
	)
	return err
}
