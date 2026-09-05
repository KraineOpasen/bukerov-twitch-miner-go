package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// earningsTestSeq keeps streamer logins and event identities unique for the
// life of the process (the package shares one database singleton), so these
// tests stay hermetic under `go test -count=N`.
var earningsTestSeq atomic.Uint64

func uniqueLogin(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, earningsTestSeq.Add(1))
}

// recordExact writes one exact point event for a streamer through the
// analytics service — the production write path of an accepted points-earned
// frame — at an explicit timestamp. The event identity is derived from the
// streamer login and the suffix, so it is unique per test run.
func recordExact(t *testing.T, srv *Server, streamer, suffix, reason string, amount, balance int, ts time.Time) {
	t.Helper()
	st := models.NewStreamer(streamer, models.DefaultStreamerSettings())
	rec, err := srv.analytics.RecordPointEvent(st, analytics.PointEvent{
		EventID:      "sha256:" + streamer + "-" + suffix,
		Timestamp:    ts.UnixMilli(),
		ReasonCode:   reason,
		TotalPoints:  amount,
		BalanceAfter: balance,
		BalanceKnown: true,
	})
	if err != nil || !rec {
		t.Fatalf("RecordPointEvent %s/%s: recorded=%v err=%v", streamer, suffix, rec, err)
	}
}

func fetchRaw(t *testing.T, srv *Server, path string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status = %d, want 200; body=%s", path, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}

func fetchHistory(t *testing.T, srv *Server, path string) analytics.PointsHistory {
	t.Helper()
	body := fetchRaw(t, srv, path)
	var got analytics.PointsHistory
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, body)
	}
	return got
}

// TestPointsHistoryWindowBoundsExactAggregate: both endpoints hand the
// selected range to the ledger aggregation — an exact event older than the
// preset is absent from the breakdown and from exactSince, while an in-range
// one is present.
func TestPointsHistoryWindowBoundsExactAggregate(t *testing.T) {
	srv := newStatsTestServer(t)
	s := uniqueLogin("earn_window")
	old := time.Now().Add(-25 * time.Hour)
	recent := time.Now().Add(-time.Hour)
	recordExact(t, srv, s, "old", "RAID", 250, 1250, old)
	recordExact(t, srv, s, "recent", "WATCH_STREAK", 450, 1700, recent)

	for _, path := range []string{"/api/points-history?streamer=" + s + "&range=24h", "/api/points-history/export?streamer=" + s + "&range=24h"} {
		got := fetchHistory(t, srv, path)
		want := []analytics.ReasonShare{{Reason: "WATCH_STREAK", Gained: 450, Count: 1}}
		if len(got.ExactBreakdown) != 1 || got.ExactBreakdown[0] != want[0] {
			t.Fatalf("%s: exactBreakdown = %+v, want only the in-range event %+v", path, got.ExactBreakdown, want)
		}
		// The compatibility breakdown has nothing to attribute from a single
		// in-range sample (first sample baseline).
		if got.Breakdown != nil {
			t.Fatalf("%s: breakdown = %+v, want none from one sample", path, got.Breakdown)
		}
		if got.Earnings.ExactSince != recent.UnixMilli() || got.Earnings.Coverage != analytics.EarningsCoverageExact {
			t.Fatalf("%s: earnings = %+v, want exact coverage since the in-range event", path, got.Earnings)
		}
		if len(got.Points) != 1 {
			t.Fatalf("%s: points = %+v, want only the in-range sample", path, got.Points)
		}
	}
	// A wider preset includes both.
	if got := fetchHistory(t, srv, "/api/points-history?streamer="+s+"&range=7d"); len(got.ExactBreakdown) != 2 || got.Earnings.ExactSince != old.UnixMilli() {
		t.Fatalf("7d: exactBreakdown = %+v since %d, want both events since the older one", got.ExactBreakdown, got.Earnings.ExactSince)
	}
}

