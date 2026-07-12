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

// inProgressDrop builds an inventory dropCampaignsInProgress timeBasedDrops
// entry with `self` watch progress, as the Inventory query returns it.
func inProgressDrop(id, name string, required, watched float64, claimed bool) map[string]interface{} {
	return map[string]interface{}{
		"id":                     id,
		"name":                   name,
		"requiredMinutesWatched": required,
		"self": map[string]interface{}{
			"currentMinutesWatched": watched,
			"isClaimed":             claimed,
		},
	}
}

// TestBuildInProgressCampaignRecoversFromInventory reproduces the regression:
// a campaign Twitch is actively crediting (present in the inventory's
// dropCampaignsInProgress with live progress) must be tracked even when the
// entry carries no per-drop date window — which the dashboard/details path
// would filter out, leaving the Drops page empty while drops keep filling up.
func TestBuildInProgressCampaignRecoversFromInventory(t *testing.T) {
	d := &DropsTracker{}
	prog := map[string]interface{}{
		"id":   "campaign-wot",
		"name": "World of Tanks Drops",
		"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			// 118/120 minutes: ~99% done, not yet claimable (no instance ID).
			inProgressDrop("drop-1", "Garage Slot", 120, 118, false),
		},
	}

	campaign := d.buildInProgressCampaign(prog)
	if campaign == nil {
		t.Fatal("expected a recovered campaign, got nil")
	}
	if campaign.ID != "campaign-wot" {
		t.Errorf("unexpected campaign id %q", campaign.ID)
	}
	if !campaign.InInventory {
		t.Error("expected campaign marked as in inventory")
	}
	if campaign.Game == nil || campaign.Game.Name != "World of Tanks" {
		t.Errorf("expected game populated, got %+v", campaign.Game)
	}
	if len(campaign.Drops) != 1 {
		t.Fatalf("expected 1 tracked drop, got %d", len(campaign.Drops))
	}
	if got := campaign.Drops[0].CurrentMinutesWatched; got != 118 {
		t.Errorf("expected watch progress applied from self, got %d", got)
	}
}

// TestBuildInProgressCampaignDropsClaimedRewards shows an already-claimed drop
// is not resurfaced, so a fully-claimed campaign contributes nothing.
func TestBuildInProgressCampaignDropsClaimedRewards(t *testing.T) {
	d := &DropsTracker{}
	prog := map[string]interface{}{
		"id":   "campaign-done",
		"name": "Finished Campaign",
		"game": map[string]interface{}{"id": "game-x", "name": "Game X"},
		"timeBasedDrops": []interface{}{
			inProgressDrop("drop-1", "Reward", 60, 60, true),
		},
	}

	campaign := d.buildInProgressCampaign(prog)
	if campaign == nil {
		t.Fatal("expected a campaign, got nil")
	}
	if len(campaign.Drops) != 0 {
		t.Errorf("expected claimed drop to be dropped, got %d drops", len(campaign.Drops))
	}
}

func TestBuildInProgressCampaignNoIDReturnsNil(t *testing.T) {
	d := &DropsTracker{}
	if got := d.buildInProgressCampaign(map[string]interface{}{"name": "no id"}); got != nil {
		t.Errorf("expected nil for entry without a campaign id, got %+v", got)
	}
}

// TestBuildTrackedCampaignBackfillsDatesFromSummary verifies that when the
// DropCampaignDetails response omits the campaign-level date window, the dates
// (and thus DateMatch) are backfilled from the ViewerDropsDashboard summary,
// so an in-window campaign is tracked instead of being silently skipped as
// "outside its date window".
func TestBuildTrackedCampaignBackfillsDatesFromSummary(t *testing.T) {
	now := time.Now()
	summary := map[string]interface{}{
		"id":      "campaign-amd",
		"name":    "AMD Summer Arena Drops#2",
		"status":  "ACTIVE",
		"startAt": rfc3339(now.Add(-2 * time.Hour)),
		"endAt":   rfc3339(now.Add(48 * time.Hour)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}
	// Details carry drops but no campaign-level startAt/endAt.
	detail := map[string]interface{}{
		"id":     "campaign-amd",
		"name":   "AMD Summer Arena Drops#2",
		"status": "ACTIVE",
		"game":   map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			activeDrop("drop-1", "Garage Slot", 60),
		},
	}

	campaign, _, skip := buildTrackedCampaign(summary, detail)
	if skip != skipNone {
		t.Fatalf("expected campaign to be tracked after date backfill, got skip reason %v", skip)
	}
	if campaign.StartAt.IsZero() || campaign.EndAt.IsZero() {
		t.Errorf("expected dates backfilled from summary, got start=%v end=%v", campaign.StartAt, campaign.EndAt)
	}
	if !campaign.DateMatch {
		t.Error("expected DateMatch true after backfilling an in-window date range")
	}
}

// TestBuildTrackedCampaignDetailsExpiredNotOverridden ensures the date backfill
// never resurrects a campaign the details response genuinely reports as expired:
// when details carry their own (out-of-window) dates, those win over the
// summary.
func TestBuildTrackedCampaignDetailsExpiredNotOverridden(t *testing.T) {
	now := time.Now()
	summary := map[string]interface{}{
		"id":      "campaign-amd",
		"name":    "AMD Summer Arena Drops#2",
		"status":  "ACTIVE",
		"startAt": rfc3339(now.Add(-2 * time.Hour)),
		"endAt":   rfc3339(now.Add(48 * time.Hour)),
	}
	detail := map[string]interface{}{
		"id":      "campaign-amd",
		"name":    "AMD Summer Arena Drops#2",
		"status":  "ACTIVE",
		"startAt": rfc3339(now.Add(-72 * time.Hour)),
		"endAt":   rfc3339(now.Add(-24 * time.Hour)),
		"timeBasedDrops": []interface{}{
			activeDrop("drop-1", "Garage Slot", 60),
		},
	}

	_, _, skip := buildTrackedCampaign(summary, detail)
	if skip != skipOutsideDateWindow {
		t.Fatalf("expected details' expired window to win, got skip reason %v", skip)
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
