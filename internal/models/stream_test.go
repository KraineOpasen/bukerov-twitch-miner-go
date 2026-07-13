package models

import (
	"testing"
	"time"
)

// TestUpdateMinuteWatchedAccumulatesContinuous verifies that a report arriving
// within maxGap of the previous one credits the elapsed minutes as continuous
// watch time, exactly as before the continuity guard was added.
func TestUpdateMinuteWatchedAccumulatesContinuous(t *testing.T) {
	s := NewStream()
	s.InitWatchStreak()
	s.MinuteWatched = 3
	s.minuteWatchedUpdated = time.Now().Add(-60 * time.Second)

	delta := s.UpdateMinuteWatched(2 * time.Minute)

	if delta < 0.9 || delta > 1.1 {
		t.Fatalf("expected ~1.0 minute credited, got %v", delta)
	}
	if s.MinuteWatched < 3.9 || s.MinuteWatched > 4.1 {
		t.Fatalf("expected MinuteWatched ~4.0, got %v", s.MinuteWatched)
	}
}

// TestUpdateMinuteWatchedResetsOnBreak is the core streak-continuity fix: a gap
// larger than maxGap means the streamer was not watched continuously (rotated
// out of a watch slot), so the counter must restart from zero and credit
// nothing for the gap - never cross the streak threshold on phantom minutes.
func TestUpdateMinuteWatchedResetsOnBreak(t *testing.T) {
	s := NewStream()
	s.InitWatchStreak()
	s.MinuteWatched = 6.5 // almost "past threshold" under the old wall-clock logic
	s.minuteWatchedUpdated = time.Now().Add(-5 * time.Minute)

	delta := s.UpdateMinuteWatched(2 * time.Minute)

	if delta != 0 {
		t.Fatalf("expected 0 minutes credited across a continuity break, got %v", delta)
	}
	if s.MinuteWatched != 0 {
		t.Fatalf("expected MinuteWatched reset to 0 after a break, got %v", s.MinuteWatched)
	}
}

// TestUpdateMinuteWatchedFirstCallReturnsZero keeps the fresh-session contract:
// the first report after InitWatchStreak has no prior timestamp to measure from,
// so it credits nothing and just anchors the continuity clock.
func TestUpdateMinuteWatchedFirstCallReturnsZero(t *testing.T) {
	s := NewStream()
	s.InitWatchStreak()

	delta := s.UpdateMinuteWatched(2 * time.Minute)

	if delta != 0 {
		t.Fatalf("expected first report to credit 0, got %v", delta)
	}
	if s.MinuteWatched != 0 {
		t.Fatalf("expected MinuteWatched to stay 0 on first report, got %v", s.MinuteWatched)
	}
	if s.minuteWatchedUpdated.IsZero() {
		t.Fatal("expected the continuity clock to be anchored after the first report")
	}
}

// TestUpdateMinuteWatchedZeroMaxGapIsUnbounded documents the escape hatch: a
// non-positive maxGap disables the break check and restores the historical
// wall-clock accumulation.
func TestUpdateMinuteWatchedZeroMaxGapIsUnbounded(t *testing.T) {
	s := NewStream()
	s.InitWatchStreak()
	s.MinuteWatched = 6.5
	s.minuteWatchedUpdated = time.Now().Add(-5 * time.Minute)

	delta := s.UpdateMinuteWatched(0)

	if delta < 4.5 {
		t.Fatalf("expected the full ~5 minute gap credited with maxGap=0, got %v", delta)
	}
	if s.MinuteWatched < 11 {
		t.Fatalf("expected unbounded accumulation (~11.5), got %v", s.MinuteWatched)
	}
}
