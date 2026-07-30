package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"
)

// newRunningController builds a Controller whose persisted desired state is
// "running" (so Run immediately starts a first generation) using rf as its
// Factory, and starts it in the background. It returns once the first
// generation has actually started.
func newRunningController(t *testing.T, rf *runnerFactory) (*Controller, context.Context, context.CancelFunc, <-chan error) {
	t.Helper()
	cfg := Config{
		Factory:     rf.Factory,
		Persistence: newFakePersistence(LoadResult{Found: false}), // missing row -> running
		Clock:       newFakeClock(),
	}
	c := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedRunning)
	return c, ctx, cancel, done
}

// (1) Never more than one active generation, even under concurrent
// commands. fakeRunner itself panics if this invariant is ever violated
// (shared "active" counter); this test drives several overlapping
// pause/resume/restart cycles to try to trigger that panic.
func TestNeverMoreThanOneActiveGeneration(t *testing.T) {
	rf := newRunnerFactory()
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	for i := 0; i < 5; i++ {
		res := c.Restart(context.Background())
		if res.Outcome != OutcomeAccepted {
			t.Fatalf("restart %d: got %v, want accepted (%v)", i, res.Outcome, res.Err)
		}
		// waitObserved(running) alone is not enough here: observed is
		// ALREADY running from the previous iteration's completion, so it
		// would trivially "pass" before this restart has even been
		// dequeued. Wait for a NEW generation to actually appear too.
		rf.waitCount(t, i+2)
		waitObserved(t, c, ObservedRunning)
	}

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause: got %v (%v)", res.Outcome, res.Err)
	}
	waitObserved(t, c, ObservedPaused)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// (2) A given Runner's Run is never called twice — enforced structurally by
// fakeRunner's panic guard; this test just exercises enough transitions
// that a violation would have fired, then double-checks each runner's own
// call count.
func TestRunnerRunNeverCalledTwice(t *testing.T) {
	rf := newRunnerFactory()
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	for i := 0; i < 3; i++ {
		if res := c.Restart(context.Background()); res.Outcome != OutcomeAccepted {
			t.Fatalf("restart %d: %v (%v)", i, res.Outcome, res.Err)
		}
		// waitObserved(running) alone is not enough: observed is ALREADY
		// running from the previous iteration before this restart is even
		// dequeued. Wait for the NEW runner to actually exist AND start.
		rf.waitCount(t, i+2)
		rf.at(i + 1).waitStarted(t)
		waitObserved(t, c, ObservedRunning)
	}

	cancel()
	<-done

	if got := rf.count(); got != 4 {
		t.Fatalf("expected 4 generations (1 initial + 3 restarts), got %d", got)
	}
	for i := 0; i < rf.count(); i++ {
		r := rf.at(i)
		if r.ran != 1 {
			t.Fatalf("runner %d: Run called %d times, want 1", i, r.ran)
		}
	}
}

// (3) Restart builds a fresh Runner (never reuses the old one).
func TestRestartBuildsFreshRunner(t *testing.T) {
	rf := newRunnerFactory()
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	first := rf.latest(t)

	res := c.Restart(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("restart: %v (%v)", res.Outcome, res.Err)
	}
	rf.waitCount(t, 2)
	rf.at(1).waitStarted(t)
	waitObserved(t, c, ObservedRunning)

	if rf.count() != 2 {
		t.Fatalf("expected a second runner to be built, count=%d", rf.count())
	}
	second := rf.at(1)
	if second == first {
		t.Fatal("restart reused the old Runner instance")
	}

	cancel()
	<-done
}

// (4) The old generation reaches its own terminal (its Run call actually
// returns) strictly before the new generation is published as running.
func TestOldGenerationTerminalBeforeNewRunning(t *testing.T) {
	rf := newRunnerFactory()
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	res := c.Restart(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("restart: %v (%v)", res.Outcome, res.Err)
	}
	rf.waitCount(t, 2)
	rf.at(1).waitStarted(t) // ensures "start-2" has actually been recorded
	waitObserved(t, c, ObservedRunning)

	events := rf.rec.snapshot()
	iReturn1 := indexOf(events, "return-1")
	iStart2 := indexOf(events, "start-2")
	if iReturn1 == -1 || iStart2 == -1 {
		t.Fatalf("missing expected events in %v", events)
	}
	if iReturn1 > iStart2 {
		t.Fatalf("generation 1 returned AFTER generation 2 started: %v", events)
	}

	cancel()
	<-done
}

