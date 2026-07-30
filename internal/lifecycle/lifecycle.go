// Package lifecycle implements the app-level durable lifecycle controller
// core described in design v6 §5 (state machine), §6 (concurrency) and §8
// (persistence): a single actor ("the worker") that owns every transition
// between a durable, operator-chosen DesiredState (running/paused/stopped)
// and the process's current ObservedState, plus the one Runner "generation"
// that is alive at any instant.
//
// This package is intentionally narrow: it has NO knowledge of HTTP, the
// web dashboard, or the concrete Miner type. It is driven purely through the
// Runner/Factory seam (Config.Factory builds one Runner per generation) and
// observed purely through Snapshot()/the optional StatusSink port. Wiring a
// concrete Miner factory, a web-facing StatusSink adapter and the process
// exit/signal plumbing is b3's job (design v6 §14); this package only needs
// to exist and behave correctly in isolation, which is what its test suite
// exercises with fake Runners, a fake Persistence port and fake timers.
//
// # Concurrency model
//
// Exactly one goroutine — the worker, running inside Controller.Run — ever
// starts, tears down, or classifies the completion of a generation. Every
// other goroutine (HTTP handlers calling Submit/Pause/Resume/Restart/Stop,
// the updater calling UpdateApplied, the process signal handler cancelling
// Run's ctx) only ever: (a) reads/writes the small statusMu-guarded snapshot
// fields, and (b) hands the worker a message through one of three channels
// (cmdCh, updateCh, or ctx.Done()). statusMu is always a LEAF lock — nothing
// under it ever calls out to a Runner, the Persistence port, or blocks on a
// channel with no ready sender — so Snapshot() stays fast and non-blocking
// even while the worker is deep inside a slow teardown (design v6 I9/I10,
// tests: TestStatusReadableDuringSlowTeardown, TestNoLockHeldDuringTeardown).
//
// # The pending-command slot
//
// At most one command is ever "in flight" (accepted but not yet resolved to
// a terminal ObservedState): Submit occupies a capacity-1 slot atomically
// under statusMu, hands the accepted command to the worker over a
// capacity-1 channel, and the slot is released — exactly once — only when
// the worker publishes that command's terminal observed state. This is what
// makes a concurrent second Submit deterministically 409/idempotent instead
// of racing the first (design v6 §5.2, I7/I8).
//
// # Generations
//
// Each time the controller needs to run the miner it asks Config.Factory
// for a fresh Runner and hands it a fresh, cancelable context derived from
// the ctx passed to Controller.Run — so an OS signal cancelling that parent
// ctx cancels whichever generation is current WITHOUT the worker's
// involvement (design v6 §5.3 "д"). Every generation gets a monotonically
// increasing token; the worker is the only reader/writer of "current
// generation" state, so a completion tagged with a stale token (an old,
// already-superseded generation finally returning) is recognized and
// discarded rather than corrupting the current snapshot (I6).
package lifecycle

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// DesiredState is the durable, operator-chosen intent: what the miner
// SHOULD be doing. It is the only fact persisted across restarts (design v6
// §5.4: "running" is never persisted as an observed FACT, only as intent).
type DesiredState string

const (
	DesiredRunning DesiredState = "running"
	DesiredPaused  DesiredState = "paused"
	DesiredStopped DesiredState = "stopped"
)

func (d DesiredState) String() string { return string(d) }

// ObservedState is what the controller currently sees happening: either a
// transitional state the worker is actively driving (starting, pausing,
// stopping, restarting) or a steady state where the pending-command slot is
// empty and new commands are evaluated normally against the table in
// tableLookup (running, paused, stopped, failed, degraded, exiting).
type ObservedState string

const (
	ObservedStarting   ObservedState = "starting"
	ObservedRunning    ObservedState = "running"
	ObservedPausing    ObservedState = "pausing"
	ObservedStopping   ObservedState = "stopping"
	ObservedRestarting ObservedState = "restarting"
	ObservedPaused     ObservedState = "paused"
	ObservedStopped    ObservedState = "stopped"
	ObservedFailed     ObservedState = "failed"
	ObservedDegraded   ObservedState = "degraded"
	ObservedExiting    ObservedState = "exiting"
)

