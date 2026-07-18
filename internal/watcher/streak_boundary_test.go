package watcher

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// TestStreakCapConstantsSplit (Q1) pins the split hard cap: the pursuit cap is the
// expected grant point PLUS a positive delivery grace, and the grace genuinely
// extends the hold past the expected minute. Collapsing the grace to zero (or
// folding the cap back to a single 15-minute constant) fails here.
func TestStreakCapConstantsSplit(t *testing.T) {
	if streakExpectedGrantMinutes <= 0 {
		t.Fatalf("expected-grant reference must be positive, got %v", streakExpectedGrantMinutes)
	}
	if streakDeliveryGraceMinutes <= 0 {
		t.Fatalf("delivery grace must be positive, got %v", streakDeliveryGraceMinutes)
	}
	if streakPursuitCapMinutes != streakExpectedGrantMinutes+streakDeliveryGraceMinutes {
		t.Fatalf("hard cap must equal expected+grace: cap=%v expected=%v grace=%v",
			streakPursuitCapMinutes, streakExpectedGrantMinutes, streakDeliveryGraceMinutes)
	}
	// The grace must actually push the release past the expected grant point —
	// otherwise the seat would drop exactly at the minute the async WATCH_STREAK is
	// most likely to be triggered, the very boundary the grace exists to cover.
	if streakPursuitCapMinutes <= streakExpectedGrantMinutes {
		t.Fatalf("hard cap %v must exceed the expected grant point %v", streakPursuitCapMinutes, streakExpectedGrantMinutes)
	}
}

// TestBoostHeldThroughGraceReleasedAtHardCap (Q1) proves the release boundary: a
// pending streak is held at the expected grant point and all through the delivery
// grace, and released only when the continuous-watch minutes reach the hard cap.
// This is the mutation anchor for the 15/19/20 split.
func TestBoostHeldThroughGraceReleasedAtHardCap(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 10)

	// At the expected grant point: NOT released — the grace begins here so a grant
	// triggered by this very minute can still land while we keep watching.
	s.Stream.MinuteWatched = streakExpectedGrantMinutes
	if !w.isBoostEligible(0) {
		t.Errorf("at the expected grant point (%.0f min) the seat must be held through the grace", streakExpectedGrantMinutes)
	}
	// Upper edge of the grace (just under the hard cap): still held.
	s.Stream.MinuteWatched = streakPursuitCapMinutes - 1
	if !w.isBoostEligible(0) {
		t.Errorf("just under the hard cap (%.0f min) the seat must still be held", streakPursuitCapMinutes-1)
	}
	// Hard cap reached: released.
	s.Stream.MinuteWatched = streakPursuitCapMinutes
	if w.isBoostEligible(0) {
		t.Errorf("at the hard cap (%.0f min) the seat must be released", streakPursuitCapMinutes)
	}
}

// TestStreakGrantDuringGraceReleasesImmediately (Q1) proves the authoritative
// WATCH_STREAK grant beats the cap: a grant delivered mid-grace releases the seat
// at once, regardless of minutes — the grace is a fallback ceiling, not a wait.
func TestStreakGrantDuringGraceReleasesImmediately(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 10)

	s.Stream.MinuteWatched = streakExpectedGrantMinutes + 2 // inside the grace window
	if !w.isBoostEligible(0) {
		t.Fatal("a pending streak inside the delivery grace must stay boost-eligible")
	}
	s.Stream.MarkStreakEarned("b1") // the real grant lands mid-grace
	if w.isBoostEligible(0) {
		t.Error("the authoritative WATCH_STREAK grant must release the seat immediately, before the hard cap")
	}
}

