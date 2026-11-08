package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/ctl"
	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/frontend"
	"github.com/alxgsv/ucfs/internal/index"
)

// These tests run the ucfs command itself, in this process, against
// internal/fake: apiTransport carries its REST and Upload API requests and
// --cdn-base points at its CDN. Those that mount for real (NFS on macOS as
// the user, FUSE on Linux) need UCFS_MOUNT_TESTS=1.

const (
	fakePublicKey = "demopublickeyfake01"
	fakeSecretKey = "demosecretkeyfake02"
)

// fakeHome gives the command a home of its own, so the mount registry and
// the default config file are the test's, and routes its API requests to
// srv. It returns the global flags that select the fake project.
func fakeHome(t testing.TB, srv *fake.Server, cache string) []string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	apiTransport = srv.Transport()
	t.Cleanup(func() { apiTransport = nil })
	return []string{"--public-key", fakePublicKey, "--secret-key", fakeSecretKey, "--cdn-base", srv.CDNBase(),
		"--cache-dir", cache}
}

// ucfs runs one ucfs command to completion and returns its output.
func ucfs(t testing.TB, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs(args)
	err := root.ExecuteContext(context.Background())
	return out.String(), err
}

type fakeMount struct {
	dir, sock string
	stop      func()
}

func needMounts(t testing.TB) {
	if os.Getenv("UCFS_MOUNT_TESTS") != "1" {
		t.Skip("mounts for real; set UCFS_MOUNT_TESTS=1")
	}
}

