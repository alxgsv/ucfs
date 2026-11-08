package index

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"golang.org/x/text/unicode/norm"
)

// Object is the identity of a file the mount wrote: the inode number that
// clients keep across its upload, a later overwrite and renames. Key is
// the uuid whose hash gave the number when the mount first wrote over a
// file it had not written, empty when the number was drawn at random; NFS
// file handles carry it, so a handle handed out before the object had a row
// stays the same bytes afterwards.
type Object struct {
	Ino uint64
	Key string
}

// ObjectByIno returns the file an object holds now.
func (ix *Index) ObjectByIno(ctx context.Context, ino uint64) (uuid string, o Object, err error) {
	o.Ino = ino
	err = ix.db.QueryRowContext(ctx, `SELECT uuid, key FROM objects WHERE ino = ?`, int64(ino)).Scan(&uuid, &o.Key)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return uuid, o, err
}

// PutObject makes an object hold uuid, replacing what it held before.
func (ix *Index) PutObject(ctx context.Context, o Object, uuid string) error {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM objects WHERE uuid = ? AND ino != ?`, uuid, int64(o.Ino)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO objects (ino, uuid, key) VALUES (?, ?, ?)
		ON CONFLICT (ino) DO UPDATE SET uuid = excluded.uuid, key = excluded.key`, int64(o.Ino), uuid, o.Key); err != nil {
		return err
	}
	return tx.Commit()
}

// Alias is a name written in a view that stands for an uploaded file until
// it expires.
type Alias struct {
	View, Name, UUID string
	Expires          time.Time
}

func (ix *Index) PutAlias(ctx context.Context, a Alias) error {
	_, err := ix.db.ExecContext(ctx, `INSERT INTO aliases (view, name, uuid, expires_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (view, name) DO UPDATE SET uuid = excluded.uuid, expires_at = excluded.expires_at`,
		a.View, norm.NFC.String(a.Name), a.UUID, a.Expires.Unix())
	return err
}

// Alias returns the file a name stands for, if it has not expired by now.
func (ix *Index) Alias(ctx context.Context, view, name string, now time.Time) (string, error) {
	var uuid string
	err := ix.db.QueryRowContext(ctx, `SELECT uuid FROM aliases WHERE view = ? AND name = ? AND expires_at > ?`,
		view, norm.NFC.String(name), now.Unix()).Scan(&uuid)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return uuid, err
}

// AliasOf returns the newest live alias of a file.
func (ix *Index) AliasOf(ctx context.Context, view, uuid string, now time.Time) (string, error) {
	var name string
	err := ix.db.QueryRowContext(ctx, `SELECT name FROM aliases WHERE view = ? AND uuid = ? AND expires_at > ?
		ORDER BY expires_at DESC LIMIT 1`, view, uuid, now.Unix()).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return name, err
}

// RetargetAliases points the aliases of a replaced file at its replacement.
func (ix *Index) RetargetAliases(ctx context.Context, from, to string) error {
	_, err := ix.db.ExecContext(ctx, `UPDATE aliases SET uuid = ? WHERE uuid = ?`, to, from)
	return err
}

func (ix *Index) DeleteAlias(ctx context.Context, view, name string) error {
	_, err := ix.db.ExecContext(ctx, `DELETE FROM aliases WHERE view = ? AND name = ?`, view, norm.NFC.String(name))
	return err
}

// PruneAliases drops aliases that expired before now.
func (ix *Index) PruneAliases(ctx context.Context, now time.Time) error {
	_, err := ix.db.ExecContext(ctx, `DELETE FROM aliases WHERE expires_at <= ?`, now.Unix())
	return err
}
