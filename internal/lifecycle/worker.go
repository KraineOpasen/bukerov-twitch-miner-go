package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
)

// RetryBackoffSchedule is the base delay before retry N of a failed
// generation start; past the last entry every retry waits the final
// (capped) value — the same shape as internal/miner's
// startupBackoffSchedule (design v6 item 12). Package-level so tests can
// shrink it to make retry tests fast and deterministic.
var RetryBackoffSchedule = []time.Duration{
	5 * time.Second,
	15 * time.Second,
	30 * time.Second,
	60 * time.Second,
}

// teardownSlowThreshold is how long a generation's teardown (or a start's
// cancellation) may run before it is logged as slow (design v6 §5.3в:
// "событие teardown_slow(threshold, package var)"). Package-level so tests
// can shrink it; measured against the injectable Clock seam, never the
// wall clock, so no test needs a real sleep to exercise it.
var teardownSlowThreshold = 5 * time.Second

// retryBackoff returns the jittered delay before retry number attempt
// (1-based): the schedule's entry for that attempt, capped at its last
// value, with ±20% jitter so many deployments retrying at once don't hit
// Twitch (or whatever the Runner talks to) in lockstep.
func retryBackoff(attempt int) time.Duration {
	idx := attempt - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(RetryBackoffSchedule) {
		idx = len(RetryBackoffSchedule) - 1
	}
	base := RetryBackoffSchedule[idx]
	jitter := (rand.Float64() - 0.5) * 0.4
	return time.Duration(float64(base) * (1 + jitter))
}

// Run drives the controller until the process should exit: either the
// parent ctx is cancelled (process-shutdown, design v6 §5.3д/item 19 — the
// generation's ctx is a direct descendant of ctx, so a signal cancels
// whichever generation is current without the worker's involvement) or
// UpdateApplied is called (update-exit, item 19 — always returns nil, no
// os.Exit anywhere, I31). Run performs startup reconciliation (design v6
// §5.4) before serving its first command, then loops for the rest of the
// process's life; it is the ONLY goroutine that ever starts, tears down, or
// classifies the completion of a generation (package doc).
func (c *Controller) Run(ctx context.Context) error {
	c.runCtx = ctx
	c.reconcileAndMaybeStart(ctx)

	for {
		if c.testBeforeSelect != nil {
			c.testBeforeSelect()
		}

		// Priority pre-check (design v6 §5.2 item 5 / §7: process-shutdown
		// > update-applied > user command): if either priority channel is
		// already ready, handle it before even looking at cmdCh/doneCh/the
		// retry timer, so an accepted-but-not-yet-dispatched command is
		// reliably preempted rather than raced against Go's pseudo-random
		// select among simultaneously-ready cases.
		select {
		case <-ctx.Done():
			return c.handleProcessShutdown()
		case <-c.updateCh:
			return c.handleUpdateApplied()
		default:
		}

		select {
		case <-ctx.Done():
			return c.handleProcessShutdown()

		case <-c.updateCh:
			return c.handleUpdateApplied()

		case pc := <-c.cmdCh:
			// pc may already have been resolved by a spontaneous
			// classification that ran first (doneCh consumed while pc was
			// still sitting here, both channels ready at once — orchestrator
			// concurrency review, defect 1): validate it is STILL the slot's
			// occupant before touching anything. Dispatching (or
			// superseding) a stale pc would either deadlock awaiting a
			// generation completion already consumed, or corrupt whatever
			// different command has since taken the slot.
			stillCurrent := c.state.isCurrentPending(pc)

			// Go's select has no ordering among simultaneously-ready cases:
			// cmdCh can win this select even when ctx/updateCh became ready
			// at essentially the same instant (the outer pre-check above
			// only catches priority events ready STRICTLY BEFORE this
			// select was entered). Re-check here, with the command already
			// in hand, so priority still wins instead of racing dispatch.
			select {
			case <-ctx.Done():
				if stillCurrent {
					c.supersedeUndispatched(pc, ReasonSignal)
				} else {
					recordDiscardedStaleCommand(pc)
				}
				return c.handleProcessShutdown()
			case <-c.updateCh:
				if stillCurrent {
					c.supersedeUndispatched(pc, ReasonUpdater)
				} else {
					recordDiscardedStaleCommand(pc)
				}
				return c.handleUpdateApplied()
			default:
				if !stillCurrent {
					recordDiscardedStaleCommand(pc)
				} else if exitErr, shouldExit := c.dispatch(pc); shouldExit {
					return exitErr
				}
			}

		case res := <-c.doneCh:
			if exitErr, shouldExit := c.handleSpontaneousCompletion(res); shouldExit {
				return exitErr
			}

		case <-c.readyChSafe():
			c.handleReady()

		case <-c.retryTimerChan():
			c.retryTimer = nil
			// This fire may already be stale: cancelRetryLocked's
			// timer.Stop() cannot un-send a value already buffered in this
			// SAME timer's channel, so an accepted command that raced the
			// literal fire (armRetryTimer's seq no longer matches) must be
			// discarded rather than launching a second, spurious generation
			// (orchestrator concurrency review, defect 1's retry-timer
			// race).
			if c.state.currentRetrySeq() != c.retryTimerSeq {
				recordRetryFireDiscardedStale()
			} else {
				c.state.clearRetry()
				c.startGeneration(ReasonRetry, "", "")
			}
		}
	}
}

