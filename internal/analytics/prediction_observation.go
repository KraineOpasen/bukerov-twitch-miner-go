package analytics

// P1 — immutable Prediction observations.
//
// This file owns the append-only audit trail of what the Prediction subsystem
// SAW and DECIDED: one row per observed fact in prediction_observations, plus
// one control row per collector run in prediction_observation_sessions
// (analytics migration v6). It is a pure OBSERVER. Nothing here may change a
// betting decision, a stake, a timer, the number/arguments/results of any
// Twitch call, the manual/auto split, the A3 admission verdict, the exact
// points ledger, or anything the dashboard reads. Every failure mode is borne
// by P1 alone: a fact that cannot be captured or written is DROPPED and its
// collector session is finalized INCOMPLETE — never retried into a business
// path, never surfaced as an error to a producer.
//
// Ownership. There is exactly ONE persistence owner (the shared
// *database.DB opened once with SetMaxOpenConns(1)) and exactly one writer:
// the Service-owned collector goroutine started by Service.Start. Producers
// (the PubSub pool/connections, the miner's manual relay, the eligibility
// decisions) never touch SQLite: they build a bounded immutable copy, bump a
// few atomics and perform ONE nonblocking send on a capacity-512 private
// queue. No I/O, no JSON normalization, no callback and no wait ever happens
// under a WebSocket, pool or placement lock.
//
// Non-interference. Every business write path claims txPriority before it
// touches the shared connection; the collector holds at most ONE cancellable
// lease at a time and yields it immediately. A business writer therefore waits
// only for a bounded cancellation of a single one-row transaction, and the
// collector accepts the loss.
//
// Privacy. Only CLOSED, typed, sanitized projections are persisted or hashed.
// Raw PubSub/GraphQL/HTTP bodies, Topic.String(), the transport
// EventFingerprint, tokens, cookies, headers, Twitch transaction identifiers,
// raw error strings, predictor identities and arbitrary/unknown enum strings
// never reach this package's storage: an unrecognized enum becomes the closed
// sentinel ValueUnknown, never the raw text.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// ObservationProducerRevision stamps every collector session with the exact
// producer contract the rows were written under: the observation schema
// generation and the policy base commit that authorized it. A reader that
// finds an unfamiliar revision knows the rows were produced by a different
// contract and must not assume this one's invariants.
const ObservationProducerRevision = "obs-v1|policy-0f98c316a8bcc24e055e2a0006ca6f96d1ff3a42"

// ObservationPayloadVersion is the version of the closed typed projection
// stored in payload_json. It is bumped only when the projection's meaning
// changes, never for an additive optional field.
const ObservationPayloadVersion = 1

// ValueUnknown is the closed sentinel every enum falls back to. An input that
// is not a member of a closed set is recorded as ValueUnknown — the raw string
// is never persisted, never hashed and never logged from here.
const ValueUnknown = "UNKNOWN"

// Observation kinds — the closed set of families this trail records.
const (
	// KindSourceUnknown: a routed Prediction-domain frame whose family could
	// not be classified. Recorded so an unclassifiable input is visible as a
	// fact rather than silently absent.
	KindSourceUnknown = "source_unknown"
	// KindChannelEvent: a predictions-channel-v1 round lifecycle frame.
	KindChannelEvent = "channel_event"
	// KindScheduleDecision: the decision to schedule (or not schedule) an
	// auto-bet for a newly seen round.
	KindScheduleDecision = "schedule_decision"
	// KindAutoDecision: the automated strategy decision taken when a
	// scheduled round comes due.
	KindAutoDecision = "auto_decision"
	// KindManualControl: an operator-initiated placement relayed through the
	// miner (MANUAL_MINER_ROOT) or taken directly against the pool
	// (MANUAL_DIRECT_ROOT), and the phases it passes through.
	KindManualControl = "manual_control"
	// KindPlacement: the single Twitch placement call — one fact immediately
	// before it and one immediately after. Never wraps, retries or alters it.
	KindPlacement = "placement"
	// KindUserPredictionMade: the predictions-user-v1 confirmation that a
	// placement was accepted by Twitch. Outside A3 by design.
	KindUserPredictionMade = "user_prediction_made"
	// KindUserTerminal: the predictions-user-v1 terminal result delivery and
	// the admission verdict A3 already reached for it — observed, never
	// re-decided.
	KindUserTerminal = "user_terminal"
	// KindRoundCleanup: the round's tracked state being dropped.
	KindRoundCleanup = "round_cleanup"
)

// observationKinds is the closed kind set, in schema order.
var observationKinds = []string{
	KindSourceUnknown,
	KindChannelEvent,
	KindScheduleDecision,
	KindAutoDecision,
	KindManualControl,
	KindPlacement,
	KindUserPredictionMade,
	KindUserTerminal,
	KindRoundCleanup,
}

// Source topic types — the closed projection of the Prediction-relevant
// PubSub topics. Only the topic TYPE is stored; Topic.String() (which
// concatenates the type with an account-scoped identifier) never is.
const (
	TopicTypePredictionsChannel = "predictions-channel-v1"
	TopicTypePredictionsUser    = "predictions-user-v1"
)

var observationTopicTypes = []string{
	TopicTypePredictionsChannel,
	TopicTypePredictionsUser,
	ValueUnknown,
}

// Source message types — the closed projection of the Prediction-relevant
// PubSub message types.
const (
	MessageTypeEventCreated     = "event-created"
	MessageTypeEventUpdated     = "event-updated"
	MessageTypePredictionMade   = "prediction-made"
	MessageTypePredictionResult = "prediction-result"
)

var observationMessageTypes = []string{
	MessageTypeEventCreated,
	MessageTypeEventUpdated,
	MessageTypePredictionMade,
	MessageTypePredictionResult,
	ValueUnknown,
}

// Producer time sources classify where producer_at_ms came from, so a reader
// never mistakes a locally derived timestamp for one Twitch asserted.
const (
	// TimeSourceProducer: the frame carried its own event timestamp.
	TimeSourceProducer = "PRODUCER"
	// TimeSourceServer: the frame carried only a server time.
	TimeSourceServer = "SERVER"
	// TimeSourceReceiver: no producer time existed; producer_at_ms is NULL and
	// only received_at_ms is meaningful.
	TimeSourceReceiver = "RECEIVER"
)

var observationTimeSources = []string{TimeSourceProducer, TimeSourceServer, TimeSourceReceiver, ValueUnknown}

// Collector session close states.
const (
	// SessionOpen: the collector is running, or the process died before it
	// could finalize. An OPEN row older than the current epoch is a
	// crash-left session and is never automatically pruned.
	SessionOpen = "OPEN"
	// SessionComplete: the collector finalized having dropped nothing and
	// left no obligation unsettled.
	SessionComplete = "COMPLETE"
	// SessionIncomplete: the collector finalized after at least one drop,
	// unsettled obligation, post-fence producer or uncertain producer
	// shutdown. Its facts are still exact; the SET of facts is not provably
	// whole.
	SessionIncomplete = "INCOMPLETE"
)

// Session readings — how a reader must classify a session before drawing any
// conclusion from the facts that belong to it.
const (
	// ReadingUnfinalized: close_state is OPEN. Either the collector is live
	// or it died; completeness is unknown and no absence may be inferred.
	ReadingUnfinalized = "UNFINALIZED"
	// ReadingIntegrityError: the row contradicts itself (a finalized session
	// with no close time, an unknown close state, negative counters, or
	// committed_count disagreeing with the facts actually present). Nothing
	// may be concluded from the session at all.
	ReadingIntegrityError = "INTEGRITY_ERROR"
	// ReadingAdministrativelyTruncated: the session finalized coherently but
	// facts were removed afterwards by retention or a privacy erasure. The
	// surviving facts are exact; the set is deliberately not whole.
	ReadingAdministrativelyTruncated = "ADMINISTRATIVELY_TRUNCATED"
	// ReadingAsFinalized: the session finalized coherently and the facts
	// present are exactly the ones it committed. Only here may a reader treat
	// the absence of a fact as evidence.
	ReadingAsFinalized = "AS_FINALIZED"
)

// Compile-time bounds. These are frozen: they are the contract that keeps an
// observer from ever becoming a load source on the shared connection or an
// unbounded consumer of the operator's disk.
//
// Where each is ENFORCED, so none of them is merely decorative:
//   - queue capacity: the channel's own size; a full queue drops.
//   - outcomes, top predictors, string, payload: by the producer's bounded
//     projection and by sanitizeObservation/marshalObservationPayload.
//   - session rows/bytes: per fact, from atomics, in the collector's write
//     path (withinSessionBounds).
//   - store rows/bytes/sessions: by the collector's maintenance pass, which
//     pauses capture when a bound is reached and resumes when retention frees
//     space.
//   - round rows/bytes: by the retention unit — one whole round is the
//     largest thing a single pruning transaction removes.
//   - deletion identity and proved-union rows/bytes: the erasure's cost
//     envelope, exercised by the identity-purge pilot.
const (
	// ObservationQueueCapacity is the collector's private queue depth. A full
	// queue drops rather than blocking a producer.
	ObservationQueueCapacity = 512
	// MaxObservationOutcomes bounds the outcomes projected into one fact.
	MaxObservationOutcomes = 64
	// MaxTopPredictorsExamined bounds how many top predictors a producer may
	// examine when counting them. ZERO identities are ever retained.
	MaxTopPredictorsExamined = 256
	// MaxObservationString bounds any single projected string (4 KiB).
	MaxObservationString = 4 << 10
	// MaxObservationPayloadBytes bounds one fact's payload_json (64 KiB).
	MaxObservationPayloadBytes = 64 << 10
	// MaxRoundRows / MaxRoundBytes bound one round's retained facts.
	MaxRoundRows  = 128
	MaxRoundBytes = 1 << 20
	// MaxSessionRows / MaxSessionBytes bound one collector session.
	MaxSessionRows  = 65536
	MaxSessionBytes = 256 << 20
	// MaxStoreRows / MaxStoreBytes / MaxStoreSessions bound the whole store.
	MaxStoreRows     = 262144
	MaxStoreBytes    = 1 << 30
	MaxStoreSessions = 4096
	// MaxDeletionIdentityRows / MaxDeletionIdentityBytes bound one erasure
	// identity (a single routed or retention-group-owner channel).
	MaxDeletionIdentityRows  = 4096
	MaxDeletionIdentityBytes = 16 << 20
	// MaxProvedIdentityUnionRows / MaxProvedIdentityUnionBytes bound the union
	// of a proved parent and channel in one erasure.
	MaxProvedIdentityUnionRows  = 8192
	MaxProvedIdentityUnionBytes = 32 << 20
	// ObservationWriteDeadline is the hard per-fact deadline covering the
	// priority lease AND the one-row transaction. There is no retry.
	ObservationWriteDeadline = 5 * time.Millisecond
	// observationPruneUnit bounds one retention transaction's NULL-round or
	// factless-session batch.
	observationPruneUnit = 128
	// observationPruneUnitsPerPass bounds how many bounded units one
	// maintenance pass removes. Each unit is still its own transaction, so a
	// business writer never waits for more than one of them; the cap keeps a
	// single pass from monopolizing the connection on a large backlog.
	observationPruneUnitsPerPass = 16
	// observationMaintenanceInterval paces the worker-owned retention pass.
	observationMaintenanceInterval = 10 * time.Minute
)

// errObservationDisabled marks a collector that never bootstrapped (or was
// disabled by a privacy erasure): capture is off and every enqueue is dropped.
var errObservationDisabled = errors.New("analytics: prediction observations disabled")

// errObservationAtCapacity marks a store that has reached a hard bound with
// nothing left to prune. Capture pauses rather than growing past the bound.
var errObservationAtCapacity = errors.New("analytics: observation store at capacity")

// ObservationOutcome is the sanitized projection of ONE round outcome. It
// carries aggregate shape only: no predictor identity, no display text, no
// raw Twitch identifier.
type ObservationOutcome struct {
	// Slot is the outcome's positional index within the round.
	Slot int `json:"slot"`
	// Color is a closed enum (BLUE, PINK) or ValueUnknown.
	Color string `json:"color,omitempty"`
	// TotalPoints/TotalUsers are the aggregate pool figures.
	TotalPoints int64 `json:"totalPoints"`
	TotalUsers  int64 `json:"totalUsers"`
	// TopPredictorsExamined counts how many top predictors the producer
	// looked at while deriving the aggregates above (bounded by
	// MaxTopPredictorsExamined). No identity of any of them is retained.
	TopPredictorsExamined int `json:"topPredictorsExamined,omitempty"`
}

var observationOutcomeColors = []string{"BLUE", "PINK", ValueUnknown}

// Closed key sets for the payload's counter and presence maps. A key outside
// these sets is dropped by sanitizeObservationPayload; a value outside the
// presence set becomes ValueUnknown.
var (
	observationCounterKeys = []string{
		"outcomeCount", "trackedRounds", "windowSeconds", "closingBetAfterSeconds",
		"minimumPoints", "balance", "stake", "returnedStake", "payout", "odds1e4",
		"attempt", "queueDepth", "topPredictorsExamined",
	}
	observationPresenceKeys = []string{
		"event", "outcomes", "result", "decision", "balance", "pool",
		"correlationToken", "terminalVerdict", "channelIdentity",
	}
	observationPresenceValues = []string{"PRESENT", "ABSENT", ValueUnknown}
)

