package nfs

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"

	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/spool"
	"github.com/alxgsv/ucfs/internal/vfs"
	"github.com/alxgsv/ucfs/internal/vfs/vfstest"
)

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func uuid(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func mount(t *testing.T, env *vfstest.Env) *nfsc.Target {
	t.Helper()
	return mountAs(t, env, uint32(os.Getuid()))
}

// mountAs talks to a server over RPC with AUTH_UNIX credentials of uid, as
// the kernel client does for a local user.
func mountAs(t *testing.T, env *vfstest.Env, uid uint32) *nfsc.Target {
	t.Helper()
	srv, err := Serve(env.FS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	// The client binds a random local port, which may be in use.
	var c *rpc.Client
	for range 5 {
		if c, err = rpc.DialTCP("tcp", fmt.Sprintf("127.0.0.1:%d", srv.Port()), false); !errors.Is(err, syscall.EADDRINUSE) {
			break
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	m := nfsc.Mount{Client: c}
	target, err := m.Mount("/", rpc.NewAuthUnix("ucfs-test", uid, uint32(os.Getgid())).Auth())
	if err != nil {
		t.Fatal(err)
	}
	return target
}

func TestServeOverRPC(t *testing.T) {
	const n = 1500
	files := make([]fake.File, n)
	for i := range files {
		files[i] = fake.File{UUID: uuid(i), Name: fmt.Sprintf("f%04d.bin", i), Size: int64(i % 7), Uploaded: t0.Add(time.Duration(i) * time.Second)}
	}
	files[7].Name = "café.txt"
	files[8].Size = 3<<20 + 5
	env := vfstest.New(t, files...)
	target := mount(t, env)

	// The client pages with 4 KiB replies, so this takes hundreds of READDIRPLUS calls.
	entries, err := target.ReadDirPlus("/files")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.FileName == "." || e.FileName == ".." {
			continue
		}
		if seen[e.FileName] {
			t.Fatalf("duplicate entry %q", e.FileName)
		}
		seen[e.FileName] = true
	}
	if len(seen) != n {
		t.Fatalf("listed %d entries, want %d", len(seen), n)
	}
	if reqs, _ := env.Fake.CDNStats(); reqs != 0 {
		t.Errorf("listing made %d CDN requests", reqs)
	}

	f, err := target.Open("/files/" + uuid(8) + "_f0008.bin")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, fake.Content(uuid(8), files[8].Size)) {
		t.Errorf("read %d bytes, content mismatch", len(got))
	}

	if _, _, err := target.Lookup("/files/" + uuid(7) + "_café.txt"); err != nil {
		t.Errorf("NFC lookup of an NFD name: %v", err)
	}
	if _, _, err := target.Lookup("/files/" + uuid(7) + "_nope.txt"); err == nil {
		t.Error("lookup with the wrong name succeeded")
	}
	// ACCESS reads its mask after the handle, so it catches misaligned handles.
	for _, p := range []string{"/", "/files", "/files/" + uuid(8) + "_f0008.bin"} {
		if got, err := target.Access(p, 0x3f); err != nil || got != 0x23 {
			t.Errorf("access %s = %#x, %v; want read, lookup and execute (0x23)", p, got, err)
		}
	}
	if _, err := target.Create("/files/new.txt", 0o644); err == nil {
		t.Error("create succeeded on a read-only export")
	}

	// Concurrent RPCs on one connection exercise ConcurrentHandlers.
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			name := fmt.Sprintf("/files/%s_f%04d.bin", uuid(100+i), 100+i)
			f, err := target.Open(name)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = f.Close() }()
			if got, err := io.ReadAll(f); err != nil || !bytes.Equal(got, fake.Content(uuid(100+i), files[100+i].Size)) {
				t.Errorf("%s: %v", name, err)
			}
		})
	}
	wg.Wait()
}

// ACCESS grants nothing to another user of the machine, so the kernel
// client keeps them out; the daemon's user and root get what they ask.
func TestAccessOnlyForOwner(t *testing.T) {
	files := []fake.File{{UUID: uuid(1), Name: "a.jpg", Size: 3, Uploaded: t0}}
	p := "/files/" + uuid(1) + "_a.jpg"
	for _, tc := range []struct {
		uid  uint32
		want uint32
	}{{uint32(os.Getuid()), 0x23}, {0, 0x23}, {uint32(os.Getuid()) + 1, 0}} {
		target := mountAs(t, vfstest.New(t, files...), tc.uid)
		if got, err := target.Access(p, 0x3f); err != nil || got != tc.want {
			t.Errorf("uid %d: access = %#x, %v; want %#x", tc.uid, got, err, tc.want)
		}
	}
}

