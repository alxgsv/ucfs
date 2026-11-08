package cache

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeFetcher struct {
	data    map[string][]byte
	delay   time.Duration
	calls   atomic.Int64
	fetched atomic.Int64
	fail    error
}

func (f *fakeFetcher) ReadRange(ctx context.Context, uuid, _ string, p []byte, off int64) (int, error) {
	f.calls.Add(1)
	if f.fail != nil {
		return 0, f.fail
	}
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	n := copy(p, f.data[uuid][off:])
	f.fetched.Add(int64(n))
	if n < len(p) {
		return n, io.ErrUnexpectedEOF
	}
	return n, nil
}

func pattern(n int, seed byte) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7) ^ seed
	}
	return b
}

func newCache(t *testing.T, f *fakeFetcher, max int64) *Cache {
	t.Helper()
	c, err := Open(f, Options{Dir: t.TempDir(), MaxBytes: max})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Wait)
	return c
}

// waitIdle waits for background prefetches to finish.
func waitIdle(t *testing.T, c *Cache) {
	t.Helper()
	c.Wait()
}

func TestReadAcrossBlocksAndEOF(t *testing.T) {
	size := 3*BlockSize + 123
	f := &fakeFetcher{data: map[string][]byte{"a": pattern(size, 1)}}
	c := newCache(t, f, 1<<30)
	ctx := context.Background()
	k := Key{UUID: "a"}

	p := make([]byte, 2*BlockSize)
	n, err := c.ReadAt(ctx, k, int64(size), p, BlockSize/2)
	if err != nil || n != len(p) || !bytes.Equal(p, f.data["a"][BlockSize/2:BlockSize/2+len(p)]) {
		t.Fatalf("middle read: n=%d err=%v equal=%v", n, err, bytes.Equal(p, f.data["a"][BlockSize/2:]))
	}
	n, err = c.ReadAt(ctx, k, int64(size), p, int64(size-100))
	if !errors.Is(err, io.EOF) || n != 100 || !bytes.Equal(p[:n], f.data["a"][size-100:]) {
		t.Fatalf("tail read: n=%d err=%v", n, err)
	}
	if _, err := c.ReadAt(ctx, k, int64(size), p, int64(size)); !errors.Is(err, io.EOF) {
		t.Fatalf("read at size: err=%v", err)
	}
}

func TestSmallReadFetchesOneBlock(t *testing.T) {
	f := &fakeFetcher{data: map[string][]byte{"a": pattern(16*BlockSize, 2)}}
	c := newCache(t, f, 1<<30)
	if _, err := c.ReadAt(context.Background(), Key{UUID: "a"}, 16*BlockSize, make([]byte, 4096), 0); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, c)
	if got := f.fetched.Load(); got != BlockSize {
		t.Errorf("fetched %d bytes for a 4 KiB read, want one block", got)
	}
	st := c.Stats()
	if st.FetchedDemand != BlockSize || st.FetchedPrefetch != 0 || st.Served != 4096 {
		t.Errorf("stats = %+v", st)
	}
}

func TestSequentialReadPrefetchesAndFetchesOnce(t *testing.T) {
	size := 16 * BlockSize
	f := &fakeFetcher{data: map[string][]byte{"a": pattern(size, 3)}, delay: 20 * time.Millisecond}
	c := newCache(t, f, 1<<30)
	k := Key{UUID: "a"}
	buf := make([]byte, BlockSize)
	var got []byte
	start := time.Now()
	for off := int64(0); off < int64(size); off += BlockSize {
		n, err := c.ReadAt(context.Background(), k, int64(size), buf, off)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		got = append(got, buf[:n]...)
	}
	elapsed := time.Since(start)
	waitIdle(t, c)
	if !bytes.Equal(got, f.data["a"]) {
		t.Fatal("content mismatch")
	}
	if f.fetched.Load() != int64(size) {
		t.Errorf("fetched %d bytes, want each block exactly once (%d)", f.fetched.Load(), size)
	}
	st := c.Stats()
	if st.FetchedPrefetch == 0 || st.Hit == 0 {
		t.Errorf("no prefetch happened: %+v", st)
	}
	// 16 sequential 20 ms fetches take 320 ms; prefetch must overlap them.
	if elapsed > 200*time.Millisecond {
		t.Errorf("sequential read took %v, prefetch is not concurrent", elapsed)
	}
}

