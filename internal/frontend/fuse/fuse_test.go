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
