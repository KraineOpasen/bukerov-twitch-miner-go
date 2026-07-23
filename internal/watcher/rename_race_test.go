package watcher

import (
	"sync"
	"testing"
)

// TestPublishDebugState_RaceSafeAgainstConcurrentRename is a regression test
// for the BKM-006 rename-reconciliation race gate: a config-driven rename
// commits streamer.Manager.reconcile via *models.Streamer.RenameIfCurrent,
// which mutates Username under s.mu WITHOUT going through
// watcher.UpdateStreamers for a pure rename (added/removed both empty in
// miner.ApplySettings) - so live watcher goroutines can observe the mutation
// with no happens-before edge unless every reader uses GetUsername().
//
// publishDebugState is a real, unmodified production reader (called every
// watch tick via processWatching, and driven here through the exact same
// method) that used to read s.Username directly; it now reads it via
// GetUsername(). This test drives that SAME reader concurrently, on a live
// goroutine, against a goroutine hammering RenameIfCurrent on the very
// streamer object being read - exactly the concurrency pattern the review
// gate flagged. Pre-fix (raw field read) this fails under `go test -race`;
// post-fix (GetUsername(), which takes s.mu.RLock) it passes.
//
// Synchronization is entirely WaitGroup/channel based (a closed start
// barrier + wg.Wait for completion) - no time.Sleep - so the test is
// deterministic: it always waits for both goroutines to run their full
// iteration count before asserting anything, and the race detector's
// vector-clock tracking needs only one true unsynchronized overlap (all but
// guaranteed across thousands of unsynchronized iterations) to flag a
// pre-fix regression, so it never depends on scheduler luck to catch one.
func TestPublishDebugState_RaceSafeAgainstConcurrentRename(t *testing.T) {
	const iterations = 5000

	w, _ := newTestWatcher(2)
	for _, s := range w.streamers {
		s.SetConfirmedOnline()
	}

	var wg sync.WaitGroup
	start := make(chan struct{})
	wg.Add(2)

	// Reader goroutine: repeatedly runs the exact production path a live
	// watch tick uses to snapshot every streamer's login for the debug
	// endpoint. ModeIdle keeps it off the ActivePair/PostponedSwapOuts branch
	// (rotation-only), so every iteration exercises the per-streamer
	// Decisions loop - the read this regression targets.
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < iterations; i++ {
			w.publishDebugState(nil, ModeIdle)
		}
	}()

	// Writer goroutine: repeatedly renames streamer[0] in place, mirroring
	// streamer.Manager.reconcile's RenameIfCurrent call on a confirmed
	// config-driven rename. Alternating between two logins keeps every call a
	// genuine mutation (never a no-op write).
	go func() {
		defer wg.Done()
		<-start
		logins := [2]string{"renamed-a", "renamed-b"}
		for i := 0; i < iterations; i++ {
			obs := w.streamers[0].BeginLoginObservation()
			w.streamers[0].RenameIfCurrent(logins[i%2], obs)
		}
	}()

	close(start)
	wg.Wait()

	// Sanity check: the writer goroutine really ran and really mutated the
	// streamer being read, so a passing race detector run above is evidence
	// of correct synchronization, not of the rename loop having silently
	// done nothing.
	final := w.streamers[0].GetUsername()
	if final != "renamed-a" && final != "renamed-b" {
		t.Fatalf("streamer[0] login = %q, want one of the renamed logins (rename loop did not take effect)", final)
	}
}
