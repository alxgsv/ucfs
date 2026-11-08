package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
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
		syncState = fmt.Sprintf("%d syncs failed in a row, the last with: %s; next try in %s", st.Index.SyncFailures,
			st.Index.SyncError, time.Until(st.Index.NextSync).Round(time.Second))
	}
	_, err := fmt.Fprintf(w, "%s  %s, %s, pid %d, up %s\n"+
		"  index  %d files, %s; last sync %s ago (%s)\n"+
		"  cdn    %d requests (%d HEAD, %d GET), %s (%d bytes); %d of %d in flight, %d reads and %d prefetches waiting\n"+
		"  cache  served %s, %s from disk; fetched %s on demand + %s prefetched; %s of %s used\n",
		st.MountDir, st.Project, st.Frontend, st.PID, time.Since(st.Started).Round(time.Second),
		st.Index.Files, b(st.Index.Bytes), time.Since(st.Index.LastSync).Round(time.Second), syncState,
		st.CDN.Requests, st.CDN.Heads, st.CDN.Gets, b(st.CDN.Bytes), st.CDN.Bytes,
		st.CDN.Inflight, st.CDN.MaxInflight, st.CDN.Queued, st.CDN.QueuedPrefetch,
		b(st.Cache.Served), b(st.Cache.Hit), b(st.Cache.FetchedDemand), b(st.Cache.FetchedPrefetch),
		b(st.Cache.Used), b(st.Cache.Max))
	if err != nil {
		return err
	}
	if bs := st.Budget; bs != nil {
		state := ""
		if bs.Refused > 0 {
			state = fmt.Sprintf("; reached, %d reads refused with EDQUOT", bs.Refused)
		} else if !bs.Prefetch {
			state = "; prefetch stopped"
		}
		_, err = fmt.Fprintf(w, "  budget %s of %s used today, %s left; resets %s (in %s)%s\n",
			b(bs.Used), b(bs.Limit), b(bs.Remaining), bs.ResetsAt.Format("Jan 2 15:04"),
			time.Until(bs.ResetsAt).Round(time.Minute), state)
		if err != nil {
			return err
		}
	}
	apiState := ""
	switch st.API.State {
	case "denied":
		apiState = fmt.Sprintf("; API denied since %s: %s", st.API.Since.Local().Format(time.DateTime), st.API.Detail)
	case "throttled":
		apiState = fmt.Sprintf("; throttled since %s", st.API.Since.Local().Format(time.DateTime))
	}
	if st.API.Degraded {
		apiState += " (serving the index; writes refused)"
	}
	_, err = fmt.Fprintf(w, "  api    %d REST, %d Upload API, %d part PUTs, %d throttled%s\n",
		st.API.REST, st.API.Upload, st.API.Parts, st.API.Throttled, apiState)
	if err != nil {
		return err
	}
	m := st.Meta
	_, err = fmt.Fprintf(w, "  views  %s\n"+
		"  meta   %d info refreshes (%d failed, ttl %s); xattr %d list, %d get, %d set, %d remove; %d md5 HEADs, %d group reads\n",
		strings.Join(st.Views, ","), m.Refreshes, m.RefreshErrors, m.TTL, m.XattrLists, m.XattrGets, m.XattrSets,
		m.XattrRemoves, m.MD5Heads, m.GroupFetches)
	if err != nil || !st.Writes.Enabled {
		return err
	}
	wr := st.Writes
	_, err = fmt.Fprintf(w, "  writes %d uploaded (%s), %d failed; spooled: %d writing, %d queued, %d uploading, %d failed; overwrite %s\n"+
		"  local  %d shadow files created, %d writes, %s kept local\n",
		wr.Uploads, b(wr.UploadBytes), wr.UploadFails, wr.Writing, wr.Queued, wr.Uploading, wr.Failed, wr.Overwrite,
		wr.ShadowCreates, wr.ShadowWrites, b(wr.ShadowBytes))
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