// retryTimerChan returns the current retry timer's fire channel, or nil
// (a select case on a nil channel never fires) when no retry is armed.
func (c *Controller) retryTimerChan() <-chan time.Time {
	if c.retryTimer == nil {
		return nil
	}
	return c.retryTimer.C()
}

// readyChSafe returns the current generation's Ready() channel while the
// worker is awaiting it, or nil (a select case on a nil channel never
// fires) when nothing is awaiting readiness right now.
func (c *Controller) readyChSafe() <-chan struct{} {
	if c.awaitingStart == nil {
		return nil
	}
	return c.readyCh
}

// reconcileAndMaybeStart implements design v6 §5.4 in full: resolve the
// persisted desired state (missing row/valid/corrupt/read-error), apply the
// ForceRunning override (in-memory only), publish the resolved state, and
// — only when the resolved desired is running — launch the first
// generation.
func (c *Controller) reconcileAndMaybeStart(ctx context.Context) {
	desired, observed, reason, lastErr := c.resolvePersistedState(ctx)

	override := false
	if c.cfg.ForceRunning {
		override = true
		if desired == DesiredRunning {
			recordRing(events.TypeLifecycleEnvOverride, "LIFECYCLE_FORCE_RUNNING set; already running")
		} else {
			recordRing(events.TypeLifecycleEnvOverride,
				fmt.Sprintf("LIFECYCLE_FORCE_RUNNING forced in-memory running; durable desired stays %q", desired))
		}
		slog.Warn("lifecycle: LIFECYCLE_FORCE_RUNNING override honored at startup reconciliation",
			"persisted_desired", desired)
		desired = DesiredRunning
		observed = ObservedStarting
		reason = ReasonReconcile
	}

	c.state.reconcile(desired, observed, reason, lastErr, override)

	if desired == DesiredRunning {
		c.startGeneration(reason, "", "")
	}
}

// resolvePersistedState reads the persisted desired state and classifies it
// into one of four outcomes (design v6 §5.4): a valid row, a missing
// row/table (back-compat -> running), an unrecognized value (fail-closed ->
// paused, durable row REWRITTEN with the raw value in its reason), or a
// plain read error (in-memory -> paused, durable row left untouched).
func (c *Controller) resolvePersistedState(ctx context.Context) (DesiredState, ObservedState, Reason, string) {
	loaded, err := c.cfg.Persistence.Load(ctx)
	if err == nil {
		d := DesiredRunning
		if loaded.Found {
			d = loaded.Desired
		}
		return d, observedForDesiredAtBoot(d), ReasonReconcile, ""
	}

	var corrupt *CorruptStateError
	if errors.As(err, &corrupt) {
		reasonStr := fmt.Sprintf("fail-closed: was %q", corrupt.Raw)
		if saveErr := c.cfg.Persistence.Save(ctx, DesiredPaused, reasonStr, ""); saveErr != nil {
			slog.Error("lifecycle: failed to rewrite corrupt persisted desired state", "error", saveErr)
		}
		recordRing(events.TypeLifecycleStateCorrupt, corrupt.Error())
		return DesiredPaused, ObservedPaused, Reason(reasonStr), corrupt.Error()
	}

	// A plain read error (I/O, database.ErrClosed, ...): do NOT rewrite —
	// we don't know the row was actually bad, only that we couldn't read
	// it. In-memory desired fails closed to paused; the next command's own
	// Load/Save retries against the store normally.
	recordRing(events.TypeLifecycleStateReadFailed, err.Error())
	return DesiredPaused, ObservedPaused, ReasonReconcile, err.Error()
}

