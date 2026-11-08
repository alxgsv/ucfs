// Package vfstest builds a vfs.FS over the fakes for front-end tests.
package vfstest

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/cache"
	"github.com/alxgsv/ucfs/internal/cdn"
	"github.com/alxgsv/ucfs/internal/config"
	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/index"
	"github.com/alxgsv/ucfs/internal/vfs"
)

type Env struct {
	FS     *vfs.FS
	Fake   *fake.Server
	Index  *index.Index
	Cache  *cache.Cache
	CDN    *cdn.Client
	Client *api.Client
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
	cd := cdn.New(srv.CDNBase(), nil)
	c, err := cache.Open(cd, cache.Options{Dir: filepath.Join(dir, "blocks"), MaxBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Wait)
	return &Env{FS: vfs.New(ix, c, cd, nil), Fake: srv, Index: ix, Cache: c, CDN: cd, Client: client}
}
