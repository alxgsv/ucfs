// Package index is the local SQLite copy of a project's file list.
package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/alxgsv/ucfs/internal/api"
)

// migrations are applied in order; PRAGMA user_version records how many ran.
var migrations = []string{`
CREATE TABLE files (
	uuid TEXT PRIMARY KEY, name TEXT NOT NULL, size INTEGER NOT NULL, mime TEXT,
	is_image INTEGER NOT NULL, is_ready INTEGER NOT NULL, stored INTEGER NOT NULL, removed INTEGER NOT NULL,
	uploaded_at INTEGER NOT NULL, stored_at INTEGER, removed_at INTEGER,
	md5 TEXT, width INTEGER, height INTEGER, metadata TEXT, tags TEXT,
	tree_path TEXT, info_refreshed_at INTEGER
);
CREATE INDEX files_uploaded_at ON files (uploaded_at);
CREATE INDEX files_name ON files (name);
CREATE INDEX files_tree_path ON files (tree_path);
CREATE INDEX files_removed ON files (removed);
CREATE TABLE variants (
	uuid TEXT NOT NULL, ops TEXT NOT NULL, size INTEGER, mime TEXT, etag TEXT, checked_at INTEGER,
	PRIMARY KEY (uuid, ops)
);
CREATE TABLE groups (uuid TEXT PRIMARY KEY, created_at INTEGER, files TEXT);
CREATE TABLE aliases (
	view TEXT NOT NULL, name TEXT NOT NULL, uuid TEXT NOT NULL, expires_at INTEGER,
	PRIMARY KEY (view, name)
);
CREATE TABLE tree_dirs (path TEXT PRIMARY KEY, created_at INTEGER);
CREATE TABLE shadow (path TEXT PRIMARY KEY, blob_ref TEXT, size INTEGER, mtime INTEGER);
CREATE TABLE sync_state (key TEXT PRIMARY KEY, value TEXT);
`, `
ALTER TABLE variants ADD COLUMN changed_at INTEGER;
CREATE TABLE node_ops (id INTEGER PRIMARY KEY, uuid TEXT NOT NULL, ops TEXT NOT NULL);
`, `
ALTER TABLE files ADD COLUMN tree_dir TEXT;
ALTER TABLE files ADD COLUMN tree_key TEXT;
ALTER TABLE files ADD COLUMN name_key TEXT;
CREATE INDEX files_tree ON files (tree_dir, tree_key);
CREATE INDEX files_name_key ON files (name_key);
CREATE TABLE objects (ino INTEGER PRIMARY KEY, uuid TEXT NOT NULL UNIQUE, key TEXT NOT NULL DEFAULT '');
CREATE TABLE spools (
	id TEXT PRIMARY KEY, ino INTEGER NOT NULL, key TEXT NOT NULL DEFAULT '', path TEXT NOT NULL,
	state TEXT NOT NULL, replaces TEXT NOT NULL DEFAULT '', upload_uuid TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL, committed_at INTEGER NOT NULL DEFAULT 0,
	attempts INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX aliases_uuid ON aliases (uuid);
ALTER TABLE shadow ADD COLUMN ino INTEGER;
ALTER TABLE shadow ADD COLUMN dir INTEGER NOT NULL DEFAULT 0;
CREATE UNIQUE INDEX shadow_ino ON shadow (ino);
`, `
ALTER TABLE spools ADD COLUMN queued TEXT NOT NULL DEFAULT '';
`, `
CREATE TABLE node_salts (key TEXT PRIMARY KEY, salt INTEGER NOT NULL);
`}

const (
	keyListCursor   = "list_cursor"
	keyLastFullScan = "last_full_scan"
	batchSize       = 500
)

type Index struct {
	db *sql.DB
}

// Open opens or creates the index at path and applies pending migrations.
func Open(path string) (*Index, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	ix := &Index{db: db}
	if err := ix.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	// SQLite creates the files under the umask, new -wal and -shm files
	// taking the database file's mode. The index lists every file name and
	// its metadata.
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Chmod(p, 0o600); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = db.Close()
			return nil, err
		}
	}
	if err := ix.backfillKeys(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("backfill %s: %w", path, err)
	}
	return ix, nil
}

func (ix *Index) Close() error { return ix.db.Close() }

