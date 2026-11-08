package index

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	// TreePathKey is the metadata key that places a file in tree/.
	TreePathKey = "ucfs:path"
	// NameMetaKey holds a file's name when the uploaded filename could not:
	// a base upload drops everything up to the last backslash.
	NameMetaKey = "ucfs:name"
	// MaxTreePath is the metadata value limit, counted in characters.
	MaxTreePath = 512
)

// ParseTreePath splits a ucfs:path value into its directory and name:
// "/photos/a.jpg" is ("/photos", "a.jpg") and "/a.jpg" is ("/", "a.jpg").
// A value that is not an absolute, clean path of at most MaxTreePath
// characters is not a tree path, and its file stays in tree/_inbox.
func ParseTreePath(v string) (dir, name string, ok bool) {
	if len(v) < 2 || v[0] != '/' || utf8.RuneCountInString(v) > MaxTreePath || strings.ContainsRune(v, 0) {
		return "", "", false
	}
	parts := strings.Split(v[1:], "/")
	for _, p := range parts {
		if p == "" || p == "." || p == ".." {
			return "", "", false
		}
	}
	i := strings.LastIndexByte(v, '/')
	dir = v[:i]
	if dir == "" {
		dir = "/"
	}
	return dir, v[i+1:], true
}

// treeKeys are the indexed forms of a tree path: its directory and name
// under NFC, which macOS sends in lookups whatever form a name was stored in.
func treeKeys(v string) (dir, key any) {
	d, n, ok := ParseTreePath(v)
	if !ok {
		return nil, nil
	}
	return norm.NFC.String(d), norm.NFC.String(n)
}

// NameKey is the indexed form of an original filename.
func NameKey(name string) string { return norm.NFC.String(name) }

