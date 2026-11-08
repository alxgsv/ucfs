package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/alxgsv/ucfs/internal/api"
)

// Details is what extended attributes and `ucfs resolve` show of a file
// beyond File.
type Details struct {
	File
	IsReady bool
	Stored  bool
	// MD5 is the content hash from the CDN's ETag, empty until asked for.
	MD5           string
	Width, Height int
	Metadata      map[string]string
	// Tags is nil when the file info carried no tags field, which is how the
	// API answers for a project without the tags feature.
	Tags []string
	// RefreshedAt is when the file's info last came from the API.
	RefreshedAt time.Time
}

// Details returns one file, removed or not, with its metadata and tags.
func (ix *Index) Details(ctx context.Context, uuid string) (Details, error) {
	var d Details
	var uploaded, ino, refreshed int64
	var meta, tags sql.NullString
	err := ix.db.QueryRowContext(ctx, `SELECT `+fileColumns+`, f.is_ready, f.stored, coalesce(f.md5, ''),
			coalesce(f.width, 0), coalesce(f.height, 0), f.metadata, f.tags, coalesce(f.info_refreshed_at, 0)`+
		fromFiles+`WHERE f.uuid = ?`, uuid).Scan(&d.UUID, &d.Name, &d.Size, &d.MIME, &uploaded, &d.Removed, &d.TreePath,
		&ino, &d.Object.Key, &d.IsReady, &d.Stored, &d.MD5, &d.Width, &d.Height, &meta, &tags, &refreshed)
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	if err != nil {
		return d, err
	}
	d.UploadedAt = time.Unix(uploaded, 0).UTC()
	d.Object.Ino = uint64(ino)
	d.RefreshedAt = time.Unix(refreshed, 0)
	d.Metadata = map[string]string{}
	if meta.Valid && meta.String != "" {
		if err := json.Unmarshal([]byte(meta.String), &d.Metadata); err != nil {
			return d, err
		}
		if d.Metadata == nil {
			d.Metadata = map[string]string{}
		}
	}
	if tags.Valid && tags.String != "" {
		if err := json.Unmarshal([]byte(tags.String), &d.Tags); err != nil {
			return d, err
		}
	}
	return d, nil
}

// PutInfo upserts files fetched at a time outside a listing, such as a
// refresh of one file's info, without moving the sync cursor: a file the
// mount uploaded has a server upload time that may be past files other
// clients uploaded since the last listing.
func (ix *Index) PutInfo(ctx context.Context, at time.Time, files ...api.File) error {
	return ix.upsert(ctx, files, false, at)
}

// metaPath is the JSON path of a metadata key, which may contain dots.
func metaPath(key string) string { return `$."` + key + `"` }

// SetMeta records a metadata value the mount set.
func (ix *Index) SetMeta(ctx context.Context, uuid, key, value string) error {
	_, err := ix.db.ExecContext(ctx, `UPDATE files SET metadata = json_set(coalesce(nullif(metadata, 'null'), '{}'), ?, ?)
		WHERE uuid = ?`, metaPath(key), value, uuid)
	return err
}

// DeleteMeta records a metadata key the mount deleted.
func (ix *Index) DeleteMeta(ctx context.Context, uuid, key string) error {
	_, err := ix.db.ExecContext(ctx, `UPDATE files SET metadata = json_remove(coalesce(nullif(metadata, 'null'), '{}'), ?)
		WHERE uuid = ?`, metaPath(key), uuid)
	return err
}

// SetTags records the tag list the API answered after the mount set it.
func (ix *Index) SetTags(ctx context.Context, uuid string, tags []string) error {
	if tags == nil {
		tags = []string{}
	}
	b, err := json.Marshal(tags)
	if err != nil {
		return err
	}
	_, err = ix.db.ExecContext(ctx, `UPDATE files SET tags = ? WHERE uuid = ?`, string(b), uuid)
	return err
}

// SetMD5 records a file's content hash, which never changes.
func (ix *Index) SetMD5(ctx context.Context, uuid, md5 string) error {
	_, err := ix.db.ExecContext(ctx, `UPDATE files SET md5 = ? WHERE uuid = ?`, md5, uuid)
	return err
}

// tagRows joins each present file with its tags; the tags column holds
// JSON null for files whose info carried no tags.
const tagRows = ` FROM files f JOIN json_each(f.tags) t LEFT JOIN objects o ON o.uuid = f.uuid
	WHERE f.removed = 0 AND f.tags IS NOT NULL AND t.type = 'text' `

// Tags returns every tag a present file carries, sorted.
func (ix *Index) Tags(ctx context.Context) ([]string, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT DISTINCT t.value`+tagRows+`ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TaggedFiles returns the present files that carry a tag, ordered by uuid.
func (ix *Index) TaggedFiles(ctx context.Context, tag string) ([]File, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT `+fileColumns+tagRows+`AND t.value = ? ORDER BY f.uuid`, tag)
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

// HasTag reports whether a present file carries a tag.
func (ix *Index) HasTag(ctx context.Context, tag string) (bool, error) {
	var n int
	err := ix.db.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1`+tagRows+`AND t.value = ? LIMIT 1)`, tag).Scan(&n)
	return n > 0, err
}

// UploadedSpan returns the first and last upload times of present files;
// ok is false when there are none.
func (ix *Index) UploadedSpan(ctx context.Context) (first, last time.Time, ok bool, err error) {
	var lo, hi sql.NullInt64
	err = ix.db.QueryRowContext(ctx, `SELECT min(uploaded_at), max(uploaded_at) FROM files WHERE removed = 0`).Scan(&lo, &hi)
	if err != nil || !lo.Valid {
		return first, last, false, err
	}
	return time.Unix(lo.Int64, 0), time.Unix(hi.Int64, 0), true, nil
}

// UploadedBetween reports whether a present file was uploaded in [from, to).
func (ix *Index) UploadedBetween(ctx context.Context, from, to time.Time) (bool, error) {
	var n int
	err := ix.db.QueryRowContext(ctx, `SELECT count(*) FROM (SELECT 1 FROM files
		WHERE uploaded_at >= ? AND uploaded_at < ? AND removed = 0 LIMIT 1)`, from.Unix(), to.Unix()).Scan(&n)
	return n > 0, err
}

// FilesBetween returns the present files uploaded in [from, to), ordered by
// uuid.
func (ix *Index) FilesBetween(ctx context.Context, from, to time.Time) ([]File, error) {
	return ix.files(ctx, `WHERE f.uploaded_at >= ? AND f.uploaded_at < ? AND f.removed = 0 ORDER BY f.uuid`,
		from.Unix(), to.Unix())
}
