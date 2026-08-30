package drops

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func observationCampaign(campaignID, dropID string, minutes int) *models.Campaign {
	return &models.Campaign{
		ID: campaignID,
		Drops: []*models.Drop{{
			ID:                    dropID,
			CurrentMinutesWatched: minutes,
		}},
	}
}

func TestProgressObservationIsCoherentForExactDrop(t *testing.T) {
	const iterations = 2000

	tracker := &DropsTracker{}
	publish := func(epoch uint64) {
		tracker.mu.Lock()
		tracker.progressRuns = int(epoch)
		tracker.progressLastSyncAt = time.Unix(int64(epoch), 0).UTC()
		tracker.revision = epoch
		tracker.progressRevision = epoch
		tracker.campaigns = []*models.Campaign{
			observationCampaign("campaign", "drop", int(epoch)),
		}
		tracker.progressExact = map[progressTuple]int{{campaignID: "campaign", dropID: "drop"}: int(epoch)}
		tracker.progressUnknown = map[progressTuple]struct{}{}
		tracker.mu.Unlock()
	}
	publish(1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for epoch := uint64(2); epoch <= iterations; epoch++ {
			publish(epoch)
		}
	}()
	defer func() { <-done }()

	for {
		got := tracker.ProgressObservation("campaign", "drop")
		if !got.Found {
			t.Fatal("exact current campaign/drop must be found")
		}
		if got.CampaignID != "campaign" || got.DropID != "drop" {
			t.Fatalf("observation identity = %q/%q, want campaign/drop", got.CampaignID, got.DropID)
		}
		if got.Revision != got.Run || got.ObservedAt.Unix() != int64(got.Run) ||
			got.Error != "" || !got.Complete || got.AuthoritativeAbsent || got.Minutes != int(got.Run) {
			t.Fatalf("torn progress observation: %+v", got)
		}

		select {
		case <-done:
			return
		default:
		}
	}
}

func TestProgressObservationDoesNotMatchUnrelatedTuple(t *testing.T) {
	observedAt := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	tracker := &DropsTracker{
		campaigns:          []*models.Campaign{observationCampaign("campaign-a", "drop-a", 17)},
		revision:           9,
		progressRevision:   9,
		progressRuns:       4,
		progressLastSyncAt: observedAt,
		progressExact:      map[progressTuple]int{{campaignID: "campaign-a", dropID: "drop-a"}: 17},
		progressUnknown:    map[progressTuple]struct{}{},
	}

	for _, tc := range []struct {
		name       string
		campaignID string
		dropID     string
	}{
		{name: "same campaign other drop", campaignID: "campaign-a", dropID: "drop-b"},
		{name: "other campaign same drop", campaignID: "campaign-b", dropID: "drop-a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tracker.ProgressObservation(tc.campaignID, tc.dropID)
			if got.Found || got.Minutes != 0 || !got.Complete || !got.AuthoritativeAbsent {
				t.Fatalf("unrelated tuple must not inherit progress: %+v", got)
			}
			if got.CampaignID != tc.campaignID || got.DropID != tc.dropID {
				t.Fatalf("requested identity was not preserved: %+v", got)
			}
			if got.Run != 4 || !got.ObservedAt.Equal(observedAt) || got.Revision != 9 {
				t.Fatalf("observation metadata must still be coherent when tuple is absent: %+v", got)
			}
		})
	}
}

func TestProgressObservationKeepsErrorDistinctFromRetainedCampaignMinutes(t *testing.T) {
	observedAt := time.Date(2026, 8, 30, 9, 5, 0, 0, time.UTC)
	tracker := &DropsTracker{
		campaigns:            []*models.Campaign{observationCampaign("campaign", "drop", 23)},
		revision:             12,
		progressRevision:     12,
		progressRuns:         8,
		progressLastSyncAt:   observedAt,
		progressAuthorityErr: "inventory unavailable",
	}

	got := tracker.ProgressObservation("campaign", "drop")
	if got.Found || got.Minutes != 0 {
		t.Fatalf("a failed run must not attach retained minutes as fresh exact presence: %+v", got)
	}
	if got.Error != "inventory unavailable" || got.Run != 8 || got.Revision != 12 || !got.ObservedAt.Equal(observedAt) {
		t.Fatalf("latest failed observation metadata was not preserved: %+v", got)
	}
	if campaigns := tracker.Campaigns(); len(campaigns) != 1 || campaigns[0].Drops[0].CurrentMinutesWatched != 23 {
		t.Fatalf("failed authority run should still preserve display/assignment state: %+v", campaigns)
	}
}

