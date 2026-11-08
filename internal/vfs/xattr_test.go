package vfs_test

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/spool"
	"github.com/alxgsv/ucfs/internal/vfs"
	"github.com/alxgsv/ucfs/internal/vfs/vfstest"
)

func apiFile(t *testing.T, env *vfstest.Env, uuid string) api.File {
	t.Helper()
	f, err := env.Client.Info(context.Background(), uuid)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func getx(t *testing.T, env *vfstest.Env, p, name string) (string, error) {
	t.Helper()
	v, err := env.FS.Getxattr(context.Background(), walk(t, env, p).Node, name)
	return string(v), err
}

func listx(t *testing.T, env *vfstest.Env, p string) []string {
	t.Helper()
	names, err := env.FS.Listxattr(context.Background(), walk(t, env, p).Node)
	if err != nil {
		t.Fatalf("listxattr %s: %v", p, err)
	}
	return names
}

func photo() fake.File {
	return fake.File{UUID: uuid(1), Name: "photo.jpg", Size: 1000, MIME: "image/jpeg", Uploaded: t0, Stored: true,
		Width: 800, Height: 600, Metadata: map[string]string{"camera": "Canon", "ucfs:path": "/p/photo.jpg"}, Tags: []string{"cat"}}
}

func TestReadOnlyXattrs(t *testing.T) {
	env := vfstest.New(t, photo(), fake.File{UUID: uuid(2), Name: "big.bin", Size: 10 << 20, Uploaded: t0})
	p := "files/" + uuid(1) + "_photo.jpg"
	want := []string{vfs.XattrUUID, vfs.XattrCDNURL, vfs.XattrMIME, vfs.XattrMD5, vfs.XattrWidth, vfs.XattrHeight,
		vfs.XattrStored, vfs.XattrTags, vfs.XattrMeta + "camera", vfs.XattrMeta + "ucfs:path"}
	if got := listx(t, env, p); !slices.Equal(got, want) {
		t.Errorf("listxattr = %q, want %q", got, want)
	}
	sum := fmt.Sprintf("%x", md5.Sum(fake.Content(uuid(1), 1000)))
	values := map[string]string{
		vfs.XattrUUID: uuid(1), vfs.XattrCDNURL: env.Fake.CDNBase() + "/" + uuid(1) + "/", vfs.XattrMIME: "image/jpeg",
		vfs.XattrMD5: sum, vfs.XattrWidth: "800", vfs.XattrHeight: "600", vfs.XattrStored: "true",
		vfs.XattrTags: "cat", vfs.XattrMeta + "camera": "Canon", vfs.XattrMeta + "ucfs:path": "/p/photo.jpg",
	}
	before := env.Fake.Requests()
	for _, view := range []string{p, "tree/p/photo.jpg", "by-date/2026/03/04/" + uuid(1) + "_photo.jpg"} {
		for name, v := range values {
			if got, err := getx(t, env, view, name); err != nil || got != v {
				t.Errorf("%s %s = %q, %v; want %q", view, name, got, err, v)
			}
		}
	}
	if n := env.Fake.Requests() - before; n != 0 {
		t.Errorf("reading fresh attributes made %d REST requests", n)
	}
	if heads, _ := env.Fake.CDNMethods(); heads != 1 {
		t.Errorf("md5 cost %d HEADs, want one, kept", heads)
	}
	if !slices.Equal(listx(t, env, p), want) {
		t.Error("listing changed")
	}
	for _, name := range []string{"user.uc.nope", "security.capability", "system.posix_acl_access", "user.other"} {
		if _, err := getx(t, env, p, name); !errors.Is(err, syscall.ENODATA) {
			t.Errorf("getxattr %s = %v, want ENODATA", name, err)
		}
	}
	if got := listx(t, env, "files"); len(got) != 0 {
		t.Errorf("a directory lists %q", got)
	}
	// A file of 10 MiB or more may have been uploaded in parts, whose ETag
	// is no MD5, so md5 is not listed until known; reading it asks the CDN.
	big := "files/" + uuid(2) + "_big.bin"
	if got := listx(t, env, big); slices.Contains(got, vfs.XattrMD5) {
		t.Errorf("a 10 MiB file lists md5: %q", got)
	}
	if got, err := getx(t, env, big, vfs.XattrMD5); err != nil || len(got) != 32 {
		t.Errorf("md5 of a base upload of 10 MiB = %q, %v", got, err)
	}
	if got := listx(t, env, big); !slices.Contains(got, vfs.XattrMD5) {
		t.Errorf("a known md5 is not listed: %q", got)
	}
	if err := env.FS.Setxattr(context.Background(), walk(t, env, p).Node, vfs.XattrMeta+"x", []byte("y"), 0); !errors.Is(err, syscall.EROFS) {
		t.Errorf("setxattr on a read-only mount = %v, want EROFS", err)
	}
}

func TestVariantXattrs(t *testing.T) {
	env := vfstest.New(t, photo())
	p := "files/" + uuid(1) + "/-/resize/10x/photo.jpg"
	if got := listx(t, env, p); !slices.Equal(got, []string{vfs.XattrUUID, vfs.XattrCDNURL, vfs.XattrMIME}) {
		t.Errorf("variant lists %q", got)
	}
	if got, _ := getx(t, env, p, vfs.XattrCDNURL); got != env.Fake.CDNBase()+"/"+uuid(1)+"/-/resize/10x/" {
		t.Errorf("variant cdn_url = %q", got)
	}
}

func TestMetadataXattrs(t *testing.T) {
	env := writable(t, nil, photo())
	ctx := context.Background()
	p := "files/" + uuid(1) + "_photo.jpg"
	n := walk(t, env, p).Node
	set := func(name, v string, flags int) error { return env.FS.Setxattr(ctx, n, name, []byte(v), flags) }

	before := env.Fake.Requests()
	if err := set(vfs.XattrMeta+"lens", "50mm", 0); err != nil {
		t.Fatal(err)
	}
	if n := env.Fake.Requests() - before; n != 1 {
		t.Errorf("setxattr made %d requests, want one PUT", n)
	}
	if f, _ := env.Fake.File(uuid(1)); f.Metadata["lens"] != "50mm" {
		t.Errorf("API metadata = %v", f.Metadata)
	}
	if got, _ := getx(t, env, p, vfs.XattrMeta+"lens"); got != "50mm" {
		t.Errorf("read back %q", got)
	}
	if err := set(vfs.XattrMeta+"lens", "35mm", vfs.XattrCreate); !errors.Is(err, syscall.EEXIST) {
		t.Errorf("XATTR_CREATE of an existing key = %v", err)
	}
	if err := set(vfs.XattrMeta+"iso", "100", vfs.XattrReplace); !errors.Is(err, syscall.ENODATA) {
		t.Errorf("XATTR_REPLACE of a missing key = %v", err)
	}
	if err := env.FS.Removexattr(ctx, n, vfs.XattrMeta+"lens"); err != nil {
		t.Fatal(err)
	}
	if f, _ := env.Fake.File(uuid(1)); f.Metadata["lens"] != "" {
		t.Errorf("key still set: %v", f.Metadata)
	}
	if _, err := getx(t, env, p, vfs.XattrMeta+"lens"); !errors.Is(err, syscall.ENODATA) {
		t.Errorf("removed key reads %v", err)
	}
	if err := env.FS.Removexattr(ctx, n, vfs.XattrMeta+"lens"); !errors.Is(err, syscall.ENODATA) {
		t.Errorf("removing a missing key = %v, want ENODATA", err)
	}

	tests := []struct {
		name, value string
		want        error
	}{
		{vfs.XattrMeta + strings.Repeat("k", 64), "v", nil},
		{vfs.XattrMeta + strings.Repeat("k", 65), "v", syscall.ENAMETOOLONG},
		{vfs.XattrMeta + "a b", "v", syscall.EINVAL},
		{vfs.XattrMeta + "a/b", "v", syscall.EINVAL},
		{vfs.XattrMeta + "", "v", syscall.EINVAL},
		{vfs.XattrMeta + "ucfs:path", "/elsewhere.jpg", syscall.EPERM},
		{vfs.XattrMeta + "big", strings.Repeat("é", 512), nil},
		{vfs.XattrMeta + "bigger", strings.Repeat("é", 513), syscall.E2BIG},
		{vfs.XattrMeta + "empty", "", syscall.EINVAL},
		{vfs.XattrMeta + "bad", "\xff", syscall.EINVAL},
		{vfs.XattrUUID, "x", syscall.EPERM},
		{vfs.XattrMD5, "x", syscall.EPERM},
		{"user.other", "x", syscall.ENOTSUP},
		{"security.selinux", "x", syscall.ENOTSUP},
	}
	for _, tt := range tests {
		if err := set(tt.name, tt.value, 0); !errors.Is(err, tt.want) && (err != nil || tt.want != nil) {
			t.Errorf("setxattr %.40s = %v, want %v", tt.name, err, tt.want)
		}
	}
	if err := env.FS.Removexattr(ctx, n, vfs.XattrMeta+"ucfs:path"); !errors.Is(err, syscall.EPERM) {
		t.Errorf("removing ucfs:path = %v, want EPERM", err)
	}

	// The 50-key limit comes from the API, whose answer is ENOSPC.
	f, _ := env.Fake.File(uuid(1))
	for i := len(f.Metadata); i < 50; i++ {
		if err := set(fmt.Sprintf("%sk%d", vfs.XattrMeta, i), "v", 0); err != nil {
			t.Fatal(err)
		}
	}
	if err := set(vfs.XattrMeta+"one-more", "v", 0); !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("51st key = %v, want ENOSPC", err)
	}
	if err := set(vfs.XattrMeta+"k49", "w", 0); err != nil {
		t.Errorf("replacing a key at the limit = %v", err)
	}
}

