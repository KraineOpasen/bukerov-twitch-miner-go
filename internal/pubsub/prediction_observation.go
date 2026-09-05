package pubsub

// Producer side of the immutable Prediction observation trail (P1).
//
// This package OBSERVES; it never records. It builds a bounded, immutable copy
// of what it already holds, stamps a few atomics onto it, and hands it to a
// sink through one nonblocking call. It deliberately does not import the
// analytics package: a transport must not depend on a persistence layer, so
// the value below is this package's own and the miner adapts it.
//
// Every call site in this package obeys three rules:
//
//   - It performs no I/O, no JSON work, no allocation-heavy formatting and no
//     wait. Building a fact is a handful of field copies.
//   - It never changes control flow. An observation call is a statement whose
//     result is discarded; removing every one of them would leave the
//     betting, timing, call-count and result behaviour identical.
//   - It is safe under a WebSocket, pool or placement lock. The sink is read
//     from an atomic pointer, and the sink contract requires a nonblocking
//     hand-off.

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

// Observation kinds, mirroring the closed set the store accepts. They are
// plain strings here so this package carries no dependency on the store.
const (
	ObsKindSourceUnknown      = "source_unknown"
	ObsKindChannelEvent       = "channel_event"
	ObsKindScheduleDecision   = "schedule_decision"
	ObsKindAutoDecision       = "auto_decision"
	ObsKindManualControl      = "manual_control"
	ObsKindPlacement          = "placement"
	ObsKindUserPredictionMade = "user_prediction_made"
	ObsKindUserTerminal       = "user_terminal"
	ObsKindRoundCleanup       = "round_cleanup"
)

// Producer time sources.
const (
	ObsTimeProducer = "PRODUCER"
	ObsTimeServer   = "SERVER"
	ObsTimeReceiver = "RECEIVER"
)

// Presence values.
const (
	ObsPresent = "PRESENT"
	ObsAbsent  = "ABSENT"
)

// ObservationOutcome is the sanitized aggregate projection of one round
// outcome. No predictor identity and no display text is carried.
type ObservationOutcome struct {
	Slot                  int
	Color                 string
	TotalPoints           int64
	TotalUsers            int64
	TopPredictorsExamined int
}

// ObservationPayload is the closed typed projection. Every string field is a
// member of a closed vocabulary the store validates; there is no free-text
// field, which is why no raw frame, error or identity can reach storage
// through this type.
type ObservationPayload struct {
	Phase       string
	RoundState  string
	Decision    string
	ReasonCode  string
	ErrorClass  string
	Manual      *bool
	OutcomeSlot *int
	Outcomes    []ObservationOutcome
	Counters    map[string]int64
	Presence    map[string]string
}

// PredictionObservation is one immutable fact as this package produces it.
type PredictionObservation struct {
	PoolInstanceID string

	RoutedChannelID string
	RoutedLogin     string

	RoundOwnerChannelID string
	RoundOwnerLogin     string

	RetentionGroupOwnerChannelID string
	RetentionGroupOwnerLogin     string

	EventID string

	Kind              string
	SourceTopicType   string
	SourceMessageType string

	ProducerAtMS       int64
	ProducerTimeSource string
	ReceivedAtMS       int64

	ConnectionIndex      int
	ConnectionGeneration uint64
	ConnectionSequence   uint64
	ConnectionKnown      bool

	Payload ObservationPayload
}

// PredictionObservationSink receives immutable Prediction observations.
//
// CONTRACT: RecordPredictionObservation MUST NOT block, perform I/O, take a
// lock this package could already hold, or call back into this package. It is
// invoked from message-handling and placement paths, sometimes with a
// WebSocket, pool or round-placement lock held. A sink that cannot accept a
// fact must drop it silently — there is no error channel, because a producer
// has nothing it may legitimately do about one.
type PredictionObservationSink interface {
	RecordPredictionObservation(PredictionObservation)
}

// newPoolInstanceID mints the identity of one pool instance. Random and
// non-identifying: it distinguishes this pool's facts from another
// generation's, and carries no information about the account or its channels.
func newPoolInstanceID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A pool instance id is required for every fact, so fall back to a
		// fixed, obviously-degraded value rather than emitting facts with no
		// provenance at all.
		return "pool-unknown"
	}
	return "pool-" + hex.EncodeToString(b[:])
}

// SetPredictionObservationSink installs the observation sink. It is wired once
// at construction time, before Start; the atomic store means every later read
// on a hot path is lock-free.
func (p *WebSocketPool) SetPredictionObservationSink(sink PredictionObservationSink) {
	p.observationSink.Store(&sink)
}

