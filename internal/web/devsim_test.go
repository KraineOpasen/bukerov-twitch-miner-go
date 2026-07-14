package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestDevPredictionsEndToEnd exercises the full route dispatch: with the dev
// simulator enabled, the Overview renders fake bettable rounds with manual
// controls, and the /api/prediction/bet route places against the simulator.
func TestDevPredictionsEndToEnd(t *testing.T) {
	t.Setenv("MINER_DEV_PREDICTIONS", "1")
	srv, _, _ := newOverviewTestServer(t)
	h := srv.handler()

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/overview", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("overview status = %d", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{"data-manual", "Сделать ставку", "Will they clutch", "data-skip"} {
		if !strings.Contains(body, want) {
			t.Errorf("dev overview missing %q", want)
		}
	}

	// Place a manual bet against the first seeded round via the real route.
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, postJSON("/api/prediction/bet", `{"eventId":"dev-evt-1","outcomeId":"o-yes","amount":500}`))
	if rr2.Code != http.StatusOK {
		t.Fatalf("bet status = %d body=%s", rr2.Code, rr2.Body.String())
	}
	var resp map[string]interface{}
	_ = json.Unmarshal(rr2.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Errorf("dev bet should succeed, got %v", resp)
	}

	// Skip route reaches the simulator too.
	rr3 := httptest.NewRecorder()
	h.ServeHTTP(rr3, postJSON("/api/prediction/skip", `{"eventId":"dev-evt-2","skip":true}`))
	if rr3.Code != http.StatusOK {
		t.Fatalf("skip status = %d", rr3.Code)
	}
}

func TestDevSimSeedsBettableRounds(t *testing.T) {
	sim := newDevPredictionSim(nil)
	sim.seed()

	preds := sim.LivePredictions()
	if len(preds) != 2 {
		t.Fatalf("expected 2 seeded rounds, got %d", len(preds))
	}
	// Both should be ACTIVE, online, and carry outcomes with ids.
	for _, p := range preds {
		if p.Status != "ACTIVE" || !p.Online {
			t.Errorf("round %s should be ACTIVE+online, got %s online=%v", p.EventID, p.Status, p.Online)
		}
		if len(p.Outcomes) == 0 || p.Outcomes[0].ID == "" {
			t.Errorf("round %s should have outcomes with ids", p.EventID)
		}
	}
}

func TestDevSimManualBetLifecycle(t *testing.T) {
	sim := newDevPredictionSim(nil)
	sim.latency = 0
	sim.seed()

	preds := sim.LivePredictions()
	// Pick the healthy-balance round.
	var target LivePrediction
	for _, p := range preds {
		if p.Balance >= 1000 {
			target = p
		}
	}
	if target.EventID == "" {
		t.Fatal("no high-balance round seeded")
	}
	outcome := target.Outcomes[0]

	// Insufficient balance path on the low-balance round.
	var low LivePrediction
	for _, p := range preds {
		if p.Balance < 1000 {
			low = p
		}
	}
	if _, err := sim.PlaceManualBet(low.EventID, low.Outcomes[0].ID, 1000); err == nil ||
		!strings.Contains(err.Error(), "not enough") {
		t.Errorf("expected insufficient-points error, got %v", err)
	}

	// Forced Twitch failure.
	sim.failNext = true
	if _, err := sim.PlaceManualBet(target.EventID, outcome.ID, 500); err == nil {
		t.Error("expected forced failure")
	}

	// Successful manual bet.
	title, err := sim.PlaceManualBet(target.EventID, outcome.ID, 500)
	if err != nil {
		t.Fatalf("manual bet: %v", err)
	}
	if title != outcome.Title {
		t.Errorf("title = %q, want %q", title, outcome.Title)
	}

	// Now a second bet is rejected as already placed, and it shows as manual.
	if _, err := sim.PlaceManualBet(target.EventID, outcome.ID, 500); err == nil ||
		!strings.Contains(err.Error(), "already been placed") {
		t.Errorf("second bet should be rejected, got %v", err)
	}
	for _, p := range sim.LivePredictions() {
		if p.EventID == target.EventID && (!p.BetPlaced || !p.ManualBet) {
			t.Errorf("round should reflect a placed manual bet: %+v", p)
		}
	}
}

func TestDevSimSkipToggle(t *testing.T) {
	sim := newDevPredictionSim(nil)
	sim.seed()
	preds := sim.LivePredictions()
	id := preds[0].EventID

	if err := sim.SetAutoBetSkip(id, true); err != nil {
		t.Fatalf("skip: %v", err)
	}
	for _, p := range sim.LivePredictions() {
		if p.EventID == id && !p.AutoBetSkipped {
			t.Error("round should be marked skipped")
		}
	}
	if err := sim.SetAutoBetSkip(id, false); err != nil {
		t.Fatalf("unskip: %v", err)
	}
}
