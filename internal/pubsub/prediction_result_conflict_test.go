package pubsub

import (
	"fmt"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// All ordered distinct pairs of terminal result types: the first valid
// terminal result wins, its snapshot stays authoritative, terminal effects
// happen once, and the conflicting second result is inert.
func TestPredictionResultConflictPermutations(t *testing.T) {
	types := []string{"WIN", "LOSE", "REFUND"}
	pointsFor := map[string]float64{"WIN": 1000, "LOSE": 0, "REFUND": 0}

	for i, first := range types {
		for j, second := range types {
			if first == second {
				continue
			}
			first, second := first, second
			t.Run(first+"_then_"+second, func(t *testing.T) {
				pool := newTestPool(&fakePlacer{})
				s := newNamedTestStreamer("a3-conflict", fmt.Sprintf("chan-a3-cf-%d-%d", i, j), 100000)
				eventID := fmt.Sprintf("a3-cf-evt-%d-%d", i, j)
				ep := confirmedRound(t, pool, s, eventID, 500)

				rec := &betResultRecorder{}
				pool.SetBetResultHandler(rec.handler)

				out1 := pool.handlePredictionUser(resultMsg(eventID, first, pointsFor[first]), s)
				out2 := pool.handlePredictionUser(resultMsg(eventID, second, pointsFor[second]), s)

				if !out1.PredictionResultAccepted {
					t.Fatalf("first valid %s must be admitted", first)
				}
				if out2.PredictionResultAccepted {
					t.Fatalf("conflicting second %s after %s must be inert", second, first)
				}
				if got := rec.count(); got != 1 {
					t.Fatalf("conflict pair emitted %d BetResults, want exactly 1", got)
				}
				if got := rec.first(); got.ResultType != first {
					t.Errorf("emitted result type = %q, want first-accepted %q", got.ResultType, first)
				}
				if ep.Result.Type != models.PredictionResultType(first) {
					t.Errorf("event result snapshot = %q, want %q preserved", ep.Result.Type, first)
				}
				if got := ringBetResults(s.GetUsername()); got != 1 {
					t.Errorf("ring recorded %d bet_result events, want exactly 1", got)
				}

				// History/compensation happened exactly once, with the
				// single-delivery values of the FIRST result type.
				pCounter, pAmount := historyEntry(t, s, "PREDICTION")
				rCounter, rAmount := historyEntry(t, s, "REFUND")
				switch first {
				case "WIN":
					if pCounter != 0 || pAmount != -500 {
						t.Errorf("PREDICTION history = {%d, %d}, want single-WIN {0, -500}", pCounter, pAmount)
					}
				case "LOSE":
					if pCounter != 1 || pAmount != -500 {
						t.Errorf("PREDICTION history = {%d, %d}, want single-LOSE {1, -500}", pCounter, pAmount)
					}
				case "REFUND":
					if pCounter != 1 || pAmount != 0 || rCounter != -1 || rAmount != 0 {
						t.Errorf("history PREDICTION={%d, %d} REFUND={%d, %d}, want single-REFUND {1,0}/{-1,0}",
							pCounter, pAmount, rCounter, rAmount)
					}
				}
				if second != "REFUND" {
					if first != "REFUND" && (rCounter != 0 || rAmount != 0) {
						t.Errorf("REFUND history = {%d, %d}, want untouched", rCounter, rAmount)
					}
				}
			})
		}
	}
}
