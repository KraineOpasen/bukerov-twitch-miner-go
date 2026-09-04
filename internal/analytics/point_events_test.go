package analytics

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// pointEventTestSeq makes every streamer login and event identity these tests
// write unique for the life of the process: the package shares one database
// singleton (history_test.go TestMain), so fixed names would collide under
// `go test -count=N` and turn a repeat into a false duplicate.
var pointEventTestSeq atomic.Uint64

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, pointEventTestSeq.Add(1))
}

// streakEvent is the production-shaped exact fact behind this change: one
// accepted WATCH_STREAK grant of 450 points whose frame reported the given
// balance.
func streakEvent(id string, ts time.Time, balance int) PointEvent {
	return PointEvent{
		EventID:      id,
		Timestamp:    ts.UnixMilli(),
		ReasonCode:   "WATCH_STREAK",
		TotalPoints:  450,
		BalanceAfter: balance,
		BalanceKnown: true,
	}
}

func streakAnnotation(amount int) *PointEventAnnotation {
	return pointEventAnnotation("WATCH_STREAK", amount)
}

// ledgerRow reads one point_events row back through SQL — the persisted
// fact itself, independent of any aggregation code.
type ledgerRow struct {
	timestamp    int64
	reason       string
	total        int
	balanceAfter sql.NullInt64
	pointsID     int64
}

func readLedger(t *testing.T, r *SQLiteRepository, streamer string) []ledgerRow {
	t.Helper()
	rows, err := r.db.Query(`SELECT e.timestamp, e.reason_code, e.total_points, e.balance_after, e.points_id
	                          FROM point_events e JOIN streamers s ON s.id = e.streamer_id
	                          WHERE s.name = ? ORDER BY e.id ASC`, streamer)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ledgerRow
	for rows.Next() {
		var lr ledgerRow
		if err := rows.Scan(&lr.timestamp, &lr.reason, &lr.total, &lr.balanceAfter, &lr.pointsID); err != nil {
			t.Fatalf("scan ledger: %v", err)
		}
		out = append(out, lr)
	}
	return out
}

