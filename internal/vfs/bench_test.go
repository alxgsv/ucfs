package vfs_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/alxgsv/ucfs/internal/cache"
	"github.com/alxgsv/ucfs/internal/fake"
	"github.com/alxgsv/ucfs/internal/vfs"
	"github.com/alxgsv/ucfs/internal/vfs/vfstest"
)

// benchLatency stands for the CDN's time to first byte, which dominates a
// cold read; the fake serves bytes at loopback speed after it.
const benchLatency = 20 * time.Millisecond

// BenchmarkColdCat5MB reads a 5 MB file from start to end with nothing
// cached, in the 1 MiB reads the NFS client sends.
func BenchmarkColdCat5MB(b *testing.B) {
	const size = 5_000_000
	env := vfstest.New(b, file(1, "five.jpg", size))
	env.Fake.SetLatency(uuid(1), benchLatency)
	ctx := context.Background()
	n := walk(b, env, "files/"+uuid(1)+"_five.jpg").Node
	p := make([]byte, cache.BlockSize)
	b.SetBytes(size)
	for b.Loop() {
		b.StopTimer()
		env.Cache.Wait()
		env.Cache.Drop(cache.Key{UUID: uuid(1)})
		b.StartTimer()
		for off := int64(0); ; off += int64(len(p)) {
			if _, err := env.FS.ReadAt(ctx, n, p, off); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkLsL10000 lists files/ with 10,000 entries and the attributes of
// each, what ls -l asks of the filesystem; all of it comes from the index.
func BenchmarkLsL10000(b *testing.B) {
	files := make([]fake.File, 10_000)
	for i := range files {
		files[i] = file(i+1, fmt.Sprintf("photo-%05d.jpg", i), int64(1000+i))
	}
	env := vfstest.New(b, files...)
	ctx := context.Background()
	dir := walk(b, env, "files").Node
	for b.Loop() {
		entries, err := env.FS.ReadDir(ctx, dir)
		if err != nil || len(entries) != len(files) {
			b.Fatalf("%d entries, %v", len(entries), err)
		}
		for _, e := range entries {
			_ = env.FS.ID(e.Node)
			if _, err := env.FS.Getattr(ctx, e.Node); err != nil {
				b.Fatal(err)
			}
		}
	}
}

// BenchmarkVariantReaders times cold reads of small variants, one after
// another, alone and while a second reader streams a large variant, and
// reports the p95 of each; a fair queue keeps the two close.
func BenchmarkVariantReaders(b *testing.B) {
	big := file(1, "big.jpg", 64<<20)
	small := file(2, "small.jpg", 400<<10)
	env := vfstest.New(b, big, small)
	env.Fake.SetLatency(big.UUID, benchLatency)
	env.Fake.SetLatency(small.UUID, benchLatency)
	ctx := context.Background()
	round := 0
	read := func(n vfs.Node) time.Duration {
		start := time.Now()
		p := make([]byte, 256<<10)
		for off := int64(0); ; off += int64(len(p)) {
			if _, err := env.FS.ReadAt(ctx, n, p, off); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				b.Fatal(err)
			}
		}
		return time.Since(start)
	}
	smallReads := func() []time.Duration {
		var lat []time.Duration
		for range 20 {
			round++
			lat = append(lat, read(vfs.Node{Kind: vfs.KindVariant, UUID: small.UUID, Ops: fmt.Sprintf("-/resize/%dx/", round)}))
		}
		return lat
	}
	var alone, beside []time.Duration
	for b.Loop() {
		alone = append(alone, smallReads()...)
		stop := make(chan struct{})
		var wg sync.WaitGroup
		wg.Go(func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				read(vfs.Node{Kind: vfs.KindVariant, UUID: big.UUID, Ops: fmt.Sprintf("-/resize/%dx/-/quality/best/", i+round)})
			}
		})
		time.Sleep(5 * benchLatency)
		beside = append(beside, smallReads()...)
		close(stop)
		wg.Wait()
		env.Cache.Wait()
	}
	b.ReportMetric(p95ms(alone), "p95-alone-ms")
	b.ReportMetric(p95ms(beside), "p95-beside-ms")
}

func p95ms(d []time.Duration) float64 {
	s := slices.Clone(d)
	slices.Sort(s)
	return float64(s[min(len(s)-1, len(s)*95/100)]) / float64(time.Millisecond)
}