// TestSlotLossResetsContinuityForLostSlotKeepsHeld (Q2) proves the slot-loss
// continuity reset is SLOT-based, not timestamp-based: a configured channel that
// leaves this tick's slots has its continuous-watch accumulator reset, while one
// that keeps its slot is untouched — and the reset preserves the streak identity.
func TestSlotLossResetsContinuityForLostSlotKeepsHeld(t *testing.T) {
	w, _ := newTestWatcher(2)
	a, b := w.streamers[0], w.streamers[1]
	a.Stream.Update("ba", "t", nil, nil, 10)
	b.Stream.Update("bb", "t", nil, nil, 10)
	a.Stream.MinuteWatched = 8
	b.Stream.MinuteWatched = 8

	// Tick 1: both configured channels hold a slot (establishes the baseline set).
	w.resetLostSlotContinuity([]slotOccupant{{streamer: a, idx: 0}, {streamer: b, idx: 1}})

	// Tick 2: A loses its slot, B keeps it.
	w.resetLostSlotContinuity([]slotOccupant{{streamer: b, idx: 1}})

	if a.Stream.GetMinuteWatched() != 0 {
		t.Errorf("A lost its slot: continuity must reset to 0, got %v", a.Stream.GetMinuteWatched())
	}
	if b.Stream.GetMinuteWatched() != 8 {
		t.Errorf("B kept its slot: continuity must be preserved (8), got %v", b.Stream.GetMinuteWatched())
	}
	if w.streakPursuitExhausted(0) {
		t.Error("A's reset continuity must be well under the cap (not exhausted)")
	}
	// Streak identity survives the slot loss on A.
	if !a.Stream.StreakPending() {
		t.Error("slot loss must not clear StreakPending (a late WATCH_STREAK is still accepted)")
	}
}

// TestSlotLossAllSlotsResetsContinuity (Q2) covers the losing-EVERY-slot case
// (len(slots)==0), which the hook must handle before the no-slots early return:
// a channel watched last tick and absent this tick still resets.
func TestSlotLossAllSlotsResetsContinuity(t *testing.T) {
	w, _ := newTestWatcher(1)
	a := w.streamers[0]
	a.Stream.Update("ba", "t", nil, nil, 10)
	a.Stream.MinuteWatched = 12

	w.resetLostSlotContinuity([]slotOccupant{{streamer: a, idx: 0}}) // watched last tick
	w.resetLostSlotContinuity(nil)                                   // nothing watched this tick

	if a.Stream.GetMinuteWatched() != 0 {
		t.Errorf("losing every slot must reset continuity to 0, got %v", a.Stream.GetMinuteWatched())
	}
	if !a.Stream.StreakPending() {
		t.Error("losing every slot must not clear StreakPending")
	}
}

// TestReleaseLogZeroEvidenceAddsTransportNote (Q4) proves the bounded-timeout
// release stays OUTCOME-NEUTRAL even with zero WATCH evidence, and only then adds
// the narrow, non-outcome transport/authorization hint. It never claims the streak
// was not earned.
func TestReleaseLogZeroEvidenceAddsTransportNote(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 10)

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Reach the hard cap with ZERO WATCH credits recorded for this broadcast.
	s.Stream.MinuteWatched = streakPursuitCapMinutes
	w.noteStreakProgress(0)
	w.noteStreakProgress(0) // still logged exactly once

	out := buf.String()
	if got := strings.Count(out, "Releasing the watch-streak boost slot"); got != 1 {
		t.Fatalf("release logged %d times, want exactly 1:\n%s", got, out)
	}
	if !strings.Contains(out, "releaseReason=bounded_timeout") || !strings.Contains(out, "outcome=unknown") {
		t.Errorf("zero-evidence release must still be releaseReason=bounded_timeout outcome=unknown:\n%s", out)
	}
	if !strings.Contains(out, "check authorization/transport") {
		t.Errorf("with no WATCH credits the release must note the transport/authorization check:\n%s", out)
	}
	if !strings.Contains(out, "watchEvents=0") {
		t.Errorf("release must carry the zero evidence count:\n%s", out)
	}
	// Even with zero evidence it must not assert the streak outcome.
	for _, banned := range []string{"granted no streak", "could not be earned", "not payable"} {
		if strings.Contains(out, banned) {
			t.Errorf("zero-evidence release must not assert the streak outcome (%q):\n%s", banned, out)
		}
	}
}
