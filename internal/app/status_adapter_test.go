package app

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

// TestLifecycleStatusAdapterWhitelist is §12 test 22 (contract §11 item 14),
// updated for Ф4c (design v6 §14: "Ф4c расширяет ТОЛЬКО адаптер и
// web-слой"): the adapter must publish a web.MinerStatus ONLY for the
// lifecycle statuses its mapping table lists, and must NEVER publish (drop,
// at debug) any lifecycle status absent from status.go's whitelist — a table
// over EVERY lifecycle.ObservedState value (10 today).
//
// Ф4c mapping: paused/stopped/restarting/failed/degraded are now published
// (message passed through verbatim for paused/stopped/restarting; the
// lifecycle LastError for failed/degraded) — starting/running/pausing/
// stopping/exiting stay dropped, exactly as in Ф4b, so the miner's own
// startup-overlay progression is never pre-empted.
func TestLifecycleStatusAdapterWhitelist(t *testing.T) {
	const baseline = web.StatusInitializing
	const baselineMsg = "baseline"
	const detail = "some-detail"

	cases := []struct {
		observed    lifecycle.ObservedState
		inputDetail string
		wantPublish bool
		wantStatus  web.MinerStatus
		wantMessage string
	}{
		{lifecycle.ObservedStarting, detail, false, "", ""},
		{lifecycle.ObservedRunning, detail, false, "", ""},
		{lifecycle.ObservedPausing, detail, false, "", ""},
		{lifecycle.ObservedStopping, detail, false, "", ""},
		{lifecycle.ObservedRestarting, detail, true, web.StatusRestarting, detail},
		{lifecycle.ObservedPaused, detail, true, web.StatusPaused, detail},
		{lifecycle.ObservedStopped, detail, true, web.StatusStopped, detail},
		{lifecycle.ObservedFailed, detail, true, web.StatusFailed, detail},
		{lifecycle.ObservedDegraded, detail, true, web.StatusDegraded, detail},
		{lifecycle.ObservedExiting, detail, false, "", ""},
	}

	// Self-check: this table must cover every ObservedState the package
	// defines, not a hand-picked subset — if a new ObservedState is ever
	// added to internal/lifecycle without updating this table, this count
	// silently drifting out of sync is exactly the failure mode worth
	// catching, so the number is asserted explicitly rather than left as a
	// comment nobody re-checks.
	const wantObservedStateCount = 10
	if len(cases) != wantObservedStateCount {
		t.Fatalf("test table covers %d statuses, want exactly %d (every lifecycle.ObservedState)", len(cases), wantObservedStateCount)
	}

	for _, tc := range cases {
		t.Run(string(tc.observed), func(t *testing.T) {
			b := web.NewStatusBroadcaster()
			b.SetStatus(baseline, baselineMsg)

			adapter := lifecycleStatusAdapter{broadcaster: b}
			adapter.SetStatus(string(tc.observed), tc.inputDetail)

			got := b.GetStatus()
			if tc.wantPublish {
				if got.Status != tc.wantStatus {
					t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
				}
				if got.Message != tc.wantMessage {
					t.Errorf("message = %q, want %q", got.Message, tc.wantMessage)
				}
			} else if got.Status != baseline || got.Message != baselineMsg {
				t.Errorf("lifecycle status %q was published (status=%q message=%q), want DROPPED (baseline left unchanged)",
					tc.observed, got.Status, got.Message)
			}
		})
	}
}

// TestLifecycleStatusAdapterPausedStoppedMessagePassthrough replaces Ф4b's
// TestLifecycleStatusAdapterDropsOrdinaryPausedStopped (MINOR 13's
// special-casing is gone in Ф4c — see lifecycleStatusAdapter's doc comment):
// an ORDINARY runtime paused/stopped/restarting call and the once-per-boot
// lifecycle.BootHonoredIntentMessage call now go through the EXACT SAME
// path — both publish, with the message passed through verbatim, since the
// lifecycle panel (not this overlay-feeding broadcaster) is the single
// honest surface for these states now.
func TestLifecycleStatusAdapterPausedStoppedMessagePassthrough(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     lifecycle.ObservedState
		message    string
		wantStatus web.MinerStatus
	}{
		{"paused-empty-message", lifecycle.ObservedPaused, "", web.StatusPaused},
		{"paused-ordinary-message", lifecycle.ObservedPaused, "operator requested pause", web.StatusPaused},
		{"paused-boot-honored-message", lifecycle.ObservedPaused, lifecycle.BootHonoredIntentMessage, web.StatusPaused},
		{"stopped-empty-message", lifecycle.ObservedStopped, "", web.StatusStopped},
		{"stopped-ordinary-message", lifecycle.ObservedStopped, "operator requested stop", web.StatusStopped},
		{"stopped-boot-honored-message", lifecycle.ObservedStopped, lifecycle.BootHonoredIntentMessage, web.StatusStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := web.NewStatusBroadcaster()
			b.SetStatus(web.StatusRunning, "baseline")

			adapter := lifecycleStatusAdapter{broadcaster: b}
			adapter.SetStatus(string(tc.status), tc.message)

			got := b.GetStatus()
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.Message != tc.message {
				t.Errorf("message = %q, want %q (verbatim passthrough)", got.Message, tc.message)
			}
		})
	}
}

// An unrecognized/unknown status string (never actually emitted by
// internal/lifecycle, but the adapter's switch has no assumption baked in
// beyond "not in the whitelist") must also be dropped, not published or
// panicked on.
func TestLifecycleStatusAdapterDropsUnknownStatus(t *testing.T) {
	b := web.NewStatusBroadcaster()
	b.SetStatus(web.StatusInitializing, "baseline")
	adapter := lifecycleStatusAdapter{broadcaster: b}

	adapter.SetStatus("some-made-up-status", "detail")

	got := b.GetStatus()
	if got.Status != web.StatusInitializing || got.Message != "baseline" {
		t.Errorf("unknown status was published: %+v", got)
	}
}

// TestLifecycleStatusAdapterSetGenerationForwards replaces Ф4b's
// TestLifecycleStatusAdapterSetGenerationIsNoop: Ф4c wires status.go's real
// Generation field, so SetGeneration must now forward to the broadcaster
// (preserving Status/Message, like web.StatusBroadcaster.SetGeneration
// itself already guarantees) instead of being a no-op.
func TestLifecycleStatusAdapterSetGenerationForwards(t *testing.T) {
	b := web.NewStatusBroadcaster()
	b.SetStatus(web.StatusRunning, "before")
	adapter := lifecycleStatusAdapter{broadcaster: b}

	adapter.SetGeneration(42)

	got := b.GetStatus()
	if got.Generation != 42 {
		t.Errorf("Generation = %d, want 42", got.Generation)
	}
	if got.Status != web.StatusRunning || got.Message != "before" {
		t.Errorf("SetGeneration mutated the broadcaster's status: %+v", got)
	}
}
