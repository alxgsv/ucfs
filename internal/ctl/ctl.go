// Package ctl is the unix-socket control API of a running mount, used by
// `ucfs status`, `ucfs umount`, `ucfs resolve` and `ucfs spool`.
package ctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type Status struct {
	MountDir string    `json:"mount_dir"`
	Frontend string    `json:"frontend"`
	Project  string    `json:"project"`
	PID      int       `json:"pid"`
	Started  time.Time `json:"started"`

	Views  []string     `json:"views"`
	Index  IndexStatus  `json:"index"`
	CDN    CDNStatus    `json:"cdn"`
	Budget *Budget      `json:"budget,omitempty"`
	Cache  CacheStatus  `json:"cache"`
	API    APIStatus    `json:"api"`
	Meta   MetaStatus   `json:"meta"`
	Writes WritesStatus `json:"writes"`
	// Process is the daemon's own use of the machine, for soak runs.
	Process ProcessStatus `json:"process"`
}

// Budget is the daily traffic budget: CDN bytes used in the window that
// resets at ResetsAt, and reads refused for it.
type Budget struct {
	Limit     int64     `json:"limit"`
	Used      int64     `json:"used"`
	Remaining int64     `json:"remaining"`
	ResetsAt  time.Time `json:"resets_at"`
	Refused   int64     `json:"refused"`
	Prefetch  bool      `json:"prefetch"`
}

// ProcessStatus counts goroutines, open file descriptors and Go heap bytes
// in use.
type ProcessStatus struct {
	Goroutines int    `json:"goroutines"`
	FDs        int    `json:"fds"`
	HeapBytes  uint64 `json:"heap_bytes"`
}

// MetaStatus counts file info refreshes (lazy, when an xattr read or a
// resolve finds the info older than the TTL), extended attribute calls,
// CDN HEADs for user.uc.md5 and group info reads.
type MetaStatus struct {
	TTL           time.Duration `json:"ttl"`
	Refreshes     int64         `json:"refreshes"`
	RefreshErrors int64         `json:"refresh_errors"`
	XattrLists    int64         `json:"xattr_lists"`
	XattrGets     int64         `json:"xattr_gets"`
	XattrSets     int64         `json:"xattr_sets"`
	XattrRemoves  int64         `json:"xattr_removes"`
	MD5Heads      int64         `json:"md5_heads"`
	GroupFetches  int64         `json:"group_fetches"`
}

// APIStatus counts requests to the REST API, the Upload API, and the
// storage hosts multipart parts go to.
type APIStatus struct {
	REST      int `json:"rest"`
	Upload    int `json:"upload"`
	Parts     int `json:"parts"`
	Throttled int `json:"throttled"`
	// State is ok, denied or throttled, as the latest REST answers showed,
	// since Since; Detail is what the API said.
	State  string    `json:"state"`
	Since  time.Time `json:"since,omitzero"`
	Detail string    `json:"detail,omitempty"`
	// Degraded is set once the state has lasted a sync interval: the mount
	// then serves the index and refuses writes.
	Degraded bool `json:"degraded"`
}

type WritesStatus struct {
	Enabled bool `json:"enabled"`
	// Overwrite is the overwrite policy, replace or keep.
	Overwrite     string `json:"overwrite,omitempty"`
	Writing       int    `json:"writing"`
	Queued        int    `json:"queued"`
	Uploading     int    `json:"uploading"`
	Failed        int    `json:"failed"`
	Uploads       int64  `json:"uploads"`
	UploadFails   int64  `json:"upload_failures"`
	UploadBytes   int64  `json:"upload_bytes"`
	ShadowCreates int64  `json:"shadow_creates"`
	ShadowWrites  int64  `json:"shadow_writes"`
	ShadowBytes   int64  `json:"shadow_bytes"`
}

type IndexStatus struct {
	Files     int       `json:"files"`
	Bytes     int64     `json:"bytes"`
	LastSync  time.Time `json:"last_sync"`
	SyncError string    `json:"sync_error,omitempty"`
	// SyncFailures counts failed syncs in a row; NextSync is when the
	// loop tries again.
	SyncFailures int       `json:"sync_failures,omitempty"`
	NextSync     time.Time `json:"next_sync,omitzero"`
	// DBBytes is the size of the index file with its write-ahead log.
	DBBytes int64 `json:"db_bytes"`
}

