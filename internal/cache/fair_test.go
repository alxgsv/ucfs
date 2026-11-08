package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/cdn"
	"github.com/alxgsv/ucfs/internal/fake"
)

// fifoFetcher bounds requests in flight with a plain semaphore, the
// comparison for the limiter: the same bound without classes or turns.
type fifoFetcher struct {
	Fetcher
	sem chan struct{}
}

func (f *fifoFetcher) ReadRange(ctx context.Context, uuid, ops string, p []byte, off int64) (int, string, error) {
	f.sem <- struct{}{}
	defer func() { <-f.sem }()
	return f.Fetcher.ReadRange(ctx, uuid, ops, p, off)
}

const (
	fairLatency  = 30 * time.Millisecond
	fairInflight = 8
	smallSize    = 64 << 10
)

// fairRun reads small files one after another, cold, while bulk readers
// stream large files, and returns the small reads' latencies. With fifo
// the bound is a plain semaphore instead of the limiter.
func fairRun(t testing.TB, bulk, small int, fifo bool) []time.Duration {
	srv := fake.New(t)
	t0 := time.Now().Add(-time.Hour)
	var bigs, smalls []fake.File
	for i := range bulk {
		bigs = append(bigs, fake.File{UUID: fmt.Sprintf("b0000000-0000-4000-8000-%012d", i), Size: 64 << 20, Uploaded: t0})
	}
	for i := range small {
		smalls = append(smalls, fake.File{UUID: fmt.Sprintf("50000000-0000-4000-8000-%012d", i), Size: smallSize, Uploaded: t0})
	}
	srv.Add(append(bigs, smalls...)...)
	for _, f := range append(bigs, smalls...) {
		srv.SetLatency(f.UUID, fairLatency)
	}
	var fetch Fetcher = cdn.New(srv.CDNBase(), nil, nil)
	lim := cdn.NewLimiter(fairInflight)
	if fifo {
		fetch, lim = &fifoFetcher{Fetcher: fetch, sem: make(chan struct{}, fairInflight)}, cdn.NewLimiter(1<<20)
	}
	c, err := Open(fetch, Options{Dir: t.TempDir(), MaxBytes: 1 << 30, Limiter: lim})
	if err != nil {
		t.Fatal(err)
	}
	ctx, stop := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for _, f := range bigs {
		wg.Go(func() {
			p := make([]byte, 256<<10)
			for off := int64(0); off < f.Size && ctx.Err() == nil; off += int64(len(p)) {
				if _, err := c.ReadAt(ctx, Key{UUID: f.UUID}, f.Size, p, off); err != nil && !errors.Is(err, io.EOF) {
					t.Error(err)
					return
				}
			}
		})
	}
	if bulk > 0 {
		time.Sleep(10 * fairLatency)
	}
	var lat []time.Duration
	p := make([]byte, smallSize)
	for _, f := range smalls {
		start := time.Now()
		if _, err := c.ReadAt(ctx, Key{UUID: f.UUID}, f.Size, p, 0); err != nil && !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
		lat = append(lat, time.Since(start))
	}
	stop()
	wg.Wait()
	c.Wait()
	return lat
}

func percentile(d []time.Duration, q float64) time.Duration {
	s := slices.Clone(d)
	slices.Sort(s)
	return s[min(len(s)-1, int(float64(len(s))*q))]
}

// A cat of small files beside bulk readers of large ones waits at most
// about one request for a slot: its p95 stays within twice the latency of
// the cat alone, where a plain bound of the same size queues it behind the
// bulk readers' prefetches.
func TestFairnessSmallReadBesideBulk(t *testing.T) {
	if testing.Short() {
		t.Skip("streams several hundred MiB through the fake")
	}
	alone := percentile(fairRun(t, 0, 30, false), 0.95)
	fair := percentile(fairRun(t, 4, 30, false), 0.95)
	fifo := percentile(fairRun(t, 4, 30, true), 0.95)
	t.Logf("p95 of a cold 64 KiB read, CDN latency %v, %d in flight: alone %v; beside 4 bulk readers %v with the limiter, %v with a plain bound",
		fairLatency, fairInflight, alone, fair, fifo)
	if fair > 2*alone+fairLatency {
		t.Fatalf("p95 beside bulk readers %v, alone %v: want at most twice alone plus one request", fair, alone)
	}
}

// BenchmarkSmallReadBesideBulk reports the p95 of cold 64 KiB reads alone,
// beside four bulk readers with the limiter, and beside them with a plain
// bound of the same size.
func BenchmarkSmallReadBesideBulk(b *testing.B) {
	var alone, fair, fifo []time.Duration
	for b.Loop() {
		alone = append(alone, fairRun(b, 0, 20, false)...)
		fair = append(fair, fairRun(b, 4, 20, false)...)
		fifo = append(fifo, fairRun(b, 4, 20, true)...)
	}
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	b.ReportMetric(ms(percentile(alone, 0.95)), "p95-alone-ms")
	b.ReportMetric(ms(percentile(fair, 0.95)), "p95-beside-ms")
	b.ReportMetric(ms(percentile(fifo, 0.95)), "p95-fifo-ms")
}