// backfillKeys fills the keys added in schema version 3 for rows listed
// before it.
func (ix *Index) backfillKeys() error {
	rows, err := ix.db.Query(`SELECT uuid, name, coalesce(tree_path, '') FROM files WHERE name_key IS NULL`)
	if err != nil {
		return err
	}
	type row struct{ uuid, name, tree string }
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.uuid, &r.name, &r.tree); err != nil {
			_ = rows.Close()
			return err
		}
		todo = append(todo, r)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(todo) == 0 {
		return nil
	}
	tx, err := ix.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range todo {
		dir, key := treeKeys(r.tree)
		if _, err := tx.Exec(`UPDATE files SET tree_dir = ?, tree_key = ?, name_key = ? WHERE uuid = ?`,
			dir, key, NameKey(r.name), r.uuid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// below returns the key range of the paths strictly inside dir, which is
// "/" or a path without a trailing slash: '0' is the byte after '/'.
func below(dir string) (lo, hi string) {
	if dir == "/" {
		return "/", "0"
	}
	return dir + "/", dir + "0"
}

// TreeFiles returns the present files whose ucfs:path is directly in dir,
// grouped by name with the newest upload first.
func (ix *Index) TreeFiles(ctx context.Context, dir string) ([]File, error) {
	return ix.files(ctx, `WHERE f.tree_dir = ? AND f.removed = 0 ORDER BY f.tree_key, f.uploaded_at DESC, f.uuid`,
		norm.NFC.String(dir))
}

// TreeLookup returns the present files named name directly in dir, newest
// upload first.
func (ix *Index) TreeLookup(ctx context.Context, dir, name string) ([]File, error) {
	return ix.files(ctx, `WHERE f.tree_dir = ? AND f.tree_key = ? AND f.removed = 0 ORDER BY f.uploaded_at DESC, f.uuid`,
		norm.NFC.String(dir), norm.NFC.String(name))
}

// TreeBelow returns the present files anywhere below dir.
func (ix *Index) TreeBelow(ctx context.Context, dir string) ([]File, error) {
	dir = norm.NFC.String(dir)
	lo, hi := below(dir)
	return ix.files(ctx, `WHERE (f.tree_dir = ? OR (f.tree_dir >= ? AND f.tree_dir < ?)) AND f.removed = 0 ORDER BY f.tree_dir`,
		dir, lo, hi)
}

// TreeSubdirs returns the names of the directories directly in dir: those
// that hold a present file somewhere below them, and those made by mkdir.
func (ix *Index) TreeSubdirs(ctx context.Context, dir string) ([]string, error) {
	dir = norm.NFC.String(dir)
	lo, hi := below(dir)
	rows, err := ix.db.QueryContext(ctx, `
		SELECT DISTINCT tree_dir FROM files WHERE tree_dir >= ? AND tree_dir < ? AND tree_dir != '/' AND removed = 0
		UNION SELECT path FROM tree_dirs WHERE path >= ? AND path < ?`, lo, hi, lo, hi)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	seen := map[string]bool{}
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		name, _, _ := strings.Cut(p[len(lo):], "/")
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out, rows.Err()
}

// TreeDirExists reports whether dir was made by mkdir or holds a present
// file somewhere below it. The root always exists.
func (ix *Index) TreeDirExists(ctx context.Context, dir string) (bool, error) {
	if dir == "/" {
		return true, nil
	}
	dir = norm.NFC.String(dir)
	lo, hi := below(dir)
	var one int
	err := ix.db.QueryRowContext(ctx, `
		SELECT 1 FROM tree_dirs WHERE path = ?
		UNION ALL SELECT 1 FROM files WHERE (tree_dir = ? OR (tree_dir >= ? AND tree_dir < ?)) AND removed = 0
		LIMIT 1`, dir, dir, lo, hi).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// PutTreeDir records a directory, and its parents, as made by mkdir, so it
// exists while empty.
func (ix *Index) PutTreeDir(ctx context.Context, dir string) error {
	dir = norm.NFC.String(dir)
	now := time.Now().Unix()
	for d := dir; d != "/" && d != ""; d = parentDir(d) {
		if _, err := ix.db.ExecContext(ctx, `INSERT OR IGNORE INTO tree_dirs (path, created_at) VALUES (?, ?)`, d, now); err != nil {
			return err
		}
	}
	return nil
}

// DeleteTreeDir forgets a directory made by mkdir.
func (ix *Index) DeleteTreeDir(ctx context.Context, dir string) error {
	_, err := ix.db.ExecContext(ctx, `DELETE FROM tree_dirs WHERE path = ?`, norm.NFC.String(dir))
	return err
}

// MoveTreeDirs renames a directory made by mkdir and those below it.
func (ix *Index) MoveTreeDirs(ctx context.Context, from, to string) error {
	from, to = norm.NFC.String(from), norm.NFC.String(to)
	lo, hi := below(from)
	rows, err := ix.db.QueryContext(ctx, `SELECT path FROM tree_dirs WHERE path = ? OR (path >= ? AND path < ?)`, from, lo, hi)
	if err != nil {
		return err
	}
	var paths []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			_ = rows.Close()
			return err
		}
		paths = append(paths, p)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, p := range paths {
		if _, err := ix.db.ExecContext(ctx, `UPDATE OR REPLACE tree_dirs SET path = ? WHERE path = ?`, to+p[len(from):], p); err != nil {
			return err
		}
	}
	return ix.PutTreeDir(ctx, to)
}

func parentDir(d string) string {
	i := strings.LastIndexByte(d, '/')
	if i <= 0 {
		return "/"
	}
	return d[:i]
}

// InboxFiles returns the present files without a tree path, grouped by
// name with the newest upload first.
func (ix *Index) InboxFiles(ctx context.Context) ([]File, error) {
	return ix.files(ctx, `WHERE f.tree_dir IS NULL AND f.removed = 0 ORDER BY f.name_key, f.uploaded_at DESC, f.uuid`)
}

// InboxLookup returns the present files without a tree path whose original
// name is one of names, newest upload first.
func (ix *Index) InboxLookup(ctx context.Context, names ...string) ([]File, error) {
	if len(names) == 0 {
		return nil, nil
	}
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = NameKey(n)
	}
	return ix.files(ctx, `WHERE f.tree_dir IS NULL AND f.removed = 0 AND f.name_key IN (?`+strings.Repeat(", ?", len(names)-1)+
		`) ORDER BY f.uploaded_at DESC, f.uuid`, args...)
}
