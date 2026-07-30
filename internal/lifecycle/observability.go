package lifecycle

import (
	"log/slog"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
)

// This file implements design v6 §11's two-level observability policy:
// high-frequency, per-attempt facts go to slog ONLY (never the ring — a
// crash-looping retry chain must not evict mining events from the shared,
// fixed-capacity ring the dashboard/support-bundle/daily-summary read), and
// only actual STATE CHANGES (one entry per command/self-classification
// reaching a terminal, or per boot for one-shot facts) go to events.Record.
// Every ring call passes an empty Streamer (design v6 §11: lifecycle events
// are process-level, not per-streamer) and goes through the SAME
// process-wide ring the rest of the miner uses — recordRing does not add
// its own budget; the bound on lifecycle's ring footprint comes from how
// rarely these call sites fire (one per terminal state change), not from
// any filtering here.

// recordRing appends a lifecycle_* event to the process-wide ring.
func recordRing(t events.Type, detail string) {
	events.Record(t, "", detail)
}

func recordCommandAccepted(cmd Command, commandID string) {
	slog.Info("lifecycle: command_accepted", "command", cmd, "command_id", commandID)
}

func recordCommandIdempotent(cmd Command) {
	slog.Info("lifecycle: command_accepted", "command", cmd, "outcome", "idempotent")
}

func recordCommandRejected(cmd Command, err error) {
	slog.Info("lifecycle: command_rejected", "command", cmd, "reason", err)
}

// recordCommandRejectedPersist logs a persistence failure at BOTH levels
// (design v6 §11: "command_rejected_persist" and ring event
// lifecycle_persistence_failed are the SAME fact on two levels, not two
// separate occurrences).
func recordCommandRejectedPersist(cmd Command, err error) {
	slog.Warn("lifecycle: command_rejected_persist", "command", cmd, "error", err)
	recordRing(events.TypeLifecyclePersistenceFailed, err.Error())
}

func recordCommandAcceptedDegradedSwitch(pc *pendingCommand) {
	slog.Info("lifecycle: command_accepted", "command", pc.cmd, "outcome", "degraded_persist_only", "target", pc.target)
}

func recordTransitionStarted(cmd Command, reason Reason) {
	slog.Info("lifecycle: transition_started", "command", cmd, "reason", reason)
}

func recordGenerationStartAttempted(gen uint64, reason Reason) {
	slog.Info("lifecycle: generation_start_attempted", "generation", gen, "reason", reason)
}

// recordGenerationRunning logs the (usually near-instant) completion of a
// fresh-start transition and, when it was driven by an explicit resume/
// restart command, records the matching per-command ring event.
func recordGenerationRunning(gen uint64, cmd Command, cmdID string) {
	slog.Info("lifecycle: transition_completed", "generation", gen, "observed", ObservedRunning, "command_id", cmdID)
	switch cmd {
	case CommandResume:
		recordRing(events.TypeLifecycleResumed, "")
	case CommandRestart:
		recordRing(events.TypeLifecycleRestarted, "")
	}
}

// recordTerminalForTarget logs a pause/stop's (or a spontaneous wind-down's)
// completion and records the matching per-command ring event. cmdID is ""
// for a spontaneous (non-command-driven) wind-down.
func recordTerminalForTarget(target DesiredState, reason Reason, cmdID string) {
	slog.Info("lifecycle: transition_completed", "target", target, "reason", reason, "command_id", cmdID)
	switch target {
	case DesiredPaused:
		recordRing(events.TypeLifecyclePaused, "")
	case DesiredStopped:
		recordRing(events.TypeLifecycleStopped, "")
	}
}

func recordStaleCompletionIgnored(gen uint64) {
	slog.Info("lifecycle: stale_completion_ignored", "generation", gen)
}

func recordRetryScheduled(attempt int, next time.Time) {
	slog.Info("lifecycle: generation_retry_scheduled", "attempt", attempt, "next_retry_at", next)
}

// recordDiscardedStaleCommand logs a command message pulled from cmdCh that
// no longer occupies the slot — it was already resolved by a spontaneous
// classification (or a priority event) that ran first (orchestrator
// concurrency review, defect 1). slog-only: this is a per-race diagnostic,
// not a state change.
func recordDiscardedStaleCommand(pc *pendingCommand) {
	slog.Info("lifecycle: discarding already-resolved command message", "command", pc.cmd, "command_id", pc.id)
}

// recordRetryFireDiscardedStale logs a retry-timer fire whose seq no longer
// matches current retry bookkeeping — time.Timer.Stop cannot un-send a
// value already buffered in the timer's own channel, so a command accepted
// out of "failed" can race the literal fire; discarding it here is what
// prevents a second, spurious generation (orchestrator concurrency review,
// defect 1's retry-timer race).
func recordRetryFireDiscardedStale() {
	slog.Info("lifecycle: discarding stale retry-timer fire")
}

// recordTeardownStarted/Completed/Slow implement design v6 §11's
// teardown_started/teardown_completed/teardown_slow slog lines (§5.3в:
// "событие teardown_slow(threshold, package var)"). label is the
// originating Command's string form, or "shutdown"/"update-exit" for the
// two priority-event teardowns — purely descriptive, slog-only, never the
// ring (a slow or repeatedly-retried teardown must not threaten the ring
// budget invariant).
func recordTeardownStarted(label string) {
	slog.Info("lifecycle: teardown_started", "op", label)
}

func recordTeardownCompleted(label string, duration time.Duration) {
	slog.Info("lifecycle: teardown_completed", "op", label, "duration", duration)
}

func recordTeardownSlow(label string, elapsed time.Duration) {
	slog.Warn("lifecycle: teardown_slow", "op", label, "elapsed", elapsed, "threshold", teardownSlowThreshold)
}

// recordStartCancelled logs design v6 §5.1's "starting" row cancel-start
// case: a pause/stop command cancelled an in-flight, not-yet-ready,
// slot-free (reconcile/retry-driven) generation.
func recordStartCancelled(gen uint64, cmd Command) {
	slog.Info("lifecycle: start_cancelled", "generation", gen, "command", cmd)
}

// recordSlotReleased logs the pending-command slot's release (design v6
// §11: "slot_released(reason)") — a no-op if nothing was actually occupying
// it (the phantom-start / persist-only-degraded paths that never held it).
func recordSlotReleased(released *pendingCommand, reason string) {
	if released == nil {
		return
	}
	slog.Info("lifecycle: slot_released", "command", released.cmd, "command_id", released.id, "reason", reason)
}
