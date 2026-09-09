package reader_test

// The reader's bound has to survive the arithmetic that implements it.

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
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

func (p *budgetProbe) ReadObservationSession(ctx context.Context, epoch int64) (analytics.ObservationSessionReading, bool, error) {
	return p.real.ReadObservationSession(ctx, epoch)
}

func (p *budgetProbe) ObservationSessionSizeBySession(ctx context.Context, sessionID string) (analytics.ObservationSessionSize, error) {
	return p.size, nil
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

func (p *passThrough) ReadObservationSession(ctx context.Context, epoch int64) (analytics.ObservationSessionReading, bool, error) {
	return p.real.ReadObservationSession(ctx, epoch)
}

func (p *passThrough) ObservationSessionSizeBySession(ctx context.Context, sessionID string) (analytics.ObservationSessionSize, error) {
	return p.size, nil
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
		size, err := repo.ObservationSessionSizeBySession(ctx, sessionID)
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
		size, err := repo.ObservationSessionSizeBySession(ctx, sessionID)
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

	size, err := repo.ObservationSessionSizeBySession(ctx, sessionID)
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
