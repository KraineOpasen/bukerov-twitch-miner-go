package pubsub

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// resultMsg builds a predictions-user "prediction-result" message for eventID.
func resultMsg(eventID, resultType string, pointsWon float64) *PubSubMessage {
	result := map[string]interface{}{"type": resultType}
	if resultType != "REFUND" {
		result["points_won"] = pointsWon
	}
	return &PubSubMessage{
		Type: "prediction-result",
		Data: map[string]interface{}{
			"prediction": map[string]interface{}{
				"event_id": eventID,
				"result":   result,
			},
		},
	}
}

// confirmedRound sets up a round whose bet is confirmed with a known stake on
// outcome o1, odds computed, ready to resolve.
func confirmedRound(t *testing.T, pool *WebSocketPool, s *models.Streamer, eventID string, stake int) *models.EventPrediction {
	t.Helper()
	ep := addRound(pool, s, eventID)
	// o1 has 300 of 500 total points → odds 500/300 ≈ 1.67 (bucket 1.5–2).
	ep.Bet.UpdateOutcomes([]interface{}{
		map[string]interface{}{"id": "o1", "total_points": float64(300), "total_users": float64(3)},
		map[string]interface{}{"id": "o2", "total_points": float64(200), "total_users": float64(2)},
	})
	ep.Bet.Decision = models.Decision{Choice: 0, Amount: stake, ID: "o1"}
	ep.BetPlaced = true
	ep.BetConfirmed = true
	return ep
}

func TestBetResultHandlerFiresOnWin(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newTestStreamer(100000)
	confirmedRound(t, pool, s, "win-evt", 500)

	var got BetResult
	var fired int
	pool.SetBetResultHandler(func(r BetResult) { got = r; fired++ })

	pool.handlePredictionUser(resultMsg("win-evt", "WIN", 1000), s)

	if fired != 1 {
		t.Fatalf("handler fired %d times, want 1", fired)
	}
	if got.EventID != "win-evt" || got.Streamer != s.Username {
		t.Errorf("identity wrong: %+v", got)
	}
	if got.ResultType != "WIN" || got.Placed != 500 || got.Won != 1000 || got.Gained != 500 {
		t.Errorf("amounts wrong: %+v", got)
	}
	if got.Strategy != string(s.Settings.Bet.Strategy) {
		t.Errorf("strategy = %q, want %q", got.Strategy, s.Settings.Bet.Strategy)
	}
	if got.Odds < 1.6 || got.Odds > 1.7 {
		t.Errorf("odds = %v, want ~1.67", got.Odds)
	}
	if got.Manual {
		t.Error("auto-bet must not be marked manual")
	}
}

// TestBetResultHandlerRefundKeepsRawStake is the key regression: ParseResult
// zeroes `placed` for a REFUND, but the emitted record must still carry the raw
// stake that was put up (read before ParseResult), with Gained 0.
func TestBetResultHandlerRefundKeepsRawStake(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newTestStreamer(100000)
	confirmedRound(t, pool, s, "refund-evt", 750)

	var got BetResult
	pool.SetBetResultHandler(func(r BetResult) { got = r })

	pool.handlePredictionUser(resultMsg("refund-evt", "REFUND", 0), s)

	if got.ResultType != "REFUND" {
		t.Fatalf("result type = %q, want REFUND", got.ResultType)
	}
	if got.Placed != 750 {
		t.Errorf("refund must keep raw stake 750, got %d", got.Placed)
	}
	if got.Won != 0 || got.Gained != 0 {
		t.Errorf("refund is net-zero, got won=%d gained=%d", got.Won, got.Gained)
	}
}

func TestBetResultHandlerMarksManualStrategy(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newTestStreamer(100000)
	confirmedRound(t, pool, s, "manual-evt", 500)
	pool.controlFor("manual-evt").manualBet = true

	var got BetResult
	pool.SetBetResultHandler(func(r BetResult) { got = r })

	pool.handlePredictionUser(resultMsg("manual-evt", "WIN", 1000), s)

	if !got.Manual {
		t.Error("manual bet must be marked manual")
	}
	if got.Strategy != "MANUAL" {
		t.Errorf("manual bet strategy = %q, want MANUAL", got.Strategy)
	}
}

// TestBetResultHandlerSkippedWhenUnconfirmed ensures a result for a bet that was
// never confirmed does not produce a record.
func TestBetResultHandlerSkippedWhenUnconfirmed(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newTestStreamer(100000)
	ep := confirmedRound(t, pool, s, "unconf-evt", 500)
	ep.BetConfirmed = false

	fired := 0
	pool.SetBetResultHandler(func(BetResult) { fired++ })
	pool.handlePredictionUser(resultMsg("unconf-evt", "WIN", 1000), s)

	if fired != 0 {
		t.Fatalf("no record must be emitted for an unconfirmed bet, fired %d", fired)
	}
}