func TestTagXattrs(t *testing.T) {
	env := writable(t, nil, photo(), file(2, "other.jpg", 5))
	configure(env, vfs.AllViews...)
	ctx := context.Background()
	p := "files/" + uuid(1) + "_photo.jpg"
	n := walk(t, env, p).Node
	if err := env.FS.Setxattr(ctx, n, vfs.XattrTags, []byte(" B,a , b,,"), 0); err != nil {
		t.Fatal(err)
	}
	if f, _ := env.Fake.File(uuid(1)); !slices.Equal(f.Tags, []string{"b", "a"}) {
		t.Errorf("API tags = %q", f.Tags)
	}
	if got, _ := getx(t, env, p, vfs.XattrTags); got != "b,a" {
		t.Errorf("read back %q", got)
	}
	if got := readdir(t, env, "tags"); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("tags after setxattr = %q", got)
	}
	for v, want := range map[string]error{
		"café":                         syscall.EINVAL,
		"a b":                          syscall.EINVAL,
		strings.Repeat("t", 101):       syscall.ENAMETOOLONG,
		strings.Repeat("t,", 50) + "x": nil,
	} {
		if err := env.FS.Setxattr(ctx, n, vfs.XattrTags, []byte(v), 0); !errors.Is(err, want) && (err != nil || want != nil) {
			t.Errorf("tags %.20q = %v, want %v", v, err, want)
		}
	}
	var many []string
	for i := range 51 {
		many = append(many, fmt.Sprintf("t%d", i))
	}
	if err := env.FS.Setxattr(ctx, n, vfs.XattrTags, []byte(strings.Join(many, ",")), 0); !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("51 tags = %v, want ENOSPC", err)
	}
	if err := env.FS.Removexattr(ctx, n, vfs.XattrTags); err != nil {
		t.Fatal(err)
	}
	if f, _ := env.Fake.File(uuid(1)); len(f.Tags) != 0 {
		t.Errorf("tags left: %q", f.Tags)
	}
	if got := listx(t, env, p); slices.Contains(got, vfs.XattrTags) {
		t.Errorf("cleared tags still listed: %q", got)
	}
	if err := env.FS.Removexattr(ctx, n, vfs.XattrTags); !errors.Is(err, syscall.ENODATA) {
		t.Errorf("clearing no tags = %v, want ENODATA", err)
	}
	if got := readdir(t, env, "tags"); len(got) != 0 {
		t.Errorf("tags/ after clearing = %q", got)
	}
}