// TestPointsHistoryWireKeysArePinned pins the public JSON names the dashboard
// reads, as raw bytes: a renamed or retyped struct tag would still decode
// into the shared Go struct, so only the wire form proves the additive
// contract (no existing key renamed; new keys exactly as documented).
func TestPointsHistoryWireKeysArePinned(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	s := uniqueLogin("earn_wire")
	if err := repo.RecordPoints(s, 1000, "WATCH"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordPoints(s, 1050, "CLAIM"); err != nil {
		t.Fatal(err)
	}
	exactAt := time.Now()
	recordExact(t, srv, s, "1", "WATCH", 12, 1062, exactAt)

	for _, path := range []string{"/api/points-history?streamer=" + s + "&range=24h", "/api/points-history/export?streamer=" + s + "&range=24h"} {
		body := string(fetchRaw(t, srv, path))
		for _, want := range []string{
			`"streamer":"` + s + `"`, `"range":"24h"`, `"points":[`,
			// breakdown: the first release's attribution over the whole raw
			// timeline (+50 CLAIM, +12 WATCH); exactBreakdown: the ledger;
			// legacyBreakdown: the uncovered estimate.
			`"breakdown":[{"reason":"CLAIM","gained":50,"count":1},{"reason":"WATCH","gained":12,"count":1}]`,
			`"exactBreakdown":[{"reason":"WATCH","gained":12,"count":1}]`,
			`"legacyBreakdown":[{"reason":"CLAIM","gained":50,"count":1}]`,
			`"earnings":{"coverage":"mixed","exact":true,"exactSince":` + fmt.Sprint(exactAt.UnixMilli()) + `,"legacyStatus":"estimated"}`,
			`"balance":1062,"reason":"WATCH","exact":true}`, `"rawTruncated":false`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: wire body lacks %s\nbody=%s", path, want, body)
			}
		}
		// A legacy sample carries no exact key at all (omitempty), so old
		// consumers see byte-identical sample objects.
		if !strings.Contains(body, `"balance":1000,"reason":"WATCH"}`) {
			t.Errorf("%s: legacy sample gained a key: %s", path, body)
		}
	}
}

