package spool

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/api"
	"github.com/alxgsv/ucfs/internal/config"
	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/index"
)

type env struct {
	srv     *fake.Server
	client  *api.Client
	ix      *index.Index
	m       *Manager
	dir     string
	journal *Journal
}

func newEnv(t *testing.T, o Options, files ...fake.File) *env {
	t.Helper()
	srv := fake.New(t)
	srv.Add(files...)
	client, err := api.New(&config.Config{PublicKey: "demopublickey", SecretKey: "secret", CDNBase: srv.CDNBase()}, srv.Transport())
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	ix, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ix.Close() })
	if _, err := ix.Sync(context.Background(), client, false); err != nil {
		t.Fatal(err)
	}
	e := &env{srv: srv, client: client, ix: ix, dir: dir, journal: &Journal{Path: filepath.Join(dir, "uploads.log")}}
	e.m = e.open(t, o)
	return e
}

func (e *env) open(t *testing.T, o Options) *Manager {
	t.Helper()
	o.Dir = filepath.Join(e.dir, "spool")
	if o.Idle == 0 {
		o.Idle = time.Hour
	}
	if o.Settle == 0 {
		o.Settle = 10 * time.Millisecond
	}
	m, err := Open(context.Background(), e.ix, e.client, e.journal, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Shutdown(5 * time.Second) })
	return m
}

func write(t *testing.T, s *Spool, data []byte) {
	t.Helper()
	for off := 0; off < len(data); off += 1 << 20 {
		end := min(off+1<<20, len(data))
		if _, err := s.WriteAt(data[off:end], int64(off)); err != nil {
			t.Fatal(err)
		}
	}
}

func (e *env) uploaded(t *testing.T, name string) fake.File {
	t.Helper()
	var match []fake.File
	all, err := e.ix.Files(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range all {
		if sf, ok := e.srv.File(f.UUID); ok && sf.Name == name && !sf.Removed {
			match = append(match, sf)
		}
	}
	if len(match) != 1 {
		t.Fatalf("%d uploads named %q", len(match), name)
	}
	return match[0]
}

func TestFlushUploadsWithMetadataAndJournal(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Options{Metadata: map[string]string{"ucfs:test": "1"}})
	o := index.Object{Ino: NewIno()}
	s, err := e.m.Create(ctx, "tree/photos/photo.jpg", o, "")
	if err != nil {
		t.Fatal(err)
	}
	body := bytes.Repeat([]byte("jpeg"), 1000)
	write(t, s, body)
	if err := s.Truncate(int64(len(body))); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	f := e.uploaded(t, "photo.jpg")
	if f.Metadata["ucfs:path"] != "/photos/photo.jpg" || f.Metadata["ucfs:test"] != "1" || !f.Stored {
		t.Errorf("uploaded %+v", f)
	}
	if !bytes.Equal(e.srv.Body(f.UUID), body) {
		t.Error("content mismatch")
	}
	if got := e.m.ByIno(o.Ino); got != nil {
		t.Error("spool still registered after upload")
	}
	uuid, obj, err := e.ix.ObjectByIno(ctx, o.Ino)
	if err != nil || uuid != f.UUID || obj != o {
		t.Errorf("object %d holds %q %+v, %v", o.Ino, uuid, obj, err)
	}
	ixf, err := e.ix.Get(ctx, f.UUID)
	if err != nil || ixf.TreePath != "/photos/photo.jpg" || ixf.Object != o {
		t.Errorf("index row %+v, %v", ixf, err)
	}
	if en, ok := e.journal.Last("tree/photos/photo.jpg"); !ok || en.UUID != f.UUID || en.Size != int64(len(body)) || en.MIME != "image/jpeg" {
		t.Errorf("journal entry %+v, %v", en, ok)
	}
	if left, _ := os.ReadDir(filepath.Join(e.dir, "spool")); len(left) != 0 {
		t.Errorf("spool files left: %v", left)
	}
	if rows, _ := e.ix.Spools(ctx); len(rows) != 0 {
		t.Errorf("spool rows left: %+v", rows)
	}
}

