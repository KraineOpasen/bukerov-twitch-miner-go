package lifecycle

import (
	"context"
	"testing"
)

// This file exercises design v6 §5.1's "starting" row cell-for-cell, using
// readySignalingRunner fakes that stay not-ready until the test explicitly
// calls markReady() — modeling F4's real device-code/startup-retry phase,
// where the Runner can run for an unbounded time before settling.

// newStartingController builds a Controller whose persisted desired state
// is "running" (missing row -> back-compat running) with a
// readySignaling factory, starts it in the background, and waits until the
// first generation is launched and genuinely stuck at observed=starting
// (slot-free: this is a reconcile-driven start, so pc==nil — design v6
// §5.2's "Retry scheduled... с пустым слотом" applies equally to boot).
func newStartingController(t *testing.T) (*Controller, *runnerFactory, context.CancelFunc, <-chan error) {
	t.Helper()
	rf := newRunnerFactory()
	rf.readySignaling = true
	cfg := Config{
		Factory:     rf.Factory,
		Persistence: newFakePersistence(LoadResult{Found: false}),
		Clock:       newFakeClock(),
	}
	c := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedStarting)
	return c, rf, cancel, done
}

// (22, rewritten per orchestrator review item 1) Cancelling a start via
// Pause while it is still awaiting readiness (F4's infinite device-code/
// startup-retry phase) lands through the "starting" row's slot-free
// cancel-start cell: the generation's ctx is cancelled, its actual return
// is awaited, and the resulting "context canceled" is classified per
// §5.2.6 — an EXPECTED event, NOT LastError, NOT failed, NOT retried.
func TestCancelDuringInfiniteInternalRetryIsNotAFailure(t *testing.T) {
	c, rf, cancel, done := newStartingController(t)
	defer cancel()

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause during starting (slot-free): %v (%v)", res.Outcome, res.Err)
	}
	waitObserved(t, c, ObservedPaused)

	snap := c.Snapshot()
	if snap.LastError != "" {
		t.Fatalf("LastError = %q, want empty (cancellation is expected)", snap.LastError)
	}
	if snap.Reason != ReasonUser {
		t.Fatalf("reason = %v, want user", snap.Reason)
	}
	if !snap.NextRetryAt.IsZero() {
		t.Fatal("no retry should be scheduled after cancelling a start")
	}
	if snap.Desired != DesiredPaused {
		t.Fatalf("desired = %v, want paused", snap.Desired)
	}
	if rf.count() != 1 {
		t.Fatalf("cancelling a start must not build a new generation, got %d", rf.count())
	}

	cancel()
	<-done
}

// "starting" row, slot-free: stop -> A: cancel start -> stopped.
func TestStartingRowSlotFreeStopCancelsToStopped(t *testing.T) {
	c, rf, cancel, done := newStartingController(t)
	defer cancel()

	res := c.Stop(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("stop during starting (slot-free): %v (%v)", res.Outcome, res.Err)
	}
	waitObserved(t, c, ObservedStopped)

	snap := c.Snapshot()
	if snap.LastError != "" {
		t.Fatalf("LastError = %q, want empty (cancellation is expected)", snap.LastError)
	}
	if snap.Reason != ReasonUser {
		t.Fatalf("reason = %v, want user", snap.Reason)
	}
	if rf.count() != 1 {
		t.Fatalf("cancelling a start must not build a new generation, got %d", rf.count())
	}

	cancel()
	<-done
}

// "starting" row, slot-free: resume -> I (idempotent, desired already
// running).
func TestStartingRowSlotFreeResumeIsIdempotent(t *testing.T) {
	c, rf, cancel, done := newStartingController(t)
	defer cancel()

	res := c.Resume(context.Background())
	if res.Outcome != OutcomeIdempotent {
		t.Fatalf("resume during starting (slot-free): got %v, want idempotent", res.Outcome)
	}
	if rf.count() != 1 {
		t.Fatalf("an idempotent resume must not build a new generation, got %d", rf.count())
	}

	// Let the original start settle normally, proving the idempotent
	// resume didn't disturb it.
	rf.at(0).markReady()
	waitObserved(t, c, ObservedRunning)

	cancel()
	<-done
}

