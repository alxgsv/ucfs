package index

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/config"
	"github.com/alxgsv/ucfs/internal/fake"
)

var t0 = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// files returns n files starting at start, three per second so that
// page boundaries fall inside a second.
func files(start, n int) []fake.File {
	out := make([]fake.File, n)
	for i := range out {
		k := start + i
		out[i] = fake.File{
			UUID:     fmt.Sprintf("00000000-0000-0000-0000-%012d", k),
			Name:     fmt.Sprintf("file %d.jpg", k),
			Size:     int64(k),
			MIME:     "image/jpeg",
			Uploaded: t0.Add(time.Duration(k) * time.Second / 3),
			Stored:   k%2 == 0,
			Width:    10,
			Height:   20,
		}
	}
	return out
}

func setup(t *testing.T) (*fake.Server, *api.Client, *Index) {
	t.Helper()
	srv := fake.New(t)
	client, err := api.New(&config.Config{PublicKey: "demopublickey", SecretKey: "secret", CDNBase: "https://cdn.example.com"}, srv.Transport())
	if err != nil {
		t.Fatal(err)
	}
	ix, err := Open(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	return srv, client, ix
}

func TestSyncFullThenIncremental(t *testing.T) {
	ctx := context.Background()
	srv, client, ix := setup(t)
	srv.Add(files(0, 2500)...)

	res, err := ix.Sync(ctx, client, false)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Full || res.Listed != 2500 || res.Added != 2500 {
		t.Fatalf("first sync = %+v", res)
	}
	if got := srv.Requests(); got != 3 {
		t.Errorf("requests = %d, want 3 pages", got)
	}
	st, err := ix.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.Files != 2500 || st.Stored != 1250 || st.Bytes != 2499*2500/2 {
		t.Errorf("stats = %+v", st)
	}
	cursor, err := ix.Cursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if want := files(2499, 1)[0].Uploaded.Truncate(time.Second); !cursor.Equal(want) {
		t.Errorf("cursor = %v, want %v", cursor, want)
	}

	srv.Add(files(2500, 10)...)
	res, err = ix.Sync(ctx, client, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Full || res.Added != 10 || res.Listed < 10 || res.Listed > 13 {
		t.Errorf("incremental sync = %+v, want 10 added and at most one second re-listed", res)
	}

	res, err = ix.Sync(ctx, client, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 0 {
		t.Errorf("idle sync added %d", res.Added)
	}
}

type failingLister struct {
	*api.Client
	after int
}

var errBoom = errors.New("boom")

func (l failingLister) ListFiles(ctx context.Context, o api.ListOptions, fn func(api.File) error) error {
	n := 0
	return l.Client.ListFiles(ctx, o, func(f api.File) error {
		if n == l.after {
			return errBoom
		}
		n++
		return fn(f)
	})
}

func TestSyncResumesAfterInterruption(t *testing.T) {
	ctx := context.Background()
	srv, client, ix := setup(t)
	srv.Add(files(0, 1200)...)

	if _, err := ix.Sync(ctx, failingLister{client, 700}, false); !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	st, _ := ix.Stats(ctx)
	if st.Files != batchSize {
		t.Fatalf("after interruption files = %d, want one committed batch of %d", st.Files, batchSize)
	}
	res, err := ix.Sync(ctx, client, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Full {
		t.Error("resumed sync started from scratch")
	}
	st, _ = ix.Stats(ctx)
	if st.Files != 1200 {
		t.Errorf("files = %d, want 1200", st.Files)
	}
}

func TestSyncSkipsRemovedAndKeepsFields(t *testing.T) {
	ctx := context.Background()
	srv, client, ix := setup(t)
	fs := files(0, 3)
	fs[1].Removed = true
	fs[2].Name = "naïve café ☕.png"
	fs[2].Metadata = map[string]string{"ucfs:path": "/a/b.png", "k": "v"}
	srv.Add(fs...)

	if _, err := ix.Sync(ctx, client, false); err != nil {
		t.Fatal(err)
	}
	var name, meta, tree string
	var w, h int
	err := ix.db.QueryRow(`SELECT name, metadata, tree_path, width, height FROM files WHERE uuid = ?`, fs[2].UUID).
		Scan(&name, &meta, &tree, &w, &h)
	if err != nil {
		t.Fatal(err)
	}
	if name != fs[2].Name || tree != "/a/b.png" || w != 10 || h != 20 || meta != `{"k":"v","ucfs:path":"/a/b.png"}` {
		t.Errorf("row = %q %q %q %dx%d", name, meta, tree, w, h)
	}
	st, _ := ix.Stats(ctx)
	if st.Files != 2 {
		t.Errorf("files = %d, want 2 (removed file not listed)", st.Files)
	}
}

func TestSyncRetriesThrottle(t *testing.T) {
	srv, client, ix := setup(t)
	srv.Add(files(0, 5)...)
	srv.Throttle(1)

	res, err := ix.Sync(context.Background(), client, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 5 {
		t.Errorf("added = %d", res.Added)
	}
	if st := client.Stats(); st.Throttled != 1 || st.Requests != 2 {
		t.Errorf("client stats = %+v, want 1 throttled of 2 requests", st)
	}
}

func TestReopenKeepsSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	for range 2 {
		ix, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := ix.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSweepAndRefreshMarkRemoved(t *testing.T) {
	ctx := context.Background()
	srv, client, ix := setup(t)
	fs := files(0, 5)
	srv.Add(fs...)
	if _, err := ix.Sync(ctx, client, false); err != nil {
		t.Fatal(err)
	}

	srv.Remove(fs[1].UUID)
	n, err := ix.SweepRemoved(ctx, client)
	if err != nil || n != 1 {
		t.Fatalf("sweep marked %d, err %v; want 1", n, err)
	}
	if n, _ := ix.SweepRemoved(ctx, client); n != 0 {
		t.Errorf("second sweep marked %d again", n)
	}

	srv.Remove(fs[3].UUID)
	changed := 0
	loop := &Loop{Index: ix, Source: client, Changed: func() { changed++ }}
	loop.Refresh(ctx, fs[3].UUID)
	if f, err := ix.Get(ctx, fs[3].UUID); err != nil || !f.Removed {
		t.Errorf("after refresh: %+v %v", f, err)
	}
	loop.Refresh(ctx, fs[4].UUID) // still present: stays
	if f, _ := ix.Get(ctx, fs[4].UUID); f.Removed {
		t.Error("refresh removed a present file")
	}
	if st, _ := ix.Stats(ctx); st.Files != 3 || changed != 2 {
		t.Errorf("files = %d, changed = %d; want 3 and 2", st.Files, changed)
	}
}
