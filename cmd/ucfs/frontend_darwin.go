package main

import (
	"fmt"

	"github.com/alxgsv/ucfs/internal/frontend/nfs"
	"github.com/alxgsv/ucfs/internal/vfs"
)

const defaultFrontend = "nfs"

func mountFrontend(name, dir string, fsys *vfs.FS, f *mountFlags, _ bool) (mounted, error) {
	if name != "nfs" {
		return nil, fmt.Errorf("frontend %q is not available on macOS; use nfs", name)
	}
	return mountNFS(dir, fsys, f)
}

// unmountDir detaches a mount that no daemon answers for.
func unmountDir(dir string) error { return nfs.Unmount(dir) }