// Closed phase / decision / reason / error-class vocabularies. These are the
// only strings that may appear in the corresponding payload fields.
var (
	observationPhases = []string{
		// channel_event
		"ROUND_CREATED", "ROUND_UPDATED",
		// schedule_decision
		"SCHEDULE_CONSIDERED", "SCHEDULE_ACCEPTED", "SCHEDULE_SKIPPED",
		// auto_decision
		"AUTO_DUE", "AUTO_DECIDED", "AUTO_SKIPPED",
		// manual_control
		"MANUAL_MINER_ROOT", "MANUAL_DIRECT_ROOT", "MANUAL_POOL_LOOKUP",
		"MANUAL_ELIGIBILITY", "MANUAL_ARGUMENTS", "MANUAL_RESERVATION",
		"MANUAL_VALIDATION", "MANUAL_EXECUTION", "MANUAL_SKIPPED",
		// placement
		"CALL_STARTED", "CALL_RETURNED",
		// user_prediction_made
		"PLACEMENT_CONFIRMED",
		// user_terminal
		"TERMINAL_DELIVERED", "TERMINAL_ADMITTED", "TERMINAL_REJECTED",
		// round_cleanup
		"CLEANUP_SCHEDULED", "CLEANUP_APPLIED",
		// source_unknown
		"UNCLASSIFIED",
		ValueUnknown,
	}
	observationRoundStates = []string{"ACTIVE", "LOCKED", "RESOLVED", "CANCELED", ValueUnknown}
	observationDecisions   = []string{"PLACE", "SKIP", "DEFER", "NONE", ValueUnknown}
	observationReasonCodes = []string{
		"OK", "TOGGLE_OFF", "ALREADY_TRACKED", "NOT_ACTIVE", "NOT_ELIGIBLE",
		"WINDOW_ELAPSED", "BELOW_MINIMUM_POINTS", "NO_POOL", "NO_ROUND",
		"NO_DECISION", "ALREADY_PLACED", "FILTER_REJECTED", "STRATEGY_NO_CHOICE",
		"DUPLICATE", "CONFLICT", "ACCEPTED", "REJECTED", "REFUNDED", "WON", "LOST",
		ValueUnknown,
	}
	// observationErrorClasses is a CLOSED classification. A raw error string,
	// a Twitch transaction identifier or a provider message is never stored.
	observationErrorClasses = []string{
		"NONE", "TRANSPORT", "REJECTED_BY_TWITCH", "INVALID_ARGUMENT",
		"NOT_ENOUGH_POINTS", "ROUND_CLOSED", "INTERNAL", ValueUnknown,
	}
)

// ObservationPayload is the CLOSED, sanitized typed projection persisted as
// payload_json and covered by observation_sha256. Every field is either a
// member of a closed vocabulary, a bounded number, or a bounded aggregate.
// There is deliberately no free-text field.
type ObservationPayload struct {
	// Phase is the fact's position within its kind's lifecycle. Required.
	Phase string `json:"phase"`
	// RoundState is the round's lifecycle state as the producer saw it.
	RoundState string `json:"roundState,omitempty"`
	// Decision is what the producer decided (never what P1 thinks it should
	// have decided).
	Decision string `json:"decision,omitempty"`
	// ReasonCode explains the decision or the terminal verdict.
	ReasonCode string `json:"reasonCode,omitempty"`
	// ErrorClass is the closed classification of a failure. Never a raw error.
	ErrorClass string `json:"errorClass,omitempty"`
	// Manual distinguishes an operator-initiated action from an automated one.
	Manual *bool `json:"manual,omitempty"`
	// OutcomeSlot is the positional index the producer chose, when it chose.
	OutcomeSlot *int `json:"outcomeSlot,omitempty"`
	// Outcomes is the bounded aggregate projection of the round's outcomes.
	Outcomes []ObservationOutcome `json:"outcomes,omitempty"`
	// Counters holds bounded numeric facts under a closed key set.
	Counters map[string]int64 `json:"counters,omitempty"`
	// Presence records whether a structural part of the frame was there,
	// without recording any of its content.
	Presence map[string]string `json:"presence,omitempty"`
}

// PredictionObservation is the immutable fact a producer hands to the
// collector. It is a VALUE: the producer fills it from a bounded copy of what
// it already holds and never mutates it afterwards. Identity fields carry
// Twitch channel identifiers and the internal analytics parent id resolved
// later by the collector; no login, token or payload text is ever carried.
type PredictionObservation struct {
	// PoolInstanceID identifies the PubSub pool instance that observed the
	// fact. Required for every fact — a fact with no pool provenance is
	// rejected rather than stored with a NULL.
	PoolInstanceID string

	// RoutedChannelID is the channel the frame was ROUTED to (the streamer
	// whose topic delivered it). RoutedStreamerID is resolved by the
	// collector from the login, when one is known.
	RoutedChannelID string
	RoutedLogin     string

	// RoundOwnerChannelID is the channel that OWNS the round, when the frame
	// identified one. It is provenance only: it never widens an erasure.
	RoundOwnerChannelID string
	RoundOwnerLogin     string

	// RetentionGroupOwnerChannelID is the identity a whole round is retained
	// and erased under. When it is set the fact belongs to a round; when it
	// is empty the fact is a NULL-round fact retained individually.
	RetentionGroupOwnerChannelID string
	RetentionGroupOwnerLogin     string

	// sequence is the causal position this fact was RESERVED at, on the
	// producer's own call, before the queue. See offer.
	sequence int64

	// RoundIncarnationID names the producer's LOCAL admission of the round —
	// the pool instance that admitted it plus that pool's admission counter.
	// It is supplied by the producer and never re-derived here: only the
	// producer knows which of several admissions of one Twitch event a fact
	// belongs to. Empty when the fact is about no admitted round, in which
	// case the fact is its own retention unit.
	RoundIncarnationID string

	// EventID is the Twitch prediction event identity, when the frame carried
	// one. Deliberately NOT unique in the store: many facts describe one
	// round.
	EventID string

	// Kind, SourceTopicType, SourceMessageType are closed enums.
	Kind              string
	SourceTopicType   string
	SourceMessageType string

	// SourceFingerprint is a P1-OWN digest over the sanitized projection's
	// source identity. It is never the transport EventFingerprint (which
	// hashes the raw inner frame together with an account-scoped topic).
	SourceFingerprint string

	// ProducerAtMS is the time the producer attributes to the fact, with
	// ProducerTimeSource saying where that time came from. Zero means none.
	ProducerAtMS       int64
	ProducerTimeSource string

	// ReceivedAtMS is the collector-local receipt time. Always set.
	ReceivedAtMS int64

	// Connection provenance, when the fact came off a pooled connection.
	ConnectionIndex      int
	ConnectionGeneration uint64
	ConnectionSequence   uint64
	ConnectionKnown      bool

	// Payload is the closed typed projection.
	Payload ObservationPayload

	// generation is the capture generation the producer read before building
	// the fact. The collector drops a fact whose generation has since been
	// invalidated by a privacy erasure, so a queued fact can never resurrect
	// an erased identity. Set by the Service on enqueue.
	generation uint64
}

// ObservationSessionRecord is one collector session as stored.
type ObservationSessionRecord struct {
	CollectorEpoch                 int64
	CollectorSessionID             string
	ProducerRevision               string
	StartedAtMS                    int64
	ClosedAtMS                     int64
	ClosedAtKnown                  bool
	CloseState                     string
	LastAssignedSequence           int64
	LastAssignedSequenceKnown      bool
	CommittedCount                 int64
	DroppedCount                   int64
	UnsettledObligationCount       int64
	PostFenceProducerCount         int64
	ProducerShutdownUncertainCount int64
}

// ObservationRecord is one persisted fact as read back.
type ObservationRecord struct {
	ID                            int64
	ObservationID                 string
	CollectorSessionID            string
	CollectorEpoch                int64
	CollectorSequence             int64
	PoolInstanceID                string
	RoundIncarnationID            string
	RoutedStreamerID              int64
	RoutedChannelID               string
	RoundOwnerStreamerID          int64
	RoundOwnerChannelID           string
	RetentionGroupOwnerStreamerID int64
	RetentionGroupOwnerChannelID  string
	EventID                       string
	Kind                          string
	SourceTopicType               string
	SourceMessageType             string
	SourceFingerprint             string
	ProducerAtMS                  int64
	ProducerTimeSource            string
	ReceivedAtMS                  int64
	ConnectionIndex               int64
	ConnectionGeneration          int64
	ConnectionSequence            int64
	PayloadVersion                int64
	Payload                       ObservationPayload
	// PayloadUndecodable marks a row whose stored payload could not be
	// decoded. The row is still returned — its columns are intact and its
	// digest still witnesses them — but its payload must not be read.
	PayloadUndecodable bool
	ObservationSHA256  string
}

// ObservationSessionReading is a session plus the classification a reader MUST
// apply before drawing any conclusion from its facts.
type ObservationSessionReading struct {
	Session ObservationSessionRecord
	// Reading is one of the Reading* constants.
	Reading string
	// FactsPresent is how many of the session's facts still exist.
	FactsPresent int64
	// Detail explains a non-AS_FINALIZED reading in closed terms.
	Detail string
}

// ---------------------------------------------------------------------------
// Sanitization
// ---------------------------------------------------------------------------

// closedValue maps v onto a closed vocabulary. Anything else — including a
// value that merely differs in case or carries trailing space — becomes
// ValueUnknown. The rejected input is never returned, stored or logged.
func closedValue(v string, allowed []string) string {
	v = strings.TrimSpace(v)
	if len(v) > MaxObservationString {
		return ValueUnknown
	}
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return ValueUnknown
}

// closedOptional is closedValue for a column that may legitimately be absent:
// an empty input stays empty (SQL NULL), anything unrecognized becomes
// ValueUnknown.
func closedOptional(v string, allowed []string) string {
	if strings.TrimSpace(v) == "" {
		return ""
	}
	return closedValue(v, allowed)
}

// boundedIdentifier caps an opaque identifier (a channel id, an event id, a
// pool instance id) at MaxObservationString and trims it. These are
// identifiers the store already keys on; they are never free text.
// comparableIdentity normalizes an identity that is used to MATCH stored rows
// — an erasure selector, a fence key — rather than to be stored. It applies no
// ceiling: every stored value is already within one, so a value above the
// ceiling simply matches nothing. Shortening it here would turn an exact match
// into a prefix and let an erasure delete by a prefix of a channel id.
func comparableIdentity(v string) string { return strings.TrimSpace(v) }

func comparableLogin(v string) string { return strings.ToLower(strings.TrimSpace(v)) }

func boundedIdentifier(v string) (string, bool) {
	v = strings.TrimSpace(v)
	// Over the ceiling the FACT is refused, not shortened. A truncated
	// identifier is a different identifier: it names a different channel, a
	// different event, a different pool, and it is stored, hashed and erased
	// as if it were the real one. Refusing costs one fact and one counted
	// drop; accepting costs the trail its meaning.
	return v, len(v) <= MaxObservationString
}

// sanitizeObservationPayload projects an arbitrary producer payload onto the
// closed vocabulary. No unrecognized text survives. ok is false when the input
// breaches a frozen ceiling, which refuses the whole fact rather than storing a
// shortened version of it.
func sanitizeObservationPayload(in ObservationPayload) (ObservationPayload, bool) {
	out := ObservationPayload{
		Phase:      closedValue(in.Phase, observationPhases),
		RoundState: closedOptional(in.RoundState, observationRoundStates),
		Decision:   closedOptional(in.Decision, observationDecisions),
		ReasonCode: closedOptional(in.ReasonCode, observationReasonCodes),
		ErrorClass: closedOptional(in.ErrorClass, observationErrorClasses),
	}
	if in.Manual != nil {
		m := *in.Manual
		out.Manual = &m
	}
	if in.OutcomeSlot != nil {
		switch s := *in.OutcomeSlot; {
		case s >= MaxObservationOutcomes:
			// A slot past the outcome ceiling names an outcome that could
			// never have been stored: the fact is refused, not renumbered.
			return out, false
		case s >= 0:
			slot := s
			out.OutcomeSlot = &slot
		}
		// A negative slot is the ABSENCE of a chosen outcome, not a breach:
		// it stays absent.
	}
	if n := len(in.Outcomes); n > 0 {
		// Over the ceiling the fact is refused. Keeping the first 64 of 70
		// outcomes would store a round's aggregate cohort as if it were
		// complete, and nothing downstream could tell it was not.
		if n > MaxObservationOutcomes {
			return out, false
		}
		out.Outcomes = make([]ObservationOutcome, 0, n)
		for i := 0; i < n; i++ {
			o := in.Outcomes[i]
			if o.TopPredictorsExamined > MaxTopPredictorsExamined {
				// The cohort is larger than the bounded scan can honestly
				// account for, so the fact is refused rather than reporting a
				// clamped count as the real one.
				return out, false
			}
			examined := o.TopPredictorsExamined
			if examined < 0 {
				examined = 0
			}
			out.Outcomes = append(out.Outcomes, ObservationOutcome{
				Slot:                  i,
				Color:                 closedOptional(o.Color, observationOutcomeColors),
				TotalPoints:           o.TotalPoints,
				TotalUsers:            o.TotalUsers,
				TopPredictorsExamined: examined,
			})
		}
	}
	if len(in.Counters) > 0 {
		out.Counters = make(map[string]int64, len(in.Counters))
		for _, k := range observationCounterKeys {
			if v, ok := in.Counters[k]; ok {
				out.Counters[k] = v
			}
		}
		if len(out.Counters) == 0 {
			out.Counters = nil
		}
	}
	if len(in.Presence) > 0 {
		out.Presence = make(map[string]string, len(in.Presence))
		for _, k := range observationPresenceKeys {
			if v, ok := in.Presence[k]; ok {
				out.Presence[k] = closedValue(v, observationPresenceValues)
			}
		}
		if len(out.Presence) == 0 {
			out.Presence = nil
		}
	}
	return out, true
}

