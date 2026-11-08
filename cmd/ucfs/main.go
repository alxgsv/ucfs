package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/alxgsv/ucfs/internal/config"
)

type globalFlags struct {
	config.Options
	verbose bool
}

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRootCmd().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "ucfs:", err)
		return 1
	}
	return 0
}

func newRootCmd() *cobra.Command {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:           "ucfs",
		Short:         "Mount an Uploadcare project as a local filesystem",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(*cobra.Command, []string) {
			level := slog.LevelInfo
			if g.verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
		},
	}
	f := root.PersistentFlags()
	f.StringVar(&g.ConfigPath, "config", "", "config file (default ~/.uploadcare/config.yaml)")
	f.StringVar(&g.Project, "project", "", "project name from the config file (env UPLOADCARE_PROJECT)")
	f.StringVar(&g.PublicKey, "public-key", "", "project public key (env UPLOADCARE_PUBLIC_KEY)")
	f.StringVar(&g.SecretKey, "secret-key", "", "project secret key (env UPLOADCARE_SECRET_KEY)")
	f.StringVar(&g.CDNBase, "cdn-base", "", "CDN base URL (env UPLOADCARE_CDN_BASE; default derived from the public key)")
	f.StringVar(&g.CacheDir, "cache-dir", "", "cache directory (default <user cache dir>/ucfs/<public key>)")
	f.BoolVarP(&g.verbose, "verbose", "v", false, "debug logging")

	root.AddCommand(newVersionCmd())
	return root
}
