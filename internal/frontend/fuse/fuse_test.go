//go:build linux

package fuse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/spool"
	"github.com/alxgsv/ucfs/internal/vfs"
	"github.com/alxgsv/ucfs/internal/vfs/vfstest"
)

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func uuid(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func mount(t *testing.T, env *vfstest.Env) string {
	t.Helper()
	// UCFS_MOUNT_TESTS=1, as CI sets, makes a host that cannot mount fail
	// the test rather than skip it.
	cannot := t.Skipf
	if os.Getenv("UCFS_MOUNT_TESTS") == "1" {
		cannot = t.Fatalf
	}
	if f, err := os.OpenFile("/dev/fuse", os.O_RDWR, 0); err != nil {
		cannot("no usable /dev/fuse: %v", err)
	} else {
		_ = f.Close()
	}
	dir := t.TempDir()
	srv, err := Mount(dir, env.FS, false)
	if err != nil {
		cannot("cannot mount FUSE here: %v", err)
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
	// The mount point, its files, tree, by-date, .ucfs and
	// .metadata_never_index, the files in files/ and in tree/_inbox,
	// tree/_inbox itself, the journal, and by-date's year, month and day
	// with the files.
	if count != 1+5+2*len(files)+1+1+3+len(files) {
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
		t.Errorf("write on a read-only mount: %v, want EROFS", err)
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

func writableMount(t *testing.T, files ...fake.File) (*vfstest.Env, string) {
	t.Helper()
	env := vfstest.New(t, files...)
	env.Writable(t, spool.Options{Metadata: map[string]string{"ucfs:test": "1"}, RecentGrace: time.Nanosecond}, &vfs.Writes{})
	return env, mount(t, env)
}

func liveUpload(t *testing.T, env *vfstest.Env, name string) fake.File {
	t.Helper()
	files, _ := env.Index.Files(context.Background())
	var match []fake.File
	for _, f := range files {
		if sf, ok := env.Fake.File(f.UUID); ok && sf.Name == name && !sf.Removed {
			match = append(match, sf)
		}
	}
	if len(match) != 1 {
		t.Fatalf("%d live uploads named %q", len(match), name)
	}
	return match[0]
}

func TestWrites(t *testing.T) {
	env, dir := writableMount(t, fake.File{UUID: uuid(1), Name: "fixture.jpg", Size: 10, Uploaded: t0})
	photos := filepath.Join(dir, "tree", "photos")
	if err := os.Mkdir(photos, 0o755); err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("0123456789"), 300000)
	p := filepath.Join(photos, "photo.jpg")
	// close(2) blocks on the upload, so the file has its uuid when
	// WriteFile returns.
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	up := liveUpload(t, env, "photo.jpg")
	if up.Metadata["ucfs:path"] != "/photos/photo.jpg" {
		t.Errorf("uploaded %+v", up)
	}
	if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read back %d bytes, %v", len(got), err)
	}
	if err := os.Truncate(p, int64(len(body))); err != nil {
		t.Errorf("truncate to the same length: %v", err)
	}
	if err := os.Truncate(p, 5); !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Errorf("truncate an uploaded file to 5 bytes: %v, want EOPNOTSUPP", err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
	if err == nil {
		_, err = f.Write([]byte("more"))
		_ = f.Close()
	}
	if !errors.Is(err, syscall.EOPNOTSUPP) {
		t.Errorf("append to an uploaded file: %v, want EOPNOTSUPP", err)
	}

	// Overwrite in place.
	if err := os.WriteFile(p, []byte("second version"), 0o644); err != nil {
		t.Fatal(err)
	}
	v2 := liveUpload(t, env, "photo.jpg")
	if v2.UUID == up.UUID {
		t.Error("overwrite kept the uuid")
	}
	if old, _ := env.Fake.File(up.UUID); !old.Removed {
		t.Error("overwrite did not delete the old version")
	}

	// vim on Linux saves by renaming: the original to a backup, a new file
	// in its place, then the backup removed.
	if err := os.Rename(p, p+"~"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("third"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p + "~"); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(p); string(got) != "third" {
		t.Errorf("after the save: %q", got)
	}
	if old, _ := env.Fake.File(v2.UUID); !old.Removed {
		t.Error("the backup was not deleted")
	}

	if err := os.WriteFile(filepath.Join(photos, "._photo.jpg"), []byte("AppleDouble"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(photos, filepath.Join(dir, "tree", "pics")); err != nil {
		t.Fatal(err)
	}
	if v3 := liveUpload(t, env, "photo.jpg"); v3.Metadata["ucfs:path"] != "/pics/photo.jpg" {
		t.Errorf("after the directory rename: %+v", v3)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "tree", "pics"))
	if err != nil || len(entries) != 2 {
		t.Errorf("tree/pics = %v, %v", entries, err)
	}
	if err := os.Remove(filepath.Join(dir, "tree", "pics", "photo.jpg")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "tree", "_inbox", "fixture.jpg")); !errors.Is(err, syscall.EPERM) {
		t.Errorf("rm of a fixture: %v, want EPERM", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "files", uuid(1), "x"), nil, 0o644); !errors.Is(err, syscall.EROFS) {
		t.Errorf("create in files/<uuid>/: %v, want EROFS", err)
	}
	if st := env.Fake.UploadStats(); st.Base != 3 {
		t.Errorf("%d uploads, want 3", st.Base)
	}
}

func TestFlushReturnsUploadErrors(t *testing.T) {
	env, dir := writableMount(t)
	env.Fake.RejectUploads(func(name string) bool { return strings.HasSuffix(name, ".exe") })
	err := os.WriteFile(filepath.Join(dir, "tree", "x.exe"), []byte("MZ"), 0o644)
	if !errors.Is(err, syscall.EPERM) {
		t.Errorf("write of a rejected type: %v, want EPERM", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tree", "empty.txt"), nil, 0o644); err != nil {
		t.Errorf("empty file: %v", err)
	}
	if st := env.Fake.UploadStats(); st.Base != 0 {
		t.Errorf("an empty file was uploaded: %+v", st)
	}
}
