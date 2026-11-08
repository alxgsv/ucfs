// Package spool holds files written through the mount until they are
// uploaded: the spool files themselves, the commit triggers, the upload,
// the overwrite policy, the aliases written names leave in files/, and the
// upload journal.
package spool

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/index"
)

// States of a spool. Only Queued and Uploading spools are uploaded again by
// the next mount after a crash: a Writing spool may have been cut off
// mid-write, and a Failed one waits for `ucfs spool retry`.
const (
	Writing   = "writing"
	Queued    = "queued"
	Uploading = "uploading"
	Failed    = "failed"
)

const (
	TreeDir  = "tree"
	FilesDir = "files"
)

// Uploader is the part of api.Client a spool needs.
type Uploader interface {
	Upload(ctx context.Context, u api.Upload) (api.Uploaded, error)
	DeleteStorage(ctx context.Context, uuid string) error
	Info(ctx context.Context, uuid string) (api.File, error)
}

type Options struct {
	// Dir holds the spool files.
	Dir string
	// Metadata is added to every upload.
	Metadata map[string]string
	// Keep leaves the file an overwrite replaces in place; by default it is
	// soft-deleted once the new upload succeeds.
	Keep bool
	// Idle uploads a written file this long after its last write: NFS has
	// no close, and COMMIT may never come.
	Idle time.Duration
	// Settle is how long a COMMIT waits for further writes before the
	// upload starts.
	Settle time.Duration
	// MultipartThreshold is the size above which a file is sent in parts.
	MultipartThreshold int64
	Concurrency        int
	AliasTTL           time.Duration
	// Attempts bounds tries of an upload that fails with a network error, a
	// 5xx or a throttle.
	Attempts int
	// RecentGrace is how long after its upload a file can still take a
	// write that continues the one that was uploaded.
	RecentGrace time.Duration
}

// Manager owns every spool of one cache dir.
type Manager struct {
	ix      *index.Index
	up      Uploader
	o       Options
	journal *Journal
	// OnChange runs after spools appear, move, go away or finish.
	OnChange func()
	// OnUploaded runs with a finished upload's bytes before the spool file
	// goes away, so they can seed the block cache.
	OnUploaded func(uuid string, data io.ReaderAt, size int64)

	ctx  context.Context
	stop context.CancelFunc
	sem  chan struct{}
	wg   sync.WaitGroup

	mu     sync.Mutex
	byIno  map[uint64]*Spool
	byPath map[string]*Spool
	recent map[string]time.Time

	uploads, failures, bytes atomic.Int64
}

// Spool is one written file that is not uploaded yet.
type Spool struct {
	m    *Manager
	mu   sync.Mutex
	row  index.SpoolRow
	f    *os.File
	size int64
	// mtime moves with every write; gen counts writes, so an upload of
	// content that a later write changed is recognised as stale.
	mtime   time.Time
	gen     uint64
	timer   *time.Timer
	cancel  context.CancelFunc
	dropped bool
	waiters []chan error
	// force uploads an empty file, which only `ucfs spool retry` asks for.
	force bool
}

func defaults(o Options) Options {
	if o.Idle == 0 {
		o.Idle = 3 * time.Second
	}
	if o.Settle == 0 {
		o.Settle = 300 * time.Millisecond
	}
	if o.MultipartThreshold == 0 {
		o.MultipartThreshold = api.MinMultipart
	}
	if o.Concurrency == 0 {
		o.Concurrency = 8
	}
	if o.AliasTTL == 0 {
		o.AliasTTL = time.Hour
	}
	if o.Attempts == 0 {
		o.Attempts = 3
	}
	if o.RecentGrace == 0 {
		o.RecentGrace = time.Minute
	}
	return o
}