func TestEmptyStaysLocalAndCommitUploads(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Options{})
	s, err := e.m.Create(ctx, "files/a.txt", index.Object{Ino: NewIno()}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil || e.srv.UploadStats().Base != 0 {
		t.Fatalf("flush of an empty file: %v, %d uploads", err, e.srv.UploadStats().Base)
	}
	s.Commit()
	time.Sleep(50 * time.Millisecond)
	if e.srv.UploadStats().Base != 0 {
		t.Fatal("empty file uploaded on COMMIT")
	}
	write(t, s, []byte("hello"))
	s.Commit()
	deadline := time.Now().Add(5 * time.Second)
	for e.m.At("files/a.txt") != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	f := e.uploaded(t, "a.txt")
	if uuid, err := e.ix.Alias(ctx, "files", "a.txt", time.Now()); err != nil || uuid != f.UUID {
		t.Errorf("alias = %q, %v; want %s", uuid, err, f.UUID)
	}
	if _, ok := f.Metadata["ucfs:path"]; ok {
		t.Error("a file written in files/ got a tree path")
	}
}

func TestIdleTrigger(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Options{Idle: 50 * time.Millisecond})
	s, _ := e.m.Create(ctx, "tree/x.txt", index.Object{Ino: NewIno()}, "")
	write(t, s, []byte("abc"))
	deadline := time.Now().Add(5 * time.Second)
	for e.m.At("tree/x.txt") != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	e.uploaded(t, "x.txt")
}

func TestOverwritePolicies(t *testing.T) {
	ctx := context.Background()
	for _, keep := range []bool{false, true} {
		old := fake.File{UUID: "00000000-0000-4000-8000-000000000001", Name: "doc.txt", Size: 3, Uploaded: time.Now().Add(-time.Hour),
			Stored: true, Metadata: map[string]string{"ucfs:path": "/doc.txt", "camera": "Canon"}, Tags: []string{"approved"}}
		e := newEnv(t, Options{Keep: keep, Metadata: map[string]string{"ucfs:test": "1"}}, old)
		if err := e.ix.PutAlias(ctx, index.Alias{View: "files", Name: "doc.txt", UUID: old.UUID, Expires: time.Now().Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		o := index.Object{Ino: NewIno()}
		s, err := e.m.Create(ctx, "tree/doc.txt", o, old.UUID)
		if err != nil {
			t.Fatal(err)
		}
		write(t, s, []byte("new content"))
		if err := s.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		got, _, _ := e.ix.ObjectByIno(ctx, o.Ino)
		nf, _ := e.srv.File(got)
		if nf.Metadata["camera"] != "Canon" || nf.Metadata["ucfs:path"] != "/doc.txt" || nf.Metadata["ucfs:test"] != "1" ||
			strings.Join(nf.Tags, ",") != "approved" {
			t.Errorf("keep=%v: new file %+v", keep, nf)
		}
		of, _ := e.srv.File(old.UUID)
		if of.Removed == keep {
			t.Errorf("keep=%v: old file removed = %v", keep, of.Removed)
		}
		if a, _ := e.ix.Alias(ctx, "files", "doc.txt", time.Now()); a != got {
			t.Errorf("keep=%v: alias points at %s, want %s", keep, a, got)
		}
		if keep {
			ixo, _ := e.ix.Get(ctx, old.UUID)
			if ixo.Object.Ino == 0 || ixo.Object.Ino == o.Ino {
				t.Errorf("kept file has object %+v", ixo.Object)
			}
		}
	}
}

func TestRejectedUploadFailsFlushAndRetries(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Options{})
	e.srv.RejectUploads(func(name string) bool { return strings.HasSuffix(name, ".exe") })
	s, _ := e.m.Create(ctx, "tree/x.exe", index.Object{Ino: NewIno()}, "")
	write(t, s, []byte("MZ"))
	if err := s.Flush(ctx); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("flush = %v, want EPERM", err)
	}
	if in := s.Info(); in.State != Failed || !strings.Contains(in.Error, "not allowed") {
		t.Errorf("spool %+v", in)
	}
	if err := s.Flush(ctx); err != nil {
		t.Errorf("second flush with no write since = %v", err)
	}
	e.srv.RejectUploads(nil)
	e.m.Retry(s)
	deadline := time.Now().Add(5 * time.Second)
	for e.m.At("tree/x.exe") != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	e.uploaded(t, "x.exe")
}

