package drops

import (
	"context"
	"testing"
	"time"
)

// Stop must never block shutdown indefinitely: with loops that refuse to
// exit (simulated by a done channel that never closes), Stop returns after
// stopJoinTimeout instead of hanging.
func TestTrackerStopReturnsDespiteHungLoops(t *testing.T) {
	old := stopJoinTimeout
	stopJoinTimeout = 100 * time.Millisecond
	defer func() { stopJoinTimeout = old }()

	d := &DropsTracker{}
	_, cancel := context.WithCancel(context.Background())
	d.mu.Lock()
	d.cancel = cancel
	d.loopsDone = make(chan struct{}) // never closed = hung loops
	d.mu.Unlock()

	start := time.Now()
	finished := make(chan struct{})
	go func() {
		d.Stop()
		close(finished)
	}()

	select {
	case <-finished:
		if elapsed := time.Since(start); elapsed < stopJoinTimeout {
			t.Fatalf("Stop returned before the join timeout (%v) — did it wait at all?", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop blocked far beyond the join timeout — hung-loop protection missing")
	}
}

// The normal path: Stop waits for both loops to actually exit before
// returning, so in-flight catalog/annotation writes drain first.
func TestTrackerStopJoinsFinishedLoops(t *testing.T) {
	d := &DropsTracker{}
	ctx, cancel := context.WithCancel(context.Background())
	loopsExited := false
	done := make(chan struct{})
	d.mu.Lock()
	d.cancel = cancel
	d.loopsDone = done
	d.mu.Unlock()

	go func() {
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		loopsExited = true
		close(done)
	}()

	d.Stop()
	if !loopsExited {
		t.Fatal("Stop returned before the loops finished — join is not effective")
	}
}

// Stop on a tracker that was never started must not panic or block.
func TestTrackerStopWithoutStart(t *testing.T) {
	d := &DropsTracker{}
	finished := make(chan struct{})
	go func() {
		d.Stop()
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("Stop without Start blocked")
	}
}
