package models

import (
	"sync"
	"testing"
	"time"
)

// TestArmWatchStreakResetsSessionAtomically (§4.1/§5.2) proves the streak
// session's local fields — the missing flag, the minute counter, its timestamp,
// and the WATCH-evidence counter — all reset TOGETHER when a new broadcast
// re-arms. Any one of them surviving would let a phantom pursuit continue (stale
// minutes / stale evidence) or, conversely, count wall-clock instead of watched
// time. The grant binding is preserved (that is what StreakPending consults).
func TestArmWatchStreakResetsSessionAtomically(t *testing.T) {
	s := NewStream()
	s.Update("bcast-1", "t", nil, nil, 10) // identify the first broadcast

	// Accrue some session state on broadcast 1.
	s.UpdateMinuteWatched(0) // seed the timestamp
	time.Sleep(2 * time.Millisecond)
	s.UpdateMinuteWatched(0)
	s.NoteWatchPointsEvent()
	s.NoteWatchPointsEvent()
	s.MinuteWatched = 6

	if s.StreakWatchEvidence() == 0 {
		t.Fatal("setup: expected some WATCH evidence banked on broadcast 1")
	}

	// A genuinely new broadcast ID re-arms and must wipe every session-local field.
	s.Update("bcast-2", "t", nil, nil, 10)

	if s.MinuteWatched != 0 {
		t.Errorf("MinuteWatched = %v, want 0 after re-arm", s.MinuteWatched)
	}
	if !s.minuteWatchedUpdated.IsZero() {
		t.Errorf("minuteWatchedUpdated must be zeroed with the counter (else the next tick credits a wall-clock gap)")
	}
	if s.StreakWatchEvidence() != 0 {
		t.Errorf("StreakWatchEvidence = %d, want 0 after re-arm (evidence must not carry across broadcasts)", s.StreakWatchEvidence())
	}
	if !s.StreakPending() {
		t.Errorf("a fresh broadcast with no grant recorded for it must be pending")
	}
}

// TestInterruptedPursuitCannotReachCapOnWallClock (Q3) proves the bounded streak
// window means CONTINUOUSLY-watched minutes, not wall-clock between ACKs: a pursuit
// interrupted by a rotation gap larger than maxGap resets MinuteWatched to 0, so
// the accumulated total across the gap can never reach the cap by elapsed time —
// the boost is released only after genuinely watching the channel that long.
func TestInterruptedPursuitCannotReachCapOnWallClock(t *testing.T) {
	s := NewStream()
	s.Update("b1", "t", nil, nil, 10)
	maxGap := 2 * time.Minute

	// Accrue ~14 continuous minutes (just under a 15 cap), one report step.
	s.UpdateMinuteWatched(maxGap) // anchor the continuity clock
	s.MinuteWatched = 13
	s.minuteWatchedUpdated = time.Now().Add(-60 * time.Second)
	s.UpdateMinuteWatched(maxGap) // -> ~14 continuous
	if s.MinuteWatched < 13.9 {
		t.Fatalf("setup: expected ~14 continuous minutes, got %v", s.MinuteWatched)
	}

	// The channel is rotated OUT: the next report arrives after a gap > maxGap.
	s.minuteWatchedUpdated = time.Now().Add(-10 * time.Minute)
	delta := s.UpdateMinuteWatched(maxGap)

	if delta != 0 || s.MinuteWatched != 0 {
		t.Fatalf("a rotation gap must reset the continuous counter to 0, got delta=%v total=%v", delta, s.MinuteWatched)
	}
	// ~24 min of WALL-CLOCK elapsed, yet the continuous total is 0 again: a
	// fragmented pursuit can never reach the cap by elapsed time alone.
}

// TestResetWatchContinuityBreaksAccumulatorPreservesIdentity (Q2) proves the
// slot-loss reset zeroes ONLY the continuous-watch accumulator and its timestamp,
// so a slot regained within maxGap does not stitch the unwatched interval, while
// the streak identity (pending, remembered grant, WATCH evidence) is preserved.
func TestResetWatchContinuityBreaksAccumulatorPreservesIdentity(t *testing.T) {
	s := NewStream()
	s.Update("b1", "t", nil, nil, 10)
	maxGap := 2 * time.Minute

	// Bank ~10 continuous minutes with a RECENT (sub-maxGap) last report, plus one
	// WATCH-evidence credit.
	s.UpdateMinuteWatched(maxGap) // anchor the continuity clock
	s.MinuteWatched = 10
	s.minuteWatchedUpdated = time.Now().Add(-30 * time.Second)
	s.NoteWatchPointsEvent()

	// A real slot loss breaks continuity.
	s.ResetWatchContinuity()

	if s.MinuteWatched != 0 {
		t.Errorf("ResetWatchContinuity must zero MinuteWatched, got %v", s.MinuteWatched)
	}
	if !s.minuteWatchedUpdated.IsZero() {
		t.Error("ResetWatchContinuity must zero the report timestamp, else the next report stitches the unwatched gap")
	}

	// Regain within the old sub-maxGap wall-clock: the broken interval must NOT be
	// credited — the next report re-anchors from zero and returns 0.
	delta := s.UpdateMinuteWatched(maxGap)
	if delta != 0 || s.MinuteWatched != 0 {
		t.Fatalf("a slot-loss break must not be stitched on regain, got delta=%v total=%v", delta, s.MinuteWatched)
	}

	// Identity preserved: still pending, evidence counter untouched.
	if !s.StreakPending() {
		t.Error("ResetWatchContinuity must preserve StreakPending (a late WATCH_STREAK is still accepted)")
	}
	if s.StreakWatchEvidence() != 1 {
		t.Errorf("ResetWatchContinuity must not touch the WATCH-evidence counter, got %d", s.StreakWatchEvidence())
	}
}

