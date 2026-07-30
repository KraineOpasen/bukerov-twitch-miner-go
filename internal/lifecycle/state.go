package lifecycle

import (
	"fmt"
	"sync"
	"time"
)

// statusState is the statusMu-guarded heart of the controller (package doc:
// "Concurrency model"). Every field below is read/written only while
// holding mu; mu is always a LEAF lock — no method here ever calls a
// Runner, the Persistence port, or blocks on an unready channel, and the
// handful of methods that notify the StatusSink (beginGeneration,
// publishTerminal, enterDegraded) all release mu FIRST (design v6 §6: "под
// statusMu не делается НИ ОДИН вызов наружу") — a slow or misbehaving sink
// can never stall Snapshot() or a concurrent Submit.
type statusState struct {
	mu sync.Mutex

	desired    DesiredState
	observed   ObservedState
	transition Transition
	generation uint64
	commandID  string
	reason     Reason
	lastError  string

	startedAt           time.Time
	transitionStartedAt time.Time
	nextRetryAt         time.Time

	override bool

	// pending is the sole occupant of the capacity-1 command slot; non-nil
	// from Submit's atomic accept until the worker's terminal publish.
	pending *pendingCommand

	// retryCancel stops the worker's own retry timer. accept() calls (and
	// clears) it the instant a command is accepted out of "failed", so the
	// timer can never fire a redundant retry after the operator already
	// acted (design v6 §5.2 step 1).
	retryCancel func()

	// retrySeq increments every time retry bookkeeping changes (armed or
	// cancelled). Go's time.Timer.Stop does NOT drain an already-fired
	// timer's channel — cancelRetryLocked's Stop() call can return false
	// while a fire is already sitting in the worker's private retryTimer's
	// channel, which the worker would otherwise still read on some later
	// loop iteration and act on as if it were a live, un-cancelled retry
	// (a second, spurious generation launch racing whatever command just
	// got accepted out of "failed" — orchestrator concurrency review,
	// defect 1's "armed retry timer -> two live generations" case). The
	// worker captures the seq armRetry returns and compares it against
	// currentRetrySeq() when its retry-timer case actually fires,
	// discarding a stale fire whose seq no longer matches.
	retrySeq uint64

	sink StatusSink
}

// workerAction tells the worker what kind of generation work (if any) an
// accepted command requires. Computed once, at accept time, from the
// transition-table cell that admitted the command — the worker never
// re-derives it.
type workerAction int

const (
	// actionPersistOnlySetObserved: no generation exists (or is allowed) to
	// touch; just flip observed to match the newly-persisted desired
	// (paused<->stopped with no live generation, or out of "failed").
	actionPersistOnlySetObserved workerAction = iota
	// actionPersistOnlyKeepDegraded: same as above, but observed STAYS
	// degraded (design v6 I30: no new generation until process restart).
	actionPersistOnlyKeepDegraded
	// actionStart: launch a fresh generation (paused/stopped/failed -> running).
	actionStart
	// actionTeardown: tear down the current generation only (pause/stop of
	// a running generation).
	actionTeardown
	// actionTeardownThenStart: tear down the current generation, then
	// launch a fresh one (restart of a running generation).
	actionTeardownThenStart
	// actionCancelStart: cancel an in-flight, not-yet-ready, SLOT-FREE
	// start (design v6 §5.1 "starting" row: pause/stop cancels a
	// reconcile/retry-driven start that hasn't reached ready yet).
	actionCancelStart
	// actionPersistOnlyRunning: MAJOR 6 (F4b Q3 consolidated corrective),
	// design v6 §5.4(б) — a resume evaluated against the running row's
	// otherwise-idempotent cell WHILE LIFECYCLE_FORCE_RUNNING's override
	// is active. Deliberately a DISTINCT action from
	// actionPersistOnlySetObserved: that action's dispatch-time
	// generationLive guard exists for a completely different hazard (a
	// stale, race-produced action from the "failed" row, where a live
	// generation would be a corruption to defend against) — here, a live,
	// steady-running generation is the ENTIRELY EXPECTED, correct state
	// (that's exactly why this cell was idempotent in the first place),
	// so dispatch must never tear anything down for it. Only Submit's own
	// persist call is the point of this action; the worker's own
	// handling is a same-value republish of "running", never a
	// generation-touching operation.
	actionPersistOnlyRunning
)

