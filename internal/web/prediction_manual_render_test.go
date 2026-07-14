package web

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// TestBuildPredictionViewsManualGating verifies the ManualAllowed gate and the
// outcome Selectable flag are derived correctly from the live state.
func TestBuildPredictionViewsManualGating(t *testing.T) {
	now := time.Now()
	preds := []LivePrediction{
		// Bettable: open, online, no bet, balance covers the minimum.
		{
			Streamer: "open", EventID: "e-open", Title: "Open", Status: "ACTIVE",
			CreatedAt: now, PredictionWindowSeconds: 600, Online: true, Balance: 5000,
			Outcomes: []LivePredictionOutcome{{ID: "o1", Title: "Yes", PercentageUsers: 60}},
		},
		// Not bettable: streamer offline.
		{
			Streamer: "off", EventID: "e-off", Title: "Offline", Status: "ACTIVE",
			CreatedAt: now, PredictionWindowSeconds: 600, Online: false, Balance: 5000,
			Outcomes: []LivePredictionOutcome{{ID: "o1", Title: "Yes"}},
		},
		// Not bettable: balance below the minimum.
		{
			Streamer: "poor", EventID: "e-poor", Title: "Poor", Status: "ACTIVE",
			CreatedAt: now, PredictionWindowSeconds: 600, Online: true, Balance: 3,
			Outcomes: []LivePredictionOutcome{{ID: "o1", Title: "Yes"}},
		},
		// Not bettable: already has a manual bet -> shows placed/lock state.
		{
			Streamer: "done", EventID: "e-done", Title: "Done", Status: "ACTIVE",
			CreatedAt: now, PredictionWindowSeconds: 600, Online: true, Balance: 5000,
			BetPlaced: true, ManualBet: true, BetAmount: 250, BetOutcomeTitle: "Yes",
			Outcomes: []LivePredictionOutcome{{ID: "o1", Title: "Yes", Chosen: true}},
		},
	}

	views := buildPredictionViews(preds)
	byID := map[string]PredictionView{}
	for _, v := range views {
		byID[v.EventID] = v
	}

	if !byID["e-open"].ManualAllowed {
		t.Error("open round should allow manual betting")
	}
	if !byID["e-open"].Outcomes[0].Selectable {
		t.Error("open round outcome should be selectable")
	}
	if byID["e-off"].ManualAllowed {
		t.Error("offline round must not allow manual betting")
	}
	if byID["e-poor"].ManualAllowed {
		t.Error("insufficient balance must not allow manual betting")
	}
	if byID["e-done"].ManualAllowed {
		t.Error("already-bet round must not allow manual betting")
	}
	if !byID["e-done"].ManualBet {
		t.Error("done round should carry the manual-bet flag")
	}
}

// TestRenderPredictionCardManualControls renders the prediction card in each of
// its key states and asserts the right controls appear.
func TestRenderPredictionCardManualControls(t *testing.T) {
	partials := testPartials(t)
	if partials == nil {
		t.Fatal("partials failed to load")
	}

	render := func(pv PredictionView) string {
		var buf bytes.Buffer
		if err := partials.ExecuteTemplate(&buf, "prediction_card", pv); err != nil {
			t.Fatalf("render prediction_card: %v", err)
		}
		return buf.String()
	}

	// Manual-allowed card shows the form, amount input, quick-fills, place
	// button and the skip toggle, and marks outcomes selectable.
	open := PredictionView{
		Streamer: "shroud", EventID: "e1", Title: "Will they win?", Status: "ACTIVE",
		SecondsLeftLabel: "1:00", ManualAllowed: true, Balance: 5000, BalanceLabel: "5,000", MinBet: 10,
		Outcomes: []PredictionOutcomeView{{ID: "o1", Title: "Yes", Percent: 60, Selectable: true}},
	}
	out := render(open)
	for _, want := range []string{
		`data-event-id="e1"`, "data-manual", "data-amount", "data-fill", "Place bet",
		"data-skip", "Don't auto-bet this round", `data-outcome-id="o1"`, "Available",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("manual-allowed card missing %q", want)
		}
	}

	// Placed manual bet shows the lock state, not the form.
	placed := PredictionView{
		Streamer: "shroud", EventID: "e1", Title: "Will they win?", Status: "ACTIVE",
		BetPlaced: true, ManualBet: true, BetConfirmed: true, BetAmount: "250", BetOutcomeTitle: "Yes",
		Outcomes: []PredictionOutcomeView{{ID: "o1", Title: "Yes", Chosen: true}},
	}
	out = render(placed)
	if !strings.Contains(out, "Manual bet") || !strings.Contains(out, "auto-bet locked") {
		t.Errorf("placed manual bet should show lock state, got:\n%s", out)
	}
	if strings.Contains(out, "data-manual") {
		t.Error("placed card must not render the manual form")
	}

	// Skipped-but-not-bettable round shows the skipped state with an undo.
	skipped := PredictionView{
		Streamer: "shroud", EventID: "e1", Title: "Will they win?", Status: "ACTIVE",
		AutoBetSkipped: true, SkipUndoable: true,
		Outcomes: []PredictionOutcomeView{{ID: "o1", Title: "Yes"}},
	}
	out = render(skipped)
	if !strings.Contains(out, "Auto-bet skipped") || !strings.Contains(out, "data-unskip") {
		t.Errorf("skipped card should show skipped state + undo, got:\n%s", out)
	}

	// A server error is rendered persistently.
	errored := PredictionView{
		Streamer: "shroud", EventID: "e1", Title: "Will they win?", Status: "ACTIVE",
		ManualAllowed: true, Balance: 5000, BalanceLabel: "5,000", MinBet: 10,
		ManualError: "not enough channel points for that bet",
		Outcomes:    []PredictionOutcomeView{{ID: "o1", Title: "Yes", Selectable: true}},
	}
	out = render(errored)
	if !strings.Contains(out, "not enough channel points") || !strings.Contains(out, "data-server-error") {
		t.Errorf("error should render persistently, got:\n%s", out)
	}
}