// Open loads the spools a previous mount left in the index. It uploads
// nothing; Recover does that.
func Open(ctx context.Context, ix *index.Index, up Uploader, journal *Journal, o Options) (*Manager, error) {
	o = defaults(o)
	if err := os.MkdirAll(o.Dir, 0o700); err != nil {
		return nil, err
	}
	mctx, stop := context.WithCancel(context.WithoutCancel(ctx))
	m := &Manager{ix: ix, up: up, o: o, journal: journal, ctx: mctx, stop: stop,
		sem: make(chan struct{}, o.Concurrency), byIno: map[uint64]*Spool{}, byPath: map[string]*Spool{},
		recent: map[string]time.Time{}, OnChange: func() {}}
	rows, err := ix.Spools(ctx)
	if err != nil {
		stop()
		return nil, err
	}
	for _, r := range rows {
		f, err := os.OpenFile(m.dataPath(r.ID), os.O_RDWR|os.O_CREATE, 0o600)
		if err != nil {
			stop()
			return nil, err
		}
		info, err := f.Stat()
		if err != nil {
			_ = f.Close()
			stop()
			return nil, err
		}
		s := &Spool{m: m, row: r, f: f, size: info.Size(), mtime: info.ModTime()}
		m.byIno[r.Object.Ino], m.byPath[r.Path] = s, s
	}
	return m, nil
}

func (m *Manager) dataPath(id string) string { return filepath.Join(m.o.Dir, id) }

// NewIno draws an inode number for a new object.
func NewIno() uint64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint64(b[:])>>1 | 1<<62
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ValidName reports whether a written name can be uploaded as is. A base
// upload keeps only what follows the last backslash (Spike D).
func ValidName(name string) error {
	if strings.ContainsRune(name, '\\') {
		return syscall.EINVAL
	}
	return nil
}

// TreePath is the ucfs:path value for a spool path under tree/, or "" for
// a path elsewhere.
func TreePath(p string) string {
	rest, ok := strings.CutPrefix(p, TreeDir+"/")
	if !ok {
		return ""
	}
	return "/" + rest
}

// Create starts a spool for a new file at p, a path relative to the mount
// root. o is the identity it keeps through upload and renames; replaces is
// the uuid of the file it overwrites, if any.
func (m *Manager) Create(ctx context.Context, p string, o index.Object, replaces string) (*Spool, error) {
	if err := ValidName(path.Base(p)); err != nil {
		return nil, err
	}
	m.mu.Lock()
	old := m.byPath[p]
	m.mu.Unlock()
	if old != nil {
		if err := m.Drop(ctx, old); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	row := index.SpoolRow{ID: newID(), Object: o, Path: p, State: Writing, Replaces: replaces, Created: now}
	f, err := os.OpenFile(m.dataPath(row.ID), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if err := m.ix.PutSpool(ctx, row); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	s := &Spool{m: m, row: row, f: f, mtime: now}
	m.mu.Lock()
	m.byIno[o.Ino], m.byPath[p] = s, s
	m.mu.Unlock()
	m.OnChange()
	return s, nil
}

// ByIno returns the spool an object is being written into.
func (m *Manager) ByIno(ino uint64) *Spool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byIno[ino]
}

// At returns the spool at a path.
func (m *Manager) At(p string) *Spool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byPath[p]
}

// In returns the spools directly in dir, a path relative to the mount root.
func (m *Manager) In(dir string) []*Spool {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Spool
	for p, s := range m.byPath {
		if path.Dir(p) == dir {
			out = append(out, s)
		}
	}
	return out
}

// Below returns the spools anywhere below dir.
func (m *Manager) Below(dir string) []*Spool {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*Spool
	for p, s := range m.byPath {
		if strings.HasPrefix(p, dir+"/") {
			out = append(out, s)
		}
	}
	return out
}

// Recent reports whether this mount uploaded uuid within RecentGrace, so a
// write to it continues the write that was uploaded too early.
func (m *Manager) Recent(uuid string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.recent[uuid]
	return ok && time.Since(t) < m.o.RecentGrace
}

func (m *Manager) unregister(s *Spool) {
	m.mu.Lock()
	if m.byIno[s.row.Object.Ino] == s {
		delete(m.byIno, s.row.Object.Ino)
	}
	if m.byPath[s.row.Path] == s {
		delete(m.byPath, s.row.Path)
	}
	m.mu.Unlock()
}

// Info is a snapshot of a spool for listings and `ucfs spool list`.
type Info struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"`
	State      string    `json:"state"`
	Size       int64     `json:"size"`
	Replaces   string    `json:"replaces,omitempty"`
	Created    time.Time `json:"created"`
	Modified   time.Time `json:"modified"`
	Committed  time.Time `json:"committed,omitzero"`
	Attempts   int       `json:"attempts"`
	Error      string    `json:"error,omitempty"`
	UploadUUID string    `json:"upload_uuid,omitempty"`
	Ino        uint64    `json:"-"`
	Key        string    `json:"-"`
}

