// Package reader acquires persisted observation facts for offline replay.
//
// It is the ONLY part of the replay stack that touches SQLite, and it exists
// so that the four core stages in
// [github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval] can
// be pure functions of their arguments. Everything here is read-only: no
// migration, no write, no schema change, no runtime wiring, and nothing that
// could alter what the miner does.
//
// It also does not re-derive the store's integrity checks. Row digests and
// session witnesses are recomputed by the store itself, inside the transaction
// that reads them, from column values this package could not reconstruct — the
// reader projection COALESCEs NULL parent ids to 0 and unmarshals payload_json,
// while the digest distinguishes NULL from 0 and covers the payload's exact
// bytes. So the verdict is CONSUMED from
// (*analytics.SQLiteRepository).ReadObservationSession, never re-implemented.
package reader

import (
	"context"
	"errors"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/predictioneval"
)

// DefaultMaxRecords bounds one load. An unbounded read of an observation
// store is not a replay input, it is an availability problem: the store is
// designed to grow to its retention ceiling, and a caller that wants more than
// this should ask for it explicitly and know why.
const DefaultMaxRecords = 20000

// MaxLoadLimit is the largest bound LoadSession will honour.
//
// It exists because the bound has to survive the arithmetic that implements it.
// LoadSession probes with limit+1 so a session AT the bound stays
// distinguishable from one over it; at the platform maximum that addition wraps
// negative, and the store applies its SQL LIMIT only when the value is
// positive — so the whole session got scanned, decoded and allocated, and the
// len(rows) > limit guard could not fire either, because no slice length
// exceeds MaxInt. A caller asking for the largest possible bound received no
// bound at all.
//
// The ceiling is far above any real collector session and far below the
// overflow, so it costs a legitimate caller nothing and closes the hole for
// every value, not just the exact maximum.
const MaxLoadLimit = 1 << 20

// MaxSessionPayloadBytes bounds the AGGREGATE width of one load, and
// MaxRecordBytes bounds any single row within it. Both are enforced before a
// value is materialized.
//
// The row count is not a memory bound on its own. analytics enforces
// MaxObservationPayloadBytes (64 KiB) in the WRITER, which is the right place
// for it while the store is the only thing filling the table and no bound at
// all against a tampered or foreign database file — the exact input a reader
// most needs to bound. Under that ceiling DefaultMaxRecords already admits
// 20000 x 64 KiB = 1.25 GiB, and without it a single row is unbounded, so the
// row count permits an allocation neither this package nor its caller chose.
//
// The two bounds are not redundant. The aggregate bounds what the caller ends
// up holding; the per-row bound covers what an aggregate cannot, because a
// read aborted on exceeding a running total has already materialized the row
// that exceeded it. And neither covers only payload_json: the row carries a
// dozen other TEXT columns whose length the schema does not constrain, so a
// store keeping the payload small while inflating pool_instance_id or
// source_fingerprint would measure as tiny and still be read in full. The
// width both bounds are computed from is every variable-width column the read
// materializes.
//
// 128 MiB is far above any real collector session (a 20000-fact session of
// typical envelopes is a few tens of megabytes) and far below what a refusal
// is supposed to prevent; 1 MiB per row is far above the writer's own 64 KiB
// payload ceiling plus its identifiers. A session over either is refused, not
// truncated: a prefix would look exactly like a complete dataset downstream.
const (
	MaxSessionPayloadBytes = 128 << 20
	MaxRecordBytes         = 1 << 20
	// MaxSessionMetaBytes bounds the SESSION ROW itself.
	//
	// prediction_observation_sessions is not STRICT either, so no column of
	// that row is bounded by its declared type and none carries a length
	// constraint. That row is read FIRST, so bounding the facts while scanning
	// their session metadata unguarded would leave the earliest allocation of
	// the whole load the only unbounded one. 64 KiB is orders of magnitude
	// above a session id plus a revision string and the counters beside them.
	//
	// It is applied as a predicate IN the reading statement, on BOTH readings
	// of that row — the classification and the coherence re-read. An unbounded
	// re-read would still report that the store changed, by scanning the row it
	// should have refused, which is the right verdict reached in the wrong
	// order.
	//
	// MaxRecordBytes above bounds a fact row, and it is applied TWICE for two
	// different reads: once keyed on the epoch, before ReadObservationSession
	// recomputes stored witnesses over a prefix of the session's facts, and
	// once inside the statement that reads the facts themselves. The first is
	// not redundant — the witness sweep runs before this function holds a
	// session id, so nothing keyed on the session could reach it — and it is
	// the one bound here that is a separate statement from the read it guards,
	// because ReadObservationSession is code this package consumes rather than
	// modifies.
	MaxSessionMetaBytes = 64 << 10
)

