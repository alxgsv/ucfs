package vfs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/cache"
	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/vfs"
	"github.com/alxgsv/ucfs/internal/vfs/vfstest"
)

// degrade makes the filesystem consult the client's REST state, with no
// grace period, and has one sync see the fake's refusal.
func degrade(t *testing.T, env *vfstest.Env, deny func(bool)) {
	t.Helper()
	env.FS.Configure(vfs.Options{DateZone: time.UTC, CDNBase: env.Fake.CDNBase(), Remote: env.Client, MetaTTL: time.Nanosecond,
		Gate: func() error { return env.Client.Health().Degraded(time.Now(), 0) }})
	deny(true)
	if _, err := env.Index.Sync(context.Background(), env.Client, false); err == nil {
		t.Fatal("sync succeeded against a refusing API")
	}
}

// While the API answers 403 permission_error, the mount lists and reads
// from the index and the CDN, sends no REST request for reads, and refuses
// writes with EACCES before spooling anything.
func TestDegradedDenied(t *testing.T) {
	env := writable(t, nil, file(1, "photo.jpg", 2*cache.BlockSize+3))
	ctx := context.Background()
	up := put(t, env, "tree", "mine.jpg", []byte("uploaded before the denial"))
	degrade(t, env, env.Fake.DenyREST)
	rest := env.Fake.Requests()

	if got := list(t, env, "files"); len(got) != 2 {
		t.Fatalf("files/ = %v", got)
	}
	n := walk(t, env, "files/"+uuid(1)+"_photo.jpg").Node
	if got := read(t, env, n); !bytes.Equal(got, fake.Content(uuid(1), 2*cache.BlockSize+3)) {
		t.Fatal("read while denied")
	}
	if _, err := env.FS.Getxattr(ctx, up.Node, "user.uc.meta.ucfs:test"); err != nil {
		t.Fatalf("getxattr past the TTL while denied: %v", err)
	}
	if got := env.Fake.Requests(); got != rest {
		t.Fatalf("reads while denied sent %d REST requests", got-rest)
	}

	tree := walk(t, env, "tree").Node
	if _, err := env.FS.Create(ctx, tree, "new.jpg"); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("create while denied: %v, want EACCES", err)
	}
	if err := env.FS.Remove(ctx, tree, "mine.jpg"); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("rm while denied: %v, want EACCES", err)
	}
	if err := env.FS.Rename(ctx, tree, "mine.jpg", tree, "moved.jpg"); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("mv while denied: %v, want EACCES", err)
	}
	if err := env.FS.Setxattr(ctx, up.Node, "user.uc.meta.camera", []byte("Canon"), 0); !errors.Is(err, syscall.EACCES) {
		t.Fatalf("setxattr while denied: %v, want EACCES", err)
	}
	if got := env.Fake.Requests(); got != rest {
		t.Fatalf("refused writes sent %d REST requests", got-rest)
	}
	if st := env.Spools.Stats(); st.Writing+st.Queued+st.Uploading+st.Failed != 0 {
		t.Fatalf("spools after refused writes: %+v", st)
	}
	if _, err := env.FS.Create(ctx, walk(t, env, "").Node, ".DS_Store"); err != nil {
		t.Fatalf("a local-only file while denied: %v", err)
	}

	env.Fake.DenyREST(false)
	if _, err := env.Index.Sync(ctx, env.Client, false); err != nil {
		t.Fatal(err)
	}
	if _, err := env.FS.Create(ctx, tree, "new.jpg"); err != nil {
		t.Fatalf("create once the API answers again: %v", err)
	}
}

// Throttled past the retries, writes fail with EAGAIN.
func TestDegradedThrottled(t *testing.T) {
	env := writable(t, nil)
	degrade(t, env, func(on bool) {
		if on {
			env.Fake.ThrottleREST(120)
		}
	})
	if _, err := env.FS.Create(context.Background(), walk(t, env, "tree").Node, "x.jpg"); !errors.Is(err, syscall.EAGAIN) {
		t.Fatalf("create while throttled: %v, want EAGAIN", err)
	}
}

