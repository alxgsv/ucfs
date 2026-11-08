package main

import (
	"fmt"
	"os/exec"
	"strings"

	gofuse "github.com/hanwen/go-fuse/v2/fuse"

	"github.com/alxgsv/ucfs/internal/frontend/fuse"
	"github.com/alxgsv/ucfs/internal/frontend/nfs"
	"github.com/alxgsv/ucfs/internal/vfs"
)

const defaultFrontend = "fuse"

func mountFrontend(name, dir string, fsys *vfs.FS, f *mountFlags, debug bool) (mounted, error) {
	switch name {
	case "fuse":
		srv, err := fuse.Mount(dir, fsys, debug)
		if err != nil {
			return nil, err
		}
		return newFuseMount(srv), nil
	case "nfs":
		return mountNFS(dir, fsys, f)
	}
	return nil, fmt.Errorf("unknown frontend %q", name)
}

type fuseMount struct {
	srv  *gofuse.Server
	done chan struct{}
}

func newFuseMount(srv *gofuse.Server) *fuseMount {
	m := &fuseMount{srv: srv, done: make(chan struct{})}
	go func() {
		srv.Wait()
		close(m.done)
	}()
	return m
}

func (m *fuseMount) Done() <-chan struct{} { return m.done }

func (m *fuseMount) Unmount() error {
	if err := m.srv.Unmount(); err != nil {
		return err
	}
	<-m.done
	return nil
}

// unmountDir detaches a mount that no daemon answers for.
func unmountDir(dir string) error {
	out, err := exec.Command("fusermount3", "-u", dir).CombinedOutput()
	if err == nil {
		return nil
	}
	if nerr := nfs.Unmount(dir); nerr == nil {
		return nil
	}
	return fmt.Errorf("fusermount3 -u: %w: %s", err, strings.TrimSpace(string(out)))
}