var (
	// ErrSessionNotFound reports an epoch with no session row.
	ErrSessionNotFound = errors.New("predictioneval/reader: no collector session for that epoch")
	// ErrSnapshotIncoherent reports that the store changed underneath the
	// load: the session classification read before the facts does not match
	// the one read after them.
	//
	// This matters more than it looks. The session row and the facts are read
	// by two separate statements, and retention or a privacy erasure can
	// commit between them. A dataset assembled across that boundary would pair
	// one state's classification with another state's facts — exactly the kind
	// of quietly-wrong input a replay must never be handed.
	ErrSnapshotIncoherent = errors.New("predictioneval/reader: the store changed while the snapshot was being read")
	// ErrLimitExceeded reports a session larger than the caller's bound. It is
	// an error rather than a silent truncation: a truncated dataset would look
	// exactly like a complete one to every stage downstream.
	ErrLimitExceeded = errors.New("predictioneval/reader: session holds more facts than the requested bound")
	// ErrLimitOutOfRange reports a bound this reader will not honour, because
	// honouring it would mean not bounding the read at all. See MaxLoadLimit.
	ErrLimitOutOfRange = errors.New("predictioneval/reader: requested bound exceeds the maximum this reader can enforce")
	// ErrPayloadBudgetExceeded reports a session whose stored payloads exceed
	// MaxSessionPayloadBytes. It is raised from a measurement, so the refusal
	// costs the size of the answer rather than the size of the data.
	ErrPayloadBudgetExceeded = errors.New("predictioneval/reader: session payloads exceed the maximum this reader will load")
	// ErrRecordTooLarge reports a single row wider than MaxRecordBytes.
	ErrRecordTooLarge = errors.New("predictioneval/reader: a stored fact exceeds the maximum width this reader will load")
	// ErrSessionMetaTooLarge reports a session row wider than
	// MaxSessionMetaBytes — the load is refused before that row is scanned.
	ErrSessionMetaTooLarge = errors.New("predictioneval/reader: session metadata exceeds the maximum width this reader will load")
)

// ObservationSource is the read surface this package needs. It is an interface
// so a test can supply a controlled store, and so nothing here depends on more
// of the repository than it uses.
//
// *analytics.SQLiteRepository satisfies it.
type ObservationSource interface {
	// ReadObservationSessionWithinBudget reads the session row ONLY if it
	// fits the given width, with the bound evaluated in the SAME statement
	// that selects and scans the row.
	//
	// A width measured by an earlier, separate query would be a
	// time-of-check/time-of-use gap: another connection can enlarge a column
	// between the measurement and the read, and the read then transfers the
	// enlarged value across the driver having never been covered by any
	// bound. The coherence re-read below detects that the session changed; it
	// cannot un-allocate what was already scanned.
	ReadObservationSessionWithinBudget(ctx context.Context, epoch int64,
		maxRowBytes, maxFactRowBytes int64) (analytics.ObservationSessionReading, bool,
		analytics.ObservationReadBudget, error)
	// ObservationSessionSizeBySession measures what a load would cost without
	// paying it. It is part of the read surface rather than an optimization:
	// a byte bound cannot be enforced after the rows are in memory.
	ObservationSessionSizeBySession(ctx context.Context, sessionID string,
		candidateRows int) (analytics.ObservationSessionSize, error)
	// ObservationEpochRowCount counts the FACT rows an epoch holds, bounded to
	// a candidate window, before the session read runs. That read classifies
	// the session by aggregating over its facts and recomputing a prefix of
	// their witnesses, so an epoch nobody has counted can cost unbounded
	// database work before any limit here could refuse it.
	ObservationEpochRowCount(ctx context.Context, epoch int64,
		candidateRows int) (int64, error)
	// ObservationsBySessionWithinBudget reads the rows only if they fit, with
	// the bounds evaluated in the SAME statement as the read. The measurement
	// above cannot carry that job alone: between a separate measuring query
	// and a separate reading one, another connection can enlarge a value, the
	// row count is unchanged so a count re-check still passes, and the
	// enlarged value is materialized having never been covered by any bound.
	ObservationsBySessionWithinBudget(ctx context.Context, sessionID string, limit int,
		maxRowBytes, maxTotalBytes int64) ([]analytics.ObservationRecord, bool, error)
}

