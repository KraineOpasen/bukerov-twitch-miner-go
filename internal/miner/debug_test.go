package miner

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
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

func (snapshotDropsClient) ClaimDrop(*models.Drop) (twitch.ClaimStatus, error) {
	return twitch.ClaimStatusRejected, nil
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
	// RED D (false half): after an authoritative listing the snapshot JSON must
	// carry the explicit dashboardListingUnavailable=false, so an operator can
	// tell "0 listed, authoritative" apart from "0 listed, listing unknown" —
	// absence of the key is not a truthful encoding of false.
	raw, err := json.Marshal(snap.Drops)
	if err != nil {
		t.Fatalf("marshal drops snapshot: %v", err)
	}
	if !strings.Contains(string(raw), `"dashboardListingUnavailable":false`) {
		t.Errorf("snapshot JSON must expose dashboardListingUnavailable=false after an authoritative listing, got: %s", raw)
	}
}

// nullListingDropsClient answers ViewerDropsDashboard with the owner-observed
// production shape — HTTP-200-equivalent, no errors, an explicit JSON null at
// data.currentUser.dropCampaigns — and an Inventory carrying one in-progress
// campaign, so the tracker completes a null-listing sync with positive
// recovery (SyncStatus.DashboardListingUnavailable=true, LastError empty).
type nullListingDropsClient struct{}

func (nullListingDropsClient) PostGQL(op constants.GQLOperation) (map[string]interface{}, error) {
	switch op.OperationName {
	case "ViewerDropsDashboard":
		return map[string]interface{}{
			"data": map[string]interface{}{
				"currentUser": map[string]interface{}{"dropCampaigns": nil},
			},
		}, nil
	default:
		return map[string]interface{}{
			"data": map[string]interface{}{
				"currentUser": map[string]interface{}{
					"inventory": map[string]interface{}{
						"dropCampaignsInProgress": []interface{}{
							map[string]interface{}{
								"id":   "c-null",
								"name": "Recovered Campaign",
								"game": map[string]interface{}{"id": "g1", "name": "World of Warships"},
								"timeBasedDrops": []interface{}{
									map[string]interface{}{
										"id":                     "d-null",
										"name":                   "Flag",
										"requiredMinutesWatched": float64(120),
										"self": map[string]interface{}{
											"currentMinutesWatched": float64(30),
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
}

func (nullListingDropsClient) GetDropCampaignDetails(string) (map[string]interface{}, error) {
	return nil, nil
}

func (nullListingDropsClient) ClaimDrop(*models.Drop) (twitch.ClaimStatus, error) {
	return twitch.ClaimStatusRejected, nil
}

// TestBuildDebugSnapshotExposesUnavailableListing is RED D: after a full sync
// that observed the explicit-null (UNKNOWN) dashboard listing and recovered a
// campaign from the inventory, the debug snapshot's drops section must expose
// dashboardListingUnavailable=true in its JSON — the operator-facing proof
// that dashboardCampaigns=0 means "listing unknown", not "Twitch reported
// zero campaigns".
func TestBuildDebugSnapshotExposesUnavailableListing(t *testing.T) {
	tracker := drops.NewDropsTracker(nullListingDropsClient{}, nil, config.RateLimitSettings{}, nil)
	tracker.SyncNow()

	m := &Miner{
		config:       &config.Config{Username: "tester"},
		dropsTracker: tracker,
	}
	snap := m.BuildDebugSnapshot()
	if snap.Drops == nil {
		t.Fatal("expected snapshot to include a drops section, got nil")
	}
	if snap.Drops.DashboardCampaigns != 0 {
		t.Errorf("expected dashboardCampaigns=0 (nothing was listed), got %d", snap.Drops.DashboardCampaigns)
	}
	if snap.Drops.TrackedCampaigns != 1 {
		t.Errorf("expected trackedCampaigns=1 (recovered from inventory), got %d", snap.Drops.TrackedCampaigns)
	}
	if snap.Drops.LastError != "" {
		t.Errorf("null listing must not be a sync error, got %q", snap.Drops.LastError)
	}
	raw, err := json.Marshal(snap.Drops)
	if err != nil {
		t.Fatalf("marshal drops snapshot: %v", err)
	}
	if !strings.Contains(string(raw), `"dashboardListingUnavailable":true`) {
		t.Errorf("snapshot JSON must expose dashboardListingUnavailable=true for an UNKNOWN listing, got: %s", raw)
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
func (snapshotStreamerClient) CheckStreamerOnline(*models.Streamer) models.StatusTransition {
	return models.StatusTransition{}
}

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
	s.SetConfirmedOnline()
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
