package drops

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
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

// TestBuildTrackedCampaignUnknownEndIsNotOutsideDateWindow is the behavioral
// RED for a dashboard/details campaign whose authoritative current response
// says ACTIVE but omits endAt. Missing deadline evidence must not be converted
// into the positive claim that the campaign is outside its date window.
func TestBuildTrackedCampaignUnknownEndIsNotOutsideDateWindow(t *testing.T) {
	now := time.Now()
	summary := map[string]interface{}{
		"id":      "campaign-unknown-end",
		"name":    "Current Campaign With Unknown End",
		"status":  "ACTIVE",
		"startAt": rfc3339(now.Add(-2 * time.Hour)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}
	detail := map[string]interface{}{
		"id":      "campaign-unknown-end",
		"name":    "Current Campaign With Unknown End",
		"status":  "ACTIVE",
		"startAt": rfc3339(now.Add(-2 * time.Hour)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			activeDrop("drop-unknown-end", "Reward", 60),
		},
	}

	campaign, _, skip := buildTrackedCampaign(summary, detail)
	if skip != skipNone {
		t.Fatalf("ACTIVE campaign with unknown EndAt was discarded: skip=%v start=%v end=%v status=%q", skip, campaign.StartAt, campaign.EndAt, campaign.Status)
	}
	if !campaign.EndAt.IsZero() {
		t.Fatalf("unknown EndAt must remain zero, got %v", campaign.EndAt)
	}
}

// TestSyncCampaignsTracksCurrentUnknownEndWithoutInventory is the production-
// pipeline RED: inventory recovery cannot save a current dashboard campaign
// that has not appeared in dropCampaignsInProgress yet, so the full sync must
// preserve it from ViewerDropsDashboard + DropCampaignDetails on current-state
// evidence alone.
func TestSyncCampaignsTracksCurrentUnknownEndWithoutInventory(t *testing.T) {
	now := time.Now()
	summary := map[string]interface{}{
		"id":      "campaign-unknown-end",
		"name":    "Current Campaign With Unknown End",
		"status":  "ACTIVE",
		"startAt": rfc3339(now.Add(-2 * time.Hour)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}
	detail := map[string]interface{}{
		"id":      "campaign-unknown-end",
		"name":    "Current Campaign With Unknown End",
		"status":  "ACTIVE",
		"startAt": rfc3339(now.Add(-2 * time.Hour)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			activeDrop("drop-unknown-end", "Reward", 60),
		},
	}
	client := &fakeDropsClient{
		dashboard: dashboardResponse(summary),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"campaign-unknown-end": detail},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)

	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 {
		t.Fatalf("current unknown-deadline campaign vanished before inventory recovery: tracked=%d campaigns=%+v", len(got), got)
	}
	if got[0].ID != "campaign-unknown-end" || !got[0].EndAt.IsZero() {
		t.Fatalf("unexpected tracked campaign: %+v", got[0])
	}
}

func TestBuildTrackedCampaignDateEvidenceMatrix(t *testing.T) {
	now := time.Now()
	pastStart := rfc3339(now.Add(-2 * time.Hour))
	pastEnd := rfc3339(now.Add(-time.Hour))
	futureStart := rfc3339(now.Add(2 * time.Hour))
	futureEnd := rfc3339(now.Add(24 * time.Hour))

	type fixture struct {
		status         string
		startAt        interface{}
		endAt          interface{}
		windowlessDrop bool
		wantSkip       campaignSkipReason
		wantDateMatch  bool
		wantStartZero  bool
		wantEndZero    bool
	}
	cases := map[string]fixture{
		"known active window": {
			status: "ACTIVE", startAt: pastStart, endAt: futureEnd,
			wantSkip: skipNone, wantDateMatch: true,
		},
		"known expired window": {
			status: "ACTIVE", startAt: pastStart, endAt: pastEnd,
			wantSkip: skipOutsideDateWindow,
		},
		"known future start and known end": {
			status: "ACTIVE", startAt: futureStart, endAt: futureEnd,
			wantSkip: skipOutsideDateWindow,
		},
		"known future start and unknown end": {
			status: "ACTIVE", startAt: futureStart,
			wantSkip: skipOutsideDateWindow, wantEndZero: true,
		},
		"started with unknown end and current status": {
			status: "ACTIVE", startAt: pastStart,
			wantSkip: skipNone, wantEndZero: true,
		},
		"both dates unknown with current status": {
			status:   "ACTIVE",
			wantSkip: skipNone, wantStartZero: true, wantEndZero: true,
		},
		"both campaign and drop windows unknown with current status": {
			status: "ACTIVE", windowlessDrop: true,
			wantSkip: skipNone, wantStartZero: true, wantEndZero: true,
		},
		"both dates unknown without current evidence": {
			wantSkip: skipCurrentStateUnknown, wantStartZero: true, wantEndZero: true,
		},
		"unknown start and known future end with current status": {
			status: "ACTIVE", endAt: futureEnd,
			wantSkip: skipNone, wantStartZero: true,
		},
		"unknown start and known future end without current evidence": {
			endAt:    futureEnd,
			wantSkip: skipCurrentStateUnknown, wantStartZero: true,
		},
		"malformed end remains unknown": {
			status: "ACTIVE", startAt: pastStart, endAt: "not-rfc3339",
			wantSkip: skipNone, wantEndZero: true,
		},
		"explicit expired status with unknown dates": {
			status:   "EXPIRED",
			wantSkip: skipOutsideDateWindow, wantStartZero: true, wantEndZero: true,
		},
		"known past end overrides current status": {
			status: "ACTIVE", endAt: pastEnd,
			wantSkip: skipOutsideDateWindow, wantStartZero: true,
		},
		"unknown dates and non-active status lack timing proof": {
			status:   "UPCOMING",
			wantSkip: skipCurrentStateUnknown, wantStartZero: true, wantEndZero: true,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			drop := map[string]interface{}{
				"id": "drop-" + name, "name": "Reward", "requiredMinutesWatched": 60,
			}
			if !tc.windowlessDrop {
				drop = activeDrop("drop-"+name, "Reward", 60)
			}
			detail := map[string]interface{}{
				"id":             "campaign-" + name,
				"name":           name,
				"game":           map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
				"timeBasedDrops": []interface{}{drop},
			}
			if tc.status != "" {
				detail["status"] = tc.status
			}
			if tc.startAt != nil {
				detail["startAt"] = tc.startAt
			}
			if tc.endAt != nil {
				detail["endAt"] = tc.endAt
			}

			campaign, _, skip := buildTrackedCampaign(detail, detail)
			if skip != tc.wantSkip {
				t.Fatalf("skip=%v, want %v (status=%q start=%v end=%v)", skip, tc.wantSkip, campaign.Status, campaign.StartAt, campaign.EndAt)
			}
			if campaign.DateMatch != tc.wantDateMatch {
				t.Errorf("DateMatch=%t, want %t", campaign.DateMatch, tc.wantDateMatch)
			}
			if campaign.StartAt.IsZero() != tc.wantStartZero {
				t.Errorf("StartAt.IsZero=%t, want %t (StartAt=%v)", campaign.StartAt.IsZero(), tc.wantStartZero, campaign.StartAt)
			}
			if campaign.EndAt.IsZero() != tc.wantEndZero {
				t.Errorf("EndAt.IsZero=%t, want %t (EndAt=%v)", campaign.EndAt.IsZero(), tc.wantEndZero, campaign.EndAt)
			}
			if skip == skipNone && (campaign.StartAt.IsZero() || campaign.EndAt.IsZero()) && campaign.DateMatch {
				t.Error("an incomplete date window must not masquerade as DateMatch=true")
			}
		})
	}
}