// cellOutcome is one transition-table cell's disposition (design v6 §5.1).
type cellOutcome int

const (
	cellReject cellOutcome = iota
	// cellRejectDegraded: reject with the specific "process restart
	// required" message (resume/restart while degraded).
	cellRejectDegraded
	cellIdempotent
	cellAcceptPersistOnly
	cellAcceptPersistOnlyDegraded
	cellAcceptStart
	cellAcceptTeardown
	cellAcceptRestart
	// cellAcceptCancelStart: the "starting" row's slot-free pause/stop cell
	// — cancel an in-flight reconcile/retry-driven start (design v6 §5.1).
	cellAcceptCancelStart
)

// tableLookup implements design v6 §5.1's transition table EXACTLY, one
// cell per (observed, cmd) pair — including the "starting" row's real
// cancel-start semantics (ReadySignaler makes ObservedStarting a genuinely
// observable, non-instantaneous state; see lifecycle.go's ReadySignaler
// doc comment for why this needs no change to the Runner interface
// itself). desired is needed only for the two "degraded" cells, whose
// outcome depends on which of paused/stopped is already in effect.
// slotHeld distinguishes the "starting" row's two reachable shapes:
//
//   - slot-FREE (reconcile at boot, or a fired retry timer — design v6
//     §5.2: "Retry scheduled... с пустым слотом"): pause/stop are ACCEPTED
//     as a real cancel-start (cellAcceptCancelStart), restart is rejected,
//     a duplicate resume is idempotent.
//   - slot-HELD (an accepted resume/restart command is what's driving this
//     start): pause/stop/restart are all rejected — the exact same "second
//     submit -> 409 by slot" rule every other in-flight transition
//     (pausing/stopping/restarting) already gets, since the slot stays
//     occupied by that command until ITS OWN terminal (running, once
//     ready, or failed) is reached. Only a duplicate resume is idempotent.
func tableLookup(observed ObservedState, desired DesiredState, cmd Command, slotHeld bool) cellOutcome {
	switch observed {
	case ObservedStarting:
		switch cmd {
		case CommandResume:
			return cellIdempotent
		case CommandPause, CommandStop:
			if slotHeld {
				return cellReject
			}
			return cellAcceptCancelStart
		default: // CommandRestart
			return cellReject
		}

	case ObservedRunning:
		switch cmd {
		case CommandPause:
			return cellAcceptTeardown
		case CommandResume:
			return cellIdempotent
		case CommandRestart:
			return cellAcceptRestart
		case CommandStop:
			return cellAcceptTeardown
		}

	case ObservedPausing:
		if cmd == CommandPause {
			return cellIdempotent
		}
		return cellReject

	case ObservedPaused:
		switch cmd {
		case CommandPause:
			return cellIdempotent
		case CommandResume, CommandRestart:
			return cellAcceptStart
		case CommandStop:
			return cellAcceptPersistOnly
		}

	case ObservedStopping:
		if cmd == CommandStop {
			return cellIdempotent
		}
		return cellReject

	case ObservedStopped:
		switch cmd {
		case CommandStop:
			return cellIdempotent
		case CommandResume, CommandRestart:
			return cellAcceptStart
		case CommandPause:
			return cellAcceptPersistOnly
		}

	case ObservedRestarting:
		if cmd == CommandRestart {
			return cellIdempotent
		}
		return cellReject

	case ObservedFailed:
		switch cmd {
		case CommandPause, CommandStop:
			return cellAcceptPersistOnly
		case CommandResume, CommandRestart:
			return cellAcceptStart
		}

	case ObservedDegraded:
		switch cmd {
		case CommandPause:
			if desired == DesiredPaused {
				return cellIdempotent
			}
			return cellAcceptPersistOnlyDegraded
		case CommandStop:
			if desired == DesiredStopped {
				return cellIdempotent
			}
			return cellAcceptPersistOnlyDegraded
		case CommandResume, CommandRestart:
			return cellRejectDegraded
		}

	case ObservedExiting:
		return cellReject
	}
	return cellReject
}

