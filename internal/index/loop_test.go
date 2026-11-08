package index

import (
	"context"
	"testing"
	"time"
)

// While the API refuses, syncs back off exponentially instead of running
// every interval, and the first success ends the streak.
func TestLoopBacksOff(t *testing.T) {
	srv, client, ix := setup(t)
	srv.Add(files(0, 3)...)
	if _, err := ix.Sync(context.Background(), client, false); err != nil {
		t.Fatal(err)
	}
	before := srv.Requests()
	srv.DenyREST(true)
	loop := &Loop{Index: ix, Source: client, Interval: 10 * time.Millisecond, SweepEvery: time.Hour, Changed: func() {}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		loop.Run(ctx)
		close(done)
	}()
	time.Sleep(330 * time.Millisecond)
	// Unthrottled, 33 syncs; backing off from 20 ms: at 0, 20, 60, 140, 300 ms.
	refused := srv.Requests() - before
	if refused < 3 || refused > 6 {
		t.Fatalf("%d syncs in 330 ms while refused, want about 5", refused)
	}
	st := loop.Status()
	if st.Failures != refused || st.Err == nil || !st.Next.After(st.Last) {
		t.Fatalf("status %+v after %d refused syncs", st, refused)
	}
	srv.DenyREST(false)
	srv.Add(files(3, 1)...)
	deadline := time.Now().Add(2 * time.Second)
	for loop.Status().Failures > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st := loop.Status(); st.Failures != 0 || st.Err != nil {
		t.Fatalf("status %+v once the API answers", st)
	}
	cancel()
	<-done
	if s, _ := ix.Stats(context.Background()); s.Files != 4 {
		t.Fatalf("%d files after recovery, want 4", s.Files)
	}
}