// sanitizeObservation projects a producer's fact onto the closed contract.
// The returned value is what is hashed and stored; ok is false when the fact
// cannot be stored AT ALL.
//
// There are two different failures here and only one of them is a projection.
// An unrecognized enum value becomes UNKNOWN: the fact is still exactly true,
// it just says "a value this build does not know" instead of carrying raw text
// into the store. A value over a frozen ceiling is not like that. Shortening a
// 5 KiB channel id, keeping 64 of 70 outcomes, or replacing an oversized
// payload with a stub produces a fact that is FALSE and indistinguishable from
// a true one — and it is then hashed, counted as committed, and erased as if it
// were real. Those are refused whole, and the refusal is counted as a drop,
// which is what makes the session INCOMPLETE and says so.
func sanitizeObservation(in PredictionObservation, now int64) (PredictionObservation, bool) {
	overCeiling := false
	bounded := func(v string) string {
		s, ok := boundedIdentifier(v)
		if !ok {
			overCeiling = true
		}
		return s
	}
	login := func(v string) string {
		s, ok := canonicalObservationLogin(v)
		if !ok {
			overCeiling = true
		}
		return s
	}
	payload, payloadOK := sanitizeObservationPayload(in.Payload)
	out := PredictionObservation{
		PoolInstanceID:               bounded(in.PoolInstanceID),
		RoutedChannelID:              bounded(in.RoutedChannelID),
		RoutedLogin:                  login(in.RoutedLogin),
		RoundOwnerChannelID:          bounded(in.RoundOwnerChannelID),
		RoundOwnerLogin:              login(in.RoundOwnerLogin),
		RetentionGroupOwnerChannelID: bounded(in.RetentionGroupOwnerChannelID),
		RetentionGroupOwnerLogin:     login(in.RetentionGroupOwnerLogin),
		RoundIncarnationID:           bounded(in.RoundIncarnationID),
		EventID:                      bounded(in.EventID),
		Kind:                         closedValue(in.Kind, observationKinds),
		SourceTopicType:              closedOptional(in.SourceTopicType, observationTopicTypes),
		SourceMessageType:            closedOptional(in.SourceMessageType, observationMessageTypes),
		ProducerAtMS:                 in.ProducerAtMS,
		ProducerTimeSource:           closedValue(in.ProducerTimeSource, observationTimeSources),
		ReceivedAtMS:                 in.ReceivedAtMS,
		ConnectionIndex:              in.ConnectionIndex,
		ConnectionGeneration:         in.ConnectionGeneration,
		ConnectionSequence:           in.ConnectionSequence,
		ConnectionKnown:              in.ConnectionKnown,
		Payload:                      payload,
		generation:                   in.generation,
		sequence:                     in.sequence,
	}
	if out.Kind == ValueUnknown {
		out.Kind = KindSourceUnknown
	}
	if overCeiling || !payloadOK {
		return out, false
	}
	if out.PoolInstanceID == "" {
		return out, false
	}
	// A parent login without its channel identity would produce a row that
	// cannot be erased by channel; drop the parent rather than store a
	// half-identity the schema forbids.
	if out.RoutedChannelID == "" {
		out.RoutedLogin = ""
	}
	if out.RoundOwnerChannelID == "" {
		out.RoundOwnerLogin = ""
	}
	if out.RetentionGroupOwnerChannelID == "" {
		out.RetentionGroupOwnerLogin = ""
	}
	// A round incarnation and a retention-group owner exist together or not at
	// all: the incarnation is what makes whole-round retention and whole-round
	// erasure well defined, and a retention group with no round is not a group.
	// The producer already enforces this on its own emit path; enforcing it
	// again here is what makes the invariant hold for EVERY caller of this
	// store, not just the one producer that exists today.
	if out.RoundIncarnationID == "" {
		out.RetentionGroupOwnerChannelID = ""
		out.RetentionGroupOwnerLogin = ""
	} else if out.RetentionGroupOwnerChannelID == "" {
		out.RoundIncarnationID = ""
	}
	if out.ProducerAtMS <= 0 {
		out.ProducerAtMS = 0
		out.ProducerTimeSource = TimeSourceReceiver
	}
	if out.ReceivedAtMS <= 0 {
		out.ReceivedAtMS = now
	}
	// The source fingerprint is P1's OWN digest over the sanitized source
	// identity — never the transport EventFingerprint.
	out.SourceFingerprint = observationSourceFingerprint(out)
	// Last, the rendered size. Every field is individually within its ceiling
	// and the whole can still exceed the payload ceiling, so it is checked on
	// the bytes that would actually be stored.
	if _, ok := marshalObservationPayload(out.Payload); !ok {
		return out, false
	}
	return out, true
}

func canonicalObservationLogin(login string) (string, bool) {
	v, ok := boundedIdentifier(login)
	return strings.ToLower(v), ok
}

// observationSourceFingerprint digests the sanitized SOURCE identity of a
// fact: its kind, topic/message type, event and channel identities. Two
// deliveries of the same source event share a fingerprint, which is why the
// column is deliberately NOT unique — a duplicate delivery is a fact in its
// own right and must remain visible as one.
func observationSourceFingerprint(o PredictionObservation) string {
	h := sha256.New()
	for _, part := range []string{
		"prediction-observation-source-v1",
		o.Kind,
		o.SourceTopicType,
		o.SourceMessageType,
		o.EventID,
		o.RoutedChannelID,
		o.RetentionGroupOwnerChannelID,
		o.Payload.Phase,
	} {
		writeDigestPart(h, part)
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// writeDigestPart feeds one field into a digest LENGTH-PREFIXED rather than
// NUL-separated. A separator can be forged by a field that contains it; a
// length prefix cannot, so two different field sequences can never hash the
// same way.
func writeDigestPart(h hash.Hash, part string) {
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(part)))
	_, _ = h.Write(lenBuf[:])
	_, _ = h.Write([]byte(part))
}

// marshalObservationPayload renders the sanitized projection as canonical
// JSON. Go's encoder emits struct fields in declaration order and map keys in
// sorted order, so the same projection always renders the same bytes — which
// is what makes observation_sha256 reproducible.
//
// A payload over MaxObservationPayloadBytes is REFUSED. It used to be replaced
// by a minimal phase/reason stub and committed as a success: a fact that had
// quietly lost its content, was hashed as though that stub were what happened,
// and counted toward the session as a fact faithfully recorded. A refused fact
// is counted as a drop instead, which is both true and visible.
func marshalObservationPayload(p ObservationPayload) (string, bool) {
	b, err := json.Marshal(p)
	if err != nil || len(b) > MaxObservationPayloadBytes {
		return "", false
	}
	return string(b), true
}

// observationDigest is the content hash of one stored fact: a canonical digest
// over every persisted column except the digest itself. It lets a reader prove
// a row was not altered after it was written.
func observationDigest(o PredictionObservation, observationID, sessionID string, epoch, seq int64, incarnation, payloadJSON string, routedID, ownerID, retentionID interface{}) string {
	h := sha256.New()
	write := func(parts ...string) {
		for _, p := range parts {
			writeDigestPart(h, p)
		}
	}
	write(
		"prediction-observation-v1",
		observationID,
		sessionID,
		fmt.Sprintf("%d", epoch),
		fmt.Sprintf("%d", seq),
		o.PoolInstanceID,
		incarnation,
		o.RoutedChannelID,
		o.RoundOwnerChannelID,
		o.RetentionGroupOwnerChannelID,
		o.EventID,
		o.Kind,
		o.SourceTopicType,
		o.SourceMessageType,
		o.SourceFingerprint,
		fmt.Sprintf("%d", o.ProducerAtMS),
		o.ProducerTimeSource,
		fmt.Sprintf("%d", o.ReceivedAtMS),
		// The resolved parent ids and the connection provenance are persisted
		// columns too, so the digest covers them: a witness that omitted them
		// could not detect a row whose parent or provenance had been altered.
		observationNullableDigestPart(routedID),
		observationNullableDigestPart(ownerID),
		observationNullableDigestPart(retentionID),
		observationConnectionDigestPart(o.ConnectionKnown, int64(o.ConnectionIndex)),
		observationConnectionDigestPart(o.ConnectionKnown, int64(o.ConnectionGeneration)),
		observationConnectionDigestPart(o.ConnectionKnown, int64(o.ConnectionSequence)),
		fmt.Sprintf("%d", ObservationPayloadVersion),
		payloadJSON,
	)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// observationNullableDigestPart renders a nullable parent id for the digest,
// distinguishing SQL NULL from any real id.
func observationNullableDigestPart(v interface{}) string {
	if v == nil {
		return "\x00NULL"
	}
	return fmt.Sprintf("%v", v)
}

// observationConnectionDigestPart renders a connection provenance column for
// the digest, distinguishing "no connection" from any real value.
func observationConnectionDigestPart(known bool, v int64) string {
	if !known {
		return "\x00NULL"
	}
	return fmt.Sprintf("%d", v)
}

// newCollectorSessionID mints a random, non-identifying session identifier.
func newCollectorSessionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("analytics: mint collector session id: %w", err)
	}
	return "obs-" + hex.EncodeToString(b[:]), nil
}

// ---------------------------------------------------------------------------
// Low-priority admission gate
// ---------------------------------------------------------------------------

// txPriority is the low-priority admission gate that keeps the observation
// writer from ever delaying a business write beyond a bounded cancellation.
//
// A business writer Claims before touching the shared connection: the claim
// cancels the single in-flight observation lease and waits for it to settle
// (bounded — an observation transaction is one row under a 5 ms deadline),
// then keeps new leases out until it Releases. The observation writer takes a
// lease only while no claim is outstanding, and its lease context is cancelled
// the instant one arrives.
//
// The gate touches neither the repository mutex nor the database handle, so it
// introduces no new lock order: a business path takes gate -> repo.mu -> DB,
// and the observation path takes gate -> DB, never repo.mu. A Tx helper
// running inside a caller-owned *sql.Tx never claims — the claim already
// happened at the top of the owning call.
type txPriority struct {
	mu sync.Mutex
	// claims counts outstanding business claims. While > 0, no new lease is
	// granted.
	claims int
	// active is the single in-flight observation lease, if any.
	active *observationLease
	// idle is closed and replaced whenever the active lease settles, so a
	// claimer can wait without polling.
	idle chan struct{}
	// free is closed and replaced whenever claims drops to zero, so the
	// observation writer can wait for the gate to clear.
	free chan struct{}
}

type observationLease struct {
	cancel context.CancelFunc
	done   chan struct{}
}

func newTxPriority() *txPriority {
	p := &txPriority{idle: make(chan struct{}), free: make(chan struct{})}
	close(p.idle) // no lease in flight
	close(p.free) // no claim outstanding
	return p
}

// Claim announces a business write. It returns the release function the caller
// must invoke when its work on the shared connection is done. Claim is safe on
// a nil gate (a repository built before the collector exists).
func (p *txPriority) Claim() func() {
	if p == nil {
		return func() {}
	}
	p.mu.Lock()
	if p.claims == 0 {
		p.free = make(chan struct{})
	}
	p.claims++
	lease := p.active
	idle := p.idle
	if lease != nil {
		lease.cancel()
	}
	p.mu.Unlock()

	if lease != nil {
		// Bounded: the observation transaction is a single row under a hard
		// deadline and its context has just been cancelled.
		<-idle
	}
	return p.release
}

func (p *txPriority) release() {
	p.mu.Lock()
	p.claims--
	if p.claims == 0 {
		close(p.free)
	}
	p.mu.Unlock()
}

// lease grants the observation writer permission to use the shared connection.
// It waits (bounded by ctx, which always carries the per-fact deadline) until
// no business claim is outstanding. It returns a derived context that is
// cancelled the moment a business writer claims, and the settle function the
// writer must call. ok is false when the deadline expired first — the caller
// then drops the fact.
func (p *txPriority) lease(ctx context.Context) (context.Context, func(), bool) {
	if p == nil {
		return ctx, func() {}, true
	}
	for {
		p.mu.Lock()
		if p.claims == 0 {
			leaseCtx, cancel := context.WithCancel(ctx)
			l := &observationLease{cancel: cancel, done: make(chan struct{})}
			p.active = l
			p.idle = make(chan struct{})
			idle := p.idle
			p.mu.Unlock()
			var once sync.Once
			settle := func() {
				once.Do(func() {
					cancel()
					p.mu.Lock()
					if p.active == l {
						p.active = nil
					}
					p.mu.Unlock()
					close(l.done)
					close(idle)
				})
			}
			return leaseCtx, settle, true
		}
		free := p.free
		p.mu.Unlock()
		select {
		case <-free:
			// A claim cleared; retry.
		case <-ctx.Done():
			return ctx, func() {}, false
		}
	}
}

// ---------------------------------------------------------------------------
// Schema (analytics migration v6)
// ---------------------------------------------------------------------------