// targetFor is the DesiredState a command drives toward, independent of the
// current observed state.
func targetFor(cmd Command) DesiredState {
	switch cmd {
	case CommandPause:
		return DesiredPaused
	case CommandStop:
		return DesiredStopped
	default: // CommandResume, CommandRestart
		return DesiredRunning
	}
}

// actionFor maps an "accept"-class cellOutcome to the workerAction that
// carries out the command.
func actionFor(o cellOutcome) workerAction {
	switch o {
	case cellAcceptPersistOnly:
		return actionPersistOnlySetObserved
	case cellAcceptPersistOnlyDegraded:
		return actionPersistOnlyKeepDegraded
	case cellAcceptStart:
		return actionStart
	case cellAcceptTeardown:
		return actionTeardown
	case cellAcceptRestart:
		return actionTeardownThenStart
	case cellAcceptCancelStart:
		return actionCancelStart
	}
	return actionPersistOnlySetObserved
}

// steadyObservedFor is the ObservedState a persist-only transition settles
// into once its DesiredState target is reached with no generation involved.
func steadyObservedFor(d DesiredState) ObservedState {
	switch d {
	case DesiredPaused:
		return ObservedPaused
	case DesiredStopped:
		return ObservedStopped
	default:
		return ObservedRunning
	}
}

// acceptKind is accept()'s outcome classification for Submit.
type acceptKind int

const (
	acceptRejected acceptKind = iota
	acceptIdempotent
	acceptOccupied
)

// acceptDecision is accept()'s full result.
type acceptDecision struct {
	kind       acceptKind
	err        error
	existingID string
	pending    *pendingCommand
	// stopRetry, set only for an acceptOccupied decision that cancelled an
	// armed retry timer, is the timer's own stop closure captured while
	// still holding mu (MINOR 9, F4b Q3 consolidated corrective) — the
	// caller (Submit) must invoke it only AFTER accept() has already
	// returned (and thus already released statusMu). nil when no retry
	// needed cancelling.
	stopRetry func()
}

// errProcessRestartRequired is the degraded row's resume/restart rejection
// message (design v6 §5.1, I30).
var errProcessRestartRequired = fmt.Errorf("process restart required")

func errSlotBusy(p *pendingCommand) error {
	if p == nil {
		return fmt.Errorf("lifecycle: a command is already in flight")
	}
	return fmt.Errorf("lifecycle: a command is already in flight (%s, id=%s)", p.cmd, p.id)
}

func errRejectedByState(observed ObservedState, cmd Command) error {
	return fmt.Errorf("lifecycle: %s not valid while observed=%s", cmd, observed)
}

// init prepares a zero-value statusState for use. desired/observed start at
// DesiredRunning/ObservedStarting as harmless placeholders — Controller.Run
// overwrites them via reconcile() before the worker ever serves a command,
// and nothing external can observe the placeholder window in practice
// (Run's reconciliation runs before any caller could plausibly have a
// reference to the Controller to call Submit/Snapshot on).
func (s *statusState) init(sink StatusSink) {
	s.desired = DesiredRunning
	s.observed = ObservedStarting
	s.transition = TransitionNone
	s.sink = sink
}

// snapshot returns a full, consistent point-in-time view.
func (s *statusState) snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Snapshot{
		Desired:             s.desired,
		Observed:            s.observed,
		Transition:          s.transition,
		Generation:          s.generation,
		CommandID:           s.commandID,
		Reason:              s.reason,
		LastError:           s.lastError,
		StartedAt:           s.startedAt,
		TransitionStartedAt: s.transitionStartedAt,
		NextRetryAt:         s.nextRetryAt,
		Capabilities:        s.capabilitiesLocked(),
		Override:            s.override,
	}
}

