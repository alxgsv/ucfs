package cache

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"syscall"
	"time"
)

// ErrBudget is a read that needs CDN bytes the day's budget has no room for.
var ErrBudget = fmt.Errorf("daily traffic budget reached: %w", syscall.EDQUOT)

// TrafficStore persists the bytes counted against a day's budget, so a
// remount does not start the day over and mounts of one project share it.
type TrafficStore interface {
	// AddTraffic adds n bytes to day's count, starting it at n when the
	// stored count is of another day, and returns the day's total.
	AddTraffic(ctx context.Context, day string, n int64) (int64, error)
}

// Budget caps the CDN bytes read per calendar day in the local zone. Only
// the bodies of range reads count: a HEAD carries no body, and the CDN's
// error pages, about 1 KiB each, are left out. A block is counted before it
// is fetched and given back if the fetch fails, so concurrent fetches never
// take the count past the limit. Prefetch stops while less than a reserve
// remains, which keeps the last bytes for reads someone waits for.
type Budget struct {
	limit, reserve int64
	store          TrafficStore
	now            func() time.Time

	mu sync.Mutex
	// day is the window, as 2006-01-02. base is its count as stored at the
	// last flush, local what this mount added since.
	day         string
	base, local int64
	refused     int64
	logged      bool
	prefetchOff bool
}

const budgetFlush = 5 * time.Second

// NewBudget loads today's count from store.
func NewBudget(ctx context.Context, limit int64, store TrafficStore) (*Budget, error) {
	return newBudgetAt(ctx, limit, store, time.Now)
}

func newBudgetAt(ctx context.Context, limit int64, store TrafficStore, now func() time.Time) (*Budget, error) {
	b := &Budget{limit: limit, reserve: min(limit/10, maxPrefetchWindow), store: store, now: now}
	b.day = dayOf(b.now())
	total, err := store.AddTraffic(ctx, b.day, 0)
	if err != nil {
		return nil, err
	}
	b.base = total
	return b, nil
}

func dayOf(t time.Time) string { return t.Format(time.DateOnly) }

// rollLocked starts a new window when the day changed. What the mount
// counted in the old one is lost unless flushed, which only matters to a
// day that is over.
func (b *Budget) rollLocked() {
	if d := dayOf(b.now()); d != b.day {
		b.day, b.base, b.local, b.refused = d, 0, 0, 0
		b.logged, b.prefetchOff = false, false
	}
}

// take counts n bytes if the window has room for them.
func (b *Budget) take(n int64, prefetch bool) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	room := b.limit - b.base - b.local
	if prefetch {
		room -= b.reserve
	}
	if n <= room {
		b.local += n
		return true
	}
	if prefetch {
		if !b.prefetchOff {
			b.prefetchOff = true
			slog.Info("traffic budget nearly used; prefetch stopped", "used", b.base+b.local, "limit", b.limit)
		}
		return false
	}
	b.refused++
	if !b.logged {
		b.logged = true
		slog.Warn("traffic budget reached; reads that need the CDN fail with EDQUOT until the window resets",
			"used", b.base+b.local, "limit", b.limit, "resets", b.resetsAtLocked())
	}
	return false
}

// allowPrefetch reports whether a prefetch of n bytes would be taken now.
func (b *Budget) allowPrefetch(n int64) bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	return n <= b.limit-b.base-b.local-b.reserve
}

// give returns bytes taken for a fetch that failed or was not needed.
func (b *Budget) give(n int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.local -= n
}

func (b *Budget) resetsAtLocked() time.Time {
	t, _ := time.ParseInLocation(time.DateOnly, b.day, b.now().Location())
	return t.AddDate(0, 0, 1)
}

// Flush adds what the mount counted to the store and takes up what other
// mounts of the project counted.
func (b *Budget) Flush(ctx context.Context) error {
	b.mu.Lock()
	b.rollLocked()
	day, delta := b.day, b.local
	b.local = 0
	b.mu.Unlock()
	total, err := b.store.AddTraffic(ctx, day, delta)
	b.mu.Lock()
	defer b.mu.Unlock()
	if day != b.day {
		return err
	}
	if err != nil {
		b.local += delta
		return err
	}
	b.base = total
	return nil
}

// Run flushes every few seconds until ctx ends, then once more.
func (b *Budget) Run(ctx context.Context) {
	t := time.NewTicker(budgetFlush)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := b.Flush(context.WithoutCancel(ctx)); err != nil {
				slog.Warn("store traffic count", "err", err)
			}
			return
		case <-t.C:
			if err := b.Flush(ctx); err != nil && ctx.Err() == nil {
				slog.Warn("store traffic count", "err", err)
			}
		}
	}
}

type BudgetStatus struct {
	Limit, Used, Remaining int64
	ResetsAt               time.Time
	// Refused counts reads failed with EDQUOT in this window; Prefetch is
	// false once prefetch stopped for it.
	Refused  int64
	Prefetch bool
}

func (b *Budget) Status() BudgetStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.rollLocked()
	used := b.base + b.local
	return BudgetStatus{Limit: b.limit, Used: used, Remaining: max(b.limit-used, 0), ResetsAt: b.resetsAtLocked(),
		Refused: b.refused, Prefetch: b.limit-used-b.reserve > 0}
}
