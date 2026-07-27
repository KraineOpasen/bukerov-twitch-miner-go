package miner

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestJoinLoopsWaitsForFinishingLoop (S1): joinLoops must join, not just
// return — a loop finishing the tail of its in-flight tick (e.g. a daily
// summary's analytics read) completes before joinLoops returns.
func TestJoinLoopsWaitsForFinishingLoop(t *testing.T) {
	m := &Miner{}
	ctx, cancel := context.WithCancel(context.Background())

	finished := false
	tail := make(chan struct{})
	m.startLoop(ctx, func(ctx context.Context) {
		<-ctx.Done()
		// The tail of an in-flight tick finishing its DB read after
		// cancellation but before the loop exits (channel barrier, not a
		// sleep: joinLoops must wait however long the tail takes).
		<-tail
		finished = true
	})

	cancel()
	close(tail)
	if err := m.joinLoops(); err != nil {
		t.Fatalf("joinLoops: %v", err)
	}

	if !finished {
		t.Fatal("joinLoops returned before the loop finished — join is not effective")
	}
}

// TestJoinLoopsReturnsDespiteHungLoop (S1): the join is bounded — a loop that
// refuses to exit cannot hang shutdown past loopJoinTimeout.
func TestJoinLoopsReturnsDespiteHungLoop(t *testing.T) {
	old := loopJoinTimeout
	loopJoinTimeout = 100 * time.Millisecond
	defer func() { loopJoinTimeout = old }()

	m := &Miner{}
	hang := make(chan struct{})
	defer close(hang) // release the leaked goroutine after the test
	m.startLoop(context.Background(), func(context.Context) {
		<-hang
	})

	start := time.Now()
	done := make(chan error, 1)
	go func() {
		done <- m.joinLoops()
	}()

	select {
	case err := <-done:
		if elapsed := time.Since(start); elapsed < loopJoinTimeout {
			t.Fatalf("joinLoops returned before the join timeout (%v) — did it wait at all?", elapsed)
		}
		if !errors.Is(err, errLoopJoinTimeout) {
			t.Fatalf("joinLoops on a hung loop = %v, want errLoopJoinTimeout — the timeout must be an explicit shutdown error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("joinLoops blocked far beyond the join timeout — hung-loop protection missing")
	}
}

// TestJoinLoopsWithoutStartMining (S1): a miner whose startMining never ran
// (struct-literal test miners, early-exit paths) joins an empty set and
// returns immediately without panicking.
func TestJoinLoopsWithoutStartMining(t *testing.T) {
	m := &Miner{}
	done := make(chan error, 1)
	go func() {
		done <- m.joinLoops()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("joinLoops without startMining = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("joinLoops without startMining blocked")
	}
}