func (o ObservedState) String() string { return string(o) }

// Transition names what the worker is currently doing to the generation, or
// "none" when observed is a steady state with an empty slot. "pending" is
// the narrow window between Submit's atomic accept and the worker actually
// dequeuing the command (design v6 §5.2 step 1): observed has NOT changed
// yet in that window, only Transition has.
type Transition string

const (
	TransitionNone            Transition = "none"
	TransitionPending         Transition = "pending"
	TransitionStart           Transition = "start"
	TransitionPause           Transition = "pause"
	TransitionStop            Transition = "stop"
	TransitionRestart         Transition = "restart"
	TransitionUpdateExit      Transition = "update-exit"
	TransitionProcessShutdown Transition = "process-shutdown"
)

func (t Transition) String() string { return string(t) }

// Reason classifies WHY a transition happened, independent of what it
// transitioned to. It is reported in Snapshot and used by tests/observers to
// distinguish, e.g., an operator-cancelled start (reason=user, not an
// error) from a self-inflicted startup failure (reason=startup-failure,
// eligible for retry).
type Reason string

const (
	ReasonNone           Reason = ""
	ReasonUser           Reason = "user"
	ReasonSignal         Reason = "signal"
	ReasonUpdater        Reason = "updater"
	ReasonStartupFailure Reason = "startup-failure"
	ReasonReconcile      Reason = "reconcile"
	ReasonRetry          Reason = "retry"
)

func (r Reason) String() string { return string(r) }

// Command is one of the four operator-facing verbs Submit accepts.
type Command string

const (
	CommandPause   Command = "pause"
	CommandResume  Command = "resume"
	CommandRestart Command = "restart"
	CommandStop    Command = "stop"
)

// Outcome is Submit's immediate, synchronous disposition of a command —
// the HTTP-layer's 202/200/409 decision (design v6 §5.1/§9) — as opposed to
// the eventual terminal ObservedState the worker publishes later.
type Outcome string

const (
	// OutcomeAccepted: the command was admitted; its terminal ObservedState
	// will appear in a later Snapshot (202-semantics).
	OutcomeAccepted Outcome = "accepted"
	// OutcomeRejected: refused — either the transition table forbids this
	// command in the current observed state, the pending-command slot was
	// already occupied (409-semantics), or persistence failed (500-semantics
	// at the HTTP layer, but structurally the same "nothing changed" result).
	OutcomeRejected Outcome = "rejected"
	// OutcomeIdempotent: a no-op — the system is already in (or already
	// heading to) the requested state, so nothing changed (200-semantics).
	OutcomeIdempotent Outcome = "idempotent"
)

// SubmitResult is Submit's synchronous return value.
type SubmitResult struct {
	Outcome   Outcome
	CommandID string
	// Err is set for OutcomeRejected: a human-readable rejection reason
	// (e.g. "process restart required") or a wrapped persistence error.
	Err error
}

// Capabilities mirrors the transition table's "A"/"A-idempotent" cells for
// the CURRENT observed state: which of the four commands would currently be
// accepted or are a no-op. It is derived, never stored independently of
// observed/slot occupancy.
type Capabilities struct {
	CanPause   bool
	CanResume  bool
	CanRestart bool
	CanStop    bool
}

// Snapshot is the full, consistent, point-in-time view Snapshot() returns.
// Every field is read under statusMu in one critical section, so a caller
// never observes a torn combination (e.g. Observed=paused with a
// Generation still pointing at the old, torn-down generation).
type Snapshot struct {
	Desired    DesiredState
	Observed   ObservedState
	Transition Transition
	// Generation is the monotonically increasing token of the generation
	// currently (or most recently) owned by the controller. 0 before the
	// first generation has ever started.
	Generation uint64
	CommandID  string
	Reason     Reason
	// LastError is a sanitized, human-readable string (design v6 I24: no
	// secrets) describing the most recent unexpected failure. It is cleared
	// on the next transition into a state that supersedes it.
	LastError string

	// StartedAt is when the CURRENT generation was launched (zero if no
	// generation has ever run). TransitionStartedAt is when the CURRENT
	// transition (Transition != none) began (zero if idle). NextRetryAt is
	// when the retry timer will next fire (zero if no retry is scheduled).
	StartedAt           time.Time
	TransitionStartedAt time.Time
	NextRetryAt         time.Time

	Capabilities Capabilities

	// Override reports whether ForceRunning was honored at the last startup
	// reconciliation (design v6 §5.4): true means the in-memory desired was
	// forced to running without rewriting the durable row.
	Override bool
}

