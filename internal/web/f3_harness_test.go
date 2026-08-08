package web

// F3 browser-evidence harness. Runs a real dashboard server on localhost with
// deterministic fake providers so Playwright can capture baseline/post
// evidence for the six evolved pages. Env-gated: skipped unless
// MINER_F3_HARNESS=1. Never talks to Twitch, Discord, or any network.
//
// Usage:
//   MINER_F3_HARNESS=1 MINER_F3_HARNESS_ADDR=127.0.0.1:8973 \
//     go test -run TestF3EvidenceHarness -timeout 3600s ./internal/web/
//
// The server stops when the harness receives SIGINT/SIGTERM or the timeout
// elapses.

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/discovery"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// --- fakes (harness-local names, prefixed f3 to avoid collisions) ---

type f3Campaigns struct{ campaigns []*models.Campaign }

func (f *f3Campaigns) Campaigns() []*models.Campaign { return f.campaigns }
func (f *f3Campaigns) SyncStatus() drops.SyncStatus {
	return drops.SyncStatus{LastSyncAt: time.Now().Add(-2 * time.Minute), LastSuccessAt: time.Now().Add(-2 * time.Minute), Runs: 7}
}
func (f *f3Campaigns) RequestManualSync() drops.ManualSyncResult {
	return drops.ManualSyncResult{Triggered: true, Status: f.SyncStatus()}
}

type f3Catalog struct {
	upcoming []*models.Campaign
	past     []drops.CatalogRecord
}

func (f *f3Catalog) UpcomingCampaigns() []*models.Campaign         { return f.upcoming }
func (f *f3Catalog) RelevantUpcomingCampaigns() []*models.Campaign { return f.upcoming }
func (f *f3Catalog) CampaignSyncStatus() drops.SyncStatus {
	return drops.SyncStatus{LastSyncAt: time.Now().Add(-3 * time.Minute), LastSuccessAt: time.Now().Add(-3 * time.Minute), Runs: 7}
}
func (f *f3Catalog) PastCampaigns() ([]drops.CatalogRecord, error) { return f.past, nil }

type f3Discovery struct{}

func (f3Discovery) State() discovery.State {
	return discovery.State{
		Enabled: true,
		Games:   []string{"World of Tanks"},
		Channels: []discovery.ChannelState{
			{Login: "tanker_one", Game: "World of Tanks", Viewers: 5321, Status: "watching", MinutesWatched: 42},
			{Login: "tanker_two", Game: "World of Tanks", Viewers: 812, Status: "available"},
			{Login: "tanker_off", Game: "World of Tanks", Viewers: 0, Status: "offline"},
		},
		Watching: "tanker_one",
		LastSync: time.Now().Add(-40 * time.Second),
	}
}

type f3Health struct{ settings config.HealthSettings }

func (f *f3Health) HealthSnapshot() health.Snapshot {
	now := time.Now()
	return health.Snapshot{
		ActiveClientID: "abcdef1234",
		Signals: []health.Signal{
			{Name: health.SignalOAuth, Status: health.StatusOK, CheckedAt: now.Add(-24 * time.Second), Duration: 180 * time.Millisecond, Stage: "validate", Detail: "token valid"},
			{Name: health.SignalGQLAPI, Status: health.StatusDegraded, CheckedAt: now.Add(-70 * time.Second), Duration: 900 * time.Millisecond, Stage: "query", Detail: "2 retries in last window"},
			{Name: health.SignalPubSub, Status: health.StatusFailed, CheckedAt: now.Add(-3 * time.Minute), Duration: 2 * time.Second, Stage: "connect", Detail: "websocket dial timed out", ErrorCode: "WS_TIMEOUT"},
			{Name: health.SignalWatchTransport, Status: health.StatusOK, CheckedAt: now.Add(-90 * time.Second), Duration: 240 * time.Millisecond, Stage: "beacon", Detail: "minute-watched accepted"},
			{Name: health.SignalDropsInventory, Status: health.StatusIdle, CheckedAt: now.Add(-45 * time.Minute), Stage: "sync", Detail: "no active drops"},
			{Name: health.SignalDropsProgress, Status: health.StatusUnknown},
		},
	}
}
func (f *f3Health) RunCanaryNow()                                {}
func (f *f3Health) CurrentHealthSettings() config.HealthSettings { return f.settings }
func (f *f3Health) ApplyHealthSettings(cfg config.HealthSettings) error {
	f.settings = cfg
	return nil
}

