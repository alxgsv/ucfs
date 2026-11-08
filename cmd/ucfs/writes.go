package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/pflag"

	"github.com/alxgsv/ucfs/internal/ctl"
	"github.com/alxgsv/ucfs/internal/index"
	"github.com/alxgsv/ucfs/internal/spool"
	"github.com/alxgsv/ucfs/internal/vfs"
)

type writeFlags struct {
	overwrite   string
	metadata    []string
	foreign     bool
	threshold   string
	idle        time.Duration
	concurrency int
	aliasTTL    time.Duration
}

func (w *writeFlags) register(fl *pflag.FlagSet) {
	fl.StringVar(&w.overwrite, "on-overwrite", "replace",
		"replace: writing over a file uploads a new one, copies metadata and tags, and soft-deletes the old one; keep: leave the old one too")
	fl.StringArrayVar(&w.metadata, "upload-metadata", nil, "key=value metadata added to every upload (repeatable)")
	fl.BoolVar(&w.foreign, "allow-foreign", false,
		"let rm, overwrites and mv change files other clients uploaded (files/<uuid>_<name>, tree/_inbox/); off, a stray rm -r cannot empty a project")
	fl.StringVar(&w.threshold, "multipart-threshold", "10MiB", "upload files above this size in parts (the Upload API's minimum is 10 MiB)")
	fl.DurationVar(&w.idle, "commit-idle", 3*time.Second, "NFS: upload a written file this long after its last write if no COMMIT came")
	fl.IntVar(&w.concurrency, "upload-concurrency", 8, "uploads running at once")
	fl.DurationVar(&w.aliasTTL, "alias-ttl", time.Hour, "how long a name written in files/ keeps resolving")
}

var metadataKey = regexp.MustCompile(`^[\w.:-]{1,64}$`)

func (w *writeFlags) options() (spool.Options, error) {
	o := spool.Options{Idle: w.idle, Concurrency: w.concurrency, AliasTTL: w.aliasTTL, Metadata: map[string]string{}}
	switch w.overwrite {
	case "replace":
	case "keep":
		o.Keep = true
	default:
		return o, fmt.Errorf("--on-overwrite: %q is neither replace nor keep", w.overwrite)
	}
	for _, kv := range w.metadata {
		k, v, ok := strings.Cut(kv, "=")
		switch {
		case !ok || !metadataKey.MatchString(k):
			return o, fmt.Errorf("--upload-metadata %q: want key=value with a key of up to 64 letters, digits, _ . : -", kv)
		case k == index.TreePathKey:
			return o, fmt.Errorf("--upload-metadata: %s is set by the mount", k)
		case len([]rune(v)) > 512:
			return o, fmt.Errorf("--upload-metadata %s: values are limited to 512 characters", k)
		}
		o.Metadata[k] = v
	}
	t, err := humanize.ParseBytes(w.threshold)
	if err != nil {
		return o, fmt.Errorf("--multipart-threshold: %w", err)
	}
	o.MultipartThreshold = int64(t)
	return o, nil
}

// lockCache keeps a second writable mount off a cache dir: two daemons would
// upload the same spools.
func lockCache(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "write.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another writable mount uses %s; mount with --read-only or another --cache-dir", dir)
		}
		return nil, err
	}
	return func() { _ = f.Close() }, nil
}

// resolved is a vfs.Resolution with the CDN URL a script wants.
type resolved struct {
	vfs.Resolution
	CDNURL string `json:"cdn_url,omitempty"`
}

type spoolAction struct {
	IDs []string `json:"ids"`
	All bool     `json:"all"`
}

// routes are the control API endpoints of a mount beyond status and unmount.
func routes(fsys *vfs.FS, spools *spool.Manager, journal *spool.Journal, cdnBase string) []ctl.Route {
	return []ctl.Route{
		{Pattern: "POST /resolve", Handler: func(w http.ResponseWriter, r *http.Request) {
			var in struct {
				Path string `json:"path"`
			}
			if err := decode(r, &in); err != nil {
				ctl.Reply(w, nil, err)
				return
			}
			res, err := fsys.Resolve(r.Context(), in.Path, journal)
			out := resolved{Resolution: res}
			if res.UUID != "" {
				out.CDNURL = strings.TrimSuffix(cdnBase, "/") + "/" + res.UUID + "/" + res.Ops
			}
			ctl.Reply(w, out, err)
		}},
		{Pattern: "GET /spools", Handler: func(w http.ResponseWriter, _ *http.Request) {
			if spools == nil {
				ctl.Reply(w, []spool.Info{}, nil)
				return
			}
			ctl.Reply(w, spools.List(), nil)
		}},
		{Pattern: "POST /spools/{action}", Handler: func(w http.ResponseWriter, r *http.Request) {
			var in spoolAction
			if err := decode(r, &in); err != nil {
				ctl.Reply(w, nil, err)
				return
			}
			if spools == nil {
				ctl.Reply(w, nil, errors.New("the mount is read-only"))
				return
			}
			n, err := applySpools(spools, r.PathValue("action"), in)
			ctl.Reply(w, map[string]int{"spools": n}, err)
		}},
	}
}

// applySpools retries or drops the spools an action names.
func applySpools(m *spool.Manager, action string, in spoolAction) (int, error) {
	var targets []*spool.Spool
	if in.All {
		for _, info := range m.List() {
			if s := m.Find(info.ID); s != nil {
				targets = append(targets, s)
			}
		}
	}
	for _, id := range in.IDs {
		s := m.Find(id)
		if s == nil {
			return 0, fmt.Errorf("no spool %s", id)
		}
		targets = append(targets, s)
	}
	for _, s := range targets {
		switch action {
		case "retry":
			m.Retry(s)
		case "drop":
			if err := m.Drop(context.Background(), s); err != nil {
				return 0, err
			}
		default:
			return 0, fmt.Errorf("unknown action %q", action)
		}
	}
	return len(targets), nil
}

func decode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }
