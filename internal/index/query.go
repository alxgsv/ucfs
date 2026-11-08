package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/alxgsv/ucfs/internal/api"
)

var ErrNotFound = errors.New("file not in index")

// File is what the filesystem needs to know about one indexed file.
type File struct {
	UUID       string
	Name       string
	Size       int64
	MIME       string
	UploadedAt time.Time
	Removed    bool
	// TreePath is the raw ucfs:path value, valid or not.
	TreePath string
	// Object is the identity the mount gave the file when it wrote it,
	// zero for files it never wrote (see Object).
	Object Object
}

const fileColumns = `f.uuid, f.name, f.size, coalesce(f.mime, ''), f.uploaded_at, f.removed, coalesce(f.tree_path, ''),
	coalesce(o.ino, 0), coalesce(o.key, '')`

// fromFiles joins the object a file was written as.
const fromFiles = ` FROM files f LEFT JOIN objects o ON o.uuid = f.uuid `

func scanFile(s interface{ Scan(...any) error }) (File, error) {
	var f File
	var uploaded, ino int64
	err := s.Scan(&f.UUID, &f.Name, &f.Size, &f.MIME, &uploaded, &f.Removed, &f.TreePath, &ino, &f.Object.Key)
	f.UploadedAt = time.Unix(uploaded, 0).UTC()
	f.Object.Ino = uint64(ino)
	return f, err
}

func (ix *Index) files(ctx context.Context, where string, args ...any) ([]File, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT `+fileColumns+fromFiles+where, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// Get returns one file, removed or not.
func (ix *Index) Get(ctx context.Context, uuid string) (File, error) {
	f, err := scanFile(ix.db.QueryRowContext(ctx, `SELECT `+fileColumns+fromFiles+`WHERE f.uuid = ?`, uuid))
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}

// Files returns every file that is not removed, ordered by uuid.
func (ix *Index) Files(ctx context.Context) ([]File, error) {
	return ix.files(ctx, `WHERE f.removed = 0 ORDER BY f.uuid`)
}

// Put upserts files fetched outside a sync, such as a refresh after a CDN 404.
func (ix *Index) Put(ctx context.Context, files ...api.File) error {
	return ix.upsert(ctx, files, true, time.Now())
}

// PutUploaded records a file the mount just uploaded, without moving the
// sync cursor; the next sync lists it and replaces the row with the server's
// view.
func (ix *Index) PutUploaded(ctx context.Context, f api.File) error {
	return ix.upsert(ctx, []api.File{f}, false, time.Now())
}

// MarkRemoved records a soft delete the mount made.
func (ix *Index) MarkRemoved(ctx context.Context, uuid string) error {
	_, err := ix.db.ExecContext(ctx, `UPDATE files SET removed = 1, removed_at = ? WHERE uuid = ?`, time.Now().Unix(), uuid)
	return err
}

// Meta returns a file's metadata and tags as last listed.
func (ix *Index) Meta(ctx context.Context, uuid string) (map[string]string, []string, error) {
	var meta, tags sql.NullString
	err := ix.db.QueryRowContext(ctx, `SELECT metadata, tags FROM files WHERE uuid = ?`, uuid).Scan(&meta, &tags)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	m := map[string]string{}
	var tl []string
	if meta.Valid && meta.String != "" && meta.String != "null" {
		if err := json.Unmarshal([]byte(meta.String), &m); err != nil {
			return nil, nil, err
		}
	}
	if tags.Valid && tags.String != "" && tags.String != "null" {
		if err := json.Unmarshal([]byte(tags.String), &tl); err != nil {
			return nil, nil, err
		}
	}
	return m, tl, nil
}

// SetTreePath records a ucfs:path the mount set or, with p empty, deleted.
func (ix *Index) SetTreePath(ctx context.Context, uuid, p string) error {
	dir, key := treeKeys(p)
	var err error
	if p == "" {
		_, err = ix.db.ExecContext(ctx, `UPDATE files SET tree_path = NULL, tree_dir = NULL, tree_key = NULL,
			metadata = json_remove(coalesce(metadata, '{}'), '$."`+TreePathKey+`"') WHERE uuid = ?`, uuid)
	} else {
		_, err = ix.db.ExecContext(ctx, `UPDATE files SET tree_path = ?, tree_dir = ?, tree_key = ?,
			metadata = json_set(coalesce(nullif(metadata, 'null'), '{}'), '$."`+TreePathKey+`"', ?) WHERE uuid = ?`,
			p, dir, key, p, uuid)
	}
	return err
}

// SweepRemoved lists removed files and marks the indexed ones removed.
// GET /files/ has no "removed since" cursor, so the sweep starts at the
// oldest file still indexed as present; older removals cannot matter.
func (ix *Index) SweepRemoved(ctx context.Context, l Lister) (int, error) {
	var oldest sql.NullInt64
	if err := ix.db.QueryRowContext(ctx, `SELECT min(uploaded_at) FROM files WHERE removed = 0`).Scan(&oldest); err != nil {
		return 0, err
	}
	if !oldest.Valid {
		return 0, nil
	}
	marked := 0
	err := l.ListFiles(ctx, api.ListOptions{From: time.Unix(oldest.Int64, 0), Removed: true}, func(f api.File) error {
		removedAt := f.RemovedAt
		if removedAt.IsZero() {
			removedAt = time.Now()
		}
		res, err := ix.db.ExecContext(ctx, `UPDATE files SET removed = 1, removed_at = ? WHERE uuid = ? AND removed = 0`,
			removedAt.Unix(), f.UUID)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		marked += int(n)
		return err
	})
	return marked, err
}

// FindUploaded returns present files with this original name and size
// uploaded at or after since, newest first: how a restarted mount tells
// whether an upload cut off by a crash reached the server after all.
func (ix *Index) FindUploaded(ctx context.Context, name string, size int64, since time.Time) ([]File, error) {
	return ix.files(ctx, `WHERE f.name_key = ? AND f.size = ? AND f.uploaded_at >= ? AND f.removed = 0
		ORDER BY f.uploaded_at DESC, f.uuid`, NameKey(name), size, since.Unix())
}
