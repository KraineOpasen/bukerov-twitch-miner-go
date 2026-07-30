package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
)

// (30) K retry cycles (K far larger than the shared ring's capacity, 200)
// produce a bounded, small number of lifecycle_failed_entered ring records
// (design v6 §11's dedup: a whole crash-loop is ONE episode for ring
// purposes), and a mining event seeded before the storm is still present
// afterward — lifecycle events must never evict mining events from the
// shared ring.
func TestRingBudgetBoundedAcrossRetryStorm(t *testing.T) {
	// Unique markers so this run's own events can be counted precisely even
	// though the ring is a process-wide singleton shared with every other
	// test (including this same test re-run under -count=N) — counting a
	// bare Type across the whole process would conflate this storm's
	// contribution with unrelated runs' and fail spuriously.
	runID := time.Now().UnixNano()
	seedMarker := fmt.Sprintf("test-seed-%d", runID)
	failMarker := fmt.Sprintf("boom-%d", runID)
	events.Record(events.TypeBonusClaimed, "seed-streamer", seedMarker)

	orig := RetryBackoffSchedule
	RetryBackoffSchedule = []time.Duration{time.Millisecond}
	t.Cleanup(func() { RetryBackoffSchedule = orig })

	rf := newRunnerFactory()
	rf.readySignaling = true // never marked ready — each attempt fails
	// before ready, so recovery is never spuriously declared partway
	// through and the dedup rule is exercised against a genuine,
	// uninterrupted crash-loop (design v6 item 12).
	clock := newFakeClock()
	c, cancel, done := buildAndRun(Config{
		Factory:     rf.Factory,
		Persistence: newFakePersistence(LoadResult{Found: false}),
		Clock:       clock,
	})
	defer cancel()

	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedStarting)

	const k = 300 // far larger than the ring's fixed capacity (200)
	for i := 0; i < k; i++ {
		rf.at(i).finish(errors.New(failMarker))
		waitObserved(t, c, ObservedFailed)
		waitFor(t, 2*time.Second, func() bool { return clock.pendingCount() > 0 })
		clock.fireNext()
		rf.waitCount(t, i+2)
		rf.at(i + 1).waitStarted(t)
		waitObserved(t, c, ObservedStarting)
	}

	recent := events.Recent(200)
	lifecycleFailedCount := 0
	seedFound := false
	for _, e := range recent {
		if e.Type == events.TypeLifecycleFailedEntered && e.Detail == failMarker {
			lifecycleFailedCount++
		}
		if e.Detail == seedMarker {
			seedFound = true
		}
	}
	// The dedup rule (design v6 §11) records lifecycle_failed_entered only
	// on the FIRST failure of a streak — so this storm's own contribution
	// must be exactly 1, no matter how many of the k retries happened.
	if lifecycleFailedCount != 1 {
		t.Fatalf("expected exactly 1 lifecycle_failed_entered record attributable to this %d-retry storm (dedup), got %d", k, lifecycleFailedCount)
	}
	if !seedFound {
		t.Fatal("a mining event seeded before the retry storm was evicted from the ring — lifecycle events must never evict mining events")
	}

	cancel()
	<-done
}

// TestRecordRingUsesEmptyStreamer confirms lifecycle ring events are
// recorded with an empty Streamer (design v6 §11: process-level facts, not
// per-streamer) — the exact contract handlers_overview.go relies on to
// keep these invisible to the Ф1-Ф3 per-streamer UI without any web-layer
// changes in b1.
func TestRecordRingUsesEmptyStreamer(t *testing.T) {
	rf := newRunnerFactory()
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}
	waitObserved(t, c, ObservedPaused)

	found := false
	for _, e := range events.Recent(20) {
		if e.Type == events.TypeLifecyclePaused {
			found = true
			if e.Streamer != "" {
				t.Fatalf("lifecycle_paused recorded with non-empty Streamer %q", e.Streamer)
			}
		}
	}
	if !found {
		t.Fatal("expected a lifecycle_paused ring event")
	}

	cancel()
	<-done
}
