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
