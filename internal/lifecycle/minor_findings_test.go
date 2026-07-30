package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// MINOR 7 (F4b Q3 consolidated corrective), design v6 §5.2 step 1:
// "all canX=false" covers the WHOLE span a command is accepted through its
// own terminal (slot held), not just the narrow pre-dispatch
// Transition==pending window before the worker even dequeues it.
// capabilitiesLocked used to only special-case Transition==pending,
// leaving capabilities computed normally (via the table) once the worker
// had dispatched the command and moved Transition on to pausing/stopping/
// etc — even though the slot is still very much occupied throughout.
func TestCapabilitiesAllFalseWhileSlotHeldNotJustPending(t *testing.T) {
	rf := newRunnerFactory()
	rf.ignoreCt = true // teardown won't resolve until the test calls finish()
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}
	waitObserved(t, c, ObservedPausing)

	snap := c.Snapshot()
	if snap.Transition == TransitionPending {
		t.Fatal("test setup: expected transition to have moved past pending (dispatched to pausing) by now")
	}
	caps := snap.Capabilities
	if caps.CanPause || caps.CanResume || caps.CanRestart || caps.CanStop {
		t.Fatalf("capabilities must be all-false while a command is in flight (slot held), got %+v", caps)
	}

	rf.at(0).finish(nil)
	waitObserved(t, c, ObservedPaused)
	if !c.Snapshot().Capabilities.CanPause {
		t.Fatal("capabilities must be restored once the slot is released again")
	}

	cancel()
	<-done
}

// MINOR 8 (F4b Q3 consolidated corrective): handleUpdateApplied used to
// discard tearDownForExit's error entirely (`_ = ...`), publishing "" as
// LastError regardless. Since update-exit always returns nil (I31 — no
// os.Exit, no surfaced Run error on this path), a non-nil teardown error
// would otherwise be completely invisible. It must now be logged and
// surfaced as the exiting publish's own LastError, exactly like
// handleProcessShutdown already does for its own exit path.
func TestUpdateAppliedSurfacesTeardownErrorAsLastError(t *testing.T) {
	rf := newRunnerFactory()
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	wantErr := errors.New("teardown hiccup")
	rf.at(0).returnOnCancel = wantErr

	c.UpdateApplied()

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Controller.Run did not return after update-applied")
	}
	if runErr != nil {
		t.Fatalf("Run() returned %v, want nil (update-exit never surfaces an error, I31)", runErr)
	}

	snap := c.Snapshot()
	if snap.Observed != ObservedExiting {
		t.Fatalf("observed = %v, want exiting", snap.Observed)
	}
	if snap.LastError == "" {
		t.Fatal("LastError should surface the teardown error that update-exit's own Run swallowed from its return value")
	}

	cancel()
}

// MINOR 15 (F4b Q3 consolidated corrective): enterDegraded used to leave
// CommandID untouched when releasing the slot, unlike publishTerminal
// (which always sets it from the released command) — a degraded terminal
// reached via a command-in-flight (e.g. a dirty teardown during that
// command's own pause/stop) would leave Snapshot().CommandID stuck on
// whatever it was before, instead of reporting the command that actually
// triggered the degraded entry.
func TestEnterDegradedSetsCommandIDFromReleasedPending(t *testing.T) {
	rf := newRunnerFactory()
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	rf.at(0).returnOnCancel = fmt.Errorf("background loop join timed out: %w", ErrDirtyTeardown)

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}
	waitObserved(t, c, ObservedDegraded)

	if got := c.Snapshot().CommandID; got != res.CommandID {
		t.Fatalf("CommandID after entering degraded = %q, want the pause command's own id %q", got, res.CommandID)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// MINOR 16 (F4b Q3 consolidated corrective): Controller.Run had no
// run-once guard at all — nothing stopped a second concurrent (or
// sequential) call to Run on the same Controller, which would spawn a
// second worker goroutine racing the first over every worker-owned field
// (currentGen, awaitingStart, generationLive, ...) with no synchronization
// whatsoever. A second call must instead return an error immediately.
func TestControllerRunSecondCallReturnsError(t *testing.T) {
	rf := newRunnerFactory()
	c, ctx, cancel, done := newRunningController(t, rf)
	defer cancel()

	err := c.Run(ctx)
	if err == nil {
		t.Fatal("a second concurrent Run call must return an error, got nil")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the FIRST Run call did not return after cancel")
	}
}
