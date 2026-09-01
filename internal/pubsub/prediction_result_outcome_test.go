package pubsub

import (
	"errors"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// The handler's returned verdict is the single admission truth: true exactly
// for the first valid terminal result, false for every rejection class.
func TestPredictionResultOutcomeVerdicts(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-outcome-verdicts", "chan-a3-ov", 100000)
	ep := confirmedRound(t, pool, s, "a3-ov-evt", 500)

	// Untracked round.
	if out := pool.handlePredictionUser(resultMsg("a3-ov-unknown", "WIN", 1000), s); out.PredictionResultAccepted {
		t.Error("untracked event must not be accepted")
	}

	// Unconfirmed bet.
	ep.BetConfirmed = false
	if out := pool.handlePredictionUser(resultMsg("a3-ov-evt", "WIN", 1000), s); out.PredictionResultAccepted {
		t.Error("unconfirmed result must not be accepted")
	}
	ep.BetConfirmed = true

	// Malformed result (missing type).
	if out := pool.handlePredictionUser(&PubSubMessage{
		Type: "prediction-result",
		Data: map[string]interface{}{
			"prediction": map[string]interface{}{
				"event_id": "a3-ov-evt",
				"result":   map[string]interface{}{"points_won": float64(1000)},
			},
		},
	}, s); out.PredictionResultAccepted {
		t.Error("malformed result must not be accepted")
	}

	// Unsupported terminal type.
	if out := pool.handlePredictionUser(resultMsg("a3-ov-evt", "CANCELED", 0), s); out.PredictionResultAccepted {
		t.Error("unsupported result type must not be accepted")
	}

	// First valid terminal result: the one and only acceptance.
	if out := pool.handlePredictionUser(resultMsg("a3-ov-evt", "WIN", 1000), s); !out.PredictionResultAccepted {
		t.Fatal("first valid terminal result must be accepted")
	}

	// Duplicate after acceptance.
	if out := pool.handlePredictionUser(resultMsg("a3-ov-evt", "WIN", 1000), s); out.PredictionResultAccepted {
		t.Error("duplicate result must not be accepted")
	}

	// prediction-made never carries the terminal verdict.
	if out := pool.handlePredictionUser(&PubSubMessage{
		Type: "prediction-made",
		Data: map[string]interface{}{
			"prediction": map[string]interface{}{"event_id": "a3-ov-evt"},
		},
	}, s); out.PredictionResultAccepted {
		t.Error("prediction-made must never carry the terminal admission verdict")
	}
}

// The admission verdict must reach the generic onMessage boundary — the same
// callback the miner consumes — through the normal full route, so no raw
// side path can bypass it.
func TestPredictionResultOutcomeReachesOnMessage(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-outcome-route", "chan-a3-or", 100000)
	pool.streamers = []*models.Streamer{s}
	confirmedRound(t, pool, s, "a3-or-evt", 500)

	var outcomes []MessageOutcome
	pool.SetMessageHandler(func(msg *PubSubMessage, streamer *models.Streamer, outcome MessageOutcome) {
		outcomes = append(outcomes, outcome)
	})

	ws := NewWebSocketClient(0, nil, 3600, 1, pool.handleMessage, nil)
	ws.handleMessage(predictionResultWire("a3-or-evt", "chan-a3-or", "WIN", 1000, "2026-08-25T10:00:00Z"))
	ws.handleMessage(predictionResultWire("a3-or-evt", "chan-a3-or", "WIN", 1000, "2026-08-25T10:00:00.100Z"))

	if len(outcomes) != 2 {
		t.Fatalf("onMessage invoked %d times, want 2", len(outcomes))
	}
	if !outcomes[0].PredictionResultAccepted {
		t.Error("first delivery must reach onMessage as accepted")
	}
	if outcomes[1].PredictionResultAccepted {
		t.Error("transport-distinct duplicate must reach onMessage as NOT accepted")
	}
}

// The valid LOSE terminal contract at the pool's producer boundary (one of
// the two adjoining boundary proofs — the miner consumer half lives in
// internal/miner's annotation tests): a tracked confirmed round's
// validator-valid LOSE (no points_won) is admitted with a true verdict
// exactly once; the duplicate is rejected.
func TestPredictionResultValidLoseAdmittedOnceWithTrueVerdict(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-lose-verdict", "chan-a3-lv", 100000)
	confirmedRound(t, pool, s, "a3-lv-evt", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	loseMsg := func() *PubSubMessage {
		return rawResultMsg("a3-lv-evt", map[string]interface{}{"type": "LOSE"})
	}
	if out := pool.handlePredictionUser(loseMsg(), s); !out.PredictionResultAccepted {
		t.Fatal("valid LOSE for a tracked confirmed round must be admitted with a true verdict")
	}
	if out := pool.handlePredictionUser(loseMsg(), s); out.PredictionResultAccepted {
		t.Error("duplicate LOSE must carry a false verdict")
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("valid LOSE emitted %d BetResults, want exactly 1", got)
	}
	if got := rec.first(); got.ResultType != "LOSE" || got.Won != 0 || got.Gained != -500 {
		t.Errorf("LOSE record = %+v, want won=0 gained=-500", got)
	}
}

// Pins the BetResultHandler lock contract: the sink runs OUTSIDE p.mu, so a
// handler that re-enters the pool through both the read-lock path (snapshot)
// and the write-lock path (SetAutoBetSkip) completes instead of deadlocking.
// The delivery runs on its own goroutine with a deadline so a broken contract
// fails cleanly rather than hanging the whole package run.
func TestPredictionResultBetResultHandlerInvokedOutsidePoolLock(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-lockfree", "chan-a3-lf", 100000)
	confirmedRound(t, pool, s, "a3-lf-evt", 500)

	var skipErr error
	pool.SetBetResultHandler(func(BetResult) {
		_ = pool.PredictionsSnapshot()
		skipErr = pool.SetAutoBetSkip("a3-lf-evt", true)
	})

	done := make(chan struct{})
	go func() {
		pool.handlePredictionUser(resultMsg("a3-lf-evt", "WIN", 1000), s)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("re-entrant BetResultHandler deadlocked: sink is being invoked under p.mu")
	}
	// The round has a placed bet, so the write-lock path must have reached its
	// decision point and reported exactly that — proving the Lock was taken.
	if !errors.Is(skipErr, ErrAutoBetPlaced) {
		t.Fatalf("SetAutoBetSkip inside the sink returned %v, want ErrAutoBetPlaced", skipErr)
	}
}
