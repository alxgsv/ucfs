package index

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Variant is what a CDN HEAD said about one variant of a file.
type Variant struct {
	UUID string
	Ops  string
	Size int64
	MIME string
	ETag string
	// CheckedAt is the last HEAD. ChangedAt is when the CDN first answered
	// with this size and ETag.
	CheckedAt time.Time
	ChangedAt time.Time
}

func (ix *Index) Variant(ctx context.Context, uuid, ops string) (Variant, error) {
	v := Variant{UUID: uuid, Ops: ops}
	var checked, changed int64
	err := ix.db.QueryRowContext(ctx, `SELECT size, coalesce(mime, ''), coalesce(etag, ''), checked_at,
		coalesce(changed_at, checked_at) FROM variants WHERE uuid = ? AND ops = ?`, uuid, ops).
		Scan(&v.Size, &v.MIME, &v.ETag, &checked, &changed)
	if errors.Is(err, sql.ErrNoRows) {
		return v, ErrNotFound
	}
	v.CheckedAt, v.ChangedAt = time.Unix(checked, 0), time.Unix(changed, 0)
	return v, err
}

func (ix *Index) PutVariant(ctx context.Context, v Variant) error {
	_, err := ix.db.ExecContext(ctx, `INSERT INTO variants (uuid, ops, size, mime, etag, checked_at, changed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (uuid, ops) DO UPDATE SET size = excluded.size, mime = excluded.mime, etag = excluded.etag,
			checked_at = excluded.checked_at, changed_at = excluded.changed_at`,
		v.UUID, v.Ops, v.Size, v.MIME, v.ETag, v.CheckedAt.Unix(), v.ChangedAt.Unix())
	return err
}

// PutNodeOps records the operations path behind a node id, so a file handle
// that carries only the id resolves after a restart.
func (ix *Index) PutNodeOps(ctx context.Context, id uint64, uuid, ops string) error {
	_, err := ix.db.ExecContext(ctx, `INSERT OR IGNORE INTO node_ops (id, uuid, ops) VALUES (?, ?, ?)`,
		int64(id), uuid, ops)
	return err
}

func (ix *Index) NodeOps(ctx context.Context, id uint64) (uuid, ops string, err error) {
	err = ix.db.QueryRowContext(ctx, `SELECT uuid, ops FROM node_ops WHERE id = ?`, int64(id)).Scan(&uuid, &ops)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return uuid, ops, err
}

// MoveNodePaths maps the ids recorded for a directory known by path, and for
// those below it, to the directory's new path. Only rows without a uuid,
// which tree and shadow directories leave, are paths.
func (ix *Index) MoveNodePaths(ctx context.Context, from, to string) error {
	lo, hi := from+"/", from+"0"
	rows, err := ix.db.QueryContext(ctx, `SELECT id, ops FROM node_ops WHERE uuid = '' AND (ops = ? OR (ops >= ? AND ops < ?))`,
		from, lo, hi)
	if err != nil {
		return err
	}
	type row struct {
		id  int64
		ops string
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.ops); err != nil {
			_ = rows.Close()
			return err
		}
		todo = append(todo, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, r := range todo {
		if _, err := ix.db.ExecContext(ctx, `UPDATE node_ops SET ops = ? WHERE id = ?`, to+r.ops[len(from):], r.id); err != nil {
			return err
		}
	}
	return nil
}
