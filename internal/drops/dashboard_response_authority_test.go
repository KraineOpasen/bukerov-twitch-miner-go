package drops

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func TestGetDropsDashboardResponseAuthority(t *testing.T) {
	active := map[string]interface{}{
		"id":     "campaign-active",
		"name":   "Active Campaign",
		"status": "ACTIVE",
	}
	validEmpty := dashboardResponse()
	var nilCampaigns []interface{}

	tests := []struct {
		name     string
		response map[string]interface{}
		wantErr  bool
		wantIDs  []string
	}{
		{
			name:     "valid non-empty",
			response: dashboardResponse(active),
			wantIDs:  []string{"campaign-active"},
		},
		{
			name:     "valid explicit empty",
			response: validEmpty,
			wantIDs:  []string{},
		},
		{
			name: "valid summary with unknown status",
			response: dashboardResponse(map[string]interface{}{
				"id": "campaign-status-unknown",
			}),
			wantIDs: []string{"campaign-status-unknown"},
		},
		{
			name: "top-level GraphQL errors without data",
			response: map[string]interface{}{
				"errors": []interface{}{map[string]interface{}{"message": "temporary dashboard failure"}},
			},
			wantErr: true,
		},
		{
			name: "top-level GraphQL errors with otherwise valid data",
			response: map[string]interface{}{
				"errors": []interface{}{map[string]interface{}{"message": "partial dashboard failure"}},
				"data":   validEmpty["data"],
			},
			wantErr: true,
		},
		{
			name: "present empty errors member",
			response: map[string]interface{}{
				"errors": []interface{}{},
				"data":   validEmpty["data"],
			},
			wantErr: true,
		},
		{
			name: "malformed errors member",
			response: map[string]interface{}{
				"errors": "invalid",
				"data":   validEmpty["data"],
			},
			wantErr: true,
		},
		{name: "nil response", response: nil, wantErr: true},
		{name: "missing data", response: map[string]interface{}{}, wantErr: true},
		{name: "null data", response: map[string]interface{}{"data": nil}, wantErr: true},
		{name: "non-object data", response: map[string]interface{}{"data": "invalid"}, wantErr: true},
		{
			name:     "missing currentUser",
			response: map[string]interface{}{"data": map[string]interface{}{}},
			wantErr:  true,
		},
		{
			name:     "null currentUser",
			response: map[string]interface{}{"data": map[string]interface{}{"currentUser": nil}},
			wantErr:  true,
		},
		{
			name:     "non-object currentUser",
			response: map[string]interface{}{"data": map[string]interface{}{"currentUser": "invalid"}},
			wantErr:  true,
		},
		{
			name: "missing dropCampaigns",
			response: map[string]interface{}{
				"data": map[string]interface{}{"currentUser": map[string]interface{}{}},
			},
			wantErr: true,
		},
		{
			name: "null dropCampaigns",
			response: map[string]interface{}{
				"data": map[string]interface{}{"currentUser": map[string]interface{}{"dropCampaigns": nil}},
			},
			wantErr: true,
		},
		{
			name: "typed nil dropCampaigns",
			response: map[string]interface{}{
				"data": map[string]interface{}{"currentUser": map[string]interface{}{"dropCampaigns": nilCampaigns}},
			},
			wantErr: true,
		},
		{
			name: "non-array dropCampaigns",
			response: map[string]interface{}{
				"data": map[string]interface{}{"currentUser": map[string]interface{}{"dropCampaigns": map[string]interface{}{}}},
			},
			wantErr: true,
		},
		{
			name: "non-object campaign element",
			response: map[string]interface{}{
				"data": map[string]interface{}{"currentUser": map[string]interface{}{"dropCampaigns": []interface{}{"invalid"}}},
			},
			wantErr: true,
		},
		{
			name: "mixed valid and malformed campaign elements",
			response: map[string]interface{}{
				"data": map[string]interface{}{"currentUser": map[string]interface{}{"dropCampaigns": []interface{}{active, nil}}},
			},
			wantErr: true,
		},
		{
			name: "campaign missing id",
			response: dashboardResponse(map[string]interface{}{
				"name": "Missing ID",
			}),
			wantErr: true,
		},
		{
			name: "campaign empty id",
			response: dashboardResponse(map[string]interface{}{
				"id": "",
			}),
			wantErr: true,
		},
		{
			name: "campaign whitespace id",
			response: dashboardResponse(map[string]interface{}{
				"id": " \t ",
			}),
			wantErr: true,
		},
		{
			name: "campaign id with surrounding whitespace",
			response: dashboardResponse(map[string]interface{}{
				"id": " campaign-active ",
			}),
			wantErr: true,
		},
		{
			name: "campaign non-string id",
			response: dashboardResponse(map[string]interface{}{
				"id": 42,
			}),
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeDropsClient{dashboard: tc.response}
			tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)

			campaigns, err := tracker.getDropsDashboard("ACTIVE")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("getDropsDashboard() error = nil, want untrusted response rejected; campaigns=%v", campaigns)
				}
				return
			}
			if err != nil {
				t.Fatalf("getDropsDashboard() unexpected error: %v", err)
			}
			if len(campaigns) != len(tc.wantIDs) {
				t.Fatalf("getDropsDashboard() campaigns=%v, want IDs %v", campaigns, tc.wantIDs)
			}
			for i, wantID := range tc.wantIDs {
				if gotID, _ := campaigns[i]["id"].(string); gotID != wantID {
					t.Fatalf("campaign[%d].id=%q, want %q", i, gotID, wantID)
				}
			}
		})
	}
}

