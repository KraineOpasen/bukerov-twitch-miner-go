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
)

// ObservationSource is the read surface this package needs. It is an interface
// so a test can supply a controlled store, and so nothing here depends on more
// of the repository than it uses.
//
// *analytics.SQLiteRepository satisfies it.
type ObservationSource interface {
	ReadObservationSession(ctx context.Context, epoch int64) (analytics.ObservationSessionReading, bool, error)
	ObservationsBySession(ctx context.Context, sessionID string, limit int) ([]analytics.ObservationRecord, error)
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

	before, found, err := src.ReadObservationSession(ctx, epoch)
	if err != nil {
		return predictioneval.SourceDataset{}, err
	}
	if !found {
		return predictioneval.SourceDataset{}, ErrSessionNotFound
	}

	// Ask for one more than the bound, so a session AT the bound is
	// distinguishable from one over it. Silently returning the first `limit`
	// facts of a longer session would hand the replay a prefix that looks
	// complete.
	rows, err := src.ObservationsBySession(ctx, before.Session.CollectorSessionID, limit+1)
	if err != nil {
		return predictioneval.SourceDataset{}, err
	}
	if len(rows) > limit {
		return predictioneval.SourceDataset{}, ErrLimitExceeded
	}

	after, found, err := src.ReadObservationSession(ctx, epoch)
	if err != nil {
		return predictioneval.SourceDataset{}, err
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
