package ctl

import (
	"path/filepath"
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