func (ix *Index) migrate() error {
	var version int
	if err := ix.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	for i := version; i < len(migrations); i++ {
		tx, err := ix.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			_ = tx.Rollback()
			return err
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// Lister is the part of api.Client that sync needs.
type Lister interface {
	ListFiles(ctx context.Context, o api.ListOptions, fn func(api.File) error) error
}

type SyncResult struct {
	Full     bool
	From     time.Time
	Listed   int
	Added    int
	Duration time.Duration
}

// Sync lists files uploaded since the stored cursor (or all files when full
// is set or no cursor exists) and upserts them. The cursor advances with
// every committed batch, so an interrupted sync resumes where it stopped.
func (ix *Index) Sync(ctx context.Context, l Lister, full bool) (SyncResult, error) {
	start := time.Now()
	res := SyncResult{Full: full}
	if !full {
		cursor, err := ix.Cursor(ctx)
		if err != nil {
			return res, err
		}
		res.From = cursor
		res.Full = cursor.IsZero()
	}
	before, err := ix.count(ctx)
	if err != nil {
		return res, err
	}

	batch := make([]api.File, 0, batchSize)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := ix.upsert(ctx, batch, true, time.Now())
		res.Listed += len(batch)
		batch = batch[:0]
		return err
	}
	err = l.ListFiles(ctx, api.ListOptions{From: res.From}, func(f api.File) error {
		batch = append(batch, f)
		if len(batch) == batchSize {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err == nil && res.Full {
		err = ix.setState(ctx, ix.db, keyLastFullScan, time.Now().UTC().Format(time.RFC3339))
	}
	after, cerr := ix.count(ctx)
	res.Added = after - before
	res.Duration = time.Since(start)
	return res, errors.Join(err, cerr)
}

// upsert stores files fetched at a time and advances the sync cursor past
// them. advance is false for files the mount uploaded itself: their upload
// time comes from the local clock, and a cursor moved by it could skip
// files other clients uploaded in between.
func (ix *Index) upsert(ctx context.Context, files []api.File, advance bool, at time.Time) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO files (uuid, name, size, mime, is_image, is_ready, stored, removed,
			uploaded_at, stored_at, removed_at, width, height, metadata, tags, tree_path, info_refreshed_at,
			tree_dir, tree_key, name_key)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (uuid) DO UPDATE SET
			name = excluded.name, size = excluded.size, mime = excluded.mime,
			is_image = excluded.is_image, is_ready = excluded.is_ready,
			stored = excluded.stored, removed = excluded.removed,
			uploaded_at = excluded.uploaded_at, stored_at = excluded.stored_at, removed_at = excluded.removed_at,
			width = excluded.width, height = excluded.height,
			metadata = excluded.metadata, tags = excluded.tags, tree_path = excluded.tree_path,
			info_refreshed_at = excluded.info_refreshed_at,
			tree_dir = excluded.tree_dir, tree_key = excluded.tree_key, name_key = excluded.name_key`)
	if err != nil {
		return err
	}
	now := at.Unix()
	var cursor time.Time
	for _, f := range files {
		meta, err := json.Marshal(f.Metadata)
		if err != nil {
			return err
		}
		tags, err := json.Marshal(f.Tags)
		if err != nil {
			return err
		}
		var treePath any
		if p, ok := f.Metadata[TreePathKey]; ok {
			treePath = p
		}
		dir, key := treeKeys(f.Metadata[TreePathKey])
		name := f.Name
		if n := f.Metadata[NameMetaKey]; n != "" {
			name = n
		}
		if _, err := stmt.ExecContext(ctx, f.UUID, name, f.Size, f.MIME, f.IsImage, f.IsReady,
			!f.StoredAt.IsZero(), !f.RemovedAt.IsZero(),
			f.UploadedAt.Unix(), unixOrNil(f.StoredAt), unixOrNil(f.RemovedAt),
			f.Width, f.Height, string(meta), string(tags), treePath, now,
			dir, key, NameKey(name)); err != nil {
			return err
		}
		if f.UploadedAt.After(cursor) {
			cursor = f.UploadedAt
		}
	}
	if advance {
		if err := ix.advanceCursor(ctx, tx, cursor); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (ix *Index) setState(ctx context.Context, e execer, key, value string) error {
	_, err := e.ExecContext(ctx, `INSERT INTO sync_state (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// advanceCursor never moves the cursor backwards, so a full rescan of old
// files leaves incremental sync where it was.
func (ix *Index) advanceCursor(ctx context.Context, tx *sql.Tx, t time.Time) error {
	if t.IsZero() {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO sync_state (key, value) VALUES (?, ?)
		ON CONFLICT (key) DO UPDATE SET value = max(value, excluded.value)`,
		keyListCursor, t.UTC().Format(time.RFC3339))
	return err
}

// Synced reports whether a full listing went through once, so the index
// holds the project, even an empty one.
func (ix *Index) Synced(ctx context.Context) (bool, error) {
	var v string
	err := ix.db.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key = ?`, keyLastFullScan).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// Cursor is the newest upload time seen, zero before the first sync.
func (ix *Index) Cursor(ctx context.Context) (time.Time, error) {
	var v string
	err := ix.db.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key = ?`, keyListCursor).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339, v)
}

type Stats struct {
	Files  int
	Stored int
	Bytes  int64
}

// Stats summarises the files that are not removed.
func (ix *Index) Stats(ctx context.Context) (Stats, error) {
	var s Stats
	err := ix.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(stored), 0), coalesce(sum(size), 0)
		FROM files WHERE removed = 0`).Scan(&s.Files, &s.Stored, &s.Bytes)
	return s, err
}

func (ix *Index) count(ctx context.Context) (int, error) {
	var n int
	err := ix.db.QueryRowContext(ctx, `SELECT count(*) FROM files`).Scan(&n)
	return n, err
}

func unixOrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}
