package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"syscall"
	"testing"
	"time"
)

type memTraffic struct {
	mu   sync.Mutex
	day  string
	used int64
}

func (m *memTraffic) AddTraffic(_ context.Context, day string, n int64) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.day != day {
		m.day, m.used = day, 0
	}
	m.used += n
	return m.used, nil
}

func newBudget(t *testing.T, limit int64, store TrafficStore) *Budget {
	return newBudgetClock(t, limit, store, time.Now)
}

func newBudgetClock(t *testing.T, limit int64, store TrafficStore, now func() time.Time) *Budget {
	t.Helper()
	b, err := newBudgetAt(context.Background(), limit, store, now)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A sequential read that crosses the budget gets every block that fits and
// then EDQUOT; the count never passes the limit, and cached blocks are
// still served.
func TestBudgetBoundary(t *testing.T) {
	size := 6 * BlockSize
	f := &fakeFetcher{data: map[string][]byte{"a": pattern(size, 3)}}
	b := newBudget(t, 3*BlockSize+BlockSize/2, &memTraffic{})
	c, err := Open(f, Options{Dir: t.TempDir(), MaxBytes: 1 << 30, Budget: b})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k := Key{UUID: "a"}
	p := make([]byte, BlockSize)
	var got int
	for off := int64(0); off < int64(size); off += BlockSize {
		_, err := c.ReadAt(ctx, k, int64(size), p, off)
		c.Wait()
		if err != nil {
			if !errors.Is(err, syscall.EDQUOT) || !errors.Is(err, ErrBudget) {
				t.Fatalf("read at %d: %v, want EDQUOT", off, err)
			}
			break
		}
		if !bytes.Equal(p, f.data["a"][off:off+BlockSize]) {
			t.Fatalf("read at %d: wrong bytes", off)
		}
		got++
	}
	st := b.Status()
	if got != 3 || st.Used != 3*BlockSize || st.Used > st.Limit || st.Refused != 1 {
		t.Fatalf("read %d blocks, status %+v; want 3 blocks, %d used, 1 refused", got, st, 3*BlockSize)
	}
	if f.fetched.Load() != st.Used {
		t.Fatalf("fetched %d bytes, budget counted %d", f.fetched.Load(), st.Used)
	}
	if _, err := c.ReadAt(ctx, k, int64(size), p, 0); err != nil {
		t.Fatalf("cached block after the budget ran out: %v", err)
	}
}

// Prefetch stops while less than the reserve remains, so the bytes left go
// to reads someone waits for.
func TestBudgetStopsPrefetchFirst(t *testing.T) {
	size := 40 * BlockSize
	f := &fakeFetcher{data: map[string][]byte{"a": pattern(size, 4)}}
	b := newBudget(t, 20*BlockSize, &memTraffic{})
	c, err := Open(f, Options{Dir: t.TempDir(), MaxBytes: 1 << 30, Budget: b})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	k := Key{UUID: "a"}
	p := make([]byte, 128<<10)
	off := int64(0)
	for ; off < int64(size); off += int64(len(p)) {
		if _, err := c.ReadAt(ctx, k, int64(size), p, off); err != nil {
			break
		}
	}
	c.Wait()
	st := c.Stats()
	bs := b.Status()
	if bs.Prefetch || bs.Used != 20*BlockSize || bs.Refused == 0 {
		t.Fatalf("budget %+v: want prefetch stopped, 20 blocks used, reads refused", bs)
	}
	// The reserve is 2 blocks (a tenth of 20); prefetch never took them.
	if st.FetchedPrefetch > 18*BlockSize || st.FetchedDemand < 2*BlockSize {
		t.Fatalf("prefetched %d, demand %d: the last two blocks should go to demand reads", st.FetchedPrefetch, st.FetchedDemand)
	}
}

// The count survives a remount through the store, other mounts' bytes
// count too, and a new day starts over.
func TestBudgetPersistsAndRolls(t *testing.T) {
	store := &memTraffic{}
	now := time.Date(2026, 11, 8, 23, 59, 0, 0, time.Local)
	clock := func() time.Time { return now }
	b := newBudgetClock(t, 10*BlockSize, store, clock)
	if !b.take(4*BlockSize, false) {
		t.Fatal("take within the budget refused")
	}
	if err := b.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTraffic(context.Background(), b.day, BlockSize); err != nil {
		t.Fatal(err)
	}
	b2 := newBudgetClock(t, 10*BlockSize, store, clock)
	if st := b2.Status(); st.Used != 5*BlockSize || st.Remaining != 5*BlockSize {
		t.Fatalf("remount sees %+v, want 5 blocks used", st)
	}
	if st := b.Status(); !st.ResetsAt.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.Local)) {
		t.Fatalf("resets at %v", st.ResetsAt)
	}
	now = now.Add(2 * time.Minute)
	if st := b2.Status(); st.Used != 0 || st.Remaining != 10*BlockSize {
		t.Fatalf("next day %+v, want a fresh window", st)
	}
	if !b2.take(10*BlockSize, false) || b2.take(1, false) {
		t.Fatal("the new window should take exactly the limit")
	}
}

