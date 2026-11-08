package cdn

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

// grantOrder queues waiters on a full limiter and records the order they
// are let through as slots free one at a time.
func grantOrder(t *testing.T, l *Limiter, held []func(), owners []string) []string {
	t.Helper()
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	for i, o := range owners {
		wg.Go(func() {
			release, err := l.Acquire(context.Background(), o)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, o)
			mu.Unlock()
			release()
		})
		// Let each waiter queue before the next, so arrival order is known.
		for l.Stats().Queued < i+1 {
			time.Sleep(time.Millisecond)
		}
	}
	for _, r := range held {
		r()
	}
	wg.Wait()
	return order
}

func TestLimiterTakesTurnsByOwner(t *testing.T) {
	l := NewLimiter(1)
	release, err := l.Acquire(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	got := grantOrder(t, l, []func(){release}, []string{"bulk", "bulk", "bulk", "cat", "bulk"})
	if want := []string{"bulk", "cat", "bulk", "bulk", "bulk"}; !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v: the second owner waits for one turn, not the whole queue", got, want)
	}
}

// Prefetches hold at most three quarters of the slots and go after every
// waiting read.
func TestLimiterReadsBeforePrefetch(t *testing.T) {
	l := NewLimiter(4)
	block := make(chan struct{})
	started := make(chan struct{}, 8)
	for range 8 {
		l.Go("bulk", func() {
			started <- struct{}{}
			<-block
		})
	}
	for range 3 {
		<-started
	}
	if st := l.Stats(); st.Inflight != 3 || st.QueuedPrefetch != 5 {
		t.Fatalf("stats %+v: want 3 prefetches in flight, 5 queued", st)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := l.Acquire(ctx, "cat")
	if err != nil {
		t.Fatalf("a read waited behind prefetches: %v", err)
	}
	release()
	close(block)
}

func TestLimiterCancelledWaiterLeaves(t *testing.T) {
	l := NewLimiter(1)
	release, _ := l.Acquire(context.Background(), "a")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() {
		_, err := l.Acquire(ctx, "b")
		done <- err
	}()
	for l.Stats().Queued == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v", err)
	}
	if st := l.Stats(); st.Queued != 0 || st.Inflight != 1 {
		t.Fatalf("stats after cancel %+v", st)
	}
	release()
	if st := l.Stats(); st.Inflight != 0 {
		t.Fatalf("slot not freed: %+v", st)
	}
}

func TestLimiterDropsPrefetchBeyondQueue(t *testing.T) {
	l := NewLimiter(1)
	release, _ := l.Acquire(context.Background(), "a")
	for range maxBackground {
		if !l.Go("p", func() {}) {
			t.Fatal("prefetch dropped before the queue is full")
		}
	}
	if l.Go("p", func() {}) || l.Stats().Dropped != 1 {
		t.Fatalf("stats %+v: want the prefetch past the queue dropped", l.Stats())
	}
	release()
}