// Runner is one generation's runtime engine. A Runner is used for EXACTLY
// one Run call — Config.Factory must return a fresh Runner every time it is
// invoked (design v6 item 10).
type Runner interface {
	Run(ctx context.Context) error
}

// ReadySignaler is an OPTIONAL interface a Runner may additionally
// implement to report when it has finished its own internal startup phase
// (device-code auth, initial GraphQL calls, ...) and is durably up. This is
// this package's OWN seam for design v6 F4's "starting может длиться
// неограниченно" — it does not require touching internal/miner: b3's real
// Miner adapter simply does not implement it (yet), and the worker treats
// a Runner without it as ready immediately upon launch, which is exactly
// today's (pre-readiness) behavior, preserved.
//
// When a Runner DOES implement ReadySignaler, observed stays "starting"
// (or "restarting", for a restart's relaunch) from the moment its goroutine
// is spawned until Ready's channel closes, and the design v6 §5.1
// "starting" row becomes genuinely reachable: cancelling the start via
// Pause/Stop (when the slot is free — a reconcile/retry-driven start) is a
// real, worker-mediated cancellation of the in-flight generation, not just
// a narrow, synchronous race window.
type ReadySignaler interface {
	// Ready returns a channel that is closed once the generation has
	// finished starting. Called at most once per generation, right after
	// Config.Factory builds it (before its Run goroutine is spawned).
	Ready() <-chan struct{}
}

// Factory builds a fresh Runner for a new generation. Called by the worker
// goroutine only, never concurrently.
type Factory func() Runner

// Persistence is the port over durable storage for DesiredState. Store (in
// store.go) implements it over *database.DB in production; tests fake it to
// exercise persistence-failure and ordering behavior without a real DB.
type Persistence interface {
	// Load reads the persisted desired state at startup. See LoadResult's
	// doc comment for how the three outcome classes (found, missing,
	// corrupt) are distinguished from a plain read error.
	Load(ctx context.Context) (LoadResult, error)
	// Save durably records a new desired state plus the reason/commandID
	// that caused it. Implementations MUST apply their own budget (Store
	// uses the package-level persistTimeout) — Save is expected to return
	// (possibly with an error) within that budget even under contention.
	Save(ctx context.Context, desired DesiredState, reason, commandID string) error
}

// LoadResult is Persistence.Load's success value. Found=false (with a nil
// error) means "no row yet" — back-compat default of DesiredRunning is the
// CALLER's job (reconciliation), not Load's. A corrupt/unrecognized stored
// value is reported as an error (a *CorruptStateError, see errors.go), not
// as a LoadResult field, so callers can't accidentally ignore it.
type LoadResult struct {
	Desired DesiredState
	Found   bool
}

// Clock/Timer seams (design v6 item 12: "fakeable time seam"). Production
// uses realClock; tests substitute a fake that fires on demand instead of
// racing the wall clock, so retry-schedule tests are deterministic and fast.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer abstracts a one-shot timer. C returns the channel that receives the
// fire time; Stop cancels a pending fire (same contract as *time.Timer.Stop).
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) NewTimer(d time.Duration) Timer {
	return &realTimer{t: time.NewTimer(d)}
}

type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time { return r.t.C }
func (r *realTimer) Stop() bool          { return r.t.Stop() }

