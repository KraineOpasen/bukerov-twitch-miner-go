package miner

import (
	"testing"
	"time"
)

func TestNextDailySummaryTime(t *testing.T) {
	loc := time.UTC
	// now before the target time today → fires today.
	now := time.Date(2026, 7, 14, 8, 0, 0, 0, loc)
	next := nextDailySummaryTime(now, "09:00")
	if !next.Equal(time.Date(2026, 7, 14, 9, 0, 0, 0, loc)) {
		t.Fatalf("expected today 09:00, got %v", next)
	}

	// now after the target time → fires tomorrow.
	now = time.Date(2026, 7, 14, 10, 0, 0, 0, loc)
	next = nextDailySummaryTime(now, "09:00")
	if !next.Equal(time.Date(2026, 7, 15, 9, 0, 0, 0, loc)) {
		t.Fatalf("expected tomorrow 09:00, got %v", next)
	}

	// exactly at the target time → next is tomorrow (strictly after now).
	now = time.Date(2026, 7, 14, 9, 0, 0, 0, loc)
	next = nextDailySummaryTime(now, "09:00")
	if !next.Equal(time.Date(2026, 7, 15, 9, 0, 0, 0, loc)) {
		t.Fatalf("at target time expected tomorrow, got %v", next)
	}
}

func TestNextDailySummaryTimeInvalidFallsBackTo0900(t *testing.T) {
	now := time.Date(2026, 7, 14, 6, 0, 0, 0, time.UTC)
	next := nextDailySummaryTime(now, "not-a-time")
	if next.Hour() != 9 || next.Minute() != 0 {
		t.Fatalf("invalid time must fall back to 09:00, got %v", next)
	}
}

func TestPreviousLocalDay(t *testing.T) {
	loc := time.UTC
	fire := time.Date(2026, 7, 14, 9, 0, 0, 0, loc)
	start, end := previousLocalDay(fire)
	if !start.Equal(time.Date(2026, 7, 13, 0, 0, 0, 0, loc)) {
		t.Errorf("start = %v, want 2026-07-13 00:00", start)
	}
	if !end.Equal(time.Date(2026, 7, 14, 0, 0, 0, 0, loc)) {
		t.Errorf("end = %v, want 2026-07-14 00:00", end)
	}
	// The window is a full calendar day.
	if end.Sub(start) != 24*time.Hour {
		t.Errorf("window = %v, want 24h", end.Sub(start))
	}
}
