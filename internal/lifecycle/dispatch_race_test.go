package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"
)

// This file exercises the orchestrator's concurrency review of worker.go:
// two real races in the "accepted but not yet dequeued" window.
//
// Defect 1: a command is accepted (slot occupied, pc pushed to cmdCh) at
// the same moment its target generation's completion becomes independently
// available on doneCh. Both channels are ready; Go's select has no
// ordering between them. If doneCh wins, the spontaneous classification
// resolves (and releases) the slot — but the SAME pc is still sitting,
// un-dequeued, in cmdCh. Without dispatch-time validation, the next loop
// iteration would dequeue and dispatch it anyway: for a teardown-class
// action that deadlocks forever awaiting a completion already consumed;
// for a start-class action that double-starts a generation (I1
// violation). The fix (isCurrentPending, in worker.go's cmdCh case) must
// make BOTH possible orderings converge to the same correct, safe outcome.
//
// Defect 2: a phantom (slot-free) not-ready start races an accepted
// cancel-start the same way; without classifying by the SLOT's target
// first, a stale desired read could publish "failed" instead of the
// cancel's own "paused"/"stopped".
//
// Every test here uses the testBeforeSelect seam to construct the race
// deterministically (both channels provably ready before the worker's
// select ever runs), and relies on `go test -race -count=20` to exercise
// both of Go's pseudo-random select orderings — assertions are written to
// hold regardless of which one actually happened.

// armOnce installs a testBeforeSelect hook that pauses the worker exactly
// once, at its Nth call (1-based), rendezvousing on enter/resumeCh; every
// other call is an instant no-op passthrough.
func armOnce(c *Controller, n int) (enter <-chan struct{}, resumeFn func()) {
	callN := 0
	enterCh := make(chan struct{})
	resumeCh := make(chan struct{})
	c.testBeforeSelect = func() {
		callN++
		if callN != n {
			return
		}
		enterCh <- struct{}{}
		<-resumeCh
	}
	return enterCh, func() { resumeCh <- struct{}{} }
}

