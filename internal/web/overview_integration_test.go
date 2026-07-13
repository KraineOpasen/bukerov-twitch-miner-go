package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// fakeOverviewProvider stands in for the miner's in-memory watch/prediction
// state during the HTTP integration test.
type fakeOverviewProvider struct {
	slots WatchSlotsView
	preds []LivePrediction
}

func (f *fakeOverviewProvider) WatchSlots() WatchSlotsView        { return f.slots }
func (f *fakeOverviewProvider) LivePredictions() []LivePrediction { return f.preds }

func newOverviewTestServer(t *testing.T) (*Server, *models.Streamer, *bool) {
	t.Helper()
	// database.Open is a process-wide singleton (sync.Once); it is shared
	// across tests and must not be closed here.
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	svc, err := analytics.NewService(db, t.TempDir())
	if err != nil {
		t.Fatalf("analytics service: %v", err)
	}

	online := models.NewStreamer("shroud", models.DefaultStreamerSettings())
	online.ChannelID = "1"
	online.SetOnline()
	online.SetChannelPoints(100000)
	online.Stream.Update("b1", "Ranked", &models.Game{Name: "VALORANT"}, nil, 40000)

	offline := models.NewStreamer("summit", models.DefaultStreamerSettings())
	offline.ChannelID = "2"
	offline.SetChannelPoints(5000)

	// Seed a couple of point samples so points-today has something to diff.
	repo := svc.Repository()
	_ = repo.RecordPoints("shroud", 95000, "WATCH")
	_ = repo.RecordPoints("shroud", 100000, "WATCH")

	srv := NewServer(config.AnalyticsSettings{Refresh: 5, DaysAgo: 7}, "tester", t.TempDir(), svc, []*models.Streamer{online, offline})
	srv.status.SetStatus(StatusRunning, "Mining active")

	prov := &fakeOverviewProvider{
		slots: WatchSlotsView{
			ActivePair: []string{"shroud"},
			Watching:   map[string]bool{"shroud": true},
			Mode:       "rotation",
		},
		preds: []LivePrediction{{
			Streamer: "shroud", Title: "Will they win?", Status: "ACTIVE",
			PredictionWindowSeconds: 60, BetPlaced: true, BetAmount: 500, TotalPoints: 500,
			Outcomes: []LivePredictionOutcome{{Title: "Yes", PercentageUsers: 60, Odds: 1.6, TotalPoints: 300, Chosen: true}},
		}},
	}
	srv.SetOverviewProvider(prov)

	// Fake settings provider/callback for the quick-action test.
	applied := new(bool)
	sp := &fakeSettingsProvider{rt: settings.RuntimeSettings{
		Streamers:       []settings.StreamerConfig{{Username: "shroud"}, {Username: "summit"}},
		DefaultSettings: settings.StreamerSettingsToDTO(models.DefaultStreamerSettings()),
	}}
	srv.SetSettingsProvider(sp)
	srv.SetSettingsUpdateCallback(func(rt settings.RuntimeSettings) {
		*applied = true
		sp.rt = rt
	})

	return srv, online, applied
}

type fakeSettingsProvider struct{ rt settings.RuntimeSettings }

func (f *fakeSettingsProvider) GetRuntimeSettings() settings.RuntimeSettings { return f.rt }
func (f *fakeSettingsProvider) GetDefaultSettings() settings.RuntimeSettings { return f.rt }

func TestHandleAPIOverview(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)

	rec := httptest.NewRecorder()
	srv.handleAPIOverview(rec, httptest.NewRequest(http.MethodGet, "/api/overview", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"shroud", "summit", "Live Predictions", "Will they win?", "▶ Watching", "● Offline", "VALORANT"} {
		if !strings.Contains(body, want) {
			t.Errorf("overview body missing %q", want)
		}
	}
}

func TestHandleAPINowWatching(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)

	rec := httptest.NewRecorder()
	srv.handleAPINowWatching(rec, httptest.NewRequest(http.MethodGet, "/api/now-watching", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "shroud") {
		t.Error("now-watching should list the watched streamer")
	}
}

func TestHandleQuickActionTogglesWatch(t *testing.T) {
	srv, _, applied := newOverviewTestServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/streamer-action/shroud", strings.NewReader(`{"action":"toggle-watch"}`))
	srv.handleAPIStreamerQuickAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success      bool `json:"success"`
		DisableWatch bool `json:"disableWatch"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || !resp.DisableWatch {
		t.Errorf("expected success + disableWatch=true, got %+v", resp)
	}
	if !*applied {
		t.Error("quick action must invoke the settings-update callback (existing pipeline)")
	}
}

func TestHandleQuickActionCyclesPreference(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/streamer-action/shroud", strings.NewReader(`{"action":"cycle-preference"}`))
	srv.handleAPIStreamerQuickAction(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Preference string `json:"preference"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Preference != "prefer" {
		t.Errorf("first cycle should set 'prefer', got %q", resp.Preference)
	}
}

func TestHandleOverviewEvents(t *testing.T) {
	srv, _, _ := newOverviewTestServer(t)

	rec := httptest.NewRecorder()
	srv.handleAPIOverviewEvents(rec, httptest.NewRequest(http.MethodGet, "/api/overview/events/shroud", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "shroud") {
		t.Error("events drawer should name the streamer")
	}
}