func TestBuildTrackedCampaignBackfillsCurrentStatusFromSummary(t *testing.T) {
	summary := map[string]interface{}{
		"id": "campaign-summary-status", "name": "Summary Status", "status": "ACTIVE",
		"startAt": rfc3339(time.Now().Add(-2 * time.Hour)),
	}
	detail := map[string]interface{}{
		"id": "campaign-summary-status", "name": "Summary Status",
		"startAt": rfc3339(time.Now().Add(-2 * time.Hour)),
		"timeBasedDrops": []interface{}{
			activeDrop("drop-summary-status", "Reward", 60),
		},
	}

	campaign, _, skip := buildTrackedCampaign(summary, detail)
	if skip != skipNone || campaign.Status != "ACTIVE" {
		t.Fatalf("summary ACTIVE evidence was not retained: skip=%v status=%q", skip, campaign.Status)
	}
	if !campaign.EndAt.IsZero() || campaign.DateMatch {
		t.Fatalf("status backfill must not manufacture date knowledge: end=%v DateMatch=%t", campaign.EndAt, campaign.DateMatch)
	}
}

func TestSyncCampaignsUnknownDateClassificationIsDeterministic(t *testing.T) {
	now := time.Now()
	makePair := func(id, status string, startAt, endAt interface{}) (map[string]interface{}, map[string]interface{}) {
		summary := map[string]interface{}{
			"id": id, "name": id, "status": status,
			"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		}
		detail := map[string]interface{}{
			"id": id, "name": id, "status": status,
			"game":           map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
			"timeBasedDrops": []interface{}{activeDrop("drop-"+id, "Reward", 60)},
		}
		if startAt != nil {
			summary["startAt"], detail["startAt"] = startAt, startAt
		}
		if endAt != nil {
			summary["endAt"], detail["endAt"] = endAt, endAt
		}
		return summary, detail
	}
	startedSummary, startedDetail := makePair("unknown-started", "ACTIVE", rfc3339(now.Add(-2*time.Hour)), nil)
	undatedSummary, undatedDetail := makePair("unknown-both", "ACTIVE", nil, nil)
	futureSummary, futureDetail := makePair("future-unknown-end", "UPCOMING", rfc3339(now.Add(2*time.Hour)), nil)
	expiredSummary, expiredDetail := makePair("expired-known", "EXPIRED", rfc3339(now.Add(-4*time.Hour)), rfc3339(now.Add(-time.Hour)))
	unprovenSummary, unprovenDetail := makePair("unknown-unproven", "", nil, nil)
	details := map[string]map[string]interface{}{
		"unknown-started":    startedDetail,
		"unknown-both":       undatedDetail,
		"future-unknown-end": futureDetail,
		"expired-known":      expiredDetail,
		"unknown-unproven":   unprovenDetail,
	}
	orders := [][]map[string]interface{}{
		{startedSummary, undatedSummary, futureSummary, expiredSummary, unprovenSummary},
		{unprovenSummary, expiredSummary, futureSummary, undatedSummary, startedSummary},
	}

	for i, order := range orders {
		client := &fakeDropsClient{
			dashboard: dashboardResponse(order...),
			inventory: emptyInventoryResponse(),
			details:   details,
		}
		tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
		tracker.syncCampaigns()

		active := keptIDs(tracker.Campaigns())
		if len(active) != 2 || !active["unknown-started"] || !active["unknown-both"] {
			t.Fatalf("order %d active set=%v, want exactly the two current UNKNOWN campaigns", i, active)
		}
		for _, id := range []string{"future-unknown-end", "expired-known", "unknown-unproven"} {
			if active[id] {
				t.Fatalf("order %d non-active campaign %q leaked into the active set: %v", i, id, active)
			}
		}
	}
}

func TestCampaignStartBoundaryIsInclusive(t *testing.T) {
	startAt := time.Now().UTC().Truncate(time.Second)
	summary := map[string]interface{}{
		"id": "equal", "name": "Equal", "status": "ACTIVE",
		"startAt": rfc3339(startAt), "endAt": rfc3339(startAt.Add(time.Hour)),
		"game": map[string]interface{}{"id": "game", "name": "Game"},
	}
	detail := map[string]interface{}{
		"id": "equal", "name": "Equal", "status": "ACTIVE",
		"startAt": rfc3339(startAt), "endAt": rfc3339(startAt.Add(time.Hour)),
		"game":           map[string]interface{}{"id": "game", "name": "Game"},
		"timeBasedDrops": []interface{}{activeDrop("equal-drop", "Reward", 60)},
	}
	campaign, _, skip := buildTrackedCampaignAt(summary, detail, startAt)
	if skip != skipNone || !campaign.DateMatch {
		t.Fatalf("campaign at exact StartAt: skip=%v DateMatch=%v, want active", skip, campaign.DateMatch)
	}
}

func TestBuildTrackedCampaignDoesNotMutateSourceMaps(t *testing.T) {
	summary := map[string]interface{}{
		"id": "campaign-immutable", "name": "Immutable", "status": "ACTIVE",
		"startAt": rfc3339(time.Now().Add(-2 * time.Hour)),
		"game":    map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
	}
	detail := map[string]interface{}{
		"id": "campaign-immutable", "name": "Immutable",
		"timeBasedDrops": []interface{}{activeDrop("drop-immutable", "Reward", 60)},
	}
	beforeSummary, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	beforeDetail, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}

	if _, _, skip := buildTrackedCampaign(summary, detail); skip != skipNone {
		t.Fatalf("precondition: current campaign should be tracked, got skip=%v", skip)
	}
	afterSummary, _ := json.Marshal(summary)
	afterDetail, _ := json.Marshal(detail)
	if !bytes.Equal(beforeSummary, afterSummary) || !bytes.Equal(beforeDetail, afterDetail) {
		t.Fatalf("buildTrackedCampaign mutated its source maps\nsummary before=%s\nsummary after=%s\ndetail before=%s\ndetail after=%s", beforeSummary, afterSummary, beforeDetail, afterDetail)
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

// TestObserveClaimedFromInventoryExtractsFullIdentity is a direct, low-level
// check of the E3 (S4) identity extraction: given a raw inventory
// dropCampaignsInProgress entry with self.isClaimed=true, the skip ledger row
// it produces carries the complete identity bundle (game/benefit/instance/
// drop/campaign IDs) read straight from the decoded maps, and an unclaimed
// sibling drop in the SAME entry is ignored entirely.
func TestObserveClaimedFromInventoryExtractsFullIdentity(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	d := &DropsTracker{skipLedger: ledger}

	prog := []interface{}{
		map[string]interface{}{
			"id":   "camp-e3c",
			"name": "Camp E3c",
			"game": map[string]interface{}{"id": "game-e3c", "name": "Game"},
			"timeBasedDrops": []interface{}{
				claimedInProgressDrop("drop-e3c", "Reward", 60, 60, "inst-e3c", "ben-e3c"),
				// An unclaimed sibling drop in the same entry must be ignored.
				inProgressDrop("drop-e3c-2", "Other Reward", 60, 30, false),
			},
		},
	}

	d.observeClaimedFromInventory(prog)

	snap, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	row, ok := snap.byInstance["inst-e3c"]
	if !ok {
		t.Fatal("expected a ledger row for the claimed drop's instance")
	}
	if row.gameID != "game-e3c" || row.benefitID != "ben-e3c" || row.campaignID != "camp-e3c" || row.dropID != "drop-e3c" {
		t.Fatalf("incomplete identity extracted from raw inventory: %+v", row)
	}
	if _, ok := snap.byComposite[compositeKey{"camp-e3c", "drop-e3c-2"}]; ok {
		t.Error("an unclaimed drop in the same entry must not create a ledger row")
	}
}
