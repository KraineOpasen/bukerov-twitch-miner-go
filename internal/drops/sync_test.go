package drops

import (
	"fmt"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func nowMinusHours(h int) time.Time { return time.Now().Add(-time.Duration(h) * time.Hour) }
func nowPlusHours(h int) time.Time  { return time.Now().Add(time.Duration(h) * time.Hour) }

// fakeDropsClient is a scripted twitchClient for exercising the whole
// syncCampaigns pipeline without a live Twitch connection. PostGQL is
// dispatched by operation name so ViewerDropsDashboard and Inventory can be
// answered independently, and GetDropCampaignDetails is keyed by campaign ID.
type fakeDropsClient struct {
	dashboard    map[string]interface{}
	inventory    map[string]interface{}
	inventoryErr error
	details      map[string]map[string]interface{}
}

func (f *fakeDropsClient) PostGQL(op constants.GQLOperation) (map[string]interface{}, error) {
	switch op.OperationName {
	case "ViewerDropsDashboard":
		return f.dashboard, nil
	case "Inventory":
		if f.inventoryErr != nil {
			return nil, f.inventoryErr
		}
		return f.inventory, nil
	default:
		return map[string]interface{}{}, nil
	}
}

func (f *fakeDropsClient) GetDropCampaignDetails(campaignID string) (map[string]interface{}, error) {
	return f.details[campaignID], nil
}

func (f *fakeDropsClient) ClaimDrop(*models.Drop) (bool, error) { return false, nil }

// dashboardResponse wraps campaign summaries the way ViewerDropsDashboard does.
func dashboardResponse(campaigns ...map[string]interface{}) map[string]interface{} {
	list := make([]interface{}, 0, len(campaigns))
	for _, c := range campaigns {
		list = append(list, c)
	}
	return map[string]interface{}{
		"data": map[string]interface{}{
			"currentUser": map[string]interface{}{
				"dropCampaigns": list,
			},
		},
	}
}

// emptyInventoryResponse is an Inventory response with no in-progress
// campaigns and no claim history, so syncWithInventory/applyClaimHistory are
// no-ops and the test isolates the dashboard+details path.
func emptyInventoryResponse() map[string]interface{} {
	return map[string]interface{}{
		"data": map[string]interface{}{
			"currentUser": map[string]interface{}{
				"inventory": map[string]interface{}{},
			},
		},
	}
}

// inventoryWithInProgress is an Inventory response carrying the given
// dropCampaignsInProgress entries, used to exercise the inventory-recovery path.
func inventoryWithInProgress(campaigns ...map[string]interface{}) map[string]interface{} {
	list := make([]interface{}, 0, len(campaigns))
	for _, c := range campaigns {
		list = append(list, c)
	}
	return map[string]interface{}{
		"data": map[string]interface{}{
			"currentUser": map[string]interface{}{
				"inventory": map[string]interface{}{
					"dropCampaignsInProgress": list,
				},
			},
		},
	}
}

// TestSyncCampaignsRecoversInventoryCampaignAndReportsIt is the composition
// guard for the two fixes landing together: the inventory-recovery path (a
// campaign live in dropCampaignsInProgress that the dashboard/details path
// never produced) must fold into the tracked set, AND the observability layer
// must attribute it to recovery (dashboardCampaigns=0, recovered=1) rather than
// to the dashboard. This proves the two approaches reinforce each other instead
// of duplicating or masking one another.
func TestSyncCampaignsRecoversInventoryCampaignAndReportsIt(t *testing.T) {
	prog := map[string]interface{}{
		"id":   "campaign-wot",
		"name": "World of Tanks AMD Summer Arena Drops#2",
		"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			// 118/120 min: ~99% done, not yet claimable (no instance ID).
			inProgressDrop("drop-1", "Garage Slot", 120, 118, false),
		},
	}

	client := &fakeDropsClient{
		dashboard: dashboardResponse(), // dashboard yields nothing
		inventory: inventoryWithInProgress(prog),
		details:   map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 {
		t.Fatalf("expected 1 recovered campaign, got %d", len(got))
	}
	if !got[0].InInventory {
		t.Error("expected the recovered campaign to be marked InInventory")
	}
	if got[0].Name != "World of Tanks AMD Summer Arena Drops#2" {
		t.Errorf("unexpected recovered campaign name %q", got[0].Name)
	}

	status := tracker.SyncStatus()
	if status.DashboardCampaigns != 0 {
		t.Errorf("expected dashboardCampaigns=0 (dashboard was empty), got %d", status.DashboardCampaigns)
	}
	if status.RecoveredCampaigns != 1 {
		t.Errorf("expected recoveredCampaigns=1 (recovered from inventory), got %d", status.RecoveredCampaigns)
	}
	if status.TrackedCampaigns != 1 {
		t.Errorf("expected trackedCampaigns=1, got %d", status.TrackedCampaigns)
	}
}

// TestSyncCampaignsTracksActiveCampaign is the end-to-end regression guard for
// the empty-Drops-page bug: an account with a live, in-progress campaign
// (mirroring "World of Tanks AMD Summer Arena Drops#2") must end up in the
// tracker's Campaigns() pool and be reflected in SyncStatus. It exercises the
// real syncCampaigns pipeline (dashboard listing -> per-campaign details fetch
// -> merge/filter -> publish), not just the pure buildTrackedCampaign helper,
// so a future change that breaks the live path - in drops.go or in how the
// details fetch is wired - is caught here instead of silently emptying the
// page in production.
func TestSyncCampaignsTracksActiveCampaign(t *testing.T) {
	summary := map[string]interface{}{
		"id":     "campaign-amd",
		"name":   "AMD Summer Arena Drops#2",
		"status": "ACTIVE",
		"game":   map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}
	detail := map[string]interface{}{
		"id":      "campaign-amd",
		"name":    "AMD Summer Arena Drops#2",
		"status":  "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)),
		"endAt":   rfc3339(nowPlusHours(48)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			activeDrop("drop-1", "Garage Slot", 60),
		},
	}

	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"campaign-amd": detail},
	}

	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 {
		t.Fatalf("expected 1 tracked campaign, got %d", len(got))
	}
	if got[0].Name != "AMD Summer Arena Drops#2" {
		t.Errorf("unexpected campaign name %q", got[0].Name)
	}
	if len(got[0].Drops) != 1 {
		t.Errorf("expected 1 tracked drop, got %d", len(got[0].Drops))
	}

	status := tracker.SyncStatus()
	if status.Runs != 1 {
		t.Errorf("expected 1 sync run, got %d", status.Runs)
	}
	if status.DashboardCampaigns != 1 {
		t.Errorf("expected dashboardCampaigns=1, got %d", status.DashboardCampaigns)
	}
	if status.TrackedCampaigns != 1 {
		t.Errorf("expected trackedCampaigns=1, got %d", status.TrackedCampaigns)
	}
	if status.LastError != "" {
		t.Errorf("expected no sync error, got %q", status.LastError)
	}
	if status.LastSyncAt.IsZero() {
		t.Error("expected lastSyncAt to be set")
	}
}

