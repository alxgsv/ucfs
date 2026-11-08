package index

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
)

const (
	keyTrafficDay  = "traffic_day"
	keyTrafficUsed = "traffic_used"
)

// AddTraffic counts n CDN bytes against day for the traffic budget,
// starting over when the stored count is of another day, and returns the
// day's total. Mounts of one project add to the same count.
func (ix *Index) AddTraffic(ctx context.Context, day string, n int64) (int64, error) {
	tx, err := ix.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	var stored, used string
	err = tx.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key = ?`, keyTrafficDay).Scan(&stored)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	total := n
	if stored == day {
		err = tx.QueryRowContext(ctx, `SELECT value FROM sync_state WHERE key = ?`, keyTrafficUsed).Scan(&used)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, err
		}
		if used != "" {
			prev, err := strconv.ParseInt(used, 10, 64)
			if err != nil {
				return 0, err
			}
			total += prev
		}
	}
	if n == 0 && stored == day {
		return total, nil
	}
	if err := ix.setState(ctx, tx, keyTrafficDay, day); err != nil {
		return 0, err
	}
	if err := ix.setState(ctx, tx, keyTrafficUsed, strconv.FormatInt(total, 10)); err != nil {
		return 0, err
	}
	return total, tx.Commit()
}
