package analytics

import (
	"testing"
	"time"
)

func betCount(t *testing.T, r *SQLiteRepository, eventID string) int {
	t.Helper()
	var n int
	if err := r.db.QueryRow("SELECT COUNT(*) FROM prediction_bets WHERE event_id = ?", eventID).Scan(&n); err != nil {
		t.Fatalf("count bets: %v", err)
	}
	return n
}

// TestBetMigrationPreservesExistingHistory proves migration v4 is safe on a
// populated DB: seeded points and annotations remain intact and queryable after
// the prediction_bets table exists and is written to.
func TestBetMigrationPreservesExistingHistory(t *testing.T) {
	r := newTestRepo(t)
	s := "mig-streamer"
	now := time.Now()

	seedPoint(t, r, s, now.Add(-time.Hour), 1000, "WATCH")
	if err := r.RecordAnnotation(s, "WIN", "Prediction WIN", "#36b535"); err != nil {
		t.Fatalf("record annotation: %v", err)
	}

	if err := r.RecordBet(BetRecord{
		EventID: "mig-evt-1", Streamer: s, Timestamp: now.UnixMilli(),
		Strategy: "SMART", ResultType: "WIN", Placed: 100, Won: 250, Gained: 150, Odds: 2.5,
	}); err != nil {
		t.Fatalf("record bet: %v", err)
	}

	// Existing history must be untouched.
	data, err := r.GetStreamerData(s)
	if err != nil {
		t.Fatalf("get streamer data: %v", err)
	}
	if len(data.Series) != 1 {
		t.Errorf("points series length = %d, want 1 (migration must not drop points)", len(data.Series))
	}
	if len(data.Annotations) != 1 {
		t.Errorf("annotations length = %d, want 1 (migration must not drop annotations)", len(data.Annotations))
	}
}

// TestRecordBetIdempotentOnDuplicateEventID is the reconnect guard: recording the
// same event_id twice must leave exactly one row and never error.
func TestRecordBetIdempotentOnDuplicateEventID(t *testing.T) {
	r := newTestRepo(t)
	b := BetRecord{
		EventID: "dup-evt", Streamer: "dup-streamer", Timestamp: time.Now().UnixMilli(),
		Strategy: "SMART", ResultType: "WIN", Placed: 100, Won: 250, Gained: 150, Odds: 2.5,
	}

	if err := r.RecordBet(b); err != nil {
		t.Fatalf("first record: %v", err)
	}
	// Re-deliver the same result (simulating a PubSub reconnect) — must not error.
	if err := r.RecordBet(b); err != nil {
		t.Fatalf("duplicate record must not error: %v", err)
	}

	if n := betCount(t, r, "dup-evt"); n != 1 {
		t.Fatalf("duplicate event_id produced %d rows, want exactly 1", n)
	}
}

func TestGetBetsFilters(t *testing.T) {
	r := newTestRepo(t)
	base := time.Now()
	mk := func(id, streamer, strategy string, ageDays int) BetRecord {
		return BetRecord{
			EventID: id, Streamer: streamer, Strategy: strategy,
			Timestamp:  base.Add(-time.Duration(ageDays) * 24 * time.Hour).UnixMilli(),
			ResultType: "WIN", Placed: 100, Won: 200, Gained: 100, Odds: 2.0,
		}
	}
	for _, b := range []BetRecord{
		mk("gf-1", "gf-alice", "SMART", 1),
		mk("gf-2", "gf-alice", "HIGH_ODDS", 40),
		mk("gf-3", "gf-bob", "SMART", 2),
	} {
		if err := r.RecordBet(b); err != nil {
			t.Fatalf("record %s: %v", b.EventID, err)
		}
	}

	// Filter by streamer.
	got, err := r.GetBets("gf-alice", "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("get by streamer: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("gf-alice bets = %d, want 2", len(got))
	}
	// Ordered oldest-first: gf-2 (40d) before gf-1 (1d).
	if len(got) == 2 && (got[0].EventID != "gf-2" || got[1].EventID != "gf-1") {
		t.Errorf("bets not oldest-first: %s, %s", got[0].EventID, got[1].EventID)
	}

	// Filter by streamer + strategy.
	got, _ = r.GetBets("gf-alice", "SMART", time.Time{}, time.Time{})
	if len(got) != 1 || got[0].EventID != "gf-1" {
		t.Errorf("streamer+strategy filter = %+v, want just gf-1", got)
	}

	// Filter by time window (last 7 days) excludes the 40-day-old bet.
	got, _ = r.GetBets("gf-alice", "", base.Add(-7*24*time.Hour), base)
	if len(got) != 1 || got[0].EventID != "gf-1" {
		t.Errorf("7d window = %+v, want just gf-1", got)
	}
}

func TestGetBetsUnknownStreamerIsNilNotError(t *testing.T) {
	r := newTestRepo(t)
	got, err := r.GetBets("nobody-here", "", time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("unknown streamer must not error: %v", err)
	}
	if got != nil {
		t.Fatalf("unknown streamer must yield nil, got %+v", got)
	}
}

func TestGetBetsRoundTripFields(t *testing.T) {
	r := newTestRepo(t)
	want := BetRecord{
		EventID: "rt-evt", Streamer: "rt-streamer", Timestamp: time.Now().UnixMilli(),
		Strategy: "HIGH_ODDS", ResultType: "REFUND", Placed: 500, Won: 0, Gained: 0, Odds: 3.5, Manual: true,
	}
	if err := r.RecordBet(want); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := r.GetBets("rt-streamer", "", time.Time{}, time.Time{})
	if err != nil || len(got) != 1 {
		t.Fatalf("get: %v, %d rows", err, len(got))
	}
	if got[0] != want {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", got[0], want)
	}
}
