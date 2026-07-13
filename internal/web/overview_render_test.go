package web

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// sampleOverview builds an OverviewData exercising every card state and board
// branch, for template-render coverage.
func sampleOverview() OverviewData {
	return OverviewData{
		Username:       "tester",
		RefreshMinutes: 5,
		Version:        "test",
		BotStatusLabel: "Running",
		Connected:      true,
		TotalPoints:    "1,234,567",
		PointsToday:    "12,345",
		StreamerCount:  4,
		LiveCount:      2,
		Ticker: []TickerItem{
			{Streamer: "shroud", Kind: "goal", Label: "New Emote", Percent: 72, HasPct: true},
		},
		Predictions: []PredictionView{
			{
				Streamer: "shroud", Title: "Will they win?", Status: "ACTIVE",
				SecondsLeftLabel: "0:42", WindowEndUnix: 1000, PoolLabel: "50,000",
				BetPlaced: true, BetConfirmed: true, BetAmount: "5,000",
				Outcomes: []PredictionOutcomeView{
					{Title: "Yes", Percent: 61, Odds: "1.60x", PointsLabel: "30,000", Chosen: true},
					{Title: "No", Percent: 39, Odds: "2.50x", PointsLabel: "20,000"},
				},
			},
			{Streamer: "ninja", Title: "Locked one", Status: "LOCKED", Locked: true},
		},
		TrackedLive: []StreamerInfo{
			{
				Name: "shroud", State: "watching", Watching: true, IsLive: true,
				PointsFormatted: "100,000", PointsPerHour: "1,200", PointsToday: "5,000",
				GameName: "VALORANT", ViewersCount: 40000, ViewersCountFormatted: "40,000",
				StreakPending: true, StreakMinutes: 5, StreakPercent: 71,
				HasCampaign: true, CampaignName: "Drop", CampaignPercent: 40, CampaignMinutesInfo: "8/20 min",
				HasGoal: true, GoalTitle: "New Emote", GoalPercent: 72,
				Preference: "prefer", HasActivePrediction: true,
				LastEventText: "Bonus claimed", LastEventAgo: "2m ago",
			},
			{
				Name: "pokimane", State: "queued", Queued: true, IsLive: true,
				PointsFormatted: "80,000", Preference: "avoid",
			},
		},
		TrackedOffline: []StreamerInfo{
			{Name: "summit", State: "offline", PointsFormatted: "5,000", OfflineDuration: "3h"},
			{Name: "benched", State: "disabled", DisableWatch: true, PointsFormatted: "1,000", WatchReason: "watching disabled"},
		},
	}
}

func TestRenderOverviewTemplates(t *testing.T) {
	tmpls := loadTemplates()
	partials := tmpls["partials"]
	if partials == nil {
		t.Fatal("partials template set failed to load")
	}

	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "overview_live", sampleOverview()); err != nil {
		t.Fatalf("render overview_live: %v", err)
	}
	out := buf.String()

	for _, want := range []string{
		"shroud", "pokimane", "summit", "benched",
		"Live Predictions", "Will they win?", "Locked",
		"▶ Watching", "◷ Queued", "⊘ Disabled", "● Offline",
		"★ Prefer", "Avoid",
		"New Emote", "72%", "1,200/h", "cycle-preference", "toggle-watch",
		"data-window-end", "data-card-streamer",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("overview_live output missing %q", want)
		}
	}
}

func TestRenderNowWatching(t *testing.T) {
	partials := loadTemplates()["partials"]
	view := NowWatchingView{
		Slots: []WatchSlotView{
			{Name: "shroud", Points: "100,000", Game: "VALORANT", HasGain: true, GainPerHour: "1,200", StreakPending: true, StreakMinutes: 5, StreakPercent: 71},
		},
		QueuedNames:      []string{"pokimane", "ninja"},
		HasNextRotation:  true,
		NextRotationUnix: 1234567890,
	}
	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "now_watching", view); err != nil {
		t.Fatalf("render now_watching: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"shroud", "VALORANT", "1,200/h", "pokimane", "Next rotation", "data-countdown-to"} {
		if !strings.Contains(out, want) {
			t.Errorf("now_watching output missing %q", want)
		}
	}
}

func TestRenderNowWatchingEmpty(t *testing.T) {
	partials := loadTemplates()["partials"]
	var buf bytes.Buffer
	if err := partials.ExecuteTemplate(&buf, "now_watching", NowWatchingView{Stale: true}); err != nil {
		t.Fatalf("render empty now_watching: %v", err)
	}
	if !strings.Contains(buf.String(), "Nothing being watched") {
		t.Error("empty now_watching should show empty-state text")
	}
}

func TestRenderEventsDrawer(t *testing.T) {
	partials := loadTemplates()["partials"]
	var buf bytes.Buffer
	data := map[string]interface{}{
		"Name": "shroud",
		"Events": []struct {
			Label string
			Ago   string
		}{{Label: "Bonus claimed", Ago: "2m ago"}},
	}
	if err := partials.ExecuteTemplate(&buf, "events_drawer", data); err != nil {
		t.Fatalf("render events_drawer: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"shroud", "Bonus claimed", "2m ago", "Full page"} {
		if !strings.Contains(out, want) {
			t.Errorf("events_drawer output missing %q", want)
		}
	}
}

func TestNextPreferenceCycle(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "prefer"},
		{"prefer", "avoid"},
		{"avoid", ""},
		{"bogus", "prefer"},
	}
	for _, c := range cases {
		if got := nextPreference(c.in); got != c.want {
			t.Errorf("nextPreference(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFmtSeconds(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{{0, "0:00"}, {5, "0:05"}, {65, "1:05"}, {-3, "0:00"}, {600, "10:00"}}
	for _, c := range cases {
		if got := fmtSeconds(c.in); got != c.want {
			t.Errorf("fmtSeconds(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestBuildPredictionViewsSortedAndMapped(t *testing.T) {
	now := time.Now()
	preds := []LivePrediction{
		{Streamer: "a", Title: "slow", Status: "ACTIVE", CreatedAt: now, PredictionWindowSeconds: 600, TotalPoints: 100,
			Outcomes: []LivePredictionOutcome{{Title: "X", PercentageUsers: 60.4, Odds: 1.6, TotalPoints: 60, Chosen: true}}},
		{Streamer: "b", Title: "fast", Status: "LOCKED", CreatedAt: now, PredictionWindowSeconds: 0},
	}
	views := buildPredictionViews(preds)
	if len(views) != 2 {
		t.Fatalf("expected 2 views, got %d", len(views))
	}
	// LOCKED/0s should sort first (soonest closing).
	if views[0].Streamer != "b" || !views[0].Locked {
		t.Errorf("expected locked 'b' first, got %+v", views[0])
	}
	var a *PredictionView
	for i := range views {
		if views[i].Streamer == "a" {
			a = &views[i]
		}
	}
	if a == nil || len(a.Outcomes) != 1 || a.Outcomes[0].Percent != 60 || a.Outcomes[0].Odds != "1.60x" || !a.Outcomes[0].Chosen {
		t.Errorf("outcome mapping wrong: %+v", a)
	}
	if a.PoolLabel != "100" {
		t.Errorf("pool label = %q, want 100", a.PoolLabel)
	}
}

func TestBotStatusLabel(t *testing.T) {
	if botStatusLabel(StatusRunning) != "Running" {
		t.Error("running label wrong")
	}
	if botStatusLabel(StatusAuthRequired) != "Login required" {
		t.Error("auth label wrong")
	}
}
