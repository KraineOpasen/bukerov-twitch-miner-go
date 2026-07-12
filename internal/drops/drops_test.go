package drops

import (
	"testing"
	"time"
)

func rfc3339(t time.Time) string {
	return t.Format(time.RFC3339)
}

// activeDrop builds a timeBasedDrops entry that is currently within its date
// window and unclaimed, so ClearClaimedDrops keeps it.
func activeDrop(id, name string, required float64) map[string]interface{} {
	now := time.Now()
	return map[string]interface{}{
		"id":                     id,
		"name":                   name,
		"requiredMinutesWatched": required,
		"startAt":                rfc3339(now.Add(-time.Hour)),
		"endAt":                  rfc3339(now.Add(24 * time.Hour)),
	}
}

// TestBuildTrackedCampaignUsesDetailsForDrops reproduces the production bug:
// the ViewerDropsDashboard summary carries no timeBasedDrops, so a campaign
// built from the summary alone has zero drops and would be filtered out. The
// per-campaign DropCampaignDetails response supplies the drops, so merging the
// two must yield a tracked campaign.
func TestBuildTrackedCampaignUsesDetailsForDrops(t *testing.T) {
	now := time.Now()

	// Summary as returned by ViewerDropsDashboard: metadata only, no drops.
	summary := map[string]interface{}{
		"id":      "campaign-amd",
		"name":    "AMD Summer Arena Drops#2",
		"status":  "ACTIVE",
		"startAt": rfc3339(now.Add(-2 * time.Hour)),
		"endAt":   rfc3339(now.Add(48 * time.Hour)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}

	// Details as returned by DropCampaignDetails: includes timeBasedDrops.
	detail := map[string]interface{}{
		"id":      "campaign-amd",
		"name":    "AMD Summer Arena Drops#2",
		"status":  "ACTIVE",
		"startAt": rfc3339(now.Add(-2 * time.Hour)),
		"endAt":   rfc3339(now.Add(48 * time.Hour)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			activeDrop("drop-1", "Garage Slot", 60),
		},
	}

	campaign, dropsFromDetails, skip := buildTrackedCampaign(summary, detail)
	if skip != skipNone {
		t.Fatalf("expected campaign to be tracked, got skip reason %v", skip)
	}
	if dropsFromDetails != 1 {
		t.Errorf("expected 1 drop from details, got %d", dropsFromDetails)
	}
	if len(campaign.Drops) != 1 {
		t.Errorf("expected 1 tracked drop, got %d", len(campaign.Drops))
	}
	if campaign.Name != "AMD Summer Arena Drops#2" {
		t.Errorf("unexpected campaign name %q", campaign.Name)
	}
}

// TestBuildTrackedCampaignSummaryOnlyIsSkipped shows the failure the fix
// addresses: with only the summary (no details drops), the campaign has no
// usable drops and is correctly skipped — which is exactly why the old code
// path that never fetched details produced an empty Drops page.
func TestBuildTrackedCampaignSummaryOnlyIsSkipped(t *testing.T) {
	now := time.Now()
	summary := map[string]interface{}{
		"id":      "campaign-amd",
		"name":    "AMD Summer Arena Drops#2",
		"status":  "ACTIVE",
		"startAt": rfc3339(now.Add(-2 * time.Hour)),
		"endAt":   rfc3339(now.Add(48 * time.Hour)),
	}

	// "detail" here is the summary itself, mimicking building from the
	// dashboard listing without a real details fetch.
	_, _, skip := buildTrackedCampaign(summary, summary)
	if skip != skipNoActiveDrops {
		t.Fatalf("expected skipNoActiveDrops when no drops are present, got %v", skip)
	}
}

func TestBuildTrackedCampaignOutsideDateWindow(t *testing.T) {
	now := time.Now()
	detail := map[string]interface{}{
		"id":      "campaign-old",
		"name":    "Expired Campaign",
		"status":  "ACTIVE",
		"startAt": rfc3339(now.Add(-72 * time.Hour)),
		"endAt":   rfc3339(now.Add(-24 * time.Hour)),
		"timeBasedDrops": []interface{}{
			activeDrop("drop-1", "Reward", 60),
		},
	}

	_, _, skip := buildTrackedCampaign(detail, detail)
	if skip != skipOutsideDateWindow {
		t.Fatalf("expected skipOutsideDateWindow for an ended campaign, got %v", skip)
	}
}

// TestBuildTrackedCampaignBackfillsFromSummary verifies id/name/game fall back
// to the summary when the details response omits them.
func TestBuildTrackedCampaignBackfillsFromSummary(t *testing.T) {
	now := time.Now()
	summary := map[string]interface{}{
		"id":   "campaign-amd",
		"name": "AMD Summer Arena Drops#2",
		"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}
	detail := map[string]interface{}{
		// No id/name/game here — must be backfilled from the summary.
		"status":  "ACTIVE",
		"startAt": rfc3339(now.Add(-2 * time.Hour)),
		"endAt":   rfc3339(now.Add(48 * time.Hour)),
		"timeBasedDrops": []interface{}{
			activeDrop("drop-1", "Garage Slot", 60),
		},
	}

	campaign, _, skip := buildTrackedCampaign(summary, detail)
	if skip != skipNone {
		t.Fatalf("expected campaign to be tracked, got %v", skip)
	}
	if campaign.ID != "campaign-amd" {
		t.Errorf("expected id backfilled from summary, got %q", campaign.ID)
	}
	if campaign.Name != "AMD Summer Arena Drops#2" {
		t.Errorf("expected name backfilled from summary, got %q", campaign.Name)
	}
	if campaign.Game == nil || campaign.Game.ID != "game-wot" {
		t.Errorf("expected game backfilled from summary, got %+v", campaign.Game)
	}
}