// predictionObservationSchemaSQL is the body of analytics migration v6. It is
// purely ADDITIVE: two new tables and their indexes, no ALTER of any v1..v5
// table, no backfill of any historical row, and no FOREIGN KEY (this codebase
// never enables PRAGMA foreign_keys, so an FK would be decorative — parents
// are resolved before insert and removed explicitly by the erasure paths,
// exactly as v4 and v5 already document).
//
// Deliberate NON-constraints:
//   - event_id is NOT unique: one round produces many facts.
//   - source_fingerprint is NOT unique: a duplicate delivery is itself a fact
//     and must stay visible as a separate row.
//
// Deliberate constraints:
//   - UNIQUE(collector_epoch, collector_sequence) makes the causal order of a
//     session's facts total and gap-detectable.
//   - Every parent id requires its corresponding channel id, so no row can
//     exist that a channel-scoped privacy erasure cannot reach.
//   - round_incarnation_id exists exactly when a retention-group owner
//     channel does, which is what makes whole-round retention well defined.
var predictionObservationSchemaSQL = `
	CREATE TABLE IF NOT EXISTS prediction_observation_sessions (
		collector_epoch                   INTEGER PRIMARY KEY AUTOINCREMENT,
		collector_session_id              TEXT NOT NULL UNIQUE,
		producer_revision                 TEXT NOT NULL,
		started_at_ms                     INTEGER NOT NULL,
		closed_at_ms                      INTEGER,
		close_state                       TEXT NOT NULL
			CHECK (close_state IN ('OPEN', 'COMPLETE', 'INCOMPLETE')),
		last_assigned_sequence            INTEGER,
		committed_count                   INTEGER NOT NULL CHECK (committed_count >= 0),
		dropped_count                     INTEGER NOT NULL CHECK (dropped_count >= 0),
		unsettled_obligation_count        INTEGER NOT NULL CHECK (unsettled_obligation_count >= 0),
		post_fence_producer_count         INTEGER NOT NULL CHECK (post_fence_producer_count >= 0),
		producer_shutdown_uncertain_count INTEGER NOT NULL CHECK (producer_shutdown_uncertain_count >= 0),
		CHECK (
			(close_state =  'OPEN' AND closed_at_ms IS NULL) OR
			(close_state <> 'OPEN' AND closed_at_ms IS NOT NULL)
		)
	);

	CREATE TABLE IF NOT EXISTS prediction_observations (
		id                                INTEGER PRIMARY KEY AUTOINCREMENT,
		observation_id                    TEXT NOT NULL UNIQUE,
		collector_session_id              TEXT NOT NULL,
		collector_epoch                   INTEGER NOT NULL,
		collector_sequence                INTEGER NOT NULL,
		pool_instance_id                  TEXT NOT NULL,
		round_incarnation_id              TEXT,
		routed_streamer_id                INTEGER,
		routed_channel_id                 TEXT,
		round_owner_streamer_id           INTEGER,
		round_owner_channel_id            TEXT,
		retention_group_owner_streamer_id INTEGER,
		retention_group_owner_channel_id  TEXT,
		event_id                          TEXT,
		kind                              TEXT NOT NULL
			CHECK (kind IN (
				'source_unknown', 'channel_event', 'schedule_decision',
				'auto_decision', 'manual_control', 'placement',
				'user_prediction_made', 'user_terminal', 'round_cleanup'
			)),
		source_topic_type                 TEXT
			CHECK (source_topic_type IS NULL OR source_topic_type IN (
				'predictions-channel-v1', 'predictions-user-v1', 'UNKNOWN'
			)),
		source_message_type               TEXT
			CHECK (source_message_type IS NULL OR source_message_type IN (
				'event-created', 'event-updated', 'prediction-made',
				'prediction-result', 'UNKNOWN'
			)),
		source_fingerprint                TEXT,
		producer_at_ms                    INTEGER,
		producer_time_source              TEXT NOT NULL
			CHECK (producer_time_source IN ('PRODUCER', 'SERVER', 'RECEIVER', 'UNKNOWN')),
		received_at_ms                    INTEGER NOT NULL,
		connection_index                  INTEGER,
		connection_generation             INTEGER,
		connection_sequence               INTEGER,
		payload_version                   INTEGER NOT NULL,
		payload_json                      TEXT NOT NULL,
		observation_sha256                TEXT NOT NULL,
		UNIQUE (collector_epoch, collector_sequence),
		CHECK (routed_streamer_id                IS NULL OR routed_channel_id                IS NOT NULL),
		CHECK (round_owner_streamer_id           IS NULL OR round_owner_channel_id           IS NOT NULL),
		CHECK (retention_group_owner_streamer_id IS NULL OR retention_group_owner_channel_id IS NOT NULL),
		CHECK (
			(round_incarnation_id IS NULL     AND retention_group_owner_channel_id IS NULL) OR
			(round_incarnation_id IS NOT NULL AND retention_group_owner_channel_id IS NOT NULL)
		)
	);

	CREATE INDEX IF NOT EXISTS idx_predobs_session
		ON prediction_observations(collector_session_id, collector_sequence);
	CREATE INDEX IF NOT EXISTS idx_predobs_epoch
		ON prediction_observations(collector_epoch, id);
	CREATE INDEX IF NOT EXISTS idx_predobs_routed_identity
		ON prediction_observations(routed_channel_id, id);
	CREATE INDEX IF NOT EXISTS idx_predobs_retention_identity
		ON prediction_observations(retention_group_owner_channel_id, id);
	CREATE INDEX IF NOT EXISTS idx_predobs_routed_parent
		ON prediction_observations(routed_streamer_id, id);
	CREATE INDEX IF NOT EXISTS idx_predobs_retention_parent
		ON prediction_observations(retention_group_owner_streamer_id, id);
	CREATE INDEX IF NOT EXISTS idx_predobs_round
		ON prediction_observations(round_incarnation_id, id);
	CREATE INDEX IF NOT EXISTS idx_predobs_round_unit
		ON prediction_observations(collector_epoch, pool_instance_id, round_incarnation_id, received_at_ms);
	CREATE INDEX IF NOT EXISTS idx_predobs_null_round_retention
		ON prediction_observations(received_at_ms, id)
		WHERE round_incarnation_id IS NULL;
	CREATE INDEX IF NOT EXISTS idx_predobs_fingerprint
		ON prediction_observations(source_fingerprint);
`

// ---------------------------------------------------------------------------
// Repository: session control
// ---------------------------------------------------------------------------

// OpenObservationSession allocates a collector epoch by COMMITTING one INSERT
// and reading LastInsertId — never MAX(epoch)+1, which would reuse an epoch
// after a deletion and silently merge two collector runs. The row starts OPEN
// with zeroed counters.
func (r *SQLiteRepository) OpenObservationSession(ctx context.Context, sessionID string, startedAtMS int64) (int64, error) {
	var epoch int64
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO prediction_observation_sessions
				(collector_session_id, producer_revision, started_at_ms, closed_at_ms, close_state,
				 last_assigned_sequence, committed_count, dropped_count,
				 unsettled_obligation_count, post_fence_producer_count,
				 producer_shutdown_uncertain_count)
			VALUES (?, ?, ?, NULL, 'OPEN', NULL, 0, 0, 0, 0, 0)`,
			sessionID, ObservationProducerRevision, startedAtMS)
		if err != nil {
			return err
		}
		epoch, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return 0, err
	}
	return epoch, nil
}

// FinalizeObservationSession performs the session's ONE compare-and-set from
// OPEN to COMPLETE or INCOMPLETE, recording the final accounting. A session
// already finalized is left exactly as it is (applied=false): finalization
// happens once, and a second attempt must never rewrite history.
func (r *SQLiteRepository) FinalizeObservationSession(ctx context.Context, epoch int64, acct ObservationAccounting, closedAtMS int64) (applied bool, err error) {
	state := SessionComplete
	if !acct.Whole() {
		state = SessionIncomplete
	}
	var lastSeq interface{}
	if acct.LastAssignedSequence > 0 {
		lastSeq = acct.LastAssignedSequence
	}
	err = r.db.WithTx(ctx, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(ctx, `
			UPDATE prediction_observation_sessions
			   SET close_state                        = ?,
			       closed_at_ms                       = ?,
			       last_assigned_sequence             = ?,
			       committed_count                    = ?,
			       dropped_count                      = ?,
			       unsettled_obligation_count         = ?,
			       post_fence_producer_count          = ?,
			       producer_shutdown_uncertain_count  = ?
			 WHERE collector_epoch = ? AND close_state = 'OPEN'`,
			state, closedAtMS, lastSeq,
			acct.Committed, acct.Dropped, acct.UnsettledObligations,
			acct.PostFenceProducers, acct.ProducerShutdownUncertain,
			epoch)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		applied = n > 0
		return nil
	})
	return applied, err
}

// ObservationAccounting is the collector's in-memory tally, written once at
// finalization.
type ObservationAccounting struct {
	LastAssignedSequence      int64
	Committed                 int64
	Dropped                   int64
	UnsettledObligations      int64
	PostFenceProducers        int64
	ProducerShutdownUncertain int64
	// IdentityErasures is NOT persisted — it has no column, because it is not
	// a property of the facts. It records that a privacy erasure ran during
	// this session, which is enough to make the session INCOMPLETE: its facts
	// are deliberately no longer the whole set it observed.
	IdentityErasures int64
	// PreIntakeLosses is NOT persisted either. It records facts offered while
	// capture was not running, which took no causal position and so cannot be
	// counted as drops without contradicting the session's counter form. They
	// are still losses, and a session that had any is not COMPLETE.
	PreIntakeLosses int64
}

// Whole reports whether the session may be finalized COMPLETE: nothing was
// dropped, no obligation was left unsettled, no producer ran past the fence
// and no producer shutdown was uncertain.
func (a ObservationAccounting) Whole() bool {
	return a.Dropped == 0 &&
		a.UnsettledObligations == 0 &&
		a.PostFenceProducers == 0 &&
		a.ProducerShutdownUncertain == 0 &&
		a.IdentityErasures == 0 &&
		a.PreIntakeLosses == 0
}

// ---------------------------------------------------------------------------
// Repository: facts
// ---------------------------------------------------------------------------

// Typed failures of the fact path. They are errors rather than silent drops
// because each one means the store was asked for something it must refuse, and
// the collector has to be able to tell them apart from a busy connection.
var (
	// errObservationSessionNotOpen: the target session is not the currently
	// published OPEN one. A crash-left, abandoned or already-finalized session
	// must never accept another row: its counters are fixed and a late row
	// would make them a lie.
	errObservationSessionNotOpen = errors.New("analytics: observation session is not open")
	// errObservationCollision: an observation id already exists carrying
	// DIFFERENT capture-supplied content. Fail closed and never overwrite: the
	// first row is the fact, and a second one claiming the same causal
	// position with different content is an integrity failure, not an update.
	errObservationCollision = errors.New("analytics: observation id collides with different content")
	// errObservationGroupConflict: a round group's immutable retention-group
	// owner disagrees with this fact's.
	errObservationGroupConflict = errors.New("analytics: round group owner conflict")
)

// observationCaptureColumns is the part of a fact the PRODUCER supplied. It is
// what an identical retry has to match, and deliberately excludes the three
// repository-resolved parent ids: those are insert metadata, and a parent row
// created between the first attempt and the retry must not turn a retry into a
// collision.
type observationCaptureColumns struct {
	pool, incarnation, routedChannel, ownerChannel, retentionChannel string
	eventID, kind, topicType, messageType, fingerprint               string
	producerAtMS                                                     int64
	timeSource                                                       string
	receivedAtMS                                                     int64
	connIndex, connGeneration, connSequence                          sql.NullInt64
	payloadVersion                                                   int64
	payloadJSON                                                      string
}

// AppendObservation writes exactly ONE fact in ONE transaction. Facts are
// INSERT-only: there is no UPDATE, REPLACE or upsert anywhere in this file's
// fact path, which is what makes the trail immutable. The parent ids are
// resolved LOOKUP-ONLY through the shared lookupStreamerID — an observation
// never creates a streamers row, so it can neither resurrect a purged
// streamer nor invent one.
//
// The transaction does four things before it inserts, in this order:
//
//  1. It proves the target session is the published OPEN one. A finalized or
//     crash-left session's counters are fixed; a late row would contradict them.
//  2. It looks the observation id up. An identical retry is idempotent SUCCESS
//     — the fact is already recorded, and a second row would double-count it.
//     The same id with different capture-supplied content is a typed collision
//     and the existing row is never overwritten.
//  3. It resolves the parents lookup-only, freezing a round group's
//     retention-group owner from whatever the group's FIRST committed row
//     resolved, so companions of one round cannot disagree about who owns it
//     after a parent appears or a rename lands mid-round.
//  4. It computes the digest over the ids actually stored.
//
// ctx carries the per-fact deadline; every statement is context-aware, so a
// cancellation (a business writer claiming priority) interrupts the statement
// in flight and releases the shared connection instead of finishing a write
// nobody is waiting for.
func (r *SQLiteRepository) AppendObservation(ctx context.Context, o PredictionObservation, sessionID string, epoch, seq int64) error {
	payloadJSON, ok := marshalObservationPayload(o.Payload)
	if !ok {
		return fmt.Errorf("analytics: observation payload is not renderable")
	}
	incarnation := o.RoundIncarnationID
	// Composed from the exact causal coordinates the producer reserved, so it
	// is the same value on every attempt at this fact and different for every
	// other fact in the store.
	observationID := fmt.Sprintf("%d:%s:%d", epoch, sessionID, seq)

	want := observationCaptureColumns{
		pool: o.PoolInstanceID, incarnation: incarnation,
		routedChannel: o.RoutedChannelID, ownerChannel: o.RoundOwnerChannelID,
		retentionChannel: o.RetentionGroupOwnerChannelID,
		eventID:          o.EventID, kind: o.Kind,
		topicType: o.SourceTopicType, messageType: o.SourceMessageType,
		fingerprint:  o.SourceFingerprint,
		producerAtMS: o.ProducerAtMS, timeSource: o.ProducerTimeSource,
		receivedAtMS:   o.ReceivedAtMS,
		connIndex:      nullableConnection(o.ConnectionKnown, int64(o.ConnectionIndex)),
		connGeneration: nullableConnection(o.ConnectionKnown, int64(o.ConnectionGeneration)),
		connSequence:   nullableConnection(o.ConnectionKnown, int64(o.ConnectionSequence)),
		payloadVersion: ObservationPayloadVersion, payloadJSON: payloadJSON,
	}

	return r.db.WithTx(ctx, func(tx *sql.Tx) error {
		// (1) the exact published OPEN session, by BOTH halves of the pair.
		var closeState string
		switch e := tx.QueryRowContext(ctx, `
			SELECT close_state FROM prediction_observation_sessions
			 WHERE collector_epoch = ? AND collector_session_id = ?`, epoch, sessionID).
			Scan(&closeState); {
		case e == sql.ErrNoRows:
			return errObservationSessionNotOpen
		case e != nil:
			return e
		case closeState != SessionOpen:
			return errObservationSessionNotOpen
		}

		// (2) identical retry, before anything is written.
		existing, found, e := readObservationCaptureColumns(ctx, tx, observationID)
		if e != nil {
			return e
		}
		if found {
			if existing != want {
				return errObservationCollision
			}
			return nil
		}

		routedID, err := observationParentID(ctx, tx, o.RoutedLogin, o.RoutedChannelID)
		if err != nil {
			return err
		}
		ownerID, err := observationParentID(ctx, tx, o.RoundOwnerLogin, o.RoundOwnerChannelID)
		if err != nil {
			return err
		}

		// (3) the round group's owner is decided ONCE, by its first committed
		// row, and repeated verbatim by every companion. Resolving it per row
		// would let a parent created mid-round, or a rename, give two facts of
		// one round two different owners — and the owner is what whole-round
		// retention and whole-round erasure act on.
		var retentionID interface{}
		if incarnation != "" {
			frozenChannel, frozenParent, groupFound, e := observationGroupOwner(ctx, tx, epoch, o.PoolInstanceID, incarnation)
			if e != nil {
				return e
			}
			if groupFound {
				if frozenChannel != o.RetentionGroupOwnerChannelID {
					return errObservationGroupConflict
				}
				retentionID = frozenParent
			}
			if !groupFound {
				if retentionID, err = observationParentID(ctx, tx, o.RetentionGroupOwnerLogin, o.RetentionGroupOwnerChannelID); err != nil {
					return err
				}
			}
		} else if retentionID, err = observationParentID(ctx, tx, o.RetentionGroupOwnerLogin, o.RetentionGroupOwnerChannelID); err != nil {
			return err
		}

		// (4) Computed HERE, after the parents are resolved, so the witness
		// covers the ids actually stored rather than the ones the producer
		// guessed.
		digest := observationDigest(o, observationID, sessionID, epoch, seq, incarnation, payloadJSON,
			routedID, ownerID, retentionID)
		_, err = tx.ExecContext(ctx, `
			INSERT INTO prediction_observations
				(observation_id, collector_session_id, collector_epoch, collector_sequence,
				 pool_instance_id, round_incarnation_id,
				 routed_streamer_id, routed_channel_id,
				 round_owner_streamer_id, round_owner_channel_id,
				 retention_group_owner_streamer_id, retention_group_owner_channel_id,
				 event_id, kind, source_topic_type, source_message_type, source_fingerprint,
				 producer_at_ms, producer_time_source, received_at_ms,
				 connection_index, connection_generation, connection_sequence,
				 payload_version, payload_json, observation_sha256)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			observationID, sessionID, epoch, seq,
			o.PoolInstanceID, nullableText(incarnation),
			routedID, nullableText(o.RoutedChannelID),
			ownerID, nullableText(o.RoundOwnerChannelID),
			retentionID, nullableText(o.RetentionGroupOwnerChannelID),
			nullableText(o.EventID), o.Kind,
			nullableText(o.SourceTopicType), nullableText(o.SourceMessageType),
			nullableText(o.SourceFingerprint),
			nullableInt(o.ProducerAtMS), o.ProducerTimeSource, o.ReceivedAtMS,
			observationConnectionValue(o.ConnectionKnown, int64(o.ConnectionIndex)),
			observationConnectionValue(o.ConnectionKnown, int64(o.ConnectionGeneration)),
			observationConnectionValue(o.ConnectionKnown, int64(o.ConnectionSequence)),
			ObservationPayloadVersion, payloadJSON, digest,
		)
		return err
	})
}

