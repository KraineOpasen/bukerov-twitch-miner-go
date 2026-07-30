package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"
)

// buildAndRun constructs a Controller from cfg, runs it in the background,
// and returns it along with its cancel func and result channel. It does
// NOT wait for any particular state (reconciliation may or may not start a
// generation) — callers assert on that themselves.
func buildAndRun(cfg Config) (*Controller, context.CancelFunc, <-chan error) {
	c := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)
	return c, cancel, done
}

// (14) A persisted "paused" desired state is honored at startup: no
// generation is started, and observed settles directly on "paused".
func TestReconcilePersistedPausedHonored(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: true, Desired: DesiredPaused})
	c, cancel, done := buildAndRun(Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()})
	defer cancel()

	waitObserved(t, c, ObservedPaused)
	time.Sleep(20 * time.Millisecond) // give any errant generation-start a chance to show up
	if rf.count() != 0 {
		t.Fatalf("no generation should have been started for a persisted paused state, got %d", rf.count())
	}
	snap := c.Snapshot()
	if snap.Desired != DesiredPaused {
		t.Fatalf("desired = %v, want paused", snap.Desired)
	}
	if !snap.Capabilities.CanResume {
		t.Fatal("resume should be available from a reconciled paused state")
	}

	cancel()
	<-done
}

// (15) A persisted "stopped" desired state is honored at startup the same
// way.
func TestReconcilePersistedStoppedHonored(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: true, Desired: DesiredStopped})
	c, cancel, done := buildAndRun(Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()})
	defer cancel()

	waitObserved(t, c, ObservedStopped)
	time.Sleep(20 * time.Millisecond)
	if rf.count() != 0 {
		t.Fatalf("no generation should have been started for a persisted stopped state, got %d", rf.count())
	}
	snap := c.Snapshot()
	if snap.Desired != DesiredStopped {
		t.Fatalf("desired = %v, want stopped", snap.Desired)
	}

	cancel()
	<-done
}

// (16) A missing persisted row (Found=false, nil error — no table/no row
// yet) defaults to running, back-compat with pre-lifecycle behavior.
func TestReconcileMissingRowDefaultsToRunning(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: false})
	c, cancel, done := buildAndRun(Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()})
	defer cancel()

	rf.waitCount(t, 1)
	waitObserved(t, c, ObservedRunning)
	if c.Snapshot().Desired != DesiredRunning {
		t.Fatalf("desired = %v, want running", c.Snapshot().Desired)
	}

	cancel()
	<-done
}

// (17) An unrecognized persisted value fails closed to paused AND rewrites
// the durable row with a reason recording the raw value verbatim.
func TestReconcileUnknownValueFailsClosedAndRewrites(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{})
	pers.loadErr = &CorruptStateError{Raw: "sleeping"}

	c, cancel, done := buildAndRun(Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()})
	defer cancel()

	waitObserved(t, c, ObservedPaused)
	time.Sleep(20 * time.Millisecond)
	if rf.count() != 0 {
		t.Fatalf("no generation should start on a fail-closed reconciliation, got %d", rf.count())
	}

	snap := c.Snapshot()
	if snap.Desired != DesiredPaused {
		t.Fatalf("desired = %v, want paused (fail-closed)", snap.Desired)
	}
	if snap.LastError == "" {
		t.Fatal("LastError should describe the corrupt value")
	}

	if pers.saveCount() != 1 {
		t.Fatalf("expected the corrupt row to be rewritten exactly once, got %d saves", pers.saveCount())
	}
	saved := pers.lastSave()
	if saved.desired != DesiredPaused {
		t.Fatalf("rewritten desired = %v, want paused", saved.desired)
	}
	wantReason := `fail-closed: was "sleeping"`
	if saved.reason != wantReason {
		t.Fatalf("rewritten reason = %q, want %q", saved.reason, wantReason)
	}

	cancel()
	<-done
}

// (18) A plain read error (I/O, database.ErrClosed, ...) fails closed to an
// in-memory paused WITHOUT rewriting the durable row (we don't know it was
// actually bad — only that we couldn't read it).
func TestReconcileReadErrorDoesNotRewrite(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{})
	pers.loadErr = errors.New("disk I/O error")

	c, cancel, done := buildAndRun(Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()})
	defer cancel()

	waitObserved(t, c, ObservedPaused)
	time.Sleep(20 * time.Millisecond)
	if rf.count() != 0 {
		t.Fatalf("no generation should start after a read error, got %d", rf.count())
	}

	snap := c.Snapshot()
	if snap.Desired != DesiredPaused {
		t.Fatalf("desired = %v, want paused (fail-closed, in-memory only)", snap.Desired)
	}
	if snap.LastError == "" {
		t.Fatal("LastError should describe the read failure")
	}
	if pers.saveCount() != 0 {
		t.Fatalf("a read error must NOT rewrite the durable row, got %d saves", pers.saveCount())
	}

	cancel()
	<-done
}

// (19) ForceRunning forces in-memory desired to running WITHOUT rewriting
// the durable row, and reports Override=true.
func TestForceRunningDoesNotRewriteDurableRow(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: true, Desired: DesiredPaused})

	c, cancel, done := buildAndRun(Config{
		Factory:      rf.Factory,
		Persistence:  pers,
		Clock:        newFakeClock(),
		ForceRunning: true,
	})
	defer cancel()

	rf.waitCount(t, 1)
	waitObserved(t, c, ObservedRunning)

	snap := c.Snapshot()
	if snap.Desired != DesiredRunning {
		t.Fatalf("desired = %v, want running (forced)", snap.Desired)
	}
	if !snap.Override {
		t.Fatal("Override should be true when ForceRunning was honored")
	}
	if pers.saveCount() != 0 {
		t.Fatalf("ForceRunning must not rewrite the durable row, got %d saves", pers.saveCount())
	}

	cancel()
	<-done
}

// (20) A runtime command submitted while ForceRunning's override is active
// still persists durably as normal; once a fresh controller starts WITHOUT
// the override, the durable value (not the previous in-memory override)
// governs.
func TestCommandDuringOverridePersistsDurably(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: true, Desired: DesiredPaused})

	c, cancel, done := buildAndRun(Config{
		Factory:      rf.Factory,
		Persistence:  pers,
		Clock:        newFakeClock(),
		ForceRunning: true,
	})
	rf.waitCount(t, 1)
	waitObserved(t, c, ObservedRunning)

	// Pause while the override is active: this is a normal runtime command
	// and must persist durably like any other.
	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause under override: %v (%v)", res.Outcome, res.Err)
	}
	waitObserved(t, c, ObservedPaused)

	if pers.saveCount() != 1 {
		t.Fatalf("pause under override should persist durably, got %d saves", pers.saveCount())
	}
	if pers.lastSave().desired != DesiredPaused {
		t.Fatalf("persisted desired = %v, want paused", pers.lastSave().desired)
	}

	cancel()
	<-done

	// A fresh controller (override cleared) over the SAME persistence must
	// now honor the durable paused value, not the in-memory override.
	rf2 := newRunnerFactory()
	c2, cancel2, done2 := buildAndRun(Config{Factory: rf2.Factory, Persistence: pers, Clock: newFakeClock()})
	defer cancel2()

	waitObserved(t, c2, ObservedPaused)
	if c2.Snapshot().Override {
		t.Fatal("override must not be reported once ForceRunning is no longer set")
	}
	if rf2.count() != 0 {
		t.Fatalf("no generation should start: durable state is paused, got %d generations", rf2.count())
	}

	cancel2()
	<-done2
}
