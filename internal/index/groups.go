package index

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/alxgsv/ucfs/internal/api"
)

// Group is a file group as the index knows it.
type Group struct {
	ID        string
	CreatedAt time.Time
	// Files are the members' uuids in group order, "" for a member the API
	// no longer returns. Fetched is false until the group's info was read.
	Files   []string
	Fetched bool
}

// GroupLister is the part of api.Client that group sync needs.
type GroupLister interface {
	ListGroups(ctx context.Context, from time.Time, fn func(api.Group) error) error
}

// SyncGroups lists every group the API knows, records the new ones, whose
// members are read when a group is first listed, and drops the ones it no
// longer lists: the API lists no deleted group. It costs one request per
// thousand groups. It returns how many groups were added and dropped.
func (ix *Index) SyncGroups(ctx context.Context, l GroupLister) (int, error) {
	listed := map[string]bool{}
	added := 0
	err := l.ListGroups(ctx, time.Time{}, func(g api.Group) error {
		listed[g.ID] = true
		res, err := ix.db.ExecContext(ctx, `INSERT INTO groups (uuid, created_at) VALUES (?, ?) ON CONFLICT (uuid) DO NOTHING`,
			g.ID, g.CreatedAt.UnixNano())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		added += int(n)
		return err
	})
	if err != nil {
		return added, err
	}
	known, err := ix.Groups(ctx)
	if err != nil {
		return added, err
	}
	dropped := 0
	for _, g := range known {
		if listed[g.ID] {
			continue
		}
		if err := ix.DeleteGroup(ctx, g.ID); err != nil {
			return added + dropped, err
		}
		dropped++
	}
	return added + dropped, nil
}

func scanGroup(s interface{ Scan(...any) error }) (Group, error) {
	var g Group
	var created int64
	var files sql.NullString
	if err := s.Scan(&g.ID, &created, &files); err != nil {
		return g, err
	}
	g.CreatedAt = time.Unix(0, created)
	if files.Valid {
		g.Fetched = true
		if err := json.Unmarshal([]byte(files.String), &g.Files); err != nil {
			return g, err
		}
	}
	return g, nil
}

// Groups returns every known group, oldest first.
func (ix *Index) Groups(ctx context.Context) ([]Group, error) {
	rows, err := ix.db.QueryContext(ctx, `SELECT uuid, coalesce(created_at, 0), files FROM groups ORDER BY created_at, uuid`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Group returns one group or ErrNotFound.
func (ix *Index) Group(ctx context.Context, id string) (Group, error) {
	g, err := scanGroup(ix.db.QueryRowContext(ctx, `SELECT uuid, coalesce(created_at, 0), files FROM groups WHERE uuid = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	return g, err
}

// PutGroup records a group fetched at a time with its members, whose info
// is upserted without moving the sync cursor.
func (ix *Index) PutGroup(ctx context.Context, g api.Group, at time.Time) error {
	ids := make([]string, len(g.Members))
	var files []api.File
	for i, m := range g.Members {
		if m != nil {
			ids[i] = m.UUID
			files = append(files, *m)
		}
	}
	if len(files) > 0 {
		if err := ix.PutInfo(ctx, at, files...); err != nil {
			return err
		}
	}
	b, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	_, err = ix.db.ExecContext(ctx, `INSERT INTO groups (uuid, created_at, files) VALUES (?, ?, ?)
		ON CONFLICT (uuid) DO UPDATE SET created_at = excluded.created_at, files = excluded.files`,
		g.ID, g.CreatedAt.UnixNano(), string(b))
	return err
}

// DeleteGroup forgets a group the API no longer knows.
func (ix *Index) DeleteGroup(ctx context.Context, id string) error {
	_, err := ix.db.ExecContext(ctx, `DELETE FROM groups WHERE uuid = ?`, id)
	return err
}
