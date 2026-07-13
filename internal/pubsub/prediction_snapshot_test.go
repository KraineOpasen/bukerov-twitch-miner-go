package pubsub

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestPredictionsSnapshotIncludesOutcomes verifies the widened snapshot
// surfaces per-outcome detail (title/points/chosen), the prediction window and
// the bet amount - the data the dashboard's live-predictions board renders.
func TestPredictionsSnapshotIncludesOutcomes(t *testing.T) {
	streamer := models.NewStreamer("shroud", models.DefaultStreamerSettings())
	streamer.ChannelID = "123"

	outcomes := []interface{}{
		map[string]interface{}{"id": "o1", "title": "Yes", "color": "blue", "total_points": float64(300), "total_users": float64(3)},
		map[string]interface{}{"id": "o2", "title": "No", "color": "pink", "total_points": float64(200), "total_users": float64(2)},
	}
	ep := models.NewEventPrediction(streamer, "evt1", "Will they win?", time.Now(), 60, "ACTIVE", outcomes)
	ep.BetPlaced = true
	ep.Bet.Decision.Choice = 0
	ep.Bet.Decision.Amount = 500
	ep.Bet.TotalPoints = 500

	pool := &WebSocketPool{predictions: map[string]*models.EventPrediction{"evt1": ep}}

	snaps := pool.PredictionsSnapshot()
	if len(snaps) != 1 {
		t.Fatalf("expected 1 snapshot, got %d", len(snaps))
	}
	s := snaps[0]
	if s.Streamer != "shroud" || s.Title != "Will they win?" {
		t.Errorf("basic fields wrong: %+v", s)
	}
	if s.PredictionWindowSeconds != 60 {
		t.Errorf("window seconds = %v, want 60", s.PredictionWindowSeconds)
	}
	if s.BetAmount != 500 || s.TotalPoints != 500 {
		t.Errorf("bet/pool wrong: amount=%d pool=%d", s.BetAmount, s.TotalPoints)
	}
	if len(s.Outcomes) != 2 {
		t.Fatalf("expected 2 outcomes, got %d", len(s.Outcomes))
	}
	if s.Outcomes[0].Title != "Yes" || !s.Outcomes[0].Chosen {
		t.Errorf("outcome 0 should be the chosen 'Yes': %+v", s.Outcomes[0])
	}
	if s.Outcomes[1].Chosen {
		t.Error("outcome 1 should not be chosen")
	}
	if s.Outcomes[0].TotalPoints != 300 {
		t.Errorf("outcome 0 total points = %d, want 300", s.Outcomes[0].TotalPoints)
	}
}
