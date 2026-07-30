package lifecycle

import (
	"errors"
	"testing"
	"time"
)

// MAJOR 4 (F4b Q3 consolidated corrective), design v6 §5.4/OD3: "running ->
// start (failure -> failed+retry; if there is no web in the composition ->
// exit 1 as today, OD3)". classifyUnhealthyCompletion used to ALWAYS enter
// failed+retry for a non-dirty-teardown startup failure at desired=running,
// regardless of whether a control surface (dashboard) exists for this
// process at all. With NoControlSurface set, failed+retry is invisible AND
// unactionable (nothing can ever show "retrying" or accept a command to
// intervene) — so a startup failure must instead make Controller.Run return
// the failure (App.Run -> main exits 1, the supervisor restarts it), the
// exact pre-lifecycle behavior OD3 preserves. A control surface being
// present must still get failed+retry, unchanged.

// (a) NoControlSurface=true: a startup failure at desired=running exits the
// process instead of entering failed+retry.
func TestNoControlSurfaceStartupFailureExitsProcess(t *testing.T) {
	rf := newRunnerFactory()
	clock := newFakeClock()
	c, cancel, done := buildAndRun(Config{
		Factory:          rf.Factory,
		Persistence:      newFakePersistence(LoadResult{Found: false}), // missing row -> running
		Clock:            clock,
		NoControlSurface: true,
	})
	defer cancel()

	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedRunning)

	wantErr := errors.New("auth rejected")
	rf.at(0).finish(wantErr)

	select {
	case err := <-done:
		if err == nil || !errors.Is(err, wantErr) {
			t.Fatalf("Run() returned %v, want an error wrapping %v (OD3: no control surface -> exit, not retry)", err, wantErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Controller.Run did not return after a no-control-surface startup failure")
	}

	if clock.pendingCount() != 0 {
		t.Fatal("no retry should ever be armed when NoControlSurface causes an exit instead")
	}
	if got := c.Snapshot().Observed; got == ObservedFailed {
		t.Fatalf("observed = %v, must never settle on failed when exiting instead (OD3)", got)
	}
	if rf.count() != 1 {
		t.Fatalf("no new generation should be built after an OD3 exit, got %d", rf.count())
	}
}

// (b) A control surface present (NoControlSurface=false, the default): a
// startup failure at desired=running still enters failed+retry, unchanged.
func TestControlSurfacePresentStartupFailureStillRetries(t *testing.T) {
	rf := newRunnerFactory()
	clock := newFakeClock()
	c, cancel, done := buildAndRun(Config{
		Factory:          rf.Factory,
		Persistence:      newFakePersistence(LoadResult{Found: false}),
		Clock:            clock,
		NoControlSurface: false,
	})
	defer cancel()

	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedRunning)

	rf.at(0).finish(errors.New("auth rejected"))
	snap := waitFailedWithRetryScheduled(t, c)
	if snap.Reason != ReasonStartupFailure {
		t.Fatalf("reason = %v, want startup-failure", snap.Reason)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
