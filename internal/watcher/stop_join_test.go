package watcher

import (
	"context"
	"testing"
	"time"
)

// Stop must never block shutdown indefinitely: with a loop that refuses to
// exit (simulated by a done channel that never closes), Stop returns after
// stopJoinTimeout instead of hanging.
func TestStopReturnsDespiteHungLoop(t *testing.T) {
	old := stopJoinTimeout
	stopJoinTimeout = 100 * time.Millisecond
	defer func() { stopJoinTimeout = old }()

	w := &MinuteWatcher{}
	_, cancel := context.WithCancel(context.Background())
	w.mu.Lock()
	w.cancel = cancel
	w.loopDone = make(chan struct{}) // never closed = hung loop
	w.mu.Unlock()

	start := time.Now()
	done := make(chan struct{})
	go func() {
		w.Stop()
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed < stopJoinTimeout {
			t.Fatalf("Stop returned before the join timeout (%v) — did it wait at all?", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked far beyond the join timeout — hung-loop protection missing")
	}
}

// The normal path: Stop waits for the loop to actually exit (join, not just
// cancel), so an in-flight tick's DB write drains before Stop returns.
func TestStopJoinsFinishedLoop(t *testing.T) {
	w := &MinuteWatcher{}
	ctx, cancel := context.WithCancel(context.Background())
	loopExited := false
	done := make(chan struct{})
	w.mu.Lock()
	w.cancel = cancel
	w.loopDone = done
	w.mu.Unlock()

	go func() {
		<-ctx.Done()
		// Simulates the tail of an in-flight tick finishing its DB write
		// after cancellation but before the loop exits.
		time.Sleep(50 * time.Millisecond)
		loopExited = true
		close(done)
	}()

	w.Stop()
	if !loopExited {
		t.Fatal("Stop returned before the loop finished — join is not effective")
	}
}

// Stop on a watcher that was never started must not panic or block.
func TestStopWithoutStart(t *testing.T) {
	w := &MinuteWatcher{}
	finished := make(chan struct{})
	go func() {
		w.Stop()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Stop without Start blocked")
	}
}