type f3Progress struct{}

func (f3Progress) DropProgress() health.ProgressSnapshot {
	return health.ProgressSnapshot{Enabled: true, EvaluatedAt: time.Now()}
}

type f3Policy struct {
	mode      string
	decisions []policy.Decision
	rules     map[string]config.DropRule
}

func (f *f3Policy) PolicySnapshot() (policy.Mode, []policy.Decision) {
	return policy.Mode(f.mode), f.decisions
}
func (f *f3Policy) CurrentCampaignPolicy() (string, map[string]config.DropRule) {
	return f.mode, f.rules
}
func (f *f3Policy) ApplyCampaignPolicy(mode string)                    { f.mode = mode }
func (f *f3Policy) SetDropRule(rewardKey string, rule config.DropRule) { f.rules[rewardKey] = rule }

type f3Settings struct{ rt settings.RuntimeSettings }

func (f *f3Settings) GetRuntimeSettings() settings.RuntimeSettings { return f.rt }
func (f *f3Settings) GetDefaultSettings() settings.RuntimeSettings { return f.rt }

type f3Followed struct{}

func (f3Followed) FollowedChannels() ([]twitch.FollowedChannel, bool, error) {
	return []twitch.FollowedChannel{
		{Login: "streamer_a", DisplayName: "Streamer A"},
		{Login: "streamer_b", DisplayName: "Streamer B"},
	}, false, nil
}
func (f3Followed) TrackedUsernames() []string { return []string{"streamer_a"} }
func (f3Followed) ImportStreamers(_ context.Context, logins []string) (int, error) {
	return len(logins), nil
}

// --- fixture builders ---

func f3BuildCampaigns() []*models.Campaign {
	now := time.Now()
	mk := func(id, name, game string, drops []*models.Drop, claimed bool) *models.Campaign {
		c := &models.Campaign{
			ID: id, Name: name,
			Game:    &models.Game{ID: "g" + id, Name: game, DisplayName: game},
			StartAt: now.Add(-48 * time.Hour), EndAt: now.Add(30 * time.Hour),
			Drops: drops,
		}
		if claimed {
			c.ClaimStatus = models.CampaignClaimStatusAlreadyClaimed
		} else {
			c.ClaimStatus = models.CampaignClaimStatusInProgress
		}
		return c
	}
	return []*models.Campaign{
		mk("c1", "Anniversary Drops", "World of Tanks", []*models.Drop{
			{ID: "d1", Name: "Gold Crate", Benefit: "Gold Crate", MinutesRequired: 240, CurrentMinutesWatched: 180, PercentageProgress: 75},
			{ID: "d2", Name: "Premium Day", Benefit: "Premium Day", MinutesRequired: 480, CurrentMinutesWatched: 180, PercentageProgress: 37},
		}, false),
		mk("c2", "Winter Event", "Rust", []*models.Drop{
			{ID: "d3", Name: "Snow Jacket", Benefit: "Snow Jacket", MinutesRequired: 120, CurrentMinutesWatched: 120, PercentageProgress: 100, IsClaimed: true},
		}, true),
	}
}

func f3BuildUpcoming() []*models.Campaign {
	now := time.Now()
	return []*models.Campaign{
		{
			ID: "u1", Name: "Spring Marathon", Game: &models.Game{ID: "gu1", Name: "World of Warships", DisplayName: "World of Warships"},
			StartAt: now.Add(26 * time.Hour), EndAt: now.Add(6 * 24 * time.Hour),
			Drops: []*models.Drop{{ID: "ud1", Name: "Camo Pack", Benefit: "Camo Pack", MinutesRequired: 60}},
		},
	}
}