// capabilitiesLocked derives Capabilities from the current observed/desired
// via the same table Submit uses, so the two can never disagree. Every
// capability is false for as long as the slot is occupied (MINOR 7, F4b Q3
// consolidated corrective) — design v6 §5.2 step 1's "all canX=false"
// covers the WHOLE accepted-through-terminal span (pausing/stopping/
// starting-slot-held/restarting too), not just the narrow pre-dispatch
// Transition==pending window before the worker has even dequeued the
// command.
func (s *statusState) capabilitiesLocked() Capabilities {
	if s.pending != nil {
		return Capabilities{}
	}
	can := func(cmd Command) bool {
		switch tableLookup(s.observed, s.desired, cmd, false) {
		case cellReject, cellRejectDegraded:
			return false
		default:
			return true
		}
	}
	return Capabilities{
		CanPause:   can(CommandPause),
		CanResume:  can(CommandResume),
		CanRestart: can(CommandRestart),
		CanStop:    can(CommandStop),
	}
}

// currentDesired returns the current in-memory desired state.
func (s *statusState) currentDesired() DesiredState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.desired
}

// currentReason returns the current reason (used by the worker when
// publishing a terminal state that doesn't have its own pendingCommand,
// e.g. a spontaneous wind-down classification).
func (s *statusState) currentReason() Reason {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reason
}

// pendingAndDesired returns the current slot occupant (nil if the slot is
// empty) and the current desired state, read together in one atomic
// section. Spontaneous-completion classification (design v6 §5.2.5) reads
// BOTH together: "классифицируется по ЦЕЛИ команды в слоте" — the slot's
// occupant is authoritative whenever one exists (it may be racing ahead of
// Submit's own desired-commit, or simply be the thing that's about to
// resolve this exact generation), and only an EMPTY slot falls back to the
// plain current desired.
func (s *statusState) pendingAndDesired() (*pendingCommand, DesiredState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending, s.desired
}

// accept is Submit's atomic accept step (design v6 §5.2 step 1): one
// critical section that checks the pending slot, looks up the transition
// table, and — for an "accept"-class outcome — occupies the slot and
// cancels any failed-state retry timer, all before persistence ever runs.
func (s *statusState) accept(cmd Command, commandID string) acceptDecision {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Pending window: accepted but not yet dequeued by the worker. ALL
	// capabilities are false here regardless of the table (§5.2 step 1) —
	// a second submit is rejected by the slot alone.
	if s.transition == TransitionPending {
		return acceptDecision{kind: acceptRejected, err: errSlotBusy(s.pending)}
	}

	slotHeld := s.pending != nil
	outcome := tableLookup(s.observed, s.desired, cmd, slotHeld)
	switch outcome {
	case cellReject:
		return acceptDecision{kind: acceptRejected, err: errRejectedByState(s.observed, cmd)}

	case cellRejectDegraded:
		return acceptDecision{kind: acceptRejected, err: errProcessRestartRequired}

	case cellIdempotent:
		// MAJOR 6 (F4b Q3 consolidated corrective), design v6 §5.4(б): a
		// resume evaluated against the running row's idempotent cell
		// WHILE LIFECYCLE_FORCE_RUNNING's override is active must still
		// persist durable=running — otherwise the durable row is left
		// exactly as it was before the override forever, even though the
		// operator explicitly asked to run, defeating the override's own
		// documented purpose of letting a later env-var removal converge
		// cleanly. Restricted to observed==running specifically (matching
		// "observed unchanged" — publishing running while still merely
		// "starting" would be a lie, and the natural ready-completion
		// path already covers that narrower window on its own).
		if s.override && cmd == CommandResume && s.observed == ObservedRunning {
			stop := s.cancelRetryLocked()
			s.transition = TransitionPending
			s.pending = &pendingCommand{
				id:     commandID,
				cmd:    cmd,
				reason: ReasonUser,
				target: DesiredRunning,
				action: actionPersistOnlyRunning,
			}
			return acceptDecision{kind: acceptOccupied, pending: s.pending, stopRetry: stop}
		}
		id := s.commandID
		if s.pending != nil {
			id = s.pending.id
		}
		return acceptDecision{kind: acceptIdempotent, existingID: id}

	default: // every remaining outcome is some flavor of "accept"
		// Defense-in-depth beyond the Transition==pending check above
		// (BLOCKER 1, F4b Q3 consolidated corrective): that check alone
		// only catches the window where the SLOT'S OWN owner has not yet
		// reached a terminal. A slot-FREE phantom start's own completion
		// (publishTerminalNoSlot — worker.go's completeStart when pc==nil)
		// forcibly resets Transition to none WITHOUT touching a DIFFERENT,
		// already-accepted-but-not-yet-dispatched command's slot occupancy
		// (a phantom start never held that slot in the first place). If
		// s.pending is still non-nil here despite Transition reading none,
		// the table above was evaluated against a stale post-completion
		// observed/desired that has no idea a slot is still occupied — most
		// rows (unlike "starting") never consult slotHeld, so an unrelated
		// second command could otherwise be admitted, overwrite s.pending,
		// and silently drop its own cmdCh send behind the still-buffered
		// first command (design v6 §5.2 step 1: slot occupancy — not just
		// Transition — is the actual "second submit -> 409" invariant).
		// Idempotent same-command cells of transitional rows are unaffected
		// (cellIdempotent returns above, before ever reaching here).
		if s.pending != nil {
			return acceptDecision{kind: acceptRejected, err: errSlotBusy(s.pending)}
		}
		stop := s.cancelRetryLocked()
		s.transition = TransitionPending
		s.pending = &pendingCommand{
			id:     commandID,
			cmd:    cmd,
			reason: ReasonUser,
			target: targetFor(cmd),
			action: actionFor(outcome),
		}
		return acceptDecision{kind: acceptOccupied, pending: s.pending, stopRetry: stop}
	}
}