// PoolInstanceID is this pool instance's observation provenance.
func (p *WebSocketPool) PoolInstanceID() string { return p.instanceID }

// observe hands ONE fact to the sink. It is the only way this package emits an
// observation, and it is a no-op when no sink is wired — which is the case in
// every existing test and whenever analytics is disabled.
func (p *WebSocketPool) observe(obs PredictionObservation) {
	sinkPtr := p.observationSink.Load()
	if sinkPtr == nil || *sinkPtr == nil {
		return
	}
	obs.PoolInstanceID = p.instanceID
	if obs.ReceivedAtMS == 0 {
		obs.ReceivedAtMS = time.Now().UnixMilli()
	}
	if obs.ProducerTimeSource == "" {
		obs.ProducerTimeSource = ObsTimeReceiver
	}
	(*sinkPtr).RecordPredictionObservation(obs)
}

// observing reports whether a sink is wired, so a call site can skip building
// a fact nobody will receive. Purely an allocation guard.
func (p *WebSocketPool) observing() bool {
	sinkPtr := p.observationSink.Load()
	return sinkPtr != nil && *sinkPtr != nil
}

// observationFromMessage seeds a fact from an inbound frame: its routing
// identity, its source classification, its producer time and the connection
// that delivered it. The caller fills in the kind and payload.
//
// Only the topic TYPE is carried — never Topic.String(), which concatenates
// the type with an account-scoped identifier — and never EventFingerprint,
// which digests the raw inner frame.
func observationFromMessage(msg *PubSubMessage, streamer streamerIdentity) PredictionObservation {
	obs := PredictionObservation{
		RoutedChannelID:   msg.ChannelID,
		SourceTopicType:   string(msg.Topic.Type),
		SourceMessageType: msg.Type,
		ReceivedAtMS:      time.Now().UnixMilli(),
		// A round is retained and erased under the channel that owns it, which
		// for both Prediction topics is the channel the frame is about.
		RetentionGroupOwnerChannelID: msg.ChannelID,

		ConnectionIndex:      msg.ConnectionIndex,
		ConnectionGeneration: msg.ConnectionGeneration,
		ConnectionSequence:   msg.ConnectionSequence,
		ConnectionKnown:      msg.ConnectionKnown,
	}
	if streamer != nil {
		login := streamer.GetUsername()
		obs.RoutedLogin = login
		obs.RetentionGroupOwnerLogin = login
	}
	if !msg.Timestamp.IsZero() {
		obs.ProducerAtMS = msg.Timestamp.UnixMilli()
		obs.ProducerTimeSource = ObsTimeProducer
	} else {
		obs.ProducerTimeSource = ObsTimeReceiver
	}
	return obs
}

// streamerIdentity is the tiny slice of *models.Streamer an observation needs.
// Declaring it here keeps the helper testable without a full streamer.
type streamerIdentity interface {
	GetUsername() string
}

// observationRoundIdentity fills in the round identity of a fact that is about
// a specific tracked round on a specific channel.
func observationRoundIdentity(obs *PredictionObservation, eventID, channelID, login string) {
	obs.EventID = eventID
	obs.RetentionGroupOwnerChannelID = channelID
	obs.RetentionGroupOwnerLogin = login
	obs.RoutedChannelID = channelID
	obs.RoutedLogin = login
}

// presence reports PRESENT/ABSENT for a structural part of a frame without
// recording any of its content.
func presence(ok bool) string {
	if ok {
		return ObsPresent
	}
	return ObsAbsent
}

// boolPtr / intPtr keep the optional payload fields readable at call sites.
func boolPtr(v bool) *bool { return &v }
func intPtr(v int) *int    { return &v }

// maxObservedOutcomes / maxTopPredictorsExamined mirror the store's frozen
// bounds, so a producer never builds a fact the store would have to truncate.
const (
	maxObservedOutcomes      = 64
	maxTopPredictorsExamined = 256
)