func observedForDesiredAtBoot(d DesiredState) ObservedState {
	switch d {
	case DesiredPaused:
		return ObservedPaused
	case DesiredStopped:
		return ObservedStopped
	default:
		return ObservedStarting
	}
}

// dispatch runs one dequeued command to completion, returning (exitErr,
// true) only in the one case where a command's OWN processing must end
// Controller.Run entirely (a dirty teardown while desired=running — design
// v6 §5.3(a)).
func (c *Controller) dispatch(pc *pendingCommand) (error, bool) {
	switch pc.action {
	case actionPersistOnlySetObserved:
		wasFailed := c.state.snapshot().Observed == ObservedFailed
		c.state.publishTerminal(steadyObservedFor(pc.target), pc.reason, "", true)
		recordTerminalForTarget(pc.target, pc.reason, pc.id)
		if wasFailed {
			// design v6 revision #2(b): a user command that terminates a
			// failed lineage (pause/stop out of "failed") ends the streak
			// just as surely as reaching ready does.
			c.retryAttempt = 0
			c.recoveryPending = false
		}
		return nil, false

	case actionPersistOnlyKeepDegraded:
		c.state.releasePendingOnly()
		recordCommandAcceptedDegradedSwitch(pc)
		return nil, false

	case actionStart:
		c.startGeneration(pc.reason, pc.id, pc.cmd)
		return nil, false

	case actionTeardown:
		return c.runTeardown(pc)

	case actionTeardownThenStart:
		return c.runRestart(pc)

	case actionCancelStart:
		return c.runCancelStart(pc)
	}
	return nil, false
}

// startGeneration launches a fresh generation. It is the single entry point
// for every path that begins one with no existing generation to tear down
// first: startup reconciliation (reason=reconcile, cmd=""), an accepted
// resume/restart from paused/stopped/failed (reason=user, cmd=the
// originating command), and a fired retry timer (reason=retry, cmd="").
// cmdID is the originating command's id, or "" when there is none
// (reconcile/retry — these are the design v6 §5.1 "starting" row's
// SLOT-FREE case: pc is nil, so accept() can admit a real pause/stop
// cancel-start while this is in flight).
func (c *Controller) startGeneration(reason Reason, cmdID string, cmd Command) {
	// Track whether THIS generation is emerging from a failed/retry
	// lineage — but do NOT declare recovery yet. Recovery is declared
	// later, in maybeDeclareRecovery, only once this generation actually
	// becomes READY (or, for a slot-held start, some other genuine
	// non-failure terminal is reached) — see maybeDeclareRecovery's doc
	// comment for why merely launching is not enough (design v6 item 12:
	// backoff must keep growing across a whole crash-loop).
	c.recoveryPending = c.state.snapshot().Observed == ObservedFailed

	now := c.cfg.Clock.Now()
	c.state.beginTransition(ObservedStarting, TransitionStart, now)
	recordTransitionStarted(cmd, reason)

	var pc *pendingCommand
	if cmdID != "" {
		pc = &pendingCommand{id: cmdID, cmd: cmd, reason: reason, target: DesiredRunning, action: actionStart}
	}
	c.launchFreshGeneration(pc, reason)
}

// maybeDeclareRecovery resets the retry-attempt counter and records
// lifecycle_failed_recovered the first time a generation that emerged from
// a failed/retry lineage (recoveryPending) reaches READY — proof that the
// retry chain actually produced a working, durably-up generation rather
// than one that merely launched before dying again. Called from
// completeStart (the ready-reached path) and from the wind-down paths that
// end a lineage via a genuine non-failure terminal (a restart's old
// generation tearing down cleanly). NOT called from the "self-death while
// desired=running" / "died before ready" paths, which are themselves NEW
// failures and must keep the backoff growing (design v6 item 12/§5.3).
func (c *Controller) maybeDeclareRecovery() {
	if !c.recoveryPending {
		return
	}
	c.recoveryPending = false
	if c.retryAttempt != 0 {
		c.retryAttempt = 0
		recordRing(events.TypeLifecycleFailedRecovered, "")
	}
}

