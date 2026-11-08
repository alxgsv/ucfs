package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/cache"
	"github.com/alxgsv/ucfs/internal/cdn"
	"github.com/alxgsv/ucfs/internal/ctl"
	"github.com/alxgsv/ucfs/internal/frontend"
	"github.com/alxgsv/ucfs/internal/index"
	"github.com/alxgsv/ucfs/internal/vfs"
)

type mountFlags struct {
	frontend      string
	readOnly      bool
	readAhead     int
	browse        bool
	cacheSize     string
	syncInterval  time.Duration
	sweepInterval time.Duration
}

// mounted is a filesystem attached by one of the front-ends.
type mounted interface {
	// Done is closed when the filesystem goes away without Unmount, for
	// example after an external umount.
	Done() <-chan struct{}
	Unmount() error
}

func newMountCmd(g *globalFlags) *cobra.Command {
	f := &mountFlags{}
	cmd := &cobra.Command{
		Use:   "mount <dir>",
		Short: "Mount the project at dir and serve it until unmounted",
		Long: "Mounts the project read-only and keeps running in the foreground until\n" +
			"`ucfs umount <dir>`, an external umount, or Ctrl-C.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runMount(cmd.Context(), g, f, args[0]) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.frontend, "frontend", defaultFrontend, "nfs or fuse (fuse is Linux only)")
	fl.BoolVar(&f.readOnly, "read-only", true, "mount read-only (writes arrive in a later phase)")
	fl.IntVar(&f.readAhead, "readahead", 0, "macOS NFS client read-ahead in 1 MiB blocks; ucfs prefetches by itself")
	fl.BoolVar(&f.browse, "browse", false, "show the mount in the Finder sidebar (macOS)")
	fl.StringVar(&f.cacheSize, "cache-size", "2GiB", "on-disk block cache limit")
	fl.DurationVar(&f.syncInterval, "sync-interval", 30*time.Second, "how often to list new uploads")
	fl.DurationVar(&f.sweepInterval, "sweep-interval", 10*time.Minute, "how often to look for removed files")
	return cmd
}

func runMount(ctx context.Context, g *globalFlags, f *mountFlags, dir string) error {
	if !f.readOnly {
		return errors.New("only read-only mounts are supported so far")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := frontend.CheckMountDir(dir); err != nil {
		return err
	}
	maxCache, err := humanize.ParseBytes(f.cacheSize)
	if err != nil {
		return fmt.Errorf("--cache-size: %w", err)
	}
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

	res, err := ix.Sync(ctx, client, false)
	if err != nil {
		if res.Full {
			return fmt.Errorf("initial index sync: %w", err)
		}
		slog.Warn("index sync failed, serving the existing index", "err", err)
	}

	cd := cdn.New(cfg.CDNBase, nil)
	c, err := cache.Open(cd, cache.Options{Dir: filepath.Join(cfg.CacheDir, "blocks"), MaxBytes: int64(maxCache)})
	if err != nil {
		return err
	}
	defer c.Wait()

	loop := &index.Loop{Index: ix, Source: client, Interval: f.syncInterval, SweepEvery: f.sweepInterval}
	fsys := vfs.New(ix, c, cd, func(uuid string) { loop.Refresh(context.WithoutCancel(ctx), uuid) })
	loop.Changed = fsys.Touch
	loopCtx, stopLoop := context.WithCancel(ctx)
	defer stopLoop()
	go loop.Run(loopCtx)

	m, err := mountFrontend(f.frontend, dir, fsys, f, g.verbose)
	if err != nil {
		return err
	}
	unmountReq := make(chan struct{}, 1)
	started := time.Now()
	name := cfg.Project
	if name == "" {
		name = cfg.PublicKey
	}
	status := func() ctl.Status {
		st := ctl.Status{MountDir: dir, Frontend: f.frontend, Project: name, PID: os.Getpid(), Started: started}
		if is, err := ix.Stats(context.Background()); err == nil {
			st.Index.Files, st.Index.Bytes = is.Files, is.Bytes
		}
		last, serr := loop.Status()
		if last.IsZero() {
			last = started
		}
		st.Index.LastSync = last
		if serr != nil {
			st.Index.SyncError = serr.Error()
		}
		cs := cd.Stats()
		st.CDN = ctl.CDNStatus{Requests: cs.Requests, Heads: cs.Heads, Gets: cs.Gets, Bytes: cs.Bytes}
		ks := c.Stats()
		st.Cache = ctl.CacheStatus{Served: ks.Served, Hit: ks.Hit, FetchedDemand: ks.FetchedDemand,
			FetchedPrefetch: ks.FetchedPrefetch, Used: ks.Used, Max: ks.Max}
		return st
	}
	srv, err := ctl.Serve(ctl.SocketPath(cfg.CacheDir, dir), status, func() {
		select {
		case unmountReq <- struct{}{}:
		default:
		}
	})
	if err != nil {
		return errors.Join(err, m.Unmount())
	}
	defer func() { _ = srv.Close() }()

	is, _ := ix.Stats(ctx)
	slog.Info("mounted", "dir", dir, "frontend", f.frontend, "project", name, "files", is.Files)
	select {
	case <-ctx.Done():
		slog.Info("signal received, unmounting", "dir", dir)
	case <-unmountReq:
		slog.Info("unmount requested", "dir", dir)
	case <-m.Done():
		slog.Info("unmounted externally", "dir", dir)
		return nil
	}
	return m.Unmount()
}
