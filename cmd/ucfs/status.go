package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"

	"github.com/alxgsv/ucfs/internal/ctl"
)

func newStatusCmd(g *globalFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status [dir]",
		Short: "Show traffic and index counters of running mounts",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := g.load()
			if err != nil {
				return err
			}
			var socks []string
			if len(args) == 1 {
				dir, err := filepath.Abs(args[0])
				if err != nil {
					return err
				}
				socks = []string{socketFor(cfg.CacheDir, dir)}
			} else if socks, err = allSockets(cfg.CacheDir); err != nil {
				return err
			}
			var all []ctl.Status
			for _, s := range socks {
				st, err := ctl.Get(s)
				if err != nil {
					if len(args) == 1 {
						return fmt.Errorf("no ucfs daemon for %s: %w", args[0], err)
					}
					continue
				}
				all = append(all, st)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(all)
			}
			if len(all) == 0 {
				return errors.New("no running mounts")
			}
			for _, st := range all {
				if err := printStatus(out, st); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// socketFor finds the control socket of the mount at dir: in the registry of
// running mounts, or where a daemon using cacheDir puts it.
func socketFor(cacheDir, dir string) string {
	if reg, err := ctl.RegistryDir(); err == nil {
		if m, ok := ctl.Lookup(reg, dir); ok {
			return m.Socket
		}
	}
	return ctl.SocketPath(cacheDir, dir)
}

// allSockets lists the control sockets of every registered mount plus those
// of mounts using cacheDir, which covers a daemon that failed to register.
func allSockets(cacheDir string) ([]string, error) {
	socks, err := ctl.Sockets(cacheDir)
	if err != nil {
		return nil, err
	}
	reg, err := ctl.RegistryDir()
	if err != nil {
		return socks, nil
	}
	mounts, err := ctl.Mounts(reg)
	if err != nil {
		return nil, err
	}
	for _, m := range mounts {
		if !slices.Contains(socks, m.Socket) {
			socks = append(socks, m.Socket)
		}
	}
	return socks, nil
}

func printStatus(w io.Writer, st ctl.Status) error {
	b := func(n int64) string { return humanize.IBytes(uint64(n)) }
	syncState := "ok"
	if st.Index.SyncError != "" {
		syncState = st.Index.SyncError
	}
	_, err := fmt.Fprintf(w, "%s  %s, %s, pid %d, up %s\n"+
		"  index  %d files, %s; last sync %s ago (%s)\n"+
		"  cdn    %d requests (%d HEAD, %d GET), %s (%d bytes)\n"+
		"  cache  served %s, %s from disk; fetched %s on demand + %s prefetched; %s of %s used\n",
		st.MountDir, st.Project, st.Frontend, st.PID, time.Since(st.Started).Round(time.Second),
		st.Index.Files, b(st.Index.Bytes), time.Since(st.Index.LastSync).Round(time.Second), syncState,
		st.CDN.Requests, st.CDN.Heads, st.CDN.Gets, b(st.CDN.Bytes), st.CDN.Bytes,
		b(st.Cache.Served), b(st.Cache.Hit), b(st.Cache.FetchedDemand), b(st.Cache.FetchedPrefetch),
		b(st.Cache.Used), b(st.Cache.Max))
	return err
}

func newUmountCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "umount <dir>",
		Short: "Unmount dir and stop its daemon",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, err := filepath.Abs(args[0])
			if err != nil {
				return err
			}
			cfg, err := g.load()
			if err != nil {
				return err
			}
			sock := socketFor(cfg.CacheDir, dir)
			if _, err := ctl.Get(sock); err != nil {
				// No daemon answers, but a mount may be left behind.
				return unmountDir(dir)
			}
			return ctl.Unmount(sock, 30*time.Second)
		},
	}
}
