package analytics

import (
	"os"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// TestMain opens the process-wide DB singleton against a durable directory
// before any test runs. database.Open is a sync.Once singleton, so if the first
// caller used a t.TempDir() it would be removed at that test's end and leave the
// shared handle pointing at a deleted file (readonly errors). Opening once here
// keeps the backing dir alive for the whole package run.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "analytics-test-*")
	if err != nil {
		panic(err)
	}
	if _, err := database.Open(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// newTestRepo returns a registered analytics repository backed by the shared
// singleton opened in TestMain. Tests isolate themselves by using unique
// streamer names rather than separate databases.
func newTestRepo(t *testing.T) *SQLiteRepository {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	repo, err := NewSQLiteRepository(db, t.TempDir())
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	return repo
}

// seedPoint inserts a points row at an explicit timestamp (RecordPoints always
// stamps time.Now(), so range/retention tests write directly).
func seedPoint(t *testing.T, r *SQLiteRepository, streamer string, ts time.Time, points int, event string) {
	t.Helper()
	id, err := r.getOrCreateStreamer(streamer)
	if err != nil {
		t.Fatalf("streamer id: %v", err)
	}
	if _, err := r.db.Exec(
		"INSERT INTO points (streamer_id, timestamp, points, event_type) VALUES (?, ?, ?, ?)",
		id, ts.UnixMilli(), points, event,
	); err != nil {
		t.Fatalf("seed point: %v", err)
	}
}

func seedAnnotation(t *testing.T, r *SQLiteRepository, streamer string, ts time.Time, eventType, text, color string) {
	t.Helper()
	id, err := r.getOrCreateStreamer(streamer)
	if err != nil {
		t.Fatalf("streamer id: %v", err)
	}
	if _, err := r.db.Exec(
		"INSERT INTO annotations (streamer_id, timestamp, text, color, event_type) VALUES (?, ?, ?, ?, ?)",
		id, ts.UnixMilli(), text, color, eventType,
	); err != nil {
		t.Fatalf("seed annotation: %v", err)
	}
}

// TestRecordPointsWritesRow verifies a recorded event is persisted and read back.
func TestRecordPointsWritesRow(t *testing.T) {
	repo := newTestRepo(t)
	const s = "rec_writes_row"

	if err := repo.RecordPoints(s, 12345, "WATCH"); err != nil {
		t.Fatalf("record: %v", err)
	}

	samples, err := repo.GetPointSamples(s, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("want 1 sample, got %d", len(samples))
	}
	if samples[0].Balance != 12345 {
		t.Errorf("balance = %d, want 12345", samples[0].Balance)
	}
	if samples[0].Reason != "WATCH" {
		t.Errorf("reason = %q, want WATCH", samples[0].Reason)
	}
}

// TestGetPointSamplesRange verifies inclusive time-range filtering and ordering.
func TestGetPointSamplesRange(t *testing.T) {
	repo := newTestRepo(t)
	const s = "range_streamer"
	now := time.Now()

	seedPoint(t, repo, s, now.Add(-10*24*time.Hour), 100, "WATCH") // outside 7d
	seedPoint(t, repo, s, now.Add(-5*24*time.Hour), 200, "WATCH")  // inside
	seedPoint(t, repo, s, now.Add(-1*24*time.Hour), 300, "CLAIM")  // inside

	start := now.Add(-7 * 24 * time.Hour)
	samples, err := repo.GetPointSamples(s, start, now, 0)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("want 2 in-range samples, got %d", len(samples))
	}
	// Oldest-first ordering.
	if samples[0].Balance != 200 || samples[1].Balance != 300 {
		t.Errorf("ordering wrong: %+v", samples)
	}
}

// TestGetPointSamplesLimit verifies the fetch cap bounds the row count.
func TestGetPointSamplesLimit(t *testing.T) {
	repo := newTestRepo(t)
	const s = "limit_streamer"
	base := time.Now().Add(-time.Hour)
	for i := 0; i < 10; i++ {
		seedPoint(t, repo, s, base.Add(time.Duration(i)*time.Minute), 1000+i, "WATCH")
	}
	samples, err := repo.GetPointSamples(s, time.Time{}, time.Time{}, 4)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(samples) != 4 {
		t.Fatalf("want 4 (limited), got %d", len(samples))
	}
}

// TestGetAnnotationRecords verifies annotations carry event_type and reason and
// fall back to the label text for legacy rows without a type.
func TestGetAnnotationRecords(t *testing.T) {
	repo := newTestRepo(t)
	const s = "ann_streamer"
	now := time.Now()

	seedAnnotation(t, repo, s, now.Add(-2*time.Hour), "WATCH_STREAK", "+450 - Watch Streak", "#8b7fd1")
	seedAnnotation(t, repo, s, now.Add(-1*time.Hour), "", "Legacy marker", "#ffffff") // pre-v3 row

	recs, err := repo.GetAnnotationRecords(s, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("want 2 annotations, got %d", len(recs))
	}
	if recs[0].Type != "WATCH_STREAK" || recs[0].Reason != "+450 - Watch Streak" {
		t.Errorf("first annotation = %+v", recs[0])
	}
	// Legacy row: Type falls back to the reason text.
	if recs[1].Type != "Legacy marker" {
		t.Errorf("legacy fallback type = %q, want %q", recs[1].Type, "Legacy marker")
	}
	// The persisted per-type colour is carried through to the DTO so the chart can
	// give each marker its own hue (regression: WATCH_STREAK once rendered in the
	// series colour because the colour never left the annotations table).
	if recs[0].Color != "#8b7fd1" {
		t.Errorf("first annotation colour = %q, want %q", recs[0].Color, "#8b7fd1")
	}
	if recs[1].Color != "#ffffff" {
		t.Errorf("legacy annotation colour = %q, want %q", recs[1].Color, "#ffffff")
	}
}

// TestGetAnnotationRecordsRangeAndTimestamp verifies annotation markers are
// filtered by the requested window and their timestamp round-trips exactly, so a
// watch streak lands on the right point of the chart: a 3-day-old streak (with
// its colour) is inside the 30d window but excluded by the 24h window.
// Deliberately uses no row older than 30d — PruneBefore counts stale rows across
// the shared package DB, so a >30d seed here would leak into TestPruneBefore.
func TestGetAnnotationRecordsRangeAndTimestamp(t *testing.T) {
	repo := newTestRepo(t)
	const s = "ann_range_streamer"
	now := time.Now()

	inWindow := now.Add(-3 * 24 * time.Hour) // inside 30d, outside 24h
	seedAnnotation(t, repo, s, inWindow, "WATCH_STREAK", "+450 - Watch Streak", "#45c1ff")

	recs, err := repo.GetAnnotationRecords(s, now.Add(-30*24*time.Hour), now)
	if err != nil {
		t.Fatalf("query 30d: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("30d window: want 1 annotation, got %d", len(recs))
	}
	if recs[0].T != inWindow.UnixMilli() {
		t.Errorf("timestamp = %d, want exact round-trip %d", recs[0].T, inWindow.UnixMilli())
	}
	if recs[0].Color != "#45c1ff" {
		t.Errorf("colour = %q, want #45c1ff", recs[0].Color)
	}

	recent, err := repo.GetAnnotationRecords(s, now.Add(-24*time.Hour), now)
	if err != nil {
		t.Fatalf("query 24h: %v", err)
	}
	if len(recent) != 0 {
		t.Errorf("24h window: want 0 annotations, got %d", len(recent))
	}
}

// TestPruneBefore verifies retention deletes rows older than the cutoff while
// keeping recent ones, across both points and annotations.
func TestPruneBefore(t *testing.T) {
	repo := newTestRepo(t)
	const s = "prune_streamer"
	now := time.Now()

	seedPoint(t, repo, s, now.Add(-40*24*time.Hour), 100, "WATCH") // stale
	seedPoint(t, repo, s, now.Add(-2*24*time.Hour), 200, "WATCH")  // fresh
	seedAnnotation(t, repo, s, now.Add(-40*24*time.Hour), "WIN", "old win", "#7fa88c")
	seedAnnotation(t, repo, s, now.Add(-2*24*time.Hour), "WIN", "new win", "#7fa88c")

	cutoff := now.Add(-30 * 24 * time.Hour)
	deleted, err := repo.PruneBefore(cutoff)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2 (1 point + 1 annotation)", deleted)
	}

	pts, _ := repo.GetPointSamples(s, time.Time{}, time.Time{}, 0)
	if len(pts) != 1 || pts[0].Balance != 200 {
		t.Errorf("remaining points = %+v, want single fresh row", pts)
	}
	anns, _ := repo.GetAnnotationRecords(s, time.Time{}, time.Time{})
	if len(anns) != 1 || anns[0].Reason != "new win" {
		t.Errorf("remaining annotations = %+v, want single fresh row", anns)
	}
}

// TestEmptyStreamerData verifies unknown streamers yield empty (not error) results.
func TestEmptyStreamerData(t *testing.T) {
	repo := newTestRepo(t)

	pts, err := repo.GetPointSamples("never_seen", time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("points: %v", err)
	}
	if len(pts) != 0 {
		t.Errorf("want no points, got %d", len(pts))
	}
	anns, err := repo.GetAnnotationRecords("never_seen", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("annotations: %v", err)
	}
	if len(anns) != 0 {
		t.Errorf("want no annotations, got %d", len(anns))
	}
}

// TestDownsample verifies uniform downsampling keeps the count within budget and
// preserves the first and last readings.
func TestDownsample(t *testing.T) {
	samples := make([]PointSample, 1000)
	for i := range samples {
		samples[i] = PointSample{T: int64(i), Balance: i}
	}

	out := Downsample(samples, 100)
	if len(out) != 100 {
		t.Fatalf("len = %d, want 100", len(out))
	}
	if out[0].Balance != 0 {
		t.Errorf("first = %d, want 0", out[0].Balance)
	}
	if out[len(out)-1].Balance != 999 {
		t.Errorf("last = %d, want 999", out[len(out)-1].Balance)
	}

	// Within-budget and disabled cases return the input unchanged.
	if got := Downsample(samples[:50], 100); len(got) != 50 {
		t.Errorf("small input len = %d, want 50", len(got))
	}
	if got := Downsample(samples, 0); len(got) != 1000 {
		t.Errorf("disabled len = %d, want 1000", len(got))
	}
}

// TestServiceMaybePruneThrottle verifies the retention sweep runs when due,
// respects the throttle interval, and is a no-op when retention is disabled.
func TestServiceMaybePruneThrottle(t *testing.T) {
	repo := newTestRepo(t)
	const s = "throttle_streamer"

	fixed := time.Now()
	svc := &Service{repo: repo, retentionDays: 30, now: func() time.Time { return fixed }}

	seedPoint(t, repo, s, fixed.Add(-40*24*time.Hour), 100, "WATCH") // stale
	seedPoint(t, repo, s, fixed.Add(-1*24*time.Hour), 200, "WATCH")  // fresh

	svc.maybePrune() // first call: due (lastPruneAt zero) -> prunes the stale row
	pts, _ := repo.GetPointSamples(s, time.Time{}, time.Time{}, 0)
	if len(pts) != 1 {
		t.Fatalf("after first prune: %d points, want 1", len(pts))
	}

	// Seed another stale row and call again within the throttle window: no prune.
	seedPoint(t, repo, s, fixed.Add(-50*24*time.Hour), 50, "WATCH")
	svc.maybePrune()
	pts, _ = repo.GetPointSamples(s, time.Time{}, time.Time{}, 0)
	if len(pts) != 2 {
		t.Fatalf("throttled call pruned unexpectedly: %d points, want 2", len(pts))
	}

	// Advance beyond the throttle interval: prune runs again.
	svc.now = func() time.Time { return fixed.Add(2 * pruneInterval) }
	svc.maybePrune()
	pts, _ = repo.GetPointSamples(s, time.Time{}, time.Time{}, 0)
	if len(pts) != 1 {
		t.Fatalf("after interval elapsed: %d points, want 1", len(pts))
	}

	// Disabled retention never prunes.
	seedPoint(t, repo, s, fixed.Add(-99*24*time.Hour), 10, "WATCH")
	disabled := &Service{repo: repo, retentionDays: 0, now: func() time.Time { return fixed }}
	disabled.maybePrune()
	pts, _ = repo.GetPointSamples(s, time.Time{}, time.Time{}, 0)
	if len(pts) != 2 {
		t.Fatalf("disabled retention pruned: %d points, want 2", len(pts))
	}
}