func TestProgressObservationLatestRunOwnsExactPresence(t *testing.T) {
	tests := []struct {
		name      string
		inventory map[string]interface{}
	}{
		{name: "valid empty", inventory: inventoryWithInProgress()},
		{name: "unrelated exact tuple", inventory: inventoryWithInProgress(map[string]interface{}{
			"id": "other-campaign",
			"timeBasedDrops": []interface{}{
				inProgressDrop("other-drop", "Other", 60, 7, false),
			},
		})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tracker := NewDropsTracker(&fakeDropsClient{inventory: tc.inventory}, nil, config.RateLimitSettings{}, nil)
			tracker.mu.Lock()
			tracker.campaigns = []*models.Campaign{observationCampaign("campaign", "drop", 5)}
			tracker.revision = 1
			tracker.mu.Unlock()

			tracker.syncProgress()

			got := tracker.ProgressObservation("campaign", "drop")
			if got.Run != 1 || got.Error != "" || got.Found || got.Minutes != 0 ||
				!got.Complete || !got.AuthoritativeAbsent {
				t.Fatalf("latest run borrowed retained exact progress: %+v", got)
			}
			if campaigns := tracker.Campaigns(); len(campaigns) != 1 || campaigns[0].Drops[0].CurrentMinutesWatched != 5 {
				t.Fatalf("exact-presence publication mutated retained campaign state: %+v", campaigns)
			}
		})
	}
}

func TestProgressObservationMissingOrNullListIsUnknownNotAuthoritativeAbsence(t *testing.T) {
	tests := []struct {
		name      string
		inventory map[string]interface{}
	}{
		{name: "missing", inventory: emptyInventoryResponse()},
		{name: "null", inventory: func() map[string]interface{} {
			response := emptyInventoryResponse()
			inventory := response["data"].(map[string]interface{})["currentUser"].(map[string]interface{})["inventory"].(map[string]interface{})
			inventory["dropCampaignsInProgress"] = nil
			return response
		}()},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tracker := NewDropsTracker(&fakeDropsClient{inventory: tc.inventory}, nil, config.RateLimitSettings{}, nil)
			tracker.mu.Lock()
			tracker.campaigns = []*models.Campaign{observationCampaign("campaign", "drop", 5)}
			tracker.revision = 1
			tracker.mu.Unlock()

			tracker.syncProgress()
			got := tracker.ProgressObservation("campaign", "drop")
			if got.Error == "" || got.Complete || got.Found || got.AuthoritativeAbsent {
				t.Fatalf("missing/null list became exact absence authority: %+v", got)
			}
			if status := tracker.SyncStatus(); status.ProgressLastError != "" {
				t.Fatalf("strict exact parsing changed ordinary light-sync status: %+v", status)
			}
		})
	}
}

func TestProgressObservationMalformedTupleIsUnknownNotAbsence(t *testing.T) {
	malformed := inventoryWithInProgress(map[string]interface{}{
		"id": "campaign",
		"timeBasedDrops": []interface{}{
			map[string]interface{}{"id": "drop", "self": map[string]interface{}{}},
		},
	})
	tracker := NewDropsTracker(&fakeDropsClient{inventory: malformed}, nil, config.RateLimitSettings{}, nil)
	tracker.mu.Lock()
	tracker.campaigns = []*models.Campaign{observationCampaign("campaign", "drop", 5)}
	tracker.revision = 1
	tracker.mu.Unlock()

	tracker.syncProgress()

	got := tracker.ProgressObservation("campaign", "drop")
	if got.Run != 1 || got.Error != "" || got.Found || got.Minutes != 0 ||
		!got.Complete || !got.TupleUnknown || got.AuthoritativeAbsent {
		t.Fatalf("tuple-specific UNKNOWN was treated as exact progress/absence: %+v", got)
	}
}