// mountFake runs `ucfs mount` with the given flags in the background and
// waits until its control socket answers; the test's end unmounts it.
func mountFake(t testing.TB, global []string, cache string, flags ...string) *fakeMount {
	t.Helper()
	needMounts(t)
	dir := filepath.Join(t.TempDir(), "mnt")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	root := newRootCmd()
	root.SetArgs(append(append(append([]string{}, global...), "mount", dir), flags...))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()
	m := &fakeMount{dir: dir, sock: ctl.SocketPath(cache, dir)}
	stopped := false
	m.stop = func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("mount exited with %v", err)
			}
		case <-time.After(3 * time.Minute):
			t.Errorf("mount did not exit")
		}
		if mounted, _ := frontend.IsMountPoint(dir); mounted {
			t.Errorf("%s still mounted", dir)
		}
	}
	t.Cleanup(m.stop)
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := ctl.Get(m.sock); err == nil {
			return m
		}
		select {
		case err := <-done:
			stopped = true
			t.Fatalf("mount exited before serving: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("mount did not come up")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (m *fakeMount) status(t testing.TB) ctl.Status {
	t.Helper()
	st, err := ctl.Get(m.sock)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// statusText is what `ucfs status` prints for the mount.
func (m *fakeMount) statusText(t testing.TB) string {
	t.Helper()
	var b bytes.Buffer
	if err := printStatus(&b, m.status(t)); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

var t0 = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func fakeFile(i int, name string, size int64) fake.File {
	return fake.File{UUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i), Name: name, Size: size, MIME: "image/jpeg",
		Uploaded: t0.Add(time.Duration(i) * time.Minute), Stored: true}
}

// `ucfs index refresh` reads a file's current info, metadata included, into
// the index, and marks a file the API no longer knows removed.
func TestIndexRefresh(t *testing.T) {
	srv := fake.New(t)
	a, b := fakeFile(1, "a.jpg", 10), fakeFile(2, "b.jpg", 10)
	srv.Add(a, b)
	cache := t.TempDir()
	global := fakeHome(t, srv, cache)
	if _, err := ucfs(t, append(global, "index", "sync")...); err != nil {
		t.Fatal(err)
	}
	srv.SetMetadata(a.UUID, "camera", "Canon")
	srv.Remove(b.UUID)
	out, err := ucfs(t, append(global, "index", "refresh", a.UUID, b.UUID, "00000000-0000-4000-8000-000000000099")...)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, a.UUID+": a.jpg") || !strings.Contains(out, b.UUID+": removed") ||
		!strings.Contains(out, "000000000099: not found") {
		t.Fatalf("output:\n%s", out)
	}
	ix, err := index.Open(filepath.Join(cache, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ix.Close() }()
	d, err := ix.Details(context.Background(), a.UUID)
	if err != nil || d.Metadata["camera"] != "Canon" {
		t.Fatalf("index after refresh: %+v, %v", d, err)
	}
	if f, err := ix.Get(context.Background(), b.UUID); err != nil || !f.Removed {
		t.Fatalf("removed file: %+v, %v", f, err)
	}
}

// A mount whose index exists starts while REST answers 403
// permission_error, lists and reads, serves cached reads with the CDN
// down too, refuses writes with EACCES once the denial lasted a sync
// interval, and says so in `ucfs status`.
func TestMountServesDeniedAPI(t *testing.T) {
	srv := fake.New(t)
	small, big := fakeFile(1, "small.jpg", 70_000), fakeFile(2, "big.jpg", 3<<20+5)
	srv.Add(small, big)
	cache := t.TempDir()
	global := fakeHome(t, srv, cache)
	if _, err := ucfs(t, append(global, "index", "sync")...); err != nil {
		t.Fatal(err)
	}
	srv.DenyREST(true)
	m := mountFake(t, global, cache, "--sync-interval", "1s")

	entries, err := os.ReadDir(filepath.Join(m.dir, "files"))
	if err != nil || len(entries) != 2 {
		t.Fatalf("ls files/: %d entries, %v", len(entries), err)
	}
	p := filepath.Join(m.dir, "files", small.UUID+"_small.jpg")
	got, err := os.ReadFile(p)
	if err != nil || !bytes.Equal(got, fake.Content(small.UUID, small.Size)) {
		t.Fatalf("read while denied: %d bytes, %v", len(got), err)
	}
	srv.FailCDN(503)
	if got, err := os.ReadFile(p); err != nil || !bytes.Equal(got, fake.Content(small.UUID, small.Size)) {
		t.Fatalf("cached read with the CDN down: %d bytes, %v", len(got), err)
	}
	srv.FailCDN(0)

	deadline := time.Now().Add(10 * time.Second)
	for !m.status(t).API.Degraded {
		if time.Now().After(deadline) {
			t.Fatalf("not degraded: %+v", m.status(t).API)
		}
		time.Sleep(100 * time.Millisecond)
	}
	err = os.WriteFile(filepath.Join(m.dir, "tree", "new.jpg"), []byte("x"), 0o644)
	if !errors.Is(err, syscall.EACCES) {
		t.Fatalf("write while denied: %v, want EACCES", err)
	}
	st := m.status(t)
	text := m.statusText(t)
	t.Logf("write while denied: %v\n%s", err, text)
	if st.Index.SyncFailures < 1 || !strings.Contains(text, "API denied since") || !strings.Contains(st.API.Detail, "permission_error") {
		t.Fatalf("status does not report the denial:\n%s", text)
	}
	if st.Writes.Uploads+int64(st.Writes.Writing+st.Writes.Queued+st.Writes.Failed) != 0 {
		t.Fatalf("writes reached the spool: %+v", st.Writes)
	}
}

// A cat that crosses the traffic budget gets the blocks that fit and then
// EDQUOT, and `ucfs status` shows the budget used up.
func TestMountBudget(t *testing.T) {
	srv := fake.New(t)
	f := fakeFile(1, "big.jpg", 3<<20+5)
	srv.Add(f)
	cache := t.TempDir()
	global := fakeHome(t, srv, cache)
	m := mountFake(t, global, cache, "--traffic-budget", "2MiB")
	_, err := os.ReadFile(filepath.Join(m.dir, "files", f.UUID+"_big.jpg"))
	if !errors.Is(err, syscall.EDQUOT) {
		t.Fatalf("cat past the budget: %v, want EDQUOT", err)
	}
	st := m.status(t)
	b, _ := json.Marshal(st.Budget)
	t.Logf("cat: %v\nbudget: %s\n%s", err, b, m.statusText(t))
	if st.Budget == nil || st.Budget.Used != 2<<20 || st.Budget.Remaining != 0 || st.Budget.Refused < 1 {
		t.Fatalf("budget %s", b)
	}
	if _, bytes := srv.CDNStats(); bytes > 2<<20+64<<10 {
		t.Fatalf("CDN served %d bytes against a budget of %d", bytes, 2<<20)
	}
}

// What a writer sees when the API is throttled past its retries: EAGAIN,
// which NFS carries as JUKEBOX. Opt-in with UCFS_JUKEBOX=1, as a client
// that retries JUKEBOX can hang the test.
func TestMountThrottledWrite(t *testing.T) {
	if os.Getenv("UCFS_JUKEBOX") != "1" {
		t.Skip("set UCFS_JUKEBOX=1 to measure")
	}
	srv := fake.New(t)
	cache := t.TempDir()
	global := fakeHome(t, srv, cache)
	if _, err := ucfs(t, append(global, "index", "sync")...); err != nil {
		t.Fatal(err)
	}
	srv.ThrottleREST(120)
	m := mountFake(t, global, cache, "--sync-interval", "1s")
	for !m.status(t).API.Degraded {
		time.Sleep(100 * time.Millisecond)
	}
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- os.WriteFile(filepath.Join(m.dir, "tree", "new.jpg"), []byte("x"), 0o644) }()
	select {
	case err := <-done:
		t.Logf("write while throttled: %v after %v", err, time.Since(start).Round(time.Millisecond))
	case <-time.After(2 * time.Minute):
		t.Logf("write while throttled still waiting after 2 min; lifting the throttle")
		srv.ThrottleREST(0)
		t.Logf("then: %v after %v", <-done, time.Since(start).Round(time.Millisecond))
	}
}
