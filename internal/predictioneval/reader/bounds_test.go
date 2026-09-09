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

func (p *budgetProbe) ObservationsBySession(ctx context.Context, sessionID string, limit int) ([]analytics.ObservationRecord, error) {
	p.loads++
	p.t.Errorf("the rows were loaded even though the session measured over the budget; " +
		"the refusal has to happen BEFORE this call, because this call is the cost")
	return p.real.ObservationsBySession(ctx, sessionID, limit)
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
		size: analytics.ObservationSessionSize{Rows: 3, PayloadBytes: reader.MaxSessionPayloadBytes + 1},
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
		size: analytics.ObservationSessionSize{Rows: 3, PayloadBytes: reader.MaxSessionPayloadBytes},
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

func (p *passThrough) ObservationsBySession(ctx context.Context, sessionID string, limit int) ([]analytics.ObservationRecord, error) {
	return p.real.ObservationsBySession(ctx, sessionID, limit)
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

	if got := c.PayloadBytes - a.PayloadBytes; got != extra {
		t.Fatalf("two payloads of equal RUNE length measured %d and %d bytes, a difference of "+
			"%d; want exactly %d, the multi-byte overhead. A difference of 0 means LENGTH is "+
			"counting characters and the byte budget under-counts the memory it exists to bound",
			a.PayloadBytes, c.PayloadBytes, got, extra)
	}
	t.Logf("equal-rune payloads measured %d and %d bytes; the %d-byte difference is the "+
		"multi-byte overhead a character count would have hidden",
		a.PayloadBytes, c.PayloadBytes, extra)
}
