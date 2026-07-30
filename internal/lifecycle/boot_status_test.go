package lifecycle

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
)

// countRing counts how many currently-recent ring events have type t — used
// for a before/after DELTA (never an absolute count: events.Recent is a
// process-wide ring shared across every test in this binary, including
// repeated `-count=N` runs of the SAME test — see b1's precedent).
func countRing(t events.Type) int {
	n := 0
	for _, e := range events.Recent(200) {
		if e.Type == t {
			n++
		}
	}
	return n
}

// --- contract §11 item 9: StatusSink reaches a boot-honored paused/stopped ---

// A boot-honored persisted paused/stopped intent reaches the StatusSink
// exactly once, carrying the resolved observed state as its status.
func TestStatusSinkPublishesBootHonoredPausedStopped(t *testing.T) {
	for _, tc := range []struct {
		name         string
		desired      DesiredState
		wantObserved ObservedState
	}{
		{"paused", DesiredPaused, ObservedPaused},
		{"stopped", DesiredStopped, ObservedStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rf := newRunnerFactory()
			pers := newFakePersistence(LoadResult{Found: true, Desired: tc.desired})
			sink := &fakeStatusSink{}
			c, cancel, done := buildAndRun(Config{
				Factory: rf.Factory, Persistence: pers, Clock: newFakeClock(),
				StatusSink: sink,
			})
			defer cancel()

			waitObserved(t, c, tc.wantObserved)
			time.Sleep(20 * time.Millisecond) // give an errant duplicate call a chance to show up

			calls := sink.snapshotStatuses()
			if len(calls) != 1 {
				t.Fatalf("SetStatus called %d times, want exactly 1: %+v", len(calls), calls)
			}
			if calls[0].status != string(tc.wantObserved) {
				t.Errorf("SetStatus status = %q, want %q", calls[0].status, tc.wantObserved)
			}

			cancel()
			<-done
		})
	}
}

// A boot desired=running (the back-compat missing-row default) must NOT get
// an EXTRA SetStatus call from reconciliation itself: the miner drives its
// own startup statuses once its generation actually starts, via the SAME
// completeStart/publishTerminalNoSlot path any other reconcile/retry-driven
// start uses (pre-existing behavior, unrelated to contract §11 item 9) — so
// exactly ONE call is expected here (the ordinary generation-reached-running
// notification), not zero and not two.
func TestStatusSinkNotCalledWhenBootDesiredRunning(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: false}) // missing row -> running
	sink := &fakeStatusSink{}
	c, cancel, done := buildAndRun(Config{
		Factory: rf.Factory, Persistence: pers, Clock: newFakeClock(),
		StatusSink: sink,
	})
	defer cancel()

	rf.waitCount(t, 1)
	waitObserved(t, c, ObservedRunning)
	time.Sleep(20 * time.Millisecond)

	calls := sink.snapshotStatuses()
	if len(calls) != 1 {
		t.Fatalf("SetStatus called %d times for a running boot, want exactly 1 (the ordinary generation-running notification, no extra call from reconciliation): %+v", len(calls), calls)
	}
	if calls[0].status != string(ObservedRunning) {
		t.Errorf("SetStatus status = %q, want %q", calls[0].status, ObservedRunning)
	}

	cancel()
	<-done
}

// ForceRunning overriding a persisted paused intent to running must ALSO
// skip the reconciliation-added StatusSink call — the override folds desired
// to running before the boot-honored check runs, so this is the same "boot
// desired=running" case as far as item 9 is concerned; the single call that
// does happen is the ordinary generation-running notification.
func TestStatusSinkNotCalledWhenForceRunningOverridesPaused(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: true, Desired: DesiredPaused})
	sink := &fakeStatusSink{}
	c, cancel, done := buildAndRun(Config{
		Factory: rf.Factory, Persistence: pers, Clock: newFakeClock(),
		StatusSink: sink, ForceRunning: true,
	})
	defer cancel()

	rf.waitCount(t, 1)
	waitObserved(t, c, ObservedRunning)
	time.Sleep(20 * time.Millisecond)

	calls := sink.snapshotStatuses()
	if len(calls) != 1 {
		t.Fatalf("SetStatus called %d times for a ForceRunning-overridden boot, want exactly 1 (the ordinary generation-running notification, no extra call from reconciliation): %+v", len(calls), calls)
	}
	if calls[0].status != string(ObservedRunning) {
		t.Errorf("SetStatus status = %q, want %q", calls[0].status, ObservedRunning)
	}

	cancel()
	<-done
}

// --- contract §11 item 8: OD3 no-control-surface observability ---

