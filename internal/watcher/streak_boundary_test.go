package watcher

import (
	"bytes"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// The 20m Stream-owned cap is the only behavioral boundary. Fifteen minutes is
// retained solely as diagnostics and causes no transition.
func TestStreakCapOwnedByStream(t *testing.T) {
	if streakExpectedGrantMinutes <= 0 {
		t.Fatalf("expected-grant reference must be positive, got %v", streakExpectedGrantMinutes)
	}
	if streakPursuitCapMinutes != models.WatchStreakPursuitCapMinutes {
		t.Fatalf("watcher cap=%v model cap=%v, want one exact 20m owner", streakPursuitCapMinutes, models.WatchStreakPursuitCapMinutes)
	}
	if streakPursuitCapMinutes != 20 {
		t.Fatalf("hard cap=%v, want exact 20m", streakPursuitCapMinutes)
	}
}

func TestBoostVerdictBoundaries(t *testing.T) {
	for _, minutes := range []float64{0, 7, 8, 15, 19} {
		t.Run(fmt.Sprintf("%.0fm", minutes), func(t *testing.T) {
			w, _ := newTestWatcher(1)
			s := w.streamers[0]
			s.Stream.Update("b1", "t", nil, nil, 10)
			s.Stream.MinuteWatched = minutes
			if !w.isBoostEligible(0) {
				t.Fatalf("%.0fm must remain pursuit-eligible", minutes)
			}
			decision := s.Stream.EvaluateWatchStreak(time.Now())
			want := models.WatchStreakPursuing
			if minutes == 0 {
				want = models.WatchStreakEligible
			}
			if decision.State != want || decision.Transitioned {
				t.Fatalf("%.0fm decision=%+v, want %s without transition", minutes, decision, want)
			}
		})
	}

	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 10)
	s.Stream.MinuteWatched = 20
	if w.isBoostEligible(0) {
		t.Fatal("exactly 20m must release pursuit priority")
	}
	if got := s.Stream.EvaluateWatchStreak(time.Now()).State; got != models.WatchStreakTimedOutUnknown {
		t.Fatalf("exactly 20m state=%s, want TIMED_OUT_UNKNOWN", got)
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
	acceptBoundStreakForWatcherTest(t, s.Stream, "grant-b1", "b1")
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