func TestLazyRefresh(t *testing.T) {
	env := writable(t, nil, photo())
	ctx := context.Background()
	now := time.Now()
	vfs.SetClock(env.FS, func() time.Time { return now })
	p := "files/" + uuid(1) + "_photo.jpg"
	env.Fake.SetMetadata(uuid(1), "camera", "Nikon")
	env.Fake.SetMetadata(uuid(1), "lens", "35mm")

	before := env.Fake.Requests()
	if got, _ := getx(t, env, p, vfs.XattrMeta+"camera"); got != "Canon" {
		t.Errorf("within the TTL camera = %q, want the indexed Canon", got)
	}
	if n := env.Fake.Requests() - before; n != 0 {
		t.Errorf("a fresh entry made %d requests", n)
	}

	now = now.Add(vfs.DefaultMetaTTL + time.Second)
	if got := listx(t, env, p); slices.Contains(got, vfs.XattrMeta+"lens") {
		t.Error("listxattr refreshed: it must stay off the network")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if got, err := getx(t, env, p, vfs.XattrMeta+"camera"); got != "Nikon" {
				t.Errorf("after the TTL camera = %q, %v; want Nikon", got, err)
			}
		})
	}
	wg.Wait()
	if n := env.Fake.Requests() - before; n != 1 {
		t.Errorf("eight readers of a stale entry made %d requests, want one", n)
	}
	if got := listx(t, env, p); !slices.Contains(got, vfs.XattrMeta+"lens") {
		t.Errorf("refreshed listing = %q", got)
	}
	if st := env.FS.MetaStats(); st.Refreshes != 1 || st.RefreshErrors != 0 {
		t.Errorf("stats = %+v", st)
	}

	// Resolve refreshes too, and a file removed by another client is gone.
	now = now.Add(vfs.DefaultMetaTTL + time.Second)
	env.Fake.SetTags(uuid(1), "dog")
	r, err := env.FS.Resolve(ctx, p, nil)
	if err != nil || !slices.Equal(r.Tags, []string{"dog"}) || r.Metadata["lens"] != "35mm" || r.CDNURL == "" {
		t.Errorf("resolve = %+v, %v", r, err)
	}
	now = now.Add(vfs.DefaultMetaTTL + time.Second)
	env.Fake.Remove(uuid(1))
	n := walk(t, env, p).Node
	if _, err := env.FS.Getxattr(ctx, n, vfs.XattrUUID); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("a file removed by another client reads %v, want ENOENT once refreshed", err)
	}
	if _, err := env.FS.Walk(ctx, p); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("walk after the refresh saw the removal = %v, want ENOENT", err)
	}
}

