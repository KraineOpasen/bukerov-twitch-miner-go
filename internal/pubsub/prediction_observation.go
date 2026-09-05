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
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/url"
	"os"
	"strconv"
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

// Presence states. One closed vocabulary for every optional scalar or list a
// frame may carry.
//
// PRESENT and ABSENT alone lose the distinctions that matter about a wire. A
// key that was never sent, a key sent explicitly as null, and a key sent with
// the wrong type are three different things Twitch did, and a trail that
// records them identically cannot answer the question it exists for. So is the
// difference between "we looked and it was not there" and "this path never
// looks at it": collapsing those invents an observation nobody made.
const (
	// ObsPresent: the key was there, well-typed, and its value was read. A
	// legitimate zero or false is PRESENT.
	ObsPresent = "PRESENT"
	// ObsAbsentOnWire: the key was not in the frame at all.
	ObsAbsentOnWire = "ABSENT_ON_WIRE"
	// ObsNullOnWire: the key was there and explicitly null.
	ObsNullOnWire = "NULL_ON_WIRE"
	// ObsInvalid: the key was there with a value of the wrong shape.
	ObsInvalid = "INVALID"
	// ObsUnknownPresent: a well-typed value outside this build's closed
	// vocabulary. The raw value is never kept — only the fact that one was
	// there and was not recognized.
	ObsUnknownPresent = "UNKNOWN_PRESENT"
	// ObsNotObserved: this path does not read the field. Not an absence on
	// the wire: an absence of observation.
	ObsNotObserved = "NOT_OBSERVED"
	// ObsUnavailable: the field cannot be observed here at all.
	ObsUnavailable = "UNAVAILABLE"
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

	// RoundIncarnationID names the LOCAL admission of the round this fact is
	// about. It is carried from the pool's roundControl, never recomputed from
	// the channel and event id, so two separate admissions of one Twitch event
	// are two rounds. Empty when the fact is about no admitted round.
	RoundIncarnationID string

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

	// BeginPredictionProducerEpisode registers that a producer episode is
	// about to run, and returns the function that settles it.
	//
	// It exists for the fire-and-forget timers. Closing the pool joins its
	// connections, so a nil result proves no CONNECTION is still delivering —
	// but a scheduled auto-bet or cleanup is a producer of its own, sleeping
	// on a timer that Close neither cancels nor waits for. Without an episode
	// registered before the goroutine starts, such a timer could fire after
	// the collector had finalized a session as COMPLETE, and the session's
	// claim to have observed everything would be false.
	//
	// CONTRACT, same as the record call: it must not block, allocate on the
	// shared connection, or do I/O. Register before the goroutine starts and
	// settle only after its last capture attempt returns.
	BeginPredictionProducerEpisode() func()
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

// newRoundIncarnation mints the identity of ONE local round admission: this
// pool instance plus a pool-local admission counter. It is called exactly where
// a new event/control pair is published into the pool's maps, so concurrent
// successful admissions of the same event id necessarily receive different
// ids — which a value derived from the channel and event id could not do.
func (p *WebSocketPool) newRoundIncarnation() string {
	return "round:" + p.instanceID + ":" + strconv.FormatUint(p.roundAdmissions.Add(1), 10)
}

// roundIncarnation reads the local round identity of a currently tracked round,
// or "" when no round is tracked under that event id.
//
// It takes p.mu for reading, so it must NEVER be called from a path that
// already holds p.mu. The two paths that do — removePrediction and
// sweepStaleLocked — capture the incarnation themselves before deleting the
// entry, which they must do anyway: after the delete there is nothing to read.
func (p *WebSocketPool) roundIncarnation(eventID string) string {
	if eventID == "" {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if rc := p.control[eventID]; rc != nil {
		return rc.incarnation
	}
	return ""
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
	// The store's schema makes a round incarnation and a retention-group owner
	// channel exist together or not at all: the incarnation is what makes
	// whole-round retention and whole-round erasure well defined, and a
	// retention group with no round is not a group. Enforcing it HERE, on the
	// one path every fact leaves by, means no call site can produce a fact the
	// store would have to reject — and a fact about a round this pool never
	// admitted is honestly an individual fact about a channel, not a member of
	// a retention group. It keeps its routed identity either way, so a privacy
	// erasure still reaches it.
	if obs.RoundIncarnationID == "" {
		obs.RetentionGroupOwnerChannelID = ""
		obs.RetentionGroupOwnerLogin = ""
	}
	if obs.ReceivedAtMS == 0 {
		obs.ReceivedAtMS = time.Now().UnixMilli()
	}
	if obs.ProducerTimeSource == "" {
		obs.ProducerTimeSource = ObsTimeReceiver
	}
	(*sinkPtr).RecordPredictionObservation(obs)
}

// beginEpisode registers a producer episode with the sink, if one is wired.
// The returned settle function is always safe to call and is a no-op when
// nothing is observing.
func (p *WebSocketPool) beginEpisode() func() {
	sinkPtr := p.observationSink.Load()
	if sinkPtr == nil || *sinkPtr == nil {
		return func() {}
	}
	return (*sinkPtr).BeginPredictionProducerEpisode()
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
	// A frame's Timestamp is ALWAYS set — the parser falls back to this
	// process's own clock — so the value alone cannot say whether a producer
	// stated it. Only its recorded provenance can, and a receiver-clock
	// reading is not a producer time at all: it is left absent rather than
	// stored as one the producer never gave.
	switch msg.TimestampSource {
	case TimestampFromProducer:
		obs.ProducerAtMS = msg.Timestamp.UnixMilli()
		obs.ProducerTimeSource = ObsTimeProducer
	case TimestampFromServer:
		obs.ProducerAtMS = msg.Timestamp.UnixMilli()
		obs.ProducerTimeSource = ObsTimeServer
	default:
		obs.ProducerAtMS = 0
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
func observationRoundIdentity(obs *PredictionObservation, eventID, channelID, login, incarnation string) {
	obs.EventID = eventID
	obs.RoundIncarnationID = incarnation
	obs.RetentionGroupOwnerChannelID = channelID
	obs.RetentionGroupOwnerLogin = login
	obs.RoutedChannelID = channelID
	obs.RoutedLogin = login
}

// wirePresence classifies one key of a frame WITHOUT recording its content.
// want reports whether a present, non-null value had the shape this path
// expects.
func wirePresence(data map[string]interface{}, key string, wellTyped func(interface{}) bool) string {
	if data == nil {
		return ObsNotObserved
	}
	raw, found := data[key]
	switch {
	case !found:
		return ObsAbsentOnWire
	case raw == nil:
		return ObsNullOnWire
	case !wellTyped(raw):
		return ObsInvalid
	default:
		return ObsPresent
	}
}

// isList / isObject / isString are the shapes the Prediction frames use.
func isList(v interface{}) bool   { _, ok := v.([]interface{}); return ok }
func isObject(v interface{}) bool { _, ok := v.(map[string]interface{}); return ok }

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
	// Project ONE past the store's ceiling and stop. The extra entry is not
	// data the store will keep: it is what lets the store SEE that the round
	// exceeded the cap and drop the whole fact, which is what the caps
	// require. Silently projecting the first 64 of 70 outcomes would commit a
	// fact that looks complete and is not.
	if n > maxObservedOutcomes+1 {
		n = maxObservedOutcomes + 1
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
			// The COUNT of entries, never an entry. Reported truthfully even
			// when it exceeds the scan ceiling: the store drops a fact whose
			// cohort is larger than the bound, and it can only do that if the
			// real size reaches it. Nothing here reads an element, so no
			// predictor identity is representable at any size.
			o.TopPredictorsExamined = len(tp)
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
		Presence: map[string]string{missing: ObsAbsentOnWire},
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
	// An event-created frame is observed BEFORE any admission decision, so it
	// usually names no local round; an event-updated frame for a tracked round
	// names the admission that is tracking it.
	obs.RoundIncarnationID = p.roundIncarnation(eventID)

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
	payload.Presence["outcomes"] = wirePresence(eventData, "outcomes", isList)
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
	p.observeScheduleDecisionOfRound(msg, streamer, eventID, p.roundIncarnation(eventID),
		status, phase, decision, reason, counters)
}

// observeScheduleDecisionOfRound is observeScheduleDecision for the one caller
// that has just admitted the round and therefore holds its incarnation
// directly. Reading it back through the map would be a second lookup that a
// concurrent cleanup could lose.
func (p *WebSocketPool) observeScheduleDecisionOfRound(msg *PubSubMessage, streamer streamerIdentity, eventID, incarnation, status, phase, decision, reason string, counters map[string]int64) {
	if !p.observing() {
		return
	}
	obs := observationFromMessage(msg, streamer)
	obs.Kind = ObsKindScheduleDecision
	obs.EventID = eventID
	obs.RoundIncarnationID = incarnation
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
	p.observeUserFrameOfRound(msg, streamer, eventID, p.roundIncarnation(eventID), kind, phase, reason, counters, present)
}

// observeUserFrameOfRound is observeUserFrame for the admitted terminal
// delivery, which captured the round's incarnation inside the SAME critical
// section that reached the admission verdict. Looking it up again afterwards
// could miss it: cleanup may already have removed the round.
func (p *WebSocketPool) observeUserFrameOfRound(msg *PubSubMessage, streamer streamerIdentity, eventID, incarnation, kind, phase, reason string, counters map[string]int64, present map[string]string) {
	if !p.observing() {
		return
	}
	obs := observationFromMessage(msg, streamer)
	obs.Kind = kind
	obs.EventID = eventID
	obs.RoundIncarnationID = incarnation
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
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "TRANSPORT"
	}
	// Classify what remains by TYPE, never by text — a message can carry a
	// provider string, a URL or a transaction id. A transport failure is a
	// local/network fault and must not read as a Twitch rejection: conflating
	// them would make the trail claim Twitch refused a bet it never received.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return "TRANSPORT"
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return "TRANSPORT"
	}
	var syscallErr *os.SyscallError
	if errors.As(err, &syscallErr) {
		return "TRANSPORT"
	}
	return "REJECTED_BY_TWITCH"
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
func (p *WebSocketPool) observeRoundCleanup(eventID, channelID, login, incarnation, phase, reason string) {
	p.observeRoundFactOf(eventID, channelID, login, incarnation, ObsKindRoundCleanup, ObservationPayload{
		Phase:      phase,
		ReasonCode: reason,
	})
}

// observeRoundFact records a fact about a tracked round that did not arrive on
// a frame — an auto decision, a placement call, a manual control phase, a
// cleanup. The round identity is taken from the tracked round itself.
func (p *WebSocketPool) observeRoundFact(eventID, channelID, login, kind string, payload ObservationPayload) {
	p.observeRoundFactOf(eventID, channelID, login, p.roundIncarnation(eventID), kind, payload)
}

// observeRoundFactOf is observeRoundFact for the cleanup paths, which must pass
// the incarnation they captured before deleting the round's control entry:
// after the delete there is nothing left to look up, and two of them run under
// the pool write lock that a lookup would try to read-acquire.
func (p *WebSocketPool) observeRoundFactOf(eventID, channelID, login, incarnation, kind string, payload ObservationPayload) {
	if !p.observing() {
		return
	}
	var obs PredictionObservation
	observationRoundIdentity(&obs, eventID, channelID, login, incarnation)
	obs.Kind = kind
	obs.ProducerTimeSource = ObsTimeReceiver
	obs.ReceivedAtMS = time.Now().UnixMilli()
	obs.Payload = payload
	p.observe(obs)
}
