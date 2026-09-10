package reader_test

// The reader's bound has to survive the arithmetic that implements it.

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval/reader"
)

// TestABoundThatCannotBeIncrementedIsRefused regresses an overflow that
// silently removed the bound entirely.
//
// LoadSession probes with limit+1 so a session AT the bound stays
// distinguishable from one over it. At the platform maximum that addition wraps
// negative, and ObservationsBySession applies its SQL LIMIT only when the value
// is positive — so the whole session was scanned, decoded and allocated, and
// the `len(rows) > limit` guard could never fire either, because no slice
// length exceeds MaxInt. A caller asking for the largest possible bound got no
// bound at all, which is the exact opposite of what it asked for.
//
// Measured before the fix, on a six-fact session:
//
//	limit+1 at MaxInt wraps to -9223372036854775808 (negative? true)
//	LoadSession(limit=MaxInt) -> 6 records, err=<nil>
func TestABoundThatCannotBeIncrementedIsRefused(t *testing.T) {
	ctx := context.Background()
	repo := observationStore(t)

	facts := make([]analytics.PredictionObservation, 0, 6)
	for i := 0; i < 6; i++ {
		f := baseFact(analytics.KindChannelEvent)
		f.Payload = analytics.ObservationPayload{
			Phase: "ROUND_UPDATED", RoundState: "ACTIVE",
			Counters: map[string]int64{"outcomeCount": 2},
		}
		facts = append(facts, f)
	}
	epoch, _ := seedSession(t, repo, "overflow-bound", facts)

	for _, limit := range []int{math.MaxInt, math.MaxInt - 1, reader.MaxLoadLimit + 1} {
		ds, err := reader.LoadSession(ctx, repo, epoch, limit)
		if !errors.Is(err, reader.ErrLimitOutOfRange) {
			t.Fatalf("LoadSession with limit %d returned %d facts and err %v, want ErrLimitOutOfRange. "+
				"A bound this large is not a bound: incrementing it for the probe overflows, and a "+
				"negative value removes the store's SQL LIMIT altogether",
				limit, len(ds.Records), err)
		}
		if len(ds.Records) != 0 {
			t.Fatalf("the refused load still handed back %d facts", len(ds.Records))
		}
	}

	// The ceiling itself is still honoured, so the guard did not simply refuse
	// every large bound.
	ds, err := reader.LoadSession(ctx, repo, epoch, reader.MaxLoadLimit)
	if err != nil {
		t.Fatalf("LoadSession at the ceiling returned %v, want the whole session", err)
	}
	if len(ds.Records) != 6 {
		t.Fatalf("read %d facts at the ceiling, want 6", len(ds.Records))
	}
}

// budgetProbe is a source that reports a measured size of the test's choosing
// and FAILS if the row load is reached.
//
// The ordering is the property under test. A byte budget applied after
// ObservationsBySession returns has already been paid: that call scans
// payload_json for every row and json.Unmarshals it, so the memory is spent
// before any check on the returned slice could run. Asserting only on the
// returned error would pass just as happily with the check in the wrong place.
type budgetProbe struct {
	real  reader.ObservationSource
	size  analytics.ObservationSessionSize
	t     *testing.T
	loads int
}

func (p *budgetProbe) ReadObservationSessionWithinBudget(ctx context.Context, epoch int64,
	maxRowBytes int64) (analytics.ObservationSessionReading, bool, bool, error) {
	return p.real.ReadObservationSessionWithinBudget(ctx, epoch, maxRowBytes)
}

func (p *budgetProbe) ObservationSessionSizeBySession(ctx context.Context, sessionID string,
	candidateRows int) (analytics.ObservationSessionSize, error) {
	return p.size, nil
}

func (p *budgetProbe) ObservationEpochRowBounds(ctx context.Context, epoch int64,
	candidateRows int) (analytics.ObservationEpochSize, error) {
	return p.real.ObservationEpochRowBounds(ctx, epoch, candidateRows)
}

func (p *budgetProbe) ObservationsBySessionWithinBudget(ctx context.Context, sessionID string,
	limit int, maxRowBytes, maxTotalBytes int64) ([]analytics.ObservationRecord, bool, error) {
	p.loads++
	p.t.Errorf("the rows were loaded even though the session measured over the budget; " +
		"the refusal has to happen BEFORE this call, because this call is the cost")
	return p.real.ObservationsBySessionWithinBudget(ctx, sessionID, limit, maxRowBytes, maxTotalBytes)
}

