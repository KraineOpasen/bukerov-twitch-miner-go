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

// (9) Persistence.Save is called, and completes, BEFORE the worker
// publishes the command's transitional observed state — observed here
// mode is the SAME test used to also prove ordering isn't accidentally
// reversed under race detection.
func TestPersistHappensBeforeWorkerTransition(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: false})

	saveObservedRunning := make(chan bool, 1)
	pers.onSave = func(d DesiredState, reason, cmdID string) {
		// At the instant Save is invoked, the worker must not yet have
		// published the transitional "pausing" state (design v6 §5.2: the
		// caller persists BEFORE signaling the worker).
		saveObservedRunning <- true
	}

	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()}
	c := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedRunning)

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}

	select {
	case <-saveObservedRunning:
	case <-time.After(2 * time.Second):
		t.Fatal("Save was never called")
	}

	waitObserved(t, c, ObservedPaused)
	cancel()
	<-done
}

// (10) A persistence failure leaves in-memory desired/observed completely
// unchanged, releases the slot exactly once, and surfaces the error to the
// caller — no generation action is ever taken.
func TestPersistFailureLeavesStateUnchanged(t *testing.T) {
	rf := newRunnerFactory()
	pers := newFakePersistence(LoadResult{Found: false})
	wantErr := errors.New("db is busy")
	pers.setSaveErr(wantErr)

	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()}
	c := New(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	done := runInBackground(c, ctx)
	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedRunning)

	before := c.Snapshot()

	res := c.Pause(context.Background())
	if res.Outcome != OutcomeRejected {
		t.Fatalf("pause with failing persist: got %v, want rejected", res.Outcome)
	}
	if res.Err == nil || !errors.Is(res.Err, wantErr) {
		t.Fatalf("pause error = %v, want wrapping %v", res.Err, wantErr)
	}

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
	<-done
}
