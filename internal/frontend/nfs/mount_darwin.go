package nfs

import (
	"fmt"
	"os/exec"
	"strings"
)

// MountOptions are the user-facing knobs of the mount_nfs option string.
type MountOptions struct {
	ReadOnly  bool
	ReadAhead int
	Browse    bool
}

// mountArgs is the option string confirmed by Spike A (docs/spikes.md).
func mountArgs(port int, dir string, o MountOptions) []string {
	opts := []string{
		fmt.Sprintf("port=%d", port), fmt.Sprintf("mountport=%d", port),
		"vers=3", "tcp", "locallocks", "noresvport",
		"rsize=1048576", "wsize=1048576", fmt.Sprintf("readahead=%d", o.ReadAhead),
		"nfc", "soft", "intr", "acregmax=3600", "acdirmax=5",
	}
	if !o.Browse {
		opts = append(opts, "nobrowse")
	}
	if o.ReadOnly {
		opts = append(opts, "rdonly")
	}
	return []string{"-o", strings.Join(opts, ","), "127.0.0.1:/", dir}
}

// Mount runs mount_nfs as the current user; no root is needed on macOS.
func Mount(port int, dir string, o MountOptions) error {
	if out, err := exec.Command("/sbin/mount_nfs", mountArgs(port, dir, o)...).CombinedOutput(); err != nil {
		return fmt.Errorf("mount_nfs: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Unmount escalates from a clean unmount to a forced one, so the daemon never
// exits with the mount still in place.
func Unmount(dir string) error {
	var msgs []string
	for _, cmd := range [][]string{{"/sbin/umount", dir}, {"/sbin/umount", "-f", dir}, {"/usr/sbin/diskutil", "unmount", "force", dir}} {
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err == nil {
			return nil
		}
		msgs = append(msgs, fmt.Sprintf("%s: %v: %s", strings.Join(cmd, " "), err, strings.TrimSpace(string(out))))
	}
	return fmt.Errorf("unmount %s failed: %s", dir, strings.Join(msgs, "; "))
}