// TestASessionOverTheByteBudgetIsRefusedBeforeItIsLoaded closes a bound that
// counted facts but not their size.
//
// analytics enforces MaxObservationPayloadBytes in the WRITER. That bounds a
// store the writer filled and bounds nothing in a tampered or foreign database
// file — the input a reader most needs to bound. The row count alone admits
// DefaultMaxRecords x 64 KiB even when the writer's ceiling does hold, and an
// unbounded amount when it does not.
func TestASessionOverTheByteBudgetIsRefusedBeforeItIsLoaded(t *testing.T) {
	ctx := context.Background()
	repo := observationStore(t)

	facts := make([]analytics.PredictionObservation, 0, 3)
	for i := 0; i < 3; i++ {
		f := baseFact(analytics.KindChannelEvent)
		f.Payload = analytics.ObservationPayload{
			Phase: "ROUND_UPDATED", RoundState: "ACTIVE",
			Counters: map[string]int64{"outcomeCount": 2},
		}
		facts = append(facts, f)
	}
	epoch, _ := seedSession(t, repo, "byte-budget", facts)

	over := &budgetProbe{
		real: repo, t: t,
		size: analytics.ObservationSessionSize{Rows: 3, TotalBytes: reader.MaxSessionPayloadBytes + 1},
	}
	ds, err := reader.LoadSession(ctx, over, epoch, reader.DefaultMaxRecords)
	if !errors.Is(err, reader.ErrPayloadBudgetExceeded) {
		t.Fatalf("LoadSession returned %d facts and err %v, want ErrPayloadBudgetExceeded", len(ds.Records), err)
	}
	if len(ds.Records) != 0 {
		t.Fatalf("the refused load still handed back %d facts", len(ds.Records))
	}
	if over.loads != 0 {
		t.Fatalf("the row load ran %d times before the refusal", over.loads)
	}

	// A session exactly AT the budget is not over it, so the guard did not
	// simply refuse everything.
	atBudget := &passThrough{
		real: repo,
		size: analytics.ObservationSessionSize{Rows: 3, TotalBytes: reader.MaxSessionPayloadBytes},
	}
	if _, err := reader.LoadSession(ctx, atBudget, epoch, reader.DefaultMaxRecords); err != nil {
		t.Fatalf("a session exactly at the budget was refused: %v", err)
	}

	// And the real store, measured for real, loads.
	real, err := reader.LoadSession(ctx, repo, epoch, reader.DefaultMaxRecords)
	if err != nil {
		t.Fatalf("the real store load failed: %v", err)
	}
	if len(real.Records) != 3 {
		t.Fatalf("read %d facts, want 3", len(real.Records))
	}
}

// passThrough reports a chosen size and otherwise defers to the real store.
type passThrough struct {
	real reader.ObservationSource
	size analytics.ObservationSessionSize
}

func (p *passThrough) ReadObservationSessionWithinBudget(ctx context.Context, epoch int64,
	maxRowBytes int64) (analytics.ObservationSessionReading, bool, bool, error) {
	return p.real.ReadObservationSessionWithinBudget(ctx, epoch, maxRowBytes)
}

func (p *passThrough) ObservationSessionSizeBySession(ctx context.Context, sessionID string,
	candidateRows int) (analytics.ObservationSessionSize, error) {
	return p.size, nil
}

func (p *passThrough) ObservationEpochRowBounds(ctx context.Context, epoch int64,
	candidateRows int) (analytics.ObservationEpochSize, error) {
	return p.real.ObservationEpochRowBounds(ctx, epoch, candidateRows)
}

func (p *passThrough) ObservationsBySessionWithinBudget(ctx context.Context, sessionID string,
	limit int, maxRowBytes, maxTotalBytes int64) ([]analytics.ObservationRecord, bool, error) {
	return p.real.ObservationsBySessionWithinBudget(ctx, sessionID, limit, maxRowBytes, maxTotalBytes)
}

// TestTheMeasuredSizeIsBytesAndNotCharacters pins the CAST in the measurement.
//
// SQLite's LENGTH counts CHARACTERS on a TEXT value. A payload of multi-byte
// runes would then measure far smaller than the memory it occupies, and a
// budget computed from it would under-count in exactly the case a byte budget
// exists for. LENGTH(CAST(payload_json AS BLOB)) counts bytes.
//
// The test is DIFFERENTIAL because an absolute assertion cannot tell the two
// apart: two payloads identical except for a reason code of the same RUNE
// length measure the same under a character count, and differ by exactly the
// multi-byte overhead under a byte count.
func TestTheMeasuredSizeIsBytesAndNotCharacters(t *testing.T) {
	ctx := context.Background()
	repo := observationStore(t)

	const ascii = "abcdefg"    // 7 runes, 7 bytes
	const cyrillic = "круглый" // 7 runes, 14 bytes

	if len([]rune(ascii)) != len([]rune(cyrillic)) {
		t.Fatalf("the fixtures differ in rune length (%d vs %d), so a character count would "+
			"already separate them and the test would prove nothing",
			len([]rune(ascii)), len([]rune(cyrillic)))
	}
	extra := int64(len(cyrillic) - len(ascii))
	if extra <= 0 {
		t.Fatalf("the fixture is not multi-byte: %q is %d bytes", cyrillic, len(cyrillic))
	}

	measure := func(label, reason string) analytics.ObservationSessionSize {
		t.Helper()
		f := baseFact(analytics.KindChannelEvent)
		f.Payload = analytics.ObservationPayload{
			Phase: "ROUND_UPDATED", RoundState: "ACTIVE", ReasonCode: reason,
		}
		_, sessionID := seedSession(t, repo, label, []analytics.PredictionObservation{f})
		size, err := repo.ObservationSessionSizeBySession(ctx, sessionID, reader.DefaultMaxRecords)
		if err != nil {
			t.Fatalf("measure %s: %v", label, err)
		}
		if size.Rows != 1 {
			t.Fatalf("%s measured %d rows, want 1", label, size.Rows)
		}
		return size
	}

	a := measure("measured-bytes-ascii", ascii)
	c := measure("measured-bytes-cyrillic", cyrillic)

	if got := c.TotalBytes - a.TotalBytes; got != extra {
		t.Fatalf("two payloads of equal RUNE length measured %d and %d bytes, a difference of "+
			"%d; want exactly %d, the multi-byte overhead. A difference of 0 means LENGTH is "+
			"counting characters and the byte budget under-counts the memory it exists to bound",
			a.TotalBytes, c.TotalBytes, got, extra)
	}
	t.Logf("equal-rune payloads measured %d and %d bytes; the %d-byte difference is the "+
		"multi-byte overhead a character count would have hidden",
		a.TotalBytes, c.TotalBytes, extra)
}

