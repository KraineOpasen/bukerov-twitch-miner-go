package lifecycle

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// BLOCKER 2 (F4b Q3 consolidated corrective) — worker.go's startGeneration
// used to read "was this a failed lineage" (c.state.snapshot()) and THEN,
// in a SEPARATE lock/unlock pair, publish the new "starting" transition
// (c.state.beginTransition(...)). Between those two calls, observed was
// still whatever it was before (e.g. ObservedFailed with the slot free) —
// a concurrent Submit's accept() evaluated during that exact window sees
// STALE state and can legitimately compute action=actionStart (a
// resume/restart), occupying the slot and queuing itself on cmdCh, all
// while dispatch()'s own handling of that action naively calls
// startGeneration() a SECOND time, unconditionally — clobbering
// currentGenCancel and orphaning the first generation (which then runs
// until process exit, and immediately panics fakeRunner's own exclusivity
// guard in this test suite).
//
// The fix has two layers: (a) startGeneration's own "was failed" read and
// its beginTransition write are merged into ONE atomic statusState critical
// section (beginStartTransition) — narrowing, but not fully eliminating,
// the window (a single mutex lock/unlock is still not free); (b) dispatch()
// defensively re-derives reality from the worker's OWN state
// (c.generationLive/c.awaitingStart) before ever launching a second
// generation for an actionStart it dequeues — this is what actually closes
// the hazard regardless of how narrow (a) manages to make the window.
//
// A genuine mutex-acquisition-width race cannot be forced deterministically
// through the testBeforeSelect seam (it only pauses BETWEEN loop
// iterations, never mid-function) without inventing a new code-only-for-
// testing seam inside startGeneration itself, which would be a worse
// trade-off than testing the defensive guard (layer b) directly: these
// tests construct EXACTLY the command shape a raced accept() would have
// produced (same-package whitebox — this file follows dispatch_race_test.go
// and slot_discipline_test.go's own precedent of driving worker.go
// internals directly to construct otherwise-un-force-able interleavings)
// and feed it through the REAL worker exactly as cmdCh would once dispatch
// dequeues it, while the worker is provably paused (testBeforeSelect) so
// there is no race on the injection itself.

// pauseTestHook installs a manually-stepped testBeforeSelect: every call
// blocks (rendezvousing on ack/step) until passthrough is set, at which
// point every call (including one already blocked... note: only future
// calls) becomes a no-op passthrough. Must be installed before Run starts.
func pauseTestHook(c *Controller) (ack <-chan struct{}, step chan<- struct{}, passthrough *atomic.Bool) {
	var p atomic.Bool
	ackCh := make(chan struct{})
	stepCh := make(chan struct{})
	c.testBeforeSelect = func() {
		if p.Load() {
			return
		}
		ackCh <- struct{}{}
		<-stepCh
	}
	return ackCh, stepCh, &p
}

