//go:build linux

package fuse

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/vfs/vfstest"
)

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func uuid(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func mount(t *testing.T, env *vfstest.Env) string {
	t.Helper()
	if f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0); err != nil {
		t.Skipf("no usable /dev/fuse: %v", err)
	} else {
		_ = f.Close()
	}
	dir := t.TempDir()
	srv, err := Mount(dir, env.FS, false)
	if err != nil {
		t.Skipf("cannot mount FUSE here: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Unmount(); err != nil {
			t.Errorf("unmount: %v", err)
		}
	})
	return dir
}

func TestMountListReadAndReject(t *testing.T) {
	files := []fake.File{
		{UUID: uuid(1), Name: "photo.jpg", Size: 3<<20 + 11, Uploaded: t0},
		{UUID: uuid(2), Name: "café.txt", Size: 12, Uploaded: t0.Add(time.Second)},
		{UUID: uuid(3), Name: "slow.bin", Size: 8 << 20, Uploaded: t0.Add(2 * time.Second)},
	}
	env := vfstest.New(t, files...)
	dir := mount(t, env)

	var count int
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if _, err := d.Info(); err != nil {
			return err
		}
		count++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 1+1+len(files) {
		t.Errorf("walked %d entries", count)
	}
	if reqs, _ := env.Fake.CDNStats(); reqs != 0 {
		t.Errorf("ls -lR made %d CDN requests", reqs)
	}

	p := filepath.Join(dir, "files", uuid(1)+"_photo.jpg")
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fake.Content(uuid(1), files[0].Size)) {
		t.Errorf("content mismatch: %d bytes", len(got))
	}
	if _, err := os.Stat(filepath.Join(dir, "files", uuid(2)+"_café.txt")); err != nil {
		t.Errorf("NFC name of an NFD file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "files", uuid(2))); err != nil {
		t.Errorf("files/<uuid>/: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "files", "new.txt"), []byte("x"), 0o644); !errors.Is(err, syscall.EROFS) {
		t.Errorf("write: %v, want EROFS", err)
	}

	env.Fake.SetLatency(uuid(3), 300*time.Millisecond)
	var wg sync.WaitGroup
	wg.Go(func() {
		if _, err := os.ReadFile(filepath.Join(dir, "files", uuid(3)+"_slow.bin")); err != nil {
			t.Error(err)
		}
	})
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	if _, err := os.ReadFile(filepath.Join(dir, "files", uuid(2)+"_café.txt")); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Errorf("second reader took %v during a slow read", d)
	}
	wg.Wait()
}

func TestVariants(t *testing.T) {
	env := vfstest.New(t, fake.File{UUID: uuid(1), Name: "hero.jpg", Size: 3<<20 + 11, MIME: "image/jpeg", Uploaded: t0})
	dir := mount(t, env)
	vdir := filepath.Join(dir, "files", uuid(1), "-", "resize", "1200x", "-", "format", "webp")

	entries, err := os.ReadDir(vdir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "hero.webp" {
		t.Fatalf("readdir = %v, %v; want the one default entry", entries, err)
	}
	body, _, _ := env.Fake.Variant(uuid(1), "-/resize/1200x/-/format/webp/")
	got, err := os.ReadFile(filepath.Join(vdir, "hero.webp"))
	if err != nil || !bytes.Equal(got, body) {
		t.Errorf("read %d bytes, %v; content mismatch", len(got), err)
	}
	if heads, _ := env.Fake.CDNMethods(); heads != 1 {
		t.Errorf("%d HEADs for listing and reading one variant", heads)
	}
	bad := filepath.Join(dir, "files", uuid(1), "-", "resize", fake.BadArg, "hero.jpg")
	if _, err := os.Stat(bad); !errors.Is(err, syscall.EINVAL) {
		t.Errorf("stat of a variant the CDN rejects: %v, want EINVAL", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "files", uuid(1), "-", "nosuchop")); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("stat of an unknown operation: %v, want ENOENT", err)
	}
}