// A range with another ETag than the object's fails the read, drops the
// cached blocks, and the next read sees only the new content.
func TestChangedOriginalNeverMixes(t *testing.T) {
	size := 4 * BlockSize
	old, fresh := pattern(size, 5), pattern(size, 6)
	f := &fakeFetcher{data: map[string][]byte{}}
	f.set("a", old, `"old"`)
	c := newCache(t, f, 1<<30)
	ctx := context.Background()
	k := Key{UUID: "a"}
	p := make([]byte, BlockSize)
	if _, err := c.ReadAt(ctx, k, int64(size), p, 0); err != nil || !bytes.Equal(p, old[:BlockSize]) {
		t.Fatalf("first block: %v", err)
	}
	c.Wait()
	f.set("a", fresh, `"new"`)
	var changed *ChangedError
	if _, err := c.ReadAt(ctx, k, int64(size), p, 3*BlockSize); !errors.As(err, &changed) {
		t.Fatalf("read after the change: %v, want ChangedError", err)
	}
	if changed.Want != `"old"` || changed.Got != `"new"` || c.Stats().Changed != 1 {
		t.Fatalf("changed %+v, stats %+v", changed, c.Stats())
	}
	all := make([]byte, size)
	if _, err := c.ReadAt(ctx, k, int64(size), all, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Equal(all, fresh) {
		t.Fatal("whole read after the change mixes old and new blocks")
	}
}

// A variant's blocks are keyed by the ETag its HEAD gave, so a range with
// another fails without storing anything under that key.
func TestChangedVariantFails(t *testing.T) {
	f := &fakeFetcher{data: map[string][]byte{}}
	f.set("v", pattern(BlockSize, 7), `W/"b"`)
	c := newCache(t, f, 1<<30)
	k := Key{UUID: "v", Ops: "-/resize/10x/", Version: `W/"a"`}
	var changed *ChangedError
	if _, err := c.ReadAt(context.Background(), k, BlockSize, make([]byte, 10), 0); !errors.As(err, &changed) {
		t.Fatalf("read: %v, want ChangedError", err)
	}
	if st := c.Stats(); st.Used != 0 {
		t.Fatalf("stored %d bytes of a changed variant", st.Used)
	}
	k.Version = `W/"b"`
	if _, err := c.ReadAt(context.Background(), k, BlockSize, make([]byte, 10), 0); err != nil {
		t.Fatalf("read with the new ETag: %v", err)
	}
}

// Readers racing a change, with prefetch on, never get a read made of
// both contents: every read returns all old or all new bytes, or fails.
func TestChangeUnderConcurrentReaders(t *testing.T) {
	size := 24 * BlockSize
	old, fresh := pattern(size, 8), pattern(size, 9)
	f := &fakeFetcher{data: map[string][]byte{}, delay: time.Millisecond}
	f.set("a", old, `"old"`)
	c := newCache(t, f, 1<<30)
	k := Key{UUID: "a"}
	var wg sync.WaitGroup
	for r := range 4 {
		wg.Go(func() {
			p := make([]byte, 3*BlockSize/2)
			for off := int64(r) * BlockSize / 3; off+int64(len(p)) <= int64(size); off += int64(len(p)) {
				_, err := c.ReadAt(context.Background(), k, int64(size), p, off)
				switch {
				case err != nil:
					var changed *ChangedError
					if !errors.As(err, &changed) {
						t.Errorf("read at %d: %v", off, err)
					}
				case !bytes.Equal(p, old[off:off+int64(len(p))]) && !bytes.Equal(p, fresh[off:off+int64(len(p))]):
					t.Errorf("read at %d mixes old and new bytes", off)
				}
			}
		})
	}
	time.Sleep(5 * time.Millisecond)
	f.set("a", fresh, `"new"`)
	wg.Wait()
	c.Wait()
	all := make([]byte, size)
	if _, err := c.ReadAt(context.Background(), k, int64(size), all, 0); err != nil && !errors.Is(err, io.EOF) {
		var changed *ChangedError
		if !errors.As(err, &changed) {
			t.Fatal(err)
		}
		if _, err := c.ReadAt(context.Background(), k, int64(size), all, 0); err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(all, fresh) {
		t.Fatal("cache holds blocks of the old content after the change")
	}
}
