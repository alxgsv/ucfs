//go:build linux

package fuse

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/spool"
	"github.com/alxgsv/ucfs/internal/vfs"
	"github.com/alxgsv/ucfs/internal/vfs/vfstest"
)

func getxattr(t *testing.T, p, name string) (string, error) {
	t.Helper()
	n, err := syscall.Getxattr(p, name, nil)
	if err != nil {
		return "", err
	}
	buf := make([]byte, n)
	n, err = syscall.Getxattr(p, name, buf)
	return string(buf[:n]), err
}

func listxattr(t *testing.T, p string) []string {
	t.Helper()
	n, err := syscall.Listxattr(p, nil)
	if err != nil {
		t.Fatalf("listxattr %s: %v", p, err)
	}
	buf := make([]byte, n)
	if n, err = syscall.Listxattr(p, buf); err != nil {
		t.Fatalf("listxattr %s: %v", p, err)
	}
	return strings.Split(strings.TrimSuffix(string(buf[:n]), "\x00"), "\x00")
}

func TestXattrs(t *testing.T) {
	env := vfstest.New(t, fake.File{UUID: uuid(1), Name: "photo.jpg", Size: 100, MIME: "image/jpeg", Uploaded: t0, Stored: true,
		Width: 800, Height: 600, Metadata: map[string]string{"ucfs:path": "/p/photo.jpg"}})
	env.Writable(t, spool.Options{Metadata: map[string]string{"ucfs:test": "1"}}, &vfs.Writes{})
	env.FS.Configure(vfs.Options{Views: vfs.AllViews, DateZone: time.UTC, CDNBase: env.Fake.CDNBase(), Remote: env.Client})
	dir := mount(t, env)
	p := filepath.Join(dir, "tree", "p", "photo.jpg")

	want := []string{vfs.XattrUUID, vfs.XattrCDNURL, vfs.XattrMIME, vfs.XattrMD5, vfs.XattrWidth, vfs.XattrHeight,
		vfs.XattrStored, vfs.XattrMeta + "ucfs:path"}
	if got := listxattr(t, p); !slices.Equal(got, want) {
		t.Errorf("listxattr = %q, want %q", got, want)
	}
	for name, v := range map[string]string{vfs.XattrUUID: uuid(1), vfs.XattrWidth: "800", vfs.XattrStored: "true"} {
		if got, err := getxattr(t, p, name); err != nil || got != v {
			t.Errorf("%s = %q, %v; want %q", name, got, err, v)
		}
	}
	if _, err := getxattr(t, p, "security.capability"); !errors.Is(err, syscall.ENODATA) {
		t.Errorf("security.capability = %v, want ENODATA", err)
	}

	// setfattr -n user.uc.meta.camera -v Canon, then -x.
	before := env.Fake.Requests()
	if err := syscall.Setxattr(p, vfs.XattrMeta+"camera", []byte("Canon"), 0); err != nil {
		t.Fatal(err)
	}
	if n := env.Fake.Requests() - before; n != 1 {
		t.Errorf("setxattr made %d requests, want one PUT", n)
	}
	if f, _ := env.Fake.File(uuid(1)); f.Metadata["camera"] != "Canon" {
		t.Errorf("API metadata = %v", f.Metadata)
	}
	if got, _ := getxattr(t, p, vfs.XattrMeta+"camera"); got != "Canon" {
		t.Errorf("read back %q", got)
	}
	if err := syscall.Removexattr(p, vfs.XattrMeta+"camera"); err != nil {
		t.Fatal(err)
	}
	if f, _ := env.Fake.File(uuid(1)); f.Metadata["camera"] != "" {
		t.Errorf("key left: %v", f.Metadata)
	}
	for _, tt := range []struct {
		name, value string
		want        syscall.Errno
	}{
		{vfs.XattrUUID, "x", syscall.EPERM},
		{vfs.XattrMeta + strings.Repeat("k", 65), "v", syscall.ENAMETOOLONG},
		{vfs.XattrMeta + "big", strings.Repeat("v", 513), syscall.E2BIG},
		{vfs.XattrMeta + "ucfs:path", "/x.jpg", syscall.EPERM},
	} {
		if err := syscall.Setxattr(p, tt.name, []byte(tt.value), 0); !errors.Is(err, tt.want) {
			t.Errorf("setxattr %.30s = %v, want %v", tt.name, err, tt.want)
		}
	}
	if err := syscall.Removexattr(p, vfs.XattrMeta+"camera"); !errors.Is(err, syscall.ENODATA) {
		t.Errorf("removing a missing key = %v, want ENODATA", err)
	}

	// Tags set through the mount show in tags/ at once.
	if err := syscall.Setxattr(p, vfs.XattrTags, []byte("approved,cat"), 0); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "tags", "approved"))
	if err != nil || len(entries) != 1 || entries[0].Name() != uuid(1)+"_photo.jpg" {
		t.Errorf("tags/approved = %v, %v", entries, err)
	}

	// cp --preserve=xattr sets attributes on the open destination before
	// close; they ride along with the upload.
	dst := filepath.Join(dir, "tree", "copy.jpg")
	f, err := os.Create(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte("copied")); err != nil {
		t.Fatal(err)
	}
	if got, err := getxattr(t, dst, vfs.XattrUUID); err != nil || got != "" {
		t.Errorf("pending uuid = %q, %v; want empty", got, err)
	}
	for name, v := range map[string]string{vfs.XattrMeta + "camera": "Canon", vfs.XattrTags: "copied"} {
		if err := syscall.Setxattr(dst, name, []byte(v), 0); err != nil {
			t.Fatalf("setxattr %s on the open file: %v", name, err)
		}
	}
	if err := syscall.Setxattr(dst, vfs.XattrUUID, []byte(uuid(1)), 0); !errors.Is(err, syscall.EPERM) {
		t.Errorf("copying user.uc.uuid = %v, want EPERM", err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	id, err := getxattr(t, dst, vfs.XattrUUID)
	if err != nil || id == "" {
		t.Fatalf("uuid after close = %q, %v", id, err)
	}
	up, _ := env.Fake.File(id)
	if up.Metadata["camera"] != "Canon" || up.Metadata["ucfs:path"] != "/copy.jpg" || !slices.Equal(up.Tags, []string{"copied"}) {
		t.Errorf("uploaded with metadata %v, tags %q", up.Metadata, up.Tags)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "by-date", t0.Format("2006/01/02"), uuid(1)+"_photo.jpg")); err != nil ||
		!bytes.Equal(got, fake.Content(uuid(1), 100)) {
		t.Errorf("by-date read: %v", err)
	}
}
