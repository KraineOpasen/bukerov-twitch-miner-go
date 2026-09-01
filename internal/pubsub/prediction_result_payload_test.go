package pubsub

import (
	"fmt"
	"math"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// rawResultMsg builds a predictions-user "prediction-result" message with an
// arbitrary decoded result object, for exercising the terminal payload
// validation boundary through the real admission handler.
func rawResultMsg(eventID string, result map[string]interface{}) *PubSubMessage {
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

// malformedWinPayouts is the defensive-validation matrix for WIN: points_won
// is required and must be a finite, mathematically integral, NON-NEGATIVE
// number inside the exactly-representable integer range of the decoded
// float64 representation.
func malformedWinPayouts() []struct {
	name   string
	result map[string]interface{}
} {
	return []struct {
		name   string
		result map[string]interface{}
	}{
		{"missing points_won", map[string]interface{}{"type": "WIN"}},
		{"non-numeric points_won", map[string]interface{}{"type": "WIN", "points_won": "1000"}},
		{"NaN points_won", map[string]interface{}{"type": "WIN", "points_won": math.NaN()}},
		{"positive-infinite points_won", map[string]interface{}{"type": "WIN", "points_won": math.Inf(1)}},
		{"fractional points_won", map[string]interface{}{"type": "WIN", "points_won": 999.5}},
		{"out-of-range points_won", map[string]interface{}{"type": "WIN", "points_won": 1e300}},
		{"null points_won", map[string]interface{}{"type": "WIN", "points_won": nil}},
		{"negative points_won", map[string]interface{}{"type": "WIN", "points_won": float64(-1)}},
		{"large negative points_won", map[string]interface{}{"type": "WIN", "points_won": float64(-9007199254740992)}},
	}
}

// A malformed WIN payout must be rejected without terminal effects and
// without consuming the round's admission: each case runs against a fresh
// confirmed round and asserts zero effects plus an unconsumed admission.
func TestPredictionResultMalformedWinPayoutRejectedPerCase(t *testing.T) {
	for i, tc := range malformedWinPayouts() {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			pool := newTestPool(&fakePlacer{})
			s := newNamedTestStreamer("a3-payload-win", fmt.Sprintf("chan-a3-pw-%d", i), 100000)
			eventID := fmt.Sprintf("a3-pw-evt-%d", i)
			ep := confirmedRound(t, pool, s, eventID, 500)

			rec := &betResultRecorder{}
			pool.SetBetResultHandler(rec.handler)

			out := pool.handlePredictionUser(rawResultMsg(eventID, tc.result), s)
			if out.PredictionResultAccepted {
				t.Error("malformed WIN payout must not be admitted")
			}
			if got := rec.count(); got != 0 {
				t.Fatalf("malformed WIN payout emitted %d BetResults, want 0", got)
			}
			if ep.ResultAccepted {
				t.Fatal("malformed WIN payout consumed the terminal admission")
			}
			if ep.Result.Type != "" {
				t.Fatalf("malformed WIN payout mutated Result to %q", ep.Result.Type)
			}
			counter, amount := historyEntry(t, s, "PREDICTION")
			if counter != 0 || amount != 0 {
				t.Fatalf("malformed WIN payout wrote PREDICTION history {%d, %d}", counter, amount)
			}

			// Non-poisoning: the same round still accepts a later valid WIN,
			// exactly once.
			pool.handlePredictionUser(resultMsg(eventID, "WIN", 1000), s)
			pool.handlePredictionUser(resultMsg(eventID, "WIN", 1000), s)
			if got := rec.count(); got != 1 {
				t.Fatalf("valid WIN after malformed payout emitted %d BetResults, want exactly 1", got)
			}
			if got := rec.first(); got.ResultType != "WIN" || got.Won != 1000 || got.Gained != 500 {
				t.Errorf("accepted WIN record = %+v, want won=1000 gained=500", got)
			}
		})
	}
}

// The full malformed gauntlet against ONE round: every malformed WIN payout
// in sequence leaves the admission unconsumed, then the valid WIN wins once.
func TestPredictionResultMalformedWinGauntletThenValidWinOnce(t *testing.T) {
	pool := newTestPool(&fakePlacer{})
	s := newNamedTestStreamer("a3-payload-gauntlet", "chan-a3-pg", 100000)
	confirmedRound(t, pool, s, "a3-pg-evt", 500)

	rec := &betResultRecorder{}
	pool.SetBetResultHandler(rec.handler)

	for _, tc := range malformedWinPayouts() {
		pool.handlePredictionUser(rawResultMsg("a3-pg-evt", tc.result), s)
	}
	if got := rec.count(); got != 0 {
		t.Fatalf("malformed gauntlet emitted %d BetResults, want 0", got)
	}

	pool.handlePredictionUser(resultMsg("a3-pg-evt", "WIN", 1000), s)
	pool.handlePredictionUser(resultMsg("a3-pg-evt", "WIN", 1000), s)
	if got := rec.count(); got != 1 {
		t.Fatalf("valid WIN after gauntlet emitted %d BetResults, want exactly 1", got)
	}
}

