package miner

// Adapter between the Prediction observation PRODUCER (internal/pubsub) and
// its STORE (internal/analytics).
//
// The two packages deliberately do not know each other: a transport must not
// depend on a persistence layer. The miner already depends on both, so it is
// the natural — and only — place for the translation. The adapter is pure:
// it copies fields, allocates nothing unbounded, performs no I/O and can be
// called with a WebSocket, pool or placement lock held.

import (
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
)

// predictionObservationSink adapts the analytics service to the pool's sink
// contract. It satisfies pubsub.PredictionObservationSink.
type predictionObservationSink struct {
	svc *analytics.Service
}

// RecordPredictionObservation translates one produced fact and hands it to the
// analytics collector, which performs a single nonblocking enqueue. It never
// blocks, never returns an error and never calls back into pubsub.
func (s predictionObservationSink) RecordPredictionObservation(obs pubsub.PredictionObservation) {
	if s.svc == nil {
		return
	}
	s.svc.RecordPredictionObservation(toAnalyticsObservation(obs))
}

// BeginPredictionProducerEpisode forwards a producer episode registration to
// the collector. Like the record call it is a pure hand-off: two atomics and
// no allocation on the shared connection.
func (s predictionObservationSink) BeginPredictionProducerEpisode() func() {
	if s.svc == nil {
		return func() {}
	}
	return s.svc.BeginPredictionProducerEpisode()
}

// PredictionCaptureState forwards the collector's capture state. Like the
// other calls it is a pure hand-off: one atomic and one read lock, no I/O.
func (s predictionObservationSink) PredictionCaptureState(channelID, login string) string {
	if s.svc == nil {
		return "NO_SINK"
	}
	return s.svc.PredictionCaptureState(channelID, login)
}

// NotePredictionProducerShutdownUncertain forwards a shutdown whose evidence
// was inconclusive. Like the other two calls it is a pure hand-off.
func (s predictionObservationSink) NotePredictionProducerShutdownUncertain() {
	if s.svc == nil {
		return
	}
	s.svc.NotePredictionProducerShutdownUncertain()
}

// toAnalyticsObservation is the field-for-field translation. Every field of
// the producer's type has exactly one destination, and the store's sanitizer
// is what finally decides which values are admissible — this function
// deliberately makes no policy decision of its own, so the two closed
// vocabularies cannot drift apart silently here.
func toAnalyticsObservation(in pubsub.PredictionObservation) analytics.PredictionObservation {
	out := analytics.PredictionObservation{
		PoolInstanceID: in.PoolInstanceID,

		RoutedChannelID: in.RoutedChannelID,
		RoutedLogin:     in.RoutedLogin,

		RoundOwnerChannelID: in.RoundOwnerChannelID,
		RoundOwnerLogin:     in.RoundOwnerLogin,

		RetentionGroupOwnerChannelID: in.RetentionGroupOwnerChannelID,
		RetentionGroupOwnerLogin:     in.RetentionGroupOwnerLogin,

		RoundIncarnationID:   in.RoundIncarnationID,
		RoundCaptureOrigin:   in.RoundCaptureOrigin,
		RoundCaptureGapCause: in.RoundCaptureGapCause,

		EventID: in.EventID,

		Kind:              in.Kind,
		SourceTopicType:   in.SourceTopicType,
		SourceMessageType: in.SourceMessageType,

		ProducerAtMS:       in.ProducerAtMS,
		ProducerTimeSource: in.ProducerTimeSource,
		ReceivedAtMS:       in.ReceivedAtMS,

		ConnectionIndex:      in.ConnectionIndex,
		ConnectionGeneration: in.ConnectionGeneration,
		ConnectionSequence:   in.ConnectionSequence,
		ConnectionKnown:      in.ConnectionKnown,

		Payload: toAnalyticsObservationPayload(in.Payload),
	}
	return out
}

func toAnalyticsObservationPayload(in pubsub.ObservationPayload) analytics.ObservationPayload {
	out := analytics.ObservationPayload{
		Phase:       in.Phase,
		RoundState:  in.RoundState,
		Decision:    in.Decision,
		ReasonCode:  in.ReasonCode,
		ErrorClass:  in.ErrorClass,
		Manual:      in.Manual,
		OutcomeSlot: in.OutcomeSlot,
		Counters:    in.Counters,
		Presence:    in.Presence,

		DecisionEnvelope:  toAnalyticsDecisionEnvelope(in.DecisionEnvelope),
		AdmissionSettings: toAnalyticsBetSettings(in.AdmissionSettings),
	}
	if len(in.Outcomes) > 0 {
		out.Outcomes = make([]analytics.ObservationOutcome, 0, len(in.Outcomes))
		for _, o := range in.Outcomes {
			out.Outcomes = append(out.Outcomes, analytics.ObservationOutcome{
				Slot:                  o.Slot,
				Color:                 o.Color,
				ColorState:            o.ColorState,
				TotalPoints:           o.TotalPoints,
				TotalUsers:            o.TotalUsers,
				TopPredictorsExamined: o.TopPredictorsExamined,
				TopPredictors:         o.TopPredictors,
			})
		}
	}
	return out
}

