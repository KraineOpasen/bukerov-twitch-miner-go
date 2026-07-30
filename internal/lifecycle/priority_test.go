package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
)

// (27) A stale completion for an old, already-superseded generation,
// delivered WHILE the worker is actively (synchronously) awaiting the
// CURRENT generation's own teardown, is discarded by awaitGeneration's own
// mismatch loop rather than being mistaken for the wait it is actually
// blocked on — the current snapshot is unaffected until the real
// completion arrives.
func TestStaleCompletionDuringActiveAwaitIsDiscarded(t *testing.T) {
	rf := newRunnerFactory()
	rf.ignoreCt = true
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}
	waitObserved(t, c, ObservedPausing)

	// Inject a stale completion for a generation token that was never
	// current, while the worker is genuinely blocked awaiting generation 1.
	c.doneCh <- generationResult{gen: 424242, err: errors.New("stale, ignore me")}

	// The worker must still be waiting for generation 1 specifically —
	// give it a moment, then prove nothing changed yet.
	time.Sleep(20 * time.Millisecond)
	if c.Snapshot().Observed != ObservedPausing {
		t.Fatalf("stale completion changed the in-flight transition: observed=%v", c.Snapshot().Observed)
	}

	// Now let the REAL generation finish; the pause must still complete
	// correctly.
	rf.at(0).finish(nil)
	waitObserved(t, c, ObservedPaused)

	cancel()
	<-done
}

// (28) An update-applied signal preempts an accepted-but-not-yet-dispatched
// command: its terminal is published with reason=updater (the ACTUAL
// current observed, not the command's own target), the slot is released,
// Snapshot() never hangs, and Controller.Run returns nil (no os.Exit, I31).
//
// This is deliberately made deterministic with the testBeforeSelect hook:
// without it, whether cmdCh or updateCh "wins" a given loop iteration's
// select is an OS-scheduling race (Go's select has no ordering among
// simultaneously-ready cases) — sometimes the pause would race ahead and
// actually complete before update-applied had a chance to preempt it,
// which would make this test flaky rather than proving the invariant.
func TestUpdateAppliedPreemptsPendingCommand(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: false})
	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()}
	c := New(cfg)

	enter := make(chan struct{})
	resume := make(chan struct{})
	c.testBeforeSelect = func() {
		enter <- struct{}{}
		<-resume
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)
	defer cancel()

	// The worker is paused at the top of its very FIRST loop iteration —
	// reconciliation (which runs before the loop starts) has already
	// started generation 1, but the loop has not entered any select yet.
	// Do the setup here, while it cannot possibly observe either channel:
	// an accepted-but-undispatched command AND update-applied, eliminating
	// the OS-scheduling race entirely (the hook only fires once per
	// COMPLETED iteration, and iteration 1's select would otherwise never
	// complete on its own — so this is the one point available to inject
	// both before release).
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
	c.UpdateApplied()
	resume <- struct{}{}

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Controller.Run did not return after update-applied")
	}
	if runErr != nil {
		t.Fatalf("Run() returned %v, want nil (update-exit never os.Exits, I31)", runErr)
	}

	snap := c.Snapshot()
	if snap.Observed != ObservedExiting {
		t.Fatalf("observed = %v, want exiting", snap.Observed)
	}
	if snap.Transition != TransitionNone {
		t.Fatalf("transition = %v, want none (slot released)", snap.Transition)
	}
	if snap.Observed == ObservedPaused {
		t.Fatal("the preempted pause must not have been allowed to actually complete")
	}

	found := false
	for _, e := range events.Recent(50) {
		if e.Type == events.TypeLifecycleCommandDeferredToRestart {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected a lifecycle_command_deferred_to_restart ring event")
	}

	cancel()
}

// (29) Controller.Run does not return before the current generation's Run
// actually returns, even under process-shutdown (ctx cancellation) — the
// generation's ctx is a descendant of Run's ctx, so cancellation reaches it
// directly, but Run itself must still block on the real completion.
func TestProcessShutdownWaitsForGenerationToActuallyReturn(t *testing.T) {
	rf := newRunnerFactory()
	rf.ignoreCt = true // the generation ignores ctx cancellation entirely,
	// until the test explicitly calls finish() — modeling a hung teardown.
	c, ctx, cancel, done := newRunningController(t, rf)
	_ = ctx
	defer cancel()

	cancel() // process-shutdown

	// Run must NOT have returned yet: the generation is still "running"
	// until the test explicitly lets it finish.
	select {
	case err := <-done:
		t.Fatalf("Run() returned (%v) before the generation's own Run call returned", err)
	case <-time.After(150 * time.Millisecond):
		// expected: still blocked
	}

	if c.Snapshot().Observed == ObservedExiting {
		t.Fatal("exiting must not be published before the generation actually returns")
	}

	rf.at(0).finish(nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() returned %v, want nil (clean shutdown)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run() did not return after the generation finally returned")
	}
}
