package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"text/tabwriter"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/ctl"
	"github.com/alxgsv/ucfs/internal/index"
	"github.com/alxgsv/ucfs/internal/spool"
)

func newSpoolCmd(g *globalFlags) *cobra.Command {
	var mountDir string
	cmd := &cobra.Command{
		Use:   "spool",
		Short: "List, retry or drop files written but not uploaded",
		Long: "Files written through a mount wait in the spool until uploaded. A failed upload\n" +
			"stays there, and so does a file a crash cut off while it was written. These\n" +
			"commands talk to the running mount, or, when none runs, act on the cache dir.",
	}
	cmd.PersistentFlags().StringVar(&mountDir, "mount", "", "mount dir, when several mounts run")

	var asJSON bool
	list := &cobra.Command{
		Use:   "list",
		Short: "List spooled files",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var infos []spool.Info
			err := withSpools(cmd.Context(), g, mountDir, func(sock string) error {
				return ctl.Call(sock, http.MethodGet, "/spools", nil, &infos)
			}, func(m *spool.Manager) error {
				infos = m.List()
				return nil
			})
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(cmd.OutOrStdout())
				enc.SetIndent("", "  ")
				return enc.Encode(infos)
			}
			return printSpools(cmd.OutOrStdout(), infos)
		},
	}
	list.Flags().BoolVar(&asJSON, "json", false, "print JSON")

	action := func(name, short string) *cobra.Command {
		var all bool
		c := &cobra.Command{
			Use:   name + " [id...]",
			Short: short,
			RunE: func(cmd *cobra.Command, args []string) error {
				if len(args) == 0 && !all {
					return errors.New("name spool ids or pass --all")
				}
				in := spoolAction{IDs: args, All: all}
				var res map[string]int
				err := withSpools(cmd.Context(), g, mountDir, func(sock string) error {
					return ctl.Call(sock, http.MethodPost, "/spools/"+name, in, &res)
				}, func(m *spool.Manager) error {
					n, err := applySpools(m, name, in)
					res = map[string]int{"spools": n}
					return err
				})
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s: %d spools\n", name, res["spools"])
				return err
			},
		}
		c.Flags().BoolVar(&all, "all", false, "every spool")
		return c
	}
	cmd.AddCommand(list, action("retry", "Upload spooled files again"), action("drop", "Discard spooled files"))
	return cmd
}

// withSpools runs live against the daemon of the mount, or offline against
// the cache dir when no mount runs. Offline, retry uploads in this process
// and waits for the uploads to finish.
func withSpools(ctx context.Context, g *globalFlags, mountDir string, live func(sock string) error, offline func(*spool.Manager) error) error {
	cfg, err := g.load()
	if err != nil {
		return err
	}
	if mountDir != "" {
		dir, err := filepath.Abs(mountDir)
		if err != nil {
			return err
		}
		return live(socketFor(cfg.CacheDir, dir))
	}
	socks, err := allSockets(cfg.CacheDir)
	if err != nil {
		return err
	}
	var running []string
	for _, s := range socks {
		if _, err := ctl.Get(s); err == nil {
			running = append(running, s)
		}
	}
	switch len(running) {
	case 1:
		return live(running[0])
	case 0:
	default:
		return errors.New("several mounts run; pick one with --mount")
	}
	unlock, err := lockCache(cfg.CacheDir)
	if err != nil {
		return err
	}
	defer unlock()
	client, err := api.New(cfg, apiTransport)
	if err != nil {
		return err
	}
	ix, err := index.Open(cfg.IndexPath())
	if err != nil {
		return err
	}
	defer func() { _ = ix.Close() }()
	journal := &spool.Journal{Path: filepath.Join(cfg.CacheDir, "uploads.log")}
	m, err := spool.Open(ctx, ix, client, journal, spool.Options{Dir: filepath.Join(cfg.CacheDir, "spool")})
	if err != nil {
		return err
	}
	defer m.Shutdown(10 * time.Minute)
	return offline(m)
}

func printSpools(w io.Writer, infos []spool.Info) error {
	if len(infos) == 0 {
		_, err := fmt.Fprintln(w, "no spooled files")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tSTATE\tSIZE\tMODIFIED\tPATH\tERROR")
	for _, in := range infos {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", in.ID, in.State, humanize.IBytes(uint64(in.Size)),
			in.Modified.Local().Format(time.DateTime), in.Path, in.Error)
	}
	return tw.Flush()
}
