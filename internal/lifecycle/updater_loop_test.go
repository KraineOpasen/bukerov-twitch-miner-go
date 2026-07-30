package lifecycle

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fakeUpdaterLoop is a controllable stand-in for Config.UpdaterRun: it
// signals startedCh once per invocation, then either returns promptly on
// ctx.Done() (the well-behaved default) or, when ignoreCtx is set, keeps
// running past cancellation until the test explicitly lets it finish via
// release() — modeling a stuck updater cycle for the join-timeout test.
type fakeUpdaterLoop struct {
	calls     int32
	startedCh chan struct{}
	ignoreCtx bool
	released  chan struct{}
}

func newFakeUpdaterLoop() *fakeUpdaterLoop {
	return &fakeUpdaterLoop{startedCh: make(chan struct{}, 1), released: make(chan struct{})}
}

func (f *fakeUpdaterLoop) run(ctx context.Context) {
	atomic.AddInt32(&f.calls, 1)
	select {
	case f.startedCh <- struct{}{}:
	default:
	}
	if f.ignoreCtx {
		<-f.released
		return
	}
	<-ctx.Done()
}

func (f *fakeUpdaterLoop) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-f.startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("fake updater loop never started")
	}
}

// shrinkUpdaterJoinTimeout replaces the package-level updaterJoinTimeout with
// a tiny duration for the duration of one test, restoring the original on
// cleanup — the join-timeout test needs the real select in Run's defer to
// actually fire quickly rather than waiting out the 3s production default.
func shrinkUpdaterJoinTimeout(t *testing.T) {
	t.Helper()
	orig := updaterJoinTimeout
	updaterJoinTimeout = 20 * time.Millisecond
	t.Cleanup(func() { updaterJoinTimeout = orig })
}

// (migrated S2, design v6 F16/contract §10.12): a controller running a
// generation, with a fake UpdaterRun loop wired in exactly the shape the
// production updater.Options.OnUpdate -> Controller.UpdateApplied adapter
// will use (b3), invokes UpdateApplied() the same way that adapter would.
// Controller.Run must return nil (update-exit, I31: never os.Exit), the
// generation must be torn down exactly once, and the updater loop's own
// goroutine must already have returned — its derived context cancelled and
// its run() call back home — strictly BEFORE Run returns, not merely
// "eventually" afterwards. This migrates the invariant covered by the
// now-deleted internal/miner TestRunAutoUpdateShutdownStaysSuccessful to the
// process-level owner (design v6 §7).
func TestUpdaterLoopUpdateAppliedTeardownAndJoinBeforeRunReturns(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: false})

	loop := newFakeUpdaterLoop()
	loopReturned := make(chan struct{})
	updaterRun := func(ctx context.Context) {
		loop.run(ctx)
		close(loopReturned)
	}

	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock(), UpdaterRun: updaterRun}
	c := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(c, ctx)

	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedRunning)
	loop.waitStarted(t)

	// Drive the update-exit path exactly like the production adapter's
	// updater.Options.OnUpdate would.
	c.UpdateApplied()

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Controller.Run did not return after UpdateApplied")
	}
	if runErr != nil {
		t.Fatalf("Run() returned %v, want nil (update-exit never os.Exits, I31)", runErr)
	}

	// The updater loop must have already returned BY THE TIME Run returned
	// (joined, not merely cancelled) — a non-blocking check is exactly right
	// here: Run's own defer only returns after either the join succeeded or
	// the join timed out, and this loop is well-behaved (returns promptly on
	// ctx.Done()), so by the time <-done fired above, loopReturned must
	// already be closed.
	select {
	case <-loopReturned:
	default:
		t.Fatal("updater loop had not returned by the time Controller.Run returned (not joined before exit)")
	}

	if got := atomic.LoadInt32(&loop.calls); got != 1 {
		t.Fatalf("updater loop invoked %d times, want exactly 1", got)
	}
	if got := rf.at(0).ran; got != 1 {
		t.Fatalf("generation Run invoked %d times, want exactly 1 (torn down exactly once)", got)
	}

	snap := c.Snapshot()
	if snap.Observed != ObservedExiting {
		t.Fatalf("observed = %v, want exiting", snap.Observed)
	}

	// (44) OnUpdate/UpdateApplied is idempotent even with the loop wired: a
	// second call must not panic, block, or change the outcome.
	c.UpdateApplied()
}

// (11a) Joined-with-bound: an updater loop that ignores cancellation entirely
// makes Run's join wait out updaterJoinTimeout, log the timeout path, and
// still return — Run must never hang on a stuck updater cycle.
func TestUpdaterLoopJoinTimesOutAndRunStillReturns(t *testing.T) {
	shrinkUpdaterJoinTimeout(t)

	rf := newRunnerFactory()
	rf.ignoreCt = true // the generation waits for an explicit finish(), not ctx
	pers := newFakePersistence(LoadResult{Found: false})

	loop := newFakeUpdaterLoop()
	loop.ignoreCtx = true // never returns on ctx.Done(); only release() ends it

	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock(), UpdaterRun: loop.run}
	c := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)

	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedRunning)
	loop.waitStarted(t)

	cancel() // process-shutdown
	rf.at(0).finish(nil)

	var runErr error
	select {
	case runErr = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Controller.Run did not return; a stuck updater loop must not hang shutdown")
	}
	if runErr != nil {
		t.Fatalf("Run() returned %v, want nil (clean process-shutdown)", runErr)
	}

	// Release the loop so its goroutine doesn't leak past the test.
	close(loop.released)
}

// (11b) The updater loop is started exactly once per process: repeated
// pause/resume cycles (which build fresh generations) must not spawn a
// second updater-loop goroutine — UpdaterRun is a Run-level concern, not a
// per-generation one.
func TestUpdaterLoopStartedExactlyOnce(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: false})

	loop := newFakeUpdaterLoop()

	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock(), UpdaterRun: loop.run}
	c := New(cfg)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(c, ctx)

	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedRunning)
	loop.waitStarted(t)

	// Drive a full pause -> resume cycle: a second generation is built, but
	// the updater loop must not be restarted alongside it.
	if res := c.Pause(context.Background()); res.Outcome != OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}
	rf.at(0).finish(nil)
	waitObserved(t, c, ObservedPaused)

	if res := c.Resume(context.Background()); res.Outcome != OutcomeAccepted {
		t.Fatalf("resume: %v (%v)", res.Outcome, res.Err)
	}
	rf.waitCount(t, 2)
	rf.at(1).waitStarted(t)
	waitObserved(t, c, ObservedRunning)

	if got := atomic.LoadInt32(&loop.calls); got != 1 {
		t.Fatalf("updater loop invoked %d times across a restart cycle, want exactly 1", got)
	}

	cancel()
	rf.at(1).finish(nil)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Controller.Run did not return on shutdown")
	}
}

// (B6) Arbitration with the updater loop wired: shutdown/update priority
// over an in-flight/pending command (design v6 §5.2.5) still holds when a
// real UpdaterRun loop is running alongside the worker, not just when the
// test drives UpdateApplied() directly with no loop attached.
func TestUpdaterLoopWiredUpdateAppliedStillPreemptsPendingCommand(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: false})

	loop := newFakeUpdaterLoop()

	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock(), UpdaterRun: loop.run}
	c := New(cfg)

	enter := make(chan struct{})
	resume := make(chan struct{})
	c.testBeforeSelect = func() {
		enter <- struct{}{}
		<-resume
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(c, ctx)

	<-enter
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	loop.waitStarted(t)
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
}
