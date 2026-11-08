package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alxgsv/ucfs/internal/ctl"
	"github.com/alxgsv/ucfs/internal/frontend"
)

// daemonEnv marks the process `ucfs mount --daemon` starts: it gets the same
// arguments and serves the mount in the foreground.
const daemonEnv = "UCFS_DAEMON_CHILD"

// daemonWait bounds the wait for the mount: a first mount lists the whole
// project before it serves anything.
const daemonWait = 10 * time.Minute

func isDaemonChild() bool { return os.Getenv(daemonEnv) != "" }

// runDaemon starts `ucfs mount` again detached from the terminal and returns
// once the mount answers on its control socket.
func runDaemon(ctx context.Context, g *globalFlags, dir string, out io.Writer) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if err := frontend.CheckMountDir(dir); err != nil {
		return err
	}
	cfg, err := g.load()
	if err != nil {
		return err
	}
	sock := ctl.SocketPath(cfg.CacheDir, dir)
	if _, err := ctl.Get(sock); err == nil {
		return fmt.Errorf("a ucfs daemon already serves %s", dir)
	}
	if err := os.MkdirAll(cfg.CacheDir, 0o700); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath := daemonLog(cfg.CacheDir, dir)
	pid, err := spawnDaemon(ctx, daemonSpec{Exe: exe, Args: os.Args[1:], Env: append(os.Environ(), daemonEnv+"=1"),
		Log: logPath, Socket: sock, Wait: daemonWait})
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "mounted %s: pid %d, log %s\n", dir, pid, logPath)
	return err
}

// daemonLog is the log of the daemon serving mountDir, named like its
// control socket.
func daemonLog(cacheDir, mountDir string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(mountDir))
	return filepath.Join(cacheDir, fmt.Sprintf("mount-%08x.log", h.Sum32()))
}

type daemonSpec struct {
	Exe    string
	Args   []string
	Env    []string
	Log    string
	Socket string
	Wait   time.Duration
}

// spawnDaemon starts the daemon in its own session, so closing the terminal
// does not stop it, with stdin from /dev/null and stdout and stderr appended
// to the log. It returns the daemon's pid once the socket answers, and an
// error with what the daemon logged if it exits first. A daemon still
// starting after Wait keeps running; one still starting when ctx ends is
// stopped.
func spawnDaemon(ctx context.Context, s daemonSpec) (int, error) {
	logf, err := os.OpenFile(s.Log, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer func() { _ = logf.Close() }()
	start, err := logf.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		return 0, err
	}
	defer func() { _ = null.Close() }()

	cmd := exec.Command(s.Exe, s.Args...)
	cmd.Env = s.Env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = null, logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	pid := cmd.Process.Pid
	deadline := time.NewTimer(s.Wait)
	defer deadline.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-exited:
			if err == nil {
				err = errors.New("exited")
			}
			return 0, fmt.Errorf("the daemon (pid %d) %w before mounting:\n%s", pid, err, logSince(s.Log, start))
		case <-ctx.Done():
			_ = cmd.Process.Signal(syscall.SIGTERM)
			return 0, fmt.Errorf("stopped the daemon (pid %d) while it was starting: %w", pid, ctx.Err())
		case <-deadline.C:
			return 0, fmt.Errorf("the daemon (pid %d) is still starting after %s; it keeps running, see %s", pid, s.Wait, s.Log)
		case <-tick.C:
			if _, err := ctl.Get(s.Socket); err == nil {
				return pid, nil
			}
		}
	}
}

// logSince returns the last lines the log got after offset start.
func logSince(path string, start int64) string {
	b, err := os.ReadFile(path)
	if err != nil || int64(len(b)) < start {
		return ""
	}
	lines := strings.Split(strings.TrimRight(string(b[start:]), "\n"), "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	return strings.Join(lines, "\n")
}
