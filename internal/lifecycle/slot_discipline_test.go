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

// Spec re-review BLOCKER (post-F4b Q3 consolidated corrective): MAJOR 6's
// override-resume branch (accept()'s cellIdempotent case, active only when
// s.override && cmd==Resume && observed==running) is ITSELF an
// accept-class outcome — it occupies the slot — but was added without the
// same s.pending!=nil guard BLOCKER 1 added to the default accept-class
// branch (now: occupySlotLocked, the single centralized helper every
// occupying branch routes through, precisely so a FUTURE occupying cell
// cannot repeat this class of bug by forgetting to duplicate the check).
// Omitting the guard reproduced BLOCKER 1's exact scenario verbatim with
// ForceRunning:true and Resume in place of Stop: a resume racing a
// still-in-flight, undispatched pcA would overwrite s.pending, its own
// cmdCh send would be silently dropped behind pcA's still-buffered one,
// and the worker would later discard pcA via isCurrentPending while the
// resume's own pc never dispatches — Transition stuck at pending forever,
// every later Submit 409s, controller permanently wedged.
//
// This pins the WHOLE CLASS, not a single instance: the exact BLOCKER-1
// race window (pcA's slot occupied, undispatched, while Transition has
// been reset to none by a phantom start's own completion — see
// TestAcceptRejectsWhenSlotOccupiedDespiteTransitionCleared's own doc
// comment for the full mechanics) is replayed for EVERY combination of a
// second command (Stop, which always goes through the default
// accept-class branch regardless of override; Resume, which is idempotent
// UNLESS override is active, in which case it goes through the
// override-resume occupying branch this BLOCKER is about) crossed with
// override on/off. This proves both that the guard now covers every
// occupying branch, AND — the resume-no-override row — that override
// cannot accidentally turn a genuinely idempotent cell into a 409: design
// v6 §5.1's idempotent cells must keep returning idempotent while the
// slot is held.
func TestSlotBusyGuardCoversEveryOccupyingBranch(t *testing.T) {
	cases := []struct {
		name         string
		forceRunning bool
		submit       func(c *Controller) SubmitResult
		wantOutcome  Outcome
	}{
		{
			name:         "stop-no-override",
			forceRunning: false,
			submit:       func(c *Controller) SubmitResult { return c.Stop(context.Background()) },
			wantOutcome:  OutcomeRejected,
		},
		{
			name:         "stop-with-override",
			forceRunning: true,
			submit:       func(c *Controller) SubmitResult { return c.Stop(context.Background()) },
			wantOutcome:  OutcomeRejected,
		},
		{
			name:         "resume-no-override-stays-idempotent",
			forceRunning: false,
			submit:       func(c *Controller) SubmitResult { return c.Resume(context.Background()) },
			wantOutcome:  OutcomeIdempotent,
		},
		{
			name:         "resume-with-override-is-the-blocker",
			forceRunning: true,
			submit:       func(c *Controller) SubmitResult { return c.Resume(context.Background()) },
			wantOutcome:  OutcomeRejected,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rf := newRunnerFactory()
			rf.readySignaling = true
			var pers *fakePersistence
			if tc.forceRunning {
				// Persisted paused, overridden to running in-memory by
				// ForceRunning — this is what makes s.override true for
				// the whole process lifetime.
				pers = newFakePersistence(LoadResult{Found: true, Desired: DesiredPaused})
			} else {
				pers = newFakePersistence(LoadResult{Found: false}) // missing row -> running
			}
			cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock(), ForceRunning: tc.forceRunning}
			c := New(cfg)

			// Manually-stepped testBeforeSelect: pauses the worker at the
			// TOP of every loop iteration until passthrough is set —
			// identical technique to
			// TestAcceptRejectsWhenSlotOccupiedDespiteTransitionCleared,
			// see that test's doc comment for why both barriers
			// (armSaveBarrier + this) are needed.
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

			<-ackCh
			rf.waitCount(t, 1)
			rf.at(0).waitStarted(t)
			if got := c.Snapshot().Observed; got != ObservedStarting {
				t.Fatalf("observed before releasing iteration 1 = %v, want starting", got)
			}
			if tc.forceRunning && !c.Snapshot().Override {
				t.Fatal("test setup: Override should be true (ForceRunning honored)")
			}
			stepCh <- struct{}{} // let iteration 1 enter its select; nothing is ready yet, so it blocks there.

			// Hold Pause's own persist open: accept() (occupying the slot
			// as pcA, via the "starting" row's slot-free cancel-start
			// cell — unaffected by override, which only special-cases
			// Resume) completes in-memory immediately, but Submit's send
			// into cmdCh is delayed for as long as we hold this barrier.
			release := pers.armSaveBarrier()

			pauseResCh := make(chan SubmitResult, 1)
			go func() { pauseResCh <- c.Pause(context.Background()) }()
			waitFor(t, 2*time.Second, func() bool { return c.Snapshot().Transition == TransitionPending })

			// The phantom start reaches ready while Pause's cmdCh send is
			// still pending: iteration 1's blocked select wakes on
			// readyChSafe alone and runs completeStart ->
			// publishTerminalNoSlot, resetting Transition to none and
			// Observed to running WITHOUT touching pcA's slot.
			rf.at(0).markReady()
			<-ackCh
			waitObserved(t, c, ObservedRunning)

			if got := c.Snapshot().Transition; got != TransitionNone {
				t.Fatalf("transition = %v, want none (phantom completion must reset it even though pcA's slot is still occupied)", got)
			}

			// Release Pause's Save: Submit proceeds to actually send pcA
			// into cmdCh and returns. The worker is STILL frozen before
			// iteration 2's select, so pcA is provably sitting there,
			// undispatched, while we submit the second command below.
			release()
			pauseRes := <-pauseResCh
			if pauseRes.Outcome != OutcomeAccepted {
				t.Fatalf("pause: %v (%v)", pauseRes.Outcome, pauseRes.Err)
			}

			before := len(c.cmdCh)
			res := tc.submit(c)
			if res.Outcome != tc.wantOutcome {
				t.Fatalf("second command: got %v, want %v (%v)", res.Outcome, tc.wantOutcome, res.Err)
			}
			if tc.wantOutcome == OutcomeRejected {
				if after := len(c.cmdCh); after != before {
					t.Fatalf("cmdCh length changed from %d to %d — a rejected command must never touch cmdCh (no send, no drain)", before, after)
				}
			}

			// Let the worker run freely from here on.
			passthrough.Store(true)
			stepCh <- struct{}{}

			// The controller must remain fully live regardless of the
			// second command's outcome: pcA's own pause still reaches its
			// terminal (paused), and a subsequent command is still
			// accepted normally afterwards — distinguishing a correct
			// 409/idempotent result from the bug's permanent wedge.
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
		})
	}
}
