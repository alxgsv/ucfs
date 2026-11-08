package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/ctl"
)

// TestDaemonHelper is the process spawnDaemon starts in these tests, run
// from the test binary: it serves a control socket like a mounted daemon,
// fails, or hangs, as UCFS_TEST_DAEMON says.
func TestDaemonHelper(t *testing.T) {
	mode := os.Getenv("UCFS_TEST_DAEMON")
	if mode == "" {
		t.Skip("helper process only")
	}
	null, _ := os.Stat(os.DevNull)
	in, _ := os.Stdin.Stat()
	fmt.Printf("stdout from %d\n", os.Getpid())
	fmt.Fprintf(os.Stderr, "stdin is /dev/null: %v\n", os.SameFile(null, in))
	switch mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "ucfs: mount_nfs: exit status 1: no such file")
		os.Exit(1)
	case "hang":
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	stop := make(chan struct{})
	srv, err := ctl.Serve(os.Getenv("UCFS_TEST_SOCK"), func() ctl.Status { return ctl.Status{PID: os.Getpid()} },
		func() { close(stop) })
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	<-stop
	_ = srv.Close()
	os.Exit(0)
}

func helperSpec(t *testing.T, mode string, wait time.Duration) daemonSpec {
	dir, err := os.MkdirTemp("", "ucfsd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "ctl.sock")
	return daemonSpec{Exe: os.Args[0], Args: []string{"-test.run=^TestDaemonHelper$"},
		Env: append(os.Environ(), "UCFS_TEST_DAEMON="+mode, "UCFS_TEST_SOCK="+sock),
		Log: filepath.Join(dir, "mount.log"), Socket: sock, Wait: wait}
}

// gone waits for pid to exit; spawnDaemon's goroutine reaps it.
func gone(t *testing.T, pid int) {
	t.Helper()
	for range 100 {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("pid %d still running", pid)
}

func TestSpawnDaemon(t *testing.T) {
	s := helperSpec(t, "serve", 30*time.Second)
	if err := os.WriteFile(s.Log, []byte("an earlier run\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	pid, err := spawnDaemon(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if st, err := ctl.Get(s.Socket); err != nil || st.PID != pid {
		t.Fatalf("status = %+v, %v; want pid %d", st, err, pid)
	}
	if pgid, err := syscall.Getpgid(pid); err != nil || pgid != pid {
		t.Errorf("pgid of %d = %d, %v: not a session of its own", pid, pgid, err)
	}
	if err := ctl.Unmount(s.Socket, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	gone(t, pid)
	log, err := os.ReadFile(s.Log)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"an earlier run\n", fmt.Sprintf("stdout from %d\n", pid), "stdin is /dev/null: true\n"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("log lacks %q:\n%s", want, log)
		}
	}
	if fi, err := os.Stat(s.Log); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, %v", fi.Mode(), err)
	}
}

func TestSpawnDaemonFails(t *testing.T) {
	s := helperSpec(t, "fail", 30*time.Second)
	if err := os.WriteFile(s.Log, []byte("ucfs: an earlier failure\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := spawnDaemon(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "exit status 1 before mounting") ||
		!strings.Contains(err.Error(), "mount_nfs: exit status 1: no such file") ||
		strings.Contains(err.Error(), "earlier failure") {
		t.Errorf("err = %v", err)
	}
}

func TestSpawnDaemonSlow(t *testing.T) {
	s := helperSpec(t, "hang", 300*time.Millisecond)
	_, err := spawnDaemon(context.Background(), s)
	if err == nil || !strings.Contains(err.Error(), "still starting after 300ms; it keeps running") {
		t.Fatalf("err = %v", err)
	}
	var pid int
	if _, err := fmt.Sscanf(err.Error(), "the daemon (pid %d)", &pid); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Errorf("pid %d not running after the wait: %v", pid, err)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	gone(t, pid)

	ctx, cancel := context.WithCancel(context.Background())
	s.Wait = time.Minute
	time.AfterFunc(300*time.Millisecond, cancel)
	_, err = spawnDaemon(ctx, s)
	if err == nil || !strings.Contains(err.Error(), "stopped the daemon") {
		t.Fatalf("cancelled: err = %v", err)
	}
	if _, err := fmt.Sscanf(err.Error(), "stopped the daemon (pid %d)", &pid); err != nil {
		t.Fatal(err)
	}
	gone(t, pid)
}

func TestDaemonLog(t *testing.T) {
	a, b := daemonLog("/c", "/Users/me/uc"), daemonLog("/c", "/Users/me/uc2")
	if a == b || filepath.Dir(a) != "/c" || !strings.HasPrefix(filepath.Base(a), "mount-") || filepath.Ext(a) != ".log" {
		t.Errorf("daemonLog = %s, %s", a, b)
	}
}