// revertPendingOnPersistFailure undoes accept()'s slot occupancy for pc when
// Submit's subsequent persistence call fails (design v6 I17): in-memory
// desired/observed were never touched by accept() in the first place, so
// there is nothing else to roll back. A no-op if pc is no longer the slot's
// occupant (MAJOR 3, F4b Q3 consolidated corrective): a slow, failing Save
// can race pc's OWN generation dying spontaneously — independently
// classified and released by the worker while Save is still in flight — and
// by the time Save finally returns, a completely different, unrelated
// command may have since been accepted into the (by-then-free) slot. Blindly
// clearing s.pending/s.transition here would rip that DIFFERENT command's
// still-legitimate slot occupancy out from under it (corrupting
// capabilities, and — via publishTerminal's own "released := s.pending"
// read — its eventual terminal's CommandID) purely because pc's own,
// long-stale Submit call finally got around to failing. Mirrors the
// isCurrentPending pattern dispatch() already uses for the analogous
// cmdCh-dequeue race.
func (s *statusState) revertPendingOnPersistFailure(pc *pendingCommand) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != pc {
		return
	}
	s.pending = nil
	s.transition = TransitionNone
}

// commitDesiredAndReason publishes a newly-and-durably-persisted desired
// state (plus the reason that drove it) into memory for pc. Called by
// Submit right after a successful Persistence.Save, before the worker is
// signaled, so the worker (and any concurrent Snapshot/accept caller)
// always sees a desired value that already matches what was just committed
// to disk. A no-op if pc is no longer the slot's occupant (MAJOR 3): the
// symmetric case of revertPendingOnPersistFailure's own race — a slow but
// SUCCEEDING Save for pc, returning only after pc's own generation already
// died spontaneously and a DIFFERENT command has since committed its own,
// newer desired state, must never overwrite that newer value with pc's
// now-stale one.
func (s *statusState) commitDesiredAndReason(pc *pendingCommand, d DesiredState, r Reason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != pc {
		return
	}
	s.desired = d
	s.reason = r
}

// cancelRetryLocked drops any armed retry timer's bookkeeping. Must be
// called with mu held. Bumps retrySeq so a fire already buffered in the
// worker's private timer channel (Stop() cannot un-send it) is recognized
// as stale when the worker eventually reads it (see retrySeq's doc
// comment). Returns the timer's own stop closure (a no-op func if none was
// armed) WITHOUT invoking it (MINOR 9, F4b Q3 consolidated corrective):
// the injected Timer.Stop is an outward call — design v6 §6's leaf-lock
// rule ("под statusMu не делается НИ ОДИН вызов наружу") requires every
// caller to invoke the returned closure only AFTER releasing mu.
func (s *statusState) cancelRetryLocked() (stop func()) {
	cancel := s.retryCancel
	s.retryCancel = nil
	s.nextRetryAt = time.Time{}
	s.retrySeq++
	if cancel == nil {
		return func() {}
	}
	return cancel
}

