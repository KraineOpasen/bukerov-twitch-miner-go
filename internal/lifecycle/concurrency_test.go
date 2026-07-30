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

// MAJOR 3 (F4b Q3 consolidated corrective): Submit's persist-failure
// rollback (revertPendingOnPersistFailure) and persist-success commit
// (commitDesiredAndReason) both used to touch statusState unconditionally,
// with no idea whether the command they belong to (pc) was even STILL the
// slot's occupant by the time Save finally returned. A slow Save racing pc's
// OWN generation dying spontaneously (independently classified and
// released by the worker while Save is still in flight — exactly the
// mechanism slot_discipline_test.go's BLOCKER-1 regression also exploits)
// leaves the slot free for a SECOND, completely unrelated command (pcB) to
// be accepted and start its own generation — all while pcA's Submit
// goroutine is still blocked in Save. When pcA's Save finally returns
// (here: with a failure), its stale rollback must be a no-op — NOT clear
// pcB's still-legitimately-occupied slot/transition out from under it
// (which would corrupt capabilities — a concurrent command would then be
// wrongly evaluated as if the slot were free — and, once pcB's own
// generation reaches ready, leave CommandID stuck on pcA's stale value
// since publishTerminal's "released := s.pending" would read the
// wrongly-nulled slot instead of pcB).
func TestPersistFailureRollbackScopedToOwnCommand(t *testing.T) {
	rf := newRunnerFactory()
	rf.readySignaling = true
	pers := newFakePersistence(LoadResult{Found: false}) // missing row -> running
	wantErr := errors.New("db is busy")

	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()}
	c, cancel, done := buildAndRun(cfg)
	defer cancel()

	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedStarting)
	rf.at(0).markReady()
	waitObserved(t, c, ObservedRunning)

	// pcA = Pause, whose own Save is about to be slow and eventually fail.
	pers.setSaveErr(wantErr)
	releaseA := pers.armSaveBarrier() // one-shot: applies to pcA's Save call only.

	pcAResCh := make(chan SubmitResult, 1)
	go func() { pcAResCh <- c.Pause(context.Background()) }()
	waitFor(t, 2*time.Second, func() bool { return c.Snapshot().Transition == TransitionPending })

	// pcA's own generation dies spontaneously WHILE its Save is still in
	// flight: handleSpontaneousCompletion classifies by the SLOT's own
	// target (paused, since pcA occupies it) and releases pcA's slot —
	// entirely independently of pcA's own still-blocked Submit call.
	rf.at(0).finish(errors.New("boom"))
	waitObserved(t, c, ObservedPaused)

	// pcB = Resume, submitted (accepted, persisted, and dispatched —
	// launching a SECOND generation) entirely AFTER pcA's slot was
	// released above. pcA's Submit goroutine is still blocked in Save the
	// whole time.
	pers.setSaveErr(nil)
	pcBRes := c.Resume(context.Background())
	if pcBRes.Outcome != OutcomeAccepted {
		t.Fatalf("resume (pcB): %v (%v)", pcBRes.Outcome, pcBRes.Err)
	}
	rf.waitCount(t, 2)
	rf.at(1).waitStarted(t)
	// Deliberately not marked ready yet: pcB's own generation must still be
	// legitimately "in flight" (slot occupied) when pcA's stale Save
	// returns below.
	waitObserved(t, c, ObservedStarting)

	// Now release pcA's failing Save: its Submit's persist-failure
	// rollback must be a no-op (pcA is no longer the slot's occupant — pcB
	// is) — not clear pcB's still-legitimately-in-flight slot/transition
	// out from under it.
	releaseA()
	pcARes := <-pcAResCh
	if pcARes.Outcome != OutcomeRejected {
		t.Fatalf("pause (pcA) with failing persist: got %v, want rejected", pcARes.Outcome)
	}

	if snap := c.Snapshot(); snap.Transition == TransitionNone {
		t.Fatal("pcA's stale persist-failure rollback cleared pcB's slot/transition out from under it")
	}

	// pcB is genuinely unaffected: it still reaches its OWN terminal
	// correctly, with its OWN CommandID, once ITS generation actually
	// becomes ready.
	rf.at(1).markReady()
	waitObserved(t, c, ObservedRunning)
	if got := c.Snapshot().CommandID; got != pcBRes.CommandID {
		t.Fatalf("CommandID = %q after pcB's generation became ready, want pcB's own id %q (pcA's stale rollback corrupted the slot before release, so publishTerminal never saw pcB as the occupant)", got, pcBRes.CommandID)
	}
	if rf.count() != 2 {
		t.Fatalf("expected exactly 2 generations (1 initial + 1 from pcB's resume), got %d", rf.count())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// The symmetric case: a slow SUCCEEDING Save for pcA must not overwrite
// in-memory desired/reason with pcA's now-stale target after a DIFFERENT
// command (pcB) has since committed its own, newer desired state — the
// commit side of MAJOR 3's same scoping fix.
func TestPersistSuccessCommitScopedToOwnCommand(t *testing.T) {
	rf := newRunnerFactory()
	rf.readySignaling = true
	pers := newFakePersistence(LoadResult{Found: false}) // missing row -> running
	cfg := Config{Factory: rf.Factory, Persistence: pers, Clock: newFakeClock()}
	c, cancel, done := buildAndRun(cfg)
	defer cancel()

	rf.waitCount(t, 1)
	rf.at(0).waitStarted(t)
	waitObserved(t, c, ObservedStarting)
	rf.at(0).markReady()
	waitObserved(t, c, ObservedRunning)

	// pcA = Pause, whose own (succeeding) Save is about to be slow.
	releaseA := pers.armSaveBarrier() // one-shot: applies to pcA's Save call only.

	pcAResCh := make(chan SubmitResult, 1)
	go func() { pcAResCh <- c.Pause(context.Background()) }()
	waitFor(t, 2*time.Second, func() bool { return c.Snapshot().Transition == TransitionPending })

	// pcA's own generation dies spontaneously WHILE its Save is still in
	// flight, releasing its slot independently of its own still-blocked
	// Submit call.
	rf.at(0).finish(errors.New("boom"))
	waitObserved(t, c, ObservedPaused)

	// pcB = Resume, fully accepted/persisted/dispatched — committing
	// desired=running — entirely AFTER pcA's slot was released above.
	pcBRes := c.Resume(context.Background())
	if pcBRes.Outcome != OutcomeAccepted {
		t.Fatalf("resume (pcB): %v (%v)", pcBRes.Outcome, pcBRes.Err)
	}
	if got := c.Snapshot().Desired; got != DesiredRunning {
		t.Fatalf("desired after pcB committed = %v, want running", got)
	}
	rf.waitCount(t, 2)
	rf.at(1).waitStarted(t)
	waitObserved(t, c, ObservedStarting)

	// Now release pcA's SUCCEEDING Save: its stale commit must be a no-op
	// (pcA is no longer the slot's occupant) — not overwrite desired=running
	// (pcB's own commit) back to paused (pcA's own, now-stale target).
	releaseA()
	pcARes := <-pcAResCh
	if pcARes.Outcome != OutcomeAccepted {
		t.Fatalf("pause (pcA), stale but nominally succeeding: %v (%v)", pcARes.Outcome, pcARes.Err)
	}

	if got := c.Snapshot().Desired; got != DesiredRunning {
		t.Fatalf("desired = %v after pcA's stale commit, want still running (pcB's commit must not be overwritten)", got)
	}

	rf.at(1).markReady()
	waitObserved(t, c, ObservedRunning)
	if got := c.Snapshot().CommandID; got != pcBRes.CommandID {
		t.Fatalf("CommandID = %q, want pcB's own id %q", got, pcBRes.CommandID)
	}
	if rf.count() != 2 {
		t.Fatalf("expected exactly 2 generations, got %d", rf.count())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}