// TestTheBudgetCountsEveryColumnTheReadMaterializes closes a bound that
// measured one column and read seventeen.
//
// The budget originally summed payload_json alone. The row also carries a
// dozen TEXT columns and the schema constrains the length of NONE of them, so
// a store that kept the payload small while putting a very large value in
// pool_instance_id or source_fingerprint measured as tiny and was still
// scanned into Go strings in full — the bound reporting a number unrelated to
// the memory it was supposed to bound.
func TestTheBudgetCountsEveryColumnTheReadMaterializes(t *testing.T) {
	ctx := context.Background()
	repo := observationStore(t)

	fat := ""
	for i := 0; i < 4096; i++ {
		fat += "x"
	}

	measure := func(label string, mut func(*analytics.PredictionObservation)) analytics.ObservationSessionSize {
		t.Helper()
		f := baseFact(analytics.KindChannelEvent)
		f.Payload = analytics.ObservationPayload{Phase: "ROUND_UPDATED", RoundState: "ACTIVE"}
		mut(&f)
		_, sessionID := seedSession(t, repo, label, []analytics.PredictionObservation{f})
		size, err := repo.ObservationSessionSizeBySession(ctx, sessionID, reader.DefaultMaxRecords)
		if err != nil {
			t.Fatalf("measure %s: %v", label, err)
		}
		return size
	}

	plain := measure("width-plain", func(*analytics.PredictionObservation) {})

	for _, tc := range []struct {
		name string
		mut  func(*analytics.PredictionObservation)
	}{
		{"pool_instance_id", func(f *analytics.PredictionObservation) { f.PoolInstanceID = fat }},
		{"source_fingerprint", func(f *analytics.PredictionObservation) { f.SourceFingerprint = fat }},
		{"event_id", func(f *analytics.PredictionObservation) { f.EventID = fat }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := measure("width-"+tc.name, tc.mut)
			grew := got.TotalBytes - plain.TotalBytes
			// The inflated value REPLACES a shorter one, so the growth is the
			// new length minus the old — a few bytes under len(fat), never
			// equal to it. The discriminating fact is not the exact number but
			// its magnitude: a column the budget does not count moves the
			// measurement by exactly 0.
			if grew < int64(len(fat))/2 {
				t.Fatalf("inflating %s by %d bytes moved the measured width by only %d. "+
					"A column the read materializes but the budget does not count is a column "+
					"an oversized value can hide in", tc.name, len(fat), grew)
			}
			if grew > int64(len(fat)) {
				t.Fatalf("inflating %s by %d bytes moved the measured width by %d, which is "+
					"more than the value added; the width expression is counting something twice",
					tc.name, len(fat), grew)
			}
			if got.WidestRowBytes < int64(len(fat)) {
				t.Fatalf("the widest-row measure was %d for a row carrying a %d-byte value",
					got.WidestRowBytes, len(fat))
			}
		})
	}
}

// TestTheBoundTravelsWithTheReadNotWithAnEarlierMeasurement closes a
// time-of-check/time-of-use gap.
//
// The budget was checked by one query and the rows read by another. Between
// them another connection can enlarge a value: the ROW COUNT is unchanged so a
// count re-check still passes, and the enlarged value is materialized having
// never been covered by any bound. The bounds are therefore restated inside
// the reading statement, where they cannot be stale by the time rows are
// produced.
//
// The check here is on the reading call itself rather than on LoadSession,
// because that is the call whose guard must hold on its own.
func TestTheBoundTravelsWithTheReadNotWithAnEarlierMeasurement(t *testing.T) {
	ctx := context.Background()
	repo := observationStore(t)

	facts := make([]analytics.PredictionObservation, 0, 3)
	for i := 0; i < 3; i++ {
		f := baseFact(analytics.KindChannelEvent)
		f.Payload = analytics.ObservationPayload{Phase: "ROUND_UPDATED", RoundState: "ACTIVE"}
		facts = append(facts, f)
	}
	epoch, sessionID := seedSession(t, repo, "bound-travels", facts)

	size, err := repo.ObservationSessionSizeBySession(ctx, sessionID, reader.DefaultMaxRecords)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if size.Rows != 3 || size.TotalBytes <= 0 || size.WidestRowBytes <= 0 {
		t.Fatalf("unexpected measurement %+v", size)
	}

	// Generous bounds: the rows come back.
	rows, within, err := repo.ObservationsBySessionWithinBudget(ctx, sessionID, 10,
		reader.MaxRecordBytes, reader.MaxSessionPayloadBytes)
	if err != nil {
		t.Fatalf("bounded read: %v", err)
	}
	if !within || len(rows) != 3 {
		t.Fatalf("within=%v rows=%d, want a full read inside a generous budget", within, len(rows))
	}

	// An aggregate bound one byte under the real width: the read itself
	// refuses, with no help from the earlier measurement.
	rows, within, err = repo.ObservationsBySessionWithinBudget(ctx, sessionID, 10,
		reader.MaxRecordBytes, size.TotalBytes-1)
	if err != nil {
		t.Fatalf("bounded read: %v", err)
	}
	if within || len(rows) != 0 {
		t.Fatalf("within=%v rows=%d; the aggregate bound was not enforced by the reading "+
			"statement, so an enlargement after the measurement would be materialized",
			within, len(rows))
	}

	// A per-row bound one byte under the widest row. The aggregate alone
	// cannot stand in for this: a read aborted on exceeding a running total
	// has already materialized the row that exceeded it.
	rows, within, err = repo.ObservationsBySessionWithinBudget(ctx, sessionID, 10,
		size.WidestRowBytes-1, reader.MaxSessionPayloadBytes)
	if err != nil {
		t.Fatalf("bounded read: %v", err)
	}
	if within || len(rows) != 0 {
		t.Fatalf("within=%v rows=%d; the per-row bound was not enforced", within, len(rows))
	}

	// And the whole path still works end to end.
	ds, err := reader.LoadSession(ctx, repo, epoch, reader.DefaultMaxRecords)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(ds.Records) != 3 {
		t.Fatalf("read %d facts, want 3", len(ds.Records))
	}
}

