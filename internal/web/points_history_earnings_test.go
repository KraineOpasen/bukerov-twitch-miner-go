package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// recordExact writes one exact point event for a streamer through the
// analytics service — the production write path of an accepted points-earned
// frame — at an explicit timestamp.
func recordExact(t *testing.T, srv *Server, streamer, id, reason string, amount, balance int, ts time.Time) {
	t.Helper()
	st := models.NewStreamer(streamer, models.DefaultStreamerSettings())
	rec, err := srv.analytics.RecordPointEvent(st, analytics.PointEvent{
		EventID:      id,
		Timestamp:    ts.UnixMilli(),
		ReasonCode:   reason,
		TotalPoints:  amount,
		BalanceAfter: balance,
		BalanceKnown: true,
	})
	if err != nil || !rec {
		t.Fatalf("RecordPointEvent %s: recorded=%v err=%v", id, rec, err)
	}
}

func fetchHistory(t *testing.T, srv *Server, path string) analytics.PointsHistory {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
	}
	var got analytics.PointsHistory
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	return got
}

// TestPointsHistoryLegacyOnlyRangeIsAnEstimate: history recorded before the
// exact ledger is still served, but labelled: coverage "legacy", exact=false,
// the breakdown IS the balance-delta estimate, no exactSince.
func TestPointsHistoryLegacyOnlyRangeIsAnEstimate(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	const s = "earn_legacy_only"
	for _, p := range []struct {
		balance int
		reason  string
	}{{11310, "WATCH"}, {11772, "WATCH STREAK"}, {11784, "WATCH"}} {
		if err := repo.RecordPoints(s, p.balance, p.reason); err != nil {
			t.Fatal(err)
		}
	}

	got := fetchHistory(t, srv, "/api/points-history?streamer="+s+"&range=7d")
	want := analytics.EarningsAccounting{Coverage: analytics.EarningsCoverageLegacy, Exact: false, LegacyStatus: analytics.LegacyStatusEstimated}
	if got.Earnings != want {
		t.Fatalf("earnings = %+v, want %+v", got.Earnings, want)
	}
	wantShares := []analytics.ReasonShare{{Reason: "WATCH_STREAK", Gained: 462, Count: 1}, {Reason: "WATCH", Gained: 12, Count: 1}}
	if len(got.Breakdown) != 2 || got.Breakdown[0] != wantShares[0] || got.Breakdown[1] != wantShares[1] {
		t.Fatalf("breakdown = %+v, want the legacy estimate %+v", got.Breakdown, wantShares)
	}
	if len(got.LegacyBreakdown) != 2 || got.LegacyBreakdown[0] != wantShares[0] {
		t.Fatalf("legacyBreakdown = %+v, want the same estimate", got.LegacyBreakdown)
	}
	for _, p := range got.Points {
		if p.Exact {
			t.Fatalf("legacy sample flagged exact: %+v", p)
		}
	}
}

// TestPointsHistoryExactOnlyRange: a range fully covered by ledger events
// reports the exact aggregation, the coverage boundary, flagged samples and
// no legacy part.
func TestPointsHistoryExactOnlyRange(t *testing.T) {
	srv := newStatsTestServer(t)
	const s = "earn_exact_only"
	first := time.Now().Add(-3 * time.Hour)
	recordExact(t, srv, s, "sha256:exact-only-1", "WATCH_STREAK", 450, 11772, first)
	recordExact(t, srv, s, "sha256:exact-only-2", "WATCH", 12, 11784, first.Add(time.Minute))
	recordExact(t, srv, s, "sha256:exact-only-3", "WATCH_STREAK", 450, 12234, first.Add(2*time.Minute))

	got := fetchHistory(t, srv, "/api/points-history?streamer="+s+"&range=24h")
	want := analytics.EarningsAccounting{Coverage: analytics.EarningsCoverageExact, Exact: true, ExactSince: first.UnixMilli(), LegacyStatus: analytics.LegacyStatusNone}
	if got.Earnings != want {
		t.Fatalf("earnings = %+v, want %+v", got.Earnings, want)
	}
	wantShares := []analytics.ReasonShare{{Reason: "WATCH_STREAK", Gained: 900, Count: 2}, {Reason: "WATCH", Gained: 12, Count: 1}}
	if len(got.Breakdown) != 2 || got.Breakdown[0] != wantShares[0] || got.Breakdown[1] != wantShares[1] {
		t.Fatalf("breakdown = %+v, want %+v", got.Breakdown, wantShares)
	}
	if got.LegacyBreakdown != nil {
		t.Fatalf("legacyBreakdown = %+v, want none", got.LegacyBreakdown)
	}
	if len(got.Points) != 3 {
		t.Fatalf("points = %+v, want 3", got.Points)
	}
	for _, p := range got.Points {
		if !p.Exact {
			t.Fatalf("exact sample not flagged: %+v", p)
		}
	}
}

