package miner

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

// snapshotDropsClient is a minimal drops.twitchClient (via the exported
// constructor) that returns one active campaign, so the tracker publishes a
// campaign we can assert the snapshot surfaces.
type snapshotDropsClient struct{}

func (snapshotDropsClient) PostGQL(op constants.GQLOperation) (map[string]interface{}, error) {
	switch op.OperationName {
	case "ViewerDropsDashboard":
		return map[string]interface{}{
			"data": map[string]interface{}{
				"currentUser": map[string]interface{}{
					"dropCampaigns": []interface{}{
						map[string]interface{}{
							"id":     "c1",
							"name":   "World of Warships Update 15.5",
							"status": "ACTIVE",
							"game":   map[string]interface{}{"id": "g1", "name": "World of Warships"},
						},
					},
				},
			},
		}, nil
	default:
		return map[string]interface{}{
			"data": map[string]interface{}{
				"currentUser": map[string]interface{}{"inventory": map[string]interface{}{}},
			},
		}, nil
	}
}

func (snapshotDropsClient) GetDropCampaignDetails(campaignID string) (map[string]interface{}, error) {
	now := time.Now()
	return map[string]interface{}{
		"id":      campaignID,
		"name":    "World of Warships Update 15.5",
		"status":  "ACTIVE",
		"startAt": now.Add(-2 * time.Hour).Format(time.RFC3339),
		"endAt":   now.Add(48 * time.Hour).Format(time.RFC3339),
		"game":    map[string]interface{}{"id": "g1", "name": "World of Warships"},
		"timeBasedDrops": []interface{}{
			map[string]interface{}{
				"id":                     "d1",
				"name":                   "Flag",
				"requiredMinutesWatched": float64(120),
				"startAt":                now.Add(-2 * time.Hour).Format(time.RFC3339),
				"endAt":                  now.Add(48 * time.Hour).Format(time.RFC3339),
			},
		},
	}, nil
}

func (snapshotDropsClient) ClaimDrop(*models.Drop) (api.ClaimStatus, error) {
	return api.ClaimStatusRejected, nil
}

// TestBuildDebugSnapshotIncludesDropsSection guards the miner wiring that makes
// the drop-campaign sync observable: BuildDebugSnapshot must surface the drops
// tracker's SyncStatus and tracked campaigns. If a future change to miner.go
// drops the tracker (or stops wiring it into the snapshot), this fails instead
// of the regression only showing up as an empty, undiagnosable Drops page in
// production.
func TestBuildDebugSnapshotIncludesDropsSection(t *testing.T) {
	tracker := drops.NewDropsTracker(snapshotDropsClient{}, nil, config.RateLimitSettings{}, nil)
	// Populate the tracker the same way the running miner does on its first
	// sync tick, without starting the background goroutine.
	tracker.SyncNow()

	m := &Miner{
		config:       &config.Config{Username: "tester"},
		dropsTracker: tracker,
	}

	snap := m.BuildDebugSnapshot()

	if snap.Drops == nil {
		t.Fatal("expected snapshot to include a drops section, got nil")
	}
	if snap.Drops.SyncRuns != 1 {
		t.Errorf("expected syncRuns=1, got %d", snap.Drops.SyncRuns)
	}
	if snap.Drops.DashboardCampaigns != 1 {
		t.Errorf("expected dashboardCampaigns=1, got %d", snap.Drops.DashboardCampaigns)
	}
	if snap.Drops.TrackedCampaigns != 1 {
		t.Errorf("expected trackedCampaigns=1, got %d", snap.Drops.TrackedCampaigns)
	}
	if len(snap.Drops.Campaigns) != 1 || snap.Drops.Campaigns[0].Name != "World of Warships Update 15.5" {
		t.Errorf("expected the tracked campaign in the snapshot, got %+v", snap.Drops.Campaigns)
	}
	if snap.Drops.Campaigns[0].GameID != "g1" {
		t.Errorf("expected the tracked campaign's opaque game ID in the snapshot, got %q", snap.Drops.Campaigns[0].GameID)
	}
}

// TestDropsTrackerSatisfiesWebProvider is a compile-time guard that the drops
// tracker still satisfies the web dashboard's CampaignsProvider contract, so
// the Drops page keeps reading live campaigns from the same object the miner
// syncs. A signature drift that broke this wiring is exactly what leaves the
// page stuck on "No active drop campaigns".
func TestDropsTrackerSatisfiesWebProvider(t *testing.T) {
	var _ web.CampaignsProvider = (*drops.DropsTracker)(nil)
}

// snapshotStreamerClient is a no-network streamer.twitchClient: it resolves a
// channel ID and no-ops the rest, so a streamer can be loaded into a Manager for
// the snapshot test without HTTP.
type snapshotStreamerClient struct{}

func (snapshotStreamerClient) GetChannelID(u string) (string, error)           { return "ch-" + u, nil }
func (snapshotStreamerClient) LoadChannelPointsContext(*models.Streamer) error { return nil }
func (snapshotStreamerClient) CheckStreamerOnline(*models.Streamer)            {}

// TestBuildDebugSnapshotIncludesBroadcastID guards that the per-streamer debug
// snapshot surfaces the Twitch broadcast ID for an online streamer, so an
// operator can tell same-broadcast slot churn apart from a new broadcast.
func TestBuildDebugSnapshotIncludesBroadcastID(t *testing.T) {
	mgr := streamer.NewManager(snapshotStreamerClient{}, models.DefaultStreamerSettings())
	if err := mgr.LoadFromConfig([]config.StreamerConfig{{Username: "cyganzor"}}, nil); err != nil {
		t.Fatalf("load streamers: %v", err)
	}
	s := mgr.Get("cyganzor")
	if s == nil {
		t.Fatal("streamer not loaded")
	}
	s.SetOnline()
	s.Stream.Update("bc-xyz-9", "Ranked", nil, nil, 1234)

	m := &Miner{config: &config.Config{Username: "tester"}, streamers: mgr}
	snap := m.BuildDebugSnapshot()

	var present, online bool
	var bid string
	for _, st := range snap.Streamers {
		if st.Username == "cyganzor" {
			present, online, bid = true, st.Online, st.BroadcastID
			break
		}
	}
	if !present {
		t.Fatalf("cyganzor missing from snapshot: %+v", snap.Streamers)
	}
	if !online {
		t.Fatal("expected cyganzor online in the snapshot")
	}
	if bid != "bc-xyz-9" {
		t.Errorf("snapshot broadcastId = %q, want bc-xyz-9", bid)
	}
}
