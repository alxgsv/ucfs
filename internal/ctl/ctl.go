// Package ctl is the unix-socket control API of a running mount, used by
// `ucfs status` and `ucfs umount`.
package ctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type Status struct {
	MountDir string    `json:"mount_dir"`
	Frontend string    `json:"frontend"`
	Project  string    `json:"project"`
	PID      int       `json:"pid"`
	Started  time.Time `json:"started"`

	Index IndexStatus `json:"index"`
	CDN   CDNStatus   `json:"cdn"`
	Cache CacheStatus `json:"cache"`
}

type IndexStatus struct {
	Files     int       `json:"files"`
	Bytes     int64     `json:"bytes"`
	LastSync  time.Time `json:"last_sync"`
	SyncError string    `json:"sync_error,omitempty"`
}

type CDNStatus struct {
	Requests int64 `json:"requests"`
	Bytes    int64 `json:"bytes"`
}

type CacheStatus struct {
	Served          int64 `json:"served"`
	Hit             int64 `json:"hit"`
	FetchedDemand   int64 `json:"fetched_demand"`
	FetchedPrefetch int64 `json:"fetched_prefetch"`
	Used            int64 `json:"used"`
	Max             int64 `json:"max"`
}

// SocketPath is the control socket of a mount. It lives in the project's
// cache dir and is named after the mount point, kept short for the 104-byte
// sun_path limit on macOS.
func SocketPath(cacheDir, mountDir string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(mountDir))
	return filepath.Join(cacheDir, fmt.Sprintf("ctl-%08x.sock", h.Sum32()))
}

// Sockets lists the control sockets in a cache dir.
func Sockets(cacheDir string) ([]string, error) {
	return filepath.Glob(filepath.Join(cacheDir, "ctl-*.sock"))
}

type Server struct {
	srv  *http.Server
	path string
}

// Serve answers GET /status and POST /unmount on path. A socket left by a
// daemon that died is replaced; a live one is an error.
func Serve(path string, status func() Status, unmount func()) (*Server, error) {
	if _, err := Get(path); err == nil {
		return nil, fmt.Errorf("a ucfs daemon already serves %s", path)
	}
	_ = os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status())
	})
	mux.HandleFunc("POST /unmount", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		go unmount()
	})
	s := &Server{srv: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}, path: path}
	go func() { _ = s.srv.Serve(l) }()
	return s, nil
}

func (s *Server) Close() error {
	err := s.srv.Close()
	_ = os.Remove(s.path)
	return err
}

func client(path string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		}},
	}
}

func Get(path string) (Status, error) {
	var st Status
	resp, err := client(path).Get("http://ucfs/status")
	if err != nil {
		return st, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return st, errors.New(resp.Status)
	}
	return st, json.NewDecoder(resp.Body).Decode(&st)
}

// Unmount asks the daemon to unmount and exit, and waits until its socket
// is gone.
func Unmount(path string, timeout time.Duration) error {
	resp, err := client(path).Post("http://ucfs/unmount", "", nil)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not exit within %v", timeout)
}