// launchFreshGeneration does the actual factory-call/spawn work shared by
// startGeneration and runRestart's post-teardown relaunch. If the built
// Runner implements ReadySignaler, observed stays "starting"/"restarting"
// (whatever the caller already published via beginTransition) until Ready
// fires — the main loop's readyChSafe/handleReady picks that up — making
// design v6 §5.1's "starting" row genuinely reachable (see lifecycle.go's
// ReadySignaler doc comment). A Runner without it is treated as ready
// immediately (today's behavior, and what b3's Miner adapter currently
// gets since Miner has no such signal yet).
//
// pc is nil for a phantom (reconcile/retry-driven) start — the slot stays
// free throughout, so a real pause/stop can be independently accepted
// (occupying the slot itself) to cancel it; see runCancelStart.
func (c *Controller) launchFreshGeneration(pc *pendingCommand, reason Reason) {
	now := c.cfg.Clock.Now()

	c.currentGen++
	gen := c.currentGen
	gctx, cancel := context.WithCancel(c.runCtx)
	c.currentGenCancel = cancel

	c.state.beginGeneration(gen, now)
	recordGenerationStartAttempted(gen, reason)

	runner := c.cfg.Factory()
	var ready <-chan struct{}
	if rs, ok := runner.(ReadySignaler); ok {
		ready = rs.Ready()
	}

	c.generationLive = true
	go c.launchGeneration(gen, runner, gctx)

	if ready == nil {
		c.completeStart(gen, pc, reason)
		return
	}
	c.readyCh = ready
	c.awaitingStart = &startInfo{gen: gen, pc: pc, reason: reason}
}

// completeStart publishes ObservedRunning for a generation that has become
// ready (or never needed to wait, because its Runner has no ReadySignaler)
// and, when a real command drove this start, releases the slot with that
// command's terminal. Called either synchronously from launchFreshGeneration
// (instant-ready case) or from handleReady (once Ready() actually fires).
func (c *Controller) completeStart(gen uint64, pc *pendingCommand, reason Reason) {
	cmdID, cmd := "", Command("")
	if pc != nil {
		cmdID, cmd = pc.id, pc.cmd
	}
	if pc != nil {
		c.state.publishTerminal(ObservedRunning, reason, "", true)
	} else {
		// Phantom (slot-free) start: this generation never held the
		// pending-command slot, so its own completion must not touch
		// whatever (unrelated) command a concurrent Submit may have placed
		// there since — see publishTerminalNoSlot's doc comment.
		c.state.publishTerminalNoSlot(ObservedRunning, reason, "", true)
	}
	recordGenerationRunning(gen, cmd, cmdID)
	c.maybeDeclareRecovery()
}

// handleReady is the main loop's case when the current generation's
// Ready() channel fires: the "starting"/"restarting" window ends and
// ObservedRunning is published (design v6 §5.1).
func (c *Controller) handleReady() {
	info := c.awaitingStart
	c.awaitingStart = nil
	c.readyCh = nil
	c.completeStart(info.gen, info.pc, info.reason)
}

// launchGeneration runs one generation's Runner to completion and reports
// its result, tagged with its generation token, on doneCh. This is the
// ONLY method that ever calls Runner.Run, and it is called exactly once per
// generation (design v6 item 10).
func (c *Controller) launchGeneration(gen uint64, r Runner, ctx context.Context) {
	err := r.Run(ctx)
	c.doneCh <- generationResult{gen: gen, err: err}
}

// awaitGeneration blocks (holding no controller lock — design v6 I9/I10)
// until the generation identified by gen actually completes, discarding any
// mismatched entries it happens to receive first (defensive: under the
// single-active-generation invariant this loop should never actually
// iterate more than once for a mismatch, since gen is always the current
// generation when awaitGeneration is called). label identifies the calling
// operation (a Command's string form, or "shutdown"/"update-exit") purely
// for the teardown_started/completed/slow slog lines (design v6 §11/§5.3в)
// — teardown_slow fires at most once per wait, timed against the
// injectable Clock seam so no test needs a real sleep to exercise it.
func (c *Controller) awaitGeneration(gen uint64, label string) generationResult {
	start := c.cfg.Clock.Now()
	recordTeardownStarted(label)

	timer := c.cfg.Clock.NewTimer(teardownSlowThreshold)
	timerActive := true
	defer func() {
		if timerActive {
			timer.Stop()
		}
	}()

	for {
		var timerC <-chan time.Time
		if timerActive {
			timerC = timer.C()
		}
		select {
		case res := <-c.doneCh:
			if res.gen == gen {
				if gen == c.currentGen {
					c.generationLive = false
				}
				recordTeardownCompleted(label, c.cfg.Clock.Now().Sub(start))
				return res
			}
			recordStaleCompletionIgnored(res.gen)
		case <-timerC:
			timerActive = false
			recordTeardownSlow(label, teardownSlowThreshold)
		}
	}
}

