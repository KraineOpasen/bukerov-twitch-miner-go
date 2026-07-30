package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// (25) A dirty (join-timeout class) teardown while desired transitions to
// paused/stopped settles on observed=degraded (design v6 §5.3(b), I30): the
// process stays alive/observable, but resume/restart are rejected with
// "process restart required" until the process itself restarts. Pause/stop
// (switching desired between paused and stopped) remain accepted as
// persist-only, staying in degraded.
func TestDirtyTeardownAtDesiredPausedEntersDegraded(t *testing.T) {
	rf := newRunnerFactory()
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	dirtyErr := fmt.Errorf("background loop join timed out: %w", ErrDirtyTeardown)
	rf.at(0).returnOnCancel = dirtyErr

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}
	waitObserved(t, c, ObservedDegraded)

	snap := c.Snapshot()
	if snap.Desired != DesiredPaused {
		t.Fatalf("desired = %v, want paused", snap.Desired)
	}
	if snap.LastError == "" {
		t.Fatal("LastError should describe the dirty teardown")
	}

	resumeRes := c.Resume(context.Background())
	if resumeRes.Outcome != OutcomeRejected {
		t.Fatalf("resume from degraded: got %v, want rejected", resumeRes.Outcome)
	}
	if resumeRes.Err == nil || resumeRes.Err.Error() != "process restart required" {
		t.Fatalf("resume rejection error = %v, want %q", resumeRes.Err, "process restart required")
	}

	restartRes := c.Restart(context.Background())
	if restartRes.Outcome != OutcomeRejected {
		t.Fatalf("restart from degraded: got %v, want rejected", restartRes.Outcome)
	}

	// Duplicate pause (desired already paused) is idempotent, staying in
	// degraded.
	dupPause := c.Pause(context.Background())
	if dupPause.Outcome != OutcomeIdempotent {
		t.Fatalf("duplicate pause in degraded: got %v, want idempotent", dupPause.Outcome)
	}

	// Stop switches desired paused->stopped, persist-only, staying degraded
	// (no new generation is ever built).
	stopRes := c.Stop(context.Background())
	if stopRes.Outcome != OutcomeAccepted {
		t.Fatalf("stop (paused->stopped) in degraded: got %v (%v)", stopRes.Outcome, stopRes.Err)
	}
	waitFor(t, 5*time.Second, func() bool { return c.Snapshot().Desired == DesiredStopped })
	if c.Snapshot().Observed != ObservedDegraded {
		t.Fatalf("observed after switching desired in degraded = %v, want still degraded", c.Snapshot().Observed)
	}

	if rf.count() != 1 {
		t.Fatalf("no new generation should ever be built while degraded, got %d", rf.count())
	}

	cancel()
	<-done
}

// (26) A dirty teardown of the OLD generation during a restart, where
// desired stays running throughout, is NOT held as degraded — Controller.Run
// itself returns the teardown error (design v6 §5.3(a): process-exit path,
// matching today's "shutdown drain incomplete" -> exit 1 behavior).
func TestDirtyTeardownAtDesiredRunningExitsProcess(t *testing.T) {
	rf := newRunnerFactory()
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	dirtyErr := fmt.Errorf("background loop join timed out: %w", ErrDirtyTeardown)
	rf.at(0).returnOnCancel = dirtyErr

	res := c.Restart(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("restart: %v (%v)", res.Outcome, res.Err)
	}

	select {
	case err := <-done:
		if err == nil || !errors.Is(err, ErrDirtyTeardown) {
			t.Fatalf("Run() returned %v, want an error wrapping ErrDirtyTeardown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Controller.Run did not return after a dirty teardown at desired=running")
	}

	if rf.count() != 1 {
		t.Fatalf("no new generation should be built after a dirty restart teardown, got %d", rf.count())
	}

	cancel()
}
