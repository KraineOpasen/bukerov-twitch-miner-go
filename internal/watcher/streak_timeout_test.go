package watcher

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// captureLogs redirects slog to a buffer for the duration of the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// watchTick marks the given configured streamer index as holding a slot this tick
// (drives the real slot-membership bookkeeping resetLostSlotContinuity consumes).
func watchTick(w *MinuteWatcher, idx int) {
	w.resetLostSlotContinuity([]slotOccupant{{streamer: w.streamers[idx], idx: idx}})
}

// idleTick marks a tick where NO configured streamer holds a slot, so any channel
// watched last tick has genuinely lost its slot (drives ResetWatchContinuity).
func idleTick(w *MinuteWatcher) {
	w.resetLostSlotContinuity(nil)
}

// TestTimedOutStreakPursuitDoesNotRearmSameBroadcast (T-H1) is the core regression
// for the repeat-pursuit defect: once the hard cap is hit for one broadcast, the
// continuity reset that follows a slot loss must NOT hand the same broadcast a
// fresh 20-minute pursuit window. Driven entirely through the production
// selection/eligibility/slot-transition functions.
func TestTimedOutStreakPursuitDoesNotRearmSameBroadcast(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 10)

	buf := captureLogs(t)

	// T1: under the cap and pending -> eligible; the channel holds the slot and
	// its continuous minutes reach the hard cap while watched.
	if !w.isBoostEligible(0) {
		t.Fatal("T1: a pending streak under the cap must be boost-eligible")
	}
	watchTick(w, 0) // A holds a slot this tick
	s.Stream.MinuteWatched = streakPursuitCapMinutes
	w.noteStreakProgress(0) // production release path: latches timeout + logs once

	// T2: cap reached -> not eligible; the slot is lost -> continuity resets to 0.
	if w.isBoostEligible(0) {
		t.Fatal("T2: at the hard cap the broadcast must not be boost-eligible")
	}
	idleTick(w) // A loses its slot -> ResetWatchContinuity
	if s.Stream.GetMinuteWatched() != 0 {
		t.Fatalf("T2: slot loss must reset continuity to 0, got %v", s.Stream.GetMinuteWatched())
	}

	// T3+: despite MinuteWatched being 0 again, the SAME broadcast must stay out.
	for tick := 0; tick < 4; tick++ {
		if w.isBoostEligible(0) {
			t.Fatalf("T3+ tick %d: timed-out broadcast re-armed a fresh pursuit after the continuity reset", tick)
		}
		w.noteStreakProgress(0) // must not re-log the release or change state
		idleTick(w)
	}

	// The latch is set and the streak identity is intact (a late grant is still
	// acceptable — StreakPending stays true).
	if !s.Stream.StreakPursuitTimedOut() {
		t.Error("the pursuit-timeout latch must be set for the timed-out broadcast")
	}
	if !s.Stream.StreakPending() {
		t.Error("StreakPending must survive the timeout so a late WATCH_STREAK is still accepted")
	}
	// The release logged exactly once across the whole sequence.
	if got := strings.Count(buf.String(), "Releasing the watch-streak boost slot"); got != 1 {
		t.Errorf("release logged %d times, want exactly 1 (transition-only):\n%s", got, buf.String())
	}
}

// TestPreCapSlotLossResetsContinuityButAllowsRetry (T-H2) proves the latch is not
// set by a slot loss BEFORE the cap: continuity resets, but the same pending
// broadcast may be pursued again (an interruption under the cap is not a timeout).
func TestPreCapSlotLossResetsContinuityButAllowsRetry(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 10)

	// Watched a while but strictly BELOW the hard cap.
	watchTick(w, 0)
	s.Stream.MinuteWatched = streakExpectedGrantMinutes // 15, under the cap 20
	w.noteStreakProgress(0)                             // pursuing, not exhausted -> no latch

	if s.Stream.StreakPursuitTimedOut() {
		t.Fatal("a pre-cap tick must not latch the pursuit timeout")
	}

	// The slot is lost before the cap.
	idleTick(w)
	if s.Stream.GetMinuteWatched() != 0 {
		t.Fatalf("pre-cap slot loss must reset continuity to 0, got %v", s.Stream.GetMinuteWatched())
	}
	if s.Stream.StreakPursuitTimedOut() {
		t.Error("a pre-cap slot loss must NOT latch the pursuit timeout")
	}
	if !s.Stream.StreakPending() {
		t.Error("the streak is still pending after a pre-cap interruption")
	}
	// Retry allowed: the same pending broadcast is boost-eligible again.
	if !w.isBoostEligible(0) {
		t.Error("a pre-cap interruption must allow a fresh pursuit attempt for the same broadcast")
	}
}