// Config wires a Controller's external dependencies. Only Factory is
// required; every other field has a safe zero-value default (New fills
// them in).
type Config struct {
	// Factory builds a fresh Runner for each generation. Required.
	Factory Factory

	// Persistence is the durable store for DesiredState. Defaults to an
	// in-memory-only stub (desired starts at DesiredRunning, saves succeed
	// but are not observable across a restart) so a Controller built with a
	// zero Config is still usable in tests that don't care about durability.
	Persistence Persistence

	// StatusSink receives outbound status/generation notifications.
	// Defaults to a no-op sink.
	StatusSink StatusSink

	// ForceRunning is the LIFECYCLE_FORCE_RUNNING escape hatch (design v6
	// §5.4): honored ONLY at startup reconciliation, forces in-memory
	// desired to running without rewriting the durable row.
	ForceRunning bool

	// Clock is the time/timer seam. Defaults to the real wall clock.
	Clock Clock

	// NoControlSurface reports whether this process has NO way at all for an
	// operator to change desired at runtime (design v6 §14/OD3: no web
	// dashboard was built, e.g. EnableAnalytics=false). When true AND
	// startup reconciliation honors a persisted paused/stopped intent, the
	// controller records a one-per-boot ring event plus a rare periodic slog
	// reminder (noControlSurfaceTickInterval) explaining why the miner never
	// started and how to regain control — since with no control surface
	// there would otherwise be NOTHING in the logs to explain the silence.
	NoControlSurface bool

	// UpdaterRun, if set, is the process-level updater loop (design v6 §7:
	// updater ownership moves OUT of any per-generation runtime and onto the
	// process/controller). Controller.Run starts it exactly once, as its own
	// goroutine, on a context DERIVED from the ctx passed to Run — see Run's
	// doc comment for why a derived (not shared) context is required. nil (the
	// default) means this controller does not own an updater loop at all
	// (every existing test, and any caller that manages the updater
	// independently).
	UpdaterRun func(ctx context.Context)
}