// (13a) Pause is accepted (slot occupied) at the same instant its target
// generation's completion becomes independently available — both cmdCh and
// doneCh ready together. Regardless of which one Go's select picks: exactly
// one terminal publish (paused), no retry scheduled, the slot released
// exactly once, and — the deadlock detector — the controller remains fully
// responsive afterward (a subsequent Resume is accepted and actually runs).
func TestPauseAcceptedRacesSpontaneousDeath(t *testing.T) {
	rf := newRunnerFactory()
	c := New(Config{
		Factory:     rf.Factory,
		Persistence: newFakePersistence(LoadResult{Found: false}),
		Clock:       newFakeClock(),
	})

	// Hold the worker at the TOP of its very first loop iteration:
	// reconciliation (before the loop) has already started generation 1,
	// but nothing has been dequeued from any channel yet.
	enter, release := armOnce(c, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)
	defer cancel()

	<-enter
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	if got := c.Snapshot().Observed; got != ObservedRunning {
		t.Fatalf("observed before pausing the worker = %v, want running", got)
	}

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}
	// Make the generation's completion independently available BEFORE
	// releasing the worker, so it is provably buffered on doneCh at the
	// same instant cmdCh also holds the pause's pc.
	rf.at(0).finish(errors.New("spontaneous boom"))
	waitFor(t, 2*time.Second, func() bool { return len(c.doneCh) > 0 })

	release()

	waitObserved(t, c, ObservedPaused)

	snap := c.Snapshot()
	if snap.Transition != TransitionNone {
		t.Fatalf("transition = %v, want none (slot released)", snap.Transition)
	}
	if !snap.NextRetryAt.IsZero() {
		t.Fatal("no retry should be scheduled for a pause-driven wind-down")
	}
	if snap.Reason != ReasonUser {
		t.Fatalf("reason = %v, want user", snap.Reason)
	}

	// Deadlock detector: the controller must still be fully responsive.
	respondedCh := make(chan SubmitResult, 1)
	go func() { respondedCh <- c.Resume(context.Background()) }()
	select {
	case res2 := <-respondedCh:
		if res2.Outcome != OutcomeAccepted {
			t.Fatalf("resume after the race: %v (%v)", res2.Outcome, res2.Err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("controller did not respond to Resume after the race (deadlock)")
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

// (13b) A resume-driven fresh generation dies before ever becoming ready.
// Constructed deterministically (dispatch already ran, the generation's
// death is provably buffered before the worker's NEXT select) as a direct
// regression guard on defect 2's rewritten classification path: the
// terminal must be failed/reason=startup-failure with a retry scheduled
// (unchanged from before this fix), the slot released exactly once, the
// controller still responsive, and — critically — exactly the ONE
// generation this resume itself launched exists; no leftover command
// message or stale signal launches a second one (design v6 I1).
func TestResumeDrivenGenerationDiesBeforeReadyStartsExactlyOne(t *testing.T) {
	rf := newRunnerFactory()
	rf.readySignaling = true
	c := New(Config{
		Factory:     rf.Factory,
		Persistence: newFakePersistence(LoadResult{Found: true, Desired: DesiredPaused}),
		Clock:       newFakeClock(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)
	defer cancel()
	waitObserved(t, c, ObservedPaused)

	res := c.Resume(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("resume: %v (%v)", res.Outcome, res.Err)
	}
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedStarting)

	rf.at(0).finish(errors.New("boom")) // dies before ever becoming ready
	snap := waitFailedWithRetryScheduled(t, c)
	if snap.Reason != ReasonStartupFailure {
		t.Fatalf("reason = %v, want startup-failure", snap.Reason)
	}
	if snap.Transition != TransitionNone {
		t.Fatalf("transition = %v, want none (slot released)", snap.Transition)
	}

	// Give any wrongly-launched second generation a moment to appear
	// before asserting the count.
	time.Sleep(50 * time.Millisecond)
	if got := rf.count(); got != 1 {
		t.Fatalf("expected exactly 1 generation (the resume's own), got %d — something double-started", got)
	}

	// Responsiveness check.
	respondedCh := make(chan SubmitResult, 1)
	go func() { respondedCh <- c.Stop(context.Background()) }()
	select {
	case res2 := <-respondedCh:
		if res2.Outcome != OutcomeAccepted {
			t.Fatalf("stop after the failure: %v (%v)", res2.Outcome, res2.Err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("controller did not respond to Stop (deadlock)")
	}
	waitObserved(t, c, ObservedStopped)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// (defect 2) A phantom (slot-free), not-yet-ready start races an accepted
// cancel-start (Pause, landing through the "starting" row) against that
// same generation's own spontaneous death — both cmdCh and doneCh ready
// together. Classification must go by the SLOT's target (paused), not
// blindly by "desired=running" — the terminal must be paused, NEVER
// failed, with no retry, regardless of which channel Go's select picks.
func TestCancelStartRacesSpontaneousDeathClassifiesAsPaused(t *testing.T) {
	rf := newRunnerFactory()
	rf.readySignaling = true
	c := New(Config{
		Factory:     rf.Factory,
		Persistence: newFakePersistence(LoadResult{Found: false}),
		Clock:       newFakeClock(),
	})

	enter, release := armOnce(c, 1)

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)
	defer cancel()

	<-enter
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	if got := c.Snapshot().Observed; got != ObservedStarting {
		t.Fatalf("observed before pausing the worker = %v, want starting", got)
	}

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause during starting (slot-free): %v (%v)", res.Outcome, res.Err)
	}
	rf.at(0).finish(errors.New("spontaneous boom before ready"))
	waitFor(t, 2*time.Second, func() bool { return len(c.doneCh) > 0 })

	release()

	waitObserved(t, c, ObservedPaused)

	snap := c.Snapshot()
	if snap.Reason != ReasonUser {
		t.Fatalf("reason = %v, want user (never startup-failure)", snap.Reason)
	}
	if !snap.NextRetryAt.IsZero() {
		t.Fatal("no retry should be scheduled — this must classify as paused, not failed")
	}
	if snap.Transition != TransitionNone {
		t.Fatalf("transition = %v, want none (slot released)", snap.Transition)
	}

	respondedCh := make(chan SubmitResult, 1)
	go func() { respondedCh <- c.Resume(context.Background()) }()
	select {
	case res2 := <-respondedCh:
		if res2.Outcome != OutcomeAccepted {
			t.Fatalf("resume after the race: %v (%v)", res2.Outcome, res2.Err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("controller did not respond to Resume after the race (deadlock)")
	}
	rf.waitCount(t, 2)
	rf.at(1).waitStarted(t)
	rf.at(1).markReady() // this factory is readySignaling=true throughout
	waitObserved(t, c, ObservedRunning)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// (defect 1, retry-timer variant) Go's time.Timer.Stop cannot un-send a
// value already buffered in the timer's own channel: firing the fake
// clock's retry timer and THEN accepting a resume (whose accept() cancels
// the retry) can leave a stale fire sitting in the worker's private
// retryTimer channel racing the resume's own queued command — exactly
// defect 1's own "armed retry timer -> two live generations" example.
// Constructed deterministically via testBeforeSelect (both signals
// provably ready before the worker's select runs); the retrySeq guard
// (armRetryTimer/currentRetrySeq) must discard the stale fire regardless
// of which one Go's select happens to pick.
func TestStaleRetryFireDiscardedWhenResumeRaces(t *testing.T) {
	rf := newRunnerFactory()
	clock := newFakeClock()
	c := New(Config{
		Factory:     rf.Factory,
		Persistence: newFakePersistence(LoadResult{Found: false}),
		Clock:       clock,
	})

	// Pause at the top of the SECOND loop iteration: the first lets
	// reconciliation's generation 1 launch through untouched; by the start
	// of the second, that generation has already failed and a retry is
	// armed, but this iteration's own select has not run yet.
	callN := 0
	enterCh := make(chan struct{})
	resumeCh := make(chan struct{})
	c.testBeforeSelect = func() {
		callN++
		if callN != 2 {
			return
		}
		enterCh <- struct{}{}
		<-resumeCh
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)
	defer cancel()

	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedRunning)

	rf.at(0).finish(errors.New("boom"))
	waitObserved(t, c, ObservedFailed)

	<-enterCh

	if !clock.fireNext() {
		t.Fatal("expected a retry timer to be armed")
	}
	res := c.Resume(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("resume: %v (%v)", res.Outcome, res.Err)
	}

	resumeCh <- struct{}{}

	waitObserved(t, c, ObservedRunning)
	// Give a wrongly-launched second (stale-retry-triggered) generation a
	// moment to appear before asserting the count.
	time.Sleep(50 * time.Millisecond)
	if got := rf.count(); got != 2 {
		t.Fatalf("expected exactly 2 generations total (1 initial + 1 from resume), got %d — a stale retry fire double-started", got)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
