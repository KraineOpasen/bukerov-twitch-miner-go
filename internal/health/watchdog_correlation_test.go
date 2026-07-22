package health

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// --- Group E: watchdog async recovery correlation ---
//
// The stream_info (stage 3) and session_recreate (stage 5) stages stage a
// correlated session refresh into the broker and PARK on the outcome. These tests
// pin that the pipeline advances only on the exact matching outcome, and behaves
// correctly for success / failure / stale / skipped / timeout / duplicate.

// driveToStreamInfoStage drives a confirmed stall through the two synchronous
// stages to the point where the async stream_info stage has been issued (a
// refresh staged, pipeline parked). The caller sets h.watch.outcomeMode first to
// choose how that refresh resolves.
func (h *watchdogHarness) driveToStreamInfoStage(t *testing.T) {
	t.Helper()
	h.driveToStall(t)               // stage 1: progress_sync
	h.tick(10*time.Minute, true, 3) // stage 2: full_resync
	h.tick(10*time.Minute, true, 3) // stage 3: stream_info (async) issued
	st := h.state(t)
	if st.RecoveryStage != 3 || st.RecoveryStageName != "stream_info" {
		t.Fatalf("expected the async stream_info stage to be issued, got %+v", st)
	}
	if calls := h.watch.refreshCalls(); len(calls) != 1 || calls[0].mode != watcher.RefreshStreamInfo {
		t.Fatalf("expected exactly one staged stream-info refresh, got %+v", calls)
	}
}

// E1 + E10: a queued request neither advances the stage nor re-queues on
// duplicate evaluations while it is still pending.
func TestWatchdogAsyncStageDoesNotAdvanceOnQueue(t *testing.T) {
	h := newWatchdogHarness(t)
	h.watch.setOutcomeMode("none") // broker never publishes an outcome
	h.driveToStreamInfoStage(t)

	// Several more evaluations while pending (each well within the deadline): the
	// stage must not advance and no duplicate request may be staged.
	for i := 0; i < 3; i++ {
		h.tick(time.Minute, true, 3)
		if st := h.state(t); st.RecoveryStage != 3 {
			t.Fatalf("a queued async stage must not advance, got %+v", st)
		}
	}
	if calls := h.watch.refreshCalls(); len(calls) != 1 {
		t.Fatalf("duplicate evaluation must not re-stage the request, got %d calls", len(calls))
	}
}

// E2: the exact matching success outcome completes the pending stage, and the
// pipeline then advances to the next stage.
func TestWatchdogAsyncStageCompletesOnMatchingSuccess(t *testing.T) {
	h := newWatchdogHarness(t) // default outcomeMode "success"
	h.driveToStreamInfoStage(t)

	h.tick(10*time.Minute, true, 3) // resolve the matching success — no new stage yet
	if got := h.prober.callCount(); got != 0 {
		t.Fatalf("resolving the outcome must not itself run the next stage, got probe=%d", got)
	}
	h.tick(10*time.Minute, true, 3) // now the next stage (transport probe) runs
	if got := h.prober.callCount(); got != 1 {
		t.Fatalf("after a matching success the pipeline must advance, got probe=%d", got)
	}
}

// E3/E4/E5: an outcome with the wrong RequestID, an old signature, or from an old
// broadcast is ignored — it never completes the pending stage.
func TestWatchdogAsyncStageIgnoresNonMatchingOutcomes(t *testing.T) {
	cases := map[string]func(req watcher.SessionRefreshRequest) watcher.SessionRefreshOutcome{
		"wrong request id": func(req watcher.SessionRefreshRequest) watcher.SessionRefreshOutcome {
			return watcher.SessionRefreshOutcome{RequestID: "totally-different", Signature: req.Signature, Success: true}
		},
		"old signature": func(req watcher.SessionRefreshRequest) watcher.SessionRefreshOutcome {
			return watcher.SessionRefreshOutcome{RequestID: req.RequestID, Signature: "old-signature", Success: true}
		},
		"old broadcast": func(req watcher.SessionRefreshRequest) watcher.SessionRefreshOutcome {
			return watcher.SessionRefreshOutcome{RequestID: "old-broadcast-req", Signature: "old-broadcast-sig", Success: true}
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			h := newWatchdogHarness(t)
			h.watch.setOutcomeMode("none")
			h.driveToStreamInfoStage(t)

			req := h.watch.lastRequest("chan")
			h.watch.injectOutcome("chan", build(req))

			// Several passes, all inside the bounded deadline: a non-matching outcome
			// must never complete the pending stage, so the pipeline must not advance
			// on this pass OR any later one (a match-by-login regression would clear
			// the pending here and let the probe run on the next pass).
			for i := 0; i < 3; i++ {
				h.tick(time.Minute, true, 3)
				if st := h.state(t); st.RecoveryStage != 3 {
					t.Fatalf("a non-matching outcome must not complete the stage, got %+v", st)
				}
				if got := h.prober.callCount(); got != 0 {
					t.Fatalf("a non-matching outcome must not advance the pipeline, got probe=%d", got)
				}
			}
		})
	}
}

