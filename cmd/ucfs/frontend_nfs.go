package main

import (
	"errors"
	"log/slog"
	"time"

	"github.com/alxgsv/ucfs/internal/frontend"
	"github.com/alxgsv/ucfs/internal/frontend/nfs"
	"github.com/alxgsv/ucfs/internal/vfs"
)

type nfsMount struct {
	srv  *nfs.Server
	dir  string
	done chan struct{}
	stop chan struct{}
}

// mountNFS starts the server before mount_nfs, which talks to it at once.
func mountNFS(dir string, fsys *vfs.FS, f *mountFlags) (*nfsMount, error) {
	srv, err := nfs.Serve(fsys)
	if err != nil {
		return nil, err
	}
	o := nfs.MountOptions{ReadOnly: f.readOnly, ReadAhead: f.readAhead, Browse: f.browse}
	if err := nfs.Mount(srv.Port(), dir, o); err != nil {
		return nil, errors.Join(err, srv.Close())
	}
	m := &nfsMount{srv: srv, dir: dir, done: make(chan struct{}), stop: make(chan struct{})}
	go m.watch()
	return m, nil
}

// watch notices an external umount: NFS gives the server no signal for it.
func (m *nfsMount) watch() {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-t.C:
			if mounted, err := frontend.IsMountPoint(m.dir); err == nil && !mounted {
				_ = m.srv.Close()
				close(m.done)
				return
			}
		}
	}
}

func (m *nfsMount) Done() <-chan struct{} { return m.done }

// Unmount detaches the mount first and stops the server only after, so the
// kernel never holds a mount whose server is gone.
func (m *nfsMount) Unmount() error {
	close(m.stop)
	if err := nfs.Unmount(m.dir); err != nil {
		slog.Error("unmount failed; the server keeps running", "dir", m.dir, "err", err)
		return err
	}
	return m.srv.Close()
}