func TestProgressObservationMixedNullableRowsDoNotPoisonExactAuthority(t *testing.T) {
	inventory := inventoryWithInProgress(map[string]interface{}{
		"id": "campaign",
		"timeBasedDrops": []interface{}{
			inProgressDrop("drop", "Target", 60, 9, false),
			map[string]interface{}{"id": "future-null", "self": nil},
			map[string]interface{}{"id": "future-missing"},
			map[string]interface{}{"id": "future-minutes-null", "self": map[string]interface{}{"currentMinutesWatched": nil}},
		},
	})
	tracker := NewDropsTracker(&fakeDropsClient{inventory: inventory}, nil, config.RateLimitSettings{}, nil)
	tracker.mu.Lock()
	tracker.campaigns = []*models.Campaign{observationCampaign("campaign", "drop", 0)}
	tracker.revision = 1
	tracker.mu.Unlock()

	tracker.syncProgress()

	target := tracker.ProgressObservation("campaign", "drop")
	if target.Error != "" || !target.Complete || !target.Found || target.Minutes != 9 || target.AuthoritativeAbsent {
		t.Fatalf("unrelated nullable rows poisoned target authority: %+v", target)
	}
	for _, dropID := range []string{"future-null", "future-missing", "future-minutes-null"} {
		got := tracker.ProgressObservation("campaign", dropID)
		if got.Error != "" || !got.Complete || !got.TupleUnknown || got.Found || got.AuthoritativeAbsent {
			t.Fatalf("present nullable tuple %q was collapsed into exact absence: %+v", dropID, got)
		}
	}
}

func TestProgressObservationMalformedPresentMinutesFailsExactAuthorityOnly(t *testing.T) {
	inventory := inventoryWithInProgress(map[string]interface{}{
		"id": "campaign",
		"timeBasedDrops": []interface{}{
			map[string]interface{}{
				"id":   "drop",
				"self": map[string]interface{}{"currentMinutesWatched": "nine"},
			},
		},
	})
	tracker := NewDropsTracker(&fakeDropsClient{inventory: inventory}, nil, config.RateLimitSettings{}, nil)
	tracker.mu.Lock()
	tracker.campaigns = []*models.Campaign{observationCampaign("campaign", "drop", 5)}
	tracker.revision = 1
	tracker.mu.Unlock()

	tracker.syncProgress()

	got := tracker.ProgressObservation("campaign", "drop")
	if got.Run != 1 || got.Error == "" || got.Complete || got.Found || got.AuthoritativeAbsent {
		t.Fatalf("malformed present minutes escaped exact-authority fail-closed handling: %+v", got)
	}
	if status := tracker.SyncStatus(); status.ProgressLastError != "" {
		t.Fatalf("strict provisional parser changed legacy light-sync status: %+v", status)
	}
}

func TestProgressObservationWhitespaceIDsFailExactAuthority(t *testing.T) {
	for _, tc := range []struct {
		name       string
		campaignID string
		dropID     string
	}{
		{name: "blank campaign", campaignID: " ", dropID: "drop"},
		{name: "padded drop", campaignID: "campaign", dropID: "drop "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inventory := inventoryWithInProgress(map[string]interface{}{
				"id": tc.campaignID,
				"timeBasedDrops": []interface{}{
					inProgressDrop(tc.dropID, "Target", 60, 9, false),
				},
			})
			tracker := NewDropsTracker(&fakeDropsClient{inventory: inventory}, nil, config.RateLimitSettings{}, nil)
			tracker.mu.Lock()
			tracker.campaigns = []*models.Campaign{observationCampaign("campaign", "drop", 5)}
			tracker.revision = 1
			tracker.mu.Unlock()

			tracker.syncProgress()

			got := tracker.ProgressObservation("campaign", "drop")
			if got.Run != 1 || got.Error == "" || got.Complete || got.Found || got.AuthoritativeAbsent {
				t.Fatalf("whitespace-bearing ID escaped exact-authority fail-closed handling: %+v", got)
			}
		})
	}
}

func TestGetInventoryRejectsAnyPresentTopLevelErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		errors any
	}{
		{name: "nil", errors: nil},
		{name: "empty array", errors: []interface{}{}},
		{name: "non-empty array", errors: []interface{}{map[string]interface{}{"message": "partial failure"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := emptyInventoryResponse()
			response["errors"] = tc.errors
			tracker := NewDropsTracker(&fakeDropsClient{inventory: response}, nil, config.RateLimitSettings{}, nil)

			inventory, authorityErr, err := tracker.getProgressInventory()
			if err != nil {
				t.Fatalf("ordinary partial-data acquisition changed: %v", err)
			}
			if authorityErr == nil {
				t.Fatal("a present top-level errors member must reject otherwise-valid inventory data")
			}
			if inventory == nil {
				t.Fatal("partial inventory must remain available to the legacy merge/claim-sighting path")
			}
			if got := authorityErr.Error(); got != "twitch GQL Inventory: top-level errors present" {
				t.Fatalf("unexpected authority error: %q", got)
			}

			// Strictness is confined to the lightweight progress authority.
			// Full-sync/claim callers retain their established best-effort
			// handling of partial Inventory data.
			legacy, legacyErr := tracker.getInventory()
			if legacyErr != nil || legacy == nil {
				t.Fatalf("ordinary Inventory semantics changed: inventory=%+v err=%v", legacy, legacyErr)
			}
		})
	}
}