// TestSyncProgressAdvancesTrackedProgress verifies the lightweight,
// inventory-only progress sync updates the watched-minute counters of an
// already-tracked campaign without a full re-sync -- the fix for the Drops
// page lagging up to a full CampaignSyncInterval behind Twitch's real
// progress. A campaign is first tracked at 140/240 min, Twitch then advances
// it to 180/240, and syncProgress (a single Inventory read, no
// dashboard/details fetch) must surface the new value.
func TestSyncProgressAdvancesTrackedProgress(t *testing.T) {
	summary := map[string]interface{}{
		"id":     "campaign-amd",
		"name":   "AMD Summer Arena Drops#2",
		"status": "ACTIVE",
		"game":   map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}
	detail := map[string]interface{}{
		"id":      "campaign-amd",
		"name":    "AMD Summer Arena Drops#2",
		"status":  "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)),
		"endAt":   rfc3339(nowPlusHours(48)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			activeDrop("drop-1", "Alienware Mystery Drop", 240),
		},
	}

	progressAt := func(watched float64) map[string]interface{} {
		return map[string]interface{}{
			"id":   "campaign-amd",
			"name": "AMD Summer Arena Drops#2",
			"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
			"timeBasedDrops": []interface{}{
				inProgressDrop("drop-1", "Alienware Mystery Drop", 240, watched, false),
			},
		}
	}

	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: inventoryWithInProgress(progressAt(140)),
		details:   map[string]map[string]interface{}{"campaign-amd": detail},
	}

	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 || len(got[0].Drops) != 1 {
		t.Fatalf("expected 1 tracked campaign with 1 drop, got %+v", got)
	}
	if w := got[0].Drops[0].CurrentMinutesWatched; w != 140 {
		t.Fatalf("expected initial tracked progress 140, got %d", w)
	}

	// Twitch advances the drop; the lightweight progress sync must pick it up
	// without going through the dashboard/details discovery path again.
	client.inventory = inventoryWithInProgress(progressAt(180))
	tracker.syncProgress()

	got = tracker.Campaigns()
	if len(got) != 1 || len(got[0].Drops) != 1 {
		t.Fatalf("expected the campaign to remain tracked after progress sync, got %+v", got)
	}
	if w := got[0].Drops[0].CurrentMinutesWatched; w != 180 {
		t.Errorf("expected progress advanced to 180 after syncProgress, got %d", w)
	}
}

// TestSyncProgressNoTrackedCampaignsIsSafe guards that a progress sync run
// before the full sync has populated any campaigns is a harmless no-op (it must
// not panic or fabricate campaigns from the inventory -- discovery stays with
// the full sync).
func TestSyncProgressNoTrackedCampaignsIsSafe(t *testing.T) {
	client := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: inventoryWithInProgress(map[string]interface{}{
			"id":   "campaign-amd",
			"name": "AMD Summer Arena Drops#2",
			"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
			"timeBasedDrops": []interface{}{
				inProgressDrop("drop-1", "Alienware Mystery Drop", 240, 180, false),
			},
		}),
		details: map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)

	tracker.syncProgress()

	if got := len(tracker.Campaigns()); got != 0 {
		t.Fatalf("expected progress sync to add no campaigns, got %d", got)
	}
}

