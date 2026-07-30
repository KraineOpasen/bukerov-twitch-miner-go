package lifecycle

import (
	"context"
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
	// The ring is a process-wide singleton shared with every other test in
	// this binary, INCLUDING this same test re-run many times under
	// -count=N — a bounded Recent(N) COUNT delta ("before" vs "after")
	// is unsound here even wrapped in a poll: once enough consecutive
	// iterations of THIS SAME test have run, the ring's fixed window fills
	// with an alternating lifecycle_failed_entered/lifecycle_failed_recovered
	// pattern that is entirely this test's own history, so each new
	// iteration's pair (one of each type) evicts an old pair of the same
	// two types from the tail — the COUNT of lifecycle_failed_recovered
	// within the window stays flat, and "after - before" never becomes 1
	// even though a brand-new event was genuinely recorded (reproduced
	// deterministically at -count=100: every iteration timed out polling
	// for delta==1). Recent(N) is a bounded window; use each Event's own
	// Time field instead — set at Record() — to identify the recovery
	// event unambiguously by "recorded at or after this test's own marker",
	// which stays correct regardless of how saturated the window is with
	// this test's own prior iterations.
	marker := time.Now()
	recoveredSinceMarker := func() int {
		n := 0
		for _, e := range events.Recent(200) {
			if e.Type == events.TypeLifecycleFailedRecovered && !e.Time.Before(marker) {
				n++
			}
		}
		return n
	}

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

	// completeStart publishes ObservedRunning (publishTerminal/
	// publishTerminalNoSlot) BEFORE maybeDeclareRecovery records the ring
	// event — two sequential, non-atomic worker-goroutine calls with no
	// channel/lock in between (the same class of gap
	// waitFailedWithRetryScheduled's own doc comment describes for
	// observed=failed/NextRetryAt). A bare check taken immediately after
	// waitObserved(running) can therefore win the race and look before the
	// worker has actually written to the ring (found via an independent
	// verification pass's own -count=20 stress run). Poll instead of
	// asserting immediately; a short settle re-check afterward confirms
	// recovery is declared exactly once, never twice.
	waitFor(t, 2*time.Second, func() bool { return recoveredSinceMarker() == 1 })
	time.Sleep(20 * time.Millisecond)
	if got := recoveredSinceMarker(); got != 1 {
		t.Fatalf("expected exactly 1 lifecycle_failed_recovered ring event (timestamped at/after this test's own marker) once the retried generation became ready, got %d", got)
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

// MAJOR 5 (F4b Q3 consolidated corrective): a persist failure for a command
// that raced out of "failed" (accept() cancels the currently-armed retry
// the instant it occupies the slot, design v6 §5.2 step 1) must not
// permanently disarm design v6 §5.3's automatic retry chain — the worker
// re-arms it (rearmRetryCh) using the CURRENT retryAttempt, and a
// subsequent (fake-clock) fire still launches a generation.
func TestPersistFailureOutOfFailedReArmsRetryChain(t *testing.T) {
	rf := newRunnerFactory()
	clock := newFakeClock()
	pers := newFakePersistence(LoadResult{Found: false}) // missing row -> running
	c, cancel, done := buildAndRun(Config{
		Factory:     rf.Factory,
		Persistence: pers,
		Clock:       clock,
	})
	defer cancel()

	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedRunning)

	rf.at(0).finish(errors.New("boom"))
	failedSnap := waitFailedWithRetryScheduled(t, c)
	if failedSnap.NextRetryAt.IsZero() {
		t.Fatal("precondition: a retry must be armed after the first startup failure")
	}

	// A resume, raced out of "failed", whose OWN persist fails: accept()
	// already cancelled the just-armed retry as part of occupying the
	// slot; the failing Save must not let that cancellation stick.
	wantErr := errors.New("db is busy")
	pers.setSaveErr(wantErr)
	res := c.Resume(context.Background())
	if res.Outcome != OutcomeRejected {
		t.Fatalf("resume with failing persist: got %v, want rejected", res.Outcome)
	}
	if !errors.Is(res.Err, wantErr) {
		t.Fatalf("resume error = %v, want wrapping %v", res.Err, wantErr)
	}

	waitFor(t, 2*time.Second, func() bool { return !c.Snapshot().NextRetryAt.IsZero() })
	reArmedSnap := c.Snapshot()
	if reArmedSnap.Observed != ObservedFailed {
		t.Fatalf("observed = %v, want still failed", reArmedSnap.Observed)
	}

	// The re-armed retry actually still fires and launches a generation —
	// the chain genuinely survived, not just NextRetryAt cosmetically
	// looking set.
	pers.setSaveErr(nil)
	waitFor(t, 2*time.Second, func() bool { return clock.pendingCount() > 0 })
	if !clock.fireNext() {
		t.Fatal("expected the re-armed retry timer to actually be pending")
	}
	rf.waitCount(t, 2)
	waitObserved(t, c, ObservedRunning)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