func (s *Spool) Info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.row
	return Info{ID: r.ID, Path: r.Path, State: r.State, Size: s.size, Replaces: r.Replaces, Created: r.Created,
		Modified: s.mtime, Committed: r.Committed, Attempts: r.Attempts, Error: r.Error, UploadUUID: r.UploadUUID,
		Ino: r.Object.Ino, Key: r.Object.Key}
}

// List returns every spool, oldest first.
func (m *Manager) List() []Info {
	m.mu.Lock()
	all := make([]*Spool, 0, len(m.byPath))
	for _, s := range m.byPath {
		all = append(all, s)
	}
	m.mu.Unlock()
	out := make([]Info, len(all))
	for i, s := range all {
		out[i] = s.Info()
	}
	slices.SortFunc(out, func(a, b Info) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// Find returns the spool with an id.
func (m *Manager) Find(id string) *Spool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.byPath {
		if s.row.ID == id {
			return s
		}
	}
	return nil
}

// ReadAt fails with ESTALE once the spool is gone, uploaded or dropped;
// the caller resolves the file again.
func (s *Spool) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped {
		return 0, syscall.ESTALE
	}
	size := s.size
	if off >= size {
		return 0, io.EOF
	}
	if int64(len(p)) > size-off {
		p = p[:size-off]
	}
	n, err := s.f.ReadAt(p, off)
	if err == nil && off+int64(n) >= size {
		err = io.EOF
	}
	return n, err
}

// Queued returns the metadata set on the spool through extended attributes.
func (s *Spool) Queued() index.Queued {
	s.mu.Lock()
	defer s.mu.Unlock()
	return copyQueued(s.row.Queued)
}

// Queue changes the metadata the upload form will carry; fn edits a copy.
// Once the upload is under way its form is sent, so changes fail with
// EAGAIN until it ends.
func (s *Spool) Queue(fn func(*index.Queued) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.dropped:
		return syscall.ESTALE
	case s.row.State == Uploading:
		return syscall.EAGAIN
	}
	q := copyQueued(s.row.Queued)
	if err := fn(&q); err != nil {
		return err
	}
	s.row.Queued = q
	s.persistLocked()
	return nil
}

func copyQueued(q index.Queued) index.Queued {
	out := index.Queued{Meta: make(map[string]string, len(q.Meta))}
	for k, v := range q.Meta {
		out.Meta[k] = v
	}
	if q.Tags != nil {
		tags := slices.Clone(*q.Tags)
		if tags == nil {
			tags = []string{}
		}
		out.Tags = &tags
	}
	return out
}

// changedLocked records a write: an upload in flight is stale now, and the
// idle timer starts over.
func (s *Spool) changedLocked() {
	s.gen++
	s.mtime = time.Now()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	if s.row.State != Writing {
		s.row.State, s.row.Error = Writing, ""
		s.persistLocked()
	}
	s.armLocked(s.m.o.Idle)
}

func (s *Spool) WriteAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped {
		return 0, syscall.ESTALE
	}
	s.changedLocked()
	n, err := s.f.WriteAt(p, off)
	s.size = max(s.size, off+int64(n))
	return n, err
}

// Truncate to the current length changes nothing: macOS cp sends one after
// every write (Spike C).
func (s *Spool) Truncate(size int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped {
		return syscall.ESTALE
	}
	if size == s.size {
		return nil
	}
	s.changedLocked()
	if err := s.f.Truncate(size); err != nil {
		return err
	}
	s.size = size
	return nil
}

// Commit is the NFS COMMIT trigger: upload once no write follows for Settle.
func (s *Spool) Commit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	slog.Debug("commit", "path", s.row.Path, "state", s.row.State, "size", s.size)
	if s.row.State == Writing && !s.dropped {
		s.armLocked(s.m.o.Settle)
	}
}

func (s *Spool) armLocked(d time.Duration) {
	if s.timer == nil {
		s.timer = time.AfterFunc(d, func() { s.m.enqueue(s, false) })
		return
	}
	s.timer.Reset(d)
}

