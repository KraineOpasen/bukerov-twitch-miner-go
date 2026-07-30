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

// MAJOR 6 (F4b Q3 consolidated corrective), design v6 §5.4(б): a resume
// submitted while LIFECYCLE_FORCE_RUNNING's override is active used to hit
// the running row's plain idempotent cell and return WITHOUT ever calling
// Persistence.Save — leaving the durable row exactly as it was before the
// override (e.g. still "paused"), even though the operator just explicitly
// asked to run. That silently defeats the override's own stated purpose
// ("resume при override... пишет durable=running (снимает расхождение
// память/диск)"): with no runtime command surface to publish the divergence
// resolution, removing the env var later would still reconcile back to the
// stale durable value. A resume evaluated against this idempotent cell must
// now persist durable=running like any other accepted command.
func TestResumeUnderOverridePersistsDurableRunning(t *testing.T) {
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
	if pers.saveCount() != 0 {
		t.Fatalf("ForceRunning itself must not persist anything yet, got %d saves", pers.saveCount())
	}

	res := c.Resume(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("resume under override: got %v, want accepted (%v)", res.Outcome, res.Err)
	}

	waitFor(t, 2*time.Second, func() bool { return pers.saveCount() == 1 })
	if got := pers.lastSave().desired; got != DesiredRunning {
		t.Fatalf("persisted desired = %v, want running", got)
	}

	// Observed must stay running throughout — this is a same-value
	// republish, not a visible transition, and no new generation is built.
	if got := c.Snapshot().Observed; got != ObservedRunning {
		t.Fatalf("observed = %v, want still running", got)
	}
	if rf.count() != 1 {
		t.Fatalf("resume under an already-running override must not build a new generation, got %d", rf.count())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	// A fresh controller (override cleared) over the SAME persistence now
	// honors the durable running value the override-resume just wrote —
	// the memory/disk divergence is genuinely resolved.
	rf2 := newRunnerFactory()
	c2, cancel2, done2 := buildAndRun(Config{Factory: rf2.Factory, Persistence: pers, Clock: newFakeClock()})
	defer cancel2()

	rf2.waitCount(t, 1)
	waitObserved(t, c2, ObservedRunning)
	if c2.Snapshot().Override {
		t.Fatal("override must not be reported once ForceRunning is no longer set")
	}

	cancel2()
	<-done2
}