func countRows(t *testing.T, r *SQLiteRepository, query string, args ...interface{}) int {
	t.Helper()
	var n int
	if err := r.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// TestRecordPointEventWritesLedgerSampleAndAnnotationTogether: one accepted
// event yields exactly one ledger row (event-local amount and balance), one
// balance-timeline sample flagged Exact that the row references, and one
// annotation built from the same amount — all stamped with the event's
// timestamp.
func TestRecordPointEventWritesLedgerSampleAndAnnotationTogether(t *testing.T) {
	r := newTestRepo(t)
	s := uniqueName("pe-write")
	ts := time.Now().Add(-time.Hour)

	recorded, err := r.RecordPointEvent(s, streakEvent("sha256:"+s+"-1", ts, 11772), 11772, streakAnnotation(450))
	if err != nil {
		t.Fatalf("RecordPointEvent: %v", err)
	}
	if !recorded {
		t.Fatal("first delivery must be recorded")
	}

	samples, err := r.GetPointSamples(s, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 1 {
		t.Fatalf("samples = %+v, want exactly one", samples)
	}
	if !samples[0].Exact || samples[0].Balance != 11772 || samples[0].Reason != "WATCH STREAK" || samples[0].T != ts.UnixMilli() {
		t.Fatalf("sample = %+v, want exact-backed WATCH STREAK at 11772 stamped %d", samples[0], ts.UnixMilli())
	}

	ledger := readLedger(t, r, s)
	if len(ledger) != 1 {
		t.Fatalf("ledger = %+v, want one row", ledger)
	}
	row := ledger[0]
	if row.reason != "WATCH_STREAK" || row.total != 450 || row.timestamp != ts.UnixMilli() {
		t.Fatalf("ledger row = %+v, want WATCH_STREAK 450 at %d", row, ts.UnixMilli())
	}
	if !row.balanceAfter.Valid || row.balanceAfter.Int64 != 11772 {
		t.Fatalf("balance_after = %+v, want 11772", row.balanceAfter)
	}
	var sampleID int64
	if err := r.db.QueryRow(`SELECT p.id FROM points p JOIN streamers s ON s.id = p.streamer_id WHERE s.name = ?`, s).Scan(&sampleID); err != nil {
		t.Fatal(err)
	}
	if row.pointsID != sampleID {
		t.Fatalf("ledger points_id = %d, want the sample it produced (%d)", row.pointsID, sampleID)
	}

	anns, err := r.GetAnnotationRecords(s, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(anns) != 1 || anns[0].Type != "WATCH_STREAK" || anns[0].Reason != "+450 - Watch Streak" || anns[0].T != ts.UnixMilli() {
		t.Fatalf("annotations = %+v, want one WATCH_STREAK '+450 - Watch Streak' at %d", anns, ts.UnixMilli())
	}

	exact, err := r.ExactEarningsBetween(s, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	want := ExactEarnings{Breakdown: []ReasonShare{{Reason: "WATCH_STREAK", Gained: 450, Count: 1}}, Events: 1, Since: ts.UnixMilli()}
	if !reflect.DeepEqual(exact, want) {
		t.Fatalf("exact earnings = %+v, want %+v", exact, want)
	}
}

// TestRecordPointEventDuplicateIdentityWritesNothing: an exact re-delivery of
// one event identity — even with a different balance and timestamp, as a late
// replay would carry — is rejected as a whole: no second ledger row, no second
// sample, no second annotation, one exact earning.
func TestRecordPointEventDuplicateIdentityWritesNothing(t *testing.T) {
	r := newTestRepo(t)
	s := uniqueName("pe-dup")
	id := "sha256:" + s
	ts := time.Now().Add(-time.Hour)

	if rec, err := r.RecordPointEvent(s, streakEvent(id, ts, 11772), 11772, streakAnnotation(450)); err != nil || !rec {
		t.Fatalf("first delivery: recorded=%v err=%v", rec, err)
	}
	replay := streakEvent(id, ts.Add(time.Minute), 11784)
	rec, err := r.RecordPointEvent(s, replay, 11784, streakAnnotation(450))
	if err != nil {
		t.Fatalf("replay must not error: %v", err)
	}
	if rec {
		t.Fatal("replay of the same identity reported as recorded")
	}

	if n := countRows(t, r, `SELECT COUNT(*) FROM point_events WHERE event_id = ?`, id); n != 1 {
		t.Fatalf("ledger rows for the identity = %d, want 1", n)
	}
	samples, _ := r.GetPointSamples(s, time.Time{}, time.Time{}, 0)
	if len(samples) != 1 || samples[0].Balance != 11772 {
		t.Fatalf("samples = %+v, want the single original sample (replay must leave no timeline row)", samples)
	}
	anns, _ := r.GetAnnotationRecords(s, time.Time{}, time.Time{})
	if len(anns) != 1 {
		t.Fatalf("annotations = %+v, want one (replay must leave no marker)", anns)
	}
	exact, _ := r.ExactEarningsBetween(s, time.Time{}, time.Time{})
	if exact.Events != 1 || exact.Breakdown[0].Gained != 450 || exact.Breakdown[0].Count != 1 {
		t.Fatalf("exact earnings after replay = %+v, want one 450 event", exact)
	}
}

// TestRecordPointEventDistinctIdentitiesSameAmountBothKept: two distinct
// events with identical amount, reason and timestamp are two facts.
func TestRecordPointEventDistinctIdentitiesSameAmountBothKept(t *testing.T) {
	r := newTestRepo(t)
	s := uniqueName("pe-distinct")
	ts := time.Now().Add(-time.Hour)

	for _, suffix := range []string{"-a", "-b"} {
		if rec, err := r.RecordPointEvent(s, streakEvent("sha256:"+s+suffix, ts, 11772), 11772, nil); err != nil || !rec {
			t.Fatalf("%s: recorded=%v err=%v", suffix, rec, err)
		}
	}
	exact, _ := r.ExactEarningsBetween(s, time.Time{}, time.Time{})
	if exact.Events != 2 || exact.Breakdown[0].Gained != 900 || exact.Breakdown[0].Count != 2 {
		t.Fatalf("exact earnings = %+v, want two distinct 450 events (900 over 2)", exact)
	}
	samples, _ := r.GetPointSamples(s, time.Time{}, time.Time{}, 0)
	if len(samples) != 2 || !samples[0].Exact || !samples[1].Exact {
		t.Fatalf("samples = %+v, want two exact-backed samples", samples)
	}
}

// TestRecordPointEventTombstonedIsRefused: the resurrection fence covers the
// ledger write like every other write path — a late event for a login being
// purged writes nothing and cannot recreate the streamer row.
func TestRecordPointEventTombstonedIsRefused(t *testing.T) {
	r := newTestRepo(t)
	s := uniqueName("pe-fenced")
	r.Tombstone(s)
	rec, err := r.RecordPointEvent(s, streakEvent("sha256:"+s, time.Now(), 1), 1, streakAnnotation(450))
	if !errors.Is(err, ErrStreamerDeleted) || rec {
		t.Fatalf("tombstoned write: recorded=%v err=%v, want ErrStreamerDeleted", rec, err)
	}
	if n := countRows(t, r, `SELECT COUNT(*) FROM streamers WHERE name = ?`, s); n != 0 {
		t.Fatal("a fenced point event recreated the streamer row")
	}
	if n := countRows(t, r, `SELECT COUNT(*) FROM point_events WHERE event_id = ?`, "sha256:"+s); n != 0 {
		t.Fatal("a fenced point event wrote a ledger row")
	}
	// The timeline-only marker takes the same fence.
	if err := r.RecordPointMarker(s, time.Now().UnixMilli(), *streakAnnotation(450)); !errors.Is(err, ErrStreamerDeleted) {
		t.Fatalf("tombstoned marker: err=%v, want ErrStreamerDeleted", err)
	}
	if n := countRows(t, r, `SELECT COUNT(*) FROM streamers WHERE name = ?`, s); n != 0 {
		t.Fatal("a fenced point marker recreated the streamer row")
	}
}

// TestRecordPointEventNonConflictFailureSurfaces: only the event_id conflict
// is swallowed as a duplicate. Any other constraint failure inside the ledger
// insert (here a CHECK standing in for one, on a private copy of the table)
// surfaces as an error, is never classified as a duplicate, and rolls the
// timeline sample back with it — and the identity is not consumed, so a later
// clean write of the same event still lands. An `INSERT OR IGNORE` would
// swallow the CHECK violation too and report a false duplicate.
func TestRecordPointEventNonConflictFailureSurfaces(t *testing.T) {
	r, db := openPrivateRepo(t, t.TempDir())
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`DROP TABLE point_events`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE point_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		streamer_id INTEGER NOT NULL,
		event_id TEXT NOT NULL UNIQUE,
		timestamp INTEGER NOT NULL,
		reason_code TEXT NOT NULL CHECK (reason_code <> 'POISON'),
		total_points INTEGER NOT NULL,
		balance_after INTEGER,
		points_id INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	s := uniqueName("pe-nonconflict")
	id := "sha256:" + s
	poison := PointEvent{EventID: id, Timestamp: 1, ReasonCode: "POISON", TotalPoints: 5}
	rec, err := r.RecordPointEvent(s, poison, 100, nil)
	if err == nil || rec {
		t.Fatalf("poisoned insert: recorded=%v err=%v, want an error and no row", rec, err)
	}
	if errors.Is(err, errDuplicatePointEvent) {
		t.Fatalf("a non-conflict failure was classified as a duplicate: %v", err)
	}
	if n := countRows(t, r, `SELECT COUNT(*) FROM points p JOIN streamers st ON st.id = p.streamer_id WHERE st.name = ?`, s); n != 0 {
		t.Fatalf("timeline sample survived the rolled-back ledger insert (%d rows)", n)
	}
	good := PointEvent{EventID: id, Timestamp: 2, ReasonCode: "WATCH", TotalPoints: 5}
	if rec, err := r.RecordPointEvent(s, good, 100, nil); err != nil || !rec {
		t.Fatalf("clean write after a failed one: recorded=%v err=%v, want recorded", rec, err)
	}
}

// TestRecordPointEventRefusesEmptyIdentity: no identity, no exact fact.
func TestRecordPointEventRefusesEmptyIdentity(t *testing.T) {
	r := newTestRepo(t)
	s := uniqueName("pe-noid")
	rec, err := r.RecordPointEvent(s, PointEvent{ReasonCode: "WATCH", TotalPoints: 12, Timestamp: time.Now().UnixMilli()}, 100, nil)
	if !errors.Is(err, errPointEventNoIdentity) || rec {
		t.Fatalf("empty identity: recorded=%v err=%v, want errPointEventNoIdentity and nothing recorded", rec, err)
	}
	if samples, _ := r.GetPointSamples(s, time.Time{}, time.Time{}, 0); len(samples) != 0 {
		t.Fatalf("empty-identity event wrote a sample: %+v", samples)
	}
}

// TestRecordPointEventUnknownBalanceIsNull: a frame without a balance stores
// NULL balance_after (unknown stays unknown) while the timeline still gets the
// caller's display balance.
func TestRecordPointEventUnknownBalanceIsNull(t *testing.T) {
	r := newTestRepo(t)
	s := uniqueName("pe-nobalance")
	ev := PointEvent{EventID: "sha256:" + s, Timestamp: time.Now().UnixMilli(), ReasonCode: "CLAIM", TotalPoints: 50}
	if rec, err := r.RecordPointEvent(s, ev, 777, nil); err != nil || !rec {
		t.Fatalf("recorded=%v err=%v", rec, err)
	}
	ledger := readLedger(t, r, s)
	if len(ledger) != 1 || ledger[0].balanceAfter.Valid {
		t.Fatalf("ledger = %+v, want one row with NULL balance_after", ledger)
	}
	samples, _ := r.GetPointSamples(s, time.Time{}, time.Time{}, 0)
	if len(samples) != 1 || samples[0].Balance != 777 || !samples[0].Exact {
		t.Fatalf("samples = %+v, want one exact sample at the display balance 777", samples)
	}
}

// TestExactEarningsBetweenAggregatesInRangeByCanonicalReason: the SQL
// aggregation sums event-local amounts per canonical reason (unknown reasons
// pool into OTHER, never vanish), counts positive events only, reports every
// row in Events (zero and negative amounts included), honors the window, and
// reports the earliest in-range event.
func TestExactEarningsBetweenAggregatesInRangeByCanonicalReason(t *testing.T) {
	r := newTestRepo(t)
	s := uniqueName("pe-agg")
	base := time.Now().Add(-2 * time.Hour)
	events := []struct {
		id     string
		reason string
		amount int
		at     time.Duration
	}{
		{"old", "WATCH", 12, -48 * time.Hour}, // outside the window
		{"1", "WATCH", 12, 0},
		{"2", "WATCH", 12, time.Minute},
		{"3", "CLAIM", 50, 2 * time.Minute},
		{"4", "RAID", 250, 3 * time.Minute},
		{"5", "WATCH_STREAK", 450, 4 * time.Minute},
		{"6", "PREDICTION", 1000, 5 * time.Minute},
		{"7", "WEEKLY_REWARDS", 7, 6 * time.Minute}, // unknown reason: OTHER, still exact
		{"8", "WATCH", 0, 7 * time.Minute},          // accepted fact, not an earning
		{"9", "CLAIM", -50, 8 * time.Minute},        // accepted fact, never an earning
	}
	for _, e := range events {
		ev := PointEvent{EventID: "sha256:" + s + "-" + e.id, Timestamp: base.Add(e.at).UnixMilli(), ReasonCode: e.reason, TotalPoints: e.amount}
		if rec, err := r.RecordPointEvent(s, ev, 0, nil); err != nil || !rec {
			t.Fatalf("%s: recorded=%v err=%v", e.id, rec, err)
		}
	}

	got, err := r.ExactEarningsBetween(s, base.Add(-time.Minute), base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := ExactEarnings{
		Breakdown: []ReasonShare{
			{Reason: "PREDICTION", Gained: 1000, Count: 1},
			{Reason: "WATCH_STREAK", Gained: 450, Count: 1},
			{Reason: "RAID", Gained: 250, Count: 1},
			{Reason: "CLAIM", Gained: 50, Count: 1},
			{Reason: "WATCH", Gained: 24, Count: 2},
			{Reason: "OTHER", Gained: 7, Count: 1},
		},
		Events: 9,
		Since:  base.UnixMilli(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exact earnings = %+v, want %+v", got, want)
	}

	// An unknown streamer and an empty window are empty, not errors.
	if empty, err := r.ExactEarningsBetween(uniqueName("pe-agg-nobody"), time.Time{}, time.Time{}); err != nil || empty.Events != 0 || empty.Breakdown != nil || empty.Since != 0 {
		t.Fatalf("unknown streamer = %+v err=%v, want empty", empty, err)
	}
	if empty, _ := r.ExactEarningsBetween(s, base.Add(2*time.Hour), base.Add(3*time.Hour)); empty.Events != 0 || empty.Breakdown != nil {
		t.Fatalf("empty window = %+v, want empty", empty)
	}
}

// TestGetPointSamplesFlagsExactAndOrdersSameMillisecond: legacy samples and
// exact-backed samples written at the SAME millisecond come back in insertion
// (row id) order with the correct Exact flags — the legacy estimator's adjacent
// deltas depend on that order being stable.
func TestGetPointSamplesFlagsExactAndOrdersSameMillisecond(t *testing.T) {
	r := newTestRepo(t)
	s := uniqueName("pe-order")
	ts := time.Now().Add(-time.Hour)

	seedPoint(t, r, s, ts, 1000, "WATCH") // legacy
	if _, err := r.RecordPointEvent(s, streakEvent("sha256:"+s+"-1", ts, 1450), 1450, nil); err != nil {
		t.Fatal(err)
	}
	seedPoint(t, r, s, ts, 1400, "Spent") // legacy spend at the same ms
	if _, err := r.RecordPointEvent(s, streakEvent("sha256:"+s+"-2", ts, 1850), 1850, nil); err != nil {
		t.Fatal(err)
	}

	samples, err := r.GetPointSamples(s, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantBalances := []int{1000, 1450, 1400, 1850}
	wantExact := []bool{false, true, false, true}
	if len(samples) != 4 {
		t.Fatalf("samples = %+v, want 4", samples)
	}
	for i := range samples {
		if samples[i].Balance != wantBalances[i] || samples[i].Exact != wantExact[i] || samples[i].T != ts.UnixMilli() {
			t.Fatalf("sample[%d] = %+v, want balance %d exact %v at %d (insertion order)", i, samples[i], wantBalances[i], wantExact[i], ts.UnixMilli())
		}
	}
	// The limit applies to the same ordering.
	limited, _ := r.GetPointSamples(s, time.Time{}, time.Time{}, 2)
	if len(limited) != 2 || limited[0].Balance != 1000 || limited[1].Balance != 1450 {
		t.Fatalf("limited samples = %+v, want the first two in order", limited)
	}
}

// TestEstimateLegacyBreakdownSkipsExactAndSpent pins the legacy estimator's
// two exclusions: a delta into an exact-backed sample is already accounted
// for exactly (the production 462 deltas around 450 grants must not be added
// on top), and a delta into a points-spent snapshot is a spend, never an
// earning, whatever its sign. Samples counts legacy earning samples only.
func TestEstimateLegacyBreakdownSkipsExactAndSpent(t *testing.T) {
	cases := []struct {
		name    string
		samples []PointSample
		want    LegacyEstimate
	}{
		{
			name: "exact-backed streak delta is not re-estimated",
			samples: []PointSample{
				{Balance: 11310, Reason: "WATCH"},                     // legacy baseline
				{Balance: 11772, Reason: "WATCH STREAK", Exact: true}, // exact 450 grant, +462 delta
				{Balance: 11784, Reason: "WATCH"},                     // legacy +12
			},
			want: LegacyEstimate{Breakdown: []ReasonShare{{Reason: "WATCH", Gained: 12, Count: 1}}, Samples: 2},
		},
		{
			name: "fully exact series yields no estimate and no legacy samples",
			samples: []PointSample{
				{Balance: 1000, Reason: "WATCH", Exact: true},
				{Balance: 1450, Reason: "WATCH STREAK", Exact: true},
			},
			want: LegacyEstimate{},
		},
		{
			name: "positive delta into a spent snapshot is not an earning",
			samples: []PointSample{
				{Balance: 11322, Reason: "WATCH"}, // stale-low legacy sample
				{Balance: 11700, Reason: "Spent"}, // bet placed after a real balance of 11784: +378 delta
				{Balance: 11712, Reason: "WATCH"},
			},
			want: LegacyEstimate{Breakdown: []ReasonShare{{Reason: "WATCH", Gained: 12, Count: 1}}, Samples: 2},
		},
		{
			name:    "lone legacy baseline is present but unattributable",
			samples: []PointSample{{Balance: 1450, Reason: "WATCH STREAK"}},
			want:    LegacyEstimate{Samples: 1},
		},
		{
			name:    "spent-only series is neither earning nor legacy evidence",
			samples: []PointSample{{Balance: 1000, Reason: "Spent"}, {Balance: 900, Reason: "Spent"}},
			want:    LegacyEstimate{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := EstimateLegacyBreakdown(tc.samples)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("EstimateLegacyBreakdown() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestComposeEarningsCoverageMatrix pins the coverage contract: exact is
// claimed only for a range with exact events, no legacy earning sample and a
// complete series; a mixed range reports both accountings separately; a
// legacy-only range reports its estimate once (as breakdown) so nothing can
// be summed; a truncated series never turns the legacy part into zero or
// into exactness.
func TestComposeEarningsCoverageMatrix(t *testing.T) {
	exactShares := []ReasonShare{{Reason: "WATCH_STREAK", Gained: 1350, Count: 3}}
	legacyShares := []ReasonShare{{Reason: "WATCH_STREAK", Gained: 462, Count: 1}}
	exact := ExactEarnings{Breakdown: exactShares, Events: 3, Since: 1700000000000}
	legacy := LegacyEstimate{Breakdown: legacyShares, Samples: 2}

	cases := []struct {
		name          string
		exact         ExactEarnings
		legacy        LegacyEstimate
		rawTruncated  bool
		wantBreakdown []ReasonShare
		wantLegacy    []ReasonShare
		wantAcc       EarningsAccounting
	}{
		{"exact only", exact, LegacyEstimate{}, false, exactShares, nil,
			EarningsAccounting{Coverage: EarningsCoverageExact, Exact: true, ExactSince: 1700000000000, LegacyStatus: LegacyStatusNone}},
		{"mixed keeps both separately", exact, legacy, false, exactShares, legacyShares,
			EarningsAccounting{Coverage: EarningsCoverageMixed, Exact: true, ExactSince: 1700000000000, LegacyStatus: LegacyStatusEstimated}},
		{"mixed with unattributable legacy baseline", exact, LegacyEstimate{Samples: 1}, false, exactShares, nil,
			EarningsAccounting{Coverage: EarningsCoverageMixed, Exact: true, ExactSince: 1700000000000, LegacyStatus: LegacyStatusEstimated}},
		{"legacy only is an estimate reported once", ExactEarnings{}, legacy, false, legacyShares, nil,
			EarningsAccounting{Coverage: EarningsCoverageLegacy, Exact: false, LegacyStatus: LegacyStatusEstimated}},
		{"nothing", ExactEarnings{}, LegacyEstimate{}, false, nil, nil,
			EarningsAccounting{Coverage: EarningsCoverageNone, Exact: false, LegacyStatus: LegacyStatusNone}},
		{"truncated with exact events keeps exact, legacy unavailable", exact, legacy, true, exactShares, nil,
			EarningsAccounting{Coverage: EarningsCoverageMixed, Exact: true, ExactSince: 1700000000000, LegacyStatus: LegacyStatusUnavailable}},
		{"truncated without exact events is unavailable, not zero", ExactEarnings{}, legacy, true, nil, nil,
			EarningsAccounting{Coverage: EarningsCoverageUnavailable, Exact: false, LegacyStatus: LegacyStatusUnavailable}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			breakdown, legacyBreakdown, acc := ComposeEarnings(tc.exact, tc.legacy, tc.rawTruncated)
			if !reflect.DeepEqual(breakdown, tc.wantBreakdown) {
				t.Errorf("breakdown = %+v, want %+v", breakdown, tc.wantBreakdown)
			}
			if !reflect.DeepEqual(legacyBreakdown, tc.wantLegacy) {
				t.Errorf("legacyBreakdown = %+v, want %+v", legacyBreakdown, tc.wantLegacy)
			}
			if acc != tc.wantAcc {
				t.Errorf("earnings = %+v, want %+v", acc, tc.wantAcc)
			}
		})
	}
}

// TestPruneBeforeRemovesExpiredPointEvents: the exact ledger shares the
// analytics retention window — an expired event, its sample and its marker go
// together; fresh ones stay; the returned count is the honest total.
func TestPruneBeforeRemovesExpiredPointEvents(t *testing.T) {
	// A private database: PruneBefore sweeps every streamer, so the shared
	// package handle would count other tests' stale rows.
	r, db := openPrivateRepo(t, t.TempDir())
	defer func() { _ = db.Close() }()
	const s = "pe-prune-streamer"
	now := time.Now()
	if _, err := r.RecordPointEvent(s, streakEvent("sha256:pe-prune-old", now.Add(-40*24*time.Hour), 1000), 1000, streakAnnotation(450)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RecordPointEvent(s, streakEvent("sha256:pe-prune-new", now.Add(-2*24*time.Hour), 1450), 1450, streakAnnotation(450)); err != nil {
		t.Fatal(err)
	}

	deleted, err := r.PruneBefore(now.Add(-30 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 3 {
		t.Fatalf("pruned rows = %d, want 3 (one sample + one ledger row + one annotation)", deleted)
	}
	if n := countRows(t, r, `SELECT COUNT(*) FROM point_events WHERE event_id = ?`, "sha256:pe-prune-old"); n != 0 {
		t.Fatal("expired ledger row survived retention")
	}
	exact, _ := r.ExactEarningsBetween(s, time.Time{}, time.Time{})
	if exact.Events != 1 || exact.Since != now.Add(-2*24*time.Hour).UnixMilli() {
		t.Fatalf("exact earnings after prune = %+v, want only the fresh event", exact)
	}
	samples, _ := r.GetPointSamples(s, time.Time{}, time.Time{}, 0)
	if len(samples) != 1 || !samples[0].Exact {
		t.Fatalf("samples after prune = %+v, want the single fresh exact sample", samples)
	}
}

// TestServiceRecordPointEventTriggersRetentionSweep: the ledger write is now
// the frequent analytics write, so it must drive the throttled retention
// sweep exactly as RecordPoints does — otherwise an install whose history is
// entirely exact events would never prune.
func TestServiceRecordPointEventTriggersRetentionSweep(t *testing.T) {
	r, db := openPrivateRepo(t, t.TempDir())
	defer func() { _ = db.Close() }()
	fixed := time.Now()
	svc := &Service{repo: r, retentionDays: 30, now: func() time.Time { return fixed }}
	st := models.NewStreamer("pe-svc-retention", models.DefaultStreamerSettings())

	stale := streakEvent("sha256:pe-svc-retention-stale", fixed.Add(-40*24*time.Hour), 1000)
	if rec, err := r.RecordPointEvent(st.GetUsername(), stale, 1000, streakAnnotation(450)); err != nil || !rec {
		t.Fatalf("seed stale: recorded=%v err=%v", rec, err)
	}

	fresh := PointEvent{EventID: "sha256:pe-svc-retention-fresh", ReasonCode: "WATCH", TotalPoints: 12, BalanceAfter: 1012, BalanceKnown: true}
	if rec, err := svc.RecordPointEvent(st, fresh); err != nil || !rec {
		t.Fatalf("fresh: recorded=%v err=%v", rec, err)
	}

	if n := countRows(t, r, `SELECT COUNT(*) FROM point_events WHERE event_id = ?`, stale.EventID); n != 0 {
		t.Fatal("RecordPointEvent did not trigger the retention sweep: the stale ledger row survived")
	}
	exact, _ := r.ExactEarningsBetween(st.GetUsername(), time.Time{}, time.Time{})
	if exact.Events != 1 || exact.Breakdown[0].Reason != "WATCH" {
		t.Fatalf("exact earnings after the sweep = %+v, want only the fresh WATCH event", exact)
	}
}

// TestDeleteStreamerPurgesPointEventsAndFenceHolds: the purge removes the
// ledger rows with everything else in one transaction, and a late event that
// lost the race with the tombstone cannot resurrect any of it.
func TestDeleteStreamerPurgesPointEventsAndFenceHolds(t *testing.T) {
	r := newTestRepo(t)
	victim, keep := uniqueName("pe-del-victim"), uniqueName("pe-del-keep")
	for _, s := range []string{victim, keep} {
		if _, err := r.RecordPointEvent(s, streakEvent("sha256:"+s, time.Now(), 1450), 1450, streakAnnotation(450)); err != nil {
			t.Fatal(err)
		}
	}
	r.Tombstone(victim)
	existed, err := r.DeleteStreamer(context.Background(), victim)
	if err != nil || !existed {
		t.Fatalf("delete: existed=%v err=%v", existed, err)
	}
	if n := countRows(t, r, `SELECT COUNT(*) FROM point_events WHERE event_id = ?`, "sha256:"+victim); n != 0 {
		t.Fatal("victim's ledger row survived the purge (orphan point_events)")
	}
	if n := countRows(t, r, `SELECT COUNT(*) FROM point_events WHERE event_id = ?`, "sha256:"+keep); n != 1 {
		t.Fatal("unrelated streamer's ledger row was purged")
	}
	late, err := r.RecordPointEvent(victim, streakEvent("sha256:"+victim+"-late", time.Now(), 1900), 1900, nil)
	if !errors.Is(err, ErrStreamerDeleted) || late {
		t.Fatalf("late event after purge: recorded=%v err=%v, want ErrStreamerDeleted", late, err)
	}
	if n := countRows(t, r, `SELECT COUNT(*) FROM streamers WHERE name = ?`, victim); n != 0 {
		t.Fatal("late event resurrected the purged streamer")
	}
	r.Reinstate(victim)
}

// TestRenameStreamerPreservesPointEvents: the ledger is keyed by the stable
// streamers.id, so a rename carries the exact history along without rewriting
// a single ledger row.
func TestRenameStreamerPreservesPointEvents(t *testing.T) {
	r := newTestRepo(t)
	old, newName := uniqueName("pe-rename-old"), uniqueName("pe-rename-new")
	ts := time.Now().Add(-time.Hour)
	if _, err := r.RecordPointEvent(old, streakEvent("sha256:"+old, ts, 1450), 1450, streakAnnotation(450)); err != nil {
		t.Fatal(err)
	}
	before := readLedger(t, r, old)
	if err := r.RenameStreamer(old, newName); err != nil {
		t.Fatal(err)
	}
	after := readLedger(t, r, newName)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("ledger rows changed across rename: before=%+v after=%+v", before, after)
	}
	exact, _ := r.ExactEarningsBetween(newName, time.Time{}, time.Time{})
	if exact.Events != 1 || exact.Breakdown[0].Gained != 450 {
		t.Fatalf("exact earnings under the new name = %+v, want the original event", exact)
	}
	if gone, _ := r.ExactEarningsBetween(old, time.Time{}, time.Time{}); gone.Events != 0 {
		t.Fatalf("old name still carries exact history: %+v", gone)
	}
}

// TestRecordPointEventConcurrentSameIdentityExactlyOneWinner: concurrent
// deliveries of one identity (the pool can dispatch replays on more than one
// connection generation) serialize on the repository write mutex — and behind
// it the single SQLite connection — and exactly one commits: no partial
// sample or marker from the losers.
func TestRecordPointEventConcurrentSameIdentityExactlyOneWinner(t *testing.T) {
	r := newTestRepo(t)
	s := uniqueName("pe-race")
	ev := streakEvent("sha256:"+s, time.Now(), 1450)

	const workers = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	recordedCount := 0
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			rec, err := r.RecordPointEvent(s, ev, 1450, streakAnnotation(450))
			if err != nil {
				t.Errorf("concurrent RecordPointEvent: %v", err)
				return
			}
			if rec {
				mu.Lock()
				recordedCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if recordedCount != 1 {
		t.Fatalf("recorded winners = %d, want exactly 1", recordedCount)
	}
	if n := countRows(t, r, `SELECT COUNT(*) FROM point_events WHERE event_id = ?`, "sha256:"+s); n != 1 {
		t.Fatalf("ledger rows = %d, want 1", n)
	}
	samples, _ := r.GetPointSamples(s, time.Time{}, time.Time{}, 0)
	anns, _ := r.GetAnnotationRecords(s, time.Time{}, time.Time{})
	if len(samples) != 1 || len(anns) != 1 {
		t.Fatalf("samples=%d annotations=%d, want 1/1", len(samples), len(anns))
	}
}

// openPrivateRepo builds an analytics repository over its own, non-singleton
// database file (the openRawTestDB precedent), so a test can Close it
// without touching the shared package handle.
func openPrivateRepo(t *testing.T, dir string) (*SQLiteRepository, *database.DB) {
	t.Helper()
	db := openPrivateDB(t, filepath.Join(dir, "miner.db"))
	repo, err := NewSQLiteRepository(db, dir)
	if err != nil {
		t.Fatalf("new repo: %v", err)
	}
	return repo, db
}

// TestRecordPointEventAfterCloseIsRefusedTyped: the ledger write goes through
// database.WithTx, so after shutdown closed the shared handle it is refused
// with the typed ErrClosed instead of silently reopening a connection.
func TestRecordPointEventAfterCloseIsRefusedTyped(t *testing.T) {
	r, db := openPrivateRepo(t, t.TempDir())
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	rec, err := r.RecordPointEvent("pe-closed", streakEvent("sha256:pe-closed", time.Now(), 1), 1, nil)
	if !errors.Is(err, database.ErrClosed) || rec {
		t.Fatalf("write after close: recorded=%v err=%v, want database.ErrClosed", rec, err)
	}
	// The timeline-only marker and sample are refused by the same barrier,
	// typed.
	if err := r.RecordPointMarker("pe-closed", time.Now().UnixMilli(), *streakAnnotation(450)); !errors.Is(err, database.ErrClosed) {
		t.Fatalf("marker after close: err=%v, want database.ErrClosed", err)
	}
	if err := r.RecordPoints("pe-closed", 1, "WATCH"); !errors.Is(err, database.ErrClosed) {
		t.Fatalf("sample after close: err=%v, want database.ErrClosed", err)
	}
}

// levelRecorder is a slog.Handler that records every message with its
// severity so a test can pin "each path logged exactly at Debug, and nothing
// was logged above Debug".
type levelRecorder struct {
	mu         sync.Mutex
	debug      []string
	aboveDebug []string
}

func (h *levelRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (h *levelRecorder) Handle(_ context.Context, rec slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rec.Level > slog.LevelDebug {
		h.aboveDebug = append(h.aboveDebug, rec.Message)
	} else {
		h.debug = append(h.debug, rec.Message)
	}
	return nil
}
func (h *levelRecorder) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *levelRecorder) WithGroup(string) slog.Handler      { return h }

// TestServiceRecordPointEventAfterCloseIsDroppedTyped exercises the service
// seam of the close barrier: a handler that outlived shutdown gets the typed
// refusal back (nothing partial), and every analytics write path — ledger,
// marker, timeline sample and the retention sweep behind it — drops at debug
// level, never as a warning or error, because it is the expected teardown
// race rather than a fault.
func TestServiceRecordPointEventAfterCloseIsDroppedTyped(t *testing.T) {
	r, db := openPrivateRepo(t, t.TempDir())
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	logs := &levelRecorder{}
	prev := slog.Default()
	slog.SetDefault(slog.New(logs))
	t.Cleanup(func() { slog.SetDefault(prev) })

	svc := &Service{repo: r, now: time.Now, retentionDays: 1}
	st := models.NewStreamer("pe-svc-closed", models.DefaultStreamerSettings())
	rec, err := svc.RecordPointEvent(st, streakEvent("sha256:pe-svc-closed", time.Now(), 1))
	if !errors.Is(err, database.ErrClosed) || rec {
		t.Fatalf("service write after close: recorded=%v err=%v, want database.ErrClosed", rec, err)
	}
	svc.RecordPointMarker(st, "WATCH_STREAK", 450) // must not panic or reopen anything
	svc.RecordPoints(st, "WATCH")                  // timeline-only path
	svc.maybePrune()                               // the retention sweep behind the writes
	want := []string{
		"Dropping point event after database close",
		"Dropping point marker after database close",
		"Dropping timeline sample after database close",
		"Skipping analytics retention sweep after database close",
	}
	if !reflect.DeepEqual(logs.debug, want) || len(logs.aboveDebug) != 0 {
		t.Fatalf("after-close writes logged debug=%q aboveDebug=%q; want exactly %q at debug and nothing above", logs.debug, logs.aboveDebug, want)
	}
}

// TestServiceRecordPointEventUsesEventLocalValuesNotStreamerBalance is the
// mutable-state falsifier at the service seam: the ledger stores the frame's
// own amount and balance, so a Streamer balance mutated before OR after the
// write (a poll, a later frame) changes nothing in the recorded fact.
func TestServiceRecordPointEventUsesEventLocalValuesNotStreamerBalance(t *testing.T) {
	r := newTestRepo(t)
	fixed := time.Now().Add(-time.Hour)
	svc := &Service{repo: r, now: func() time.Time { return fixed }}
	login := uniqueName("pe-svc-mutable")
	st := models.NewStreamer(login, models.DefaultStreamerSettings())
	st.SetChannelPoints(999_999) // stale/mutated shared state

	ev := PointEvent{EventID: "sha256:" + login, ReasonCode: "WATCH_STREAK", TotalPoints: 450, BalanceAfter: 11772, BalanceKnown: true}
	rec, err := svc.RecordPointEvent(st, ev)
	if err != nil || !rec {
		t.Fatalf("recorded=%v err=%v", rec, err)
	}
	st.SetChannelPoints(5)

	ledger := readLedger(t, r, login)
	if len(ledger) != 1 || ledger[0].total != 450 || !ledger[0].balanceAfter.Valid || ledger[0].balanceAfter.Int64 != 11772 || ledger[0].timestamp != fixed.UnixMilli() {
		t.Fatalf("ledger = %+v, want total 450, balance_after 11772, stamped by the service clock", ledger)
	}
	samples, _ := r.GetPointSamples(login, time.Time{}, time.Time{}, 0)
	if len(samples) != 1 || samples[0].Balance != 11772 || !samples[0].Exact {
		t.Fatalf("timeline sample = %+v, want the frame's 11772, never the streamer's 999999/5", samples)
	}
	anns, _ := r.GetAnnotationRecords(login, time.Time{}, time.Time{})
	if len(anns) != 1 || anns[0].Reason != "+450 - Watch Streak" || anns[0].Color != annotationColors["WATCH_STREAK"] {
		t.Fatalf("annotation = %+v, want '+450 - Watch Streak' in the streak colour", anns)
	}

	// Without a wire balance the timeline sample falls back to the streamer's
	// current balance (a display value), while the ledger keeps it unknown.
	st.SetChannelPoints(4242)
	rec, err = svc.RecordPointEvent(st, PointEvent{EventID: "sha256:" + login + "-nobal", ReasonCode: "RAID", TotalPoints: 250})
	if err != nil || !rec {
		t.Fatalf("recorded=%v err=%v", rec, err)
	}
	ledger = readLedger(t, r, login)
	if len(ledger) != 2 || ledger[1].balanceAfter.Valid || ledger[1].total != 250 {
		t.Fatalf("ledger = %+v, want a second RAID row with NULL balance_after", ledger)
	}
	samples, _ = r.GetPointSamples(login, time.Time{}, time.Time{}, 0)
	if len(samples) != 2 || samples[1].Balance != 4242 {
		t.Fatalf("samples = %+v, want the fallback display balance 4242 for the balance-less frame", samples)
	}
	anns, _ = r.GetAnnotationRecords(login, time.Time{}, time.Time{})
	if len(anns) != 2 || anns[1].Type != "RAID" || anns[1].Reason != "+250 - Raid" {
		t.Fatalf("annotations = %+v, want a RAID marker '+250 - Raid'", anns)
	}

	// Plain WATCH/CLAIM/PREDICTION events carry no chart marker.
	if _, err := svc.RecordPointEvent(st, PointEvent{EventID: "sha256:" + login + "-watch", ReasonCode: "WATCH", TotalPoints: 12}); err != nil {
		t.Fatal(err)
	}
	if anns, _ = r.GetAnnotationRecords(login, time.Time{}, time.Time{}); len(anns) != 2 {
		t.Fatalf("a WATCH event wrote a marker: %+v", anns)
	}
}

// TestServiceRecordPointEventRejectsEmptyIdentityAndReportsDuplicate: the
// service refuses an identity-less event with an error (nothing written) and
// reports an exact re-delivery as not recorded without an error.
func TestServiceRecordPointEventRejectsEmptyIdentityAndReportsDuplicate(t *testing.T) {
	r := newTestRepo(t)
	svc := &Service{repo: r, now: time.Now}
	login := uniqueName("pe-svc-ident")
	st := models.NewStreamer(login, models.DefaultStreamerSettings())

	if rec, err := svc.RecordPointEvent(st, PointEvent{ReasonCode: "WATCH", TotalPoints: 12}); !errors.Is(err, errPointEventNoIdentity) || rec {
		t.Fatalf("identity-less event: recorded=%v err=%v, want errPointEventNoIdentity", rec, err)
	}
	if samples, _ := r.GetPointSamples(login, time.Time{}, time.Time{}, 0); len(samples) != 0 {
		t.Fatalf("identity-less event wrote a sample: %+v", samples)
	}

	ev := PointEvent{EventID: "sha256:" + login + "-1", ReasonCode: "CLAIM", TotalPoints: 50, BalanceAfter: 150, BalanceKnown: true}
	if rec, err := svc.RecordPointEvent(st, ev); err != nil || !rec {
		t.Fatalf("first: recorded=%v err=%v", rec, err)
	}
	if rec, err := svc.RecordPointEvent(st, ev); err != nil || rec {
		t.Fatalf("replay: recorded=%v err=%v, want (false, nil)", rec, err)
	}
	exact, _ := r.ExactEarningsBetween(login, time.Time{}, time.Time{})
	if exact.Events != 1 {
		t.Fatalf("exact earnings after replay = %+v, want one event", exact)
	}
}

// TestServiceRecordPointMarkerWritesOnlyStreakAndRaidMarkers: the timeline-only
// fallback marker reuses the ledger's marker table, so a frame that earned an
// exact amount but could not be admitted still gets the same "+N - Watch
// Streak" / "+N - Raid" text, and nothing else gets a marker.
func TestServiceRecordPointMarkerWritesOnlyStreakAndRaidMarkers(t *testing.T) {
	r := newTestRepo(t)
	svc := &Service{repo: r, now: time.Now}
	login := uniqueName("pe-svc-marker")
	st := models.NewStreamer(login, models.DefaultStreamerSettings())

	svc.RecordPointMarker(st, "WATCH_STREAK", 450)
	svc.RecordPointMarker(st, "RAID", 250)
	svc.RecordPointMarker(st, "WATCH", 12)
	svc.RecordPointMarker(st, "CLAIM", 50)

	anns, err := r.GetAnnotationRecords(login, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(anns) != 2 || anns[0].Reason != "+450 - Watch Streak" || anns[1].Reason != "+250 - Raid" {
		t.Fatalf("annotations = %+v, want exactly the streak and raid markers", anns)
	}
	if exact, _ := r.ExactEarningsBetween(login, time.Time{}, time.Time{}); exact.Events != 0 {
		t.Fatalf("a marker wrote a ledger row: %+v", exact)
	}
}