// LoadSession reads one collector session as a bounded, coherent dataset.
//
// The session classification is read, then the facts, then the classification
// AGAIN; a difference between the two readings means the snapshot spans two
// committed states and is refused. Every database resource is released before
// this function returns — the value it hands back holds no rows, no
// transaction and no connection.
//
// ctx is honoured throughout: a cancelled context aborts the load rather than
// returning a partial dataset.
func LoadSession(ctx context.Context, src ObservationSource, epoch int64, limit int) (predictioneval.SourceDataset, error) {
	if limit <= 0 {
		limit = DefaultMaxRecords
	}
	if limit > MaxLoadLimit {
		return predictioneval.SourceDataset{}, ErrLimitOutOfRange
	}

	// Count the epoch's facts before the session read, because that read is
	// not cheap on a hostile store: it classifies the session by aggregating
	// over its facts and then recomputes a prefix of their witnesses. An epoch
	// nobody has counted can therefore cost unbounded database work before any
	// limit below could refuse it. The window is limit+1 so an epoch AT the
	// bound stays distinguishable from one over it.
	//
	// This bounds WORK. The bytes those rows carry are bounded inside the read
	// that materializes them, not here — see the fact-row bound passed below.
	epochRows, err := src.ObservationEpochRowCount(ctx, epoch, limit+1)
	if err != nil {
		return predictioneval.SourceDataset{}, err
	}
	if epochRows > int64(limit) {
		return predictioneval.SourceDataset{}, ErrLimitExceeded
	}

	// Read the session row under a width bound carried IN the reading
	// statement. The session row is the first thing the load materializes and
	// prediction_observation_sessions constrains no column's length, so
	// without the bound the earliest allocation of the whole load is the one
	// nothing covers — and without the co-location, the bound is a measurement
	// another connection can commit past before the read runs.
	before, found, budget, err := src.ReadObservationSessionWithinBudget(
		ctx, epoch, MaxSessionMetaBytes, MaxRecordBytes)
	if err != nil {
		return predictioneval.SourceDataset{}, err
	}
	if refusal := budgetRefusal(budget); refusal != nil {
		return predictioneval.SourceDataset{}, refusal
	}
	if !found {
		return predictioneval.SourceDataset{}, ErrSessionNotFound
	}

	// A session the store has already classified as corrupt yields no cases, so
	// there is nothing to gain by reading its rows — and something to lose. The
	// row scan decodes payload_json for every fact it returns, and the byte
	// ceiling on that column is enforced by the WRITER; a tampered store can
	// hold a payload past it. Refusing before the read keeps the failure
	// bounded instead of paying for content that was already going to be
	// thrown away.
	if before.Reading == analytics.ReadingIntegrityError {
		return predictioneval.SourceDataset{Source: convertProvenance(before)}, nil
	}

	// Measure before loading. ObservationsBySession scans payload_json for
	// every row it returns and json.Unmarshals it, so by the time a bound
	// could be applied to the returned slice the memory has already been
	// spent. The row count bounds how MANY facts arrive and says nothing about
	// how large they are; the per-payload ceiling that would is enforced by
	// the writer and therefore absent from a tampered store.
	//
	// The measurement is an aggregate the database computes without handing
	// any payload across the driver boundary, so asking is bounded whatever
	// the table holds.
	// limit+1 candidate rows: enough to tell a session AT the bound from one
	// over it, and bounded work either way.
	size, err := src.ObservationSessionSizeBySession(ctx, before.Session.CollectorSessionID, limit+1)
	if err != nil {
		return predictioneval.SourceDataset{}, err
	}
	if size.Rows > int64(limit) {
		return predictioneval.SourceDataset{}, ErrLimitExceeded
	}
	// Refuse from the measurement where it is decisive, so the caller gets the
	// specific reason rather than a bare "the store changed".
	if size.TotalBytes > MaxSessionPayloadBytes {
		return predictioneval.SourceDataset{}, ErrPayloadBudgetExceeded
	}
	if size.WidestRowBytes > MaxRecordBytes {
		return predictioneval.SourceDataset{}, ErrRecordTooLarge
	}

	// Ask for one more than the bound, so a session AT the bound is
	// distinguishable from one over it. Silently returning the first `limit`
	// facts of a longer session would hand the replay a prefix that looks
	// complete.
	//
	// The bounds are passed DOWN rather than trusted from the measurement
	// above. The measurement and the read are separate statements, so a value
	// enlarged between them would pass a count re-check unchanged and be
	// materialized having never been bounded at all. Re-stating the limits
	// here puts them in the same statement as the rows.
	rows, within, err := src.ObservationsBySessionWithinBudget(
		ctx, before.Session.CollectorSessionID, limit+1, MaxRecordBytes, MaxSessionPayloadBytes)
	if err != nil {
		return predictioneval.SourceDataset{}, err
	}
	if !within && size.Rows > 0 {
		// The measurement saw facts and the bounded read produced none, so the
		// session grew past a bound in between. That is the store changing
		// underneath the load, which is what this error means.
		return predictioneval.SourceDataset{}, ErrSnapshotIncoherent
	}
	if len(rows) > limit {
		return predictioneval.SourceDataset{}, ErrLimitExceeded
	}

	after, found, budget, err := src.ReadObservationSessionWithinBudget(
		ctx, epoch, MaxSessionMetaBytes, MaxRecordBytes)
	if err != nil {
		return predictioneval.SourceDataset{}, err
	}
	if budget != analytics.ObservationReadWithinBudget {
		// A row grew past a bound during the load. That is the store changing
		// underneath the snapshot, and the bounds carried inside the reading
		// transaction are what kept the enlarged row from being scanned to
		// find it out.
		return predictioneval.SourceDataset{}, ErrSnapshotIncoherent
	}
	if !found || after != before {
		return predictioneval.SourceDataset{}, ErrSnapshotIncoherent
	}
	if err := ctx.Err(); err != nil {
		return predictioneval.SourceDataset{}, err
	}

	return predictioneval.SourceDataset{
		Source:  convertProvenance(before),
		Records: convertRecords(rows),
	}, nil
}