// (5) A stale completion (an old, already-superseded generation's result)
// is ignored: it does not change the current snapshot, and is recognizably
// logged as ignored (behavior asserted here via snapshot stability; the
// slog line itself is not asserted, matching this package's other tests).
func TestStaleCompletionIgnored(t *testing.T) {
	rf := newRunnerFactory()
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	before := c.Snapshot()

	// Inject a stale completion for a generation token that was never
	// current (simulating a very-late report from a long-superseded
	// generation).
	c.doneCh <- generationResult{gen: 9999, err: errors.New("stale boom")}

	// Give the worker a moment to process it, then assert nothing changed.
	waitFor(t, time.Second, func() bool { return true }) // ensure at least one poll tick
	time.Sleep(20 * time.Millisecond)

	after := c.Snapshot()
	if after.Observed != before.Observed || after.Generation != before.Generation {
		t.Fatalf("stale completion changed snapshot: before=%+v after=%+v", before, after)
	}

	cancel()
	<-done
}

// (8) The pending-command slot stays occupied once the worker has dequeued
// a command, all the way to its terminal — a second command submitted
// while a teardown is still in flight is rejected (409-semantics), not
// silently queued or accepted.
func TestSlotStaysOccupiedUntilTerminal(t *testing.T) {
	rf := newRunnerFactory()
	rf.ignoreCt = true // teardown won't resolve until the test calls finish()
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}
	waitObserved(t, c, ObservedPausing)

	// The generation is still being torn down (ignoreCtx=true, no finish()
	// yet) — a second command must be rejected by the slot.
	second := c.Stop(context.Background())
	if second.Outcome != OutcomeRejected {
		t.Fatalf("second command while pausing: got %v, want rejected", second.Outcome)
	}

	rf.at(0).finish(nil)
	waitObserved(t, c, ObservedPaused)

	// Now the slot is free again.
	third := c.Stop(context.Background())
	if third.Outcome != OutcomeAccepted {
		t.Fatalf("stop after pause completed: got %v (%v)", third.Outcome, third.Err)
	}

	cancel()
	<-done
}

// (11) Snapshot() stays readable (returns promptly) while the worker is
// deep inside a slow teardown — statusMu is a leaf lock, never held across
// the blocking wait for a generation to return.
func TestStatusReadableDuringSlowTeardown(t *testing.T) {
	rf := newRunnerFactory()
	rf.ignoreCt = true
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}
	waitObserved(t, c, ObservedPausing)

	snapDone := make(chan Snapshot, 1)
	go func() { snapDone <- c.Snapshot() }()

	select {
	case snap := <-snapDone:
		if snap.Observed != ObservedPausing {
			t.Fatalf("snapshot during teardown = %v, want pausing", snap.Observed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Snapshot() blocked while the worker was mid-teardown")
	}

	rf.at(0).finish(nil)
	waitObserved(t, c, ObservedPaused)
	cancel()
	<-done
}

// (12) No controller lock is held across the blocking teardown call itself
// — proven the same way as (11) but additionally hammering Snapshot()
// concurrently from several goroutines throughout the teardown window.
func TestNoLockHeldDuringBlockingTeardown(t *testing.T) {
	rf := newRunnerFactory()
	rf.ignoreCt = true
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}
	waitObserved(t, c, ObservedPausing)

	stop := make(chan struct{})
	results := make(chan struct{}, 100)
	for i := 0; i < 10; i++ {
		go func() {
			for {
				select {
				case <-stop:
					return
				default:
					c.Snapshot()
					results <- struct{}{}
				}
			}
		}()
	}

	// Drain a good number of successful, non-blocking snapshots while the
	// teardown is still artificially held open.
	for i := 0; i < 50; i++ {
		select {
		case <-results:
		case <-time.After(2 * time.Second):
			t.Fatal("Snapshot() calls stalled while teardown was in flight")
		}
	}
	close(stop)

	rf.at(0).finish(nil)
	waitObserved(t, c, ObservedPaused)
	cancel()
	<-done
}
