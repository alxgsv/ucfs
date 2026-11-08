package index

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Shadow is a local-only file or directory: Finder and editor bookkeeping
// that is kept in the cache dir and never uploaded. Path is relative to the
// mount root; Blob names the file holding a shadow file's bytes.
type Shadow struct {
	Path string
	Ino  uint64
	Dir  bool
	Blob string
}

const shadowColumns = `path, ino, dir, coalesce(blob_ref, '')`

func scanShadow(s interface{ Scan(...any) error }) (Shadow, error) {
	var sh Shadow
	var ino int64
	err := s.Scan(&sh.Path, &ino, &sh.Dir, &sh.Blob)
	sh.Ino = uint64(ino)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return sh, err
}

func (ix *Index) Shadow(ctx context.Context, p string) (Shadow, error) {
	return scanShadow(ix.db.QueryRowContext(ctx, `SELECT `+shadowColumns+` FROM shadow WHERE path = ?`, norm.NFC.String(p)))
}

func (ix *Index) ShadowByIno(ctx context.Context, ino uint64) (Shadow, error) {
	return scanShadow(ix.db.QueryRowContext(ctx, `SELECT `+shadowColumns+` FROM shadow WHERE ino = ?`, int64(ino)))
}

func (ix *Index) PutShadow(ctx context.Context, sh Shadow) error {
	_, err := ix.db.ExecContext(ctx, `INSERT INTO shadow (path, ino, dir, blob_ref) VALUES (?, ?, ?, ?)
		ON CONFLICT (path) DO UPDATE SET ino = excluded.ino, dir = excluded.dir, blob_ref = excluded.blob_ref`,
		norm.NFC.String(sh.Path), int64(sh.Ino), sh.Dir, sh.Blob)
	return err
}

func (ix *Index) DeleteShadow(ctx context.Context, p string) error {
	_, err := ix.db.ExecContext(ctx, `DELETE FROM shadow WHERE path = ?`, norm.NFC.String(p))
	return err
}

// ShadowsBelow returns the shadow entries anywhere below dir, a path
// relative to the mount root ("" for the root itself).
func (ix *Index) ShadowsBelow(ctx context.Context, dir string) ([]Shadow, error) {
	lo, hi := "", "\xff"
	if dir != "" {
		dir = norm.NFC.String(dir)
		lo, hi = dir+"/", dir+"0"
	}
	rows, err := ix.db.QueryContext(ctx, `SELECT `+shadowColumns+` FROM shadow WHERE path >= ? AND path < ? ORDER BY path`, lo, hi)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Shadow
	for rows.Next() {
		sh, err := scanShadow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sh)
	}
	return out, rows.Err()
}

// ShadowChildren returns the shadow entries directly in dir.
func (ix *Index) ShadowChildren(ctx context.Context, dir string) ([]Shadow, error) {
	all, err := ix.ShadowsBelow(ctx, dir)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if dir != "" {
		prefix = norm.NFC.String(dir) + "/"
	}
	out := all[:0]
	for _, sh := range all {
		if !strings.Contains(sh.Path[len(prefix):], "/") {
			out = append(out, sh)
		}
	}
	return out, nil
}

// MoveShadow renames a shadow entry and, for a directory, everything below it.
func (ix *Index) MoveShadow(ctx context.Context, from, to string) error {
	from, to = norm.NFC.String(from), norm.NFC.String(to)
	below, err := ix.ShadowsBelow(ctx, from)
	if err != nil {
		return err
	}
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM shadow WHERE path = ?`, to); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE shadow SET path = ? WHERE path = ?`, to, from); err != nil {
		return err
	}
	for _, sh := range below {
		if _, err := tx.ExecContext(ctx, `UPDATE OR REPLACE shadow SET path = ? WHERE path = ?`, to+sh.Path[len(from):], sh.Path); err != nil {
			return err
		}
	}
	return tx.Commit()
}