// TestSyncProgressRecordsObservations pins the Stage 3 observation contract the
// progress watchdog builds on: a completed inventory read counts as an
// observation even when nothing moved ("checked and unchanged" is exactly the
// stall evidence), an inventory failure is recorded instead of being swallowed
// silently, and the full sync never stamps the progress-observation fields.
func TestSyncProgressRecordsObservations(t *testing.T) {
	summary := map[string]interface{}{
		"id":     "campaign-amd",
		"name":   "AMD Summer Arena Drops#2",
		"status": "ACTIVE",
		"game":   map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}
	detail := map[string]interface{}{
		"id":      "campaign-amd",
		"name":    "AMD Summer Arena Drops#2",
		"status":  "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)),
		"endAt":   rfc3339(nowPlusHours(48)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			activeDrop("drop-1", "Alienware Mystery Drop", 240),
		},
	}
	prog := map[string]interface{}{
		"id":   "campaign-amd",
		"name": "AMD Summer Arena Drops#2",
		"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			inProgressDrop("drop-1", "Alienware Mystery Drop", 240, 140, false),
		},
	}

	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: inventoryWithInProgress(prog),
		details:   map[string]map[string]interface{}{"campaign-amd": detail},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	if s := tracker.SyncStatus(); s.ProgressRuns != 0 || !s.ProgressLastSyncAt.IsZero() {
		t.Fatalf("full sync must not stamp progress observations, got %+v", s)
	}

	// Unchanged progress: still a completed observation.
	tracker.syncProgress()
	s := tracker.SyncStatus()
	if s.ProgressRuns != 1 || s.ProgressLastSyncAt.IsZero() || s.ProgressLastError != "" {
		t.Fatalf("expected one clean observation after an unchanged progress sync, got %+v", s)
	}

	// Inventory outage: recorded, not swallowed.
	client.inventoryErr = fmt.Errorf("inventory 502")
	tracker.syncProgress()
	s = tracker.SyncStatus()
	if s.ProgressRuns != 2 || s.ProgressLastError == "" {
		t.Fatalf("expected the inventory failure to be recorded, got %+v", s)
	}

	// Recovery: the next successful read clears the error.
	client.inventoryErr = nil
	tracker.syncProgress()
	s = tracker.SyncStatus()
	if s.ProgressRuns != 3 || s.ProgressLastError != "" {
		t.Fatalf("expected the observation error to clear on recovery, got %+v", s)
	}
}

// TestSyncCampaignsDistinguishesEmptyFromFiltered verifies SyncStatus makes the
// two silent-failure modes distinguishable: Twitch returning no active
// campaigns at all vs returning campaigns that all get filtered out (here the
// details response carries no drops, so the campaign is skipped). Before this,
// both looked identical to an operator - an empty page and no INFO logs.
func TestSyncCampaignsDistinguishesEmptyFromFiltered(t *testing.T) {
	// Dashboard advertises a campaign, but its details have no drops, so it is
	// filtered out: dashboardCount stays 1 while tracked drops to 0.
	summary := map[string]interface{}{
		"id":     "campaign-empty",
		"name":   "Campaign Without Drops",
		"status": "ACTIVE",
		"game":   map[string]interface{}{"id": "game-x", "name": "Game X"},
	}
	detail := map[string]interface{}{
		"id":      "campaign-empty",
		"name":    "Campaign Without Drops",
		"status":  "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)),
		"endAt":   rfc3339(nowPlusHours(48)),
		"game":    map[string]interface{}{"id": "game-x", "name": "Game X"},
		// no timeBasedDrops
	}

	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"campaign-empty": detail},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	if got := len(tracker.Campaigns()); got != 0 {
		t.Fatalf("expected 0 tracked campaigns, got %d", got)
	}
	status := tracker.SyncStatus()
	if status.DashboardCampaigns != 1 {
		t.Errorf("expected dashboardCampaigns=1 (Twitch had a campaign), got %d", status.DashboardCampaigns)
	}
	if status.TrackedCampaigns != 0 {
		t.Errorf("expected trackedCampaigns=0 (all filtered), got %d", status.TrackedCampaigns)
	}

	// The genuinely-empty case: dashboard returns nothing.
	client2 := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{},
	}
	tracker2 := NewDropsTracker(client2, nil, config.RateLimitSettings{}, nil)
	tracker2.syncCampaigns()
	if status := tracker2.SyncStatus(); status.DashboardCampaigns != 0 {
		t.Errorf("expected dashboardCampaigns=0 for an account with no campaigns, got %d", status.DashboardCampaigns)
	}
}
