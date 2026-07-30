package lifecycle

import (
	"errors"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
)

// (21) A generation that fails immediately (Run returns an error while
// desired=running) is classified as observed=failed, with LastError set and
// a retry scheduled (NextRetryAt in the future). This uses a plain
// (non-ReadySignaler) fake — production's current Miner adapter has no
// readiness signal either, and its "reaches ready" is instant, identical to
// its launch.
func TestStartupFailureSchedulesRetry(t *testing.T) {
	rf := newRunnerFactory()
	clock := newFakeClock()
	c, cancel, done := buildAndRun(Config{
		Factory:     rf.Factory,
		Persistence: newFakePersistence(LoadResult{Found: false}),
		Clock:       clock,
	})
	defer cancel()

	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedRunning)

	wantErr := errors.New("auth rejected")
	rf.at(0).finish(wantErr)
	snap := waitFailedWithRetryScheduled(t, c)
	if snap.LastError == "" {
		t.Fatal("LastError should describe the startup failure")
	}
	if snap.Reason != ReasonStartupFailure {
		t.Fatalf("reason = %v, want startup-failure", snap.Reason)
	}

	cancel()
	<-done
}

// (23) At least 3 consecutive automatic retries occur with strictly
// growing delays, following RetryBackoffSchedule (shrunk here to
// well-separated values so ±20% jitter can never make them overlap). The
// fake never becomes ready — modeling F4's real device-code/startup-retry
// phase, where the Runner can keep failing before it ever settles — so
// recovery must never be spuriously declared partway through (design v6
// item 12: the backoff streak must survive the whole crash-loop).
func TestConsecutiveRetriesGrowBackoffDelay(t *testing.T) {
	orig := RetryBackoffSchedule
	RetryBackoffSchedule = []time.Duration{10 * time.Millisecond, 50 * time.Millisecond, 200 * time.Millisecond}
	t.Cleanup(func() { RetryBackoffSchedule = orig })

	rf := newRunnerFactory()
	rf.readySignaling = true
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

	var delays []time.Duration
	for i := 0; i < 3; i++ {
		rf.at(i).finish(errors.New("boom")) // dies before ever becoming ready
		snap := waitFailedWithRetryScheduled(t, c)
		delays = append(delays, snap.NextRetryAt.Sub(clock.Now()))

		waitFor(t, time.Second, func() bool { return clock.pendingCount() > 0 })
		clock.fireNext()
		rf.waitCount(t, i+2)
		rf.at(i + 1).waitStarted(t)
		waitObserved(t, c, ObservedStarting) // still awaiting ready
	}

	for i := 1; i < len(delays); i++ {
		if delays[i] <= delays[i-1] {
			t.Fatalf("retry delays did not grow: %v", delays)
		}
	}

	cancel()
	<-done
}

// (24) Once an automatic retry produces a generation that actually becomes
// READY, the lineage is proven healthy: the retry-attempt counter resets
// and lifecycle_failed_recovered is recorded immediately — no operator
// action needed (design v6 revision #2(a): "ready reached" is recovery's
// natural trigger).
func TestRetryRecoveryEmitsFailedRecovered(t *testing.T) {
	rf := newRunnerFactory()
	rf.readySignaling = true
	clock := newFakeClock()
	shrinkRetrySchedule(t)

	c, cancel, done := buildAndRun(Config{
		Factory:     rf.Factory,
		Persistence: newFakePersistence(LoadResult{Found: false}),
		Clock:       clock,
	})
	defer cancel()

	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedStarting)

	rf.at(0).finish(errors.New("boom"))
	waitObserved(t, c, ObservedFailed)

	waitFor(t, time.Second, func() bool { return clock.pendingCount() > 0 })
	clock.fireNext()
	rf.waitCount(t, 2)
	rf.at(1).waitStarted(t)
	waitObserved(t, c, ObservedStarting)

	// This retry attempt actually becomes ready.
	rf.at(1).markReady()
	waitObserved(t, c, ObservedRunning)

	found := false
	for _, e := range events.Recent(50) {
		if e.Type == events.TypeLifecycleFailedRecovered {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected a lifecycle_failed_recovered ring event once the retried generation became ready")
	}

	cancel()
	<-done
}

// (24b) Recovery is NOT declared just because a retried generation dies
// before it ever became ready — the attempt counter must keep growing
// across the whole crash-loop (design v6 revision #2: "failure before
// ready keeps the streak and grows backoff").
func TestNotYetReadyDeathDoesNotDeclareRecovery(t *testing.T) {
	orig := RetryBackoffSchedule
	RetryBackoffSchedule = []time.Duration{10 * time.Millisecond, 200 * time.Millisecond}
	t.Cleanup(func() { RetryBackoffSchedule = orig })

	rf := newRunnerFactory()
	rf.readySignaling = true
	clock := newFakeClock()

	c, cancel, done := buildAndRun(Config{
		Factory:     rf.Factory,
		Persistence: newFakePersistence(LoadResult{Found: false}),
		Clock:       clock,
	})
	defer cancel()

	// The ring is a process-wide singleton shared with every other test, so
	// "recovery was declared" must be checked as a DELTA against a
	// baseline taken right here — an absolute "none present" check would
	// spuriously fail if an earlier, unrelated test's own legitimate
	// recovery event is still within the ring's recent window.
	recoveredCount := func() int {
		n := 0
		for _, e := range events.Recent(200) {
			if e.Type == events.TypeLifecycleFailedRecovered {
				n++
			}
		}
		return n
	}
	baseline := recoveredCount()

	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedStarting)

	rf.at(0).finish(errors.New("boom-1")) // dies before ever becoming ready
	firstSnap := waitFailedWithRetryScheduled(t, c)
	firstDelay := firstSnap.NextRetryAt.Sub(clock.Now())

	if got := recoveredCount(); got != baseline {
		t.Fatalf("a not-yet-ready death must not declare recovery (lifecycle_failed_recovered count %d -> %d)", baseline, got)
	}

	waitFor(t, time.Second, func() bool { return clock.pendingCount() > 0 })
	clock.fireNext()
	rf.waitCount(t, 2)
	rf.at(1).waitStarted(t)
	waitObserved(t, c, ObservedStarting)

	rf.at(1).finish(errors.New("boom-2")) // dies again, still before ready
	secondSnap := waitFailedWithRetryScheduled(t, c)
	secondDelay := secondSnap.NextRetryAt.Sub(clock.Now())

	if secondDelay <= firstDelay {
		t.Fatalf("backoff did not grow across consecutive not-yet-ready deaths: first=%v second=%v", firstDelay, secondDelay)
	}

	cancel()
	<-done
}