// A boot-honored paused/stopped intent with NoControlSurface set records the
// canonical ring event exactly once.
func TestNoControlSurfaceRingEventOncePerBoot(t *testing.T) {
	before := countRing(events.TypeLifecycleIntentHonoredNoControlSurface)

	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: true, Desired: DesiredStopped})
	c, cancel, done := buildAndRun(Config{
		Factory: rf.Factory, Persistence: pers, Clock: newFakeClock(),
		NoControlSurface: true,
	})
	defer cancel()

	waitObserved(t, c, ObservedStopped)
	time.Sleep(20 * time.Millisecond) // give an errant duplicate a chance to show up

	if got := countRing(events.TypeLifecycleIntentHonoredNoControlSurface) - before; got != 1 {
		t.Fatalf("expected exactly 1 new lifecycle_intent_honored_no_control_surface ring event, got %d", got)
	}

	cancel()
	<-done
}

// A boot desired=running must never record the no-control-surface event or
// arm its reminder timer, even with NoControlSurface set: there is nothing
// "honored silently" to explain when the miner is actually starting.
func TestNoControlSurfaceNotRecordedWhenRunning(t *testing.T) {
	before := countRing(events.TypeLifecycleIntentHonoredNoControlSurface)

	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: false}) // missing row -> running
	clk := newFakeClock()
	c, cancel, done := buildAndRun(Config{
		Factory: rf.Factory, Persistence: pers, Clock: clk,
		NoControlSurface: true,
	})
	defer cancel()

	rf.waitCount(t, 1)
	waitObserved(t, c, ObservedRunning)
	time.Sleep(20 * time.Millisecond)

	if got := countRing(events.TypeLifecycleIntentHonoredNoControlSurface) - before; got != 0 {
		t.Fatalf("running boot recorded %d no-control-surface ring events, want 0", got)
	}
	if clk.pendingCount() != 0 {
		t.Fatal("running boot must not arm a no-control-surface reminder timer")
	}

	cancel()
	<-done
}

// A boot-honored paused intent WITH a real control surface (NoControlSurface
// false, the default) must not record the event or arm the reminder either —
// this observability exists only for the "nothing else will ever explain
// this" case.
func TestNoControlSurfaceNotRecordedWithControlSurface(t *testing.T) {
	before := countRing(events.TypeLifecycleIntentHonoredNoControlSurface)

	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: true, Desired: DesiredPaused})
	clk := newFakeClock()
	c, cancel, done := buildAndRun(Config{
		Factory: rf.Factory, Persistence: pers, Clock: clk,
		NoControlSurface: false,
	})
	defer cancel()

	waitObserved(t, c, ObservedPaused)
	time.Sleep(20 * time.Millisecond)

	if got := countRing(events.TypeLifecycleIntentHonoredNoControlSurface) - before; got != 0 {
		t.Fatalf("paused-with-control-surface boot recorded %d no-control-surface ring events, want 0", got)
	}
	if clk.pendingCount() != 0 {
		t.Fatal("a control-surface boot must not arm a no-control-surface reminder timer")
	}

	cancel()
	<-done
}

// The periodic reminder timer re-arms itself indefinitely: firing it twice
// (via the fake clock) must leave exactly one fresh timer pending each time,
// proving the tick handler processed the fire AND re-armed for the next
// interval, rather than firing once and going silent.
func TestNoControlSurfaceTickRearmsAtLeastTwice(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: true, Desired: DesiredPaused})
	clk := newFakeClock()
	c, cancel, done := buildAndRun(Config{
		Factory: rf.Factory, Persistence: pers, Clock: clk,
		NoControlSurface: true,
	})
	defer cancel()

	waitObserved(t, c, ObservedPaused)
	waitFor(t, 2*time.Second, func() bool { return clk.pendingCount() == 1 })

	if !clk.fireNext() {
		t.Fatal("expected a pending no-control-surface timer to fire (1st tick)")
	}
	waitFor(t, 2*time.Second, func() bool { return clk.pendingCount() == 1 })
	if got := len(clk.timers); got != 2 {
		t.Fatalf("after 1 tick, %d timers total were ever armed, want 2 (initial + 1 rearm)", got)
	}

	if !clk.fireNext() {
		t.Fatal("expected the re-armed timer to fire a second time (2nd tick)")
	}
	waitFor(t, 2*time.Second, func() bool { return clk.pendingCount() == 1 })
	if got := len(clk.timers); got != 3 {
		t.Fatalf("after 2 ticks, %d timers total were ever armed, want 3 (initial + 2 rearms)", got)
	}

	cancel()
	<-done
}