// CDNStatus counts requests and body bytes, the requests in flight and
// waiting for a slot, reads that waited and for how long in all, and
// prefetches dropped because too many were waiting. Changed counts ranges
// that came with another ETag than their object's.
type CDNStatus struct {
	Requests       int64 `json:"requests"`
	Heads          int64 `json:"heads"`
	Gets           int64 `json:"gets"`
	Bytes          int64 `json:"bytes"`
	MaxInflight    int   `json:"max_inflight"`
	Inflight       int   `json:"inflight"`
	Queued         int   `json:"queued"`
	QueuedPrefetch int   `json:"queued_prefetch"`
	Waits          int64 `json:"waits"`
	WaitedMS       int64 `json:"waited_ms"`
	Dropped        int64 `json:"prefetch_dropped"`
	Changed        int64 `json:"etag_changed"`
}

type CacheStatus struct {
	Served          int64 `json:"served"`
	Hit             int64 `json:"hit"`
	FetchedDemand   int64 `json:"fetched_demand"`
	FetchedPrefetch int64 `json:"fetched_prefetch"`
	Used            int64 `json:"used"`
	Max             int64 `json:"max"`
}

// maxSocketPath is sun_path on macOS (104 bytes) minus the trailing NUL.
const maxSocketPath = 103

func hash32(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// SocketPath is the control socket of a mount: in the project's cache dir,
// named after the mount point, or in a directory of the user's under the
// temp dir when that path would not fit in sun_path.
func SocketPath(cacheDir, mountDir string) string {
	p := filepath.Join(cacheDir, fmt.Sprintf("ctl-%08x.sock", hash32(mountDir)))
	if len(p) <= maxSocketPath {
		return p
	}
	return filepath.Join(tempSocketDir(), fmt.Sprintf("%08x-%08x.sock", hash32(cacheDir), hash32(mountDir)))
}

// tempSocketDir is where sockets go that do not fit in the cache dir. It
// is the user's own, since the temp dir is shared on Linux.
func tempSocketDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("ucfs-%d", os.Getuid()))
}

// Sockets lists the control sockets of mounts that use a cache dir.
func Sockets(cacheDir string) ([]string, error) {
	inCache, err := filepath.Glob(filepath.Join(cacheDir, "ctl-*.sock"))
	if err != nil {
		return nil, err
	}
	inTemp, err := filepath.Glob(filepath.Join(tempSocketDir(), fmt.Sprintf("%08x-*.sock", hash32(cacheDir))))
	return append(inCache, inTemp...), err
}

// PrivateDir makes dir, or fixes it to mode 0700 if it exists with another
// mode, and reports whether it had to fix it. A dir that is a symlink or
// another user's is an error: someone else could read what goes in it.
func PrivateDir(dir string) (fixed bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return false, err
	}
	if !fi.IsDir() || !ownedByUs(fi) {
		return false, fmt.Errorf("%s is not a directory of this user's", dir)
	}
	if fi.Mode().Perm() == 0o700 {
		return false, nil
	}
	return true, os.Chmod(dir, 0o700)
}

func ownedByUs(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

type Server struct {
	srv  *http.Server
	path string
}

// Route is one more endpoint of the control API.
type Route struct {
	Pattern string
	Handler http.HandlerFunc
}

// Serve answers GET /status, POST /unmount and the given routes on path. A
// socket left by a daemon that died is replaced; a live one is an error.
func Serve(path string, status func() Status, unmount func(), routes ...Route) (*Server, error) {
	if _, err := Get(path); err == nil {
		return nil, fmt.Errorf("a ucfs daemon already serves %s", path)
	}
	if _, err := PrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
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
	for _, r := range routes {
		mux.HandleFunc(r.Pattern, r.Handler)
	}
	s := &Server{srv: &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}, path: path}
	go func() { _ = s.srv.Serve(l) }()
	return s, nil
}

func (s *Server) Close() error {
	err := s.srv.Close()
	_ = os.Remove(s.path)
	return err
}

// client is a client for one request. Each call makes a new transport, so
// a connection kept alive would outlive it in an idle pool nothing reuses:
// a process that asks for the status every few seconds would gather one
// connection, and the daemon one goroutine, per request.
func client(path string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
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

// Call sends in as JSON to a route of the daemon on path and decodes the
// JSON answer into out. An answer other than 200 is an error carrying its
// body.
func Call(path, method, route string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://ucfs"+route, body)
	if err != nil {
		return err
	}
	c := client(path)
	c.Timeout = time.Minute
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s", strings.TrimSpace(string(msg)))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Reply writes v as the JSON answer of a route, or err as a 400.
func Reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
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
