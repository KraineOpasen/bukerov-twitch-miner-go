package models

import (
	"fmt"
	"math"
	"time"
)

type PredictionStatus string

const (
	PredictionActive   PredictionStatus = "ACTIVE"
	PredictionLocked   PredictionStatus = "LOCKED"
	PredictionResolved PredictionStatus = "RESOLVED"
	PredictionCanceled PredictionStatus = "CANCELED"
)

type PredictionResultType string

const (
	ResultWin    PredictionResultType = "WIN"
	ResultLose   PredictionResultType = "LOSE"
	ResultRefund PredictionResultType = "REFUND"
)

type PredictionResult struct {
	Type   PredictionResultType
	String string
	Gained int
}

type EventPrediction struct {
	Streamer                *Streamer
	EventID                 string
	Title                   string
	CreatedAt               time.Time
	PredictionWindowSeconds float64
	Status                  PredictionStatus
	Result                  PredictionResult
	BetConfirmed            bool
	BetPlaced               bool
	// ResultAccepted marks that the FIRST valid terminal prediction-result
	// (WIN/LOSE/REFUND) for this round has been admitted. Guarded by the
	// owning pool's mutex like BetPlaced/BetConfirmed. The atomic
	// check-and-mark on it gives the tracked in-process round at-most-once
	// terminal admission for its process lifetime: only the admitted
	// delivery may invoke terminal side effects, and a later duplicate or
	// conflicting result must never re-invoke them or overwrite Result.
	// This is not a durable or transactional guarantee — an individual sink
	// can still fail after admission, and nothing survives a restart.
	ResultAccepted bool
	Bet            *Bet
}

func NewEventPrediction(
	streamer *Streamer,
	eventID, title string,
	createdAt time.Time,
	predictionWindowSeconds float64,
	status string,
	outcomes []interface{},
) *EventPrediction {
	return &EventPrediction{
		Streamer:                streamer,
		EventID:                 eventID,
		Title:                   title,
		CreatedAt:               createdAt,
		PredictionWindowSeconds: predictionWindowSeconds,
		Status:                  PredictionStatus(status),
		Bet:                     NewBet(outcomes, streamer.GetSettings().Bet),
	}
}

func (e *EventPrediction) Elapsed(timestamp time.Time) float64 {
	return timestamp.Sub(e.CreatedAt).Seconds()
}

func (e *EventPrediction) ClosingBetAfter(timestamp time.Time) float64 {
	return e.PredictionWindowSeconds - e.Elapsed(timestamp)
}

// maxExactTerminalPayout bounds the accepted points_won magnitude: a decoded
// JSON number arrives as float64, which represents every integer exactly only
// in [-2^53, 2^53] — beyond that the value cannot be trusted to be the
// producer's integer. The platform int range is intersected in so the bound
// also guarantees a safe int conversion on every GOARCH (on the project's
// 64-bit targets the 2^53 term is the tighter one).
var maxExactTerminalPayout = math.Min(float64(1<<53), float64(math.MaxInt))

// validTerminalPayout reports whether a present points_won value is a finite,
// mathematically integral, non-negative number inside the exactly
// representable and int-convertible range. Negative payouts are rejected: no
// terminal result type pays out negative points, and a negative value would
// silently corrupt gain accounting while consuming the round's admission.
func validTerminalPayout(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) &&
		math.Trunc(v) == v &&
		v >= 0 && v <= maxExactTerminalPayout
}

// ValidateTerminalResult is the pure defensive validation boundary for a
// predictions-user terminal result payload, run BEFORE terminal admission may
// consume a round. It accepts exactly the supported terminal types and their
// coherent payout shapes:
//
//   - WIN: points_won is required and must satisfy validTerminalPayout;
//   - LOSE / REFUND: points_won may be absent or an explicit JSON null —
//     both carry no payout value and yield the canonical won = 0 (the
//     upstream wire is known to send the key present-but-null); when a
//     numeric value is present it must be zero — contradictory payout data
//     is rejected rather than silently coerced.
//
// Anything else — a missing/non-string type, an unsupported type, or a
// malformed payout — is rejected so it can never consume the admission or
// reach ParseResult's default-zero behavior as an authoritative result.
// These rules are this miner's fail-safe admission requirements, not a claim
// about the official Twitch wire contract.
func ValidateTerminalResult(result map[string]interface{}) bool {
	if result == nil {
		return false
	}
	rawType, ok := result["type"].(string)
	if !ok {
		return false
	}
	payout, present := result["points_won"]
	switch PredictionResultType(rawType) {
	case ResultWin:
		v, numeric := payout.(float64)
		if !present || !numeric || !validTerminalPayout(v) {
			return false
		}
	case ResultLose, ResultRefund:
		if present && payout != nil {
			v, numeric := payout.(float64)
			if !numeric || !validTerminalPayout(v) || v != 0 {
				return false
			}
		}
	default:
		return false
	}
	return true
}

func (e *EventPrediction) ParseResult(result map[string]interface{}) (placed, won, gained int) {
	resultType := ""
	if rt, ok := result["type"].(string); ok {
		resultType = rt
	}

	if resultType == "REFUND" {
		placed = 0
	} else {
		placed = e.Bet.Decision.Amount
	}

	if pointsWon, ok := result["points_won"].(float64); ok {
		won = int(pointsWon)
	}
	if resultType == "REFUND" {
		won = 0
	}

	if resultType != "REFUND" {
		gained = won - placed
	}

	prefix := ""
	if gained >= 0 {
		prefix = "+"
	}

	var action string
	switch resultType {
	case "LOSE":
		action = "Lost"
	case "REFUND":
		action = "Refunded"
	default:
		action = "Gained"
	}

	e.Result = PredictionResult{
		Type:   PredictionResultType(resultType),
		String: fmt.Sprintf("%s, %s: %s%d", resultType, action, prefix, gained),
		Gained: gained,
	}

	return placed, won, gained
}