// readObservationCaptureColumns reads back the capture-supplied half of an
// existing fact, so an identical retry can be recognized without consulting
// the repository-resolved parent ids.
func readObservationCaptureColumns(ctx context.Context, tx *sql.Tx, observationID string) (observationCaptureColumns, bool, error) {
	var got observationCaptureColumns
	var incarnation, routed, owner, retention, event, topic, message, fingerprint sql.NullString
	var producerAt sql.NullInt64
	e := tx.QueryRowContext(ctx, `
		SELECT pool_instance_id, round_incarnation_id, routed_channel_id, round_owner_channel_id,
		       retention_group_owner_channel_id, event_id, kind, source_topic_type,
		       source_message_type, source_fingerprint, producer_at_ms, producer_time_source,
		       received_at_ms, connection_index, connection_generation, connection_sequence,
		       payload_version, payload_json
		  FROM prediction_observations WHERE observation_id = ?`, observationID).
		Scan(&got.pool, &incarnation, &routed, &owner, &retention, &event, &got.kind, &topic,
			&message, &fingerprint, &producerAt, &got.timeSource,
			&got.receivedAtMS, &got.connIndex, &got.connGeneration, &got.connSequence,
			&got.payloadVersion, &got.payloadJSON)
	if e == sql.ErrNoRows {
		return got, false, nil
	}
	if e != nil {
		return got, false, e
	}
	got.incarnation, got.routedChannel, got.ownerChannel = incarnation.String, routed.String, owner.String
	got.retentionChannel, got.eventID = retention.String, event.String
	got.topicType, got.messageType, got.fingerprint = topic.String, message.String, fingerprint.String
	got.producerAtMS = producerAt.Int64
	return got, true, nil
}

// observationGroupOwner returns the retention-group owner already frozen by a
// round group's first committed row, if the group has one.
func observationGroupOwner(ctx context.Context, tx *sql.Tx, epoch int64, pool, incarnation string) (string, interface{}, bool, error) {
	var channel sql.NullString
	var parent sql.NullInt64
	e := tx.QueryRowContext(ctx, `
		SELECT retention_group_owner_channel_id, retention_group_owner_streamer_id
		  FROM prediction_observations
		 WHERE collector_epoch = ? AND pool_instance_id = ? AND round_incarnation_id = ?
		 ORDER BY collector_sequence ASC
		 LIMIT 1`, epoch, pool, incarnation).Scan(&channel, &parent)
	if e == sql.ErrNoRows {
		return "", nil, false, nil
	}
	if e != nil {
		return "", nil, false, e
	}
	if parent.Valid {
		return channel.String, parent.Int64, true, nil
	}
	return channel.String, nil, true, nil
}

func nullableConnection(known bool, v int64) sql.NullInt64 {
	return sql.NullInt64{Int64: v, Valid: known}
}

// observationParentID resolves a login to its analytics parent id WITHOUT
// creating one, and only when the identity also carries its channel id (the
// schema forbids a parent without its channel). An unknown login yields SQL
// NULL, which is a complete, valid identity for a fact about a streamer this
// database has no history row for.
func observationParentID(ctx context.Context, q querier, login, channelID string) (interface{}, error) {
	if login == "" || channelID == "" {
		return nil, nil
	}
	id, ok, err := lookupStreamerID(ctx, q, login)
	if err != nil || !ok {
		return nil, err
	}
	return id, nil
}

func nullableText(v string) interface{} {
	if v == "" {
		return nil
	}
	return v
}

func nullableInt(v int64) interface{} {
	if v == 0 {
		return nil
	}
	return v
}

func observationConnectionValue(known bool, v int64) interface{} {
	if !known {
		return nil
	}
	return v
}

// ---------------------------------------------------------------------------
// Repository: readers
// ---------------------------------------------------------------------------

const observationSelectColumns = `
	id, observation_id, collector_session_id, collector_epoch, collector_sequence,
	pool_instance_id, COALESCE(round_incarnation_id, ''),
	COALESCE(routed_streamer_id, 0), COALESCE(routed_channel_id, ''),
	COALESCE(round_owner_streamer_id, 0), COALESCE(round_owner_channel_id, ''),
	COALESCE(retention_group_owner_streamer_id, 0), COALESCE(retention_group_owner_channel_id, ''),
	COALESCE(event_id, ''), kind,
	COALESCE(source_topic_type, ''), COALESCE(source_message_type, ''),
	COALESCE(source_fingerprint, ''),
	COALESCE(producer_at_ms, 0), producer_time_source, received_at_ms,
	COALESCE(connection_index, -1), COALESCE(connection_generation, -1), COALESCE(connection_sequence, -1),
	payload_version, payload_json, observation_sha256`

func scanObservationRows(rows *sql.Rows) ([]ObservationRecord, error) {
	var out []ObservationRecord
	for rows.Next() {
		var rec ObservationRecord
		var payloadJSON string
		if err := rows.Scan(
			&rec.ID, &rec.ObservationID, &rec.CollectorSessionID, &rec.CollectorEpoch, &rec.CollectorSequence,
			&rec.PoolInstanceID, &rec.RoundIncarnationID,
			&rec.RoutedStreamerID, &rec.RoutedChannelID,
			&rec.RoundOwnerStreamerID, &rec.RoundOwnerChannelID,
			&rec.RetentionGroupOwnerStreamerID, &rec.RetentionGroupOwnerChannelID,
			&rec.EventID, &rec.Kind,
			&rec.SourceTopicType, &rec.SourceMessageType, &rec.SourceFingerprint,
			&rec.ProducerAtMS, &rec.ProducerTimeSource, &rec.ReceivedAtMS,
			&rec.ConnectionIndex, &rec.ConnectionGeneration, &rec.ConnectionSequence,
			&rec.PayloadVersion, &payloadJSON, &rec.ObservationSHA256,
		); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(payloadJSON), &rec.Payload); err != nil {
			// One undecodable payload must not discard every other fact in
			// the session: the trail's whole value is that surviving facts
			// stay readable. The row is returned with an empty payload and a
			// phase that says so, rather than silently looking ordinary.
			rec.Payload = ObservationPayload{Phase: ValueUnknown}
			rec.PayloadUndecodable = true
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ObservationsBySession reads one collector session's facts in causal order.
// It takes no repository mutex and holds no transaction past its return.
func (r *SQLiteRepository) ObservationsBySession(ctx context.Context, sessionID string, limit int) ([]ObservationRecord, error) {
	query := `SELECT ` + observationSelectColumns + `
		FROM prediction_observations
		WHERE collector_session_id = ?
		ORDER BY collector_sequence ASC`
	args := []interface{}{sessionID}
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanObservationRows(rows)
}

// ObservationsByRound reads one round's facts in causal order.
func (r *SQLiteRepository) ObservationsByRound(ctx context.Context, incarnationID string) ([]ObservationRecord, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+observationSelectColumns+`
		FROM prediction_observations
		WHERE round_incarnation_id = ?
		ORDER BY collector_epoch ASC, collector_sequence ASC`, incarnationID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanObservationRows(rows)
}

// ObservationsByFingerprint reads every fact sharing one source fingerprint,
// oldest first. This is what makes a re-delivery visible AS a re-delivery:
// source_fingerprint is deliberately not unique, so two deliveries of one
// source event are two facts, and this is how they are found together. It is
// the reader idx_predobs_fingerprint exists for.
func (r *SQLiteRepository) ObservationsByFingerprint(ctx context.Context, fingerprint string) ([]ObservationRecord, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+observationSelectColumns+`
		FROM prediction_observations
		WHERE source_fingerprint = ?
		ORDER BY collector_epoch ASC, collector_sequence ASC`, fingerprint)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	return scanObservationRows(rows)
}

// ReadObservationSession returns one session together with the classification
// a reader MUST apply to it. The session row and the fact count are read in
// ONE transaction, so the count can never belong to a different committed
// state than the row it qualifies.
func (r *SQLiteRepository) ReadObservationSession(ctx context.Context, epoch int64) (ObservationSessionReading, bool, error) {
	var out ObservationSessionReading
	var found bool
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		var s ObservationSessionRecord
		var closedAt, lastSeq sql.NullInt64
		e := tx.QueryRowContext(ctx, `
			SELECT collector_epoch, collector_session_id, producer_revision, started_at_ms,
			       closed_at_ms, close_state, last_assigned_sequence, committed_count,
			       dropped_count, unsettled_obligation_count, post_fence_producer_count,
			       producer_shutdown_uncertain_count
			  FROM prediction_observation_sessions WHERE collector_epoch = ?`, epoch).
			Scan(&s.CollectorEpoch, &s.CollectorSessionID, &s.ProducerRevision, &s.StartedAtMS,
				&closedAt, &s.CloseState, &lastSeq, &s.CommittedCount,
				&s.DroppedCount, &s.UnsettledObligationCount, &s.PostFenceProducerCount,
				&s.ProducerShutdownUncertainCount)
		if e == sql.ErrNoRows {
			return nil
		}
		if e != nil {
			return e
		}
		s.ClosedAtMS, s.ClosedAtKnown = closedAt.Int64, closedAt.Valid
		s.LastAssignedSequence, s.LastAssignedSequenceKnown = lastSeq.Int64, lastSeq.Valid
		found = true

		// Counted on the EXACT (epoch, session id) pair. Counting by epoch
		// alone would let a row that carries this epoch with a DIFFERENT
		// session id — the shape a reused or corrupted epoch produces — be
		// counted as one of this session's facts and make a broken store read
		// as a coherent one.
		var facts observationSessionFacts
		var minSeq, maxSeq, distinct sql.NullInt64
		if e := tx.QueryRowContext(ctx, `
			SELECT COUNT(*), MIN(collector_sequence), MAX(collector_sequence),
			       COUNT(DISTINCT collector_sequence)
			  FROM prediction_observations
			 WHERE collector_epoch = ? AND collector_session_id = ?`, epoch, s.CollectorSessionID).
			Scan(&facts.Present, &minSeq, &maxSeq, &distinct); e != nil {
			return e
		}
		facts.MinSequence, facts.MaxSequence, facts.DistinctSequences = minSeq.Int64, maxSeq.Int64, distinct.Int64
		// A fact matching exactly ONE half of the pair is an orphan: it claims
		// a session that does not own it, or an epoch that does not. Either
		// way the dataset is not internally consistent and no reading of this
		// session can be trusted.
		if e := tx.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM prediction_observations
			 WHERE (collector_epoch =  ? AND collector_session_id <> ?)
			    OR (collector_epoch <> ? AND collector_session_id =  ?)`,
			epoch, s.CollectorSessionID, epoch, s.CollectorSessionID).Scan(&facts.HalfPair); e != nil {
			return e
		}
		out = classifyObservationSession(s, facts)
		return nil
	})
	return out, found, err
}

// observationSessionFacts is what a reader measures about a session's
// surviving facts, on the EXACT (epoch, session id) pair.
type observationSessionFacts struct {
	Present           int64
	MinSequence       int64
	MaxSequence       int64
	DistinctSequences int64
	// HalfPair counts facts matching exactly one half of the pair.
	HalfPair int64
}

// classifyObservationSession is the reader contract: it decides which of the
// four readings a session supports, so no caller can accidentally treat an
// unfinalized or truncated session as authoritative about what did NOT happen.
//
// The counter form it enforces is the one the session accounting is defined by:
// last_assigned_sequence is NULL exactly when nothing was ever reserved, and
// otherwise every reserved position was either committed or dropped, so
// committed + dropped == last_assigned_sequence. A session that fails this is
// not merely incomplete — its own numbers disagree, and nothing derived from it
// means what it says.
func classifyObservationSession(s ObservationSessionRecord, facts observationSessionFacts) ObservationSessionReading {
	out := ObservationSessionReading{Session: s, FactsPresent: facts.Present}
	integrity := func(detail string) ObservationSessionReading {
		out.Reading = ReadingIntegrityError
		out.Detail = detail
		return out
	}
	switch {
	case s.CloseState == SessionOpen:
		out.Reading = ReadingUnfinalized
		out.Detail = "collector session is still OPEN: it is either live or was left behind by an unclean shutdown"
		return out
	case s.CloseState != SessionComplete && s.CloseState != SessionIncomplete:
		return integrity("close_state is outside the closed set")
	case !s.ClosedAtKnown:
		return integrity("finalized session carries no close time")
	case s.CommittedCount < 0 || s.DroppedCount < 0:
		return integrity("negative session counter")
	case facts.HalfPair > 0:
		return integrity("facts exist that match only one half of this session's (epoch, session id) pair")
	}

	// The counter form.
	reserved := s.CommittedCount + s.DroppedCount
	switch {
	case !s.LastAssignedSequenceKnown:
		if reserved != 0 {
			return integrity("session reserved no sequence yet counts committed or dropped facts")
		}
	case s.LastAssignedSequence < 1:
		return integrity("last assigned sequence is below the first position")
	case reserved != s.LastAssignedSequence:
		return integrity("committed plus dropped does not account for every reserved position")
	}

	// Surviving positions must lie inside what was actually reserved.
	if facts.Present > 0 {
		if facts.MinSequence < 1 || (s.LastAssignedSequenceKnown && facts.MaxSequence > s.LastAssignedSequence) {
			return integrity("a surviving fact sits outside the reserved sequence range")
		}
		if facts.DistinctSequences != facts.Present {
			return integrity("two surviving facts share one causal position")
		}
	}

	switch {
	case s.CloseState == SessionComplete &&
		(s.DroppedCount > 0 || s.UnsettledObligationCount > 0 ||
			s.PostFenceProducerCount > 0 || s.ProducerShutdownUncertainCount > 0):
		return integrity("session is COMPLETE yet reports a loss, an unsettled obligation, a post-fence producer or an uncertain producer shutdown")
	case facts.Present > s.CommittedCount:
		return integrity("more facts are present than the session committed")
	case facts.Present < s.CommittedCount:
		out.Reading = ReadingAdministrativelyTruncated
		out.Detail = "facts were removed after finalization by retention or a privacy erasure"
		return out
	}

	// Equal counts. A COMPLETE session lost nothing, so its surviving facts
	// must be exactly the reserved positions {1..last} with no gap; an
	// INCOMPLETE one may legitimately have gaps where facts were dropped.
	if s.CloseState == SessionComplete && s.LastAssignedSequenceKnown {
		if facts.Present != s.LastAssignedSequence || facts.MinSequence != 1 || facts.MaxSequence != s.LastAssignedSequence {
			return integrity("COMPLETE session does not hold exactly the positions it reserved")
		}
	}

	out.Reading = ReadingAsFinalized
	if s.CloseState == SessionIncomplete {
		out.Detail = "every committed fact is present, but the session itself did not observe everything it was offered"
	}
	// A session written under a DIFFERENT producer contract is not an
	// integrity failure — its rows are exactly what that contract wrote.
	// Classifying it as one would make every session unreadable the moment
	// the revision is bumped, destroying the trail's whole value across an
	// upgrade. The reading stands; the caller is told which contract's
	// invariants apply.
	if s.ProducerRevision != ObservationProducerRevision {
		out.Detail = "session was produced under a different observation contract: read its facts under that contract's invariants"
	}
	return out
}

// ObservationStoreStats is the bounded shape of the whole store, used by the
// collector to enforce the store caps without reading any fact content.
type ObservationStoreStats struct {
	Rows     int64
	Bytes    int64
	Sessions int64
}

// ObservationStoreStats measures the store against its compile-time caps.
func (r *SQLiteRepository) ObservationStoreStats(ctx context.Context) (ObservationStoreStats, error) {
	var st ObservationStoreStats
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		if e := tx.QueryRowContext(ctx,
			`SELECT COUNT(*), COALESCE(SUM(LENGTH(payload_json)), 0) FROM prediction_observations`).
			Scan(&st.Rows, &st.Bytes); e != nil {
			return e
		}
		return tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM prediction_observation_sessions`).Scan(&st.Sessions)
	})
	return st, err
}