// A variant re-rendered between two reads fails the second with EIO rather
// than return a file made of both renderings; the next access sends a new
// HEAD and reads the new rendering whole.
func TestVariantChangedMidRead(t *testing.T) {
	env := vfstest.New(t, hero())
	ctx := context.Background()
	n := vfs.Node{Kind: vfs.KindVariant, UUID: uuid(1), Ops: webp}
	if _, err := env.FS.Getattr(ctx, n); err != nil {
		t.Fatal(err)
	}
	first, _, _ := env.Fake.Variant(uuid(1), webp)
	p := make([]byte, cache.BlockSize)
	if _, err := env.FS.ReadAt(ctx, n, p, 0); err != nil || !bytes.Equal(p, first[:cache.BlockSize]) {
		t.Fatalf("first block: %v", err)
	}
	env.Fake.ChangeVariants()
	second, _, _ := env.Fake.Variant(uuid(1), webp)
	if _, err := env.FS.ReadAt(ctx, n, p, cache.BlockSize); !errors.Is(err, syscall.EIO) {
		t.Fatalf("read after the re-render: %v, want EIO", err)
	}
	if got := readAll(t, env.FS, n, 1<<20); !bytes.Equal(got, second) {
		t.Fatal("read after the failure is not the new rendering")
	}
	if h, _ := env.Fake.CDNMethods(); h != 2 {
		t.Fatalf("%d HEADs, want a second one after the change", h)
	}
}

// An original served with another ETag midway fails the read and is then
// read whole from the new content.
func TestOriginalChangedMidRead(t *testing.T) {
	size := int64(3*cache.BlockSize + 9)
	env := vfstest.New(t, file(1, "a.bin", size))
	ctx := context.Background()
	n := walk(t, env, "files/"+uuid(1)+"_a.bin").Node
	p := make([]byte, cache.BlockSize)
	if _, err := env.FS.ReadAt(ctx, n, p, 0); err != nil {
		t.Fatal(err)
	}
	env.Fake.ChangeOriginal(uuid(1))
	if _, err := env.FS.ReadAt(ctx, n, p, 2*cache.BlockSize); !errors.Is(err, syscall.EIO) {
		t.Fatalf("read after the change: %v, want EIO", err)
	}
	if got := read(t, env, n); !bytes.Equal(got, fake.Content(uuid(1)+"#1", size)) {
		t.Fatal("read after the failure mixes contents")
	}
}

// A request whose context was cancelled, as FUSE does when a signal
// interrupts the process waiting on it, fails with EINTR, which callers
// retry, not EIO.
func TestCancelledRequestIsEINTR(t *testing.T) {
	env := vfstest.New(t, file(1, "a.jpg", 10))
	files := walk(t, env, "files").Node
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := env.FS.ReadDir(ctx, files); !errors.Is(err, syscall.EINTR) {
		t.Errorf("readdir: %v, want EINTR", err)
	}
	if _, err := env.FS.Lookup(ctx, files, uuid(1)+"_a.jpg"); !errors.Is(err, syscall.EINTR) {
		t.Errorf("lookup: %v, want EINTR", err)
	}
	if _, err := env.FS.Getattr(ctx, vfs.Node{Kind: vfs.KindFile, UUID: uuid(1)}); !errors.Is(err, syscall.EINTR) {
		t.Errorf("getattr: %v, want EINTR", err)
	}
}

// A read completes although its caller's context was cancelled.
func TestCancelledReadCompletes(t *testing.T) {
	env := vfstest.New(t, file(1, "a.jpg", 10))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := make([]byte, 10)
	if n, err := env.FS.ReadAt(ctx, vfs.Node{Kind: vfs.KindFile, UUID: uuid(1)}, p, 0); n != 10 || (err != nil && !errors.Is(err, io.EOF)) {
		t.Fatalf("read: %d, %v", n, err)
	}
}
