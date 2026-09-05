package web

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// compatAccounting is the accounting subset of a points-history response as
// an A+ consumer reads it: the compatibility `breakdown`, the authoritative
// `exactBreakdown`, the explicit `legacyBreakdown` estimate and the coverage
// metadata. Decoded from the wire, never from analytics.PointsHistory, so the
// pin holds whatever the Go type looks like.
type compatAccounting struct {
	Breakdown       []analytics.ReasonShare      `json:"breakdown"`
	ExactBreakdown  []analytics.ReasonShare      `json:"exactBreakdown"`
	LegacyBreakdown []analytics.ReasonShare      `json:"legacyBreakdown"`
	Earnings        analytics.EarningsAccounting `json:"earnings"`
	RawTruncated    bool                         `json:"rawTruncated"`
}

// baseShapedHistory is the points-history response exactly as a consumer
// built against the base commit dc5566049f1de1909d66c0f190338d54af863402
// decodes it: the pre-ledger analytics.PointsHistory and PointSample field
// sets, copied verbatim (names, types and tags), nothing newer. Such a
// consumer decodes with encoding/json's default leniency, so fields it does
// not know (earnings, legacyBreakdown, exactBreakdown, points[].exact) are
// ignored, and what it reads under `breakdown` must mean what it meant then.
type baseShapedHistory struct {
	Streamer         string                 `json:"streamer"`
	Range            string                 `json:"range"`
	Points           []baseShapedSample     `json:"points"`
	Annotations      []baseShapedAnnotation `json:"annotations"`
	Breakdown        []baseShapedShare      `json:"breakdown,omitempty"`
	BetSummary       json.RawMessage        `json:"betSummary,omitempty"`
	RawTruncated     bool                   `json:"rawTruncated"`
	ChartDownsampled bool                   `json:"chartDownsampled"`
}

type baseShapedSample struct {
	T       int64  `json:"t"`
	Balance int    `json:"balance"`
	Reason  string `json:"reason,omitempty"`
}

