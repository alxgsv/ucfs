package main

import (
	"fmt"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/index"
)

func newIndexCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Manage the local file index",
	}
	cmd.AddCommand(newIndexSyncCmd(g))
	return cmd
}

func newIndexSyncCmd(g *globalFlags) *cobra.Command {
	var full bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "List the project's files into the local index",
		Long: "Lists files uploaded since the last sync (all files on the first run or with --full),\n" +
			"marks files removed since then, and stores the result in the index under the cache directory.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			client, err := api.New(cfg, nil)
			if err != nil {
				return err
			}
			ix, err := index.Open(cfg.IndexPath())
			if err != nil {
				return err
			}
			defer func() { _ = ix.Close() }()

			res, syncErr := ix.Sync(cmd.Context(), client, full)
			var removed int
			if syncErr == nil {
				removed, syncErr = ix.SweepRemoved(cmd.Context(), client)
			}
			hs := client.Stats()
			st, err := ix.Stats(cmd.Context())
			if err != nil {
				return err
			}
			mode := "incremental from " + res.From.Format(time.RFC3339)
			if res.Full {
				mode = "full"
			}
			out := cmd.OutOrStdout()
			name := cfg.Project
			if name == "" {
				name = cfg.PublicKey
			}
			_, err = fmt.Fprintf(out, "%s: %s sync listed %d files (%d new, %d newly removed) in %s; %d requests, %d throttled\n"+
				"index: %d files, %d stored, %s, %s\n",
				name, mode, res.Listed, res.Added, removed, res.Duration.Round(time.Millisecond), hs.Requests, hs.Throttled,
				st.Files, st.Stored, humanize.IBytes(uint64(st.Bytes)), cfg.IndexPath())
			if syncErr != nil {
				return syncErr
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&full, "full", false, "list every file, not only those since the last sync")
	return cmd
}