// budgetRefusal maps the store's refusal onto this package's error, or nil
// when nothing was refused. The two are kept distinct so a caller learns WHICH
// bound stopped the load: an oversized session row and an oversized fact are
// different facts about the store.
func budgetRefusal(b analytics.ObservationReadBudget) error {
	switch b {
	case analytics.ObservationReadSessionRowTooWide:
		return ErrSessionMetaTooLarge
	case analytics.ObservationReadFactRowTooWide:
		return ErrRecordTooLarge
	default:
		return nil
	}
}

// convertProvenance carries the store's own session verdict across the fence
// verbatim. Nothing is re-derived and nothing is upgraded: an unchecked
// witness stays unchecked, a truncated session stays truncated.
func convertProvenance(r analytics.ObservationSessionReading) predictioneval.SourceProvenance {
	return predictioneval.SourceProvenance{
		CollectorEpoch:     r.Session.CollectorEpoch,
		CollectorSessionID: r.Session.CollectorSessionID,
		ProducerRevision:   r.Session.ProducerRevision,
		SessionReading:     r.Reading,
		SessionDetail:      r.Detail,
		CloseState:         r.Session.CloseState,
		WitnessesVerified:  r.WitnessesVerified,
		WitnessesUnchecked: r.WitnessesUnchecked,
		FactsPresent:       r.FactsPresent,
		CommittedCount:     r.Session.CommittedCount,
		DroppedCount:       r.Session.DroppedCount,
	}
}

func convertRecords(in []analytics.ObservationRecord) []predictioneval.SourceRecord {
	if len(in) == 0 {
		return nil
	}
	out := make([]predictioneval.SourceRecord, 0, len(in))
	for i := range in {
		out = append(out, convertRecord(&in[i]))
	}
	return out
}

func convertRecord(r *analytics.ObservationRecord) predictioneval.SourceRecord {
	return predictioneval.SourceRecord{
		ObservationID:        r.ObservationID,
		CollectorSessionID:   r.CollectorSessionID,
		CollectorEpoch:       r.CollectorEpoch,
		CollectorSequence:    r.CollectorSequence,
		PoolInstanceID:       r.PoolInstanceID,
		RoundIncarnationID:   r.RoundIncarnationID,
		RoundCaptureOrigin:   r.RoundCaptureOrigin,
		RoundCaptureGapCause: r.RoundCaptureGapCause,
		EventID:              r.EventID,
		Kind:                 r.Kind,
		PayloadVersion:       r.PayloadVersion,
		PayloadUndecodable:   r.PayloadUndecodable,
		ObservationSHA256:    r.ObservationSHA256,
		Payload:              convertPayload(r.Payload),
	}
}