// LOSE: a missing points_won, an explicit JSON null (treated as absent), and
// an explicit numeric zero are the only accepted payout shapes; anything
// else is rejected without consuming the round, and a later valid LOSE
// still wins exactly once.
func TestPredictionResultLosePayoutValidation(t *testing.T) {
	t.Run("missing points_won accepted", func(t *testing.T) {
		pool := newTestPool(&fakePlacer{})
		s := newNamedTestStreamer("a3-payload-lose-m", "chan-a3-plm", 100000)
		confirmedRound(t, pool, s, "a3-plm-evt", 500)
		rec := &betResultRecorder{}
		pool.SetBetResultHandler(rec.handler)

		pool.handlePredictionUser(rawResultMsg("a3-plm-evt", map[string]interface{}{"type": "LOSE"}), s)
		if got := rec.count(); got != 1 {
			t.Fatalf("LOSE without points_won emitted %d BetResults, want 1", got)
		}
		if got := rec.first(); got.ResultType != "LOSE" || got.Won != 0 || got.Gained != -500 {
			t.Errorf("LOSE record = %+v, want won=0 gained=-500", got)
		}
	})

	t.Run("explicit zero accepted", func(t *testing.T) {
		pool := newTestPool(&fakePlacer{})
		s := newNamedTestStreamer("a3-payload-lose-z", "chan-a3-plz", 100000)
		confirmedRound(t, pool, s, "a3-plz-evt", 500)
		rec := &betResultRecorder{}
		pool.SetBetResultHandler(rec.handler)

		pool.handlePredictionUser(rawResultMsg("a3-plz-evt", map[string]interface{}{"type": "LOSE", "points_won": float64(0)}), s)
		if got := rec.count(); got != 1 {
			t.Fatalf("LOSE with zero points_won emitted %d BetResults, want 1", got)
		}
	})

	// A decoded JSON null carries no payout value: it is treated as absent,
	// so a plausibly real wire LOSE frame is never suppressed.
	t.Run("explicit null accepted as absent", func(t *testing.T) {
		pool := newTestPool(&fakePlacer{})
		s := newNamedTestStreamer("a3-payload-lose-n", "chan-a3-pln", 100000)
		confirmedRound(t, pool, s, "a3-pln-evt", 500)
		rec := &betResultRecorder{}
		pool.SetBetResultHandler(rec.handler)

		pool.handlePredictionUser(rawResultMsg("a3-pln-evt", map[string]interface{}{"type": "LOSE", "points_won": nil}), s)
		if got := rec.count(); got != 1 {
			t.Fatalf("LOSE with null points_won emitted %d BetResults, want 1", got)
		}
		if got := rec.first(); got.ResultType != "LOSE" || got.Won != 0 || got.Gained != -500 {
			t.Errorf("LOSE record = %+v, want won=0 gained=-500", got)
		}
	})

	// Same shape through the REAL wire route (json.Unmarshal produces the nil
	// value), matching the upstream miner's observed present-but-null key.
	t.Run("wire-route null accepted", func(t *testing.T) {
		pool := newTestPool(&fakePlacer{})
		s := newNamedTestStreamer("a3-payload-lose-w", "chan-a3-plw", 100000)
		pool.streamers = []*models.Streamer{s}
		confirmedRound(t, pool, s, "a3-plw-evt", 500)
		rec := &betResultRecorder{}
		pool.SetBetResultHandler(rec.handler)

		ws := NewWebSocketClient(0, nil, 3600, 1, pool.handleMessage, nil)
		ws.handleMessage(WSMessage{Type: "MESSAGE", Data: &WSData{
			Topic:   "predictions-user-v1.42",
			Message: `{"type":"prediction-result","data":{"timestamp":"2026-08-25T10:00:00Z","prediction":{"event_id":"a3-plw-evt","channel_id":"chan-a3-plw","result":{"type":"LOSE","points_won":null,"is_acknowledged":false}}}}`,
		}})
		if got := rec.count(); got != 1 {
			t.Fatalf("wire LOSE with null points_won emitted %d BetResults, want 1", got)
		}
	})

	rejected := []struct {
		name   string
		payout interface{}
	}{
		{"non-numeric", "0"},
		{"non-zero", float64(500)},
		{"NaN", math.NaN()},
		{"fractional", 0.5},
	}
	for i, tc := range rejected {
		tc := tc
		t.Run("rejected "+tc.name, func(t *testing.T) {
			pool := newTestPool(&fakePlacer{})
			s := newNamedTestStreamer("a3-payload-lose-r", fmt.Sprintf("chan-a3-plr-%d", i), 100000)
			eventID := fmt.Sprintf("a3-plr-evt-%d", i)
			ep := confirmedRound(t, pool, s, eventID, 500)
			rec := &betResultRecorder{}
			pool.SetBetResultHandler(rec.handler)

			out := pool.handlePredictionUser(rawResultMsg(eventID, map[string]interface{}{"type": "LOSE", "points_won": tc.payout}), s)
			if out.PredictionResultAccepted || rec.count() != 0 || ep.ResultAccepted {
				t.Fatalf("contradictory LOSE payout (%s) was not rejected cleanly: accepted=%v emissions=%d consumed=%v",
					tc.name, out.PredictionResultAccepted, rec.count(), ep.ResultAccepted)
			}

			pool.handlePredictionUser(resultMsg(eventID, "LOSE", 0), s)
			pool.handlePredictionUser(resultMsg(eventID, "LOSE", 0), s)
			if got := rec.count(); got != 1 {
				t.Fatalf("valid LOSE after rejected payout emitted %d BetResults, want exactly 1", got)
			}
		})
	}
}