func TestConcurrentReadersShareOneFetch(t *testing.T) {
	f := &fakeFetcher{data: map[string][]byte{"a": pattern(BlockSize, 4)}, delay: 50 * time.Millisecond}
	c := newCache(t, f, 1<<30)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			p := make([]byte, 100)
			if _, err := c.ReadAt(context.Background(), Key{UUID: "a"}, BlockSize, p, 500); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if f.calls.Load() != 1 {
		t.Errorf("fetches = %d, want 1", f.calls.Load())
	}
}

func TestCancelledReaderDoesNotFailOthers(t *testing.T) {
	f := &fakeFetcher{data: map[string][]byte{"a": pattern(BlockSize, 5)}, delay: 100 * time.Millisecond}
	c := newCache(t, f, 1<<30)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.ReadAt(ctx, Key{UUID: "a"}, BlockSize, make([]byte, 10), 0)
		done <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancel()
	if _, err := c.ReadAt(context.Background(), Key{UUID: "a"}, BlockSize, make([]byte, 10), 0); err != nil {
		t.Fatalf("second reader: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("first reader: %v", err)
	}
}

func TestEvictionAndReopen(t *testing.T) {
	f := &fakeFetcher{data: map[string][]byte{"a": pattern(32*BlockSize, 6)}}
	dir := t.TempDir()
	c, err := Open(f, Options{Dir: dir, MaxBytes: 3 * BlockSize})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Wait)
	p := make([]byte, 10)
	for b := int64(0); b < 32; b += 8 { // farther apart than the stream slack: no prefetch
		if _, err := c.ReadAt(context.Background(), Key{UUID: "a"}, 32*BlockSize, p, b*BlockSize); err != nil {
			t.Fatal(err)
		}
	}
	if st := c.Stats(); st.Used > 3*BlockSize || st.Evicted != BlockSize {
		t.Errorf("stats = %+v, want used <= 3 blocks and one evicted", st)
	}

	c2, err := Open(f, Options{Dir: dir, MaxBytes: 3 * BlockSize})
	if err != nil {
		t.Fatal(err)
	}
	before := f.calls.Load()
	if _, err := c2.ReadAt(context.Background(), Key{UUID: "a"}, 32*BlockSize, p, 24*BlockSize); err != nil {
		t.Fatal(err)
	}
	if f.calls.Load() != before {
		t.Error("reopened cache did not serve a block it already had")
	}
}

func TestFetchErrorIsReturned(t *testing.T) {
	boom := errors.New("boom")
	f := &fakeFetcher{fail: boom}
	c := newCache(t, f, 1<<30)
	if _, err := c.ReadAt(context.Background(), Key{UUID: "a"}, 10, make([]byte, 5), 0); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
}

func TestTwoStreamsOnOneObject(t *testing.T) {
	size := int64(64 * BlockSize)
	f := &fakeFetcher{data: map[string][]byte{"a": pattern(int(size), 8)}}
	c := newCache(t, f, 1<<30)
	k := Key{UUID: "a"}
	buf := make([]byte, BlockSize)
	// Interleave two sequential readers far apart; both must turn sequential.
	for i := int64(0); i < 4; i++ {
		for _, base := range []int64{0, 32 * BlockSize} {
			if _, err := c.ReadAt(context.Background(), k, size, buf, base+i*BlockSize); err != nil {
				t.Fatal(err)
			}
		}
	}
	c.mu.Lock()
	n := len(c.streams[k])
	windows := []int64{}
	for _, s := range c.streams[k] {
		windows = append(windows, s.window)
	}
	c.mu.Unlock()
	if n != 2 || windows[0] == 0 || windows[1] == 0 {
		t.Errorf("streams = %d windows = %v, want 2 sequential streams", n, windows)
	}
}
