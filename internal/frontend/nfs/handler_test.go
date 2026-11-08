package nfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	nfsc "github.com/willscott/go-nfs-client/nfs"
	"github.com/willscott/go-nfs-client/nfs/rpc"

	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/vfs"
	"github.com/alxgsv/ucfs/internal/vfs/vfstest"
)

var t0 = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

func uuid(i int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", i) }

func mount(t *testing.T, env *vfstest.Env) *nfsc.Target {
	t.Helper()
	srv, err := Serve(env.FS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	c, err := rpc.DialTCP("tcp", fmt.Sprintf("127.0.0.1:%d", srv.Port()), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	m := nfsc.Mount{Client: c}
	target, err := m.Mount("/", rpc.AuthNull)
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

func TestHandleRoundTrip(t *testing.T) {
	env := vfstest.New(t, fake.File{UUID: uuid(42), Name: "hero.jpg", Size: 10, Uploaded: t0})
	h := newHandler(env.FS)
	// A second filesystem on the same index stands in for a restarted daemon.
	restarted := newHandler(vfs.New(env.Index, env.Cache, env.CDN, nil))
	ctx := context.Background()
	for _, p := range []string{
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
	unknown := encodeHandle(vfs.Node{Kind: vfs.KindVariantDir, UUID: uuid(42), Ops: "-/never/handed/out/"})
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