// TestPointsHistoryLegacyOnlyRangeIsAnEstimate: history recorded before the
// exact ledger is still served, but labelled: coverage "legacy", exact=false,
// no exactBreakdown, the explicit estimate in legacyBreakdown (and, for this
// spend-free timeline, the same figures under the compatibility breakdown),
// no exactSince.
func TestPointsHistoryLegacyOnlyRangeIsAnEstimate(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	s := uniqueLogin("earn_legacy_only")
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
	if !sharesEqual(got.LegacyBreakdown, wantShares) {
		t.Fatalf("legacyBreakdown = %+v, want the explicit estimate %+v for a legacy-only range", got.LegacyBreakdown, wantShares)
	}
	if got.ExactBreakdown != nil {
		t.Fatalf("exactBreakdown = %+v, want none without exact events", got.ExactBreakdown)
	}
	if !sharesEqual(got.Breakdown, wantShares) {
		t.Fatalf("breakdown = %+v, want the compatibility attribution %+v (no spend, so it coincides with the estimate)", got.Breakdown, wantShares)
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
	s := uniqueLogin("earn_exact_only")
	first := time.Now().Add(-3 * time.Hour)
	recordExact(t, srv, s, "1", "WATCH_STREAK", 450, 11772, first)
	recordExact(t, srv, s, "2", "WATCH", 12, 11784, first.Add(time.Minute))
	recordExact(t, srv, s, "3", "WATCH_STREAK", 450, 12234, first.Add(2*time.Minute))

	got := fetchHistory(t, srv, "/api/points-history?streamer="+s+"&range=24h")
	want := analytics.EarningsAccounting{Coverage: analytics.EarningsCoverageExact, Exact: true, ExactSince: first.UnixMilli(), LegacyStatus: analytics.LegacyStatusNone}
	if got.Earnings != want {
		t.Fatalf("earnings = %+v, want %+v", got.Earnings, want)
	}
	wantShares := []analytics.ReasonShare{{Reason: "WATCH_STREAK", Gained: 900, Count: 2}, {Reason: "WATCH", Gained: 12, Count: 1}}
	if !sharesEqual(got.ExactBreakdown, wantShares) {
		t.Fatalf("exactBreakdown = %+v, want %+v", got.ExactBreakdown, wantShares)
	}
	if got.LegacyBreakdown != nil {
		t.Fatalf("legacyBreakdown = %+v, want none", got.LegacyBreakdown)
	}
	// The compatibility breakdown attributes the two deltas AFTER the first
	// (baseline) exact sample: +12 WATCH, +450 WATCH STREAK — the first grant
	// is invisible to it, which is exactly why it is not accounting.
	wantCompat := []analytics.ReasonShare{{Reason: "WATCH_STREAK", Gained: 450, Count: 1}, {Reason: "WATCH", Gained: 12, Count: 1}}
	if !sharesEqual(got.Breakdown, wantCompat) {
		t.Fatalf("breakdown = %+v, want the compatibility attribution %+v", got.Breakdown, wantCompat)
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
	srv.historyRowCap = 3
	repo := srv.analytics.Repository()
	s := uniqueLogin("earn_truncated")

	// Five legacy samples first (older), then two exact events (newer): the
	// ascending fetch with a cap of 3 never reaches the exact rows' samples.
	for _, balance := range []int{1000, 1010, 1020, 1030, 1040} {
		if err := repo.RecordPoints(s, balance, "WATCH"); err != nil {
			t.Fatal(err)
		}
	}
	// Both exact events are stamped at base; the request takes its own
	// time.Now() later on the same wall clock and the SQL window end is
	// inclusive (timestamp <= end.UnixMilli()), so base is inside the window
	// and the request needs no wait. At an equal millisecond the exact
	// samples still sort after the legacy rows: the read orders by
	// (timestamp, id) and they were inserted later.
	base := time.Now()
	recordExact(t, srv, s, "1", "WATCH_STREAK", 450, 1490, base)
	recordExact(t, srv, s, "2", "CLAIM", 50, 1540, base)

	got := fetchHistory(t, srv, "/api/points-history?streamer="+s+"&range=24h")
	if !got.RawTruncated || len(got.Points) != 3 {
		t.Fatalf("rawTruncated=%v points=%d, want truncated series of 3", got.RawTruncated, len(got.Points))
	}
	if got.Earnings.LegacyStatus != analytics.LegacyStatusUnavailable || got.Earnings.Coverage != analytics.EarningsCoverageMixed || !got.Earnings.Exact {
		t.Fatalf("earnings = %+v, want exact=true, mixed coverage, legacy unavailable", got.Earnings)
	}
	wantShares := []analytics.ReasonShare{{Reason: "WATCH_STREAK", Gained: 450, Count: 1}, {Reason: "CLAIM", Gained: 50, Count: 1}}
	if !sharesEqual(got.ExactBreakdown, wantShares) {
		t.Fatalf("exactBreakdown under truncation = %+v, want %+v (truncation must not hide the exact aggregate)", got.ExactBreakdown, wantShares)
	}
	if got.LegacyBreakdown != nil {
		t.Fatalf("legacyBreakdown = %+v, want none (unavailable, not zero)", got.LegacyBreakdown)
	}
	// The compatibility breakdown is computed over the truncated raw series
	// exactly as the first release did (1000, 1010, 1020 → two +10 WATCH
	// deltas); rawTruncated is the consumer's signal that it is incomplete.
	wantCompat := []analytics.ReasonShare{{Reason: "WATCH", Gained: 20, Count: 2}}
	if !sharesEqual(got.Breakdown, wantCompat) {
		t.Fatalf("breakdown under truncation = %+v, want the compatibility attribution over the capped series %+v", got.Breakdown, wantCompat)
	}

	// Without any exact event a truncated range is "unavailable" — never a
	// silent zero and never a PASS.
	s2 := uniqueLogin("earn_truncated_legacy")
	for _, balance := range []int{1000, 1010, 1020, 1030} {
		if err := repo.RecordPoints(s2, balance, "WATCH"); err != nil {
			t.Fatal(err)
		}
	}
	got2 := fetchHistory(t, srv, "/api/points-history?streamer="+s2+"&range=24h")
	if !got2.RawTruncated || got2.Earnings.Coverage != analytics.EarningsCoverageUnavailable || got2.ExactBreakdown != nil || got2.LegacyBreakdown != nil || got2.Earnings.Exact {
		t.Fatalf("truncated legacy-only response = earnings %+v exact %+v legacy %+v, want unavailable with no accounting", got2.Earnings, got2.ExactBreakdown, got2.LegacyBreakdown)
	}
	// ...while the compatibility breakdown still reports what the first
	// release reported over the capped series (1000, 1010, 1020: two +10
	// WATCH deltas; the fourth row is beyond the cap).
	if !sharesEqual(got2.Breakdown, []analytics.ReasonShare{{Reason: "WATCH", Gained: 20, Count: 2}}) {
		t.Fatalf("truncated legacy-only breakdown = %+v, want the compatibility attribution over the capped series", got2.Breakdown)
	}

	// The export applies its own cap with the same accounting rules.
	srv.exportRowCap = 3
	exp := fetchHistory(t, srv, "/api/points-history/export?streamer="+s+"&range=24h")
	if !exp.RawTruncated || len(exp.Points) != 3 || exp.Earnings.Coverage != analytics.EarningsCoverageMixed || exp.Earnings.LegacyStatus != analytics.LegacyStatusUnavailable {
		t.Fatalf("truncated export = rawTruncated %v, %d points, earnings %+v; want 3 points, mixed, legacy unavailable", exp.RawTruncated, len(exp.Points), exp.Earnings)
	}
	if !sharesEqual(exp.ExactBreakdown, wantShares) || exp.LegacyBreakdown != nil || !sharesEqual(exp.Breakdown, wantCompat) {
		t.Fatalf("truncated export exact %+v legacy %+v breakdown %+v, want the complete exact aggregate, no legacy estimate and the compatibility attribution over the capped series", exp.ExactBreakdown, exp.LegacyBreakdown, exp.Breakdown)
	}

	// A zero cap (a Server built without its constructor) means the
	// production cap, never "always truncated".
	srv.historyRowCap, srv.exportRowCap = 0, 0
	for _, path := range []string{"/api/points-history?streamer=" + s + "&range=24h", "/api/points-history/export?streamer=" + s + "&range=24h"} {
		if got := fetchHistory(t, srv, path); got.RawTruncated || len(got.Points) != 7 || got.Earnings.LegacyStatus != analytics.LegacyStatusEstimated {
			t.Fatalf("%s with zero cap: rawTruncated=%v points=%d earnings=%+v, want the complete series", path, got.RawTruncated, len(got.Points), got.Earnings)
		}
	}
}

// TestPointsHistoryExportCarriesExactFlagsAndAccounting: the full-fidelity
// export marks which samples are exact-backed and carries the same earnings
// accounting as the history endpoint, so an external tool never sees an
// empty, undocumented accounting block.
func TestPointsHistoryExportCarriesExactFlagsAndAccounting(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	s := uniqueLogin("earn_export")
	if err := repo.RecordPoints(s, 1000, "WATCH"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordPoints(s, 1050, "CLAIM"); err != nil {
		t.Fatal(err)
	}
	exactAt := time.Now()
	recordExact(t, srv, s, "1", "WATCH", 12, 1062, exactAt)

	got := fetchHistory(t, srv, "/api/points-history/export?streamer="+s+"&range=24h")
	if len(got.Points) != 3 || got.Points[0].Exact || got.Points[1].Exact || !got.Points[2].Exact {
		t.Fatalf("export points = %+v, want [legacy, legacy, exact]", got.Points)
	}
	want := analytics.EarningsAccounting{Coverage: analytics.EarningsCoverageMixed, Exact: true, ExactSince: exactAt.UnixMilli(), LegacyStatus: analytics.LegacyStatusEstimated}
	if got.Earnings != want {
		t.Fatalf("export earnings = %+v, want %+v", got.Earnings, want)
	}
	if !sharesEqual(got.ExactBreakdown, []analytics.ReasonShare{{Reason: "WATCH", Gained: 12, Count: 1}}) {
		t.Fatalf("export exactBreakdown = %+v, want the exact WATCH event only", got.ExactBreakdown)
	}
	if !sharesEqual(got.LegacyBreakdown, []analytics.ReasonShare{{Reason: "CLAIM", Gained: 50, Count: 1}}) {
		t.Fatalf("export legacyBreakdown = %+v, want the legacy CLAIM estimate reported separately", got.LegacyBreakdown)
	}
	if !sharesEqual(got.Breakdown, []analytics.ReasonShare{{Reason: "CLAIM", Gained: 50, Count: 1}, {Reason: "WATCH", Gained: 12, Count: 1}}) {
		t.Fatalf("export breakdown = %+v, want the compatibility attribution over the whole exported series", got.Breakdown)
	}
}