// TestNewBroadcastClearsPursuitTimeout (T-H3) proves a genuinely new broadcast
// re-arms: the timeout latch clears, continuity restarts, and the channel is
// boost-eligible again.
func TestNewBroadcastClearsPursuitTimeout(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 10)

	// Time out b1 through the real gate.
	watchTick(w, 0)
	s.Stream.MinuteWatched = streakPursuitCapMinutes
	if !w.streakPursuitExhausted(0) { // gate call latches the timeout
		t.Fatal("setup: b1 must be exhausted at the hard cap")
	}
	if w.isBoostEligible(0) {
		t.Fatal("setup: timed-out b1 must not be boost-eligible")
	}

	// A genuinely new broadcast arms via Update -> armWatchStreakLocked.
	s.Stream.Update("b2", "t", nil, nil, 10)

	if s.Stream.StreakPursuitTimedOut() {
		t.Error("a new broadcast must clear the pursuit-timeout latch")
	}
	if s.Stream.GetMinuteWatched() != 0 {
		t.Errorf("a new broadcast must reset continuity, got %v", s.Stream.GetMinuteWatched())
	}
	if !s.Stream.StreakPending() {
		t.Error("a new broadcast with no grant must be pending")
	}
	if !w.isBoostEligible(0) {
		t.Error("a new broadcast must be boost-eligible again")
	}
}

// TestLateGrantAfterPursuitTimeoutRecordedOnce (T-H4) proves the authoritative
// WATCH_STREAK is still accepted after a timeout: recorded once, ends the pursuit,
// and never re-arms it — while the timeout machinery itself creates no award.
func TestLateGrantAfterPursuitTimeoutRecordedOnce(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 10)

	// Time out b1.
	watchTick(w, 0)
	s.Stream.MinuteWatched = streakPursuitCapMinutes
	if !w.streakPursuitExhausted(0) {
		t.Fatal("setup: b1 must be timed out")
	}
	idleTick(w)
	if w.isBoostEligible(0) {
		t.Fatal("setup: timed-out b1 must not be boost-eligible")
	}
	// The timeout path awards nothing — no WATCH_STREAK exists yet.
	if e := s.History["WATCH_STREAK"]; e != nil {
		t.Fatalf("the timeout machinery must not synthesize a streak award, got %+v", e)
	}

	// A late authoritative WATCH_STREAK arrives on the real recording path.
	s.UpdateHistory("WATCH_STREAK", 350)

	if e := s.History["WATCH_STREAK"]; e == nil || e.Counter != 1 || e.Amount != 350 {
		t.Fatalf("the late grant must be recorded exactly once, got %+v", s.History["WATCH_STREAK"])
	}
	if s.Stream.StreakPending() {
		t.Error("the grant must end the pursuit (StreakPending false)")
	}
	if w.isBoostEligible(0) {
		t.Error("a granted broadcast must not be boost-eligible")
	}
	if bid, _ := s.Stream.StreakEarnedGrant(); bid != "b1" {
		t.Errorf("the grant must bind to b1, got %q", bid)
	}

	// Further ticks after the grant must not re-arm a pursuit (no new award chance).
	for tick := 0; tick < 3; tick++ {
		idleTick(w)
		if w.isBoostEligible(0) {
			t.Fatalf("tick %d: a granted broadcast must never become boost-eligible again", tick)
		}
	}
	if e := s.History["WATCH_STREAK"]; e.Counter != 1 {
		t.Errorf("no ticks after the grant may add a second award, counter=%d", e.Counter)
	}
}

// TestPostTimeoutTicksAreInert (T-H5) proves many ticks after a timeout stay
// inert: no boost eligibility, no repeated release log, no synthetic WATCH_STREAK
// award, and the latch persists.
func TestPostTimeoutTicksAreInert(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 10)

	buf := captureLogs(t)

	// Reach the hard cap and log the single release.
	watchTick(w, 0)
	s.Stream.MinuteWatched = streakPursuitCapMinutes
	w.noteStreakProgress(0)
	idleTick(w)

	for tick := 0; tick < 6; tick++ {
		if w.isBoostEligible(0) {
			t.Fatalf("tick %d: timed-out broadcast must stay ineligible", tick)
		}
		w.noteStreakProgress(0) // no-op path; must not re-log or mutate
		idleTick(w)
		if !s.Stream.StreakPursuitTimedOut() {
			t.Fatalf("tick %d: the timeout latch must persist", tick)
		}
		if e := s.History["WATCH_STREAK"]; e != nil {
			t.Fatalf("tick %d: no synthetic WATCH_STREAK award may be created, got %+v", tick, e)
		}
	}

	if got := strings.Count(buf.String(), "Releasing the watch-streak boost slot"); got != 1 {
		t.Errorf("release logged %d times over many post-timeout ticks, want exactly 1:\n%s", got, buf.String())
	}
}