// (i) actionStart dequeued while a DIFFERENT generation is already live
// (the exact hazard's own shape) must never launch a second generation:
// it is ADOPTED into the live phantom start's own awaitingStart instead,
// and its own terminal (CommandID) publishes once that SAME (never
// duplicated) generation actually becomes ready.
func TestDispatchActionStartWhileGenerationLiveAdoptsIntoPhantomStart(t *testing.T) {
	rf := newRunnerFactory()
	rf.readySignaling = true
	pers := newFakePersistence(LoadResult{Found: false}) // missing row -> running: phantom start
	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()}
	c := New(cfg)

	ack, step, passthrough := pauseTestHook(c)

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)
	defer cancel()

	<-ack // iteration 1: phantom generation 1 launched, worker not yet in its select.
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	if got := c.Snapshot().Observed; got != ObservedStarting {
		t.Fatalf("observed = %v, want starting", got)
	}

	// Simulate the pc a raced accept() would have produced: a resume,
	// evaluated against a stale, already-superseded snapshot, that
	// legitimately computed action=actionStart.
	pc := &pendingCommand{id: "raced-resume", cmd: CommandResume, reason: ReasonUser, target: DesiredRunning, action: actionStart}
	c.state.mu.Lock()
	c.state.pending = pc
	c.state.transition = TransitionPending
	c.state.mu.Unlock()
	c.cmdCh <- pc

	step <- struct{}{} // let iteration 1 run its select: cmdCh is the only ready case, dispatch(pc) runs.
	<-ack              // iteration 2: dispatch(pc) has fully returned.

	if got := rf.count(); got != 1 {
		t.Fatalf("dispatching actionStart while a generation is already live must not build a second one, got %d generations", got)
	}
	if got := c.Snapshot().Observed; got != ObservedStarting {
		t.Fatalf("observed = %v, want still starting (the raced command must be ADOPTED into the live phantom start, not completed before it is actually ready)", got)
	}

	passthrough.Store(true)
	step <- struct{}{}

	rf.at(0).markReady()
	waitObserved(t, c, ObservedRunning)
	if got := c.Snapshot().CommandID; got != pc.id {
		t.Fatalf("CommandID = %q, want the adopted command's own id %q", got, pc.id)
	}
	if rf.count() != 1 {
		t.Fatalf("expected exactly 1 generation ever built throughout, got %d", rf.count())
	}
	if c.Snapshot().Transition != TransitionNone {
		t.Fatalf("transition = %v, want none (slot released)", c.Snapshot().Transition)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// (ii) actionPersistOnlySetObserved dequeued while a generation is
// ALREADY live (the sibling hazard: a pause/stop raced into "failed"
// while a retry-driven start was actually in flight) must never simply
// flip observed to paused/stopped while leaving that generation running
// orphaned — it escalates to a genuine teardown (runTeardown semantics):
// the live generation is actually cancelled and awaited BEFORE the
// terminal "paused" is published.
func TestDispatchPersistOnlyWhileGenerationLiveEscalatesToTeardown(t *testing.T) {
	rf := newRunnerFactory()
	rf.readySignaling = true
	pers := newFakePersistence(LoadResult{Found: false})
	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()}
	c := New(cfg)

	ack, step, passthrough := pauseTestHook(c)

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)
	defer cancel()

	<-ack // iteration 1: phantom generation 1 launched, worker not yet in its select.
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)

	// Simulate the pc a raced accept() would have produced for a pause
	// evaluated against a stale ObservedFailed+slot-free snapshot: action
	// = actionPersistOnlySetObserved, which is only ever correct when NO
	// generation is actually live to touch.
	pc := &pendingCommand{id: "raced-pause", cmd: CommandPause, reason: ReasonUser, target: DesiredPaused, action: actionPersistOnlySetObserved}
	c.state.mu.Lock()
	c.state.pending = pc
	c.state.transition = TransitionPending
	c.state.mu.Unlock()
	c.cmdCh <- pc

	passthrough.Store(true) // the escalated teardown needs its own doneCh wait; let the worker run freely from here.
	step <- struct{}{}      // let iteration 1 dispatch(pc).

	// The generation must actually be torn down (its fakeRunner's Run
	// call cancelled and awaited) BEFORE the terminal "paused" publishes —
	// waitObserved alone only proves the terminal was reached; the
	// runner's own recorded return (rec) proves teardown really happened,
	// not merely that observed was overwritten while the generation kept
	// mining, orphaned.
	waitObserved(t, c, ObservedPaused)

	events := rf.rec.snapshot()
	if indexOf(events, "return-1") == -1 {
		t.Fatalf("the live generation was never actually torn down (no return-1 recorded): %v", events)
	}
	if rf.count() != 1 {
		t.Fatalf("expected exactly 1 generation ever built, got %d", rf.count())
	}
	snap := c.Snapshot()
	if snap.Transition != TransitionNone {
		t.Fatalf("transition = %v, want none (slot released)", snap.Transition)
	}
	if snap.CommandID != pc.id {
		t.Fatalf("CommandID = %q, want %q", snap.CommandID, pc.id)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