func TestHandleRoundTrip(t *testing.T) {
	env := vfstest.New(t, fake.File{UUID: uuid(42), Name: "hero.jpg", Size: 10, Uploaded: t0, Tags: []string{"x"}})
	group := env.Fake.AddGroup(t0, uuid(42))
	opts := vfs.Options{Views: vfs.AllViews, DateZone: time.UTC, CDNBase: env.Fake.CDNBase(), Remote: env.Client}
	env.FS.Configure(opts)
	h := newHandler(env.FS)
	// A second filesystem on the same index stands in for a restarted daemon.
	fs2 := vfs.New(env.Index, env.Cache, env.CDN, nil)
	fs2.Configure(opts)
	restarted := newHandler(fs2)
	ctx := context.Background()
	for _, p := range []string{
		"by-date",
		"by-date/2026/03/04",
		"by-date/2026/03/04/" + uuid(42) + "_hero.jpg",
		"tags/x",
		"tags/x/" + uuid(42) + "_hero.jpg",
		"groups/" + group,
		"groups/" + group + "/0_hero.jpg",
		"",
		"files",
		"files/" + uuid(42) + "_hero.jpg",
		"files/" + uuid(42),
		"files/" + uuid(42) + "/-",
		"files/" + uuid(42) + "/-/resize/10x/-/format",
		"files/" + uuid(42) + "/-/resize/10x/-/format/webp/hero.webp",
	} {
		e, err := env.FS.Walk(ctx, p)
		if err != nil {
			t.Fatalf("walk %q: %v", p, err)
		}
		b := h.ToHandle(h.fs, strings.Split(p, "/"))
		if len(b)%4 != 0 {
			t.Errorf("%q: handle of %d bytes is not XDR aligned", p, len(b))
		}
		for _, hh := range []*handler{h, restarted} {
			if got, err := hh.decodeHandle(ctx, b); err != nil || got != e.Node {
				t.Errorf("round trip %q = %+v, %v; want %+v", p, got, err, e.Node)
			}
		}
		if _, path, err := restarted.FromHandle(b); err != nil || strings.Join(path, "/") != p {
			t.Errorf("path of %q after restart = %q, %v", p, path, err)
		}
		b[7] ^= 1
		if e.Node.UUID != "" {
			if _, err := h.decodeHandle(ctx, b); err == nil {
				t.Errorf("tampered handle for %q accepted", p)
			}
		}
	}
	unknown := encodeHandle(env.FS, vfs.Node{Kind: vfs.KindVariantDir, UUID: uuid(42), Ops: "-/never/handed/out/"})
	if _, err := h.decodeHandle(ctx, unknown); err == nil {
		t.Error("handle of a node never handed out resolved")
	}
	if _, err := h.decodeHandle(ctx, []byte{1, 2, 3}); err == nil {
		t.Error("short handle accepted")
	}
}

func TestVariantsOverRPC(t *testing.T) {
	const size = 3<<20 + 5
	env := vfstest.New(t, fake.File{UUID: uuid(1), Name: "hero.jpg", Size: size, MIME: "image/jpeg", Uploaded: t0})
	target := mount(t, env)
	dir := "/files/" + uuid(1) + "/-/resize/1200x/-/format/webp"

	entries, err := target.ReadDirPlus(dir)
	if err != nil {
		t.Fatal(err)
	}
	body, _, _ := env.Fake.Variant(uuid(1), "-/resize/1200x/-/format/webp/")
	var listed []string
	for _, e := range entries {
		if e.FileName == "." || e.FileName == ".." {
			continue
		}
		listed = append(listed, e.FileName)
		if e.Size() != int64(len(body)) {
			t.Errorf("listed size %d, want %d", e.Size(), len(body))
		}
	}
	if len(listed) != 1 || listed[0] != "hero.webp" {
		t.Errorf("listing = %q, want the one default entry", listed)
	}

	f, err := target.Open(dir + "/hero.webp")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(f)
	_ = f.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Errorf("read %d bytes, %v; content mismatch", len(got), err)
	}
	if heads, _ := env.Fake.CDNMethods(); heads != 1 {
		t.Errorf("%d HEADs for listing and reading one variant", heads)
	}

	var nerr *nfsc.Error
	_, _, err = target.Lookup("/files/" + uuid(1) + "/-/resize/" + fake.BadArg + "/hero.jpg")
	if !errors.As(err, &nerr) || nerr.ErrorNum != nfsc.NFS3ErrInval {
		t.Errorf("lookup of a variant the CDN rejects: %v, want NFS3ERR_INVAL", err)
	}
	if _, _, err := target.Lookup("/files/" + uuid(1) + "/-/nosuchop"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lookup of an unknown operation: %v, want NFS3ERR_NOENT", err)
	}
}

