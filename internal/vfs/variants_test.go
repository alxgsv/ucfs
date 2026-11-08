package vfs_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/cache"
	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/vfs"
	"github.com/alxgsv/ucfs/internal/vfs/vfstest"
)

const webp = "-/resize/1200x/-/format/webp/"

func hero() fake.File { return file(1, "hero.jpg", 3*cache.BlockSize+5) }

func methods(t *testing.T, env *vfstest.Env, heads, gets int) {
	t.Helper()
	if h, g := env.Fake.CDNMethods(); h != heads || g != gets {
		t.Errorf("CDN requests: %d HEAD, %d GET; want %d HEAD, %d GET", h, g, heads, gets)
	}
}

func TestVariantStatCostsOneHead(t *testing.T) {
	env := vfstest.New(t, hero())
	ctx := context.Background()
	p := "files/" + uuid(1) + "/" + webp + "hero.webp"
	body, _, _ := env.Fake.Variant(uuid(1), webp)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			e, err := env.FS.Walk(ctx, p)
			if err != nil || e.Node != (vfs.Node{Kind: vfs.KindVariant, UUID: uuid(1), Ops: webp}) || e.Attr.Size != int64(len(body)) {
				t.Errorf("walk = %+v, %v", e, err)
			}
		})
	}
	wg.Wait()
	methods(t, env, 1, 0)

	e, err := env.FS.Walk(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.FS.Getattr(ctx, e.Node); err != nil {
		t.Fatal(err)
	}
	methods(t, env, 1, 0)
}

func TestVariantLookup(t *testing.T) {
	env := vfstest.New(t, hero())
	ctx := context.Background()
	dir := "files/" + uuid(1) + "/"
	tests := []struct {
		path string
		want vfs.Node
		err  error
	}{
		{dir + "-", vfs.Node{Kind: vfs.KindVariantDir, UUID: uuid(1), Ops: "-/"}, nil},
		{dir + "hero.jpg", vfs.Node{Kind: vfs.KindFile, UUID: uuid(1)}, nil},
		{dir + "other.jpg", vfs.Node{}, syscall.ENOENT},
		{dir + "-/nosuchop", vfs.Node{}, syscall.ENOENT},
		{dir + "-/-", vfs.Node{}, syscall.ENOENT},
		{dir + "-/resize/100x/-/nosuchop", vfs.Node{}, syscall.ENOENT},
		{dir + "-/resize/.DS_Store", vfs.Node{}, syscall.ENOENT},
		{dir + "-/resize/._hero.jpg", vfs.Node{}, syscall.ENOENT},
		{dir + "-/resize", vfs.Node{Kind: vfs.KindVariantDir, UUID: uuid(1), Ops: "-/resize/"}, nil},
		{dir + "-/crop/1:1/center", vfs.Node{Kind: vfs.KindVariantDir, UUID: uuid(1), Ops: "-/crop/1:1/center/"}, nil},
		{dir + "-/resize/100x/-", vfs.Node{Kind: vfs.KindVariantDir, UUID: uuid(1), Ops: "-/resize/100x/-/"}, nil},
		// The variant here is hero.webp, so hero.jpg is one more argument.
		{dir + "-/format/webp/hero.jpg", vfs.Node{Kind: vfs.KindVariantDir, UUID: uuid(1), Ops: "-/format/webp/hero.jpg/"}, nil},
		{"files/" + uuid(99) + "/-/resize", vfs.Node{}, syscall.ENOENT},
	}
	for _, tt := range tests {
		e, err := env.FS.Walk(ctx, tt.path)
		if !errors.Is(err, tt.err) || e.Node != tt.want {
			t.Errorf("walk %s = %+v, %v; want %+v, %v", tt.path, e.Node, err, tt.want, tt.err)
		}
	}
	methods(t, env, 0, 0)
}