// projectOutcomes builds the bounded aggregate projection of a round's
// outcomes from a raw frame slice. It reads ONLY the aggregate fields and the
// LENGTH of top_predictors — it never looks at a predictor's contents, so no
// predictor identity can escape, by construction rather than by filtering.
func projectOutcomes(raw []interface{}) []ObservationOutcome {
	n := len(raw)
	if n > maxObservedOutcomes {
		n = maxObservedOutcomes
	}
	if n == 0 {
		return nil
	}
	out := make([]ObservationOutcome, 0, n)
	for i := 0; i < n; i++ {
		data, ok := raw[i].(map[string]interface{})
		if !ok {
			out = append(out, ObservationOutcome{Slot: i})
			continue
		}
		o := ObservationOutcome{Slot: i}
		if color, ok := data["color"].(string); ok {
			o.Color = color
		}
		if v, ok := data["total_points"].(float64); ok {
			o.TotalPoints = int64(v)
		}
		if v, ok := data["total_users"].(float64); ok {
			o.TotalUsers = int64(v)
		}
		if tp, ok := data["top_predictors"].([]interface{}); ok {
			examined := len(tp)
			if examined > maxTopPredictorsExamined {
				examined = maxTopPredictorsExamined
			}
			o.TopPredictorsExamined = examined
		}
		out = append(out, o)
	}
	return out
}

// observeUnclassifiedFrame records a Prediction-domain frame whose shape could
// not be classified — an absent envelope, not an absent event. Recording it
// keeps "we saw something we could not read" visible instead of silent.
func (p *WebSocketPool) observeUnclassifiedFrame(msg *PubSubMessage, streamer streamerIdentity, missing string) {
	if !p.observing() {
		return
	}
	obs := observationFromMessage(msg, streamer)
	obs.Kind = ObsKindSourceUnknown
	obs.Payload = ObservationPayload{
		Phase:    "UNCLASSIFIED",
		Presence: map[string]string{missing: ObsAbsent},
	}
	// An unreadable frame names no round, so it belongs to no retention group.
	obs.RetentionGroupOwnerChannelID = ""
	obs.RetentionGroupOwnerLogin = ""
	p.observe(obs)
}

// observeChannelEvent records one predictions-channel-v1 round lifecycle frame
// exactly as it arrived, before any decision is taken about it.
func (p *WebSocketPool) observeChannelEvent(msg *PubSubMessage, streamer streamerIdentity, eventID, status string, eventData map[string]interface{}) {
	if !p.observing() {
		return
	}
	obs := observationFromMessage(msg, streamer)
	obs.Kind = ObsKindChannelEvent
	obs.EventID = eventID

	phase := "ROUND_UPDATED"
	if msg.Type == "event-created" {
		phase = "ROUND_CREATED"
	}
	payload := ObservationPayload{
		Phase:      phase,
		RoundState: status,
		Presence: map[string]string{
			"event": ObsPresent,
		},
	}
	rawOutcomes, hasOutcomes := eventData["outcomes"].([]interface{})
	payload.Presence["outcomes"] = presence(hasOutcomes)
	if hasOutcomes {
		payload.Outcomes = projectOutcomes(rawOutcomes)
		payload.Counters = map[string]int64{"outcomeCount": int64(len(rawOutcomes))}
	}
	if window, ok := eventData["prediction_window_seconds"].(float64); ok {
		if payload.Counters == nil {
			payload.Counters = map[string]int64{}
		}
		payload.Counters["windowSeconds"] = int64(window)
	}
	obs.Payload = payload
	p.observe(obs)
}

// observeScheduleDecision records whether a newly seen round was scheduled for
// an auto-bet, and why not when it was not. It is emitted AFTER the decision
// the existing code already made — it never participates in making it.
func (p *WebSocketPool) observeScheduleDecision(msg *PubSubMessage, streamer streamerIdentity, eventID, status, phase, decision, reason string, counters map[string]int64) {
	if !p.observing() {
		return
	}
	obs := observationFromMessage(msg, streamer)
	obs.Kind = ObsKindScheduleDecision
	obs.EventID = eventID
	obs.Payload = ObservationPayload{
		Phase:      phase,
		RoundState: status,
		Decision:   decision,
		ReasonCode: reason,
		Manual:     boolPtr(false),
		Counters:   counters,
	}
	p.observe(obs)
}

// observeUserFrame records a predictions-user-v1 delivery: the confirmation
// that a placement was accepted, or a terminal result and the admission
// verdict the existing code already reached for it.
func (p *WebSocketPool) observeUserFrame(msg *PubSubMessage, streamer streamerIdentity, eventID, kind, phase, reason string, counters map[string]int64, present map[string]string) {
	if !p.observing() {
		return
	}
	obs := observationFromMessage(msg, streamer)
	obs.Kind = kind
	obs.EventID = eventID
	obs.Payload = ObservationPayload{
		Phase:      phase,
		ReasonCode: reason,
		Counters:   counters,
		Presence:   present,
	}
	p.observe(obs)
}