// Controller is the durable lifecycle core. Construct with New, drive with
// Run(ctx) (blocks until process-shutdown or update-exit), observe with
// Snapshot(), and mutate with Submit/Pause/Resume/Restart/Stop/UpdateApplied.
type Controller struct {
	cfg Config

	state statusState

	// cmdCh carries an accepted command from Submit's caller goroutine to
	// the worker. Buffered cap=1 with a non-blocking send: the pending-slot
	// invariant guarantees at most one command is ever in flight, so the
	// send can never block on a full channel.
	cmdCh chan *pendingCommand

	// updateCh is closed exactly once by UpdateApplied (updateOnce), giving
	// every worker-loop select an instantly-ready case without needing a
	// counted/buffered channel.
	updateCh   chan struct{}
	updateOnce sync.Once

	// doneCh carries every generation's completion, tagged with its
	// generation token so the worker can recognize (and discard) a stale
	// completion from an already-superseded generation.
	doneCh chan generationResult

	// The fields below are touched ONLY by the worker goroutine (inside
	// Run) — never concurrently, so they need no lock of their own. They
	// are the worker's private working copy of "what generation am I
	// driving right now", as opposed to statusState's published copy
	// (statusMu-guarded, for Snapshot()).
	runCtx context.Context
	// currentGen/currentGenCancel identify and control the generation the
	// worker is currently driving (starting, steady, or being torn down).
	currentGen       uint64
	currentGenCancel context.CancelFunc
	// generationLive is true from the moment the generation's goroutine is
	// spawned until its doneCh completion has actually been consumed
	// (whether via an explicit teardown wait or the spontaneous-completion
	// path) — it tells exit handling whether there is anything left to
	// wait for.
	generationLive bool
	// retryAttempt is the number of CONSECUTIVE startup failures since the
	// last confirmed recovery (design v6 §5.3, item 12); it drives
	// retryBackoff and is reset to 0 only by maybeDeclareRecovery.
	retryAttempt int
	// recoveryPending is true while the CURRENT generation emerged from a
	// failed/retry lineage and has not yet proven itself (see
	// maybeDeclareRecovery's doc comment).
	recoveryPending bool
	// retryTimer is the single, worker-owned retry timer (design v6 item
	// 12: "ONE owner, no goroutine-per-retry"). nil when no retry is armed.
	retryTimer Timer
	// retryTimerSeq is the retry identity (statusState.retrySeq) captured
	// when retryTimer was armed — compared against currentRetrySeq() when
	// the timer's case actually fires, so a fire already buffered in
	// retryTimer's channel before it was cancelled (time.Timer.Stop does
	// not drain an already-fired channel) is recognized as stale rather
	// than launching a second, spurious generation (orchestrator
	// concurrency review, defect 1's retry-timer race).
	retryTimerSeq uint64

	// noControlSurfaceTimer is the single, worker-owned periodic reminder
	// timer for design v6 §14/OD3's no-control-surface tick (contract §11
	// item 8): armed once at startup reconciliation when a persisted
	// paused/stopped intent is honored with cfg.NoControlSurface set, then
	// RE-armed every time it fires — unlike retryTimer, which fires at most
	// once per generation attempt, this one recurs for the entire life of
	// the process, since with no control surface desired can never change.
	// nil whenever no reminder is armed (the common case: either desired
	// ended up running, or NoControlSurface is false).
	noControlSurfaceTimer Timer

	// awaitingStart is non-nil while the CURRENT generation is a
	// ReadySignaler that has not yet become ready — i.e. observed is
	// "starting" or "restarting" and readyCh is the channel the main loop
	// is waiting on (see readyChSafe/handleReady). nil whenever readiness
	// is not in question: either no generation is starting right now, or
	// the current Runner doesn't implement ReadySignaler (instant-ready).
	awaitingStart *startInfo
	// readyCh mirrors awaitingStart.pc's generation's Ready() channel;
	// kept as a separate field (rather than read through awaitingStart
	// every time) so readyChSafe can return nil trivially when not
	// awaiting anything.
	readyCh <-chan struct{}

	// testBeforeSelect is a tests-only seam (nil in production, following
	// the stopObserver/applyCommitBarrier precedent in internal/miner):
	// invoked at the very top of each Run loop iteration, before the
	// priority pre-check. It lets a test pause the worker deterministically
	// right before a specific select, set up multiple channels' readiness
	// (e.g. an accepted command AND update-applied) with no race between
	// them becoming ready, and only then release the worker — exercising
	// exactly which of several equally-valid interleavings the select
	// resolves to, without depending on OS scheduling luck. Must be set
	// before Run is started (the worker goroutine only ever reads it).
	testBeforeSelect func()
}

// generationResult is one generation's outcome, tagged with its token so
// the worker can classify or discard it.
type generationResult struct {
	gen uint64
	err error
}

// startInfo identifies the generation the worker is currently awaiting
// readiness for, and the command (if any) that launched it. pc is nil for
// a phantom (reconcile/retry-driven) start — one with an empty slot, per
// design v6 §5.2's "Retry scheduled only for reason=startup-failure with
// empty slot" and the analogous boot-reconcile case.
type startInfo struct {
	gen    uint64
	pc     *pendingCommand
	reason Reason
}

// pendingCommand is the sole occupant of the capacity-1 command slot,
// spanning from Submit's atomic accept all the way to the worker's terminal
// publish (design v6 item 5: "stays occupied after dequeue until terminal").
type pendingCommand struct {
	id     string
	cmd    Command
	reason Reason
	// target is the DesiredState this command is driving toward: running
	// for resume/restart, paused for pause, stopped for stop.
	target DesiredState
	// action tells the worker what kind of generation work (if any) this
	// command requires; computed once at accept time from the transition
	// table cell that admitted it.
	action workerAction
}

// New constructs a ready-to-Run Controller. cfg.Factory must be non-nil;
// New panics otherwise (a Controller with no way to ever build a Runner is a
// programming error, not a runtime condition to recover from).
func New(cfg Config) *Controller {
	if cfg.Factory == nil {
		panic("lifecycle: Config.Factory is required")
	}
	if cfg.Persistence == nil {
		cfg.Persistence = newMemoryPersistence()
	}
	if cfg.StatusSink == nil {
		cfg.StatusSink = nopSink{}
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}

	c := &Controller{
		cfg:      cfg,
		cmdCh:    make(chan *pendingCommand, 1),
		updateCh: make(chan struct{}),
		doneCh:   make(chan generationResult, 1),
	}
	c.state.init(cfg.StatusSink)
	return c
}

