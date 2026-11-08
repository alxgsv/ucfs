// Package vfstest builds a vfs.FS over the fakes for front-end tests.
package vfstest

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/cache"
	"github.com/alxgsv/ucfs/internal/cdn"
	"github.com/alxgsv/ucfs/internal/config"
	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/index"
	"github.com/alxgsv/ucfs/internal/spool"
	"github.com/alxgsv/ucfs/internal/vfs"
)

type Env struct {
	FS     *vfs.FS
	Fake   *fake.Server
	Index  *index.Index
	Cache  *cache.Cache
	CDN    *cdn.Client
	Client *api.Client
	Dir    string
	// Spools and Journal are set by Writable.
	Spools  *spool.Manager
	Journal *spool.Journal
}

// New serves files from a fake project, synced into a fresh index.
func New(t testing.TB, files ...fake.File) *Env {
	t.Helper()
	srv := fake.New(t)
	srv.Add(files...)
	client, err := api.New(&config.Config{PublicKey: "demopublickey", SecretKey: "secret", CDNBase: srv.CDNBase()}, srv.Transport())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ix, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	if _, err := ix.Sync(context.Background(), client, false); err != nil {
		t.Fatal(err)
	}
	cd := cdn.New(srv.CDNBase(), nil, nil)
	c, err := cache.Open(cd, cache.Options{Dir: filepath.Join(dir, "blocks"), MaxBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Wait)
	fs := vfs.New(ix, c, cd, nil)
	fs.Configure(vfs.Options{DateZone: time.UTC, CDNBase: srv.CDNBase(), Remote: client})
	return &Env{FS: fs, Fake: srv, Index: ix, Cache: c, CDN: cd, Client: client, Dir: dir}
}

// Writable makes the filesystem writable, with upload timers short enough
// for tests unless o sets them.
func (e *Env) Writable(t testing.TB, o spool.Options, w *vfs.Writes) {
	t.Helper()
	o.Dir = filepath.Join(e.Dir, "spool")
	if o.Settle == 0 {
		o.Settle = 20 * time.Millisecond
	}
	if o.Idle == 0 {
		o.Idle = 200 * time.Millisecond
	}
	e.Journal = &spool.Journal{Path: filepath.Join(e.Dir, "uploads.log")}
	m, err := spool.Open(context.Background(), e.Index, e.Client, e.Journal, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown(5 * time.Second) })
	m.OnUploaded = func(uuid string, data io.ReaderAt, size int64) { _ = e.Cache.Put(cache.Key{UUID: uuid}, data, size) }
	w.Spools, w.API = m, e.Client
	w.ShadowDir = filepath.Join(e.Dir, "shadow")
	if err := os.MkdirAll(w.ShadowDir, 0o700); err != nil {
		t.Fatal(err)
	}
	e.Spools = m
	e.FS.Journal(e.Journal.Path)
	e.FS.EnableWrites(w)
}
