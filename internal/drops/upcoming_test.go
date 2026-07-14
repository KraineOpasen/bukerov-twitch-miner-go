package drops

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// upcomingCampaign builds a dashboard summary + details pair for a campaign that
// has not started yet (campaign-level start in the future), with an otherwise
// valid drop, so the pipeline classifies it as upcoming rather than active.
func upcomingCampaign(id, name string) (summary, detail map[string]interface{}) {
	now := time.Now()
	summary = map[string]interface{}{
		"id": id, "name": name, "status": "UPCOMING",
		"startAt": rfc3339(now.Add(48 * time.Hour)),
		"endAt":   rfc3339(now.Add(96 * time.Hour)),
		"game":    map[string]interface{}{"id": "g-up", "name": "Upcoming Game"},
	}
	detail = map[string]interface{}{
		"id": id, "name": name, "status": "UPCOMING",
		"startAt": rfc3339(now.Add(48 * time.Hour)),
		"endAt":   rfc3339(now.Add(96 * time.Hour)),
		"game":    map[string]interface{}{"id": "g-up", "name": "Upcoming Game"},
		"timeBasedDrops": []interface{}{
			activeDrop("up-drop-1", "Future Reward", 60),
		},
	}
	return summary, detail
}

func TestSyncCapturesUpcomingButDoesNotFarmIt(t *testing.T) {
	summary, detail := upcomingCampaign("camp-future", "Future Campaign")
	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"camp-future": detail},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.syncCampaigns()

	if active := tracker.Campaigns(); len(active) != 0 {
		t.Fatalf("upcoming campaign must NOT enter the active farm set, got %d active", len(active))
	}
	up := tracker.UpcomingCampaigns()
	if len(up) != 1 || up[0].ID != "camp-future" {
		t.Fatalf("upcoming campaign must be captured for the Upcoming tab, got %+v", up)
	}
}

func TestSyncRecordsObservedCampaignsToCatalog(t *testing.T) {
	cat := newTestCatalog(t)

	// One active campaign and one upcoming; both should be catalogued.
	activeSummary := map[string]interface{}{
		"id": "cat-active", "name": "Active Now", "status": "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)), "endAt": rfc3339(nowPlusHours(48)),
		"game": map[string]interface{}{"id": "g-a", "name": "Game A"},
	}
	activeDetail := map[string]interface{}{
		"id": "cat-active", "name": "Active Now", "status": "ACTIVE",
		"startAt": rfc3339(nowMinusHours(2)), "endAt": rfc3339(nowPlusHours(48)),
		"game": map[string]interface{}{"id": "g-a", "name": "Game A"},
		"timeBasedDrops": []interface{}{
			activeDrop("a-drop-1", "Reward A", 60),
		},
	}
	upSummary, upDetail := upcomingCampaign("cat-upcoming", "Upcoming Soon")

	client := &fakeDropsClient{
		dashboard: dashboardResponse(activeSummary, upSummary),
		inventory: emptyInventoryResponse(),
		details: map[string]map[string]interface{}{
			"cat-active":   activeDetail,
			"cat-upcoming": upDetail,
		},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.SetCatalog(cat)
	tracker.syncCampaigns()

	for _, id := range []string{"cat-active", "cat-upcoming"} {
		var n int
		if err := cat.db.QueryRow("SELECT COUNT(*) FROM drop_campaigns WHERE campaign_id = ?", id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", id, err)
		}
		if n != 1 {
			t.Errorf("campaign %s must be catalogued exactly once, got %d rows", id, n)
		}
	}
}