func writableEnv(t *testing.T, files ...fake.File) *vfstest.Env {
	t.Helper()
	env := vfstest.New(t, files...)
	// A grace of a nanosecond makes a file immutable as soon as it is uploaded.
	env.Writable(t, spool.Options{Metadata: map[string]string{"ucfs:test": "1"}, RecentGrace: time.Nanosecond}, &vfs.Writes{})
	return env
}

func waitSpools(t *testing.T, env *vfstest.Env) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for len(env.Spools.List()) > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("spools left: %+v", env.Spools.List())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func nfsErr(err error) uint32 {
	var nerr *nfsc.Error
	if errors.As(err, &nerr) {
		return nerr.ErrorNum
	}
	return 0
}

func TestWritesOverRPC(t *testing.T) {
	env := writableEnv(t, fake.File{UUID: uuid(1), Name: "fixture.jpg", Size: 10, Uploaded: t0})
	target := mount(t, env)

	if _, err := target.Mkdir("/tree/photos", 0o755); err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("0123456789"), 300000)
	f, err := target.OpenFile("/tree/photos/photo.jpg", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("COMMIT: %v", err)
	}
	waitSpools(t, env)

	r, err := target.Open("/tree/photos/photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read back %d bytes, %v", len(got), err)
	}
	same := nfsc.Sattr3{Size: nfsc.SetSize{SetIt: true, Size: uint64(len(body))}}
	if err := target.Setattr("/tree/photos/photo.jpg", same); err != nil {
		t.Errorf("truncate to the same length: %v", err)
	}
	shorter := nfsc.Sattr3{Size: nfsc.SetSize{SetIt: true, Size: 5}}
	if err := target.Setattr("/tree/photos/photo.jpg", shorter); nfsErr(err) != nfsc.NFS3ErrNotSupp {
		t.Errorf("truncate an uploaded file to 5 bytes: %v, want NFS3ERR_NOTSUPP", err)
	}

	if err := target.Rename("/tree/photos/photo.jpg", "/tree/photo.jpg"); err != nil {
		t.Fatal(err)
	}
	e, err := env.FS.Walk(context.Background(), "tree/photo.jpg")
	if err != nil {
		t.Fatal(err)
	}
	o, err := env.FS.Resolve(context.Background(), "tree/photo.jpg", env.Journal)
	if err != nil || o.State != vfs.StateUploaded {
		t.Fatalf("resolve: %+v, %v", o, err)
	}
	if sf, _ := env.Fake.File(o.UUID); sf.Metadata["ucfs:path"] != "/photo.jpg" || sf.Name != "photo.jpg" {
		t.Errorf("renamed file %+v", sf)
	}
	_ = e

	sc, err := target.OpenFile("/tree/._photo.jpg", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sc.Write([]byte("AppleDouble")); err != nil {
		t.Fatal(err)
	}
	_ = sc.Close()

	if err := target.Remove("/tree/photo.jpg"); err != nil {
		t.Fatal(err)
	}
	if sf, _ := env.Fake.File(o.UUID); !sf.Removed {
		t.Error("rm did not soft-delete")
	}
	if err := target.RmDir("/tree/photos"); err != nil {
		t.Errorf("rmdir: %v", err)
	}
	// The client turns NFS3ERR_PERM, and only it, into os.ErrPermission.
	if err := target.Remove("/tree/_inbox/fixture.jpg"); !errors.Is(err, os.ErrPermission) {
		t.Errorf("rm of a fixture: %v, want NFS3ERR_PERM", err)
	}
	if _, err := target.Create("/files/"+uuid(1)+"/x", 0o644); nfsErr(err) != nfsc.NFS3ErrROFS {
		t.Errorf("create in files/<uuid>/: %v, want NFS3ERR_ROFS", err)
	}
	if st := env.Fake.UploadStats(); st.Base != 1 {
		t.Errorf("%d uploads, want only photo.jpg", st.Base)
	}
}

