package watcher

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Stop must never block shutdown indefinitely: with a loop that refuses to
// exit (simulated by a done channel that never closes), Stop returns after
// stopJoinTimeout instead of hanging — and reports the teardown as DIRTY, which
// is what carries "this generation never quiesced" to the lifecycle owner.
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
	done := make(chan error, 1)
	go func() { done <- w.Stop() }()

	select {
	case err := <-done:
		if elapsed := time.Since(start); elapsed < stopJoinTimeout {
			t.Fatalf("Stop returned before the join timeout (%v) — did it wait at all?", elapsed)
		}
		if !errors.Is(err, ErrStopJoinTimeout) {
			t.Fatalf("a join that expired on a hung loop must report a dirty teardown, got %v", err)
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

	if err := w.Stop(); err != nil {
		t.Fatalf("a joined loop must stop cleanly: %v", err)
	}
	if !loopExited {
		t.Fatal("Stop returned before the loop finished — join is not effective")
	}
}

// Stop on a watcher that was never started must not panic or block, and is a
// CLEAN teardown: a generation that never ran has nothing outstanding.
func TestStopWithoutStart(t *testing.T) {
	w := &MinuteWatcher{}
	finished := make(chan error, 1)
	go func() { finished <- w.Stop() }()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("stopping a never-started watcher must be a clean teardown, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop without Start blocked")
	}
}