// "starting" row, slot-free: restart -> R (rejected).
func TestStartingRowSlotFreeRestartRejected(t *testing.T) {
	c, rf, cancel, done := newStartingController(t)
	defer cancel()

	res := c.Restart(context.Background())
	if res.Outcome != OutcomeRejected {
		t.Fatalf("restart during starting (slot-free): got %v, want rejected", res.Outcome)
	}
	if rf.count() != 1 {
		t.Fatalf("a rejected restart must not build a new generation, got %d", rf.count())
	}

	rf.at(0).markReady()
	waitObserved(t, c, ObservedRunning)

	cancel()
	<-done
}

// "starting" row, slot-HELD: a resume command occupies the slot (started
// from paused), so pause/stop/restart are all rejected via the slot — the
// exact same "second submit -> 409" rule pausing/stopping/restarting
// already get, since the slot stays occupied until THIS command's own
// terminal (running, once ready) is reached.
func TestStartingRowSlotHeldRejectsOtherCommands(t *testing.T) {
	rf := newRunnerFactory()
	rf.readySignaling = true
	c, cancel, done := buildAndRun(Config{
		Factory:     rf.Factory,
		Persistence: newFakePersistence(LoadResult{Found: true, Desired: DesiredPaused}),
		Clock:       newFakeClock(),
	})
	defer cancel()
	waitObserved(t, c, ObservedPaused)

	res := c.Resume(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("resume from paused: %v (%v)", res.Outcome, res.Err)
	}
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedStarting)

	if r := c.Pause(context.Background()); r.Outcome != OutcomeRejected {
		t.Fatalf("pause while slot-held starting: got %v, want rejected", r.Outcome)
	}
	if r := c.Stop(context.Background()); r.Outcome != OutcomeRejected {
		t.Fatalf("stop while slot-held starting: got %v, want rejected", r.Outcome)
	}
	if r := c.Restart(context.Background()); r.Outcome != OutcomeRejected {
		t.Fatalf("restart while slot-held starting: got %v, want rejected", r.Outcome)
	}
	// A duplicate resume is still idempotent.
	if r := c.Resume(context.Background()); r.Outcome != OutcomeIdempotent {
		t.Fatalf("duplicate resume while slot-held starting: got %v, want idempotent", r.Outcome)
	}

	rf.at(0).markReady()
	waitObserved(t, c, ObservedRunning)
	if c.Snapshot().CommandID == "" {
		t.Fatal("the resume command's own id should be reported once it reaches its terminal")
	}

	cancel()
	<-done
}

// A generation that dies BEFORE becoming ready while a resume/restart
// command holds the slot classifies exactly like any other startup
// failure — target=running, so failed+retry, unchanged from the
// slot-free case (design v6: "slot-held -> classify by slot target
// [unchanged]").
func TestStartingRowSlotHeldDeathBeforeReadyIsStartupFailure(t *testing.T) {
	rf := newRunnerFactory()
	rf.readySignaling = true
	c, cancel, done := buildAndRun(Config{
		Factory:     rf.Factory,
		Persistence: newFakePersistence(LoadResult{Found: true, Desired: DesiredPaused}),
		Clock:       newFakeClock(),
	})
	defer cancel()
	waitObserved(t, c, ObservedPaused)

	res := c.Resume(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("resume from paused: %v (%v)", res.Outcome, res.Err)
	}
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedStarting)

	rf.at(0).finish(nil) // dies before ever becoming ready
	snap := waitFailedWithRetryScheduled(t, c)
	if snap.Reason != ReasonStartupFailure {
		t.Fatalf("reason = %v, want startup-failure", snap.Reason)
	}

	cancel()
	<-done
}
