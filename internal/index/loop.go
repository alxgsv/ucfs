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

// Loop keeps a mounted index current: an incremental listing right away
// and then every Interval, a removed sweep and a group sync every
// SweepEvery, and refreshes after CDN 404s. Failed syncs back off.
type Loop struct {
	Index      *Index
	Source     Source
	Interval   time.Duration
	SweepEvery time.Duration
	// Groups, when set, lists new groups with every sweep.
	Groups GroupLister
	// Changed runs after any update that changed the index.
	Changed func()

	mu       sync.Mutex
	last     time.Time
	lastErr  error
	failures int
	next     time.Time
}

// maxBackoff caps the wait after failed syncs: it doubles with every
// failure in a row, so a project whose API access was denied is asked
// every 10 minutes rather than every Interval.
const maxBackoff = 10 * time.Minute

func (l *Loop) Run(ctx context.Context) {
	// The zero time makes the first sync sweep, so a new mount soon drops
	// files removed while nothing was mounted.
	var lastSweep time.Time
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		res, err := l.Index.Sync(ctx, l.Source, false)
		changed := res.Added > 0
		if err == nil && time.Since(lastSweep) >= l.SweepEvery {
			var n int
			n, err = l.Index.SweepRemoved(ctx, l.Source)
			lastSweep = time.Now()
			changed = changed || n > 0
			slog.Debug("removed sweep", "marked", n, "err", err)
			if err == nil && l.Groups != nil {
				n, err = l.Index.SyncGroups(ctx, l.Groups)
				changed = changed || n > 0
				slog.Debug("group sync", "added", n, "err", err)
			}
		}
		if ctx.Err() != nil {
			return
		}
		wait := l.record(err)
		timer.Reset(wait)
		if changed {
			l.Changed()
		}
	}
}

// record notes how a sync ended and returns the wait before the next. The
// first failure of a streak is logged as a warning, the rest at debug
// level, and the end of a streak at info.
func (l *Loop) record(err error) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.last, l.lastErr = time.Now(), err
	wait := l.Interval
	switch {
	case err != nil:
		l.failures++
		wait = min(l.Interval<<min(l.failures, 16), maxBackoff)
		if l.failures == 1 {
			slog.Warn("index sync failed; serving the index as it is and backing off", "err", err, "retry_in", wait)
		} else {
			slog.Debug("index sync failed", "failures", l.failures, "err", err, "retry_in", wait)
		}
	case l.failures > 0:
		slog.Info("index sync works again", "failures", l.failures)
		l.failures = 0
	}
	l.next = l.last.Add(wait)
	return wait
}

// LoopStatus is how the last sync ended: when, with which error, how many
// failed in a row, and when the next runs.
type LoopStatus struct {
	Last     time.Time
	Err      error
	Failures int
	Next     time.Time
}

func (l *Loop) Status() LoopStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	return LoopStatus{Last: l.last, Err: l.lastErr, Failures: l.failures, Next: l.next}
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
		err = l.Index.PutInfo(ctx, time.Now(), f)
	}
	if err != nil {
		slog.Warn("refresh after CDN 404", "uuid", uuid, "err", err)
		return
	}
	slog.Info("refreshed after CDN 404", "uuid", uuid, "removed", gone)
	l.Changed()
}