// TestNoteWatchPointsEventCounts covers the evidence counter and its reset.
func TestNoteWatchPointsEventCounts(t *testing.T) {
	s := NewStream()
	s.Update("b1", "t", nil, nil, 10)
	if got := s.NoteWatchPointsEvent(); got != 1 {
		t.Errorf("first WATCH event count = %d, want 1", got)
	}
	if got := s.NoteWatchPointsEvent(); got != 2 {
		t.Errorf("second WATCH event count = %d, want 2", got)
	}
	if s.StreakWatchEvidence() != 2 {
		t.Errorf("StreakWatchEvidence = %d, want 2", s.StreakWatchEvidence())
	}
	// A new broadcast resets it.
	s.Update("b2", "t", nil, nil, 10)
	if s.StreakWatchEvidence() != 0 {
		t.Errorf("evidence must reset to 0 on a new broadcast, got %d", s.StreakWatchEvidence())
	}
}

// TestMarkStreakEarnedAcceptedRegardlessOfMinutes (§5.3/§5.4) proves the streak
// grant is accepted whether it arrives early (before seven minutes) or late
// (well after) — the model never gates acceptance on watched minutes, only on
// the real WATCH_STREAK event making the streak no longer missing for the
// current broadcast.
func TestMarkStreakEarnedAcceptedRegardlessOfMinutes(t *testing.T) {
	for _, mins := range []float64{3, 8, 12} {
		s := NewStream()
		s.Update("b1", "t", nil, nil, 10)
		s.MinuteWatched = mins
		if !s.StreakPending() {
			t.Fatalf("mins=%v: expected pending before the grant", mins)
		}
		s.MarkStreakEarned("b1")
		if s.StreakPending() {
			t.Errorf("mins=%v: a WATCH_STREAK grant must be accepted and end the pursuit", mins)
		}
	}
}

// TestLateGrantAfterEvidenceStillEndsPursuit (§5.14) proves that WATCH evidence
// never marks the streak earned (no phantom grant): StreakPending stays true after
// any number of WATCH events, and only the real WATCH_STREAK grant clears it — so
// a late grant is always accepted whenever it arrives.
func TestLateGrantAfterEvidenceStillEndsPursuit(t *testing.T) {
	s := NewStream()
	s.Update("b1", "t", nil, nil, 10)
	s.MinuteWatched = 9
	s.NoteWatchPointsEvent()
	s.NoteWatchPointsEvent()

	// Evidence alone must NOT mark the streak earned — no phantom grant.
	if !s.StreakPending() {
		t.Fatal("WATCH evidence must never mark the streak earned; pursuit must stay pending")
	}
	// The late real grant is accepted.
	s.MarkStreakEarned("b1")
	if s.StreakPending() {
		t.Error("a late WATCH_STREAK after the evidence fallback must still be accepted")
	}
}

// TestUpdateHistoryRecordsWatchStreakOnce (§5.15) proves one WATCH_STREAK event
// increments the history counter exactly once and marks the streak earned — the
// authoritative, single-count recording path (the analytics rows/annotation are
// driven from the same single event, verified elsewhere).
func TestUpdateHistoryRecordsWatchStreakOnce(t *testing.T) {
	s := NewStreamer("chan", DefaultStreamerSettings())
	s.Stream.Update("b1", "t", nil, nil, 10)

	s.UpdateHistory("WATCH_STREAK", 350)
	if e := s.History["WATCH_STREAK"]; e == nil || e.Counter != 1 || e.Amount != 350 {
		t.Fatalf("one WATCH_STREAK event must record exactly once, got %+v", s.History["WATCH_STREAK"])
	}
	if s.Stream.StreakPending() {
		t.Error("WATCH_STREAK event must end the pursuit for this broadcast")
	}
}

// TestWatchEvidenceReArmRaceIsSafe (§5.16) drives NoteWatchPointsEvent (PubSub
// goroutine) concurrently with re-arm and reads (watcher goroutine) under -race.
func TestWatchEvidenceReArmRaceIsSafe(t *testing.T) {
	s := NewStream()
	s.Update("b1", "t", nil, nil, 10)

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			s.NoteWatchPointsEvent()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			_ = s.StreakWatchEvidence()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 500; i++ {
			s.Update("b1", "t", nil, nil, 10) // same ID (no re-arm) + occasional new
			if i%100 == 0 {
				s.InitWatchStreak()
			}
		}
	}()
	wg.Wait()
}
