package lifecycle

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// BLOCKER 1 (F4b Q3 consolidated corrective) / §12 missing slot-discipline
// test: accept()'s pending-window guard checked ONLY Transition==pending,
// not slot occupancy (s.pending != nil) itself. Those are usually the same
// thing, but a slot-FREE phantom start's own completion
// (publishTerminalNoSlot, worker.go's completeStart when pc==nil) forcibly
// resets Transition to none WITHOUT touching a DIFFERENT command's slot
// occupancy — by design, a phantom start never held that slot in the first
// place (see publishTerminalNoSlot's doc comment). If a real command (pcA)
// had ALREADY been accepted (occupying the slot, e.g. via the "starting"
// row's slot-free cancel-start cell) but had not yet been SENT to cmdCh —
// which only happens after Submit's own Persistence.Save returns — the
// phantom's completion could run in between, leaving Transition==none while
// s.pending still holds pcA. A second, unrelated command evaluated against
// the table AFTER that point sees Transition==none and, for any row that
// (unlike "starting") does not itself consult slotHeld — e.g. "running" —
// is admitted anyway, OVERWRITING s.pending with pcB and silently dropping
// pcB's own send to the already-full cmdCh (still holding pcA). The worker
// eventually dequeues pcA, finds it no longer isCurrentPending (s.pending is
// now pcB), and discards it as stale — but pcB's send never happened, so
// nothing ever dispatches it: Transition is stuck at pending forever, every
// later Submit 409s, and the controller is permanently wedged.
//
// Constructing this deterministically needs TWO independent barriers, or the
// test itself becomes racy in either direction:
//   - pcA's own Persistence.Save is held open (armSaveBarrier) so pcA's
//     in-memory slot occupancy (accept()) is established well before its
//     cmdCh send, which only happens once Save returns.
//   - the worker is ALSO pinned via testBeforeSelect (stepped manually,
//     call-by-call) across the whole window from "the phantom generation
//     becomes ready" through "a second command is submitted while pcA sits
//     in cmdCh": without this, the worker — idle in its select, ready to
//     receive — races ahead and can fully dequeue/teardown/release pcA's
//     slot before the test ever gets to submit the second command, which
//     would make the second command's ACCEPTANCE look legitimate (the slot
//     really would be free by then) rather than reproducing the bug.
func TestAcceptRejectsWhenSlotOccupiedDespiteTransitionCleared(t *testing.T) {
	rf := newRunnerFactory()
	rf.readySignaling = true
	pers := newFakePersistence(LoadResult{Found: false}) // missing row -> running: slot-free phantom start
	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()}
	c := New(cfg)

	// Manually-stepped testBeforeSelect: pauses the worker at the TOP of
	// every loop iteration (before its select ever runs) until passthrough
	// is set, letting the test control exactly which iteration is allowed
	// to run its select and when.
	var passthrough atomic.Bool
	ackCh := make(chan struct{})
	stepCh := make(chan struct{})
	c.testBeforeSelect = func() {
		if passthrough.Load() {
			return
		}
		ackCh <- struct{}{}
		<-stepCh
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)
	defer cancel()

	// Iteration 1: paused right after reconciliation has already
	// synchronously launched the phantom generation, before the loop's own
	// select has run at all.
	<-ackCh
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	if got := c.Snapshot().Observed; got != ObservedStarting {
		t.Fatalf("observed before releasing iteration 1 = %v, want starting", got)
	}
	stepCh <- struct{}{} // let iteration 1 enter its select; nothing is ready yet, so it blocks there.

	// Hold Pause's own persist open: accept() (occupying the slot as pcA,
	// via the "starting" row's slot-free cancel-start cell) completes
	// in-memory immediately, but Submit's send into cmdCh — which happens
	// only AFTER Save returns (lifecycle.go's Submit) — is delayed for as
	// long as we hold this barrier.
	release := pers.armSaveBarrier()

	pauseResCh := make(chan SubmitResult, 1)
	go func() { pauseResCh <- c.Pause(context.Background()) }()

	// Wait until Pause has actually occupied the slot before letting the
	// phantom generation's own completion run.
	waitFor(t, 2*time.Second, func() bool { return c.Snapshot().Transition == TransitionPending })

	// The phantom start reaches ready while Pause's cmdCh send is still
	// pending (Save still blocked): iteration 1's blocked select wakes on
	// readyChSafe alone (cmdCh is not ready yet) and runs completeStart ->
	// publishTerminalNoSlot, resetting Transition to none WITHOUT touching
	// pcA's slot (a phantom start never held it). The loop then reaches
	// iteration 2's testBeforeSelect call and pauses again, BEFORE it has
	// looked at cmdCh even once.
	rf.at(0).markReady()
	<-ackCh
	waitObserved(t, c, ObservedRunning)

	snapMidRace := c.Snapshot()
	if snapMidRace.Transition != TransitionNone {
		t.Fatalf("transition = %v, want none (phantom completion must reset it even though pcA's slot is still occupied)", snapMidRace.Transition)
	}

	// Release Pause's Save: Submit proceeds to actually send pcA into
	// cmdCh and returns. The worker is STILL frozen before iteration 2's
	// select, so pcA is provably sitting there, undispatched, while we
	// submit a second command below — not merely "probably still there".
	release()
	pauseRes := <-pauseResCh
	if pauseRes.Outcome != OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", pauseRes.Outcome, pauseRes.Err)
	}

	before := len(c.cmdCh)
	stopRes := c.Stop(context.Background())
	if stopRes.Outcome != OutcomeRejected {
		t.Fatalf("stop while a different command's slot is still occupied: got %v, want rejected (%v)", stopRes.Outcome, stopRes.Err)
	}
	if after := len(c.cmdCh); after != before {
		t.Fatalf("cmdCh length changed from %d to %d — the rejected stop must never touch cmdCh (no send, no drain)", before, after)
	}

	// Let the worker run freely from here on: iteration 2 (and every
	// iteration after) proceeds through its select normally.
	passthrough.Store(true)
	stepCh <- struct{}{}

	// The controller must remain fully live: Pause's own pcA still reaches
	// its terminal (paused), and the slot is available again afterward —
	// this is the "controller must remain live" half of the regression,
	// distinguishing a correct 409 from the bug's permanent wedge.
	waitObserved(t, c, ObservedPaused)
	resumeRes := c.Resume(context.Background())
	if resumeRes.Outcome != OutcomeAccepted {
		t.Fatalf("resume after the race: %v (%v)", resumeRes.Outcome, resumeRes.Err)
	}
	rf.waitCount(t, 2)
	rf.at(1).markReady() // this factory is readySignaling=true throughout
	waitObserved(t, c, ObservedRunning)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
