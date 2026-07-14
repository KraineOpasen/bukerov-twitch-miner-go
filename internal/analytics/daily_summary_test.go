package analytics

import (
	"testing"
	"time"
)

// EarnedPointsBetween is a GLOBAL sum across all streamers, and the analytics
// tests share one process-wide DB, so each earned-points test seeds a distinct
// far-future window that no other test writes into (and that the retention
// sweep, which only deletes rows older than a past cutoff, never touches),
// isolating the assertion.

func TestEarnedPointsBetweenNetDelta(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2035, 6, 1, 12, 0, 0, 0, time.UTC)
	// Two streamers; earned = sum of per-streamer (last - first) in window.
	seedPoint(t, r, "ep-a", base.Add(-3*time.Hour), 1000, "WATCH")
	seedPoint(t, r, "ep-a", base.Add(-2*time.Hour), 1200, "CLAIM")
	seedPoint(t, r, "ep-a", base.Add(-1*time.Hour), 1150, "Spent") // net a: 1150-1000 = 150
	seedPoint(t, r, "ep-b", base.Add(-2*time.Hour), 500, "WATCH")
	seedPoint(t, r, "ep-b", base.Add(-1*time.Hour), 800, "WATCH") // net b: 300

	got, err := r.EarnedPointsBetween(base.Add(-4*time.Hour), base)
	if err != nil {
		t.Fatalf("earned: %v", err)
	}
	if got != 450 {
		t.Fatalf("earned net = %d, want 450 (150 + 300)", got)
	}
}

func TestEarnedPointsBetweenWindowExcludesOutside(t *testing.T) {
	r := newTestRepo(t)
	base := time.Date(2036, 6, 1, 12, 0, 0, 0, time.UTC)
	seedPoint(t, r, "ep-win", base.Add(-50*time.Hour), 100, "WATCH") // outside
	seedPoint(t, r, "ep-win", base.Add(-2*time.Hour), 1000, "WATCH")
	seedPoint(t, r, "ep-win", base.Add(-1*time.Hour), 1100, "WATCH")

	// Only the two in-window samples count: 1100 - 1000 = 100.
	got, err := r.EarnedPointsBetween(base.Add(-3*time.Hour), base)
	if err != nil {
		t.Fatalf("earned: %v", err)
	}
	if got != 100 {
		t.Fatalf("windowed earned = %d, want 100", got)
	}
}

func TestEarnedPointsBetweenEmptyIsZero(t *testing.T) {
	r := newTestRepo(t)
	// A window in 2010 that no test writes to.
	start := time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)
	got, err := r.EarnedPointsBetween(start, start.Add(time.Hour))
	if err != nil {
		t.Fatalf("earned: %v", err)
	}
	if got != 0 {
		t.Fatalf("empty window earned = %d, want 0", got)
	}
}

func TestCountAnnotationsByType(t *testing.T) {
	r := newTestRepo(t)
	now := time.Now()
	// Record via the repo directly so timestamps are "now"; use a distinct type
	// so other tests' annotations don't interfere.
	for i := 0; i < 3; i++ {
		if err := r.RecordAnnotation("ca-streamer", "TESTSTREAK", "streak", "#fff"); err != nil {
			t.Fatalf("record: %v", err)
		}
	}
	if err := r.RecordAnnotation("ca-streamer", "OTHER", "x", "#000"); err != nil {
		t.Fatalf("record other: %v", err)
	}

	n, err := r.CountAnnotationsByType("TESTSTREAK", now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Fatalf("count TESTSTREAK = %d, want 3", n)
	}

	// A window in the far past sees none.
	n, _ = r.CountAnnotationsByType("TESTSTREAK", now.Add(-48*time.Hour), now.Add(-47*time.Hour))
	if n != 0 {
		t.Fatalf("past-window count = %d, want 0", n)
	}
}

func TestDropsBucketHiddenFromListStreamers(t *testing.T) {
	r := newTestRepo(t)
	// A real streamer and a drop-claim annotation under the hidden bucket.
	if err := r.RecordPoints("visible-streamer", 100, "WATCH"); err != nil {
		t.Fatalf("record points: %v", err)
	}
	if err := r.RecordAnnotation(DropsBucket, "DROP_CLAIMED", "Some Drop", dropsBucketTestColor); err != nil {
		t.Fatalf("record drop claim: %v", err)
	}

	streamers, err := r.ListStreamers()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	sawVisible := false
	for _, s := range streamers {
		if s.Name == DropsBucket {
			t.Fatalf("hidden bucket %q must not appear in the streamer list", DropsBucket)
		}
		if s.Name == "visible-streamer" {
			sawVisible = true
		}
	}
	if !sawVisible {
		t.Fatal("real streamer should still be listed")
	}

	// But its DROP_CLAIMED annotations are still countable.
	n, err := r.CountAnnotationsByType("DROP_CLAIMED", time.Now().Add(-time.Minute), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("count claims: %v", err)
	}
	if n != 1 {
		t.Fatalf("drop-claimed count = %d, want 1", n)
	}
}

const dropsBucketTestColor = "#d9a25c"
