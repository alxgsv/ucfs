package spool

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/index"
)

// enqueue starts the upload of what a spool holds now. An empty spool stays
// local unless force is set.
func (m *Manager) enqueue(s *Spool, force bool) {
	s.mu.Lock()
	if s.dropped || s.row.State != Writing || (s.size == 0 && !force && !s.force) || m.ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	if s.timer != nil {
		s.timer.Stop()
	}
	s.row.State, s.row.Committed = Queued, time.Now()
	slog.Debug("upload queued", "path", s.row.Path, "size", s.size)
	gen := s.gen
	ctx, cancel := context.WithCancel(m.ctx)
	s.cancel = cancel
	s.persistLocked()
	s.mu.Unlock()
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		m.run(ctx, s, gen)
	}()
}

// run uploads one generation of a spool, retrying network errors, 5xx
// answers and throttles with backoff.
func (m *Manager) run(ctx context.Context, s *Spool, gen uint64) {
	select {
	case m.sem <- struct{}{}:
		defer func() { <-m.sem }()
	case <-ctx.Done():
		return
	}
	for attempt := 1; ; attempt++ {
		s.mu.Lock()
		if s.dropped || s.gen != gen || ctx.Err() != nil {
			s.mu.Unlock()
			return
		}
		s.row.State, s.row.Attempts = Uploading, s.row.Attempts+1
		s.persistLocked()
		u := m.prepare(ctx, s)
		s.mu.Unlock()

		start := time.Now()
		res, err := m.up.Upload(ctx, u)
		if err == nil {
			s.mu.Lock()
			stale := s.dropped || s.gen != gen
			if !stale {
				m.finish(ctx, s, u, res, time.Since(start))
			}
			s.mu.Unlock()
			if stale {
				m.discard(res.UUID)
			}
			return
		}
		errno := api.Errno(err)
		if ctx.Err() != nil {
			if u.Multipart && s.row.UploadUUID != "" {
				m.discard(s.row.UploadUUID)
			}
			return
		}
		transient := errno == syscall.EIO || errno == syscall.EAGAIN
		if transient && attempt < m.o.Attempts {
			slog.Warn("upload failed, retrying", "path", u.Name, "attempt", attempt, "err", err)
			select {
			case <-time.After(time.Duration(attempt*attempt) * time.Second):
				continue
			case <-ctx.Done():
				return
			}
		}
		s.mu.Lock()
		if !s.dropped && s.gen == gen {
			s.row.State, s.row.Error = Failed, err.Error()
			s.persistLocked()
			s.wakeLocked(errno)
		}
		s.mu.Unlock()
		m.failures.Add(1)
		slog.Error("upload failed; kept for `ucfs spool retry`", "path", s.row.Path, "errno", errno.Error(), "err", err)
		m.OnChange()
		return
	}
}

// discard soft-deletes a file uploaded for content that changed or went
// away while it was uploading. A multipart upload that never completed is
// not a file yet, so a 404 is fine.
func (m *Manager) discard(uuid string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := m.up.DeleteStorage(ctx, uuid); err != nil && api.Errno(err) != syscall.ENOENT {
		slog.Warn("delete stale upload", "uuid", uuid, "err", err)
		return
	}
	_ = m.ix.MarkRemoved(ctx, uuid)
	slog.Info("deleted stale upload", "uuid", uuid)
}

