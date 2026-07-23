package pubsub

import (
	"errors"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/eligibility"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// eventCreatedMsg builds a predictions-channel event-created frame for an
// ACTIVE round with a comfortably open betting window.
func eventCreatedMsg(eventID string) *PubSubMessage {
	return &PubSubMessage{
		Type: "event-created",
		Data: map[string]interface{}{
			"event": map[string]interface{}{
				"id":                        eventID,
				"status":                    "ACTIVE",
				"title":                     "Will they win?",
				"created_at":                time.Now().Format(time.RFC3339),
				"prediction_window_seconds": float64(3600),
				"outcomes": []interface{}{
					map[string]interface{}{"id": "o1", "title": "Yes", "total_points": float64(300), "total_users": float64(3)},
					map[string]interface{}{"id": "o2", "title": "No", "total_points": float64(200), "total_users": float64(2)},
				},
			},
		},
	}
}

func eventUpdatedMsg(eventID, status string) *PubSubMessage {
	return &PubSubMessage{
		Type: "event-updated",
		Data: map[string]interface{}{
			"event": map[string]interface{}{
				"id":     eventID,
				"status": status,
			},
		},
	}
}

func trackedRound(pool *WebSocketPool, eventID string) bool {
	pool.mu.RLock()
	defer pool.mu.RUnlock()
	_, ok := pool.predictions[eventID]
	return ok
}

// TestPredictionDisableInFlightBlocksAutoBetPlacement is the core in-flight
// gate: a round is scheduled while MakePredictions is on, the toggle goes off
// before the bet timer fires, and the placement path must re-check the CURRENT
// setting and never call Twitch — with the stable user_disabled reason — while
// the round stays tracked for result/refund cleanup.
func TestPredictionDisableInFlightBlocksAutoBetPlacement(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	s := newTestStreamer(10000)
	ep := addRound(pool, s, "e1")
	ep.Bet.Settings = autoBetSettings(5)

	// The toggle flips AFTER scheduling, BEFORE placement.
	setStreamerFlag(s, func(st *models.StreamerSettings) { st.MakePredictions = false })

	pool.placeAutoBet("e1")

	if placer.callCount() != 0 {
		t.Fatalf("PlacePredictionBet was called %d times after MakePredictions=false, want 0", placer.callCount())
	}
	if ep.BetPlaced {
		t.Error("BetPlaced must stay false when the placement gate blocks")
	}
	if d := pointsEligibility.EvaluatePointsTask(s, eligibility.TaskPrediction); d.Eligible || d.Reason != eligibility.ReasonUserDisabled {
		t.Errorf("placement gate decision = eligible=%v reason=%q, want blocked with stable reason %q",
			d.Eligible, d.Reason, eligibility.ReasonUserDisabled)
	}
	if !trackedRound(pool, "e1") {
		t.Error("round must stay tracked so result/refund correlation and cleanup still work")
	}
}

// TestPredictionReenableAffectsOnlyNewEvents: re-enabling after a skipped
// placement never retro-places the old bet; it only lets NEW rounds schedule.
func TestPredictionReenableAffectsOnlyNewEvents(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	s := newTestStreamer(10000)
	ep := addRound(pool, s, "e1")
	ep.Bet.Settings = autoBetSettings(5)

	setStreamerFlag(s, func(st *models.StreamerSettings) { st.MakePredictions = false })
	pool.placeAutoBet("e1")
	setStreamerFlag(s, func(st *models.StreamerSettings) { st.MakePredictions = true })

	if placer.callCount() != 0 {
		t.Fatalf("re-enable retro-placed the skipped bet: %d calls", placer.callCount())
	}

	pool.handlePredictionChannel(eventCreatedMsg("e2"), s)
	if !trackedRound(pool, "e2") {
		t.Fatal("new round must be schedulable again after re-enable")
	}
}

// TestPredictionDisableKeepsPlacedBetAndResultFlow: a bet placed BEFORE the
// toggle went off is never cancelled or erased, and its settled result still
// flows to ROI analytics after the disable.
func TestPredictionDisableKeepsPlacedBetAndResultFlow(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	s := newTestStreamer(10000)
	ep := addRound(pool, s, "e1")
	ep.Bet.Settings = autoBetSettings(5)

	pool.placeAutoBet("e1")
	if placer.callCount() != 1 || !ep.BetPlaced {
		t.Fatalf("setup: bet not placed (calls=%d placed=%v)", placer.callCount(), ep.BetPlaced)
	}
	placedAmount := ep.Bet.Decision.Amount

	setStreamerFlag(s, func(st *models.StreamerSettings) { st.MakePredictions = false })

	if !ep.BetPlaced || ep.Bet.Decision.Amount != placedAmount {
		t.Fatal("disabling MakePredictions must not erase the already-placed bet")
	}

	var got BetResult
	fired := 0
	pool.SetBetResultHandler(func(r BetResult) { got = r; fired++ })
	pool.mu.Lock()
	ep.BetConfirmed = true
	pool.mu.Unlock()
	pool.handlePredictionUser(resultMsg("e1", "WIN", 900), s)

	if fired != 1 {
		t.Fatalf("settled result after disable fired %d times, want 1 (correlation intact)", fired)
	}
	if got.Placed != placedAmount {
		t.Errorf("BetResult.Placed = %d, want %d", got.Placed, placedAmount)
	}
}

// TestPredictionDisableKeepsRoundBookkeeping: event-updated for an
// already-tracked round is pure local bookkeeping and keeps working after the
// toggle goes off (no Twitch mutation is involved).
func TestPredictionDisableKeepsRoundBookkeeping(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newTestStreamer(10000)
	ep := addRound(pool, s, "e1")

	setStreamerFlag(s, func(st *models.StreamerSettings) { st.MakePredictions = false })
	pool.handlePredictionChannel(eventUpdatedMsg("e1", "LOCKED"), s)

	pool.mu.RLock()
	status := ep.Status
	pool.mu.RUnlock()
	if status != models.PredictionStatus("LOCKED") {
		t.Fatalf("tracked round status = %q after disable, want LOCKED (bookkeeping must continue)", status)
	}
}

// TestPredictionManualBetCurrentGatedByToggle: the manual path re-checks the
// CURRENT setting too, with a distinct stable user-facing error.
func TestPredictionManualBetCurrentGatedByToggle(t *testing.T) {
	placer := &fakePlacer{}
	pool := newTestPool(placer)
	s := newTestStreamer(10000)
	addRound(pool, s, "e1")

	setStreamerFlag(s, func(st *models.StreamerSettings) { st.MakePredictions = false })

	if _, err := pool.PlaceManualBet("e1", "o1", 500); !errors.Is(err, ErrPredictionsUserDisabled) {
		t.Fatalf("manual bet error = %v, want %v", err, ErrPredictionsUserDisabled)
	}
	if placer.callCount() != 0 {
		t.Fatalf("manual bet reached Twitch despite MakePredictions=false: %d calls", placer.callCount())
	}
}