func convertPayload(p analytics.ObservationPayload) predictioneval.SourcePayload {
	out := predictioneval.SourcePayload{
		Phase:      p.Phase,
		RoundState: p.RoundState,
		Decision:   p.Decision,
		ReasonCode: p.ReasonCode,
		ErrorClass: p.ErrorClass,
	}
	if p.Manual != nil {
		v := *p.Manual
		out.Manual = &v
	}
	if p.OutcomeSlot != nil {
		v := *p.OutcomeSlot
		out.OutcomeSlot = &v
	}
	if len(p.Counters) > 0 {
		counters := make(map[string]int64, len(p.Counters))
		for k, v := range p.Counters {
			counters[k] = v
		}
		out.Counters = counters
	}
	out.DecisionEnvelope = convertEnvelope(p.DecisionEnvelope)
	out.AdmissionSettings = convertSettings(p.AdmissionSettings)
	return out
}

func convertEnvelope(e *analytics.ObservationDecisionEnvelope) *predictioneval.SourceDecisionEnvelope {
	if e == nil {
		return nil
	}
	out := &predictioneval.SourceDecisionEnvelope{
		AttemptID:       e.AttemptID,
		SettingsStage:   e.SettingsStage,
		Settings:        convertSettings(e.Settings),
		CalculateStage:  e.CalculateStage,
		Balance:         copyInt64(e.Balance),
		BetTotalUsers:   copyInt64(e.BetTotalUsers),
		BetTotalPoints:  copyInt64(e.BetTotalPoints),
		ChoiceIndex:     copyInt(e.ChoiceIndex),
		ChoiceOutcomeID: e.ChoiceOutcomeID,
		ChoiceAmount:    copyInt64(e.ChoiceAmount),
		SkipStage:       e.SkipStage,
		SkipResult:      copyBool(e.SkipResult),
		SkipCompared:    copyFloat64(e.SkipCompared),
		HealthStage:     e.HealthStage,
		HealthReason:    e.HealthReason,

		StakeStage:          e.StakeStage,
		RiskMaxStakePercent: copyInt(e.RiskMaxStakePercent),
		RiskReservePoints:   copyInt(e.RiskReservePoints),
		StakeAllowed:        copyInt64(e.StakeAllowed),
		StakeReason:         e.StakeReason,
		StakeLimit:          copyInt64(e.StakeLimit),

		ClampApplied: copyBool(e.ClampApplied),
		FinalAmount:  copyInt64(e.FinalAmount),
	}
	if len(e.Outcomes) > 0 {
		out.Outcomes = make([]predictioneval.SourceModelOutcome, 0, len(e.Outcomes))
		for _, o := range e.Outcomes {
			out.Outcomes = append(out.Outcomes, predictioneval.SourceModelOutcome{
				Slot:            o.Slot,
				Present:         o.Present,
				ID:              o.ID,
				TotalUsers:      o.TotalUsers,
				TotalPoints:     o.TotalPoints,
				TopPoints:       o.TopPoints,
				PercentageUsers: o.PercentageUsers,
				Odds:            o.Odds,
				OddsPercentage:  o.OddsPercentage,
			})
		}
	}
	return out
}

func convertSettings(s *analytics.ObservationBetSettings) *predictioneval.SourceBetSettings {
	if s == nil {
		return nil
	}
	out := &predictioneval.SourceBetSettings{
		Strategy:      s.Strategy,
		Percentage:    s.Percentage,
		PercentageGap: s.PercentageGap,
		MaxPoints:     s.MaxPoints,
		MinimumPoints: s.MinimumPoints,
		StealthMode:   s.StealthMode,
		Delay:         s.Delay,
		DelayMode:     s.DelayMode,
	}
	if s.FilterCondition != nil {
		out.FilterCondition = &predictioneval.SourceFilterCondition{
			By:    s.FilterCondition.By,
			Where: s.FilterCondition.Where,
			Value: s.FilterCondition.Value,
		}
	}
	return out
}

func copyInt64(v *int64) *int64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func copyInt(v *int) *int {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func copyBool(v *bool) *bool {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}

func copyFloat64(v *float64) *float64 {
	if v == nil {
		return nil
	}
	out := *v
	return &out
}
