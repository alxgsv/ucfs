package main

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is set with -ldflags "-X main.version=..." by the Makefile.
var version = "dev"

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			commit := "unknown"
			if bi, ok := debug.ReadBuildInfo(); ok {
				for _, s := range bi.Settings {
					if s.Key == "vcs.revision" {
						commit = s.Value
					}
				}
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "ucfs %s (commit %s, %s, %s/%s)\n",
				version, commit, runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return err
		},
	}
}