// ---------------------------------------------------------------------------
// Repository: retention
// ---------------------------------------------------------------------------

// PruneObservationUnit removes exactly ONE bounded retention unit in ONE
// transaction and reports how many rows it removed:
//
//  1. one whole eligible local round — every fact of one
//     (collector_epoch, pool_instance_id, round_incarnation_id) unit whose
//     newest fact is older than the cutoff — or
//  2. at most observationPruneUnit NULL-round facts older than the cutoff, or
//  3. at most observationPruneUnit finalized sessions that have no facts left.
//
// The active epoch is never touched, and a crash-left OPEN session is never
// pruned automatically — neither its session row NOR its facts. An
// unfinalized session is evidence of an unclean shutdown, and that evidence is
// only meaningful together with the facts it did commit; pruning those while
// keeping the row would leave a session claiming committed facts that are
// gone, which reads as an integrity error rather than as the crash it was. This is deliberately
// NOT part of the generic PruneBefore sweep — P1 retention is owned by the
// collector's own worker so it can never lengthen a business retention
// transaction.
func (r *SQLiteRepository) PruneObservationUnit(ctx context.Context, cutoffMS int64, activeEpoch int64) (int64, error) {
	var removed int64
	err := r.db.WithTx(ctx, func(tx *sql.Tx) error {
		// (1) one whole eligible LOCAL round.
		//
		// The retention unit is the compound local round
		// (collector_epoch, pool_instance_id, round_incarnation_id), and this
		// query GROUPS BY exactly the key the DELETE below acts on. That
		// equality is the whole safety argument: a row this SELECT's WHERE
		// filtered out necessarily belongs to a DIFFERENT group, so no
		// filtered-out row can be swept up by the DELETE.
		//
		// Grouping on the incarnation alone did not have that property. A
		// round that spans a collector restart — an ordinary prediction that
		// outlives a miner restart — has facts in two epochs under one
		// incarnation. Excluding OPEN-session rows in the WHERE clause hid the
		// live ones from the GROUP BY, so MAX(received_at_ms) saw only the old
		// rows and the active-epoch guard was trivially satisfied; the DELETE
		// then removed the live facts the guard existed to protect.
		var (
			unitEpoch       int64
			unitPool        string
			unitIncarnation string
		)
		// collector_epoch <> activeEpoch is redundant with the OPEN filter
		// while the active session's row is OPEN, and is kept because it stops
		// being redundant during finalization.
		e := tx.QueryRowContext(ctx, `
			SELECT collector_epoch, pool_instance_id, round_incarnation_id
			  FROM prediction_observations
			 WHERE round_incarnation_id IS NOT NULL
			   AND collector_epoch <> ?
			   AND collector_epoch NOT IN (
			       SELECT collector_epoch FROM prediction_observation_sessions
			        WHERE close_state = 'OPEN')
			 GROUP BY collector_epoch, pool_instance_id, round_incarnation_id
			HAVING MAX(received_at_ms) < ?
			 LIMIT 1`, activeEpoch, cutoffMS).Scan(&unitEpoch, &unitPool, &unitIncarnation)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		if e == nil {
			res, err := tx.ExecContext(ctx, `
				DELETE FROM prediction_observations
				 WHERE collector_epoch = ?
				   AND pool_instance_id = ?
				   AND round_incarnation_id = ?`, unitEpoch, unitPool, unitIncarnation)
			if err != nil {
				return err
			}
			removed, err = res.RowsAffected()
			return err
		}

		// (2) a bounded batch of NULL-round facts.
		res, err := tx.ExecContext(ctx, `
			DELETE FROM prediction_observations
			 WHERE id IN (
			     SELECT id FROM prediction_observations
			      WHERE round_incarnation_id IS NULL
			        AND collector_epoch <> ?
			        AND received_at_ms < ?
			        AND collector_epoch NOT IN (
			            SELECT collector_epoch FROM prediction_observation_sessions
			             WHERE close_state = 'OPEN')
			      ORDER BY received_at_ms, id
			      LIMIT ?)`, activeEpoch, cutoffMS, observationPruneUnit)
		if err != nil {
			return err
		}
		if removed, err = res.RowsAffected(); err != nil {
			return err
		}
		if removed > 0 {
			return nil
		}

		// (3) a bounded batch of factless FINALIZED sessions. An OPEN session
		// is never swept: it is the only durable evidence of an unclean
		// collector shutdown.
		res, err = tx.ExecContext(ctx, `
			DELETE FROM prediction_observation_sessions
			 WHERE collector_epoch IN (
			     SELECT s.collector_epoch
			       FROM prediction_observation_sessions s
			      WHERE s.close_state <> 'OPEN'
			        AND s.collector_epoch <> ?
			        AND s.closed_at_ms < ?
			        AND NOT EXISTS (
			            SELECT 1 FROM prediction_observations o
			             WHERE o.collector_epoch = s.collector_epoch)
			      ORDER BY s.collector_epoch
			      LIMIT ?)`, activeEpoch, cutoffMS, observationPruneUnit)
		if err != nil {
			return err
		}
		removed, err = res.RowsAffected()
		return err
	})
	return removed, err
}

// ---------------------------------------------------------------------------
// Repository: identity erasure
// ---------------------------------------------------------------------------

// ObservationIdentity is the LEDGER-PROVEN identity a privacy erasure acts on.
// Both halves are carried end to end from the deletion ledger: the stable
// channel id (the primary identity) and the login (the lookup key every
// analytics table is keyed by). Neither is ever guessed — an empty or
// conflicting channel falls back only to what the ledger and the analytics
// parent row together prove.
type ObservationIdentity struct {
	ChannelID string
	Login     string
}

// EraseObservationsForIdentityTx removes one identity's observations INSIDE
// the caller's existing lifecycle transaction, so a streamer purge stays a
// single atomic operation across every store.
//
// Selection is deliberately asymmetric, because the three identity roles mean
// different things:
//
//   - A retention-group-owner match erases the WHOLE round: the round is
//     retained as a unit under that identity, so erasing the identity must
//     erase the unit.
//   - A routed-only match erases ONLY the matching fact. The fact is about a
//     different round; only its routing mentioned the erased identity.
//   - round_owner NEVER expands deletion. It is provenance, not ownership of
//     the retention unit, and letting it expand would delete another
//     streamer's round on the strength of a cross-reference.
//
// It acquires no repository mutex, waits on no worker and touches no gate:
// running any of those inside a caller-owned *sql.Tx would invert the lock
// order the business paths rely on.
func (r *SQLiteRepository) EraseObservationsForIdentityTx(tx *sql.Tx, id ObservationIdentity) (int64, error) {
	return r.eraseObservationsForIdentityTx(context.Background(), tx, id)
}

// EraseObservationsForIdentityCtxTx is EraseObservationsForIdentityTx with the
// caller's context, so the lookup it performs is cancelled with the
// transaction that owns it rather than running under a detached background
// context.
func (r *SQLiteRepository) EraseObservationsForIdentityCtxTx(ctx context.Context, tx *sql.Tx, id ObservationIdentity) (int64, error) {
	return r.eraseObservationsForIdentityTx(ctx, tx, id)
}

func (r *SQLiteRepository) eraseObservationsForIdentityTx(ctx context.Context, tx *sql.Tx, id ObservationIdentity) (int64, error) {
	channel := comparableIdentity(id.ChannelID)
	login := comparableLogin(id.Login)
	if channel == "" && login == "" {
		return 0, nil
	}

	// Resolve the proved parent id, when the login has one. This is the only
	// fallback for a fact recorded with a parent but an empty channel: proved,
	// never guessed.
	var parentID interface{}
	if login != "" {
		if pid, ok, err := lookupStreamerID(ctx, tx, login); err != nil {
			return 0, err
		} else if ok {
			parentID = pid
		}
	}
	if channel == "" && parentID == nil {
		return 0, nil
	}

	// Each selector is issued as its OWN statement against its own index.
	// A single combined statement guarded by `(? IS NOT NULL AND col = ?) OR
	// ...` would be correct but unindexable — SQLite cannot use an index
	// behind a parameter guard, so it degrades to a full scan per selector
	// and blows the bounded-purge budget on a large identity.
	var removed int64
	exec := func(what, query string, args ...interface{}) error {
		res, err := tx.Exec(query, args...)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		removed += n
		return nil
	}

	// (1) whole rounds owned by the identity, by channel and by proved parent.
	const roundsByChannel = `
		DELETE FROM prediction_observations
		 WHERE round_incarnation_id IN (
		     SELECT DISTINCT round_incarnation_id FROM prediction_observations
		      WHERE retention_group_owner_channel_id = ?)`
	const roundsByParent = `
		DELETE FROM prediction_observations
		 WHERE round_incarnation_id IN (
		     SELECT DISTINCT round_incarnation_id FROM prediction_observations
		      WHERE retention_group_owner_streamer_id = ?)`
	if channel != "" {
		if err := exec("erase observation rounds by channel", roundsByChannel, channel); err != nil {
			return 0, err
		}
	}
	if parentID != nil {
		if err := exec("erase observation rounds by parent", roundsByParent, parentID); err != nil {
			return 0, err
		}
	}

	// (2) remaining facts that merely mention the identity in a routed role.
	if channel != "" {
		if err := exec("erase routed observations by channel",
			`DELETE FROM prediction_observations WHERE routed_channel_id = ?`, channel); err != nil {
			return 0, err
		}
	}
	if parentID != nil {
		if err := exec("erase routed observations by parent",
			`DELETE FROM prediction_observations WHERE routed_streamer_id = ?`, parentID); err != nil {
			return 0, err
		}
	}
	return removed, nil
}