// TestAnOversizedValueInAnIntegerColumnIsCounted closes a bypass created by
// trusting a column's declared type.
//
// prediction_observations is not a STRICT table, and outside a STRICT table
// SQLite treats a declared type as an AFFINITY, not a constraint: an
// INTEGER-affinity column such as received_at_ms will hold an arbitrarily
// large TEXT or BLOB. The width expression used to skip those columns on the
// stated grounds that "their width is bounded by their type", which measured
// a 300 KB value as 2 bytes and handed it across the driver before Scan
// rejected its type — past both the per-row and the session bound.
//
// Written against the store directly, because the repository's own writer
// cannot produce this row: only a tampered or foreign database file can, and
// that is precisely the input the bound exists for.
func TestAnOversizedValueInAnIntegerColumnIsCounted(t *testing.T) {
	ctx := context.Background()
	repo := observationStore(t)

	f := baseFact(analytics.KindChannelEvent)
	f.Payload = analytics.ObservationPayload{Phase: "ROUND_UPDATED", RoundState: "ACTIVE"}
	_, sessionID := seedSession(t, repo, "integer-affinity", []analytics.PredictionObservation{f})

	before, err := repo.ObservationSessionSizeBySession(ctx, sessionID, reader.DefaultMaxRecords)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}

	// Tamper: put a large TEXT value into an INTEGER-affinity column.
	db, err := database.Open(observationTestDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	fat := strings.Repeat("A", 300_000)
	if _, err := db.ExecContext(ctx,
		`UPDATE prediction_observations SET received_at_ms = ? WHERE collector_session_id = ?`,
		fat, sessionID); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	var storedType string
	if err := db.QueryRowContext(ctx,
		`SELECT typeof(received_at_ms) FROM prediction_observations WHERE collector_session_id = ?`,
		sessionID).Scan(&storedType); err != nil {
		t.Fatalf("typeof: %v", err)
	}
	if storedType != "text" {
		t.Fatalf("the INTEGER-affinity column stored a %q, not a text value; this SQLite build "+
			"appears to enforce affinity and the scenario under test cannot occur", storedType)
	}

	after, err := repo.ObservationSessionSizeBySession(ctx, sessionID, reader.DefaultMaxRecords)
	if err != nil {
		t.Fatalf("measure after tamper: %v", err)
	}

	grew := after.TotalBytes - before.TotalBytes
	if grew < 250_000 {
		t.Fatalf("a %d-byte value hidden in an INTEGER-affinity column moved the measured "+
			"width by only %d bytes (%d -> %d). A column excused from measurement because of "+
			"its declared type is a place for an oversized value to hide, because this table "+
			"is not STRICT and the declared type constrains nothing.",
			len(fat), grew, before.TotalBytes, after.TotalBytes)
	}
	if after.WidestRowBytes < 250_000 {
		t.Fatalf("the widest-row measure was %d for a row carrying a %d-byte value",
			after.WidestRowBytes, len(fat))
	}

	// And the bound now actually refuses it, in the statement that reads.
	rows, within, err := repo.ObservationsBySessionWithinBudget(ctx, sessionID, 10,
		200_000, reader.MaxSessionPayloadBytes)
	if err != nil {
		t.Fatalf("bounded read: %v", err)
	}
	if within || len(rows) != 0 {
		t.Fatalf("within=%v rows=%d; the per-row bound did not refuse a row whose oversized "+
			"value sits in an INTEGER-affinity column", within, len(rows))
	}
}