// Snapshot returns a consistent point-in-time view. Never blocks on the
// worker: statusMu is a leaf lock (see package doc), so this returns
// immediately even while the worker is mid-teardown.
func (c *Controller) Snapshot() Snapshot {
	return c.state.snapshot()
}

// Pause/Resume/Restart/Stop are thin wrappers over Submit; they exist so
// callers get a typed, discoverable API instead of stringly-typed commands.
func (c *Controller) Pause(ctx context.Context) SubmitResult   { return c.Submit(ctx, CommandPause) }
func (c *Controller) Resume(ctx context.Context) SubmitResult  { return c.Submit(ctx, CommandResume) }
func (c *Controller) Restart(ctx context.Context) SubmitResult { return c.Submit(ctx, CommandRestart) }
func (c *Controller) Stop(ctx context.Context) SubmitResult    { return c.Submit(ctx, CommandStop) }

// Submit implements the full command protocol (design v6 §5.2): atomic
// accept under statusMu, persistence on the caller's own goroutine (outside
// statusMu, before signaling the worker), and — on persist failure — a
// clean rollback that leaves in-memory desired/observed untouched (I17).
func (c *Controller) Submit(ctx context.Context, cmd Command) SubmitResult {
	commandID := newCommandID()

	decision := c.state.accept(cmd, commandID)
	switch decision.kind {
	case acceptRejected:
		recordCommandRejected(cmd, decision.err)
		return SubmitResult{Outcome: OutcomeRejected, Err: decision.err}

	case acceptIdempotent:
		recordCommandIdempotent(cmd)
		return SubmitResult{Outcome: OutcomeIdempotent, CommandID: decision.existingID}
	}

	// acceptOccupied: persist first (outside statusMu), then commit the new
	// desired state into memory and signal the worker. Persist-only
	// commands (paused<->stopped without a live generation) still go
	// through the worker so it stays the sole publisher of terminal
	// observed states.
	pc := decision.pending
	if err := c.cfg.Persistence.Save(ctx, pc.target, string(pc.reason), commandID); err != nil {
		c.state.revertPendingOnPersistFailure()
		recordCommandRejectedPersist(cmd, err)
		return SubmitResult{Outcome: OutcomeRejected, Err: fmt.Errorf("lifecycle: persist desired state: %w", err)}
	}
	c.state.commitDesiredAndReason(pc.target, pc.reason)

	select {
	case c.cmdCh <- pc:
	default:
		// Unreachable under the slot invariant (at most one command is ever
		// in flight); guarded so a bug here degrades to a dropped signal
		// rather than a deadlocked caller goroutine.
	}

	recordCommandAccepted(cmd, commandID)
	return SubmitResult{Outcome: OutcomeAccepted, CommandID: commandID}
}

// UpdateApplied is the idempotent priority signal an updater adapter (b2)
// calls once a binary swap has completed. Safe to call multiple times or
// concurrently; only the first call has any effect (design v6 §7:
// "OnUpdate идемпотентен").
func (c *Controller) UpdateApplied() {
	c.updateOnce.Do(func() { close(c.updateCh) })
}

// UpdaterGate implements design v6 §7's updater-interlock matrix: the
// effective permission to actually APPLY an available update is desired !=
// stopped (both running and paused allow an apply; stopped means
// check-and-notify only, never replacing the binary while the operator has
// asked for the process to be stopped). Intended to be wired as an
// updater.Options.Gate. Reads the current desired state under statusMu via
// the existing currentDesired accessor — no new locks — so it is safe to
// call from any goroutine, in particular the updater loop's own goroutine,
// concurrently with the worker.
func (c *Controller) UpdaterGate() bool {
	return c.state.currentDesired() != DesiredStopped
}