// REFUND: same accepted/rejected payout shapes as LOSE, and the accepted
// path must preserve the live raw-stake ROI contract.
func TestPredictionResultRefundPayoutValidation(t *testing.T) {
	t.Run("missing points_won accepted", func(t *testing.T) {
		pool := newTestPool(&fakePlacer{})
		s := newNamedTestStreamer("a3-payload-refund-m", "chan-a3-prm", 100000)
		confirmedRound(t, pool, s, "a3-prm-evt", 750)
		rec := &betResultRecorder{}
		pool.SetBetResultHandler(rec.handler)

		pool.handlePredictionUser(rawResultMsg("a3-prm-evt", map[string]interface{}{"type": "REFUND"}), s)
		if got := rec.count(); got != 1 {
			t.Fatalf("REFUND without points_won emitted %d BetResults, want 1", got)
		}
		if got := rec.first(); got.ResultType != "REFUND" || got.Placed != 750 || got.Won != 0 || got.Gained != 0 {
			t.Errorf("REFUND record = %+v, want raw stake 750 and net zero", got)
		}
	})

	t.Run("explicit zero accepted", func(t *testing.T) {
		pool := newTestPool(&fakePlacer{})
		s := newNamedTestStreamer("a3-payload-refund-z", "chan-a3-prz", 100000)
		confirmedRound(t, pool, s, "a3-prz-evt", 750)
		rec := &betResultRecorder{}
		pool.SetBetResultHandler(rec.handler)

		pool.handlePredictionUser(rawResultMsg("a3-prz-evt", map[string]interface{}{"type": "REFUND", "points_won": float64(0)}), s)
		if got := rec.count(); got != 1 {
			t.Fatalf("REFUND with zero points_won emitted %d BetResults, want 1", got)
		}
	})

	t.Run("explicit null accepted as absent", func(t *testing.T) {
		pool := newTestPool(&fakePlacer{})
		s := newNamedTestStreamer("a3-payload-refund-n", "chan-a3-prn", 100000)
		confirmedRound(t, pool, s, "a3-prn-evt", 750)
		rec := &betResultRecorder{}
		pool.SetBetResultHandler(rec.handler)

		pool.handlePredictionUser(rawResultMsg("a3-prn-evt", map[string]interface{}{"type": "REFUND", "points_won": nil}), s)
		if got := rec.count(); got != 1 {
			t.Fatalf("REFUND with null points_won emitted %d BetResults, want 1", got)
		}
		if got := rec.first(); got.Placed != 750 || got.Gained != 0 {
			t.Errorf("REFUND record = %+v, want raw stake 750 and net zero", got)
		}
	})

	rejected := []struct {
		name   string
		payout interface{}
	}{
		{"non-numeric", "0"},
		{"non-zero", float64(250)},
		{"negative-infinite", math.Inf(-1)},
		{"fractional", 0.5},
	}
	for i, tc := range rejected {
		tc := tc
		t.Run("rejected "+tc.name, func(t *testing.T) {
			pool := newTestPool(&fakePlacer{})
			s := newNamedTestStreamer("a3-payload-refund-r", fmt.Sprintf("chan-a3-prr-%d", i), 100000)
			eventID := fmt.Sprintf("a3-prr-evt-%d", i)
			ep := confirmedRound(t, pool, s, eventID, 750)
			rec := &betResultRecorder{}
			pool.SetBetResultHandler(rec.handler)

			out := pool.handlePredictionUser(rawResultMsg(eventID, map[string]interface{}{"type": "REFUND", "points_won": tc.payout}), s)
			if out.PredictionResultAccepted || rec.count() != 0 || ep.ResultAccepted {
				t.Fatalf("contradictory REFUND payout (%s) was not rejected cleanly: accepted=%v emissions=%d consumed=%v",
					tc.name, out.PredictionResultAccepted, rec.count(), ep.ResultAccepted)
			}

			pool.handlePredictionUser(resultMsg(eventID, "REFUND", 0), s)
			pool.handlePredictionUser(resultMsg(eventID, "REFUND", 0), s)
			if got := rec.count(); got != 1 {
				t.Fatalf("valid REFUND after rejected payout emitted %d BetResults, want exactly 1", got)
			}
			if got := rec.first(); got.Placed != 750 {
				t.Errorf("accepted REFUND lost the raw stake: %+v", got)
			}
		})
	}
}