// TestOversizedSessionMetadataIsRefusedBeforeItIsScanned closes the earliest
// unbounded allocation in the load.
//
// prediction_observation_sessions is not STRICT either, so no column of that
// row is bounded by its declared type and none carries a length constraint.
// The session read scans every one of them into Go values and it runs FIRST —
// before any observation-row bound applies — so bounding the facts while
// reading their session metadata unguarded left the first allocation the only
// unbounded one.
//
// The bound travels IN the reading statement rather than in a measurement
// taken beforehand, and the two are not interchangeable: a separate measuring
// statement leaves a window another connection can commit into, after which
// the read materializes a value no bound ever covered. The refusal here is
// therefore observed through the read itself — an oversized row produces NO
// row, not a scanned one.
func TestOversizedSessionMetadataIsRefusedBeforeItIsScanned(t *testing.T) {
	ctx := context.Background()
	repo := observationStore(t)

	f := baseFact(analytics.KindChannelEvent)
	f.Payload = analytics.ObservationPayload{Phase: "ROUND_UPDATED", RoundState: "ACTIVE"}
	epoch, _ := seedSession(t, repo, "session-meta", []analytics.PredictionObservation{f})

	// The clean session loads, so the refusal below is not refusing everything.
	if _, err := reader.LoadSession(ctx, repo, epoch, reader.DefaultMaxRecords); err != nil {
		t.Fatalf("the clean session failed to load: %v", err)
	}

	db, err := database.Open(observationTestDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	fat := strings.Repeat("R", 200_000)
	if _, err := db.ExecContext(ctx,
		`UPDATE prediction_observation_sessions SET producer_revision = ? WHERE collector_epoch = ?`,
		fat, epoch); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	// The bounded read refuses the row and says WHY: the session exists, it
	// just does not fit. "No row" and "a row too wide to read" are different
	// answers and the caller has to be able to tell them apart.
	rd, found, within, err := repo.ReadObservationSessionWithinBudget(ctx, epoch, reader.MaxSessionMetaBytes)
	if err != nil {
		t.Fatalf("bounded session read: %v", err)
	}
	if within {
		t.Fatalf("the bounded read accepted a session row carrying a %d-byte revision "+
			"under a %d-byte bound", len(fat), reader.MaxSessionMetaBytes)
	}
	if found || rd.Session.ProducerRevision != "" {
		t.Fatalf("the refused read still handed back the row (found=%v revision=%d bytes); "+
			"a bound that returns the value it refused has not bounded anything",
			found, len(rd.Session.ProducerRevision))
	}
	// And the same row IS readable without the bound, so the refusal above is
	// the bound acting and not the row being unreadable.
	if _, found, _, err := repo.ReadObservationSessionWithinBudget(ctx, epoch, 0); err != nil || !found {
		t.Fatalf("unbounded read of the same row: found=%v err=%v", found, err)
	}

	ds, err := reader.LoadSession(ctx, repo, epoch, reader.DefaultMaxRecords)
	if !errors.Is(err, reader.ErrSessionMetaTooLarge) {
		t.Fatalf("LoadSession returned %d facts and err %v, want ErrSessionMetaTooLarge. "+
			"The session row is read before any fact bound applies, so it needs a bound of "+
			"its own", len(ds.Records), err)
	}
	if len(ds.Records) != 0 {
		t.Fatalf("the refused load still handed back %d facts", len(ds.Records))
	}
}

// TestTheSizeProbeOnlyAggregatesABoundedCandidateSet closes unbounded database
// work behind a bounded allocation.
//
// The measurement used to aggregate every row of the session before LoadSession
// compared the count with its limit. A store assigning millions of small rows
// to one session therefore spent unbounded database CPU and I/O to produce a
// number whose only use was to refuse the load: the returned allocation was
// bounded and the work to reach it was not.
func TestTheSizeProbeOnlyAggregatesABoundedCandidateSet(t *testing.T) {
	ctx := context.Background()
	repo := observationStore(t)

	const facts = 12
	seeded := make([]analytics.PredictionObservation, 0, facts)
	for i := 0; i < facts; i++ {
		f := baseFact(analytics.KindChannelEvent)
		f.Payload = analytics.ObservationPayload{Phase: "ROUND_UPDATED", RoundState: "ACTIVE"}
		seeded = append(seeded, f)
	}
	_, sessionID := seedSession(t, repo, "bounded-probe", seeded)

	// Asked for the whole session, the probe sees all of it.
	full, err := repo.ObservationSessionSizeBySession(ctx, sessionID, facts+1)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if full.Rows != facts {
		t.Fatalf("measured %d rows, want %d", full.Rows, facts)
	}

	// Asked for a smaller candidate set, it stops there — which is what makes
	// the work bounded rather than the answer complete.
	const candidates = 5
	bounded, err := repo.ObservationSessionSizeBySession(ctx, sessionID, candidates)
	if err != nil {
		t.Fatalf("measure bounded: %v", err)
	}
	if bounded.Rows != candidates {
		t.Fatalf("a %d-row candidate bound measured %d rows; the aggregate is still running "+
			"over the whole session, so a store with millions of rows would pay for all of "+
			"them before the row limit was consulted", candidates, bounded.Rows)
	}
	if bounded.TotalBytes >= full.TotalBytes {
		t.Fatalf("the bounded aggregate summed %d bytes and the full one %d; the bound is not "+
			"restricting the rows the aggregate visits", bounded.TotalBytes, full.TotalBytes)
	}

	// A candidate bound of limit+1 still distinguishes AT the limit from OVER
	// it, which is the property LoadSession relies on.
	atLimit, err := repo.ObservationSessionSizeBySession(ctx, sessionID, facts-1+1)
	if err != nil {
		t.Fatalf("measure at limit: %v", err)
	}
	if atLimit.Rows != facts {
		t.Fatalf("with limit %d the probe saw %d rows, want %d so that an over-limit session "+
			"is still detectable", facts-1, atLimit.Rows, facts)
	}

	// A bound of zero or less is refused rather than silently unbounded.
	if _, err := repo.ObservationSessionSizeBySession(ctx, sessionID, 0); err == nil {
		t.Fatal("a candidate bound of 0 was accepted; a measurement with no bound defeats " +
			"the point of asking for one")
	}
}

// TestOversizedSessionMetadataIsCountedInEveryColumn closes the same
// affinity hole on the SESSION row that the fact row already had.
//
// The first version of the session bound measured three columns —
// collector_session_id, producer_revision and close_state, the ones that look
// like text — while ReadObservationSession scans twelve. The other nine are
// INTEGER-affinity, and prediction_observation_sessions is not STRICT either,
// so they hold arbitrarily large TEXT.
//
// The CHECK (col >= 0) constraints on five of them do not help. SQLite ranks
// the TEXT storage class above INTEGER, so comparing a TEXT value against the
// integer literal 0 with >= is true whatever the text contains. Measured
// directly: inserting a 250 KB string into an INTEGER NOT NULL CHECK (col >= 0)
// column succeeds and reports typeof() = "text".
func TestOversizedSessionMetadataIsCountedInEveryColumn(t *testing.T) {
	ctx := context.Background()
	repo := observationStore(t)

	db, err := database.Open(observationTestDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	fat := strings.Repeat("Z", 250_000)

	for _, column := range []string{
		// Carries CHECK (col >= 0) — which a TEXT value satisfies.
		"committed_count",
		"dropped_count",
		"unsettled_obligation_count",
		"post_fence_producer_count",
		"producer_shutdown_uncertain_count",
		// No CHECK at all.
		"started_at_ms",
		"closed_at_ms",
		"last_assigned_sequence",
	} {
		t.Run(column, func(t *testing.T) {
			f := baseFact(analytics.KindChannelEvent)
			f.Payload = analytics.ObservationPayload{Phase: "ROUND_UPDATED", RoundState: "ACTIVE"}
			epoch, _ := seedSession(t, repo, "meta-col-"+column,
				[]analytics.PredictionObservation{f})

			// The clean row reads under the bound, so a refusal below is the
			// tampering and not the fixture.
			if _, found, within, err := repo.ReadObservationSessionWithinBudget(
				ctx, epoch, reader.MaxSessionMetaBytes); err != nil || !found || !within {
				t.Fatalf("the clean session row was not readable under the bound: "+
					"found=%v within=%v err=%v", found, within, err)
			}

			if _, err := db.ExecContext(ctx,
				`UPDATE prediction_observation_sessions SET `+column+
					` = ? WHERE collector_epoch = ?`, fat, epoch); err != nil {
				t.Skipf("this build refused a TEXT value in %s (%v); the scenario under "+
					"test cannot occur here", column, err)
			}

			var storedType string
			if err := db.QueryRowContext(ctx,
				`SELECT typeof(`+column+`) FROM prediction_observation_sessions `+
					`WHERE collector_epoch = ?`, epoch).Scan(&storedType); err != nil {
				t.Fatalf("typeof: %v", err)
			}
			if storedType != "text" {
				t.Fatalf("%s stored a %q rather than text; affinity appears to be enforced "+
					"here and the scenario under test cannot occur", column, storedType)
			}

			// The predicate in the reading statement now refuses the row, and
			// it can only do that if the width expression measures this
			// column: the read scans every one of the twelve, so a column
			// left out of the expression is a place for an oversized value to
			// hide from the very statement that materializes it.
			_, found, within, err := repo.ReadObservationSessionWithinBudget(
				ctx, epoch, reader.MaxSessionMetaBytes)
			if err != nil {
				t.Fatalf("bounded read after tamper: %v", err)
			}
			if within || found {
				t.Fatalf("a %d-byte value hidden in %s was read under a %d-byte bound "+
					"(found=%v within=%v)", len(fat), column,
					reader.MaxSessionMetaBytes, found, within)
			}

			ds, err := reader.LoadSession(ctx, repo, epoch, reader.DefaultMaxRecords)
			if !errors.Is(err, reader.ErrSessionMetaTooLarge) {
				t.Fatalf("LoadSession returned %d facts and err %v, want ErrSessionMetaTooLarge",
					len(ds.Records), err)
			}
		})
	}
}

// sweepProbe defers to the real store but fails the test if
// ReadObservationSession is reached.
//
// That call is the cost being bounded, not a neutral step before it: inside
// its transaction it recomputes the stored witness of a bounded prefix of the
// session's facts, which scans payload_json and a dozen identifier columns of
// each row into memory. Asserting only on the error LoadSession returns would
// pass just as happily with the refusal in the wrong place, because the
// refusal that matters is the one that happens before this call.
type sweepProbe struct {
	real  reader.ObservationSource
	t     *testing.T
	reads int
}

func (p *sweepProbe) ReadObservationSessionWithinBudget(ctx context.Context, epoch int64,
	maxRowBytes int64) (analytics.ObservationSessionReading, bool, bool, error) {
	p.reads++
	p.t.Errorf("the session was read even though the epoch holds a row wider than " +
		"MaxRecordBytes; this call verifies stored witnesses over the session's facts and " +
		"materializes each row to do it, so the refusal has to happen BEFORE it")
	return p.real.ReadObservationSessionWithinBudget(ctx, epoch, maxRowBytes)
}

func (p *sweepProbe) ObservationSessionSizeBySession(ctx context.Context, sessionID string,
	candidateRows int) (analytics.ObservationSessionSize, error) {
	return p.real.ObservationSessionSizeBySession(ctx, sessionID, candidateRows)
}

func (p *sweepProbe) ObservationEpochRowBounds(ctx context.Context, epoch int64,
	candidateRows int) (analytics.ObservationEpochSize, error) {
	return p.real.ObservationEpochRowBounds(ctx, epoch, candidateRows)
}

func (p *sweepProbe) ObservationsBySessionWithinBudget(ctx context.Context, sessionID string,
	limit int, maxRowBytes, maxTotalBytes int64) ([]analytics.ObservationRecord, bool, error) {
	return p.real.ObservationsBySessionWithinBudget(ctx, sessionID, limit, maxRowBytes, maxTotalBytes)
}

// TestAnOversizedFactIsRefusedBeforeTheWitnessSweepReadsIt closes the last
// unbounded allocation in the load, and the one that hid behind the others.
//
// Every bound this file already pins is keyed on the SESSION id, and
// LoadSession learns the session id from ReadObservationSession. So they all
// applied strictly after that call — and that call is not only a session-row
// read. Inside the same transaction it recomputes the stored witness of a
// bounded prefix of the session's facts, scanning payload_json and a dozen
// other columns of each row into memory one row at a time.
//
// Measured before the fix, on a payload tampered to 4 MiB, four times
// MaxRecordBytes:
//
//	ReadObservationSession: reading="INTEGRITY_ERROR" verified=1
//	LoadSession:            records=0 err=<nil>
//
// The load reported nothing wrong and returned no facts, having already
// scanned and hashed the whole 4 MiB. Only a bound that runs BEFORE the
// session id is known can close that, which is why the measurement is keyed on
// the epoch instead.
//
// The row is tampered rather than written, because the repository's own writer
// enforces MaxObservationPayloadBytes and cannot produce it. A tampered or
// foreign database file can, and is the input the bound exists for.
func TestAnOversizedFactIsRefusedBeforeTheWitnessSweepReadsIt(t *testing.T) {
	ctx := context.Background()
	repo := observationStore(t)

	f := baseFact(analytics.KindChannelEvent)
	f.Payload = analytics.ObservationPayload{Phase: "ROUND_UPDATED", RoundState: "ACTIVE"}
	epoch, sessionID := seedSession(t, repo, "witness-sweep", []analytics.PredictionObservation{f})

	// The clean session loads, so the refusal below is not refusing everything.
	if _, err := reader.LoadSession(ctx, repo, epoch, reader.DefaultMaxRecords); err != nil {
		t.Fatalf("the clean session failed to load: %v", err)
	}

	db, err := database.Open(observationTestDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	fat := `{"phase":"ROUND_UPDATED","pad":"` + strings.Repeat("A", 4<<20) + `"}`
	if int64(len(fat)) <= reader.MaxRecordBytes {
		t.Fatalf("the tampered payload is %d bytes, which is within MaxRecordBytes (%d); "+
			"this test would then prove nothing", len(fat), reader.MaxRecordBytes)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE prediction_observations SET payload_json = ? WHERE collector_session_id = ?`,
		fat, sessionID); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	// The measurement sees it, keyed on the epoch alone.
	size, err := repo.ObservationEpochRowBounds(ctx, epoch, reader.DefaultMaxRecords)
	if err != nil {
		t.Fatalf("measure epoch rows: %v", err)
	}
	if size.WidestRowBytes <= reader.MaxRecordBytes {
		t.Fatalf("the epoch measured a widest row of %d bytes while holding a %d-byte payload",
			size.WidestRowBytes, len(fat))
	}

	probe := &sweepProbe{real: repo, t: t}
	ds, err := reader.LoadSession(ctx, probe, epoch, reader.DefaultMaxRecords)
	if !errors.Is(err, reader.ErrRecordTooLarge) {
		t.Fatalf("LoadSession returned %d facts and err %v, want ErrRecordTooLarge",
			len(ds.Records), err)
	}
	if len(ds.Records) != 0 {
		t.Fatalf("the refused load still handed back %d facts", len(ds.Records))
	}
	if probe.reads != 0 {
		t.Fatalf("ReadObservationSession ran %d times before the refusal", probe.reads)
	}
}

// TestAnEpochThatFillsTheMeasurementWindowIsRefusedRatherThanTrusted pins what
// makes the bounded window sound.
//
// The epoch measurement scans at most limit+1 rows, so its answer describes the
// EPOCH only when the window did not truncate. If it did, the widest-row figure
// covers a prefix, and reading on would mean applying a per-row bound derived
// from rows nobody measured — the exact shape of "bounded because we stopped
// looking". So a count that reaches the window is a refusal, not a fallback.
//
// The two properties only hold together: unbounded work if the window is
// removed, an unsound bound if the refusal is.
func TestAnEpochThatFillsTheMeasurementWindowIsRefusedRatherThanTrusted(t *testing.T) {
	ctx := context.Background()
	repo := observationStore(t)

	const facts = 6
	seeded := make([]analytics.PredictionObservation, 0, facts)
	for i := 0; i < facts; i++ {
		f := baseFact(analytics.KindChannelEvent)
		f.Payload = analytics.ObservationPayload{Phase: "ROUND_UPDATED", RoundState: "ACTIVE"}
		seeded = append(seeded, f)
	}
	epoch, _ := seedSession(t, repo, "window-truncation", seeded)

	// At the bound the window holds all six and one to spare, so the load runs.
	if _, err := reader.LoadSession(ctx, repo, epoch, facts); err != nil {
		t.Fatalf("a session exactly at the bound failed to load: %v", err)
	}

	// One under it, the window truncates and the load is refused rather than
	// proceeding on a measurement of a prefix.
	probe := &sweepProbe{real: repo, t: t}
	ds, err := reader.LoadSession(ctx, probe, epoch, facts-1)
	if !errors.Is(err, reader.ErrLimitExceeded) {
		t.Fatalf("LoadSession returned %d facts and err %v, want ErrLimitExceeded",
			len(ds.Records), err)
	}
	if probe.reads != 0 {
		t.Fatalf("ReadObservationSession ran %d times before the refusal", probe.reads)
	}

	// And the measurement itself refuses a window that is no window.
	if _, err := repo.ObservationEpochRowBounds(ctx, epoch, 0); err == nil {
		t.Fatal("a candidate bound of zero was accepted; a measurement with no bound is " +
			"the unbounded scan the window exists to prevent")
	}
}

// growingSource enlarges the session row while the load is between its two
// readings of it, by tampering during the fact read that sits in between.
type growingSource struct {
	real reader.ObservationSource
	t    *testing.T
	fire func()
}

// ReadObservationSessionWithinBudget also asserts the property the co-located
// bound exists for, on EVERY session read the load performs.
//
// Asserting only on the error LoadSession returns cannot do that. The load
// reads the session row twice and compares the two, so an unbounded re-read
// still reports ErrSnapshotIncoherent — by scanning the enlarged row and
// noticing it differs, which is the outcome with the cost still paid. The
// bound has to be observed travelling with each read, not inferred from the
// verdict.
func (g *growingSource) ReadObservationSessionWithinBudget(ctx context.Context, epoch int64,
	maxRowBytes int64) (analytics.ObservationSessionReading, bool, bool, error) {
	if maxRowBytes <= 0 {
		g.t.Errorf("the load read the session row with maxRowBytes=%d, which is no bound at "+
			"all. Both readings of that row have to carry it: an unbounded re-read still "+
			"reports the snapshot changed, but only by scanning the row it should have "+
			"refused.", maxRowBytes)
	}
	return g.real.ReadObservationSessionWithinBudget(ctx, epoch, maxRowBytes)
}

func (g *growingSource) ObservationSessionSizeBySession(ctx context.Context, sessionID string,
	candidateRows int) (analytics.ObservationSessionSize, error) {
	return g.real.ObservationSessionSizeBySession(ctx, sessionID, candidateRows)
}

func (g *growingSource) ObservationEpochRowBounds(ctx context.Context, epoch int64,
	candidateRows int) (analytics.ObservationEpochSize, error) {
	return g.real.ObservationEpochRowBounds(ctx, epoch, candidateRows)
}

func (g *growingSource) ObservationsBySessionWithinBudget(ctx context.Context, sessionID string,
	limit int, maxRowBytes, maxTotalBytes int64) ([]analytics.ObservationRecord, bool, error) {
	rows, within, err := g.real.ObservationsBySessionWithinBudget(ctx, sessionID, limit,
		maxRowBytes, maxTotalBytes)
	g.fire()
	return rows, within, err
}

// TestASessionRowThatGrowsPastTheBoundMidLoadIsNeverScanned is what the
// co-located bound buys over a measurement taken beforehand.
//
// The session row is read TWICE — once for the classification, once to prove
// the snapshot did not span two committed states — and the facts are read in
// between. A width checked by an earlier, separate statement covers neither
// read: another connection commits into the gap, and the second read then
// transfers the enlarged value across the driver having never been bounded.
// The coherence comparison would notice the session changed, but only after
// paying for it, which is the wrong order for a bound.
//
// Here the predicate lives in the statement that selects and scans the row, so
// the enlarged row yields NO row at all. The load still reports the store
// changing underneath it — that fact is not lost — but it reports it without
// materializing what it refused.
func TestASessionRowThatGrowsPastTheBoundMidLoadIsNeverScanned(t *testing.T) {
	ctx := context.Background()
	repo := observationStore(t)

	f := baseFact(analytics.KindChannelEvent)
	f.Payload = analytics.ObservationPayload{Phase: "ROUND_UPDATED", RoundState: "ACTIVE"}
	epoch, _ := seedSession(t, repo, "grows-mid-load", []analytics.PredictionObservation{f})

	// Undisturbed, the same source loads. Whatever the run below reports is
	// therefore the tampering and not the fixture.
	quiet := &growingSource{real: repo, t: t, fire: func() {}}
	if _, err := reader.LoadSession(ctx, quiet, epoch, reader.DefaultMaxRecords); err != nil {
		t.Fatalf("the undisturbed load failed: %v", err)
	}

	db, err := database.Open(observationTestDir)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	fat := strings.Repeat("G", 200_000)
	fired := 0
	src := &growingSource{real: repo, t: t, fire: func() {
		fired++
		if _, err := db.ExecContext(ctx,
			`UPDATE prediction_observation_sessions SET producer_revision = ? WHERE collector_epoch = ?`,
			fat, epoch); err != nil {
			t.Errorf("tamper mid-load: %v", err)
		}
	}}

	ds, err := reader.LoadSession(ctx, src, epoch, reader.DefaultMaxRecords)
	if fired == 0 {
		t.Fatal("the load never reached the fact read, so nothing was tampered and this " +
			"test proves nothing")
	}
	if !errors.Is(err, reader.ErrSnapshotIncoherent) {
		t.Fatalf("LoadSession returned %d facts and err %v, want ErrSnapshotIncoherent",
			len(ds.Records), err)
	}
	if len(ds.Records) != 0 {
		t.Fatalf("the refused load still handed back %d facts", len(ds.Records))
	}

	// And the row that grew is genuinely past the bound, so the refusal above
	// is the width predicate acting rather than the values merely differing.
	rd, found, within, err := repo.ReadObservationSessionWithinBudget(
		ctx, epoch, reader.MaxSessionMetaBytes)
	if err != nil {
		t.Fatalf("bounded read after the load: %v", err)
	}
	if within || found || rd.Session.ProducerRevision != "" {
		t.Fatalf("the grown row still read under the bound (found=%v within=%v revision=%d bytes)",
			found, within, len(rd.Session.ProducerRevision))
	}
}
