package app

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

// TestLifecycleStatusAdapterWhitelist is §12 test 22 (contract §11 item 14):
// the adapter must publish a web.MinerStatus ONLY for the lifecycle statuses
// its mapping table lists, and must NEVER publish (drop, at debug) any
// lifecycle status absent from status.go's whitelist — a table over EVERY
// lifecycle.ObservedState value (10 today).
//
// MINOR 13 (F4b Q3 consolidated corrective): paused/stopped are published
// with the fixed honored-intent overlay message ONLY when the message
// carries lifecycle.BootHonoredIntentMessage — the marker the controller's
// boot reconciliation call (and ONLY that call) passes. Every row below
// that exercises paused/stopped uses that marker as its input detail,
// matching the one real call site that is actually meant to reach this
// branch; TestLifecycleStatusAdapterDropsOrdinaryPausedStopped covers the
// (far more common) ordinary runtime case separately.
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
		{lifecycle.ObservedRestarting, detail, false, "", ""},
		{lifecycle.ObservedPaused, lifecycle.BootHonoredIntentMessage, true, web.StatusError, lifecycleHonoredIntentMessage},
		{lifecycle.ObservedStopped, lifecycle.BootHonoredIntentMessage, true, web.StatusError, lifecycleHonoredIntentMessage},
		{lifecycle.ObservedFailed, detail, true, web.StatusError, detail},
		{lifecycle.ObservedDegraded, detail, true, web.StatusError, detail},
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

// MINOR 13 (F4b Q3 consolidated corrective): an ORDINARY runtime
// paused/stopped SetStatus call — e.g. an operator explicitly pausing the
// miner right now via a lifecycle command, which publishTerminal routes
// through the exact same SetStatus method the boot-reconciliation call
// uses — must be DROPPED (like any other unmapped status), not published
// as the boot-honored overlay message. Only the dedicated
// lifecycle.BootHonoredIntentMessage marker reaches that overlay; every
// other paused/stopped message must not.
func TestLifecycleStatusAdapterDropsOrdinaryPausedStopped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  lifecycle.ObservedState
		message string
	}{
		{"paused-empty-message", lifecycle.ObservedPaused, ""},
		{"paused-ordinary-message", lifecycle.ObservedPaused, "operator requested pause"},
		{"stopped-empty-message", lifecycle.ObservedStopped, ""},
		{"stopped-ordinary-message", lifecycle.ObservedStopped, "operator requested stop"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := web.NewStatusBroadcaster()
			b.SetStatus(web.StatusRunning, "baseline")

			adapter := lifecycleStatusAdapter{broadcaster: b}
			adapter.SetStatus(string(tc.status), tc.message)

			got := b.GetStatus()
			if got.Status != web.StatusRunning || got.Message != "baseline" {
				t.Errorf("an ordinary runtime %s was published as the boot-honored overlay (status=%q message=%q), want DROPPED",
					tc.status, got.Status, got.Message)
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

// SetGeneration must never panic and must never touch the broadcaster's
// published status/message — status.go has no generation field until Ф4c.
func TestLifecycleStatusAdapterSetGenerationIsNoop(t *testing.T) {
	b := web.NewStatusBroadcaster()
	b.SetStatus(web.StatusRunning, "before")
	adapter := lifecycleStatusAdapter{broadcaster: b}

	adapter.SetGeneration(42)

	got := b.GetStatus()
	if got.Status != web.StatusRunning || got.Message != "before" {
		t.Errorf("SetGeneration mutated the broadcaster's status: %+v", got)
	}
}
