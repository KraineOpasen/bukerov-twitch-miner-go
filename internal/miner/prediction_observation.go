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

		RoundIncarnationID: in.RoundIncarnationID,

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
	}
	if len(in.Outcomes) > 0 {
		out.Outcomes = make([]analytics.ObservationOutcome, 0, len(in.Outcomes))
		for _, o := range in.Outcomes {
			out.Outcomes = append(out.Outcomes, analytics.ObservationOutcome{
				Slot:                  o.Slot,
				Color:                 o.Color,
				TotalPoints:           o.TotalPoints,
				TotalUsers:            o.TotalUsers,
				TopPredictorsExamined: o.TopPredictorsExamined,
			})
		}
	}
	return out
}

// attachPredictionObservations wires the observation sink onto a freshly built
// pool. A miner with no analytics service wires nothing, so every observation
// call site in pubsub stays a no-op.
func (m *Miner) attachPredictionObservations() {
	if m.wsPool == nil || m.analyticsSvc == nil {
		return
	}
	m.wsPool.SetPredictionObservationSink(predictionObservationSink{svc: m.analyticsSvc})
}
