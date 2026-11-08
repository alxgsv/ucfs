package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/cache"
	"github.com/alxgsv/ucfs/internal/cdn"
	"github.com/alxgsv/ucfs/internal/ctl"
	"github.com/alxgsv/ucfs/internal/frontend"
	"github.com/alxgsv/ucfs/internal/index"
	"github.com/alxgsv/ucfs/internal/spool"
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
	views         []string
	metaTTL       time.Duration
	dateZone      string
	budget        string
	maxInflight   int
	writeFlags
}

// apiTransport carries REST and Upload API requests; nil is the default
// transport, and tests point it at internal/fake.
var apiTransport http.RoundTripper

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
		Long: "Mounts the project and keeps running in the foreground until\n" +
			"`ucfs umount <dir>`, an external umount, or Ctrl-C. Files written in tree/ or\n" +
			"files/ are uploaded; see `ucfs resolve` for the uuid a write got.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return runMount(cmd.Context(), g, f, args[0]) },
	}
	fl := cmd.Flags()
	fl.StringVar(&f.frontend, "frontend", defaultFrontend, "nfs or fuse (fuse is Linux only)")
	fl.BoolVar(&f.readOnly, "read-only", false, "mount read-only; on macOS this also stops Finder writing .DS_Store files")
	fl.IntVar(&f.readAhead, "readahead", 0, "macOS NFS client read-ahead in 1 MiB blocks; ucfs prefetches by itself")
	fl.BoolVar(&f.browse, "browse", false, "show the mount in the Finder sidebar (macOS)")
	fl.StringVar(&f.cacheSize, "cache-size", "2GiB", "on-disk block cache limit")
	fl.DurationVar(&f.syncInterval, "sync-interval", 30*time.Second, "how often to list new uploads")
	fl.DurationVar(&f.sweepInterval, "sweep-interval", 10*time.Minute, "how often to look for removed files and new groups")
	fl.StringSliceVar(&f.views, "views", vfs.DefaultViews, "views to mount: "+strings.Join(vfs.AllViews, ","))
	fl.DurationVar(&f.metaTTL, "meta-ttl", vfs.DefaultMetaTTL,
		"reading xattrs or resolving a file fetches its info again when the index copy is older than this")
	fl.StringVar(&f.dateZone, "by-date-zone", "Local", "time zone of by-date/: Local, UTC or a zone name such as Europe/Berlin")
	fl.StringVar(&f.budget, "traffic-budget", "",
		"CDN bytes per day, such as 500MB or 2GiB; reads past it fail with EDQUOT until local midnight")
	fl.IntVar(&f.maxInflight, "max-inflight", cdn.DefaultMaxInflight, "CDN requests in flight at once")
	f.register(fl)
	return cmd
}

// viewOptions checks --views, --meta-ttl and --by-date-zone.
func (f *mountFlags) viewOptions() (vfs.Options, error) {
	o := vfs.Options{MetaTTL: f.metaTTL}
	if len(f.views) == 0 {
		return o, errors.New("--views: name at least one view")
	}
	for _, v := range f.views {
		if !slices.Contains(vfs.AllViews, v) {
			return o, fmt.Errorf("--views: unknown view %q; views are %s", v, strings.Join(vfs.AllViews, ","))
		}
	}
	o.Views = f.views
	if f.metaTTL <= 0 {
		return o, errors.New("--meta-ttl must be positive")
	}
	zone, err := time.LoadLocation(f.dateZone)
	if err != nil {
		return o, fmt.Errorf("--by-date-zone: %w", err)
	}
	o.DateZone = zone
	return o, nil
}

// trafficBudget parses --traffic-budget, with or without a "/day" suffix;
// zero means no budget.
func (f *mountFlags) trafficBudget() (int64, error) {
	if f.budget == "" {
		return 0, nil
	}
	n, err := humanize.ParseBytes(strings.TrimSuffix(f.budget, "/day"))
	if err != nil || n == 0 {
		return 0, fmt.Errorf("--traffic-budget %q: want a size such as 500MB or 2GiB", f.budget)
	}
	return int64(n), nil
}

