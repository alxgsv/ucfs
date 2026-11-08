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
	cmd.AddCommand(newIndexSyncCmd(g), newIndexRefreshCmd(g))
	return cmd
}

func newIndexRefreshCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "refresh <uuid>...",
		Short: "Read files' current info into the index",
		Long: "Fetches each file's info, metadata and tags included, and stores it in the index, which listings\n" +
			"do not do for changes other clients make to files already listed. A file the API no longer\n" +
			"knows is marked removed. A running mount sees the result at once; it shares the index.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			client, err := api.New(cfg, apiTransport)
			if err != nil {
				return err
			}
			ix, err := index.Open(cfg.IndexPath())
			if err != nil {
				return err
			}
			defer func() { _ = ix.Close() }()
			ctx, out := cmd.Context(), cmd.OutOrStdout()
			for _, uuid := range args {
				f, err := client.Info(ctx, uuid)
				switch {
				case api.IsNotFound(err):
					err = ix.MarkRemoved(ctx, uuid)
					if err == nil {
						_, err = fmt.Fprintf(out, "%s: not found, marked removed\n", uuid)
					}
				case err == nil && !f.RemovedAt.IsZero():
					if err = ix.PutInfo(ctx, time.Now(), f); err == nil {
						_, err = fmt.Fprintf(out, "%s: removed %s\n", uuid, f.RemovedAt.Local().Format(time.DateTime))
					}
				case err == nil:
					if err = ix.PutInfo(ctx, time.Now(), f); err == nil {
						_, err = fmt.Fprintf(out, "%s: %s, %s, %d metadata keys, tags %v\n", uuid, f.Name,
							humanize.IBytes(uint64(f.Size)), len(f.Metadata), f.Tags)
					}
				}
				if err != nil {
					return fmt.Errorf("%s: %w", uuid, err)
				}
			}
			return nil
		},
	}
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
			client, err := api.New(cfg, apiTransport)
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