func TestDirectoryHandleSurvivesRename(t *testing.T) {
	env := writableEnv(t)
	h := newHandler(env.FS)
	ctx := context.Background()
	tree, err := env.FS.Walk(ctx, "tree")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.FS.Mkdir(ctx, tree.Node, "a"); err != nil {
		t.Fatal(err)
	}
	a, _ := env.FS.Walk(ctx, "tree/a")
	if _, err := env.FS.Mkdir(ctx, a.Node, "sub"); err != nil {
		t.Fatal(err)
	}
	old := h.ToHandle(h.fs, []string{"tree", "a"})
	oldSub := h.ToHandle(h.fs, []string{"tree", "a", "sub"})
	if err := env.FS.Rename(ctx, tree.Node, "a", tree.Node, "b"); err != nil {
		t.Fatal(err)
	}
	for b, want := range map[string]string{string(old): "tree/b", string(oldSub): "tree/b/sub"} {
		_, p, err := h.FromHandle([]byte(b))
		if err != nil || strings.Join(p, "/") != want {
			t.Errorf("old handle resolves to %q, %v; want %s", p, err, want)
		}
		n, err := h.decodeHandle(ctx, []byte(b))
		if err != nil || binary.BigEndian.Uint64([]byte(b)[20:]) != env.FS.ID(n) {
			t.Errorf("old handle's node %+v keeps id? %v", n, err)
		}
	}
}

// vim with backupcopy=no saves by renaming the original to name~, writing a
// new file under the name and removing the backup. On NFS the client then
// holds the original's handle for the backup and gets a new one for the new
// file, so the two must stay apart.
func TestRenameBackupSaveOverRPC(t *testing.T) {
	env := writableEnv(t)
	target := mount(t, env)
	write := func(p, body string) {
		t.Helper()
		f, err := target.OpenFile(p, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		waitSpools(t, env)
	}
	write("/tree/note.txt", "first line\n")
	_, oldFH, err := target.Lookup("/tree/note.txt")
	if err != nil {
		t.Fatal(err)
	}
	v1, _ := env.FS.Resolve(context.Background(), "tree/note.txt", env.Journal)

	if err := target.Rename("/tree/note.txt", "/tree/note.txt~"); err != nil {
		t.Fatal(err)
	}
	write("/tree/note.txt", "first line\nsecond line\n")
	_, newFH, err := target.Lookup("/tree/note.txt")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(oldFH, newFH) {
		t.Error("the new file got the handle of the renamed original")
	}
	if err := target.Remove("/tree/note.txt~"); err != nil {
		t.Fatal(err)
	}

	v2, err := env.FS.Resolve(context.Background(), "tree/note.txt", env.Journal)
	if err != nil || v2.State != vfs.StateUploaded || v2.UUID == v1.UUID {
		t.Fatalf("after the save: %+v, %v", v2, err)
	}
	if f, _ := env.Fake.File(v2.UUID); f.Name != "note.txt" || f.Metadata["ucfs:path"] != "/note.txt" {
		t.Errorf("saved file %+v", f)
	}
	if f, _ := env.Fake.File(v1.UUID); !f.Removed {
		t.Error("the backup, the previous version, was not deleted")
	}
	entries, err := target.ReadDirPlus("/tree")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.FileName, ".nfs") || strings.HasSuffix(e.FileName, "~") {
			t.Errorf("left over: %s", e.FileName)
		}
	}
}

func TestByDateOverRPC(t *testing.T) {
	env := vfstest.New(t, fake.File{UUID: uuid(1), Name: "a.jpg", Size: 3<<20 + 1, Uploaded: t0},
		fake.File{UUID: uuid(2), Name: "b.jpg", Size: 7, Uploaded: t0.AddDate(0, 0, 1)})
	target := mount(t, env)
	var dirs []string
	for _, p := range []string{"/by-date", "/by-date/2026", "/by-date/2026/03"} {
		entries, err := target.ReadDirPlus(p)
		if err != nil {
			t.Fatalf("readdir %s: %v", p, err)
		}
		var names []string
		for _, e := range entries {
			if e.FileName != "." && e.FileName != ".." {
				names = append(names, e.FileName)
			}
		}
		// The client keeps entries in a map; the server sends them sorted.
		slices.Sort(names)
		dirs = append(dirs, strings.Join(names, ","))
	}
	if want := []string{"2026", "03", "04,05"}; strings.Join(dirs, " ") != strings.Join(want, " ") {
		t.Errorf("listings = %q, want %q", dirs, want)
	}
	if reqs, _ := env.Fake.CDNStats(); reqs != 0 {
		t.Errorf("listing by-date made %d CDN requests", reqs)
	}
	f, err := target.Open("/by-date/2026/03/04/" + uuid(1) + "_a.jpg")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if got, err := io.ReadAll(f); err != nil || !bytes.Equal(got, fake.Content(uuid(1), 3<<20+1)) {
		t.Errorf("read %d bytes, %v", len(got), err)
	}
}