// armRetry records a scheduled retry (worker-only caller) and returns the
// retry's identity (retrySeq) so the worker can recognize its OWN eventual
// fire as stale if retry bookkeeping moves on before that fire is
// processed (see retrySeq's doc comment).
func (s *statusState) armRetry(next time.Time, cancel func()) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retryCancel = cancel
	s.nextRetryAt = next
	s.retrySeq++
	return s.retrySeq
}

// clearRetry drops retry bookkeeping without invoking cancel (used once the
// timer has already fired, so there is nothing left to stop).
func (s *statusState) clearRetry() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retryCancel = nil
	s.nextRetryAt = time.Time{}
	s.retrySeq++
}

// currentRetrySeq returns the current retry identity — see retrySeq's doc
// comment.
func (s *statusState) currentRetrySeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.retrySeq
}

// isCurrentPending reports whether pc is STILL the slot's occupant
// (pointer identity). Used at cmdCh dequeue time: a command pulled from
// cmdCh may have already been resolved by a spontaneous classification
// that ran first (orchestrator concurrency review, defect 1) — dispatching
// or superseding it again would either deadlock (awaiting a generation
// completion already consumed) or corrupt whatever DIFFERENT command has
// since taken the slot.
func (s *statusState) isCurrentPending(pc *pendingCommand) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pending == pc
}

// beginTransition publishes a transitional observed state (starting/
// pausing/stopping/restarting) plus the Transition label describing it.
// Worker-only caller.
func (s *statusState) beginTransition(observed ObservedState, transition Transition, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observed = observed
	s.transition = transition
	s.transitionStartedAt = now
}

// beginStartTransition is startGeneration's atomic combination of "was the
// PREVIOUS observed state failed" (for failed-lineage/recovery tracking)
// with beginTransition's own publish of the new "starting" transition
// (BLOCKER 2, F4b Q3 consolidated corrective): startGeneration used to do
// these as two SEPARATE statusMu critical sections (a snapshot() read, then
// a later beginTransition() write) with no outward call in between — but
// still a real gap. A concurrent Submit's accept() evaluated inside that
// gap would see the STALE pre-transition observed/slot state (e.g.
// ObservedFailed with the slot free) and could legitimately compute an
// "accept"-class action, occupying the slot and queuing itself on cmdCh —
// racing a worker that is, at that very moment, ALREADY committed to
// launching this new generation. Merging the read and the write into one
// critical section (still not a full fix on its own — see dispatch()'s own
// defensive re-derivation for actionStart/actionPersistOnlySetObserved,
// which is what actually closes the hazard regardless of how narrow this
// makes the window) at least removes the now-pointless extra lock/unlock
// round trip that existed for no reason other than historical accident.
func (s *statusState) beginStartTransition(observed ObservedState, transition Transition, now time.Time) (wasFailed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	wasFailed = s.observed == ObservedFailed
	s.observed = observed
	s.transition = transition
	s.transitionStartedAt = now
	return wasFailed
}

// beginGeneration records a freshly-launched generation's token and publishes
// it to the StatusSink BEFORE the caller spawns the Runner's goroutine
// (design v6 §10 ordering requirement). Worker-only caller.
func (s *statusState) beginGeneration(gen uint64, now time.Time) {
	s.mu.Lock()
	s.generation = gen
	s.startedAt = now
	sink := s.sink
	s.mu.Unlock()
	sink.SetGeneration(gen)
}