// observeAutoSkip records that the automated path declined to place, with the
// closed reason the existing code already acted on.
func (p *WebSocketPool) observeAutoSkip(eventID, channelID, login, reason string, counters map[string]int64) {
	p.observeRoundFact(eventID, channelID, login, ObsKindAutoDecision, ObservationPayload{
		Phase:      "AUTO_SKIPPED",
		Decision:   "SKIP",
		ReasonCode: reason,
		Manual:     boolPtr(false),
		Counters:   counters,
	})
}

// observeAutoSkipState is observeAutoSkip for the branch that also knows the
// round's lifecycle state.
func (p *WebSocketPool) observeAutoSkipState(eventID, channelID, login, reason, roundState string) {
	p.observeRoundFact(eventID, channelID, login, ObsKindAutoDecision, ObservationPayload{
		Phase:      "AUTO_SKIPPED",
		Decision:   "SKIP",
		ReasonCode: reason,
		RoundState: roundState,
		Manual:     boolPtr(false),
	})
}

// observePlacementCall records one side of the SINGLE Twitch placement call.
// It carries the stake and outcome slot already decided, an ok flag, and a
// CLOSED error class — never the error itself, which can carry a provider
// message or a Twitch transaction identifier.
func (p *WebSocketPool) observePlacementCall(eventID, channelID, login, phase string, ok bool, errorClass string, slot, amount int) {
	reason := "OK"
	if !ok && phase == "CALL_RETURNED" {
		reason = "REJECTED"
	}
	p.observeRoundFact(eventID, channelID, login, ObsKindPlacement, ObservationPayload{
		Phase:       phase,
		ReasonCode:  reason,
		ErrorClass:  errorClass,
		OutcomeSlot: intPtr(slot),
		Counters:    map[string]int64{"stake": int64(amount)},
	})
}

// placementErrorClass maps a placement failure onto the CLOSED error
// vocabulary. It deliberately inspects only typed sentinels this package
// already owns; anything else is INTERNAL. The error's text is never read,
// so a provider message, a URL or a transaction id has no path into a fact.
func placementErrorClass(err error) string {
	switch {
	case err == nil:
		return "NONE"
	case errors.Is(err, ErrPredictionNotFound), errors.Is(err, ErrRoundClosed),
		errors.Is(err, ErrAlreadyBet), errors.Is(err, ErrAutoBetPlaced),
		errors.Is(err, ErrStreamerOffline):
		return "ROUND_CLOSED"
	case errors.Is(err, ErrOutcomeNotFound), errors.Is(err, ErrInvalidAmount),
		errors.Is(err, ErrAmountTooLow), errors.Is(err, ErrManualBetInFlight):
		return "INVALID_ARGUMENT"
	case errors.Is(err, ErrInsufficientPoints):
		return "NOT_ENOUGH_POINTS"
	default:
		return "REJECTED_BY_TWITCH"
	}
}

// observeManualPhase records one phase of an operator-initiated placement.
func (p *WebSocketPool) observeManualPhase(eventID, channelID, login, phase, reason string, counters map[string]int64) {
	p.observeRoundFact(eventID, channelID, login, ObsKindManualControl, ObservationPayload{
		Phase:      phase,
		ReasonCode: reason,
		Manual:     boolPtr(true),
		Counters:   counters,
	})
}

// observeManualSkip records a manual placement the pool declined at the
// validation stage, with the round state it saw.
func (p *WebSocketPool) observeManualSkip(eventID, channelID, login, reason, roundState string) {
	p.observeRoundFact(eventID, channelID, login, ObsKindManualControl, ObservationPayload{
		Phase:      "MANUAL_SKIPPED",
		Decision:   "SKIP",
		ReasonCode: reason,
		RoundState: roundState,
		Manual:     boolPtr(true),
	})
}

// observeRoundCleanup records a tracked round's state being dropped.
func (p *WebSocketPool) observeRoundCleanup(eventID, channelID, login, phase, reason string) {
	p.observeRoundFact(eventID, channelID, login, ObsKindRoundCleanup, ObservationPayload{
		Phase:      phase,
		ReasonCode: reason,
	})
}

// observeRoundFact records a fact about a tracked round that did not arrive on
// a frame — an auto decision, a placement call, a manual control phase, a
// cleanup. The round identity is taken from the tracked round itself.
func (p *WebSocketPool) observeRoundFact(eventID, channelID, login, kind string, payload ObservationPayload) {
	if !p.observing() {
		return
	}
	var obs PredictionObservation
	observationRoundIdentity(&obs, eventID, channelID, login)
	obs.Kind = kind
	obs.ProducerTimeSource = ObsTimeReceiver
	obs.ReceivedAtMS = time.Now().UnixMilli()
	obs.Payload = payload
	p.observe(obs)
}
