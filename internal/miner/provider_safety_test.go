package miner

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/resources"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

// TestProvidersSafeAfterTeardown is the F12/I29 provider call-safety table
// (contract §11 item 7): after a miner's Run has returned (teardown
// complete), every F12 provider binding's underlying symbol must still be
// callable without panicking, returning a sane (error/stale/empty) result
// instead. It runs the REAL startMining-adjacent wiring (setupComponents is
// never stubbed, so the real watcher/dropsTracker/discovery/healthCenter/
// canary/progressWatchdog/notifications manager all get constructed exactly
// as production does) through a normal Run/cancel/teardown cycle, then calls
// every provider method the web layer binds to, each guarded so a panic
// fails with the provider's name rather than aborting the whole test.
//
// Scope, disclosed rather than faked (no test seam in internal/miner can
// redirect *twitch.TwitchClient's real network transport — unlike
// internal/twitch's OWN tests, which use its unexported setGQLEndpoint):
//
//   - FollowedChannels (web.FollowedProvider) calls client.GetFollowedChannels
//     UNCONDITIONALLY once m.client is non-nil (no tracked-streamer
//     short-circuit to route around, unlike ListCustomRewards/
//     RedeemCustomReward below) — exercising it here would fire a genuine
//     outbound call to Twitch's real API. Its nil-client branch (client ==
//     nil -> (nil, false, nil), no panic) IS covered by
//     TestFollowedChannelsNilClientIsSafe below, using a Miner that was
//     never authenticated.
//   - GameIDResolver.GetGameIdentity (bound to *twitch.TwitchClient directly,
//     not a Miner method) has no context.Context parameter at all — the
//     orchestrator's "with the cancelled ctx" framing does not literally
//     apply to this signature, and there is no ctx to cancel that would
//     abort the underlying request. Its one network-free path (a
//     blank/whitespace name short-circuits before any request:
//     strings.TrimSpace(name) == "" -> zero GameIdentity, nil error) is
//     exercised directly against m.client below as the largest safely
//     reachable slice; a non-blank name would reach the real network with
//     no override seam available from this package.
//   - RunCanaryNow (web.HealthProvider) triggers a real watch-transport probe
//     via the canary's own minute-sender client with no ctx/seam to redirect
//     it either, AND (since this test's startMiningFn stub deliberately does
//     NOT call canary.Start, for the same network-safety reason) calling it
//     risks firing an orphaned background goroutine outliving the test. Not
//     exercised; every other HealthProvider method (HealthSnapshot,
//     CurrentHealthSettings, ApplyHealthSettings) is.
//   - dropsTracker.RequestManualSync (web.CampaignsProvider, bound directly to
//     m.dropsTracker, not a Miner method) forces an immediate real campaign
//     sync. Not exercised for the same reason; the Miner-method drop-catalog
//     providers (UpcomingCampaigns/RelevantUpcomingCampaigns/
//     CampaignSyncStatus/PastCampaigns), which only ever read already-synced
//     in-memory/db state, ARE exercised.
//
// Every other F12 provider listed in the contract is exercised for real
// below, either by construction (an empty/never-added streamer roster makes
// ListCustomRewards/RedeemCustomReward/GetAutoRedeem/SetAutoRedeem/
// PlaceManualBet/SetAutoBetSkip fail closed on their OWN "not tracked/not
// found" guard before ever reaching a client) or because the method is
// itself a pure in-memory/db read (WatchSlots, LivePredictions, DropProgress,
// PolicySnapshot, CurrentCampaignPolicy, HealthSnapshot, BuildDebugSnapshot,
// resourceSampler.Latest) or a persist-only local write (ApplyCampaignPolicy,
// SetDropRule, ApplyHealthSettings) or fails closed before touching anything
// (ApplySettings, via beginApply once the miner is draining).
func TestProvidersSafeAfterTeardown(t *testing.T) {
	m, db := newStartupCleanupMiner(t)
	stubAuthenticate(m)
	stubLoadStreamers(m)
	m.subscribeTopicsFn = func() error { return nil } // avoid a real pubsub Submit (network)

	// A real, externally-owned, STARTED web server (mirrors
	// TestRunEarlyExitLeavesExternalWebAndAnalyticsAlone) so m.resourceSampler
	// gets constructed exactly like production. Loopback-only bind; no
	// external network involved.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	m.config.EnableAnalytics = true
	m.config.Analytics.Host = "127.0.0.1"
	m.config.Analytics.Port = port

	svc, err := analytics.NewService(db, m.config.StorageKey(), m.config.Analytics.RetentionDays)
	if err != nil {
		t.Fatalf("analytics.NewService: %v", err)
	}
	m.SetAnalyticsService(svc)

	ws := web.NewServerEarly(m.config.Analytics, m.config.Username, m.config.StorageKey(), svc)
	if ws == nil {
		t.Fatal("web.NewServerEarly returned nil")
	}
	if err := ws.Start(); err != nil {
		t.Fatalf("web server Start: %v", err)
	}
	defer ws.Stop()
	m.SetWebServer(ws)

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	dialDeadline := time.Now().Add(5 * time.Second)
	for {
		conn, derr := net.DialTimeout("tcp", addr, time.Second)
		if derr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(dialDeadline) {
			t.Fatalf("injected web server not reachable before Run: %v", derr)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Replace startMining's loop-starting section with the network-free
	// SUBSET of it (resourceSampler wiring only): the real
	// watcher/dropsTracker/discovery/canary/progressWatchdog background
	// loops are never Start()'d here because doing so would fire real
	// outbound calls to Twitch's production API on their own tick/interval
	// with no test seam available in this package to redirect them (see the
	// doc comment above). setupComponents (which Run already called before
	// this point, unstubbed) has still built every one of those components
	// for real, so their PROVIDER methods below read the exact same
	// never-ticked-but-fully-constructed state that a real teardown would
	// leave a genuinely started-then-stopped component in.
	started := make(chan struct{})
	m.startMiningFn = func(ctx context.Context) {
		if m.webServer != nil {
			m.resourceSampler = resources.New()
			m.webServer.SetResourceSnapshotProvider(m.resourceSampler.Latest)
			m.startLoop(ctx, m.resourceSampler.Run)
		}
		close(started)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- m.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("startup did not complete")
	}
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run after normal cancellation = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}
	requireExternalDBAlive(t, db)

	// --- F12 provider call-safety table ---
	cases := []struct {
		name string
		call func(t *testing.T)
	}{
		{"GetRuntimeSettings", func(*testing.T) { m.GetRuntimeSettings() }},
		{"GetDefaultSettings", func(*testing.T) { m.GetDefaultSettings() }},
		{"ApplySettings", func(t *testing.T) {
			cur := m.GetRuntimeSettings()
			if err := m.ApplySettings(context.Background(), cur); !errors.Is(err, ErrShuttingDown) {
				t.Errorf("ApplySettings after teardown = %v, want ErrShuttingDown (fail-closed via beginApply)", err)
			}
		}},
		{"GetNextStreamCheck", func(*testing.T) { m.GetNextStreamCheck() }},
		{"WatchSlots", func(*testing.T) { m.WatchSlots() }},
		{"LivePredictions", func(*testing.T) { m.LivePredictions() }},
		{"PlaceManualBet", func(t *testing.T) {
			if _, err := m.PlaceManualBet("nonexistent-event", "outcome-1", 10); err == nil {
				t.Error("PlaceManualBet on an unknown event should return an error, not nil")
			}
		}},
		{"SetAutoBetSkip", func(t *testing.T) {
			if err := m.SetAutoBetSkip("nonexistent-event", true); err == nil {
				t.Error("SetAutoBetSkip on an unknown event should return an error, not nil")
			}
		}},
		{"ListCustomRewards", func(t *testing.T) {
			if _, err := m.ListCustomRewards("not-a-tracked-streamer"); err == nil {
				t.Error("ListCustomRewards for an untracked streamer should return an error, not nil")
			}
		}},
		{"RedeemCustomReward", func(t *testing.T) {
			if err := m.RedeemCustomReward("not-a-tracked-streamer", "reward-1", ""); err == nil {
				t.Error("RedeemCustomReward for an untracked streamer should return an error, not nil")
			}
		}},
		{"GetAutoRedeem", func(*testing.T) { m.GetAutoRedeem("not-a-tracked-streamer") }},
		{"SetAutoRedeem", func(t *testing.T) {
			if err := m.SetAutoRedeem("not-a-tracked-streamer", config.AutoRedeemConfig{}); err == nil {
				t.Error("SetAutoRedeem for an untracked streamer should return an error, not nil")
			}
		}},
		{"TrackedUsernames", func(*testing.T) { m.TrackedUsernames() }},
		{"UpcomingCampaigns", func(*testing.T) { m.UpcomingCampaigns() }},
		{"RelevantUpcomingCampaigns", func(*testing.T) { m.RelevantUpcomingCampaigns() }},
		{"CampaignSyncStatus", func(*testing.T) { m.CampaignSyncStatus() }},
		{"PastCampaigns", func(*testing.T) { _, _ = m.PastCampaigns() }},
		{"DropProgress", func(*testing.T) { m.DropProgress() }},
		{"PolicySnapshot", func(*testing.T) { m.PolicySnapshot() }},
		{"CurrentCampaignPolicy", func(*testing.T) { m.CurrentCampaignPolicy() }},
		{"ApplyCampaignPolicy", func(*testing.T) { m.ApplyCampaignPolicy("balanced") }},
		{"SetDropRule", func(*testing.T) { m.SetDropRule("some-reward-key", config.DropRule{}) }},
		{"HealthSnapshot", func(*testing.T) { m.HealthSnapshot() }},
		{"CurrentHealthSettings", func(*testing.T) { m.CurrentHealthSettings() }},
		{"ApplyHealthSettings", func(t *testing.T) {
			// newStartupCleanupMiner constructs this Miner with configPath ==
			// "" (see startup_cleanup_test.go), which ApplyHealthSettings
			// documents as an unconditional no-op success — deterministically
			// nil here, not just "some sane result".
			if err := m.ApplyHealthSettings(m.CurrentHealthSettings()); err != nil {
				t.Errorf("ApplyHealthSettings after teardown = %v, want nil (configPath is empty, a documented no-op-success case)", err)
			}
		}},
		{"GetGameIdentity (blank name, no network)", func(t *testing.T) {
			id, err := m.client.GetGameIdentity("   ")
			if err != nil {
				t.Errorf("GetGameIdentity(blank) = %v, want nil error (short-circuits before any request)", err)
			}
			if id.ID != "" || id.Slug != "" {
				t.Errorf("GetGameIdentity(blank) = %+v, want zero value", id)
			}
		}},
		{"BuildDebugSnapshot", func(*testing.T) { m.BuildDebugSnapshot() }},
		{"NotificationManager", func(*testing.T) {
			// Stopped (or nil): calling into it must be a safe no-op — M4's
			// Stop closes dispatch admission before this ever runs.
			if mgr := m.NotificationManager(); mgr != nil {
				mgr.NotifyPointsReached("not-a-tracked-streamer", 1)
			}
		}},
		{"resourceSampler.Latest", func(t *testing.T) {
			if m.resourceSampler == nil {
				t.Fatal("resourceSampler was not constructed despite a live webServer")
			}
			m.resourceSampler.Latest()
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("provider %q panicked after teardown: %v", tc.name, r)
				}
			}()
			tc.call(t)
		})
	}
}

// TestFollowedChannelsNilClientIsSafe covers FollowedChannels' nil-client
// guard (see TestProvidersSafeAfterTeardown's scope note): a Miner that was
// never authenticated (m.client == nil, e.g. a startup failure before
// authenticate completed) must return a safe, empty, non-error result rather
// than panicking — this is the one FollowedChannels branch reachable without
// a real network call.
func TestFollowedChannelsNilClientIsSafe(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Username = "followed-nil-client"
	m := New(&cfg, "")

	channels, truncated, err := m.FollowedChannels()
	if err != nil {
		t.Errorf("FollowedChannels with a nil client = %v, want nil error", err)
	}
	if channels != nil || truncated {
		t.Errorf("FollowedChannels with a nil client = (%v, %v), want (nil, false)", channels, truncated)
	}
}
