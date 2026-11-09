package main

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version, commit and date are set with -ldflags "-X main.version=..." by
// the Makefile (version only) and by goreleaser (all three).
var (
	version = "dev"
	commit  = ""
	date    = ""
)

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), versionLine())
			return err
		},
	}
}

func versionLine() string {
	c, d := commit, date
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			switch {
			case s.Key == "vcs.revision" && c == "":
				c = s.Value
			case s.Key == "vcs.time" && d == "":
				d = s.Value
			}
		}
	}
	if c == "" {
		c = "unknown"
	}
	built := ""
	if d != "" {
		built = ", built " + d
	}
	return fmt.Sprintf("ucfs %s (commit %s%s, %s, %s/%s)", version, c, built, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
