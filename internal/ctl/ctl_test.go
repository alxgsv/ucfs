package ctl

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestServeStatusUnmount(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), strings.Repeat("long-directory-name-", 5))
	mountDir := "/mnt/somewhere"
	path := SocketPath(cacheDir, mountDir)
	if len(path) > maxSocketPath {
		t.Fatalf("socket path %q is %d bytes", path, len(path))
	}
	unmounted := make(chan struct{})
	var srv *Server
	srv, err := Serve(path, func() Status { return Status{MountDir: mountDir, Frontend: "nfs"} }, func() {
		close(unmounted)
		_ = srv.Close()
	})
	if err != nil {
		t.Fatal(err)
	}
	socks, err := Sockets(cacheDir)
	if err != nil || len(socks) != 1 || socks[0] != path {
		t.Fatalf("sockets = %v, %v", socks, err)
	}
	st, err := Get(path)
	if err != nil || st.MountDir != mountDir {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if _, err := Serve(path, nil, nil); err == nil {
		t.Error("a second daemon could take a live socket")
	}
	if err := Unmount(path, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	<-unmounted
}

func TestRegistry(t *testing.T) {
	reg := t.TempDir()
	live := filepath.Join(t.TempDir(), "live.sock")
	srv, err := Serve(live, func() Status { return Status{} }, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	// The mount point goes through a symlink, as /tmp does on macOS.
	mnt := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(mnt, link); err != nil {
		t.Fatal(err)
	}
	unregister, err := Register(reg, Mount{MountDir: link, Socket: live, PID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Register(reg, Mount{MountDir: "/gone", Socket: filepath.Join(t.TempDir(), "gone.sock")}); err != nil {
		t.Fatal(err)
	}
	if m, ok := Lookup(reg, mnt); !ok || m.Socket != live {
		t.Errorf("lookup by the resolved path = %+v, %v", m, ok)
	}
	mounts, err := Mounts(reg)
	if err != nil || len(mounts) != 1 || mounts[0].Socket != live {
		t.Errorf("mounts = %+v, %v; want only the live one", mounts, err)
	}
	if _, ok := Lookup(reg, "/gone"); ok {
		t.Error("the record of a dead daemon was kept")
	}
	unregister()
	if _, ok := Lookup(reg, link); ok {
		t.Error("record left after unregister")
	}
}

// Asking for the status many times leaves no connection open, on either
// side.
func TestGetLeavesNoConnection(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "c.sock")
	srv, err := Serve(sock, func() Status { return Status{} }, func() {})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Close() }()
	if _, err := Get(sock); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()
	for range 50 {
		if _, err := Get(sock); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+2 {
		t.Fatalf("goroutines %d before 50 status requests, %d after", before, after)
	}
}
