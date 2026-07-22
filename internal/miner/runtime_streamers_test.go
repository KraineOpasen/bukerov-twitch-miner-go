package miner

import (
	"context"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// fakeStreamerAPI satisfies the streamer manager's twitchClient slice so
// ApplySettings can resolve runtime-added streamers without HTTP.
type fakeStreamerAPI struct{}

func (fakeStreamerAPI) GetChannelID(username string) (string, error)    { return "chan-" + username, nil }
func (fakeStreamerAPI) LoadChannelPointsContext(*models.Streamer) error { return nil }
func (fakeStreamerAPI) CheckStreamerOnline(*models.Streamer) models.StatusTransition {
	return models.StatusTransition{}
}

// fakeDropsGQL satisfies the drops tracker's twitchClient slice. The dashboard
// is empty and one in-progress campaign is recovered from the inventory, so a
// synchronous SyncNow yields exactly one tracked campaign ("campaign-wot",
// game-wot, unrestricted) for assignment.
type fakeDropsGQL struct{}

func (fakeDropsGQL) PostGQL(op constants.GQLOperation) (map[string]interface{}, error) {
	switch op.OperationName {
	case "ViewerDropsDashboard":
		return map[string]interface{}{
			"data": map[string]interface{}{
				"currentUser": map[string]interface{}{
					"dropCampaigns": []interface{}{},
				},
			},
		}, nil
	case "Inventory":
		return map[string]interface{}{
			"data": map[string]interface{}{
				"currentUser": map[string]interface{}{
					"inventory": map[string]interface{}{
						"dropCampaignsInProgress": []interface{}{
							map[string]interface{}{
								"id":   "campaign-wot",
								"name": "World of Tanks Drops",
								"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
								"timeBasedDrops": []interface{}{
									map[string]interface{}{
										"id":                     "drop-1",
										"name":                   "Garage Slot",
										"requiredMinutesWatched": float64(120),
										"self": map[string]interface{}{
											"currentMinutesWatched": float64(10),
											"isClaimed":             false,
										},
									},
								},
							},
						},
					},
				},
			},
		}, nil
	}
	return map[string]interface{}{}, nil
}

func (fakeDropsGQL) GetDropCampaignDetails(string) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (fakeDropsGQL) ClaimDrop(*models.Drop) (api.ClaimStatus, error) {
	return api.ClaimStatusRejected, nil
}

// debugHasDecision reports whether the watcher's last published debug state
// carries a per-streamer decision for the given login — i.e. whether the watch
// loop's own roster contains (and evaluated) that streamer.
func debugHasDecision(w *watcher.MinuteWatcher, login string) bool {
	for _, d := range w.GetDebugState().Decisions {
		if d.Username == login {
			return true
		}
	}
	return false
}

func waitForCondition(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// TestApplySettingsPropagatesRuntimeRosterToWatcherAndDrops is the end-to-end
// wiring guard for the runtime streamer add/remove bug: a streamer added via
// the Settings apply path must reach the watch loop's own roster within one
// tick (observable through the published watch decisions) and receive drop
// campaigns on the next sync pass; a removed one must leave both. Reverting
// the ApplySettings -> watcher/dropsTracker UpdateStreamers wiring makes this
// fail on the corresponding leg while all package-local tests still pass.
func TestApplySettingsPropagatesRuntimeRosterToWatcherAndDrops(t *testing.T) {
	cfg := &config.Config{
		Username:         "tester",
		Streamers:        []config.StreamerConfig{{Username: "alpha"}},
		StreamerSettings: models.DefaultStreamerSettings(),
		Priority:         []config.Priority{config.PriorityOrder},
		RateLimits:       config.RateLimitSettings{MinuteWatchedInterval: 1},
	}

	mgr := streamer.NewManager(fakeStreamerAPI{}, cfg.StreamerSettings)
	if added, _ := mgr.ApplySettings(cfg.Streamers, cfg.StreamerSettings); len(added) != 1 {
		t.Fatalf("seeding the manager failed: added=%d, want 1", len(added))
	}

	w := watcher.NewMinuteWatcher(nil, mgr.All(), cfg.Priority, cfg.RateLimits, nil)
	d := drops.NewDropsTracker(fakeDropsGQL{}, mgr.All(), cfg.RateLimits, nil)

	m := &Miner{config: cfg, streamers: mgr, watcher: w, dropsTracker: d}

	// Runtime ADD via the same full-body path the Settings page posts:
	// current settings + one extra streamer. ClaimDrops makes it a drops
	// candidate; DisableWatch is left OFF because a watch opt-out now (correctly)
	// also blocks drop-campaign ASSIGNMENT (a channel that never watches never
	// farms). beta stays out of actual watch slots for the test's duration via the
	// watcher's 30s new-online settle guard, so no network send occurs.
	rs := settings.BuildRuntimeSettings(cfg)
	disableWatch, claimDrops := false, true
	rs.Streamers = append(rs.Streamers, settings.StreamerConfig{
		Username: "beta",
		Settings: &settings.StreamerSettingsConfig{DisableWatch: &disableWatch, ClaimDrops: &claimDrops},
	})
	m.ApplySettings(rs)

	beta := mgr.Get("beta")
	if beta == nil {
		t.Fatal("runtime-added streamer missing from the manager")
	}
	// Make beta a live candidate BEFORE the loops observe it (no concurrent
	// writes): online, streaming the recovered campaign's game.
	beta.SetConfirmedOnline()
	beta.Stream.Game = &models.Game{ID: "game-wot", Name: "World of Tanks"}
	beta.Stream.SetCampaignIDs([]string{"campaign-wot"})

	// Drops leg (add): one synchronous sync pass assigns the campaign.
	d.SyncNow()
	if got := beta.Stream.GetCampaigns(); len(got) != 1 || got[0].ID != "campaign-wot" {
		t.Fatalf("drops tracker did not adopt the runtime-added streamer: campaigns=%v", got)
	}

	// Watcher leg (add): the loop's first tick applies the staged roster.
	// (ApplyToConfig runs config.ValidateConfig, which clamps
	// MinuteWatchedInterval to >=30s, so the test drives tick boundaries via
	// Start's immediate first pass instead of waiting out the interval.)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.Start(ctx)
	waitForCondition(t, "watch loop to adopt the runtime-added streamer", 5*time.Second, func() bool {
		return debugHasDecision(w, "beta")
	})
	w.Stop()

	// Runtime REMOVE via the same path: full body minus beta.
	rs2 := settings.BuildRuntimeSettings(m.config)
	var keep []settings.StreamerConfig
	for _, sc := range rs2.Streamers {
		if sc.Username != "beta" {
			keep = append(keep, sc)
		}
	}
	rs2.Streamers = keep
	m.ApplySettings(rs2)

	// Watcher leg (remove): the next tick drops beta from the loop's roster.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	w.Start(ctx2)
	defer w.Stop()
	waitForCondition(t, "watch loop to drop the removed streamer", 5*time.Second, func() bool {
		return !debugHasDecision(w, "beta")
	})
	w.Stop()

	// Drops leg (remove): a fresh pass no longer assigns to beta.
	beta.Stream.SetCampaigns(nil)
	d.SyncNow()
	if got := beta.Stream.GetCampaigns(); len(got) != 0 {
		t.Fatalf("removed streamer still receives campaign assignments: %v", got)
	}
}