func f3BuildPast() []drops.CatalogRecord {
	now := time.Now()
	return []drops.CatalogRecord{
		{CampaignID: "p1", CampaignKey: "wot|autumn", Name: "Autumn Event", Game: "World of Tanks", StartAt: now.Add(-30 * 24 * time.Hour), EndAt: now.Add(-20 * 24 * time.Hour), Status: "EXPIRED", Claimed: true},
		{CampaignID: "p2", CampaignKey: "wot|autumn", Name: "Autumn Event", Game: "World of Tanks", StartAt: now.Add(-60 * 24 * time.Hour), EndAt: now.Add(-50 * 24 * time.Hour), Status: "EXPIRED", Claimed: false},
		{CampaignID: "p3", CampaignKey: "rust|summer", Name: "Summer Skins", Game: "Rust", StartAt: now.Add(-15 * 24 * time.Hour), EndAt: now.Add(-8 * 24 * time.Hour), Status: "EXPIRED", Claimed: false},
	}
}

func f3WriteLogFixture(t *testing.T, dir, username string, n int) {
	lines := []string{
		`time=2026-07-30T09:00:00.000+00:00 level=INFO msg="Twitch Channel Points Miner" version=v0.27.1`,
		`time=2026-07-30T09:00:01.000+00:00 level=INFO msg="Authenticating with Twitch"`,
		`time=2026-07-30T09:00:02.000+00:00 level=INFO msg="Authentication successful" user=devuser`,
		`time=2026-07-30T09:00:03.000+00:00 level=INFO msg="Loading streamers" count=3`,
		`time=2026-07-30T09:00:04.000+00:00 level=INFO msg="WebSocket connected" pool=1`,
		`time=2026-07-30T09:00:05.000+00:00 level=INFO msg="Streamer is online" streamer=streamer_a`,
		`time=2026-07-30T09:00:06.000+00:00 level=INFO msg="Points earned" reason=WATCH points=10 streamer=streamer_a`,
		`time=2026-07-30T09:00:07.000+00:00 level=INFO msg="Points earned" reason=WATCH_STREAK points=450 streamer=streamer_a`,
		`time=2026-07-30T09:00:08.000+00:00 level=INFO msg="Points earned" reason=CLAIM points=50 streamer=streamer_a`,
		`time=2026-07-30T09:00:09.000+00:00 level=WARN msg="Request retry scheduled" attempt=2`,
		`time=2026-07-30T09:00:10.000+00:00 level=ERROR msg="GraphQL request failed" status=502`,
		`time=2026-07-30T09:00:11.000+00:00 level=INFO msg="Reconnecting WebSocket" attempt=1`,
		`time=2026-07-30T09:00:12.000+00:00 level=INFO msg="Prediction result" result=WIN points=1200 streamer=streamer_a`,
		`time=2026-07-30T09:00:13.000+00:00 level=INFO msg="Prediction result" result=LOSE points=-500 streamer=streamer_b`,
		`time=2026-07-30T09:00:14.000+00:00 level=INFO msg="Watch slot assigned" streamer=streamer_b`,
		`time=2026-07-30T09:00:15.000+00:00 level=INFO msg="Claiming drop" drop="Gold Crate"`,
		`time=2026-07-30T09:00:16.000+00:00 level=INFO msg="Claimed drop" drop="Gold Crate"`,
		`time=2026-07-30T09:00:17.000+00:00 level=INFO msg="Discovered channel selected" channel=tanker_one`,
		`time=2026-07-30T09:00:18.000+00:00 level=INFO msg="Auto-update: newer release available" version=v0.28.0`,
		`time=2026-07-30T09:00:19.000+00:00 level=INFO msg="Settings saved to config file"`,
		`time=2026-07-30T09:00:20.000+00:00 level=INFO msg="Pruned old analytics history" rows=120`,
		`time=2026-07-30T09:00:21.000+00:00 level=INFO msg="Joined IRC chat" channel=streamer_a`,
		`time=2026-07-30T09:00:22.000+00:00 level=INFO msg="Rotating watch pair" from=streamer_a to=streamer_b`,
		`time=2026-07-30T09:00:23.000+00:00 level=INFO msg="Contributed to community goal" points=100`,
		`time=2026-07-30T09:00:24.000+00:00 level=INFO msg="Skipping bet" reason="below threshold"`,
	}
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(lines[i%len(lines)])
		fmt.Fprintf(&b, " seq=%d\n", i)
	}
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "logs", username+".log"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestF3EvidenceHarness serves the six pages with deterministic fixtures.
func TestF3EvidenceHarness(t *testing.T) {
	if os.Getenv("MINER_F3_HARNESS") != "1" {
		t.Skip("harness disabled (set MINER_F3_HARNESS=1)")
	}
	addr := os.Getenv("MINER_F3_HARNESS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8973"
	}
	const username = "devuser"

	workDir := t.TempDir()
	t.Chdir(workDir)
	f3WriteLogFixture(t, workDir, username, 500)

	dbDir := t.TempDir()
	db, err := database.Open(dbDir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc, err := analytics.NewService(db, dbDir, 0)
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	repo := svc.Repository()
	base := 20000
	for i := 0; i < 120; i++ {
		reason := []string{"WATCH", "WATCH", "WATCH", "CLAIM", "RAID", "WATCH_STREAK", "PREDICTION"}[i%7]
		delta := []int{10, 10, 10, 50, 250, 450, 1200}[i%7]
		base += delta
		if err := repo.RecordPoints("streamer_a", base, reason); err != nil {
			t.Fatal(err)
		}
	}
	_ = repo.RecordAnnotation("streamer_a", "WATCH_STREAK", "+450 - Watch Streak", "#8b7fd1")
	_ = repo.RecordAnnotation("streamer_a", "WIN", "+1200 - Prediction WIN", "#22c55e")
	for i := 0; i < 30; i++ {
		if err := repo.RecordPoints("streamer_b", 5000+i*20, "WATCH"); err != nil {
			t.Fatal(err)
		}
	}

	streamers := []*models.Streamer{
		models.NewStreamer("streamer_a", models.StreamerSettings{}),
		models.NewStreamer("streamer_b", models.StreamerSettings{}),
	}

	cfg := config.DefaultConfig()
	cfg.Streamers = []config.StreamerConfig{{Username: "streamer_a"}, {Username: "streamer_b"}}
	rt := settings.BuildRuntimeSettings(&cfg)

	srv := NewServer(config.AnalyticsSettings{Host: "127.0.0.1", Port: 0, Refresh: 5, DaysAgo: 30}, username, workDir, svc, streamers)
	srv.SetDiscordEnabled(true)
	srv.SetCampaignsProvider(&f3Campaigns{campaigns: f3BuildCampaigns()})
	srv.SetDropCatalogProvider(&f3Catalog{upcoming: f3BuildUpcoming(), past: f3BuildPast()})
	srv.SetDiscoveryProvider(f3Discovery{})
	srv.SetHealthProvider(&f3Health{settings: config.HealthSettings{CanaryEnabled: true, CanaryChannel: "canary_chan", CanaryIntervalMinutes: 120, CanaryMaxStalenessHours: 12, WatchdogEnabled: true, WatchdogStallDelayMinutes: 30, WatchdogStallConfirmations: 3, WatchdogRecoveryCooldownMinutes: 10, WatchdogAvoidTTLMinutes: 60, WatchdogRearmHours: 6}})
	srv.SetDropProgressProvider(f3Progress{})
	srv.SetPolicyProvider(&f3Policy{
		mode:  "smart",
		rules: map[string]config.DropRule{},
		decisions: []policy.Decision{
			{CampaignID: "c1", Name: "Anniversary Drops", Total: 42, Status: policy.StatusSafe,
				Feasibility: policy.Feasibility{TimeUntilEnd: 30 * time.Hour, MinutesToNextReward: 60, CanCompleteNextReward: true, CanCompleteAll: true, Status: policy.StatusSafe},
				Factors:     []policy.Factor{{Label: "ending soon", Points: 20}, {Label: "close to reward", Points: 22}}},
		},
	})
	srv.SetSettingsProvider(&f3Settings{rt: rt})
	srv.SetSettingsUpdateCallback(func(_ context.Context, s settings.RuntimeSettings) error {
		return nil
	})
	srv.SetFollowedProvider(f3Followed{})
	// Report "running" so the status overlay (the initializing/auth-required/
	// loading-streamers modal) never covers the pages during browser evidence
	// runs — this harness fakes every provider, so there's no real startup
	// sequence for it to reflect.
	srv.GetStatusBroadcaster().SetStatus(StatusRunning, "")

	mux := srv.handler()
	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = httpSrv.ListenAndServe() }()
	t.Logf("F3 harness serving on http://%s (user=%s)", addr, username)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
	case <-time.After(55 * time.Minute):
	}
	_ = httpSrv.Shutdown(context.Background())
}
