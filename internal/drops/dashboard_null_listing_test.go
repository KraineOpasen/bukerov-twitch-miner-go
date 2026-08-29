package drops

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// Explicit-null ViewerDropsDashboard listing authority
// (fix/drops-dashboard-null-authority).
//
// Production Twitch has been observed (owner-authorized read-only canaries,
// 2026-08-29, across the old and donor persisted-query hashes, both
// fetchRewardCampaigns variants, and every tested client profile) returning
// HTTP 200 with an EXPLICIT JSON null at exactly
// data.currentUser.dropCampaigns — without top-level errors on all but the
// browser/web profile, whose errors-bearing variant stays a hard error (see
// the "top-level errors with null dropCampaigns" authority-matrix row). This
// file pins the semantics of the errorless null shape:
//
//   - explicit JSON null = the dashboard listing is UNKNOWN/unavailable — a
//     distinct observation, NOT an error and NOT an authoritative empty array;
//   - the full sync must still run the existing syncWithInventory
//     reconciliation, so positive dropCampaignsInProgress evidence recovers
//     real in-progress campaigns (N1);
//   - a null listing alone must never erase last-known campaign state: with no
//     positive inventory evidence the previously published pool is preserved
//     untouched (N2), and previously tracked campaigns without fresh inventory
//     evidence survive alongside freshly recovered ones, which inherit their
//     previous version's known date window when the inventory entry omits it
//     (N3);
//   - an inventory acquisition failure during a null listing aborts exactly
//     like the established F2 inventory-failure path (N4);
//   - the sync summary must never claim "Twitch reports no active drop
//     campaigns" for an UNKNOWN listing, and must not log a sync failure (N5);
//   - both null-listing publish decisions stay race-clean against concurrent
//     status/pool readers (N6);
//   - the PR #252 authority matrix is untouched for every other shape:
//     missing key, wrong type, malformed elements, and top-level GQL errors
//     stay hard errors, and explicit [] stays an authoritative empty listing
//     (pinned in dashboard_response_authority_test.go).

// nullDashboardResponse is the exact owner-observed production shape: HTTP 200,
// no top-level errors, data and currentUser objects present, and an explicit
// JSON null under the dropCampaigns key. encoding/json decodes that null to an
// untyped nil interface value stored under a PRESENT key — distinct from a
// missing key (absent) and from a typed-nil []interface{} (hand-built only).
func nullDashboardResponse() map[string]interface{} {
	return map[string]interface{}{
		"data": map[string]interface{}{
			"currentUser": map[string]interface{}{
				"dropCampaigns": nil,
			},
		},
	}
}

// wotInProgress is the inventory dropCampaignsInProgress entry used as the
// positive authoritative evidence in the null-listing tests: a real campaign
// Twitch is actively crediting, mid-progress and unclaimed.
func wotInProgress() map[string]interface{} {
	return map[string]interface{}{
		"id":   "campaign-wot",
		"name": "World of Tanks AMD Summer Arena Drops#2",
		"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			inProgressDrop("drop-1", "Garage Slot", 120, 118, false),
		},
	}
}

