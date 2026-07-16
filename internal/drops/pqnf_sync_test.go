package drops

import (
	"fmt"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// pqnfSummaryAndDetail returns a matching dashboard summary + campaign detail
// for one active, trackable campaign.
func pqnfSummaryAndDetail(id, name string) (map[string]interface{}, map[string]interface{}) {
	summary := map[string]interface{}{
		"id":     id,
		"name":   name,
		"status": "ACTIVE",
		"game":   map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}
	detail := map[string]interface{}{
		"id":      id,
		"name":    name,
		"status":  "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)),
		"endAt":   rfc3339(nowPlusHours(48)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			activeDrop("drop-"+id, "Reward", 60),
		},
	}
	return summary, detail
}

// TestSyncCampaignsPQNFDetailsKeepsPreviousCampaigns covers the "details
// outage empties the tracked set" regression: a stale persisted-query hash
// fails GetDropCampaignDetails for EVERY campaign, and the per-campaign skip
// used to collect an empty set and swap it in, stopping drops farming for the
// whole outage. Such an operation-wide failure must abort the sync and keep
// the previously tracked campaigns, exactly like a dashboard-level error.
func TestSyncCampaignsPQNFDetailsKeepsPreviousCampaigns(t *testing.T) {
	summary, detail := pqnfSummaryAndDetail("campaign-amd", "AMD Summer Arena Drops#2")

	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"campaign-amd": detail},
	}

	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)

	// Sync 1: healthy — the campaign is tracked.
	tracker.syncCampaigns()
	if got := tracker.Campaigns(); len(got) != 1 {
		t.Fatalf("expected 1 tracked campaign after the healthy sync, got %d", len(got))
	}

	// Twitch rotates the DropCampaignDetails hash: every details call now
	// fails with ErrPersistedQueryNotFound (the dashboard listing still works).
	client.detailsErr = fmt.Errorf("%w: operation DropCampaignDetails (tried 3 client IDs)", api.ErrPersistedQueryNotFound)

	// Sync 2: must NOT swap in an empty campaign set.
	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 {
		t.Fatalf("a details outage must keep the previously tracked campaigns, got %d (empty set swapped in)", len(got))
	}
	if got[0].Name != "AMD Summer Arena Drops#2" {
		t.Errorf("unexpected surviving campaign %q", got[0].Name)
	}

	status := tracker.SyncStatus()
	if status.Runs != 2 {
		t.Errorf("expected 2 sync runs, got %d", status.Runs)
	}
	if status.LastError == "" {
		t.Error("the failed sync must be visible in SyncStatus.LastError")
	}
	if status.DashboardCampaigns != 1 {
		t.Errorf("expected dashboardCampaigns=1 recorded for the failed run (the listing succeeded), got %d", status.DashboardCampaigns)
	}
}

// TestSyncCampaignsSingleCampaignDetailErrorStillSkips pins the pre-existing
// per-campaign semantics: a details error that is NOT an operation-wide outage
// (one campaign gone or malformed) skips just that campaign, and the sync
// still completes and tracks the rest.
func TestSyncCampaignsSingleCampaignDetailErrorStillSkips(t *testing.T) {
	summaryBroken, _ := pqnfSummaryAndDetail("campaign-broken", "Broken Campaign")
	summaryOK, detailOK := pqnfSummaryAndDetail("campaign-ok", "Healthy Campaign")

	client := &fakeDropsClient{
		dashboard:     dashboardResponse(summaryBroken, summaryOK),
		inventory:     emptyInventoryResponse(),
		details:       map[string]map[string]interface{}{"campaign-ok": detailOK},
		detailErrByID: map[string]error{"campaign-broken": fmt.Errorf("campaign not found")},
	}

	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 {
		t.Fatalf("expected the healthy campaign to be tracked despite the broken one, got %d campaigns", len(got))
	}
	if got[0].Name != "Healthy Campaign" {
		t.Errorf("unexpected tracked campaign %q", got[0].Name)
	}

	status := tracker.SyncStatus()
	if status.LastError != "" {
		t.Errorf("a single-campaign detail error must not fail the sync, got LastError=%q", status.LastError)
	}
	if status.DashboardCampaigns != 2 {
		t.Errorf("expected dashboardCampaigns=2, got %d", status.DashboardCampaigns)
	}
}