// DeleteStreamerIdentityTx is DeleteStreamerTx plus the channel-scoped erasure
// of the SAME identity's immutable Prediction observations, in the caller's
// transaction — so a streamer purge remains ONE atomic operation across every
// analytics table, observations included.
//
// The observations are erased FIRST, while the streamers row still exists:
// the erasure's proved-parent fallback resolves the login through that row, so
// deleting it first would silently narrow the erasure to the channel selector
// alone. Returns true when the identity held persisted analytics state of
// either kind.
func (r *SQLiteRepository) DeleteStreamerIdentityTx(tx *sql.Tx, channelID, login string) (bool, error) {
	erased, err := r.EraseObservationsForIdentityTx(tx, ObservationIdentity{ChannelID: channelID, Login: login})
	if err != nil {
		return false, err
	}
	existed, err := r.DeleteStreamerTx(tx, login)
	if err != nil {
		return false, err
	}
	return existed || erased > 0, nil
}

// InvalidateIdentity satisfies the lifecycle coordinator's optional
// identity-fencer contract. It runs OUTSIDE — and strictly before — the purge
// transaction, and does TWO things, because either alone is insufficient:
//
//  1. It bumps the capture generation, so every fact ALREADY QUEUED (for any
//     identity) is dropped rather than committed. The bump is deliberately
//     global: a queued fact is not yet resolved against the store, so the only
//     way to guarantee it cannot resurrect the identity being erased is to
//     invalidate everything in flight. Dropping too much is the safe direction
//     for a privacy erasure.
//  2. It arms the identity fence, so facts produced AFTER the purge are
//     refused too. Without this the generation bump is a one-shot: a producer
//     still live for the erased channel would stamp its next fact with the NEW
//     generation and re-persist exactly what was erased.
//
// The fence is lifted by Reinstate, mirroring the repository's own tombstone.
func (r *SQLiteRepository) InvalidateIdentity(channelID, login string) {
	if r.observations != nil {
		r.observations.invalidateGeneration()
		r.observations.fence(channelID, login)
	}
}

// ---------------------------------------------------------------------------
// Collector
// ---------------------------------------------------------------------------

// observationCollector owns the private queue, the single writer goroutine,
// the capture generation and the session accounting. It is created by
// NewService (which constructs and migrates only) and started by
// Service.Start, which never blocks on its bootstrap.
type observationCollector struct {
	repo *SQLiteRepository
	gate *txPriority
	now  func() time.Time

	// retentionDays mirrors Analytics.RetentionDays. P1 adds no setting and
	// no scheduler of its own.
	retentionDays int

	// maintenanceInterval paces the worker-owned retention pass. Always
	// observationMaintenanceInterval in production; a test shortens it.
	maintenanceInterval time.Duration
	// overCapacity records that capture was paused by a hard store bound, so
	// a later pass that frees space can resume it.
	overCapacity atomic.Bool
	// closing is set by Close BEFORE it fences intake, so a bootstrap still
	// finishing cannot publish RUNNING over the top of a teardown.
	closing atomic.Bool
	// inFlight counts facts the writer has taken off the queue but not yet
	// settled, so a drain can tell "queue empty" from "actually finished".
	inFlight atomic.Int64
	// preIntakeLosses counts facts offered while capture was not running —
	// before bootstrap published intake, or after it was disabled. They took
	// no causal position, so they cannot be counted as drops without breaking
	// the session's counter form, but they are still losses and the session
	// must not finalize COMPLETE while any exist. Not persisted: it is a
	// reason for INCOMPLETE, not a column of its own.
	preIntakeLosses atomic.Int64

	// identityErasures counts privacy erasures performed during this session.
	// One is enough to make the session INCOMPLETE: after an erasure its facts
	// are deliberately no longer the whole set it observed. It is deliberately
	// SEPARATE from unsettled_obligation_count, which means "offered and never
	// written" — conflating the two made every ordinary streamer removal
	// report an obligation the collector had failed to settle.
	identityErasures atomic.Int64

	// writeDeadline is the hard per-fact budget. It is ALWAYS
	// ObservationWriteDeadline in production (newObservationCollector is the
	// only constructor and there is no setting for it); a test raises it only
	// to make a commit-count assertion independent of the host's fsync
	// latency. TestObservationWriteDeadlineIsFiveMilliseconds pins the
	// production value.
	writeDeadline time.Duration

	// queue is the collector's private capacity-512 channel. Producers send
	// nonblocking; a full queue drops.
	queue chan PredictionObservation

	// running gates capture. It flips true only after the bounded bootstrap
	// (retention, recount, session allocation) has completed, and false again
	// at the shutdown fence.
	running atomic.Bool
	// disabled marks a collector whose bootstrap failed or whose identity was
	// erased. Capture stays off for the rest of the process.
	disabled atomic.Bool
	// generation is bumped by a privacy erasure. A queued fact stamped with
	// an older generation is dropped rather than committed, so an erased
	// identity can never be resurrected by work ALREADY IN FLIGHT.
	generation atomic.Uint64

	// fenceMu/fenced is the identity fence, and it is what a generation bump
	// alone cannot provide. The bump only invalidates work already queued; a
	// producer that is STILL LIVE after the purge would otherwise stamp a
	// fresh fact with the NEW generation and re-persist the erased channel.
	// This mirrors the repository's own tombstone fence: an erased identity
	// stays refused until it is explicitly reinstated.
	//
	// It is deliberately NOT the repository's mutex. The collector may hold a
	// priority lease while checking the fence, and a business writer claims
	// the gate BEFORE taking repo.mu — so a collector that blocked on repo.mu
	// while holding a lease would deadlock against a claimer waiting for that
	// same lease to settle. This lock is only ever held for a map lookup and
	// never across a database call or a gate operation.
	fenceMu sync.RWMutex
	// fenced holds fence keys ("login:x" / "chan:y"); loginChannels remembers
	// which channels were fenced alongside a login so Reinstate can lift both.
	fenced        map[string]struct{}
	loginChannels map[string][]string

	// Session accounting. All are plain atomics touched by producers and the
	// writer; none is read under a lock held by a business path.
	sequence                  atomic.Int64
	committed                 atomic.Int64
	dropped                   atomic.Int64
	unsettledObligations      atomic.Int64
	postFenceProducers        atomic.Int64
	producerShutdownUncertain atomic.Int64

	sessionID string
	epoch     atomic.Int64

	// sessionBytes accumulates the payload bytes this session has committed,
	// so the per-session byte bound is enforced from an atomic rather than a
	// COUNT query on the write path. Together with committed (the row count)
	// it makes MaxSessionRows/MaxSessionBytes real ceilings instead of
	// documentation.
	sessionBytes atomic.Int64

	startOnce sync.Once
	closeOnce sync.Once
	// stop is written by Start and read by Close, which are different
	// sync.Once instances — those establish no happens-before between each
	// other, so this is an atomic rather than a plain field.
	stop   atomic.Pointer[context.CancelFunc]
	joined chan struct{}
	// bootstrapped is closed once Start's bootstrap has settled either way,
	// so tests can await a deterministic state without polling.
	bootstrapped chan struct{}
}

func newObservationCollector(repo *SQLiteRepository, gate *txPriority, retentionDays int, now func() time.Time) *observationCollector {
	return &observationCollector{
		repo:                repo,
		gate:                gate,
		now:                 now,
		retentionDays:       retentionDays,
		writeDeadline:       ObservationWriteDeadline,
		maintenanceInterval: observationMaintenanceInterval,
		queue:               make(chan PredictionObservation, ObservationQueueCapacity),
		joined:              make(chan struct{}),
		bootstrapped:        make(chan struct{}),
		fenced:              make(map[string]struct{}),
		loginChannels:       make(map[string][]string),
	}
}

// observationFenceKeys are the identity keys one fact is checked against. A
// fact is refused if ANY identity it names is fenced — the routed identity or
// the retention-group owner. round_owner is deliberately absent: it is
// provenance, and since it never widens deletion it must not widen refusal
// either.
func observationFenceKeys(o PredictionObservation) []string {
	keys := make([]string, 0, 4)
	if o.RoutedLogin != "" {
		keys = append(keys, "login:"+o.RoutedLogin)
	}
	if o.RoutedChannelID != "" {
		keys = append(keys, "chan:"+o.RoutedChannelID)
	}
	if o.RetentionGroupOwnerLogin != "" {
		keys = append(keys, "login:"+o.RetentionGroupOwnerLogin)
	}
	if o.RetentionGroupOwnerChannelID != "" {
		keys = append(keys, "chan:"+o.RetentionGroupOwnerChannelID)
	}
	return keys
}

// fence arms the identity fence for an erased (channelID, login). Both halves
// are fenced, and the pairing is remembered so lifting the login also lifts
// the channel that was erased with it.
func (c *observationCollector) fence(channelID, login string) {
	login = comparableLogin(login)
	channelID = comparableIdentity(channelID)
	if login == "" && channelID == "" {
		return
	}
	c.fenceMu.Lock()
	defer c.fenceMu.Unlock()
	if login != "" {
		c.fenced["login:"+login] = struct{}{}
	}
	if channelID != "" {
		c.fenced["chan:"+channelID] = struct{}{}
		if login != "" {
			c.loginChannels[login] = append(c.loginChannels[login], channelID)
		}
	}
}

// unfence lifts the fence for a login and every channel erased alongside it,
// so a re-added streamer records fresh observations again. It mirrors the
// repository's Reinstate exactly.
func (c *observationCollector) unfence(login string) {
	login = comparableLogin(login)
	if login == "" {
		return
	}
	c.fenceMu.Lock()
	defer c.fenceMu.Unlock()
	delete(c.fenced, "login:"+login)
	for _, ch := range c.loginChannels[login] {
		delete(c.fenced, "chan:"+ch)
	}
	delete(c.loginChannels, login)
}

// isFenced reports whether any identity this fact names has been erased.
func (c *observationCollector) isFenced(o PredictionObservation) bool {
	keys := observationFenceKeys(o)
	if len(keys) == 0 {
		return false
	}
	c.fenceMu.RLock()
	defer c.fenceMu.RUnlock()
	for _, k := range keys {
		if _, ok := c.fenced[k]; ok {
			return true
		}
	}
	return false
}

// Start launches the collector. It is NONBLOCKING: it spawns one goroutine
// that performs the bounded bootstrap (a retention unit, a store recount and
// the session INSERT) and only then publishes RUNNING. A bootstrap failure
// disables P1 alone — it never fails App or Miner startup.
func (c *observationCollector) Start() {
	c.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		c.stop.Store(&cancel)
		go c.run(ctx)
	})
}

func (c *observationCollector) run(ctx context.Context) {
	defer close(c.joined)

	if err := c.bootstrap(ctx); err != nil {
		c.disabled.Store(true)
		close(c.bootstrapped)
		observationLog("Prediction observation capture disabled", err)
		// Keep draining so a producer that raced the bootstrap never blocks;
		// every drained fact is accounted as a drop.
		c.drainUntil(ctx)
		return
	}
	// Close may already have fenced intake while the bootstrap was still
	// finishing. Publishing RUNNING unconditionally here would reopen a
	// collector that is being torn down, leaving a writer-less RUNNING state
	// and a session that finalizes COMPLETE despite having lost work.
	if !c.closing.Load() {
		c.running.Store(true)
	}
	close(c.bootstrapped)

	// Retention is WORKER-OWNED, so the worker has to own it continuously —
	// a single unit at bootstrap would let a long-running miner keep every
	// observation it ever wrote. The ticker keeps pruning off the write path
	// while still bounding each transaction to one unit.
	maintenance := time.NewTicker(c.maintenanceInterval)
	defer maintenance.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case obs := <-c.queue:
			c.inFlight.Add(1)
			c.write(ctx, obs)
			c.inFlight.Add(-1)
		case <-maintenance.C:
			c.maintain(ctx)
		}
	}
}

// maintain is one bounded retention pass: it removes up to
// observationPruneUnitsPerPass units (each its own transaction) and then
// re-measures the store against its hard caps. It runs on the collector's own
// goroutine, behind the same priority lease as a fact write, so it can never
// make a business writer wait for more than one bounded transaction.
func (c *observationCollector) maintain(ctx context.Context) {
	if c.disabled.Load() {
		return
	}
	if c.retentionDays > 0 {
		cutoff := c.now().Add(-time.Duration(c.retentionDays) * 24 * time.Hour).UnixMilli()
		for i := 0; i < observationPruneUnitsPerPass; i++ {
			n, err := c.prune(ctx, cutoff)
			if err != nil {
				observationLog("Prediction observation retention pass failed", err)
				return
			}
			if n == 0 {
				break
			}
		}
	}

	// The hard store caps are enforced HERE, against a fresh measurement —
	// not once at startup against a stale one. A store that has grown past a
	// bound with nothing left to prune stops accepting new facts rather than
	// growing without limit; capture resumes on the next pass if retention
	// later frees space.
	stats, err := c.storeStats(ctx)
	if err != nil {
		return
	}
	over := stats.Rows >= MaxStoreRows || stats.Bytes >= MaxStoreBytes || stats.Sessions >= MaxStoreSessions
	if over && c.running.Load() {
		c.running.Store(false)
		c.overCapacity.Store(true)
		observationLog("Prediction observation capture paused: store is at a hard capacity bound", errObservationAtCapacity)
		return
	}
	if !over && c.overCapacity.Load() && !c.closing.Load() {
		c.overCapacity.Store(false)
		c.running.Store(true)
	}
}

