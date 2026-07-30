package lifecycle

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// (6) A duplicate pause is idempotent, both while a first pause is already
// steady-state paused and while one is actively in flight (observed=pausing).
func TestDuplicatePauseIsIdempotent(t *testing.T) {
	rf := newRunnerFactory()
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	first := c.Pause(context.Background())
	if first.Outcome != OutcomeAccepted {
		t.Fatalf("first pause: %v (%v)", first.Outcome, first.Err)
	}
	waitObserved(t, c, ObservedPaused)

	second := c.Pause(context.Background())
	if second.Outcome != OutcomeIdempotent {
		t.Fatalf("second pause (already paused): got %v, want idempotent", second.Outcome)
	}

	cancel()
	<-done
}

func TestDuplicatePauseWhileActivelyPausingIsIdempotent(t *testing.T) {
	rf := newRunnerFactory()
	rf.ignoreCt = true
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	first := c.Pause(context.Background())
	if first.Outcome != OutcomeAccepted {
		t.Fatalf("first pause: %v (%v)", first.Outcome, first.Err)
	}
	waitObserved(t, c, ObservedPausing)

	second := c.Pause(context.Background())
	if second.Outcome != OutcomeIdempotent && second.Outcome != OutcomeRejected {
		t.Fatalf("second pause while pausing: got %v, want idempotent or rejected-by-slot", second.Outcome)
	}

	rf.at(0).finish(nil)
	waitObserved(t, c, ObservedPaused)
	cancel()
	<-done
}

// (7) N concurrent identical commands against a running controller resolve
// deterministically: exactly one is accepted, every other is either
// rejected (409, if it raced the narrow pending window) or idempotent (if
// it landed once the transition was already published) — never a second
// Accepted, and the system converges to exactly one new generation's worth
// of state change.
func TestConcurrentCommandsResolveDeterministically(t *testing.T) {
	rf := newRunnerFactory()
	rf.ignoreCt = true
	c, _, cancel, done := newRunningController(t, rf)
	defer cancel()

	const n = 10
	var wg sync.WaitGroup
	results := make([]SubmitResult, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = c.Pause(context.Background())
		}(i)
	}
	wg.Wait()

	accepted := 0
	for i, r := range results {
		switch r.Outcome {
		case OutcomeAccepted:
			accepted++
		case OutcomeIdempotent, OutcomeRejected:
			// both are valid outcomes for a loser of the race (design v6
			// §5.2: idempotent once the transition is published, rejected
			// by the slot during the narrow pending window).
		default:
			t.Fatalf("result %d: unexpected outcome %v", i, r.Outcome)
		}
	}
	if accepted != 1 {
		t.Fatalf("expected exactly 1 accepted pause, got %d (results=%+v)", accepted, results)
	}

	rf.at(0).finish(nil)
	waitObserved(t, c, ObservedPaused)

	if rf.count() != 1 {
		t.Fatalf("expected exactly 1 generation ever built, got %d", rf.count())
	}

	cancel()
	<-done
}

