package cdn

import (
	"context"
	"sync"
	"time"
)

// DefaultMaxInflight is how many CDN requests a mount keeps in flight.
const DefaultMaxInflight = 32

// maxBackground bounds the prefetches waiting for a slot. A reader that
// keeps moving schedules new blocks faster than a congested link serves
// them; beyond this, new prefetches are dropped rather than queued.
const maxBackground = 1024

// Limiter bounds the CDN requests of a mount in flight. Requests someone
// waits for (reads that missed the cache, HEADs) go before prefetches, and
// prefetches hold at most three quarters of the slots, so a read finds a
// slot free however much is being prefetched. Within each class the
// objects waiting take turns, so one reader with many queued requests
// cannot hold back another's.
type Limiter struct {
	mu                   sync.Mutex
	max, bgMax           int
	inflight, bgInflight int
	fg, bg               turns

	waits, dropped int64
	waited         time.Duration
}

func NewLimiter(n int) *Limiter {
	return &Limiter{max: n, bgMax: max(1, n*3/4), fg: newTurns(), bg: newTurns()}
}

type waiter struct {
	owner string
	// ready is closed when an Acquire is granted; run is a Go function,
	// started in its own goroutine when granted.
	ready   chan struct{}
	run     func()
	granted bool
	queued  time.Time
}

// Acquire blocks until a request for owner may go out and returns the
// function that frees its slot.
func (l *Limiter) Acquire(ctx context.Context, owner string) (func(), error) {
	l.mu.Lock()
	if l.inflight < l.max && l.fg.len() == 0 {
		l.inflight++
		l.mu.Unlock()
		return l.releaser(false), nil
	}
	w := &waiter{owner: owner, ready: make(chan struct{}), queued: time.Now()}
	l.fg.push(w)
	l.mu.Unlock()
	select {
	case <-w.ready:
		return l.releaser(false), nil
	case <-ctx.Done():
		l.mu.Lock()
		granted := w.granted
		if !granted {
			l.fg.remove(w)
		}
		l.mu.Unlock()
		if granted {
			l.release(false)
		}
		return nil, ctx.Err()
	}
}

// Go runs fn in its own goroutine once a prefetch slot for owner is free,
// and frees the slot when fn returns. It reports false, and never runs fn,
// when too many prefetches are waiting already.
func (l *Limiter) Go(owner string, fn func()) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.bg.len() >= maxBackground {
		l.dropped++
		return false
	}
	l.bg.push(&waiter{owner: owner, run: fn, queued: time.Now()})
	l.dispatchLocked()
	return true
}

func (l *Limiter) releaser(bg bool) func() {
	var once sync.Once
	return func() { once.Do(func() { l.release(bg) }) }
}

func (l *Limiter) release(bg bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inflight--
	if bg {
		l.bgInflight--
	}
	l.dispatchLocked()
}

func (l *Limiter) dispatchLocked() {
	for l.inflight < l.max {
		w := l.fg.pop()
		bg := false
		if w == nil && l.bgInflight < l.bgMax {
			w, bg = l.bg.pop(), true
		}
		if w == nil {
			return
		}
		l.inflight++
		if bg {
			l.bgInflight++
		}
		w.granted = true
		if d := time.Since(w.queued); d > 0 && !bg {
			l.waits++
			l.waited += d
		}
		if w.run != nil {
			go func() {
				defer l.release(true)
				w.run()
			}()
		} else {
			close(w.ready)
		}
	}
}

// LimiterStats is what the limiter holds and has held up: requests in
// flight and waiting, reads that had to wait for a slot and how long in
// all, and prefetches dropped because too many were waiting.
type LimiterStats struct {
	Max, Inflight, Queued, QueuedPrefetch int
	Waits                                 int64
	Waited                                time.Duration
	Dropped                               int64
}

func (l *Limiter) Stats() LimiterStats {
	l.mu.Lock()
	defer l.mu.Unlock()
	return LimiterStats{Max: l.max, Inflight: l.inflight, Queued: l.fg.len(), QueuedPrefetch: l.bg.len(),
		Waits: l.waits, Waited: l.waited, Dropped: l.dropped}
}

// turns queues waiters per owner and hands them out one owner at a time.
type turns struct {
	owners []string
	queues map[string][]*waiter
	next   int
	n      int
}

func newTurns() turns { return turns{queues: map[string][]*waiter{}} }

func (t *turns) len() int { return t.n }

func (t *turns) push(w *waiter) {
	q, ok := t.queues[w.owner]
	if !ok {
		t.owners = append(t.owners, w.owner)
	}
	t.queues[w.owner] = append(q, w)
	t.n++
}

func (t *turns) pop() *waiter {
	if t.n == 0 {
		return nil
	}
	i := t.next % len(t.owners)
	o := t.owners[i]
	q := t.queues[o]
	w := q[0]
	t.n--
	if len(q) == 1 {
		t.drop(i)
		t.next = i
	} else {
		t.queues[o] = q[1:]
		t.next = i + 1
	}
	return w
}

func (t *turns) remove(w *waiter) {
	q := t.queues[w.owner]
	for i, x := range q {
		if x != w {
			continue
		}
		t.n--
		if len(q) > 1 {
			t.queues[w.owner] = append(q[:i:i], q[i+1:]...)
			return
		}
		for j, o := range t.owners {
			if o == w.owner {
				t.drop(j)
				if j < t.next {
					t.next--
				}
				break
			}
		}
		return
	}
}

func (t *turns) drop(i int) {
	delete(t.queues, t.owners[i])
	t.owners = append(t.owners[:i], t.owners[i+1:]...)
}