// runTeardown tears down the current generation for a pause/stop command
// (design v6 §5.1 "running" row). It never itself triggers a process exit:
// by the time teardown runs, desired has already been committed to
// paused/stopped (Submit commits it before signaling the worker), so a
// dirty teardown here always falls under §5.3(b) (degraded), never §5.3(a).
func (c *Controller) runTeardown(pc *pendingCommand) (error, bool) {
	transition, observed := TransitionPause, ObservedPausing
	if pc.cmd == CommandStop {
		transition, observed = TransitionStop, ObservedStopping
	}
	c.state.beginTransition(observed, transition, c.cfg.Clock.Now())
	recordTransitionStarted(pc.cmd, pc.reason)

	gen := c.currentGen
	if c.currentGenCancel != nil {
		c.currentGenCancel()
	}
	res := c.awaitGeneration(gen, string(pc.cmd))

	c.finishWindDown(res.err, pc.target, pc.reason, pc.id)
	return nil, false
}

// runRestart tears down the current generation and, unless that teardown
// was dirty, launches a fresh one (design v6 §5.1 "running"/restart cell).
// desired stays DesiredRunning throughout a restart, so a dirty teardown of
// the OLD generation here is §5.3(a): Controller.Run returns the teardown
// error instead of holding degraded (degraded is reserved for
// desired∈{paused,stopped}). The relaunched generation goes through the
// SAME readiness gate as any other start (launchFreshGeneration): observed
// stays "restarting" (never flips to "starting") until it becomes ready,
// consistent with the "restarting" row covering the whole teardown+launch
// span.
func (c *Controller) runRestart(pc *pendingCommand) (error, bool) {
	c.state.beginTransition(ObservedRestarting, TransitionRestart, c.cfg.Clock.Now())
	recordTransitionStarted(pc.cmd, pc.reason)

	oldGen := c.currentGen
	if c.currentGenCancel != nil {
		c.currentGenCancel()
	}
	oldRes := c.awaitGeneration(oldGen, string(pc.cmd))

	if IsDirtyTeardownError(oldRes.err) {
		// design v6 §5.3(a): desired=running -> Controller.Run returns the
		// teardown error (process-exit path); degraded is reserved for
		// desired∈{paused,stopped} (§5.3(b), finishWindDown/
		// handleSpontaneousCompletion). No ring event: the process is
		// exiting, and the non-nil Run error is itself the loud signal
		// (matching today's "shutdown drain incomplete" -> exit 1 path).
		err := fmt.Errorf("lifecycle: dirty teardown during restart (desired=running): %w", oldRes.err)
		return err, true
	}

	// The old generation tore down cleanly (or with an ordinary, non-dirty
	// error) — that is a genuine non-failure terminal for it, so a pending
	// recovery is declared before moving on to the new generation.
	c.maybeDeclareRecovery()

	c.launchFreshGeneration(pc, pc.reason)
	return nil, false
}

