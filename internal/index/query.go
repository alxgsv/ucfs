package index

import (
	"context"
	"database/sql"
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
}

const fileColumns = `uuid, name, size, coalesce(mime, ''), uploaded_at, removed`

func scanFile(s interface{ Scan(...any) error }) (File, error) {
	var f File
	var uploaded int64
	err := s.Scan(&f.UUID, &f.Name, &f.Size, &f.MIME, &uploaded, &f.Removed)
	f.UploadedAt = time.Unix(uploaded, 0).UTC()
	return f, err
}

// Get returns one file, removed or not.
func (ix *Index) Get(ctx context.Context, uuid string) (File, error) {
	f, err := scanFile(ix.db.QueryRowContext(ctx, `SELECT `+fileColumns+` FROM files WHERE uuid = ?`, uuid))
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}

// Files returns every file that is not removed, ordered by uuid.
func (ix *Index) Files(ctx context.Context) ([]File, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT `+fileColumns+` FROM files WHERE removed = 0 ORDER BY uuid`)
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

// Put upserts files fetched outside a sync, such as a refresh after a CDN 404.
func (ix *Index) Put(ctx context.Context, files ...api.File) error {
	return ix.upsert(ctx, files)
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
