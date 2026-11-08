package vfs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/alxgsv/ucfs/internal/cache"
	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/vfs"
	"github.com/alxgsv/ucfs/internal/vfs/vfstest"
)

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func uuid(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func file(i int, name string, size int64) fake.File {
	return fake.File{UUID: uuid(i), Name: name, Size: size, MIME: "application/octet-stream", Uploaded: t0.Add(time.Duration(i) * time.Second)}
}

const nfd = "café.txt"

func fixtures() []fake.File {
	return []fake.File{
		file(1, "photo.jpg", 3*cache.BlockSize+17),
		file(2, nfd, 10),
		file(3, "dir/sub.bin", 5),
		file(4, "", 6),
		file(5, strings.Repeat("é", 120), 7),
	}
}

func TestListingAndStatCostNoCDN(t *testing.T) {
	env := vfstest.New(t, fixtures()...)
	ctx := context.Background()
	fs := env.FS

	var walk func(n vfs.Node, depth int)
	count := 0
	walk = func(n vfs.Node, depth int) {
		entries, err := fs.ReadDir(ctx, n)
		if err != nil {
			t.Fatalf("readdir %v: %v", n, err)
		}
		for _, e := range entries {
			count++
			got, err := fs.Lookup(ctx, n, e.Name)
			if err != nil || got.Node != e.Node {
				t.Fatalf("lookup %q: %v %v", e.Name, got.Node, err)
			}
			if _, err := fs.Getattr(ctx, e.Node); err != nil {
				t.Fatalf("getattr %q: %v", e.Name, err)
			}
			if e.Node.IsDir() {
				walk(e.Node, depth+1)
			}
		}
	}
	walk(fs.Root(), 0)
	if count != 1+5 {
		t.Errorf("walked %d entries, want 6", count)
	}
	if reqs, _ := env.Fake.CDNStats(); reqs != 0 {
		t.Errorf("ls -lR equivalent made %d CDN requests", reqs)
	}
}

func TestNames(t *testing.T) {
	env := vfstest.New(t, fixtures()...)
	ctx := context.Background()
	files := vfs.Node{Kind: vfs.KindFilesDir}
	entries, err := env.FS.ReadDir(ctx, files)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, e := range entries {
		names[e.Node.UUID] = e.Name
		if len(e.Name) > 255 || !utf8.ValidString(e.Name) || strings.ContainsAny(e.Name, "/\x00") {
			t.Errorf("bad name %q", e.Name)
		}
	}
	if want := uuid(3) + "_dir／sub.bin"; names[uuid(3)] != want {
		t.Errorf("slash name = %q, want %q", names[uuid(3)], want)
	}
	if want := uuid(4) + "_unnamed"; names[uuid(4)] != want {
		t.Errorf("empty name = %q, want %q", names[uuid(4)], want)
	}

	tests := []struct {
		name string
		want vfs.Kind
		err  error
	}{
		{uuid(2) + "_" + nfd, vfs.KindFile, nil},
		{uuid(2) + "_café.txt", vfs.KindFile, nil}, // NFC form of an NFD name
		{uuid(2) + "_other.txt", 0, syscall.ENOENT},
		{uuid(2), vfs.KindFileDir, nil},
		{uuid(99) + "_x", 0, syscall.ENOENT},
		{".DS_Store", 0, syscall.ENOENT},
		{names[uuid(5)], vfs.KindFile, nil},
	}
	for _, tt := range tests {
		e, err := env.FS.Lookup(ctx, files, tt.name)
		if !errors.Is(err, tt.err) || (err == nil && e.Node.Kind != tt.want) {
			t.Errorf("lookup %q = %v, %v; want kind %v, err %v", tt.name, e.Node.Kind, err, tt.want, tt.err)
		}
	}
	if e, err := env.FS.Walk(ctx, "files/"+uuid(1)+"_photo.jpg"); err != nil || e.Attr.Size != 3*cache.BlockSize+17 {
		t.Errorf("walk: %+v %v", e, err)
	}
}

func readAll(t *testing.T, fs *vfs.FS, n vfs.Node, chunk int) []byte {
	t.Helper()
	var out []byte
	buf := make([]byte, chunk)
	for off := int64(0); ; {
		k, err := fs.ReadAt(context.Background(), n, buf, off)
		out = append(out, buf[:k]...)
		off += int64(k)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestReadContentFetchesEachByteOnce(t *testing.T) {
	f := file(1, "photo.jpg", 5*cache.BlockSize+17)
	env := vfstest.New(t, f)
	got := readAll(t, env.FS, vfs.Node{Kind: vfs.KindFile, UUID: f.UUID}, 128<<10)
	env.Cache.Wait()
	if !bytes.Equal(got, fake.Content(f.UUID, f.Size)) {
		t.Fatal("content mismatch")
	}
	if _, cdnBytes := env.Fake.CDNStats(); cdnBytes != f.Size {
		t.Errorf("CDN bytes = %d, want file size %d", cdnBytes, f.Size)
	}
	again := readAll(t, env.FS, vfs.Node{Kind: vfs.KindFile, UUID: f.UUID}, 1<<20)
	if _, cdnBytes := env.Fake.CDNStats(); cdnBytes != f.Size || !bytes.Equal(again, got) {
		t.Errorf("second read went to the CDN: %d bytes", cdnBytes)
	}
}

func TestTwoReadersDoNotStall(t *testing.T) {
	slow := file(1, "slow.bin", 8*cache.BlockSize)
	quick := file(2, "fast.bin", cache.BlockSize)
	env := vfstest.New(t, slow, quick)
	env.Fake.SetLatency(slow.UUID, 300*time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		readAll(t, env.FS, vfs.Node{Kind: vfs.KindFile, UUID: slow.UUID}, 1<<20)
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	readAll(t, env.FS, vfs.Node{Kind: vfs.KindFile, UUID: quick.UUID}, 1<<20)
	if _, err := env.FS.Walk(context.Background(), "files"); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Errorf("second reader took %v while the first waited on the CDN", d)
	}
	<-done
}

func TestCDNNotFoundCallsMissing(t *testing.T) {
	f := file(1, "gone.bin", 10)
	env := vfstest.New(t, f)
	env.Fake.Remove(f.UUID)
	missing := make(chan string, 1)
	fs := vfs.New(env.Index, env.Cache, env.CDN, func(u string) { missing <- u })
	_, err := fs.ReadAt(context.Background(), vfs.Node{Kind: vfs.KindFile, UUID: f.UUID}, make([]byte, 10), 0)
	if !errors.Is(err, syscall.ENOENT) {
		t.Fatalf("err = %v, want ENOENT", err)
	}
	select {
	case got := <-missing:
		if got != f.UUID {
			t.Errorf("missing(%q)", got)
		}
	case <-time.After(time.Second):
		t.Error("missing hook not called")
	}
}