// TestSyncCampaignsNullDashboardListingRecoversInProgressFromInventory is N1
// — the central behavioral RED for this fix, reproducing the
// production outage: dashboard listing explicitly null, inventory carrying one
// real in-progress campaign, no previously tracked pool. On the unfixed base
// the dashboard parser rejects the null as "missing or malformed dropCampaigns
// array" and syncCampaignsLocked aborts before syncWithInventory ever runs, so
// nothing is recovered and drops farming stays dead. The fix must classify the
// null as an UNKNOWN listing (not an error), let the existing inventory
// reconciliation recover the campaign, publish it, and record the sync attempt
// as a completed observation rather than a failure.
func TestSyncCampaignsNullDashboardListingRecoversInProgressFromInventory(t *testing.T) {
	client := &fakeDropsClient{
		dashboard: nullDashboardResponse(),
		inventory: inventoryWithInProgress(wotInProgress()),
		details:   map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)

	tracker.syncCampaigns()

	got := tracker.Campaigns()
	if len(got) != 1 {
		t.Fatalf("expected 1 campaign recovered from inventory despite the null dashboard listing, got %d", len(got))
	}
	if got[0].ID != "campaign-wot" || !got[0].InInventory {
		t.Fatalf("recovered campaign = %+v, want campaign-wot marked InInventory", got[0])
	}

	status := tracker.SyncStatus()
	if status.LastError != "" {
		t.Errorf("an explicit-null listing is an UNKNOWN observation, not a failure; got LastError=%q", status.LastError)
	}
	if !status.DashboardListingUnavailable {
		t.Error("SyncStatus must report the dashboard listing as unavailable (UNKNOWN), not as an authoritative count")
	}
	if status.LastSuccessAt.IsZero() {
		t.Error("a null-listing sync that completed its inventory reconciliation must advance LastSuccessAt")
	}
	if status.DashboardCampaigns != 0 {
		t.Errorf("expected dashboardCampaigns=0 (nothing was listed), got %d", status.DashboardCampaigns)
	}
	if status.RecoveredCampaigns != 1 {
		t.Errorf("expected recoveredCampaigns=1 (attributed to inventory recovery), got %d", status.RecoveredCampaigns)
	}
	if status.TrackedCampaigns != 1 {
		t.Errorf("expected trackedCampaigns=1, got %d", status.TrackedCampaigns)
	}
	if tracker.Revision() == 0 {
		t.Error("recovering a real campaign must publish the pool (revision bump)")
	}
}

// TestSyncCampaignsNullDashboardListingWithEmptyInventoryPreservesLastKnownGood
// is N2 — the last-known-good falsifier: an explicit-null listing plus
// a valid inventory with no in-progress campaigns must NOT become authority to
// clear the previously published pool (null is not authoritative zero), must
// not republish it (no new evidence), and must not be recorded as a failure.
func TestSyncCampaignsNullDashboardListingWithEmptyInventoryPreservesLastKnownGood(t *testing.T) {
	summaryA, detailA, _, _ := campaignAAndB()
	client := &fakeDropsClient{
		dashboard: dashboardResponse(summaryA),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"campaign-a": detailA},
	}
	streamer := models.NewStreamer("null-authority", models.StreamerSettings{ClaimDrops: true})
	streamer.SetConfirmedOnline()
	streamer.Stream.Game = &models.Game{ID: "game-wot", Name: "World of Tanks"}
	streamer.Stream.SetCampaignIDs([]string{"campaign-a"})
	tracker := NewDropsTracker(client, []*models.Streamer{streamer}, config.RateLimitSettings{}, nil)

	tracker.syncCampaigns()
	assertOnlyTrackedCampaign(t, tracker, "campaign-a")
	assertAssigned(t, streamer, "campaign-a")
	revisionBefore := tracker.Revision()
	statusBefore := tracker.SyncStatus()
	if statusBefore.DashboardListingUnavailable {
		t.Fatal("an authoritative sync must not report the listing as unavailable")
	}

	client.dashboard = nullDashboardResponse()
	tracker.syncCampaigns()

	assertOnlyTrackedCampaign(t, tracker, "campaign-a")
	assertAssigned(t, streamer, "campaign-a")
	if revision := tracker.Revision(); revision != revisionBefore {
		t.Fatalf("null listing with no positive inventory evidence republished the pool: revision %d, want preserved %d", revision, revisionBefore)
	}
	status := tracker.SyncStatus()
	if status.LastError != "" {
		t.Errorf("null listing must not be recorded as a sync failure; got LastError=%q", status.LastError)
	}
	if !status.DashboardListingUnavailable {
		t.Error("SyncStatus must report the dashboard listing as unavailable (UNKNOWN)")
	}
	if status.TrackedCampaigns != 1 {
		t.Errorf("null listing must not zero the tracked count; got %d, want 1", status.TrackedCampaigns)
	}
	if !status.BackendUpdatedAt.Equal(statusBefore.BackendUpdatedAt) {
		t.Errorf("null listing with nothing recovered must not touch BackendUpdatedAt: %s -> %s",
			statusBefore.BackendUpdatedAt, status.BackendUpdatedAt)
	}
}