// runCancelStart implements design v6 §5.1's "starting" row, slot-free
// case: a pause/stop command was accepted (occupying the slot itself, per
// the normal accept()/dispatch() path) to cancel an in-flight
// reconcile/retry-driven start that has not yet become ready. It cancels
// the generation's ctx, awaits its actual return, and classifies the
// result through the SAME §5.2.6/§5.3(b) rules an ordinary teardown uses
// (finishWindDown): an expected "context canceled"-class error is NOT
// LastError/failed/retry; a dirty-teardown-class error enters degraded.
func (c *Controller) runCancelStart(pc *pendingCommand) (error, bool) {
	gen := c.currentGen
	// This start was, by construction, slot-free until this very command
	// occupied it (tableLookup's "starting" row only offers
	// cellAcceptCancelStart when slotHeld is false) — so awaitingStart's pc
	// is nil and there is nothing else to preserve; clear it, this
	// generation is no longer "awaiting readiness", it is being cancelled.
	c.awaitingStart = nil
	c.readyCh = nil

	recordStartCancelled(gen, pc.cmd)
	if c.currentGenCancel != nil {
		c.currentGenCancel()
	}
	res := c.awaitGeneration(gen, string(pc.cmd))

	c.finishWindDown(res.err, pc.target, pc.reason, pc.id)

	// design v6 revision #2(b): a user command that cancels a (necessarily
	// failed-lineage, since only retry/reconcile ever run with the slot
	// free — a resume/restart would have occupied it) start ends that
	// streak, exactly like reaching ready would have.
	c.retryAttempt = 0
	c.recoveryPending = false
	return nil, false
}

// finishWindDown classifies a completed teardown against the "dirty
// teardown" rule (design v6 §5.3(b): desired∈{paused,stopped} -> degraded,
// no new generation until process restart) and otherwise publishes the
// requested steady terminal (paused/stopped).
func (c *Controller) finishWindDown(err error, target DesiredState, reason Reason, cmdID string) {
	if IsDirtyTeardownError(err) {
		msg := describeFailure(err)
		c.state.enterDegraded(msg)
		recordRing(events.TypeLifecycleTransitionDegraded, msg)
		return
	}
	lastErr := classifyWindDownLastError(err)
	observed := steadyObservedFor(target)
	c.state.publishTerminal(observed, reason, lastErr, true)
	recordTerminalForTarget(target, reason, cmdID)
}

// handleSpontaneousCompletion classifies a generation completion the worker
// was NOT actively, synchronously waiting for (design v6 §5.2 item 5): a
// generation the operator most recently commanded (or a phantom
// reconcile/retry start) either reached "running" (or was still awaiting
// readiness) and then died on its own, WHILE a command may or may not
// ALSO have already been accepted (occupying the slot) to do something
// about it — Submit's accept/persist/commit and a generation's own death
// are fully independent, concurrent events, so either ordering is
// possible.
//
// Classification follows design v6 §5.2.5's literal rule: by the SLOT's
// occupant when one exists ("классифицируется по ЦЕЛИ команды в слоте"),
// falling back to the current desired only when the slot is empty
// (orchestrator concurrency review, defect 2 — the slot's target is
// authoritative regardless of whether Submit's own desired-commit has
// landed yet, and regardless of whether this generation was steady-running
// or still awaitingStart when it died). target=running (an empty slot with
// desired=running, or a resume/restart command occupying it) means this was
// NOT supposed to happen — a startup failure eligible for retry (design v6
// §5.3's "самосмерть", generalized: applies equally to an operator's own
// resume, not only to boot). target∈{paused,stopped} (an empty slot with
// desired paused/stopped, or a pause/stop/cancel-start command occupying
// it) means the generation was already on its way out — an expected,
// successful wind-down, exactly like that command's own teardown would
// have classified it, ending any failed-lineage tracking in flight.
//
// Whichever branch runs, this is authoritative for pc if one occupies the
// slot: dispatch()'s isCurrentPending check (design v6 concurrency review,
// defect 1) recognizes and discards that SAME pc when it is later dequeued
// from cmdCh, since publishTerminal below already released its slot.
func (c *Controller) handleSpontaneousCompletion(res generationResult) (error, bool) {
	if res.gen != c.currentGen {
		recordStaleCompletionIgnored(res.gen)
		return nil, false
	}
	c.generationLive = false

	if c.awaitingStart != nil && c.awaitingStart.gen == res.gen {
		c.awaitingStart = nil
		c.readyCh = nil
	}

	pending, desired := c.state.pendingAndDesired()
	target := desired
	reason := c.state.currentReason()
	cmdID := ""
	if pending != nil {
		target = pending.target
		reason = pending.reason
		cmdID = pending.id
		// pending occupies the slot AND this classification is about to
		// resolve/release it — but the ONLY way pending can be non-nil
		// here (transition never reached dispatch()'s beginTransition,
		// since that's mutually exclusive with THIS iteration having
		// picked doneCh instead of cmdCh) is if its pc is STILL physically
		// sitting, undequeued, in the capacity-1 cmdCh. Drain it now,
		// synchronously with releasing the slot: cmdCh's buffer must never
		// be left holding a stale entry across iterations — a concurrent
		// Submit's later, non-blocking send to a still-full channel would
		// otherwise silently no-op, accepting a command whose signal to
		// the worker is simply dropped (orchestrator concurrency review,
		// defect 1 — the failure mode dispatch's isCurrentPending check
		// alone cannot close, since that check only guards a pc already IN
		// HAND, not one still queued behind an undrained stale entry).
		c.drainStaleQueuedCommand()
	}

	if target == DesiredRunning {
		return c.classifyUnhealthyCompletion(res.err)
	}

	// target ∈ {paused, stopped}: an expected (or dirty) wind-down —
	// finishWindDown applies the exact same §5.3(b)/§5.2.6 rules an
	// ordinary command-driven teardown/cancel-start uses, and (via
	// publishTerminal) releases pending's slot if one is occupying it.
	c.finishWindDown(res.err, target, reason, cmdID)
	if pending != nil {
		// A command (pause/stop/cancel-start) is what ended this
		// generation's story — that ends any failed-lineage tracking just
		// as surely as reaching ready or an explicit teardown would
		// (design v6 revision #2(b)).
		c.retryAttempt = 0
		c.recoveryPending = false
	} else {
		c.maybeDeclareRecovery()
	}
	return nil, false
}