func TestTransientFailuresAreRetried(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Options{})
	e.srv.FailUploads(1)
	s, _ := e.m.Create(ctx, "tree/y.txt", index.Object{Ino: NewIno()}, "")
	write(t, s, []byte("y"))
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if st := e.srv.UploadStats(); st.Base != 1 {
		t.Errorf("stats %+v", st)
	}
}

func TestWriteDuringUploadMakesItStale(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Options{})
	e.srv.SetUploadLatency(200 * time.Millisecond)
	s, _ := e.m.Create(ctx, "tree/z.txt", index.Object{Ino: NewIno()}, "")
	write(t, s, []byte("first"))
	e.m.enqueue(s, false)
	time.Sleep(50 * time.Millisecond)
	write(t, s, []byte("second"))
	e.srv.SetUploadLatency(0)
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	f := e.uploaded(t, "z.txt")
	if string(e.srv.Body(f.UUID)) != "second" {
		t.Errorf("uploaded %q", e.srv.Body(f.UUID))
	}
}

func TestMultipartAndMove(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Options{})
	s, _ := e.m.Create(ctx, "tree/tmp/(A Document Being Saved By X)/big.jpg", index.Object{Ino: NewIno()}, "")
	body := bytes.Repeat([]byte{1, 2, 3, 4, 5, 6, 7}, (11<<20)/7)
	write(t, s, body)
	if err := e.m.Move(ctx, s, "tree/big.jpg", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	f := e.uploaded(t, "big.jpg")
	if f.Metadata["ucfs:path"] != "/big.jpg" || !bytes.Equal(e.srv.Body(f.UUID), body) {
		t.Errorf("uploaded %+v", f)
	}
	if st := e.srv.UploadStats(); st.Start != 1 || st.Parts != 3 || st.Complete != 1 {
		t.Errorf("stats %+v", st)
	}
	if err := e.m.Move(ctx, s, "tree/other.jpg", ""); !errors.Is(err, syscall.ENOENT) {
		t.Errorf("move of an uploaded spool = %v", err)
	}
}

