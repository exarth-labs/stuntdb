package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/exarth-labs/stuntdb/internal/buildinfo"
)

func newVersionCmd(opts *globalOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the stuntdb version",
		Args:  cobra.NoArgs,
		RunE: runE(func(cmd *cobra.Command, _ []string) error {
			return writeVersion(cmd.OutOrStdout(), buildinfo.Get(), opts.json)
		}),
	}
}

func writeVersion(w io.Writer, info buildinfo.Info, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(info)
	}
	_, err := fmt.Fprintf(w, "stuntdb %s\n  commit:   %s\n  built:    %s\n  go:       %s\n  platform: %s\n",
		info.Version, info.Commit, info.Date, info.Go, info.Platform)
	return err
}