// E6: a skipped (lost-slot) outcome does NOT count as a completed transport
// recovery — the stage rolls back so it re-runs once farming is re-confirmed.
func TestWatchdogAsyncStageSkippedRollsBack(t *testing.T) {
	h := newWatchdogHarness(t)
	h.watch.setOutcomeMode("skip")
	h.driveToStreamInfoStage(t) // RecoveryStage==3, pending

	h.tick(10*time.Minute, true, 3) // resolve skip
	st := h.state(t)
	if st.RecoveryStage != 2 {
		t.Fatalf("a skipped async stage must roll the stage back to re-run, got %+v", st)
	}
	if got := h.prober.callCount(); got != 0 {
		t.Fatalf("a skipped stage must not advance to the probe, got probe=%d", got)
	}
}

// E7: a stale (new-broadcast/session) outcome rebaselines the episode — the
// pipeline restarts from the beginning for the fresh session.
func TestWatchdogAsyncStageStaleRebaselines(t *testing.T) {
	h := newWatchdogHarness(t)
	h.watch.setOutcomeMode("stale")
	h.driveToStreamInfoStage(t)

	h.tick(10*time.Minute, true, 3) // resolve stale
	if st := h.state(t); st.RecoveryStage != 0 {
		t.Fatalf("a stale outcome must rebaseline (restart the pipeline), got %+v", st)
	}
}

// E8: a matching FAILED outcome advances to the next stage, but only after the
// recovery cooldown.
func TestWatchdogAsyncStageFailedAdvancesAfterCooldown(t *testing.T) {
	h := newWatchdogHarness(t) // cooldown 0: reach the async stage first
	h.watch.setOutcomeMode("fail")
	h.driveToStreamInfoStage(t)

	// Now impose a cooldown that gates the resolution + next stage.
	h.w.UpdateSettings(WatchdogConfig{
		Enabled: true, StallDelay: 20 * time.Minute, StallConfirmations: 3,
		RecoveryCooldown: 30 * time.Minute, AvoidTTL: time.Hour, Rearm: 6 * time.Hour,
	})

	h.tick(5*time.Minute, true, 3)  // resolve fail (LastRecoveryAt set at resolution)
	h.tick(10*time.Minute, true, 3) // still inside the 30m cooldown — no advance
	if got := h.prober.callCount(); got != 0 {
		t.Fatalf("a failed stage must respect the cooldown before the next stage, got probe=%d", got)
	}
	h.tick(30*time.Minute, true, 3) // cooldown elapsed — next stage runs
	if got := h.prober.callCount(); got != 1 {
		t.Fatalf("after cooldown a failed stage must advance, got probe=%d", got)
	}
}

// E9: a never-arriving outcome times out boundedly (it does not wait forever) and
// the pipeline then continues.
func TestWatchdogAsyncStageTimesOutBoundedly(t *testing.T) {
	h := newWatchdogHarness(t)
	h.watch.setOutcomeMode("none") // broker never publishes an outcome
	h.driveToStreamInfoStage(t)

	// Past the bounded deadline (recoveryOutcomeDeadline) with no outcome.
	h.tick(recoveryOutcomeDeadline+time.Minute, true, 3)
	if st := h.state(t); st.RecoveryStage != 3 {
		t.Fatalf("a timeout must not itself advance the stage number, got %+v", st)
	}
	// After the timeout the pipeline resumes (cooldown 0) on the next pass.
	h.tick(10*time.Minute, true, 3)
	if got := h.prober.callCount(); got != 1 {
		t.Fatalf("after a bounded timeout the pipeline must continue, got probe=%d", got)
	}
}

// E11: real progress that resumes while a stage is pending clears the episode
// (pending and all) — the drop is healthy again, no stale pending lingers.
func TestWatchdogProgressResumeClearsPending(t *testing.T) {
	h := newWatchdogHarness(t)
	h.watch.setOutcomeMode("none")
	h.driveToStreamInfoStage(t)

	// Twitch credits minutes while the stage is still pending.
	h.campaign.Drops[0].CurrentMinutesWatched = 150
	h.tick(5*time.Minute, true, 1)

	st := h.state(t)
	if st.Status != ProgressHealthy || st.RecoveryStage != 0 {
		t.Fatalf("resumed progress must clear the pending episode entirely, got %+v", st)
	}

	// A late outcome for the abandoned request must not resurrect recovery.
	req := h.watch.lastRequest("chan")
	h.watch.injectOutcome("chan", watcher.SessionRefreshOutcome{RequestID: req.RequestID, Signature: req.Signature, Success: true})
	h.tick(5*time.Minute, true, 1)
	if st := h.state(t); st.Status != ProgressHealthy {
		t.Fatalf("a late outcome must not resurrect a cleared episode, got %+v", st)
	}
}

// E12: the critical stalled notification remains transition-only across the whole
// async pipeline — exactly one per episode, none until the terminal stage.
func TestWatchdogAsyncNotificationTransitionOnly(t *testing.T) {
	h := newWatchdogHarness(t)
	h.driveToStall(t)
	if n := len(h.notifier.byKind("stalled")); n != 0 {
		t.Fatalf("no critical notification before the terminal stage, got %d", n)
	}
	h.driveToExhaustion(t)
	if n := len(h.notifier.byKind("stalled")); n != 1 {
		t.Fatalf("exactly one critical notification per episode, got %d", n)
	}
	// Extra passes past exhaustion must not re-notify.
	h.tick(10*time.Minute, true, 3)
	h.tick(10*time.Minute, true, 3)
	if n := len(h.notifier.byKind("stalled")); n != 1 {
		t.Fatalf("the notification must not repeat, got %d", n)
	}
}