func TestPendingXattrs(t *testing.T) {
	env := writable(t, nil, file(1, "old.jpg", 5))
	ctx := context.Background()
	tree := walk(t, env, "tree")
	e, err := env.FS.Create(ctx, tree.Node, "new.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.FS.WriteAt(ctx, e.Node, []byte("hello"), 0); err != nil {
		t.Fatal(err)
	}
	if got, err := env.FS.Getxattr(ctx, e.Node, vfs.XattrUUID); err != nil || len(got) != 0 {
		t.Errorf("pending uuid = %q, %v; want empty", got, err)
	}
	for name, v := range map[string]string{vfs.XattrMeta + "camera": "Canon", vfs.XattrMeta + "ucfs:test": "2", vfs.XattrTags: "x,Y"} {
		if err := env.FS.Setxattr(ctx, e.Node, name, []byte(v), 0); err != nil {
			t.Fatalf("queue %s: %v", name, err)
		}
	}
	if err := env.FS.Setxattr(ctx, e.Node, vfs.XattrMeta+"gone", []byte("v"), 0); err != nil {
		t.Fatal(err)
	}
	if err := env.FS.Removexattr(ctx, e.Node, vfs.XattrMeta+"gone"); err != nil {
		t.Fatal(err)
	}
	want := []string{vfs.XattrUUID, vfs.XattrTags, vfs.XattrMeta + "camera", vfs.XattrMeta + "ucfs:test"}
	if got, _ := env.FS.Listxattr(ctx, e.Node); !slices.Equal(got, want) {
		t.Errorf("pending lists %q, want %q", got, want)
	}
	if r, _ := env.FS.Resolve(ctx, "tree/new.jpg", nil); r.State != vfs.StatePending || r.Metadata["camera"] != "Canon" {
		t.Errorf("pending resolve = %+v", r)
	}
	if err := env.FS.Flush(ctx, e.Node); err != nil {
		t.Fatal(err)
	}
	r, err := env.FS.Resolve(ctx, "tree/new.jpg", nil)
	if err != nil || r.UUID == "" {
		t.Fatalf("resolve = %+v, %v", r, err)
	}
	f, _ := env.Fake.File(r.UUID)
	if f.Metadata["camera"] != "Canon" || f.Metadata["ucfs:test"] != "2" || f.Metadata["ucfs:path"] != "/new.jpg" ||
		!slices.Equal(f.Tags, []string{"x", "y"}) || f.Metadata["gone"] != "" {
		t.Errorf("uploaded with metadata %v, tags %q", f.Metadata, f.Tags)
	}
	if got, _ := getx(t, env, "tree/new.jpg", vfs.XattrMeta+"camera"); got != "Canon" {
		t.Errorf("after the upload camera = %q", got)
	}
	if got, _ := getx(t, env, "tree/new.jpg", vfs.XattrMD5); got != fmt.Sprintf("%x", md5.Sum([]byte("hello"))) {
		t.Errorf("md5 of an upload = %q", got)
	}
	if heads, _ := env.Fake.CDNMethods(); heads != 0 {
		t.Errorf("the md5 of an upload cost %d HEADs", heads)
	}
}

func TestPendingXattrsWhileUploading(t *testing.T) {
	env := vfstest.New(t)
	env.Writable(t, spool.Options{}, &vfs.Writes{})
	env.Fake.SetUploadLatency(300 * time.Millisecond)
	ctx := context.Background()
	e, err := env.FS.Create(ctx, walk(t, env, "tree").Node, "slow.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.FS.WriteAt(ctx, e.Node, []byte("x"), 0); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- env.FS.Flush(ctx, e.Node) }()
	deadline := time.Now().Add(5 * time.Second)
	for env.Spools.Stats().Uploading == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := env.FS.Setxattr(ctx, e.Node, vfs.XattrMeta+"late", []byte("v"), 0); !errors.Is(err, syscall.EAGAIN) {
		t.Errorf("setxattr during the upload = %v, want EAGAIN", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