// prepare builds the upload of a spool: its written name, the metadata of
// the file it replaces with the configured metadata, the metadata queued
// on the spool and its tree path on top, and the replaced file's tags
// unless tags were queued. The replaced file's metadata comes from the
// API: listings do not show changes other clients made after a file was
// first listed, so the index may hold old values.
func (m *Manager) prepare(ctx context.Context, s *Spool) api.Upload {
	meta := map[string]string{}
	var tags []string
	if s.row.Replaces != "" {
		old, oldTags := m.replacedMeta(ctx, s.row.Replaces)
		for k, v := range old {
			meta[k] = v
		}
		tags = oldTags
	}
	for k, v := range m.o.Metadata {
		meta[k] = v
	}
	for k, v := range s.row.Queued.Meta {
		meta[k] = v
	}
	if q := s.row.Queued.Tags; q != nil {
		tags = *q
	}
	delete(meta, index.TreePathKey)
	if tp := TreePath(s.row.Path); tp != "" {
		meta[index.TreePathKey] = tp
	}
	name := path.Base(s.row.Path)
	// A base upload keeps only what follows the last backslash of a name
	// (Spike D), so such a name goes up with underscores and whole in
	// ucfs:name, which the index shows instead.
	sent := name
	delete(meta, index.NameMetaKey)
	if strings.ContainsRune(name, '\\') {
		sent = strings.ReplaceAll(name, `\`, "_")
		meta[index.NameMetaKey] = name
	}
	return api.Upload{
		Name: sent, Size: s.size, MIME: guessMIME(name, s.f), Data: s.f, Metadata: meta, Tags: tags,
		Multipart: s.size > max(m.o.MultipartThreshold, api.MinMultipart) || s.size > api.MaxBase,
		Started:   func(uuid string) { s.startedUpload(uuid) },
	}
}

func (m *Manager) replacedMeta(ctx context.Context, uuid string) (map[string]string, []string) {
	f, err := m.up.Info(ctx, uuid)
	if err == nil {
		return f.Metadata, f.Tags
	}
	slog.Warn("metadata of the replaced file from the API; using the index", "uuid", uuid, "err", err)
	meta, tags, err := m.ix.Meta(ctx, uuid)
	if err != nil {
		slog.Warn("metadata of the replaced file", "uuid", uuid, "err", err)
	}
	return meta, tags
}

func (s *Spool) startedUpload(uuid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.row.UploadUUID = uuid
	s.persistLocked()
}

// guessMIME is the type sent with the upload, which the Upload API
// requires for parts; the server detects its own from the content.
func guessMIME(name string, f *os.File) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return strings.SplitN(t, ";", 2)[0]
	}
	head := make([]byte, 512)
	n, _ := f.ReadAt(head, 0)
	return strings.SplitN(http.DetectContentType(head[:n]), ";", 2)[0]
}

// finish records a completed upload, with the spool's lock held: the
// object now holds the new file, the replaced file is soft-deleted unless
// the policy keeps it, a name written in files/ becomes an alias, and the
// journal gets a line.
func (m *Manager) finish(ctx context.Context, s *Spool, u api.Upload, res api.Uploaded, took time.Duration) {
	now := time.Now().UTC()
	mimeType := res.MIME
	if mimeType == "" {
		mimeType = u.MIME
	}
	ctx = context.WithoutCancel(ctx)
	r := s.row
	if err := m.ix.PutObject(ctx, r.Object, res.UUID); err != nil {
		slog.Warn("record object", "uuid", res.UUID, "err", err)
	}
	f := api.File{UUID: res.UUID, Name: u.Name, Size: u.Size, MIME: mimeType, IsReady: true,
		UploadedAt: now, StoredAt: now, Metadata: u.Metadata, Tags: u.Tags}
	if err := m.ix.PutUploaded(ctx, f); err != nil {
		slog.Warn("record upload", "uuid", res.UUID, "err", err)
	}
	// The CDN's ETag of a multipart upload is not the content's MD5.
	sum := md5.New()
	if _, err := io.Copy(sum, io.NewSectionReader(s.f, 0, u.Size)); err == nil {
		err = m.ix.SetMD5(ctx, res.UUID, hex.EncodeToString(sum.Sum(nil)))
		if err != nil {
			slog.Warn("record md5", "uuid", res.UUID, "err", err)
		}
	}
	if r.Replaces != "" && r.Replaces != res.UUID {
		m.replaced(ctx, r.Replaces, res.UUID)
	}
	if name, ok := strings.CutPrefix(r.Path, FilesDir+"/"); ok {
		if err := m.ix.PutAlias(ctx, index.Alias{View: FilesDir, Name: name, UUID: res.UUID, Expires: now.Add(m.o.AliasTTL)}); err != nil {
			slog.Warn("record alias", "name", name, "err", err)
		}
	}
	if err := m.journal.Append(Entry{Time: now, Path: r.Path, UUID: res.UUID, Size: u.Size, MIME: mimeType}); err != nil {
		slog.Warn("journal", "err", err)
	}
	if m.OnUploaded != nil {
		m.OnUploaded(res.UUID, s.f, u.Size)
	}
	s.dropped = true
	if s.timer != nil {
		s.timer.Stop()
	}
	m.unregister(s)
	_ = s.f.Close()
	if err := errors.Join(os.Remove(s.f.Name()), m.ix.DeleteSpool(ctx, r.ID)); err != nil {
		slog.Warn("remove spool", "id", r.ID, "err", err)
	}
	m.mu.Lock()
	m.recent[res.UUID] = time.Now()
	for k, t := range m.recent {
		if time.Since(t) > m.o.RecentGrace {
			delete(m.recent, k)
		}
	}
	m.mu.Unlock()
	m.uploads.Add(1)
	m.bytes.Add(u.Size)
	slog.Info("uploaded", "path", r.Path, "uuid", res.UUID, "size", u.Size, "multipart", u.Multipart,
		"took", took.Round(time.Millisecond))
	s.wakeLocked(nil)
	m.OnChange()
}

// replaced applies the overwrite policy to the file a spool replaced. With
// replace, the old file is soft-deleted; with keep, it stays and gets an
// identity of its own, since the object it had now holds the new file.
func (m *Manager) replaced(ctx context.Context, old, uuid string) {
	if err := m.ix.RetargetAliases(ctx, old, uuid); err != nil {
		slog.Warn("retarget aliases", "from", old, "to", uuid, "err", err)
	}
	if m.o.Keep {
		if err := m.ix.PutObject(ctx, index.Object{Ino: NewIno()}, old); err != nil {
			slog.Warn("record kept file", "uuid", old, "err", err)
		}
		return
	}
	if err := m.up.DeleteStorage(ctx, old); err != nil && api.Errno(err) != syscall.ENOENT {
		slog.Error("delete the replaced file; both stay", "uuid", old, "err", err)
		if err := m.ix.PutObject(ctx, index.Object{Ino: NewIno()}, old); err != nil {
			slog.Warn("record kept file", "uuid", old, "err", err)
		}
		return
	}
	if err := m.ix.MarkRemoved(ctx, old); err != nil {
		slog.Warn("record removal", "uuid", old, "err", err)
	}
}

// Recover finishes what a mount that died left behind. A spool that was
// queued or uploading is uploaded again, unless its upload reached the
// server before the crash: then only its bookkeeping is done. A multipart
// upload is known by the uuid its start returned; one that never completed
// is listed by the API as a file that is not ready, and is deleted before
// the new upload. A base upload is recognised in the index, synced since,
// by its name, size and tree path and an upload time after its commit.
func (m *Manager) Recover(ctx context.Context) {
	for _, in := range m.List() {
		if in.State != Queued && in.State != Uploading {
			continue
		}
		s := m.Find(in.ID)
		if s == nil {
			continue
		}
		if uuid, ok := m.reconcile(ctx, in); ok {
			s.mu.Lock()
			u := m.prepare(ctx, s)
			m.finish(ctx, s, u, api.Uploaded{UUID: uuid}, 0)
			s.mu.Unlock()
			slog.Info("upload found after restart", "path", in.Path, "uuid", uuid)
			continue
		}
		if in.UploadUUID != "" {
			m.discard(in.UploadUUID)
		}
		s.mu.Lock()
		s.row.State, s.row.UploadUUID = Writing, ""
		s.mu.Unlock()
		slog.Info("uploading again after restart", "path", in.Path, "size", in.Size)
		m.enqueue(s, false)
	}
}

func (m *Manager) reconcile(ctx context.Context, in Info) (string, bool) {
	if in.UploadUUID != "" {
		f, err := m.up.Info(ctx, in.UploadUUID)
		if err != nil && api.Errno(err) != syscall.ENOENT {
			slog.Warn("look for a multipart upload after restart", "path", in.Path, "uuid", in.UploadUUID, "err", err)
		}
		return in.UploadUUID, err == nil && f.IsReady && f.RemovedAt.IsZero() && f.Size == in.Size
	}
	found, err := m.ix.FindUploaded(ctx, path.Base(in.Path), in.Size, in.Committed.Add(-time.Minute))
	if err != nil {
		slog.Warn("look for an upload after restart", "path", in.Path, "err", err)
		return "", false
	}
	tp := TreePath(in.Path)
	for _, f := range found {
		if f.Object.Ino == 0 && f.TreePath == tp && f.UUID != in.Replaces {
			return f.UUID, true
		}
	}
	return "", false
}