// (9) Persistence.Save is called, and completes, BEFORE the worker is ever
// signaled — not just before it publishes the transitional observed state.
//
// Two independent barriers make the one assertion that matters airtight
// rather than a timing guess:
//   - Save itself is held open (armSaveBarrier) so the caller goroutine is
//     provably still inside Submit's persist call when we look.
//   - The worker is ALSO independently pinned before it ever reaches its
//     first select (armOnce, the same seam dispatch_race_test.go uses to
//     construct its races) so it cannot race ahead and drain c.cmdCh on its
//     own — without this, the worker (idle in its select, ready to receive
//     the instant anything is sent) drains a wrongly-early send before the
//     test ever gets to look, and the very reordering this test exists to
//     catch would go unnoticed almost every run.
//
// With both held, c.cmdCh MUST still be empty: the send into it (Submit,
// lifecycle.go) happens strictly after Save returns, and nothing else can
// have consumed it either.
func TestPersistHappensBeforeWorkerTransition(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: false})

	saveEntered := make(chan struct{}, 1)
	pers.onSave = func(d DesiredState, reason, cmdID string) {
		// At the instant Save is invoked, the worker must not yet have
		// published the transitional "pausing" state (design v6 §5.2: the
		// caller persists BEFORE signaling the worker).
		saveEntered <- struct{}{}
	}
	releaseSave := pers.armSaveBarrier()

	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()}
	c := New(cfg)

	// Hold the worker at the TOP of its very first loop iteration (same
	// rendezvous point TestPauseAcceptedRacesSpontaneousDeath uses):
	// reconcile has already launched generation 1, but the worker has not
	// yet reached the select that would let it dequeue c.cmdCh.
	enter, releaseWorker := armOnce(c, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(c, ctx)

	<-enter
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	if got := c.Snapshot().Observed; got != ObservedRunning {
		t.Fatalf("observed before pausing the worker = %v, want running", got)
	}

	resCh := make(chan SubmitResult, 1)
	go func() { resCh <- c.Pause(context.Background()) }()

	select {
	case <-saveEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("Save was never called")
	}

	// Save is blocked in flight on the barrier, AND the worker is
	// independently held before its select: with correct ordering (persist,
	// then signal) the send into cmdCh cannot have happened yet, and even
	// if it wrongly had, the worker could not yet have drained it — this is
	// deterministic, not a timing guess.
	if n := len(c.cmdCh); n != 0 {
		t.Fatalf("cmdCh has %d pending signal(s) while Save is still in flight; want 0 (worker must not be signaled before persist completes)", n)
	}

	releaseWorker()
	releaseSave()

	res := <-resCh
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}

	waitObserved(t, c, ObservedPaused)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// (10) A persistence failure leaves in-memory desired/observed completely
// unchanged, releases the slot exactly once, and surfaces the error to the
// caller — no generation action is ever taken, and no signal ever reaches
// c.cmdCh (a send guarded behind the same "did Save fail" branch a
// reordering bug would bypass).
//
// The worker is pinned before its first select (armOnce — see
// TestPersistHappensBeforeWorkerTransition's doc comment for why this is
// what makes the cmdCh assertion below airtight rather than a race the
// worker usually wins by draining first): Submit's Save call fails
// synchronously here, so there is no need for a separate Save barrier too.
func TestPersistFailureLeavesStateUnchanged(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: false})
	wantErr := errors.New("db is busy")
	pers.setSaveErr(wantErr)

	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()}
	c := New(cfg)

	enter, releaseWorker := armOnce(c, 1)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runInBackground(c, ctx)

	<-enter
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	if got := c.Snapshot().Observed; got != ObservedRunning {
		t.Fatalf("observed before pausing the worker = %v, want running", got)
	}

	before := c.Snapshot()

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeRejected {
		t.Fatalf("pause with failing persist: got %v, want rejected", res.Outcome)
	}
	if res.Err == nil || !errors.Is(res.Err, wantErr) {
		t.Fatalf("pause error = %v, want wrapping %v", res.Err, wantErr)
	}
	if n := len(c.cmdCh); n != 0 {
		t.Fatalf("cmdCh has %d pending signal(s) after a failed persist; want 0 (no signal may leak to the worker on a failed persist)", n)
	}

	releaseWorker()

	after := c.Snapshot()
	if after.Desired != before.Desired || after.Observed != before.Observed {
		t.Fatalf("state changed on persist failure: before=%+v after=%+v", before, after)
	}
	if after.Transition != TransitionNone {
		t.Fatalf("transition not cleared after persist failure: %v", after.Transition)
	}
	if !after.Capabilities.CanPause {
		t.Fatal("slot was not released after persist failure (CanPause still false)")
	}

	// The generation is completely undisturbed: still exactly one, still
	// running, never touched.
	if rf.count() != 1 {
		t.Fatalf("a persist failure must not touch the generation, got %d generations", rf.count())
	}

	// Now let a real pause go through to prove the controller is not stuck.
	pers.setSaveErr(nil)
	res2 := c.Pause(context.Background())
	if res2.Outcome != OutcomeAccepted {
		t.Fatalf("pause after clearing persist error: %v (%v)", res2.Outcome, res2.Err)
	}
	waitObserved(t, c, ObservedPaused)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