type baseShapedAnnotation struct {
	T      int64  `json:"t"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
	Color  string `json:"color"`
}

type baseShapedShare struct {
	Reason string `json:"reason"`
	Gained int    `json:"gained"`
	Count  int    `json:"count"`
}

// baseShapedEqual compares what a base-shaped consumer decoded with a
// hand-worked base figure, field by field.
func baseShapedEqual(got []baseShapedShare, want []analytics.ReasonShare) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Reason != want[i].Reason || got[i].Gained != want[i].Gained || got[i].Count != want[i].Count {
			return false
		}
	}
	return true
}

// seedMixedWindow writes the raw timeline below for a fresh streamer, in this
// order, every sample stamped at or after the previous one (same-millisecond
// ties order by id): two legacy samples, two exact events, a spend and a
// trailing legacy sample. The streak grant's balance jumps by 462 for an
// event-local amount of 450 (the production stale-balance shape), so the
// base figure is NOT the sum of the exact and the legacy lists: an
// implementation that summed them could not pass. The expected figures are
// worked by hand from the base algorithm — consecutive positive balance
// deltas attributed to the later sample's canonical reason, first sample
// baseline — and from the ledger, never recomputed with production code.
//
//	#  balance  reason        exact (amount)  base delta
//	1  1000     WATCH         no              baseline
//	2  1012     WATCH         no              +12  WATCH
//	3  1474     WATCH STREAK  yes (450)       +462 WATCH_STREAK
//	4  1486     WATCH         yes (12)        +12  WATCH
//	5  1436     Spent         no              -50  ignored
//	6  1448     WATCH         no              +12  WATCH
func seedMixedWindow(t *testing.T, srv *Server, streamer string) {
	t.Helper()
	repo := srv.analytics.Repository()
	if err := repo.RecordPoints(streamer, 1000, "WATCH"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordPoints(streamer, 1012, "WATCH"); err != nil {
		t.Fatal(err)
	}
	recordExact(t, srv, streamer, "streak", "WATCH_STREAK", 450, 1474, time.Now())
	recordExact(t, srv, streamer, "watch", "WATCH", 12, 1486, time.Now())
	if err := repo.RecordPoints(streamer, 1436, "Spent"); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordPoints(streamer, 1448, "WATCH"); err != nil {
		t.Fatal(err)
	}
}

var (
	// mixedWindowBaseBreakdown is what the base commit reports as `breakdown`
	// for the seeded timeline: every positive delta, exact-backed or not,
	// spent-adjacent or not — 462 for the streak, not its 450 amount.
	mixedWindowBaseBreakdown = []analytics.ReasonShare{{Reason: "WATCH_STREAK", Gained: 462, Count: 1}, {Reason: "WATCH", Gained: 36, Count: 3}}
	// mixedWindowExact is the ledger: the two event-local amounts only.
	mixedWindowExact = []analytics.ReasonShare{{Reason: "WATCH_STREAK", Gained: 450, Count: 1}, {Reason: "WATCH", Gained: 12, Count: 1}}
	// mixedWindowLegacy is the estimate over the samples no event backs:
	// samples 2 and 6 (+12 each); the exact samples and the spend are not
	// estimated.
	mixedWindowLegacy = []analytics.ReasonShare{{Reason: "WATCH", Gained: 24, Count: 2}}
)

func sharesEqual(a, b []analytics.ReasonShare) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestPointsHistoryBreakdownKeepsBaseSemanticsInMixedWindow pins the Copilot
// compatibility finding (owner decision A+): in a window holding both exact
// events and legacy samples, the public `breakdown` must still mean what it
// meant at the base commit — the balance-delta attribution over the whole
// raw timeline — while the authoritative ledger accounting is published as
// `exactBreakdown` and the estimate for the uncovered part as
// `legacyBreakdown`; the three are never summed, and the two endpoints agree.
func TestPointsHistoryBreakdownKeepsBaseSemanticsInMixedWindow(t *testing.T) {
	srv := newStatsTestServer(t)
	s := uniqueLogin("compat_mixed")
	seedMixedWindow(t, srv, s)

	for _, path := range []string{"/api/points-history?streamer=" + s + "&range=24h", "/api/points-history/export?streamer=" + s + "&range=24h"} {
		var got compatAccounting
		if err := json.Unmarshal(fetchRaw(t, srv, path), &got); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		if !sharesEqual(got.Breakdown, mixedWindowBaseBreakdown) {
			t.Errorf("%s: breakdown = %+v, want the base-commit attribution %+v", path, got.Breakdown, mixedWindowBaseBreakdown)
		}
		if !sharesEqual(got.ExactBreakdown, mixedWindowExact) {
			t.Errorf("%s: exactBreakdown = %+v, want the ledger only %+v", path, got.ExactBreakdown, mixedWindowExact)
		}
		if !sharesEqual(got.LegacyBreakdown, mixedWindowLegacy) {
			t.Errorf("%s: legacyBreakdown = %+v, want the uncovered estimate only %+v", path, got.LegacyBreakdown, mixedWindowLegacy)
		}
		if got.Earnings.Coverage != analytics.EarningsCoverageMixed || !got.Earnings.Exact || got.Earnings.LegacyStatus != analytics.LegacyStatusEstimated {
			t.Errorf("%s: earnings = %+v, want mixed coverage, exact=true, legacy estimated", path, got.Earnings)
		}
		if got.RawTruncated {
			t.Errorf("%s: rawTruncated on a six-sample window", path)
		}
	}
}

// TestPointsHistoryOldConsumerSeesBaseBreakdown: a consumer compiled against
// the base commit's response type decodes the candidate's response without
// error and reads, under `breakdown`, exactly the figure the base commit's
// algorithm gives the same raw timeline — for a mixed, a legacy-only and an
// exact-only window, on both endpoints (the base history endpoint reported
// that figure; the base export carried no breakdown, so there it is the same
// attribution over the exported series, additively).
func TestPointsHistoryOldConsumerSeesBaseBreakdown(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	cases := []struct {
		name    string
		seed    func(streamer string)
		samples int
		want    []analytics.ReasonShare // hand-worked from the base algorithm
	}{
		{"mixed", func(s string) { seedMixedWindow(t, srv, s) }, 6, mixedWindowBaseBreakdown},
		{"legacy only", func(s string) {
			// 11310 → 11772 (+462 WATCH STREAK) → 11784 (+12 WATCH)
			for _, p := range []struct {
				balance int
				reason  string
			}{{11310, "WATCH"}, {11772, "WATCH STREAK"}, {11784, "WATCH"}} {
				if err := repo.RecordPoints(s, p.balance, p.reason); err != nil {
					t.Fatal(err)
				}
			}
		}, 3, []analytics.ReasonShare{{Reason: "WATCH_STREAK", Gained: 462, Count: 1}, {Reason: "WATCH", Gained: 12, Count: 1}}},
		{"exact only", func(s string) {
			// 11772 (baseline) → 11784 (+12 WATCH) → 12234 (+450 WATCH STREAK):
			// the first grant is invisible to the base algorithm.
			first := time.Now().Add(-3 * time.Hour)
			recordExact(t, srv, s, "1", "WATCH_STREAK", 450, 11772, first)
			recordExact(t, srv, s, "2", "WATCH", 12, 11784, first.Add(time.Minute))
			recordExact(t, srv, s, "3", "WATCH_STREAK", 450, 12234, first.Add(2*time.Minute))
		}, 3, []analytics.ReasonShare{{Reason: "WATCH_STREAK", Gained: 450, Count: 1}, {Reason: "WATCH", Gained: 12, Count: 1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := uniqueLogin("compat_old_consumer")
			tc.seed(s)
			for _, path := range []string{"/api/points-history?streamer=" + s + "&range=24h", "/api/points-history/export?streamer=" + s + "&range=24h"} {
				var old baseShapedHistory
				if err := json.Unmarshal(fetchRaw(t, srv, path), &old); err != nil {
					t.Fatalf("%s: a base-shaped consumer cannot decode the response: %v", path, err)
				}
				if old.Streamer != s || old.Range != "24h" || len(old.Points) != tc.samples {
					t.Fatalf("%s: base-shaped decode = streamer %q range %q %d points, want %q/24h/%d", path, old.Streamer, old.Range, len(old.Points), s, tc.samples)
				}
				if !baseShapedEqual(old.Breakdown, tc.want) {
					t.Errorf("%s: an old consumer reads breakdown = %+v, want the base-commit figure %+v", path, old.Breakdown, tc.want)
				}
				if old.RawTruncated || old.ChartDownsampled {
					t.Errorf("%s: flags = %v/%v, want false/false", path, old.RawTruncated, old.ChartDownsampled)
				}
			}
		})
	}
}

// TestPointsHistoryEmptyWindowHasNoAccounting: a streamer with nothing in
// range reports no list at all — no compatibility breakdown, no exact
// accounting, no estimate — and coverage "none", on both endpoints.
func TestPointsHistoryEmptyWindowHasNoAccounting(t *testing.T) {
	srv := newStatsTestServer(t)
	s := uniqueLogin("compat_empty")
	for _, path := range []string{"/api/points-history?streamer=" + s + "&range=24h", "/api/points-history/export?streamer=" + s + "&range=24h"} {
		var got compatAccounting
		if err := json.Unmarshal(fetchRaw(t, srv, path), &got); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		if got.Breakdown != nil || got.ExactBreakdown != nil || got.LegacyBreakdown != nil {
			t.Errorf("%s: lists = breakdown %+v exact %+v legacy %+v, want none", path, got.Breakdown, got.ExactBreakdown, got.LegacyBreakdown)
		}
		if got.Earnings.Coverage != analytics.EarningsCoverageNone || got.Earnings.Exact || got.Earnings.LegacyStatus != analytics.LegacyStatusNone {
			t.Errorf("%s: earnings = %+v, want coverage none", path, got.Earnings)
		}
	}
}

// TestPointsHistoryExactAccountingSurvivesLostSample: the supported rollback
// case — a pre-ledger binary's retention sweep removed the balance sample an
// exact event produced, the ledger row outlived it. With no sample there is
// nothing for the compatibility breakdown to attribute, yet the authoritative
// exactBreakdown is served with coverage "exact", on both endpoints.
func TestPointsHistoryExactAccountingSurvivesLostSample(t *testing.T) {
	srv := newStatsTestServer(t)
	s := uniqueLogin("compat_orphan")
	at := time.Now().Add(-time.Hour)
	recordExact(t, srv, s, "orphan", "WATCH_STREAK", 450, 11772, at)

	// The old binary's sweep: the sample goes, the ledger row it does not
	// know about stays.
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`DELETE FROM points WHERE id = (SELECT points_id FROM point_events WHERE event_id = ?)`, "sha256:"+s+"-orphan")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		t.Fatalf("sample rows deleted = %d, err %v; want exactly the event's sample", n, err)
	}

	want := []analytics.ReasonShare{{Reason: "WATCH_STREAK", Gained: 450, Count: 1}}
	for _, path := range []string{"/api/points-history?streamer=" + s + "&range=24h", "/api/points-history/export?streamer=" + s + "&range=24h"} {
		body := fetchRaw(t, srv, path)
		var got compatAccounting
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		var series struct {
			Points []analytics.PointSample `json:"points"`
		}
		if err := json.Unmarshal(body, &series); err != nil {
			t.Fatalf("%s: decode series: %v", path, err)
		}
		if len(series.Points) != 0 {
			t.Fatalf("%s: points = %+v, want none after the sweep", path, series.Points)
		}
		if !sharesEqual(got.ExactBreakdown, want) {
			t.Errorf("%s: exactBreakdown = %+v, want the orphaned ledger row %+v", path, got.ExactBreakdown, want)
		}
		if got.Breakdown != nil || got.LegacyBreakdown != nil {
			t.Errorf("%s: breakdown %+v legacy %+v, want none without samples", path, got.Breakdown, got.LegacyBreakdown)
		}
		if got.Earnings.Coverage != analytics.EarningsCoverageExact || !got.Earnings.Exact || got.Earnings.ExactSince != at.UnixMilli() {
			t.Errorf("%s: earnings = %+v, want exact coverage since the event", path, got.Earnings)
		}
	}
}

// TestPointsHistoryCompatBreakdownUsesRawSeries: like the first release, the
// compatibility breakdown is computed on the raw series BEFORE the display
// downsampling, so thinning the chart never changes the figure an old
// consumer reads. An odd number of alternating +1 samples beyond the chart
// cap yields (n-1)/2 CLAIM and (n-1)/2 WATCH deltas of 1 each, tied on
// gained and therefore ordered by reason.
func TestPointsHistoryCompatBreakdownUsesRawSeries(t *testing.T) {
	srv := newStatsTestServer(t)
	repo := srv.analytics.Repository()
	s := uniqueLogin("compat_raw")
	const n = maxChartPoints + 101
	if n%2 == 0 {
		t.Fatalf("n = %d must be odd so both reasons get (n-1)/2 deltas", n)
	}
	half := (n - 1) / 2
	for i := 0; i < n; i++ {
		reason := "CLAIM"
		if i%2 == 1 {
			reason = "WATCH"
		}
		if err := repo.RecordPoints(s, 1000+i, reason); err != nil {
			t.Fatal(err)
		}
	}
	got := fetchHistory(t, srv, "/api/points-history?streamer="+s+"&range=24h")
	if !got.ChartDownsampled || got.RawTruncated || len(got.Points) >= n {
		t.Fatalf("chartDownsampled=%v rawTruncated=%v points=%d, want a downsampled, untruncated series", got.ChartDownsampled, got.RawTruncated, len(got.Points))
	}
	want := []analytics.ReasonShare{{Reason: "CLAIM", Gained: half, Count: half}, {Reason: "WATCH", Gained: half, Count: half}}
	if !sharesEqual(got.Breakdown, want) {
		t.Fatalf("breakdown = %+v, want the base attribution over the raw series %+v", got.Breakdown, want)
	}
	if !sharesEqual(got.LegacyBreakdown, want) || got.ExactBreakdown != nil {
		t.Fatalf("legacyBreakdown = %+v exact = %+v, want the same estimate over the raw legacy series and no exact accounting", got.LegacyBreakdown, got.ExactBreakdown)
	}
}

// TestPointsHistoryExactZeroEventsAreMeasuredCoverageWithoutList pins the
// documented edge of ExactBreakdown: a window whose only ledger events carry a
// non-positive amount is exactly covered — earnings.exact is true and
// coverage is "exact", a measured zero from the authoritative ledger, not a
// fabricated one — yet no exactBreakdown is emitted (nothing positive to
// aggregate), no legacy estimate exists (no legacy sample) and the
// compatibility breakdown attributes nothing (no positive balance delta).
// Both endpoints agree.
func TestPointsHistoryExactZeroEventsAreMeasuredCoverageWithoutList(t *testing.T) {
	srv := newStatsTestServer(t)
	s := uniqueLogin("compat_zero")
	first := time.Now().Add(-2 * time.Hour)
	recordExact(t, srv, s, "zero-a", "WATCH", 0, 1000, first)
	recordExact(t, srv, s, "zero-b", "WATCH", 0, 1000, first.Add(time.Minute))

	for _, path := range []string{"/api/points-history?streamer=" + s + "&range=24h", "/api/points-history/export?streamer=" + s + "&range=24h"} {
		body := fetchRaw(t, srv, path)
		var got compatAccounting
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("%s: decode: %v", path, err)
		}
		var series struct {
			Points []analytics.PointSample `json:"points"`
		}
		if err := json.Unmarshal(body, &series); err != nil {
			t.Fatalf("%s: decode series: %v", path, err)
		}
		if len(series.Points) != 2 || !series.Points[0].Exact || !series.Points[1].Exact {
			t.Fatalf("%s: points = %+v, want the two exact-backed samples", path, series.Points)
		}
		if got.Breakdown != nil || got.ExactBreakdown != nil || got.LegacyBreakdown != nil {
			t.Errorf("%s: breakdown %+v exact %+v legacy %+v, want no list: nothing positive to attribute", path, got.Breakdown, got.ExactBreakdown, got.LegacyBreakdown)
		}
		want := analytics.EarningsAccounting{Coverage: analytics.EarningsCoverageExact, Exact: true, ExactSince: first.UnixMilli(), LegacyStatus: analytics.LegacyStatusNone}
		if got.Earnings != want {
			t.Errorf("%s: earnings = %+v, want the measured exact zero %+v", path, got.Earnings, want)
		}
	}
}