// classifyUnhealthyCompletion is design v6 §5.3's "самосмерть" rule,
// shared by (a) a steady, previously-ready generation dying on its own and
// (b) a generation dying before it ever became ready: a dirty-teardown-
// class error means desired=running -> Controller.Run returns it
// (process-exit, §5.3(a)); anything else is a startup failure — observed
// becomes failed, an automatic retry is scheduled, and (deduped, design v6
// §11) a lifecycle_failed_entered ring record is written only for the
// FIRST failure of a streak.
func (c *Controller) classifyUnhealthyCompletion(err error) (error, bool) {
	if IsDirtyTeardownError(err) {
		return fmt.Errorf("lifecycle: dirty teardown (desired=running): %w", err), true
	}
	c.retryAttempt++
	msg := describeFailure(err)
	c.state.publishTerminal(ObservedFailed, ReasonStartupFailure, msg, false)
	if c.retryAttempt == 1 {
		recordRing(events.TypeLifecycleFailedEntered, msg)
	}
	c.armRetryTimer()
	return nil, false
}

// armRetryTimer schedules the next automatic retry attempt (design v6
// §5.3 self-death rule / item 12): a single, worker-owned timer with
// capped, jittered backoff. NextRetryAt is published so Snapshot()
// reflects it; the retry-timer channel is polled by Run's main select.
func (c *Controller) armRetryTimer() {
	delay := retryBackoff(c.retryAttempt)
	next := c.cfg.Clock.Now().Add(delay)
	timer := c.cfg.Clock.NewTimer(delay)
	c.retryTimer = timer

	stopped := false
	cancel := func() {
		if !stopped {
			stopped = true
			timer.Stop()
		}
	}
	c.retryTimerSeq = c.state.armRetry(next, cancel)
	recordRetryScheduled(c.retryAttempt, next)
}

// handleProcessShutdown is Run's exit path when ctx (the ctx passed to Run)
// is cancelled (design v6 item 19, §5.3д, M1): the current generation's ctx
// is ALREADY a descendant of ctx, so it is already cancelling/cancelled
// independent of the worker; this method's own cancel call is therefore
// often redundant but harmless, and its job is really to WAIT — without any
// budget — for the generation to actually return, so Controller.Run never
// returns before the runtime it owns has actually stopped.
func (c *Controller) handleProcessShutdown() error {
	c.drainUndispatchedCommand(ReasonSignal)
	c.awaitingStart = nil
	c.readyCh = nil
	err := c.tearDownForExit()
	c.state.publishTerminal(ObservedExiting, ReasonSignal, classifyWindDownLastError(err), true)
	recordRing(events.TypeLifecycleShutdownTookPriority, "")
	return err
}

// handleUpdateApplied is Run's exit path when UpdateApplied is called
// (design v6 §7/item 19): tear down the current generation cleanly and
// return nil unconditionally — no os.Exit anywhere (I31); App/main's normal
// Shutdown path takes it from there.
func (c *Controller) handleUpdateApplied() error {
	c.drainUndispatchedCommand(ReasonUpdater)
	c.awaitingStart = nil
	c.readyCh = nil
	_ = c.tearDownForExit()
	c.state.publishTerminal(ObservedExiting, ReasonUpdater, "", true)
	recordRing(events.TypeLifecycleUpdaterTookPriority, "")
	return nil
}

