package nfs

import (
	"fmt"
	"os/exec"
	"strings"
)

// MountOptions are the user-facing knobs of the NFS mount options. On Linux
// the NFS front-end is a fallback for FUSE and needs root to mount.
type MountOptions struct {
	ReadOnly  bool
	ReadAhead int
	Browse    bool
}

func mountArgs(port int, dir string, o MountOptions) []string {
	opts := []string{
		fmt.Sprintf("port=%d", port), fmt.Sprintf("mountport=%d", port),
		"nfsvers=3", "proto=tcp", "mountproto=tcp", "nolock", "soft",
		"rsize=1048576", "wsize=1048576", "acregmax=3600", "acdirmax=5", "nosuid", "nodev",
	}
	if o.ReadOnly {
		opts = append(opts, "ro")
	}
	return []string{"-t", "nfs", "-o", strings.Join(opts, ","), "127.0.0.1:/", dir}
}

func Mount(port int, dir string, o MountOptions) error {
	if out, err := exec.Command("mount", mountArgs(port, dir, o)...).CombinedOutput(); err != nil {
		return fmt.Errorf("mount: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func Unmount(dir string) error {
	var msgs []string
	for _, cmd := range [][]string{{"umount", dir}, {"umount", "-f", dir}, {"umount", "-l", dir}} {
		out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput()
		if err == nil {
			return nil
		}
		msgs = append(msgs, fmt.Sprintf("%s: %v: %s", strings.Join(cmd, " "), err, strings.TrimSpace(string(out))))
	}
	return fmt.Errorf("unmount %s failed: %s", dir, strings.Join(msgs, "; "))
}