// Flush is the FUSE close trigger: it uploads what was written and waits
// for the result. An empty file stays local, as does one already uploaded
// or failed with no write since.
func (s *Spool) Flush(ctx context.Context) error {
	s.mu.Lock()
	if s.dropped || (s.row.State == Writing && s.size == 0) || s.row.State == Failed {
		s.mu.Unlock()
		return nil
	}
	ch := make(chan error, 1)
	s.waiters = append(s.waiters, ch)
	state := s.row.State
	s.mu.Unlock()
	if state == Writing {
		s.m.enqueue(s, false)
	}
	select {
	case err := <-ch:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Spool) wakeLocked(err error) {
	for _, ch := range s.waiters {
		ch <- err
	}
	s.waiters = nil
}

func (s *Spool) persistLocked() {
	if err := s.m.ix.PutSpool(context.Background(), s.row); err != nil {
		slog.Warn("record spool", "id", s.row.ID, "path", s.row.Path, "err", err)
	}
}

// Drop discards a spool: rm of a file not uploaded yet, or a path written
// over by a rename. An upload in flight is abandoned; if it still
// completes, the uploaded file is deleted.
func (m *Manager) Drop(ctx context.Context, s *Spool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped {
		return nil
	}
	s.dropped = true
	if s.timer != nil {
		s.timer.Stop()
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.wakeLocked(syscall.ENOENT)
	m.unregister(s)
	_ = s.f.Close()
	err := errors.Join(os.Remove(s.f.Name()), m.ix.DeleteSpool(ctx, s.row.ID))
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	m.OnChange()
	return err
}

// Move renames a spool. A spool already at to is dropped. A spool that was
// committed is uploaded again under its new name, so the name it is stored
// with is the final one.
func (m *Manager) Move(ctx context.Context, s *Spool, to, replaces string) error {
	if err := ValidName(path.Base(to)); err != nil {
		return err
	}
	if other := m.At(to); other != nil && other != s {
		if err := m.Drop(ctx, other); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dropped {
		return syscall.ENOENT
	}
	m.mu.Lock()
	if m.byPath[s.row.Path] == s {
		delete(m.byPath, s.row.Path)
	}
	m.byPath[to] = s
	m.mu.Unlock()
	s.row.Path, s.row.Replaces = to, replaces
	if s.row.State != Writing {
		s.gen++
		if s.cancel != nil {
			s.cancel()
			s.cancel = nil
		}
		s.row.State, s.row.Error = Writing, ""
		s.armLocked(m.o.Settle)
	}
	s.persistLocked()
	m.OnChange()
	return nil
}

// Retry uploads a spool now, even an empty one.
func (m *Manager) Retry(s *Spool) {
	s.mu.Lock()
	s.force = true
	if s.row.State == Failed {
		s.row.State = Writing
	}
	s.mu.Unlock()
	m.enqueue(s, true)
}

// Stats counts this manager's uploads and its spools by state.
type Stats struct {
	Uploads, Failures, Bytes           int64
	Writing, Queued, Uploading, Failed int
}

func (m *Manager) Stats() Stats {
	st := Stats{Uploads: m.uploads.Load(), Failures: m.failures.Load(), Bytes: m.bytes.Load()}
	for _, in := range m.List() {
		switch in.State {
		case Writing:
			st.Writing++
		case Queued:
			st.Queued++
		case Uploading:
			st.Uploading++
		case Failed:
			st.Failed++
		}
	}
	return st
}

// Shutdown uploads every non-empty spool that is still being written,
// waits up to timeout for the uploads in flight, and abandons the rest:
// their rows stay queued, and the next mount uploads them.
func (m *Manager) Shutdown(timeout time.Duration) {
	m.mu.Lock()
	all := make([]*Spool, 0, len(m.byPath))
	for _, s := range m.byPath {
		all = append(all, s)
	}
	m.mu.Unlock()
	for _, s := range all {
		s.mu.Lock()
		writing := s.row.State == Writing && s.size > 0 && !s.dropped
		s.mu.Unlock()
		if writing {
			m.enqueue(s, false)
		}
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		slog.Warn("uploads still running at unmount; the next mount retries them")
	}
	m.stop()
	for _, s := range all {
		s.mu.Lock()
		if s.timer != nil {
			s.timer.Stop()
		}
		s.mu.Unlock()
	}
}

func (s *Spool) String() string { return fmt.Sprintf("spool %s (%s)", s.row.ID, s.row.Path) }