func runMount(ctx context.Context, g *globalFlags, f *mountFlags, dir string) error {
	opts, err := f.options()
	if err != nil {
		return err
	}
	vopts, err := f.viewOptions()
	if err != nil {
		return err
	}
	dir, err = filepath.Abs(dir)
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
	budgetBytes, err := f.trafficBudget()
	if err != nil {
		return err
	}
	if f.maxInflight < 1 {
		return errors.New("--max-inflight must be at least 1")
	}
	cfg, err := g.load()
	if err != nil {
		return err
	}
	for _, d := range []string{cfg.CacheDir, filepath.Join(cfg.CacheDir, "blocks"), filepath.Join(cfg.CacheDir, "spool"),
		filepath.Join(cfg.CacheDir, "shadow")} {
		fixed, err := ctl.PrivateDir(d)
		if err != nil {
			return err
		}
		if fixed {
			slog.Warn("made a cache directory private to this user", "dir", d, "mode", "0700")
		}
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

	// An index synced before serves at once, whatever the API says now,
	// and the loop brings it up to date in the background; a new one has
	// nothing to serve until a full listing went through.
	cursor, err := ix.Cursor(ctx)
	if err != nil {
		return err
	}
	if cursor.IsZero() {
		if _, err := ix.Sync(ctx, client, false); err != nil {
			return fmt.Errorf("initial index sync: %w", err)
		}
	}

	var budget *cache.Budget
	if budgetBytes > 0 {
		if budget, err = cache.NewBudget(ctx, budgetBytes, ix); err != nil {
			return err
		}
		budgetCtx, stopBudget := context.WithCancel(context.WithoutCancel(ctx))
		flushed := make(chan struct{})
		go func() {
			budget.Run(budgetCtx)
			close(flushed)
		}()
		defer func() {
			stopBudget()
			<-flushed
		}()
	}
	lim := cdn.NewLimiter(f.maxInflight)
	cd := cdn.New(cfg.CDNBase, nil, lim)
	c, err := cache.Open(cd, cache.Options{Dir: filepath.Join(cfg.CacheDir, "blocks"), MaxBytes: int64(maxCache),
		Limiter: lim, Budget: budget})
	if err != nil {
		return err
	}
	defer c.Wait()

	loop := &index.Loop{Index: ix, Source: client, Interval: f.syncInterval, SweepEvery: f.sweepInterval}
	if slices.Contains(vopts.Views, vfs.ViewGroups) {
		loop.Groups = client
	}
	fsys := vfs.New(ix, c, cd, func(uuid string) { loop.Refresh(context.WithoutCancel(ctx), uuid) })
	vopts.CDNBase, vopts.Remote = cfg.CDNBase, client
	vopts.Gate = func() error { return client.Health().Degraded(time.Now(), f.syncInterval) }
	fsys.Configure(vopts)
	loop.Changed = fsys.Touch
	journal := &spool.Journal{Path: filepath.Join(cfg.CacheDir, "uploads.log")}
	fsys.Journal(journal.Path)
	var spools *spool.Manager
	if !f.readOnly {
		unlock, err := lockCache(cfg.CacheDir)
		if err != nil {
			return err
		}
		defer unlock()
		opts.Dir = filepath.Join(cfg.CacheDir, "spool")
		if spools, err = spool.Open(ctx, ix, client, journal, opts); err != nil {
			return err
		}
		defer spools.Shutdown(2 * time.Minute)
		spools.OnUploaded = func(uuid string, data io.ReaderAt, size int64) {
			if err := c.Put(cache.Key{UUID: uuid}, data, size); err != nil {
				slog.Warn("cache an upload", "uuid", uuid, "err", err)
			}
		}
		shadowDir := filepath.Join(cfg.CacheDir, "shadow")
		if err := os.MkdirAll(shadowDir, 0o700); err != nil {
			return err
		}
		fsys.EnableWrites(&vfs.Writes{Spools: spools, API: client, ShadowDir: shadowDir,
			Foreign: f.foreign, Keep: opts.Keep})
	}
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
		st := ctl.Status{MountDir: dir, Frontend: f.frontend, Project: name, PID: os.Getpid(), Started: started,
			Views: fsys.Views(), Process: processStatus()}
		if is, err := ix.Stats(context.Background()); err == nil {
			st.Index.Files, st.Index.Bytes = is.Files, is.Bytes
		}
		st.Index.DBBytes = dbBytes(cfg.IndexPath())
		ls := loop.Status()
		st.Index.LastSync = ls.Last
		if ls.Last.IsZero() {
			st.Index.LastSync = started
		}
		if ls.Err != nil {
			st.Index.SyncError = ls.Err.Error()
		}
		st.Index.SyncFailures, st.Index.NextSync = ls.Failures, ls.Next
		cs, qs, ks := cd.Stats(), lim.Stats(), c.Stats()
		st.CDN = ctl.CDNStatus{Requests: cs.Requests, Heads: cs.Heads, Gets: cs.Gets, Bytes: cs.Bytes,
			MaxInflight: qs.Max, Inflight: qs.Inflight, Queued: qs.Queued, QueuedPrefetch: qs.QueuedPrefetch,
			Waits: qs.Waits, WaitedMS: qs.Waited.Milliseconds(), Dropped: qs.Dropped, Changed: ks.Changed}
		if budget != nil {
			bs := budget.Status()
			st.Budget = &ctl.Budget{Limit: bs.Limit, Used: bs.Used, Remaining: bs.Remaining, ResetsAt: bs.ResetsAt,
				Refused: bs.Refused, Prefetch: bs.Prefetch}
		}
		st.Cache = ctl.CacheStatus{Served: ks.Served, Hit: ks.Hit, FetchedDemand: ks.FetchedDemand,
			FetchedPrefetch: ks.FetchedPrefetch, Used: ks.Used, Max: ks.Max}
		as, h := client.Stats(), client.Health()
		st.API = ctl.APIStatus{REST: as.REST, Upload: as.Upload, Parts: as.Parts, Throttled: as.Throttled,
			State: h.State, Since: h.Since, Detail: h.Detail, Degraded: h.Degraded(time.Now(), f.syncInterval) != nil}
		ms := fsys.MetaStats()
		st.Meta = ctl.MetaStatus{TTL: vopts.MetaTTL, Refreshes: ms.Refreshes, RefreshErrors: ms.RefreshErrors,
			XattrLists: ms.Lists, XattrGets: ms.Gets, XattrSets: ms.Sets, XattrRemoves: ms.Removes,
			MD5Heads: ms.MD5Heads, GroupFetches: ms.GroupFetches}
		if spools != nil {
			ss, ws := spools.Stats(), fsys.WriteStats()
			st.Writes = ctl.WritesStatus{Enabled: true, Overwrite: f.overwrite,
				Writing: ss.Writing, Queued: ss.Queued, Uploading: ss.Uploading, Failed: ss.Failed,
				Uploads: ss.Uploads, UploadFails: ss.Failures, UploadBytes: ss.Bytes,
				ShadowCreates: ws.ShadowCreates, ShadowWrites: ws.ShadowWrites, ShadowBytes: ws.ShadowBytes}
		}
		return st
	}
	sock := ctl.SocketPath(cfg.CacheDir, dir)
	srv, err := ctl.Serve(sock, status, func() {
		select {
		case unmountReq <- struct{}{}:
		default:
		}
	}, routes(fsys, spools, journal)...)
	if err != nil {
		return errors.Join(err, m.Unmount())
	}
	defer func() { _ = srv.Close() }()
	defer register(dir, sock)()

	is, _ := ix.Stats(ctx)
	slog.Info("mounted", "dir", dir, "frontend", f.frontend, "project", name, "files", is.Files, "read_only", f.readOnly)
	if spools != nil {
		spools.Recover(ctx)
	}
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

func processStatus() ctl.ProcessStatus {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	fds, _ := os.ReadDir("/dev/fd")
	return ctl.ProcessStatus{Goroutines: runtime.NumGoroutine(), FDs: len(fds), HeapBytes: m.HeapInuse}
}

func dbBytes(path string) int64 {
	var n int64
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			n += fi.Size()
		}
	}
	return n
}

// register records the mount in the registry of running mounts and returns
// the function that removes the record. Without a record, status and umount
// still find the mount when given the same --cache-dir.
func register(dir, sock string) func() {
	reg, err := ctl.RegistryDir()
	if err == nil {
		var unregister func()
		if unregister, err = ctl.Register(reg, ctl.Mount{MountDir: dir, Socket: sock, PID: os.Getpid()}); err == nil {
			return unregister
		}
	}
	slog.Warn("mount not registered; status and umount need the same --cache-dir", "err", err)
	return func() {}
}
