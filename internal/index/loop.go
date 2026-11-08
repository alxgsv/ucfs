package index

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/alxgsv/ucfs/internal/api"
)

// Source is the part of api.Client the sync loop uses.
type Source interface {
	Lister
	Info(ctx context.Context, uuid string) (api.File, error)
}

// Loop keeps a mounted index current: incremental listing every Interval,
// a removed sweep every SweepEvery, and refreshes after CDN 404s.
type Loop struct {
	Index      *Index
	Source     Source
	Interval   time.Duration
	SweepEvery time.Duration
	// Changed runs after any update that changed the index.
	Changed func()

	mu      sync.Mutex
	last    time.Time
	lastErr error
}

func (l *Loop) Run(ctx context.Context) {
	tick := time.NewTicker(l.Interval)
	defer tick.Stop()
	lastSweep := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		res, err := l.Index.Sync(ctx, l.Source, false)
		changed := res.Added > 0
		if err == nil && time.Since(lastSweep) >= l.SweepEvery {
			var n int
			n, err = l.Index.SweepRemoved(ctx, l.Source)
			lastSweep = time.Now()
			changed = changed || n > 0
			slog.Debug("removed sweep", "marked", n, "err", err)
		}
		l.record(err)
		if err != nil && ctx.Err() == nil {
			slog.Warn("index sync", "err", err)
		}
		if changed {
			l.Changed()
		}
	}
}

func (l *Loop) record(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last, l.lastErr = time.Now(), err
}

// Status returns when the last sync ran and how it ended.
func (l *Loop) Status() (time.Time, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last, l.lastErr
}

// Refresh re-reads one file's info after the CDN answered 404 for it. Only
// the API decides that a file is removed, so a CDN hiccup cannot hide files.
func (l *Loop) Refresh(ctx context.Context, uuid string) {
	f, err := l.Source.Info(ctx, uuid)
	gone := api.IsNotFound(err) || (err == nil && !f.RemovedAt.IsZero())
	switch {
	case api.IsNotFound(err):
		_, err = l.Index.db.ExecContext(ctx, `UPDATE files SET removed = 1, removed_at = ? WHERE uuid = ?`, time.Now().Unix(), uuid)
	case err == nil:
		err = l.Index.Put(ctx, f)
	}
	if err != nil {
		slog.Warn("refresh after CDN 404", "uuid", uuid, "err", err)
		return
	}
	slog.Info("refreshed after CDN 404", "uuid", uuid, "removed", gone)
	l.Changed()
}
