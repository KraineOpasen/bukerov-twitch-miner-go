package pubsub

// P4 winner-field canary: a temporary, privacy-bounded diagnostic.
//
// It answers one question about frames this package already receives: does a
// predictions-channel-v1 event-updated frame whose round is RESOLVED carry a
// nonempty winning_outcome_id naming exactly one valid outcome id of the SAME
// frame? Each qualifying frame yields at most one fixed INFO record through the
// default slog logger. Nothing reads the record back: it is not P4 resolution
// input, not a WINNER_CAPTURE, and no business decision depends on it.
//
// It is stateless between frames — no dedup, cache, counter, lock, goroutine
// or toggle — so a repeated frame is inspected again. Beyond the topic,
// message type and round status, which it compares with constants, it reads
// only the event id, winning_outcome_id, outcomes and each outcome's id, by
// fixed key: no recursion, no key enumeration, no copying, hashing or
// comparison of any of those values before its length is admitted, and no
// formatting or String/LogValue call on anything from the frame. The record
// carries closed-vocabulary labels, counts, a domain-separated SHA-256 of the
// event id (diagnostic linkage, not anonymization), the build version label
// and the receiver's clock — never a raw id, a key name taken from the frame,
// a title, predictor, channel or account identity.
//
// Silence is not evidence. A frame the resource bounds refuse, INFO disabled,
// a failing or closed logger, or a console line the async writer drops all
// leave no record; so do frames that never reach this code — one from a channel
// the pool has no streamer for, or an identical frame the connection drops as a
// replay within one second. None of them says the field was absent.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
)

const (
	winnerCanaryMessage     = "p4_winner_canary"
	winnerCanarySchema      = "p4-winner-canary/v2"
	winnerCanaryMessageType = "event-updated"
	winnerCanaryStatus      = "RESOLVED"
	winnerCanaryWinnerKey   = "winning_outcome_id"

	// Resource admission: a frame beyond either bound is refused whole, before
	// any inspected value is copied, hashed or compared, and leaves no record.
	winnerCanaryMaxOutcomes = 64
	winnerCanaryMaxBytes    = 4096 // per inspected string and the build label

	// winnerCanaryHashDomain separates event_key_hash from every other
	// SHA-256 of the same event id; it follows the schema version.
	winnerCanaryHashDomain = winnerCanarySchema + "\x00event\x00"
	winnerCanaryNoHash     = "none"

	winnerCanaryObserved    = "WINNER_FIELD_OBSERVED"
	winnerCanaryNotObserved = "NOT_OBSERVED_IN_THIS_EVENT"
	winnerCanaryMalformed   = "MALFORMED_IN_THIS_EVENT"
)

// winnerCanaryRecord holds a record's attributes other than the fixed
// constants and the observation time: the build label and what the frame said.
// Every field is a scalar, so a record retains nothing of the frame.
type winnerCanaryRecord struct {
	buildVersion     string
	winnerPresence   string
	winnerJSONType   string
	winnerValueEmpty bool
	outcomesPresence string
	outcomeCount     int
	outcomeIDsValid  bool
	winnerMatchCount int
	winnerIndex      int
	eventKeyHash     string
	result           string
}

// logWinnerCanary emits the canary record for one predictions-channel frame,
// if the frame qualifies and fits the resource bounds. It is called with no
// lock held, and whatever the logger does with the record — write it, drop it
// or fail — changes no decision or state in how the frame is then handled; a
// synchronous sink (the log file, with Save=true) can only delay it.
func logWinnerCanary(msg *PubSubMessage, eventStatus string, eventData map[string]interface{}) {
	rec, ok := classifyWinnerCanary(msg, eventStatus, eventData, version.Version)
	if !ok {
		return
	}
	slog.LogAttrs(context.Background(), slog.LevelInfo, winnerCanaryMessage, rec.attrs(time.Now())...)
}