// publishTerminal is the ONLY place an ObservedState becomes final for a
// command/self-classification that HOLDS the pending slot (design v6 §5.2
// step 4): it clears the slot exactly once (logging slot_released, design
// v6 §11), updates observed/reason/lastError, and optionally clears retry
// bookkeeping. Worker-only caller. Use publishTerminalNoSlot instead for a
// phantom (slot-free) start's own completion, which must never touch a
// slot it never held.
func (s *statusState) publishTerminal(observed ObservedState, reason Reason, lastError string, clearRetry bool) {
	s.mu.Lock()
	s.observed = observed
	s.transition = TransitionNone
	s.reason = reason
	s.lastError = lastError
	released := s.pending
	if released != nil {
		s.commandID = released.id
	}
	s.pending = nil
	var stopRetry func()
	if clearRetry {
		stopRetry = s.cancelRetryLocked()
	}
	sink := s.sink
	s.mu.Unlock()

	// MINOR 9 (F4b Q3 consolidated corrective): the retry timer's own stop
	// closure is invoked only now, after releasing statusMu (design v6
	// §6's leaf-lock rule) — exactly like the sink notification below,
	// which has always followed this same "unlock first" shape.
	if stopRetry != nil {
		stopRetry()
	}
	recordSlotReleased(released, string(observed))
	// statusMu is a leaf lock (package doc, design v6 §6: "под statusMu не
	// делается НИ ОДИН вызов наружу") — the sink is notified only after
	// releasing it, exactly like beginGeneration's SetGeneration call.
	sink.SetStatus(string(observed), lastError)
}

// publishTerminalNoSlot is publishTerminal's twin for a PHANTOM
// (reconcile/retry-driven) start reaching its own completion (ready, or a
// startup failure classified elsewhere): unlike publishTerminal, it never
// reads or clears s.pending. A phantom start never held the slot, so by the
// time it completes, the slot may legitimately be occupied by a completely
// unrelated, independently-accepted command (e.g. a cancel-start racing
// against Ready — design v6 §5.1 "starting" row, slot-free case) whose own
// terminal must be published later, by ITS OWN code path, untouched.
func (s *statusState) publishTerminalNoSlot(observed ObservedState, reason Reason, lastError string, clearRetry bool) {
	s.mu.Lock()
	s.observed = observed
	s.transition = TransitionNone
	s.reason = reason
	s.lastError = lastError
	var stopRetry func()
	if clearRetry {
		stopRetry = s.cancelRetryLocked()
	}
	sink := s.sink
	s.mu.Unlock()

	// MINOR 9: invoked only after releasing statusMu — see publishTerminal's
	// own doc comment.
	if stopRetry != nil {
		stopRetry()
	}
	sink.SetStatus(string(observed), lastError)
}

// releasePendingOnly clears the pending slot without touching observed or
// lastError — the degraded row's persist-only cells (design v6 §5.1: desired
// flips between paused and stopped while observed stays degraded, I30).
func (s *statusState) releasePendingOnly() {
	s.mu.Lock()
	released := s.pending
	if released != nil {
		s.commandID = released.id
	}
	s.pending = nil
	s.transition = TransitionNone
	s.mu.Unlock()

	recordSlotReleased(released, "degraded")
}

// enterDegraded publishes observed=degraded (design v6 §5.3(b), I30): the
// process and dashboard stay live, but no new generation is built until a
// process restart. Worker-only caller.
func (s *statusState) enterDegraded(lastError string) {
	s.mu.Lock()
	s.observed = ObservedDegraded
	s.transition = TransitionNone
	s.lastError = lastError
	released := s.pending
	if released != nil {
		// MINOR 15 (F4b Q3 consolidated corrective): parity with
		// publishTerminal — a degraded terminal reached via a
		// command-in-flight (its own pause/stop hitting a dirty teardown)
		// must report THAT command's id, not leave CommandID stuck on
		// whatever it was before.
		s.commandID = released.id
	}
	s.pending = nil
	stopRetry := s.cancelRetryLocked()
	sink := s.sink
	s.mu.Unlock()

	// MINOR 9: invoked only after releasing statusMu — see publishTerminal's
	// own doc comment.
	stopRetry()
	recordSlotReleased(released, "degraded")
	sink.SetStatus(string(ObservedDegraded), lastError)
}

// reconcile sets the controller's initial desired/observed/reason/lastError
// from startup reconciliation (design v6 §5.4). Called once, before the
// worker's main loop begins serving commands.
func (s *statusState) reconcile(desired DesiredState, observed ObservedState, reason Reason, lastError string, override bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.desired = desired
	s.observed = observed
	s.reason = reason
	s.lastError = lastError
	s.override = override
}