func TestSyncCampaignsInvalidDashboardPreservesLastKnownAndRecovers(t *testing.T) {
	summaryA, detailA, summaryB, detailB := campaignAAndB()
	client := &fakeDropsClient{
		dashboard: dashboardResponse(summaryA),
		inventory: emptyInventoryResponse(),
		details: map[string]map[string]interface{}{
			"campaign-a": detailA,
			"campaign-b": detailB,
		},
	}
	streamer := models.NewStreamer("dashboard-authority", models.StreamerSettings{ClaimDrops: true})
	streamer.SetConfirmedOnline()
	streamer.Stream.Game = &models.Game{ID: "game-wot", Name: "World of Tanks"}
	streamer.Stream.SetCampaignIDs([]string{"campaign-a", "campaign-b"})
	tracker := NewDropsTracker(client, []*models.Streamer{streamer}, config.RateLimitSettings{}, nil)

	tracker.syncCampaigns()
	assertOnlyTrackedCampaign(t, tracker, "campaign-a")
	assertAssigned(t, streamer, "campaign-a")
	revisionBeforeFailure := tracker.Revision()
	statusBeforeFailure := tracker.SyncStatus()

	client.dashboard = map[string]interface{}{
		"errors": []interface{}{map[string]interface{}{"message": "temporary dashboard failure"}},
	}
	tracker.syncCampaigns()

	assertOnlyTrackedCampaign(t, tracker, "campaign-a")
	assertAssigned(t, streamer, "campaign-a")
	if revision := tracker.Revision(); revision != revisionBeforeFailure {
		t.Fatalf("invalid dashboard response published revision %d, want preserved %d", revision, revisionBeforeFailure)
	}
	statusAfterFailure := tracker.SyncStatus()
	if statusAfterFailure.LastError == "" {
		t.Fatal("invalid dashboard response must be visible in SyncStatus.LastError")
	}
	if statusAfterFailure.TrackedCampaigns != statusBeforeFailure.TrackedCampaigns {
		t.Fatalf("invalid response changed tracked count from %d to %d despite preserving the pool",
			statusBeforeFailure.TrackedCampaigns, statusAfterFailure.TrackedCampaigns)
	}
	if !statusAfterFailure.LastSuccessAt.Equal(statusBeforeFailure.LastSuccessAt) {
		t.Fatalf("invalid response changed LastSuccessAt from %s to %s",
			statusBeforeFailure.LastSuccessAt, statusAfterFailure.LastSuccessAt)
	}
	if !statusAfterFailure.BackendUpdatedAt.Equal(statusBeforeFailure.BackendUpdatedAt) {
		t.Fatalf("invalid response changed BackendUpdatedAt from %s to %s",
			statusBeforeFailure.BackendUpdatedAt, statusAfterFailure.BackendUpdatedAt)
	}
	if statusAfterFailure.UpdateSource != statusBeforeFailure.UpdateSource {
		t.Fatalf("invalid response changed UpdateSource from %q to %q",
			statusBeforeFailure.UpdateSource, statusAfterFailure.UpdateSource)
	}

	client.dashboard = dashboardResponse(summaryB)
	tracker.syncCampaigns()

	assertOnlyTrackedCampaign(t, tracker, "campaign-b")
	assertAssigned(t, streamer, "campaign-b")
	if revision := tracker.Revision(); revision <= revisionBeforeFailure {
		t.Fatalf("later valid response did not publish a new revision: got %d, prior %d", revision, revisionBeforeFailure)
	}
	if status := tracker.SyncStatus(); status.LastError != "" {
		t.Fatalf("later valid response did not clear the prior sync error: %q", status.LastError)
	}
}

func TestSyncCampaignsValidEmptyDashboardAuthoritativelyClearsCampaigns(t *testing.T) {
	summaryA, detailA, _, _ := campaignAAndB()
	client := &fakeDropsClient{
		dashboard: dashboardResponse(summaryA),
		inventory: emptyInventoryResponse(),
		details:   map[string]map[string]interface{}{"campaign-a": detailA},
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)

	tracker.syncCampaigns()
	assertOnlyTrackedCampaign(t, tracker, "campaign-a")
	revisionBeforeEmpty := tracker.Revision()

	client.dashboard = dashboardResponse()
	tracker.syncCampaigns()

	if campaigns := tracker.Campaigns(); len(campaigns) != 0 {
		t.Fatalf("valid explicit empty dashboard did not clear tracked campaigns: %+v", campaigns)
	}
	if revision := tracker.Revision(); revision <= revisionBeforeEmpty {
		t.Fatalf("valid explicit empty dashboard did not publish: revision=%d, prior=%d", revision, revisionBeforeEmpty)
	}
	if status := tracker.SyncStatus(); status.LastError != "" || status.DashboardCampaigns != 0 || status.TrackedCampaigns != 0 {
		t.Fatalf("valid explicit empty dashboard was not recorded as authoritative zero: %+v", status)
	}
}

func assertOnlyTrackedCampaign(t *testing.T, tracker *DropsTracker, wantID string) {
	t.Helper()
	campaigns := tracker.Campaigns()
	if len(campaigns) != 1 || campaigns[0].ID != wantID {
		t.Fatalf("tracked campaigns=%+v, want exactly %q", campaigns, wantID)
	}
}
