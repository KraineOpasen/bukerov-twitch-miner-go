package notifications

import (
	"strings"
	"testing"
)

func TestDailySummaryRenderPredictionAsComponent(t *testing.T) {
	s := DailySummary{
		Date:              "2026-07-13",
		EarnedPoints:      910,
		PredictionNet:     390,
		PredictionWins:    2,
		PredictionLosses:  1,
		ClaimedDrops:      3,
		Streaks:           5,
		RecoveryIncidents: 1,
		LostMiningMinutes: 12,
	}
	title, msg := s.Render()

	if !strings.Contains(title, "2026-07-13") {
		t.Errorf("title missing date: %q", title)
	}
	// The crux: prediction net must read as a COMPONENT of net points, on the
	// same line — never as an independent parallel number.
	want := "Net points: +910 (of which +390 from predictions)"
	if !strings.Contains(msg, want) {
		t.Fatalf("net-points line wrong.\n got: %s\nwant substring: %s", msg, want)
	}
	if strings.Contains(msg, "Drops claimed: 3") == false {
		t.Errorf("missing drops line: %s", msg)
	}
	if !strings.Contains(msg, "Watch streaks: 5") {
		t.Errorf("missing streaks line: %s", msg)
	}
	if !strings.Contains(msg, "Predictions: 2W / 1L") {
		t.Errorf("missing predictions detail: %s", msg)
	}
	if !strings.Contains(msg, "best-effort") {
		t.Errorf("best-effort caveat must be present: %s", msg)
	}
}

func TestDailySummaryRenderNoPredictionsOmitsComponent(t *testing.T) {
	s := DailySummary{Date: "2026-07-13", EarnedPoints: 500, PredictionNet: 0}
	_, msg := s.Render()
	if !strings.Contains(msg, "Net points: +500") {
		t.Fatalf("net points line wrong: %s", msg)
	}
	if strings.Contains(msg, "from predictions") {
		t.Errorf("no-prediction summary must not mention the prediction component: %s", msg)
	}
}

func TestDailySummaryRenderNegativeNet(t *testing.T) {
	s := DailySummary{Date: "2026-07-13", EarnedPoints: -200, PredictionNet: -260, PredictionWins: 0, PredictionLosses: 2}
	_, msg := s.Render()
	if !strings.Contains(msg, "Net points: -200 (of which -260 from predictions)") {
		t.Fatalf("negative net rendering wrong: %s", msg)
	}
}