func names(t *testing.T, env *vfstest.Env, p string) []string {
	t.Helper()
	ctx := context.Background()
	d, err := env.FS.Walk(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := env.FS.ReadDir(ctx, d.Node)
	if err != nil {
		t.Fatalf("readdir %s: %v", p, err)
	}
	var out []string
	for _, e := range entries {
		got, err := env.FS.Lookup(ctx, d.Node, e.Name)
		if err != nil || got.Node != e.Node || got.Attr.Size != e.Attr.Size {
			t.Errorf("lookup %s/%s = %+v, %v; listed %+v", p, e.Name, got, err, e)
		}
		out = append(out, e.Name)
	}
	return out
}

func TestVariantReadDir(t *testing.T) {
	env := vfstest.New(t, hero(), file(2, "a/b.jpg", 10), file(3, "-", 10), file(4, "", 10), file(5, "README", 10))
	tests := []struct {
		path string
		want []string
	}{
		{"files/" + uuid(1), []string{"hero.jpg", "-"}},
		{"files/" + uuid(1) + "/-", nil},
		{"files/" + uuid(1) + "/-/resize/100x", []string{"hero.jpg"}},
		{"files/" + uuid(1) + "/-/resize/100x/-/format/webp", []string{"hero.webp"}},
		{"files/" + uuid(1) + "/-/format/png", []string{"hero.png"}},
		{"files/" + uuid(1) + "/-/format/jpeg", []string{"hero.jpg"}},
		{"files/" + uuid(1) + "/-/format/auto", []string{"hero.jpg"}},
		{"files/" + uuid(1) + "/-/preview/-/json", []string{"hero.json"}},
		{"files/" + uuid(2) + "/-/format/webp", []string{"a／b.webp"}},
		{"files/" + uuid(3), []string{"unnamed", "-"}},
		{"files/" + uuid(4) + "/-/preview", []string{"unnamed"}},
		{"files/" + uuid(5) + "/-/format/webp", []string{"README.webp"}},
	}
	for _, tt := range tests {
		if got := names(t, env, tt.path); !slices.Equal(got, tt.want) {
			t.Errorf("readdir %s = %q, want %q", tt.path, got, tt.want)
		}
	}
	// One HEAD per distinct variant listed, none for files/<uuid>/ and -/.
	methods(t, env, 9, 0)
}

func TestVariantContent(t *testing.T) {
	env := vfstest.New(t, hero())
	n := vfs.Node{Kind: vfs.KindVariant, UUID: uuid(1), Ops: webp}
	body, _, _ := env.Fake.Variant(uuid(1), webp)
	if got := readAll(t, env.FS, n, 128<<10); !bytes.Equal(got, body) {
		t.Fatalf("read %d bytes, content mismatch", len(got))
	}
	env.Cache.Wait()
	if _, b := env.Fake.CDNStats(); b != int64(len(body)) {
		t.Errorf("CDN bytes = %d, want the variant size %d", b, len(body))
	}
	_, gets := env.Fake.CDNMethods()
	readAll(t, env.FS, n, 1<<20)
	methods(t, env, 1, gets)
}

func TestBadVariantIsEINVAL(t *testing.T) {
	env := vfstest.New(t, hero())
	ctx := context.Background()
	ops := "-/resize/" + fake.BadArg + "/"
	dir := "files/" + uuid(1) + "/" + ops
	if _, err := env.FS.Walk(ctx, dir+"hero.jpg"); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("stat: err = %v, want EINVAL", err)
	}
	// One HEAD, and one GET for the CDN's message.
	methods(t, env, 1, 1)
	d, err := env.FS.Walk(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.FS.ReadDir(ctx, d.Node); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("readdir: err = %v, want EINVAL", err)
	}
	n := vfs.Node{Kind: vfs.KindVariant, UUID: uuid(1), Ops: ops}
	if _, err := env.FS.ReadAt(ctx, n, make([]byte, 10), 0); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("read: err = %v, want EINVAL", err)
	}
	methods(t, env, 1, 1)
}

func TestVariantExpiresAfterADay(t *testing.T) {
	env := vfstest.New(t, hero())
	var clock atomic.Int64
	clock.Store(t0.UnixNano())
	vfs.SetClock(env.FS, func() time.Time { return time.Unix(0, clock.Load()) })
	advance := func(d time.Duration) { clock.Add(int64(d)) }
	ctx := context.Background()
	n := vfs.Node{Kind: vfs.KindVariant, UUID: uuid(1), Ops: webp}

	first, _, _ := env.Fake.Variant(uuid(1), webp)
	readAll(t, env.FS, n, 1<<20)
	env.Cache.Wait()
	_, gets := env.Fake.CDNMethods()
	attr, err := env.FS.Getattr(ctx, n)
	if err != nil || !attr.Mtime.Equal(t0) {
		t.Fatalf("attr = %+v, %v", attr, err)
	}

	advance(23 * time.Hour)
	if _, err := env.FS.Getattr(ctx, n); err != nil {
		t.Fatal(err)
	}
	methods(t, env, 1, gets)

	// Expired and unchanged: one HEAD, and the cached blocks stay valid.
	advance(2 * time.Hour)
	if got := readAll(t, env.FS, n, 1<<20); !bytes.Equal(got, first) {
		t.Fatal("content changed")
	}
	methods(t, env, 2, gets)
	if attr, _ := env.FS.Getattr(ctx, n); !attr.Mtime.Equal(t0) {
		t.Errorf("mtime moved to %v for unchanged content", attr.Mtime)
	}

	// Expired and changed: the old blocks go, the new content is fetched.
	env.Fake.ChangeVariants()
	advance(25 * time.Hour)
	second, _, _ := env.Fake.Variant(uuid(1), webp)
	if bytes.Equal(first, second) {
		t.Fatal("fake variant did not change")
	}
	if got := readAll(t, env.FS, n, 1<<20); !bytes.Equal(got, second) {
		t.Fatal("served stale content after the variant changed")
	}
	env.Cache.Wait()
	if attr, _ := env.FS.Getattr(ctx, n); !attr.Mtime.Equal(t0.Add(50 * time.Hour)) {
		t.Errorf("mtime = %v, want the time the change was seen", attr.Mtime)
	}
	if h, g := env.Fake.CDNMethods(); h != 3 || g <= gets {
		t.Errorf("CDN requests: %d HEAD, %d GET after the change", h, g)
	}
	blocks := (int64(len(second)) + cache.BlockSize - 1) / cache.BlockSize
	if used := env.Cache.Stats().Used; used != int64(len(second)) && used != blocks*cache.BlockSize {
		t.Errorf("cache holds %d bytes, want only the new content's %d", used, len(second))
	}
}

func TestVariantCDNNotFoundCallsMissing(t *testing.T) {
	env := vfstest.New(t, hero())
	env.Fake.Remove(uuid(1))
	missing := make(chan string, 1)
	fs := vfs.New(env.Index, env.Cache, env.CDN, func(u string) { missing <- u })
	if _, err := fs.Walk(context.Background(), "files/"+uuid(1)+"/"+webp+"hero.webp"); !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("err = %v, want ENOENT", err)
	}
	select {
	case got := <-missing:
		if got != uuid(1) {
			t.Errorf("missing(%q)", got)
		}
	case <-time.After(time.Second):
		t.Error("missing hook not called")
	}
}