// classifyWinnerCanary decides whether a frame is a canary candidate and what
// its record says. ok is false both for a frame that is not a candidate and for
// one the resource bounds refuse.
func classifyWinnerCanary(msg *PubSubMessage, eventStatus string, eventData map[string]interface{}, buildVersion string) (winnerCanaryRecord, bool) {
	if msg == nil || msg.Topic.Type != TopicPredictionsChannel || msg.Type != winnerCanaryMessageType ||
		eventData == nil || eventStatus != winnerCanaryStatus {
		return winnerCanaryRecord{}, false
	}

	eventID, eventIDIsString := eventData["id"].(string)
	winnerRaw, winnerFound := eventData[winnerCanaryWinnerKey]
	winner, winnerIsString := winnerRaw.(string)
	outcomes, outcomesIsList := eventData["outcomes"].([]interface{})

	// Resource admission precedes all other work on the inspected values,
	// lengths first.
	if len(buildVersion) > winnerCanaryMaxBytes ||
		(eventIDIsString && len(eventID) > winnerCanaryMaxBytes) ||
		(winnerIsString && len(winner) > winnerCanaryMaxBytes) ||
		(outcomesIsList && len(outcomes) > winnerCanaryMaxOutcomes) {
		return winnerCanaryRecord{}, false
	}
	for _, outcome := range outcomes {
		if obj, outcomeIsObject := outcome.(map[string]interface{}); outcomeIsObject {
			if id, idIsString := obj["id"].(string); idIsString && len(id) > winnerCanaryMaxBytes {
				return winnerCanaryRecord{}, false
			}
		}
	}

	eventIDUsable := eventIDIsString && eventID != ""
	winnerUsable := winnerIsString && winner != ""
	rec := winnerCanaryRecord{
		buildVersion:     buildVersion,
		winnerPresence:   wirePresence(eventData, winnerCanaryWinnerKey, isString),
		winnerJSONType:   winnerCanaryJSONType(winnerRaw, winnerFound),
		winnerValueEmpty: winnerIsString && winner == "",
		outcomesPresence: wirePresence(eventData, "outcomes", isList),
		outcomeCount:     -1,
		winnerMatchCount: -1,
		winnerIndex:      -1,
		eventKeyHash:     winnerCanaryNoHash,
	}
	if outcomesIsList {
		rec.outcomeCount = len(outcomes)
		rec.outcomeIDsValid = winnerCanaryIDsValid(outcomes)
	}
	// A valid vector's ids are pairwise distinct, so at most one can match and
	// winner_index is set exactly when the match count is one.
	if winnerUsable && rec.outcomeIDsValid {
		rec.winnerMatchCount = 0
		for i, outcome := range outcomes {
			if id, _ := winnerCanaryOutcomeID(outcome); id == winner {
				rec.winnerMatchCount++
				rec.winnerIndex = i
			}
		}
	}
	if eventIDUsable {
		rec.eventKeyHash = winnerCanaryEventKeyHash(eventID)
	}

	switch {
	case !eventIDUsable:
		rec.result = winnerCanaryMalformed
	case !winnerFound || winnerRaw == nil:
		rec.result = winnerCanaryNotObserved
	case winnerUsable && rec.outcomeIDsValid && rec.winnerMatchCount == 1:
		rec.result = winnerCanaryObserved
	default:
		rec.result = winnerCanaryMalformed
	}
	return rec, true
}

// attrs renders the record as the contract's exact attribute list, in order.
func (r winnerCanaryRecord) attrs(observedAt time.Time) []slog.Attr {
	return []slog.Attr{
		slog.String("canary_schema", winnerCanarySchema),
		slog.String("build_version", r.buildVersion),
		slog.String("source_topic_type", string(TopicPredictionsChannel)),
		slog.String("source_message_type", winnerCanaryMessageType),
		slog.String("round_status", winnerCanaryStatus),
		slog.String("winner_field_name", winnerCanaryWinnerKey),
		slog.String("winner_field_presence", r.winnerPresence),
		slog.String("winner_field_json_type", r.winnerJSONType),
		slog.Bool("winner_value_empty", r.winnerValueEmpty),
		slog.String("outcomes_presence", r.outcomesPresence),
		slog.Int("outcome_count", r.outcomeCount),
		slog.Bool("outcome_ids_valid", r.outcomeIDsValid),
		slog.Int("winner_match_count", r.winnerMatchCount),
		slog.Int("winner_index", r.winnerIndex),
		slog.String("event_key_hash", r.eventKeyHash),
		slog.String("observed_at_utc", observedAt.UTC().Format(time.RFC3339)),
		slog.String("result", r.result),
	}
}

// winnerCanaryJSONType names the JSON type of a value as the package's decoder
// produces it. Anything else — a value built in Go rather than decoded — is
// "other", and is never formatted to find out what it is.
func winnerCanaryJSONType(raw interface{}, found bool) string {
	if !found {
		return "absent"
	}
	switch raw.(type) {
	case nil:
		return "null"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "bool"
	case map[string]interface{}:
		return "object"
	case []interface{}:
		return "array"
	default:
		return "other"
	}
}

// winnerCanaryOutcomeID reads one outcome's id: ok when the outcome is an
// object whose "id" is a nonempty string.
func winnerCanaryOutcomeID(outcome interface{}) (string, bool) {
	obj, outcomeIsObject := outcome.(map[string]interface{})
	if !outcomeIsObject {
		return "", false
	}
	id, idIsString := obj["id"].(string)
	return id, idIsString && id != ""
}

// winnerCanaryIDsValid reports whether the outcomes form a valid id vector: at
// least two objects, each with a nonempty id, pairwise distinct. Admission has
// already bounded the vector, so the pairwise check is at most 64×63/2
// comparisons of admitted strings, with no allocation.
func winnerCanaryIDsValid(outcomes []interface{}) bool {
	if len(outcomes) < 2 {
		return false
	}
	for i, outcome := range outcomes {
		id, ok := winnerCanaryOutcomeID(outcome)
		if !ok {
			return false
		}
		for _, earlier := range outcomes[:i] {
			if prev, _ := winnerCanaryOutcomeID(earlier); prev == id {
				return false
			}
		}
	}
	return true
}

// winnerCanaryEventKeyHash is "sha256:" + hex(SHA-256(domain + event id)).
func winnerCanaryEventKeyHash(eventID string) string {
	h := sha256.New()
	_, _ = h.Write([]byte(winnerCanaryHashDomain))
	_, _ = h.Write([]byte(eventID))
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}