// bootstrap performs the bounded start-up work: one retention unit (so a
// long-idle store makes progress without a scheduler of its own), a store cap
// recount, and the session allocation that yields this run's epoch.
func (c *observationCollector) bootstrap(ctx context.Context) error {
	sessionID, err := newCollectorSessionID()
	if err != nil {
		return err
	}
	c.sessionID = sessionID

	// Prune BEFORE measuring. Measuring first and pruning second compares the
	// caps against a figure the prune is about to invalidate, which can leave
	// capture permanently disabled on a store the very same pass just drained.
	if c.retentionDays > 0 {
		cutoff := c.now().Add(-time.Duration(c.retentionDays) * 24 * time.Hour).UnixMilli()
		for i := 0; i < observationPruneUnitsPerPass; i++ {
			n, perr := c.prune(ctx, cutoff)
			if perr != nil {
				return fmt.Errorf("analytics: observation bootstrap retention: %w", perr)
			}
			if n == 0 {
				break
			}
		}
	}
	stats, err := c.storeStats(ctx)
	if err != nil {
		return fmt.Errorf("analytics: observation store recount: %w", err)
	}
	if stats.Sessions >= MaxStoreSessions || stats.Rows >= MaxStoreRows || stats.Bytes >= MaxStoreBytes {
		// Still at a hard cap after a full pruning pass: refuse to add more
		// rather than growing past the bound.
		return fmt.Errorf("%w (rows=%d bytes=%d sessions=%d)",
			errObservationAtCapacity, stats.Rows, stats.Bytes, stats.Sessions)
	}

	epoch, err := c.leasedSession(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("analytics: open observation session: %w", err)
	}
	c.epoch.Store(epoch)
	return nil
}

// drainUntil empties the queue without writing, so a disabled collector never
// leaves a producer blocked and every offered fact is honestly counted.
func (c *observationCollector) drainUntil(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.queue:
			c.dropped.Add(1)
		}
	}
}

// write persists ONE fact under the per-fact deadline. Every failure — a
// stale generation, a refused lease, a cancelled statement, a constraint
// violation — is a DROP. There is no retry: retrying would let an observer
// compete with a business writer for the shared connection.
func (c *observationCollector) write(ctx context.Context, raw PredictionObservation) {
	if c.disabled.Load() || raw.generation != c.generation.Load() {
		c.dropped.Add(1)
		return
	}
	// Sanitize HERE, on the collector goroutine — never on the producer's.
	// A fact that cannot be projected onto the closed contract is dropped.
	obs, ok := sanitizeObservation(raw, c.now().UnixMilli())
	if !ok {
		c.dropped.Add(1)
		return
	}
	// The identity fence. A fact naming an erased identity is refused
	// outright — a live producer must not be able to re-persist what an
	// operator has just erased.
	if c.isFenced(obs) {
		c.dropped.Add(1)
		return
	}
	// The per-session bounds are ceilings, not advice: a session that has
	// reached either one stops committing rather than growing past it. The
	// facts it already wrote stay exact; the session finalizes INCOMPLETE
	// because the drops below say so.
	if !c.withinSessionBounds() {
		c.dropped.Add(1)
		return
	}
	// The causal position was reserved by offer, before this fact was queued.
	// Every return above leaves that number unused, which is what makes a loss
	// visible as a gap.
	seq := raw.sequence

	writeCtx, cancel := context.WithTimeout(ctx, c.writeDeadline)
	defer cancel()

	leaseCtx, settle, ok := c.gate.lease(writeCtx)
	if !ok {
		c.dropped.Add(1)
		return
	}
	defer settle()

	// Re-check AFTER the lease. Acquiring it can wait for a business writer to
	// release the connection, and an erasure can land in exactly that window;
	// without this re-check a fact admitted before the erasure would commit
	// after it, which is the difference between an erasure that holds and one
	// that only looks like it does.
	if c.disabled.Load() || raw.generation != c.generation.Load() || c.isFenced(obs) {
		c.dropped.Add(1)
		return
	}

	if err := c.repo.AppendObservation(leaseCtx, obs, c.sessionID, c.epoch.Load(), seq); err != nil {
		c.dropped.Add(1)
		return
	}
	c.sessionBytes.Add(int64(len(payloadJSONOf(obs))))
	c.committed.Add(1)
}

// withinSessionBounds reports whether this session may commit another fact.
// Both bounds are read from atomics the writer already maintains, so the
// check costs nothing on the write path — which is why they can be enforced
// per fact rather than merely documented.
func (c *observationCollector) withinSessionBounds() bool {
	return c.committed.Load() < MaxSessionRows && c.sessionBytes.Load() < MaxSessionBytes
}

// payloadJSONOf renders a fact's payload for accounting. It is the same
// rendering AppendObservation persists, so the byte tally tracks what is
// actually stored.
func payloadJSONOf(o PredictionObservation) string {
	rendered, ok := marshalObservationPayload(o.Payload)
	if !ok {
		return ""
	}
	return rendered
}

// leased runs one of the collector's OWN transactions behind the priority
// gate and a deadline, exactly like a fact write. Without this the bootstrap,
// the retention pass and the session control writes would be full-table work
// on the single shared connection that a business writer could not preempt —
// which is precisely the interference the gate exists to prevent.
func (c *observationCollector) leased(ctx context.Context, budget time.Duration, fn func(context.Context) error) error {
	leaseCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	granted, settle, ok := c.gate.lease(leaseCtx)
	if !ok {
		return context.DeadlineExceeded
	}
	defer settle()
	return fn(granted)
}

// observationMaintenanceBudget bounds one leased maintenance/bootstrap
// transaction. It is larger than the per-fact deadline because these
// statements are aggregate work, but it is still a hard ceiling and still
// preemptible: a business claim cancels it immediately.
const observationMaintenanceBudget = 2 * time.Second

func (c *observationCollector) prune(ctx context.Context, cutoffMS int64) (int64, error) {
	var removed int64
	err := c.leased(ctx, observationMaintenanceBudget, func(lease context.Context) error {
		var e error
		removed, e = c.repo.PruneObservationUnit(lease, cutoffMS, c.epoch.Load())
		return e
	})
	return removed, err
}

func (c *observationCollector) storeStats(ctx context.Context) (ObservationStoreStats, error) {
	var st ObservationStoreStats
	err := c.leased(ctx, observationMaintenanceBudget, func(lease context.Context) error {
		var e error
		st, e = c.repo.ObservationStoreStats(lease)
		return e
	})
	return st, err
}

func (c *observationCollector) leasedSession(ctx context.Context, sessionID string) (int64, error) {
	var epoch int64
	err := c.leased(ctx, observationMaintenanceBudget, func(lease context.Context) error {
		var e error
		epoch, e = c.repo.OpenObservationSession(lease, sessionID, c.now().UnixMilli())
		return e
	})
	return epoch, err
}

// offer is the producer-side entry point: a single nonblocking send. It never
// allocates on the shared connection, never waits and never returns an error
// a producer could be tempted to act on.
func (c *observationCollector) offer(obs PredictionObservation) {
	if !c.running.Load() {
		// Not yet bootstrapped, already fenced, or disabled: the fact is
		// honestly lost, and the session says so at finalization.
		//
		// It is NOT counted as a drop. dropped_count accounts for the
		// RESERVATION stream — positions this session handed out and did not
		// commit — and a fact refused before intake opened never took a
		// position. Counting it there would make committed + dropped exceed
		// last_assigned_sequence, which is precisely the shape a reader is
		// required to treat as an integrity failure. The session is still
		// prevented from finalizing COMPLETE, by the counters that exist for
		// exactly this: post_fence_producer_count after the shutdown fence,
		// and the pre-intake loss below before it.
		if c.closing.Load() {
			// A producer STILL offering after the shutdown fence proves the
			// collector closed while a producer was demonstrably alive.
			c.postFenceProducers.Add(1)
		} else {
			c.preIntakeLosses.Add(1)
		}
		return
	}
	obs.generation = c.generation.Load()
	// The causal position is RESERVED HERE, on the producer's call, before the
	// fact is queued — not later by the writer.
	//
	// Assigning it at the writer made the persisted sequence a record of what
	// survived rather than of what happened: a fact lost to a full queue, a
	// stale generation, an erasure fence or a session ceiling consumed no
	// number, so the stored sequence was always dense and a reader could not
	// tell "nothing happened here" from "we dropped it". Reserving first makes
	// every loss leave a GAP, which is exactly the evidence the session's
	// dropped count explains. It also fixes the position at the moment of
	// capture, so the order the store holds is the order the producers reached,
	// not the order the writer got to them.
	//
	// The reservation costs one atomic add on a path that already does two.
	obs.sequence = c.sequence.Add(1)
	select {
	case c.queue <- obs:
	default:
		c.dropped.Add(1)
	}
}

// invalidateGeneration is called BEFORE a privacy-erasure transaction opens.
// It bumps the capture generation (so every fact already queued is dropped
// rather than committed) and forces the session INCOMPLETE, permanently. The
// erasure's completeness matters more than the trail's.
// invalidateGeneration bumps the capture generation so queued work is dropped.
// It does NOT count an unsettled obligation: an erasure discarding queued facts
// is a DELIBERATE, correct outcome, and the facts it discards are already
// counted as drops — which is what makes the session INCOMPLETE. Counting it
// here as well made every ordinary streamer removal report an obligation the
// collector had failed to settle, which is a false explanation of why the
// session is incomplete.
func (c *observationCollector) invalidateGeneration() {
	c.generation.Add(1)
	c.identityErasures.Add(1)
}

// noteProducerShutdownUncertain records that a producer could not prove it had
// stopped offering facts — the pool's Close returned an error, so a late
// producer may still exist. It forces INCOMPLETE.
func (c *observationCollector) noteProducerShutdownUncertain() {
	c.producerShutdownUncertain.Add(1)
}

// accounting snapshots the tally for finalization.
func (c *observationCollector) accounting() ObservationAccounting {
	return ObservationAccounting{
		LastAssignedSequence:      c.sequence.Load(),
		Committed:                 c.committed.Load(),
		Dropped:                   c.dropped.Load(),
		UnsettledObligations:      c.unsettledObligations.Load(),
		PostFenceProducers:        c.postFenceProducers.Load(),
		ProducerShutdownUncertain: c.producerShutdownUncertain.Load(),
		IdentityErasures:          c.identityErasures.Load(),
		PreIntakeLosses:           c.preIntakeLosses.Load(),
	}
}

// observationDrainGrace bounds how long Close waits for the queue to drain
// before cancelling the writer.
const observationDrainGrace = 5 * time.Second

// drain waits, up to grace, for the writer to empty the queue. Whatever is
// still queued when the grace expires is counted as an unsettled obligation,
// which forces the session INCOMPLETE — an honest "these facts were offered
// and never written" rather than a silent gap.
func (c *observationCollector) drain(grace time.Duration) {
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		// The channel length drops the moment the writer RECEIVES a fact —
		// before it has been sanitized, leased and committed. Waiting on the
		// length alone would declare the queue drained with the last fact
		// still in flight and then cancel it. inFlight closes that window.
		if len(c.queue) == 0 && c.inFlight.Load() == 0 {
			return
		}
		select {
		case <-deadline.C:
			c.unsettledObligations.Add(int64(len(c.queue)) + c.inFlight.Load())
			return
		case <-tick.C:
		}
	}
}

// Close fences intake, gives the queue a bounded grace to drain, cancels the
// writer, JOINS it, and only then finalizes the session. No DB-capable
// collector goroutine survives this call, which is what lets App close the
// shared database immediately afterwards.
func (c *observationCollector) Close() {
	c.closeOnce.Do(func() {
		// 0. Announce the teardown FIRST. A bootstrap still in flight checks
		//    this before publishing RUNNING, so it cannot reopen intake over
		//    the top of the fence below.
		c.closing.Store(true)
		// 1. Fence intake. Every later offer is a counted drop.
		c.running.Store(false)

		// 2. Bounded drain.
		if stop := c.stop.Load(); stop != nil {
			c.drain(observationDrainGrace)
			// 3. Cancel and 4. JOIN the writer.
			(*stop)()
			<-c.joined
		}

		// 5. Finalize — after the join, so the accounting can no longer move.
		if c.epoch.Load() == 0 {
			return // bootstrap never allocated a session: nothing to finalize
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		err := c.leased(ctx, observationMaintenanceBudget, func(lease context.Context) error {
			_, e := c.repo.FinalizeObservationSession(lease, c.epoch.Load(), c.accounting(), c.now().UnixMilli())
			return e
		})
		if err != nil {
			observationLog("Failed to finalize the prediction observation session", err)
		}
	})
}

// observationLog reports a P1 problem without ever emitting a raw error into
// a field that could carry provider text. The message is fixed; the error is
// classified, not interpolated.
func observationLog(msg string, err error) {
	if err == nil {
		return
	}
	class := "INTERNAL"
	switch {
	case errors.Is(err, database.ErrClosed):
		class = "DATABASE_CLOSED"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		class = "YIELDED"
	case errors.Is(err, errObservationDisabled):
		class = "DISABLED"
	}
	slog.Debug(msg, "class", class)
}