// tearDownForExit cancels and waits for the current generation, if one is
// still live, returning its completion error (nil if none was live or it
// exited cleanly).
func (c *Controller) tearDownForExit() error {
	if !c.generationLive {
		return nil
	}
	if c.currentGenCancel != nil {
		c.currentGenCancel()
	}
	res := c.awaitGeneration(c.currentGen, "shutdown")
	return res.err
}

// drainStaleQueuedCommand non-blockingly drains a single stale command
// message from cmdCh, if one is sitting there — used by
// handleSpontaneousCompletion right when it finds the slot still occupied
// by a command whose transition never got a chance to reach dispatch()
// (design v6 concurrency review, defect 1): cmdCh's capacity-1 buffer must
// never be left holding that entry across loop iterations, or a
// concurrent Submit's later send to the still-full channel would silently
// no-op. A no-op itself if there is nothing to drain (e.g. a command whose
// generation was already properly dispatched — awaitingStart — and is only
// now dying before ready; that pc was dequeued long ago).
func (c *Controller) drainStaleQueuedCommand() {
	select {
	case pc := <-c.cmdCh:
		recordDiscardedStaleCommand(pc)
	default:
	}
}

// drainUndispatchedCommand implements design v6 §5.2 item 5, subcase (a):
// a command was accepted (occupying the slot, already sent to cmdCh) but
// the worker had not yet dequeued/dispatched it when a priority event fired
// — preempt it by publishing terminal = the ACTUAL current observed (NOT
// the command's own target — publishing the target would fabricate a state
// that was never reached) with a LastError explaining the supersession, and
// release the slot BEFORE the caller goes on to publish "exiting". The
// durable intent Submit already persisted is untouched: it takes effect
// after the process restarts and reconciliation runs again.
//
// Subcase (b) — a transition already dispatched — needs no code here: this
// worker is single-threaded, so if dispatch() had already started running a
// command's teardown/build, it always runs to ITS OWN completion (reason
// unchanged) before Run's loop can reach this priority check again.
func (c *Controller) drainUndispatchedCommand(reason Reason) {
	snap := c.state.snapshot()
	if snap.Transition != TransitionPending {
		return
	}
	select {
	case pc := <-c.cmdCh:
		c.supersedeUndispatched(pc, reason)
	default:
		// Unreachable under the single-worker invariant (nothing else ever
		// reads cmdCh), kept as a defensive no-op rather than a panic.
	}
}

// supersedeUndispatched publishes the terminal for a command that was
// accepted (occupying the slot) but never actually dispatched, because a
// priority event (process-shutdown/update-applied) preempted it first —
// design v6 §5.2 item 5, subcase (a). Terminal = the ACTUAL current
// observed (never the command's own target — that state was never
// reached), with a LastError explaining the supersession; the durable
// intent Submit already persisted is untouched and takes effect after the
// process restarts and reconciliation runs again.
func (c *Controller) supersedeUndispatched(pc *pendingCommand, reason Reason) {
	snap := c.state.snapshot()
	c.state.publishTerminal(snap.Observed, reason,
		"command superseded by update-exit/shutdown; will take effect after restart", true)
	recordRing(events.TypeLifecycleCommandDeferredToRestart, string(pc.cmd))
}

// describeFailure always returns a non-empty, human-readable description of
// why a generation is being classified as a startup failure — even a nil
// error (the Runner returned cleanly but desired=running expected it to
// keep going) is worth surfacing as LastError, since silence here would
// look identical to a healthy generation.
func describeFailure(err error) string {
	if err == nil {
		return "generation exited unexpectedly"
	}
	return err.Error()
}

// classifyWindDownLastError implements design v6 §5.2.6: a context-
// canceled-class error produced by a pause/stop/restart-teardown/shutdown/
// start-cancellation we ourselves requested is EXPECTED — it must not
// surface as LastError. Any other error is reported verbatim (sanitized:
// these are internal Go errors with no secrets in them by construction).
func classifyWindDownLastError(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return ""
	}
	return err.Error()
}