// TestSyncCampaignsNullDashboardListingKeepsUnprovenPreviousCampaigns is N3 —
// the bounded-removal rule: under an UNKNOWN listing, fresh positive inventory
// evidence refreshes the campaigns it covers, while previously tracked
// campaigns with no fresh evidence are kept as-is. Absence from
// dropCampaignsInProgress is not proof a campaign ended (a campaign whose
// drops have no started progress never appears there), so deletion authority
// requires an authoritative listing.
func TestSyncCampaignsNullDashboardListingKeepsUnprovenPreviousCampaigns(t *testing.T) {
	summaryA, detailA, summaryB, detailB := campaignAAndB()
	client := &fakeDropsClient{
		dashboard: dashboardResponse(summaryA, summaryB),
		inventory: emptyInventoryResponse(),
		details: map[string]map[string]interface{}{
			"campaign-a": detailA,
			"campaign-b": detailB,
		},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)

	tracker.syncCampaigns()
	if got := tracker.Campaigns(); len(got) != 2 {
		t.Fatalf("seed sync expected to track campaigns a and b, got %+v", got)
	}

	// Twitch now nulls the listing; the inventory proves only campaign-b is
	// actively progressing (45/120 minutes watched).
	client.dashboard = nullDashboardResponse()
	client.inventory = inventoryWithInProgress(map[string]interface{}{
		"id":   "campaign-b",
		"name": "Campaign B",
		"game": map[string]interface{}{"id": "game-wot", "name": "World of Tanks"},
		"timeBasedDrops": []interface{}{
			inProgressDrop("drop-b", "Reward B", 120, 45, false),
		},
	})
	tracker.syncCampaigns()

	byID := make(map[string]*models.Campaign)
	for _, c := range tracker.Campaigns() {
		byID[c.ID] = c
	}
	if len(byID) != 2 {
		t.Fatalf("expected campaigns a (kept) and b (refreshed), got %+v", tracker.Campaigns())
	}
	if byID["campaign-a"] == nil {
		t.Fatal("campaign-a had no fresh inventory evidence and must be kept — a null listing is not deletion authority")
	}
	b := byID["campaign-b"]
	if b == nil {
		t.Fatal("campaign-b must be present, refreshed from its in-progress inventory entry")
	}
	if !b.InInventory {
		t.Error("campaign-b must carry the fresh inventory observation (InInventory)")
	}
	if len(b.Drops) != 1 || b.Drops[0].CurrentMinutesWatched != 45 {
		t.Errorf("campaign-b must carry the live 45-minute progress from the inventory, got %+v", b.Drops)
	}
	// The inventory entry above carries no startAt/endAt: the refreshed
	// campaign must inherit the previous (details-built) version's known date
	// window instead of zeroing it out (a date-less rebuild is not authority
	// to erase good dates).
	if b.StartAt.IsZero() || b.EndAt.IsZero() {
		t.Errorf("campaign-b lost its known date window on an inventory-only rebuild: startAt=%v endAt=%v", b.StartAt, b.EndAt)
	}
	if status := tracker.SyncStatus(); status.LastError != "" {
		t.Errorf("null listing with positive recovery must not be recorded as a failure; got LastError=%q", status.LastError)
	}
}

// TestConcurrentNullListingSyncAndStatusRaceSafe is N6: both null-listing
// publish decisions — positive inventory evidence (union publish) and no
// evidence (pool untouched) — must be race-clean against concurrent
// SyncStatus/Campaigns/Revision readers. Mirrors the race guard
// TestConcurrentSyncAndStatusRaceSafe pins for the authoritative path; the
// fake's dashboard/inventory fields are swapped only between phases, while no
// concurrent caller can re-enter PostGQL.
func TestConcurrentNullListingSyncAndStatusRaceSafe(t *testing.T) {
	client := &fakeDropsClient{
		dashboard: nullDashboardResponse(),
		inventory: inventoryWithInProgress(wotInProgress()),
		details:   map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)

	runPhase := func() {
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				tracker.syncCampaigns()
			}()
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = tracker.SyncStatus()
				_ = tracker.Campaigns()
				_ = tracker.Revision()
			}()
		}
		wg.Wait()
	}

	// Phase 1: null listing with positive evidence (union-publish branch).
	runPhase()
	// Phase 2: null listing with a valid empty inventory (no-publish branch).
	client.inventory = emptyInventoryResponse()
	runPhase()

	status := tracker.SyncStatus()
	if !status.DashboardListingUnavailable || status.LastError != "" {
		t.Fatalf("null-listing syncs must record the unavailable observation without an error, got %+v", status)
	}
	if got := tracker.Campaigns(); len(got) != 1 || got[0].ID != "campaign-wot" {
		t.Fatalf("recovered campaign must survive the no-evidence phase untouched, got %+v", got)
	}
}