// TestPointsHistoryTruncationKeepsExactAggregate: when the raw balance series
// hits the row cap, the legacy estimate is reported unavailable (never zero)
// while the exact aggregation — computed in SQL, not from the truncated
// series — stays complete and visible.
func TestPointsHistoryTruncationKeepsExactAggregate(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	const s = "earn_truncated"
	prev := historyRowCap
	historyRowCap = 3
	t.Cleanup(func() { historyRowCap = prev })

	// Five legacy samples first (older), then two exact events (newer): the
	// ascending fetch with a cap of 3 never reaches the exact rows' samples.
	for _, balance := range []int{1000, 1010, 1020, 1030, 1040} {
		if err := repo.RecordPoints(s, balance, "WATCH"); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now()
	recordExact(t, srv, s, "sha256:trunc-1", "WATCH_STREAK", 450, 1490, base.Add(time.Millisecond))
	recordExact(t, srv, s, "sha256:trunc-2", "CLAIM", 50, 1540, base.Add(2*time.Millisecond))
	time.Sleep(10 * time.Millisecond) // the request window ends at its own time.Now()

	got := fetchHistory(t, srv, "/api/points-history?streamer="+s+"&range=24h")
	if !got.RawTruncated || len(got.Points) != 3 {
		t.Fatalf("rawTruncated=%v points=%d, want truncated series of 3", got.RawTruncated, len(got.Points))
	}
	if got.Earnings.LegacyStatus != analytics.LegacyStatusUnavailable || got.Earnings.Coverage != analytics.EarningsCoverageMixed || !got.Earnings.Exact {
		t.Fatalf("earnings = %+v, want exact=true, mixed coverage, legacy unavailable", got.Earnings)
	}
	wantShares := []analytics.ReasonShare{{Reason: "WATCH_STREAK", Gained: 450, Count: 1}, {Reason: "CLAIM", Gained: 50, Count: 1}}
	if len(got.Breakdown) != 2 || got.Breakdown[0] != wantShares[0] || got.Breakdown[1] != wantShares[1] {
		t.Fatalf("exact breakdown under truncation = %+v, want %+v (truncation must not hide the exact aggregate)", got.Breakdown, wantShares)
	}
	if got.LegacyBreakdown != nil {
		t.Fatalf("legacyBreakdown = %+v, want none (unavailable, not zero)", got.LegacyBreakdown)
	}

	// Without any exact event a truncated range is "unavailable" — never a
	// silent zero and never a PASS.
	const s2 = "earn_truncated_legacy"
	for _, balance := range []int{1000, 1010, 1020, 1030} {
		if err := repo.RecordPoints(s2, balance, "WATCH"); err != nil {
			t.Fatal(err)
		}
	}
	got2 := fetchHistory(t, srv, "/api/points-history?streamer="+s2+"&range=24h")
	if !got2.RawTruncated || got2.Earnings.Coverage != analytics.EarningsCoverageUnavailable || got2.Breakdown != nil || got2.Earnings.Exact {
		t.Fatalf("truncated legacy-only response = earnings %+v breakdown %+v, want unavailable with no breakdown", got2.Earnings, got2.Breakdown)
	}
}

// TestPointsHistoryExportCarriesExactFlags: the full-fidelity export marks
// which samples are exact-backed, so an external tool can tell exact history
// from legacy history without re-deriving it.
func TestPointsHistoryExportCarriesExactFlags(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	const s = "earn_export"
	if err := repo.RecordPoints(s, 1000, "WATCH"); err != nil {
		t.Fatal(err)
	}
	recordExact(t, srv, s, "sha256:export-1", "WATCH", 12, 1012, time.Now())

	got := fetchHistory(t, srv, "/api/points-history/export?streamer="+s+"&range=24h")
	if len(got.Points) != 2 || got.Points[0].Exact || !got.Points[1].Exact {
		t.Fatalf("export points = %+v, want [legacy, exact]", got.Points)
	}
}
