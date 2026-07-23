package models

import (
	"fmt"
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
	Bet                     *Bet
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
