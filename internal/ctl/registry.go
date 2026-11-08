package ctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Mount is a running mount as the registry records it, so that status and
// umount find its daemon whatever --cache-dir it was started with.
type Mount struct {
	MountDir string `json:"mount_dir"`
	Socket   string `json:"socket"`
	PID      int    `json:"pid"`
}

// RegistryDir is <user cache dir>/ucfs/mounts, shared by every project and
// cache dir of one user.
func RegistryDir() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "ucfs", "mounts"), nil
}

// canonical resolves symlinks, so /tmp/x and /private/tmp/x on macOS name one
// mount.
func canonical(dir string) string {
	if d, err := filepath.EvalSymlinks(dir); err == nil {
		return d
	}
	return filepath.Clean(dir)
}

func recordPath(reg, mountDir string) string {
	return filepath.Join(reg, fmt.Sprintf("%08x.json", hash32(canonical(mountDir))))
}

// Register records a mount and returns the function that removes the record.
func Register(reg string, m Mount) (func(), error) {
	if err := os.MkdirAll(reg, 0o700); err != nil {
		return nil, err
	}
	m.MountDir = canonical(m.MountDir)
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	p := recordPath(reg, m.MountDir)
	tmp, err := os.CreateTemp(reg, ".tmp-*")
	if err != nil {
		return nil, err
	}
	_, werr := tmp.Write(b)
	if err := errors.Join(werr, tmp.Close()); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		_ = os.Remove(tmp.Name())
		return nil, err
	}
	return func() { _ = os.Remove(p) }, nil
}

// Lookup returns the record of the mount at dir.
func Lookup(reg, dir string) (Mount, bool) {
	var m Mount
	b, err := os.ReadFile(recordPath(reg, dir))
	if err != nil || json.Unmarshal(b, &m) != nil {
		return m, false
	}
	return m, true
}

// Mounts returns the registered mounts. A record whose socket is gone or
// refuses connections belongs to a daemon that died, and is removed.
func Mounts(reg string) ([]Mount, error) {
	paths, err := filepath.Glob(filepath.Join(reg, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Mount
	for _, p := range paths {
		var m Mount
		b, err := os.ReadFile(p)
		if err != nil || json.Unmarshal(b, &m) != nil {
			continue
		}
		if _, err := Get(m.Socket); errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) {
			_ = os.Remove(p)
			continue
		}
		out = append(out, m)
	}
	return out, nil
}
