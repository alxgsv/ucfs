// Package frontend holds what the NFS and FUSE front-ends share.
package frontend

import (
	"os"
	"path/filepath"
	"syscall"
)

// IsMountPoint reports whether dir is on a different device than its parent.
func IsMountPoint(dir string) (bool, error) {
	var self, parent syscall.Stat_t
	if err := syscall.Stat(dir, &self); err != nil {
		return false, err
	}
	if err := syscall.Stat(filepath.Dir(filepath.Clean(dir)), &parent); err != nil {
		return false, err
	}
	return self.Dev != parent.Dev, nil
}

// CheckMountDir requires an existing directory that is not already mounted on.
func CheckMountDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return &os.PathError{Op: "mount", Path: dir, Err: syscall.ENOTDIR}
	}
	mounted, err := IsMountPoint(dir)
	if err != nil {
		return err
	}
	if mounted {
		return &os.PathError{Op: "mount", Path: dir, Err: syscall.EBUSY}
	}
	return nil
}