// toAnalyticsBetSettings translates one bet-settings snapshot. Like every
// other part of this adapter it makes no policy decision: it copies the nine
// fields and deep-copies the optional filter condition, and the store's
// sanitizer is what decides which values are admissible. A nil snapshot stays
// nil, because "this round had no such snapshot" and "this round had an empty
// one" are different facts.
func toAnalyticsBetSettings(in *pubsub.ObservationBetSettings) *analytics.ObservationBetSettings {
	if in == nil {
		return nil
	}
	out := &analytics.ObservationBetSettings{
		Strategy:      in.Strategy,
		Percentage:    in.Percentage,
		PercentageGap: in.PercentageGap,
		MaxPoints:     in.MaxPoints,
		MinimumPoints: in.MinimumPoints,
		StealthMode:   in.StealthMode,
		Delay:         in.Delay,
		DelayMode:     in.DelayMode,
	}
	if in.FilterCondition != nil {
		out.FilterCondition = &analytics.ObservationFilterCondition{
			By:    in.FilterCondition.By,
			Where: in.FilterCondition.Where,
			Value: in.FilterCondition.Value,
		}
	}
	return out
}

// toAnalyticsDecisionEnvelope translates one auto-decision envelope. Every
// pointer is copied by value rather than aliased, so the stored fact cannot
// share memory with anything the producer still holds.
func toAnalyticsDecisionEnvelope(in *pubsub.ObservationDecision) *analytics.ObservationDecisionEnvelope {
	if in == nil {
		return nil
	}
	out := &analytics.ObservationDecisionEnvelope{
		AttemptID:     in.AttemptID,
		SettingsStage: in.SettingsStage,
		Settings:      toAnalyticsBetSettings(in.Settings),

		CalculateStage: in.CalculateStage,
		Balance:        copyInt64(in.Balance),
		BetTotalUsers:  copyInt64(in.BetTotalUsers),
		BetTotalPoints: copyInt64(in.BetTotalPoints),

		ChoiceIndex:     copyInt(in.ChoiceIndex),
		ChoiceOutcomeID: in.ChoiceOutcomeID,
		ChoiceAmount:    copyInt64(in.ChoiceAmount),

		SkipStage:    in.SkipStage,
		SkipResult:   copyBool(in.SkipResult),
		SkipCompared: copyFloat64(in.SkipCompared),

		HealthStage:  in.HealthStage,
		HealthReason: in.HealthReason,

		StakeStage:          in.StakeStage,
		RiskMaxStakePercent: copyInt(in.RiskMaxStakePercent),
		RiskReservePoints:   copyInt(in.RiskReservePoints),
		StakeAllowed:        copyInt64(in.StakeAllowed),
		StakeReason:         in.StakeReason,
		StakeLimit:          copyInt64(in.StakeLimit),

		ClampApplied: copyBool(in.ClampApplied),
		FinalAmount:  copyInt64(in.FinalAmount),
	}
	if len(in.Outcomes) > 0 {
		out.Outcomes = make([]analytics.ObservationModelOutcome, 0, len(in.Outcomes))
		for _, o := range in.Outcomes {
			out.Outcomes = append(out.Outcomes, analytics.ObservationModelOutcome{
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

// NewPredictionObservationSink returns the production adapter that carries
// facts from the Prediction producer to the analytics collector.
//
// The miner wires this itself for a live process; it is exported so the P1.5
// capture contract can be proved END TO END through the REAL adapter — the
// pool's producer, this translation, the collector, the store and the public
// reader — rather than by hand-building a payload on one side of the seam and
// reading it back on the other, which would prove nothing about the path a
// real decision actually takes.
func NewPredictionObservationSink(svc *analytics.Service) pubsub.PredictionObservationSink {
	return predictionObservationSink{svc: svc}
}

// attachPredictionObservations wires the observation sink onto a freshly built
// pool. A miner with no analytics service wires nothing, so every observation
// call site in pubsub stays a no-op.
func (m *Miner) attachPredictionObservations() {
	if m.wsPool == nil || m.analyticsSvc == nil {
		return
	}
	// The handle is what makes the pool's shutdown PROVABLE rather than
	// assumed. Until it is settled the collector counts this pool as a live
	// producer, so a pool that is never closed at all cannot leave a session
	// claiming to have observed everything — which a bare nil-check on Close's
	// result could never detect, because a Close that never happened returns
	// no result to check.
	m.settlePredictionProducer = m.wsPool.SetPredictionObservationSink(
		predictionObservationSink{svc: m.analyticsSvc})
}

// settlePredictionObservations settles the pool's producer obligation exactly
// once, with the pool's own Close result as the evidence. Safe to call with no
// pool, no analytics and more than once.
func (m *Miner) settlePredictionObservations(poolErr error) {
	if m.settlePredictionProducer == nil {
		return
	}
	m.settlePredictionProducer(poolErr)
}