func TestPartialInventoryKeepsRawClaimSightingButCannotAuthorizeProgress(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	response := inventoryWithInProgress(map[string]interface{}{
		"id":   "campaign",
		"name": "Campaign",
		"game": map[string]interface{}{"id": "g1", "name": "Game"},
		"timeBasedDrops": []interface{}{
			claimedInProgressDrop("drop", "Reward", 60, 60, "instance", "benefit"),
		},
	})
	response["errors"] = []interface{}{map[string]interface{}{"message": "partial failure"}}
	tracker := NewDropsTracker(&fakeDropsClient{inventory: response}, nil, config.RateLimitSettings{}, nil)
	tracker.skipLedger = ledger
	tracker.mu.Lock()
	tracker.campaigns = []*models.Campaign{observationCampaign("campaign", "drop", 0)}
	tracker.revision = 1
	tracker.mu.Unlock()

	tracker.syncProgress()

	obs := tracker.ProgressObservation("campaign", "drop")
	if obs.Error == "" || obs.Complete || obs.Found || obs.AuthoritativeAbsent {
		t.Fatalf("partial GraphQL response became exact progress authority: %+v", obs)
	}
	if status := tracker.SyncStatus(); status.ProgressLastError != "" {
		t.Fatalf("partial data changed established ordinary light-sync status: %+v", status)
	}
	snapshot, err := ledger.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("skip snapshot: %v", err)
	}
	skip, reason := snapshot.decide(models.RewardIdentity{
		GameID: "g1", BenefitID: "benefit", InstanceID: "instance", DropID: "drop", CampaignID: "campaign",
	}, false)
	if !skip {
		t.Fatalf("partial response lost the pre-existing raw claimed sighting (reason=%q)", reason)
	}
}

func TestBrokerCampaignsPublishesExactSkipLedgerFilteredViews(t *testing.T) {
	ledger := newTestSkipLedger(t, uniqueAccountKey(t))
	must(t, ledger.Observe(context.Background(), skipEvidence{
		class:      evidenceClaimAccepted,
		gameID:     "g1",
		campaignID: "campaign",
		dropID:     "ghost",
		instanceID: "ghost-instance",
	}))

	ghost := claimableDrop("ghost", "ghost-instance")
	live := claimableDrop("live", "live-instance")
	campaign := campaignFor("campaign", unrestrictedACL(), ghost, live)
	tracker, streamer := assignmentTracker(models.CapabilityEnabled, campaign, func(streamer *models.Streamer) {
		streamer.Stream.SetCampaignIDs([]string{"campaign"})
	})
	tracker.skipLedger = ledger

	tracker.updateStreamerCampaigns()
	snapshot := tracker.BrokerCampaignSnapshot()
	if snapshot.Generation == 0 || snapshot.SourceRevision != snapshot.CurrentRevision ||
		snapshot.SourceRevision != tracker.SyncStatus().Revision {
		t.Fatalf("broker source fence was not published coherently: %+v", snapshot)
	}

	broker := tracker.BrokerCampaigns()
	if len(broker) != 1 || broker[0] == nil || len(broker[0].Drops) != 1 || broker[0].Drops[0].ID != "live" {
		t.Fatalf("broker snapshot did not reuse the skip-ledger-filtered view: %+v", broker)
	}
	if broker[0] == campaign {
		t.Fatal("a wired ledger must publish its filtered clone, not the unfiltered source campaign")
	}
	assigned := streamer.Stream.GetCampaigns()
	if len(assigned) != 1 || assigned[0] != broker[0] {
		t.Fatalf("stream assignment and BrokerCampaigns must use the exact same filtered view: assigned=%+v broker=%+v", assigned, broker)
	}
	if source := tracker.Campaigns(); len(source) != 1 || len(source[0].Drops) != 2 {
		t.Fatalf("broker filtering mutated the source campaign pool: %+v", source)
	}

	// The accessor owns its slice: caller-side replacement cannot corrupt the
	// atomically published view.
	broker[0] = nil
	again := tracker.BrokerCampaigns()
	if len(again) != 1 || again[0] == nil || len(again[0].Drops) != 1 || again[0].Drops[0].ID != "live" {
		t.Fatalf("caller mutated the tracker's broker slice: %+v", again)
	}

	if got := fmt.Sprintf("%s/%s", again[0].ID, again[0].Drops[0].ID); got != "campaign/live" {
		t.Fatalf("unexpected exact broker tuple: %s", got)
	}
}