func TestRecover(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Options{})
	a, _ := e.m.Create(ctx, "tree/a.txt", index.Object{Ino: NewIno()}, "")
	write(t, a, []byte("aaa"))
	b, _ := e.m.Create(ctx, "tree/b.txt", index.Object{Ino: NewIno()}, "")
	write(t, b, []byte("bbb"))
	c, _ := e.m.Create(ctx, "tree/c.txt", index.Object{Ino: NewIno()}, "")
	write(t, c, []byte("ccc"))
	// a died mid-upload; b's upload reached the server before the crash; c
	// was still being written.
	for _, s := range []*Spool{a, b} {
		s.mu.Lock()
		s.row.State, s.row.Committed = Uploading, time.Now().Add(-time.Second)
		s.persistLocked()
		s.mu.Unlock()
	}
	if _, err := e.client.Upload(ctx, api.Upload{Name: "b.txt", Size: 3, MIME: "text/plain", Data: strings.NewReader("bbb"),
		Metadata: map[string]string{"ucfs:path": "/b.txt"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ix.Sync(ctx, e.client, false); err != nil {
		t.Fatal(err)
	}
	before := e.srv.UploadStats().Base

	m2 := e.open(t, Options{})
	if len(m2.List()) != 3 {
		t.Fatalf("reopened %d spools", len(m2.List()))
	}
	m2.Recover(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for m2.At("tree/a.txt") != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := e.srv.UploadStats().Base - before; got != 1 {
		t.Errorf("recovery uploaded %d files, want 1 (only a.txt)", got)
	}
	e.uploaded(t, "a.txt")
	e.uploaded(t, "b.txt")
	if m2.At("tree/b.txt") != nil {
		t.Error("b.txt is still spooled though its upload was found")
	}
	if s := m2.At("tree/c.txt"); s == nil || s.Info().State != Writing {
		t.Error("c.txt, cut off while written, should stay spooled and not be uploaded")
	}
}

func TestRecoverAbandonedMultipart(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, Options{})
	body := bytes.Repeat([]byte{9, 8, 7}, (11<<20)/3)
	s, _ := e.m.Create(ctx, "tree/big.jpg", index.Object{Ino: NewIno()}, "")
	write(t, s, body)
	// The daemon died after /multipart/start/: the API lists the upload as a
	// file that is not ready.
	e.srv.RejectParts(true)
	if _, err := e.client.Upload(ctx, api.Upload{Name: "big.jpg", Size: int64(len(body)), MIME: "image/jpeg",
		Data: bytes.NewReader(body), Multipart: true, Metadata: map[string]string{"ucfs:path": "/big.jpg"},
		Started: func(uuid string) { s.startedUpload(uuid) }}); err == nil {
		t.Fatal("upload with rejected parts succeeded")
	}
	e.srv.RejectParts(false)
	abandoned := s.Info().UploadUUID
	if abandoned == "" {
		t.Fatal("multipart upload did not start")
	}
	s.mu.Lock()
	s.row.State, s.row.Committed = Uploading, time.Now()
	s.persistLocked()
	s.mu.Unlock()
	if _, err := e.ix.Sync(ctx, e.client, false); err != nil {
		t.Fatal(err)
	}

	m2 := e.open(t, Options{})
	m2.Recover(ctx)
	deadline := time.Now().Add(10 * time.Second)
	for m2.At("tree/big.jpg") != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	f := e.uploaded(t, "big.jpg")
	if f.UUID == abandoned || f.NotReady || !bytes.Equal(e.srv.Body(f.UUID), body) {
		t.Errorf("recovered upload %+v", f)
	}
	if old, _ := e.srv.File(abandoned); !old.Removed {
		t.Error("the abandoned multipart upload is still listed")
	}
}

func TestQueuedMetadata(t *testing.T) {
	ctx := context.Background()
	old := fake.File{UUID: "00000000-0000-4000-8000-000000000001", Name: "a.txt", Size: 3, Uploaded: time.Now().Add(-time.Hour),
		Metadata: map[string]string{"camera": "Canon", "lens": "50mm"}, Tags: []string{"old"}}
	e := newEnv(t, Options{Metadata: map[string]string{"ucfs:test": "1", "lens": "flag"}}, old)
	s, err := e.m.Create(ctx, "tree/a.txt", index.Object{Ino: NewIno()}, old.UUID)
	if err != nil {
		t.Fatal(err)
	}
	write(t, s, []byte("new"))
	if err := s.Queue(func(q *index.Queued) error {
		q.Meta["lens"] = "35mm"
		q.Meta["ucfs:path"] = "/elsewhere"
		tags := []string{"new"}
		q.Tags = &tags
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// The queue survives a restart with the spool.
	s.mu.Lock()
	s.row.State, s.row.Committed = Queued, time.Now()
	s.persistLocked()
	s.mu.Unlock()
	m2 := e.open(t, Options{Metadata: map[string]string{"ucfs:test": "1", "lens": "flag"}})
	if q := m2.At("tree/a.txt").Queued(); q.Meta["lens"] != "35mm" {
		t.Fatalf("queue after reopening = %+v", q)
	}
	m2.Recover(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for m2.At("tree/a.txt") != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	f := e.uploaded(t, "a.txt")
	if f.Metadata["camera"] != "Canon" || f.Metadata["lens"] != "35mm" || f.Metadata["ucfs:test"] != "1" ||
		f.Metadata["ucfs:path"] != "/a.txt" || strings.Join(f.Tags, ",") != "new" {
		t.Errorf("uploaded with metadata %v, tags %q", f.Metadata, f.Tags)
	}
}