// TestSyncCampaignsNullDashboardListingInventoryFailurePreservesLastKnownGood
// is N4: when the listing is null AND the inventory acquisition the
// recovery depends on fails, the sync must abort exactly like the established
// F2 inventory-failure path — last-known pool, revision, and LastSuccessAt all
// preserved, with the failure truthfully visible in LastError.
func TestSyncCampaignsNullDashboardListingInventoryFailurePreservesLastKnownGood(t *testing.T) {
	summaryA, detailA, _, _ := campaignAAndB()
	client := &fakeDropsClient{
		dashboard: dashboardResponse(summaryA),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"campaign-a": detailA},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)

	tracker.syncCampaigns()
	assertOnlyTrackedCampaign(t, tracker, "campaign-a")
	revisionBefore := tracker.Revision()
	statusBefore := tracker.SyncStatus()

	client.dashboard = nullDashboardResponse()
	client.inventoryErr = errors.New("inventory unavailable")
	tracker.syncCampaigns()

	assertOnlyTrackedCampaign(t, tracker, "campaign-a")
	if revision := tracker.Revision(); revision != revisionBefore {
		t.Fatalf("inventory failure during a null listing republished the pool: revision %d, want preserved %d", revision, revisionBefore)
	}
	status := tracker.SyncStatus()
	if status.LastError == "" {
		t.Fatal("a failed inventory acquisition must stay visible in SyncStatus.LastError even under a null listing")
	}
	if !status.LastSuccessAt.Equal(statusBefore.LastSuccessAt) {
		t.Fatalf("inventory failure advanced LastSuccessAt from %s to %s", statusBefore.LastSuccessAt, status.LastSuccessAt)
	}
	if status.TrackedCampaigns != 1 {
		t.Fatalf("failed sync must report the preserved pool, got TrackedCampaigns=%d", status.TrackedCampaigns)
	}
}

// TestSyncCampaignsNullDashboardListingSummaryIsTruthful is N5: the completion
// summary for an UNKNOWN listing must never claim Twitch authoritatively
// reported zero campaigns, and the run must not be logged as a sync failure.
// This exercises the exact production startup state: empty pool, null listing,
// no in-progress inventory evidence.
func TestSyncCampaignsNullDashboardListingSummaryIsTruthful(t *testing.T) {
	client := &fakeDropsClient{
		dashboard: nullDashboardResponse(),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)

	logs := captureLogs(t, func() { tracker.syncCampaigns() })

	if strings.Contains(logs, "Twitch reports no active drop campaigns") {
		t.Error("an UNKNOWN listing must never be summarized as an authoritative empty dashboard")
	}
	if strings.Contains(logs, "Drops sync failed") {
		t.Error("an explicit-null listing is an observation, not a sync failure")
	}
	if !strings.Contains(logs, "Drops sync complete") {
		t.Error("the null-listing sync must still publish a completion summary for the Logs page")
	}
	if tracker.Revision() != 0 {
		t.Errorf("nothing was recovered and nothing was previously tracked: no publication expected, got revision %d", tracker.Revision())
	}
	status := tracker.SyncStatus()
	if status.LastError != "" {
		t.Errorf("null listing must not set LastError; got %q", status.LastError)
	}
	if !status.DashboardListingUnavailable {
		t.Error("SyncStatus must report the dashboard listing as unavailable (UNKNOWN)")
	}
	if status.DashboardCampaigns != 0 || status.TrackedCampaigns != 0 || status.RecoveredCampaigns != 0 {
		t.Errorf("startup null-listing sync with no evidence must report zero counts alongside the unavailable flag, got %+v", status)
	}
}
