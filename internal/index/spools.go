package index

import (
	"context"
	"encoding/json"
	"time"
)

// SpoolRow is the durable part of a write that has not been uploaded yet.
type SpoolRow struct {
	ID     string
	Object Object
	// Path is the written path relative to the mount root.
	Path     string
	State    string
	Replaces string
	// UploadUUID is the uuid a multipart upload got at its start, so an
	// upload cut off by a crash can be found and dropped.
	UploadUUID string
	Created    time.Time
	Committed  time.Time
	Attempts   int
	Error      string
	// Queued is metadata set on the file before its upload.
	Queued Queued
}

// Queued is metadata set through extended attributes on a file that is not
// uploaded yet; the upload form carries it.
type Queued struct {
	Meta map[string]string `json:"meta,omitempty"`
	// Tags replaces the tag list when set; an empty list clears it.
	Tags *[]string `json:"tags,omitempty"`
}

func (q Queued) encode() (string, error) {
	if len(q.Meta) == 0 && q.Tags == nil {
		return "", nil
	}
	b, err := json.Marshal(q)
	return string(b), err
}

func (ix *Index) PutSpool(ctx context.Context, r SpoolRow) error {
	queued, err := r.Queued.encode()
	if err != nil {
		return err
	}
	_, err = ix.db.ExecContext(ctx, `INSERT INTO spools (id, ino, key, path, state, replaces, upload_uuid,
			created_at, committed_at, attempts, error, queued)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET ino = excluded.ino, key = excluded.key, path = excluded.path,
			state = excluded.state, replaces = excluded.replaces, upload_uuid = excluded.upload_uuid,
			committed_at = excluded.committed_at, attempts = excluded.attempts, error = excluded.error,
			queued = excluded.queued`,
		r.ID, int64(r.Object.Ino), r.Object.Key, r.Path, r.State, r.Replaces, r.UploadUUID,
		r.Created.UnixNano(), r.Committed.UnixNano(), r.Attempts, r.Error, queued)
	return err
}

func (ix *Index) DeleteSpool(ctx context.Context, id string) error {
	_, err := ix.db.ExecContext(ctx, `DELETE FROM spools WHERE id = ?`, id)
	return err
}

// Spools returns every spool row, oldest first.
func (ix *Index) Spools(ctx context.Context) ([]SpoolRow, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT id, ino, key, path, state, replaces, upload_uuid,
		created_at, committed_at, attempts, error, queued FROM spools ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []SpoolRow
	for rows.Next() {
		var r SpoolRow
		var ino, created, committed int64
		var queued string
		if err := rows.Scan(&r.ID, &ino, &r.Object.Key, &r.Path, &r.State, &r.Replaces, &r.UploadUUID,
			&created, &committed, &r.Attempts, &r.Error, &queued); err != nil {
			return nil, err
		}
		if queued != "" {
			if err := json.Unmarshal([]byte(queued), &r.Queued); err != nil {
				return nil, err
			}
		}
		r.Object.Ino = uint64(ino)
		r.Created = time.Unix(0, created)
		if committed != 0 {
			r.Committed = time.Unix(0, committed)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
