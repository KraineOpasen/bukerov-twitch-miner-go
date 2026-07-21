package drops

import (
	"errors"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// statusClient is a twitchClient whose ClaimDrop returns a scripted ClaimStatus
// (or error) and counts how many times it was actually invoked, so a test can
// prove both the claim-call count and the success-event count.
type statusClient struct {
	*fakeDropsClient
	mu     sync.Mutex
	status api.ClaimStatus
	err    error
	calls  int
}

func (c *statusClient) ClaimDrop(*models.Drop) (api.ClaimStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return c.status, c.err
}

func (c *statusClient) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// invEntry builds one inventory timeBasedDrops entry. extraSelf lets a test add
// dropInstanceID / isClaimable / isClaimed / hasPreconditionsMet to the `self`.
func invEntry(id string, required, watched float64, extraSelf map[string]interface{}) map[string]interface{} {
	sf := map[string]interface{}{"currentMinutesWatched": watched}
	for k, v := range extraSelf {
		sf[k] = v
	}
	return map[string]interface{}{
		"id":                     id,
		"name":                   "Reward " + id,
		"requiredMinutesWatched": required,
		"self":                   sf,
	}
}

func campaignWithDrop(dropID string, required int) *models.Campaign {
	return &models.Campaign{
		ID:    "camp-1",
		Drops: []*models.Drop{{ID: dropID, Name: "Reward " + dropID, MinutesRequired: required}},
	}
}

func trackerWithHook(t *testing.T, client twitchClient) (*DropsTracker, *int) {
	t.Helper()
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	successes := 0
	tr.SetDropClaimedHook(func(string) { successes++ })
	return tr, &successes
}

// TestSyncDropsClaimGatingMatrix drives Campaign.SyncDrops through the tracker's
// real claim callback and asserts the number of claim calls, the number of
// user-facing success events, and the drop's reconciled IsClaimed state for each
// authoritative outcome. This is the exactly-once / no-false-success core.
func TestSyncDropsClaimGatingMatrix(t *testing.T) {
	cases := []struct {
		name           string
		extraSelf      map[string]interface{}
		status         api.ClaimStatus
		err            error
		wantCalls      int
		wantSuccesses  int
		wantClaimedSet bool
	}{
		{
			name:      "no authoritative signal (no instance) -> never calls claim",
			extraSelf: map[string]interface{}{}, // local minutes complete, but no instance
			status:    api.ClaimStatusAccepted,
			wantCalls: 0, wantSuccesses: 0, wantClaimedSet: false,
		},
		{
			name:      "server isClaimable=false over local 100% -> never calls claim",
			extraSelf: map[string]interface{}{"dropInstanceID": "inst-1", "isClaimable": false},
			status:    api.ClaimStatusAccepted,
			wantCalls: 0, wantSuccesses: 0, wantClaimedSet: false,
		},
		{
			name:      "hasPreconditionsMet=false blocks -> never calls claim",
			extraSelf: map[string]interface{}{"dropInstanceID": "inst-1", "hasPreconditionsMet": false},
			status:    api.ClaimStatusAccepted,
			wantCalls: 0, wantSuccesses: 0, wantClaimedSet: false,
		},
		{
			name:      "fresh accept -> exactly one claim, one success, reconciled claimed",
			extraSelf: map[string]interface{}{"dropInstanceID": "inst-1"},
			status:    api.ClaimStatusAccepted,
			wantCalls: 1, wantSuccesses: 1, wantClaimedSet: true,
		},
		{
			name:      "already-claimed -> one claim, NO success event, reconciled claimed",
			extraSelf: map[string]interface{}{"dropInstanceID": "inst-1"},
			status:    api.ClaimStatusAlreadyClaimed,
			wantCalls: 1, wantSuccesses: 0, wantClaimedSet: true,
		},
		{
			name:      "rejected -> one claim, no success, NOT claimed (retryable)",
			extraSelf: map[string]interface{}{"dropInstanceID": "inst-1"},
			status:    api.ClaimStatusRejected,
			wantCalls: 1, wantSuccesses: 0, wantClaimedSet: false,
		},
		{
			name:      "transient error -> one claim attempt, no success, NOT claimed",
			extraSelf: map[string]interface{}{"dropInstanceID": "inst-1"},
			status:    api.ClaimStatus(""),
			err:       errors.New("boom"),
			wantCalls: 1, wantSuccesses: 0, wantClaimedSet: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			client := &statusClient{fakeDropsClient: &fakeDropsClient{}, status: tc.status, err: tc.err}
			tr, successes := trackerWithHook(t, client)

			camp := campaignWithDrop("d1", 60)
			camp.SyncDrops([]interface{}{invEntry("d1", 60, 60, tc.extraSelf)}, tr.claimDropFn())

			if got := client.callCount(); got != tc.wantCalls {
				t.Errorf("claim calls = %d, want %d", got, tc.wantCalls)
			}
			if *successes != tc.wantSuccesses {
				t.Errorf("success events = %d, want %d", *successes, tc.wantSuccesses)
			}
			if got := camp.Drops[0].IsClaimed; got != tc.wantClaimedSet {
				t.Errorf("IsClaimed = %v, want %v", got, tc.wantClaimedSet)
			}
		})
	}
}

// TestRepeatedSyncNoDuplicateSuccess proves a repeated inventory sync does not
// re-claim or re-emit a success event once Twitch reports the drop claimed.
func TestRepeatedSyncNoDuplicateSuccess(t *testing.T) {
	client := &statusClient{fakeDropsClient: &fakeDropsClient{}, status: api.ClaimStatusAccepted}
	tr, successes := trackerWithHook(t, client)
	camp := campaignWithDrop("d1", 60)

	// Sync 1: minted instance, unclaimed -> one fresh claim + one success event.
	camp.SyncDrops([]interface{}{invEntry("d1", 60, 60, map[string]interface{}{"dropInstanceID": "inst-1"})}, tr.claimDropFn())
	// Sync 2: Twitch now reports it claimed -> no second claim, no second event.
	camp.SyncDrops([]interface{}{invEntry("d1", 60, 60, map[string]interface{}{"dropInstanceID": "inst-1", "isClaimed": true})}, tr.claimDropFn())
	// Sync 3 (repeat of the claimed state): still idempotent.
	camp.SyncDrops([]interface{}{invEntry("d1", 60, 60, map[string]interface{}{"dropInstanceID": "inst-1", "isClaimed": true})}, tr.claimDropFn())

	if client.callCount() != 1 {
		t.Errorf("expected exactly one claim across repeated syncs, got %d", client.callCount())
	}
	if *successes != 1 {
		t.Errorf("expected exactly one success event across repeated syncs, got %d", *successes)
	}
}

// TestAlreadyClaimedReconciliationNoEvent isolates invariant: an authoritative
// already-claimed response reconciles local state to claimed but never emits a
// user-facing success event.
func TestAlreadyClaimedReconciliationNoEvent(t *testing.T) {
	client := &statusClient{fakeDropsClient: &fakeDropsClient{}, status: api.ClaimStatusAlreadyClaimed}
	tr, successes := trackerWithHook(t, client)
	camp := campaignWithDrop("d1", 60)

	camp.SyncDrops([]interface{}{invEntry("d1", 60, 60, map[string]interface{}{"dropInstanceID": "inst-1"})}, tr.claimDropFn())

	if client.callCount() != 1 {
		t.Errorf("already-claimed must still issue exactly one mutation, got %d", client.callCount())
	}
	if *successes != 0 {
		t.Errorf("already-claimed reconciliation must not emit a success event, got %d", *successes)
	}
	if !camp.Drops[0].IsClaimed {
		t.Error("already-claimed must reconcile local IsClaimed to true")
	}
}

// TestLightweightProgressSyncNeverClaims proves the hot progress path never
// issues a claim mutation, even for a drop Twitch reports as claimable — the
// only claiming paths are the full sync / inventory sweep, so concurrent
// progress syncs cannot trigger duplicate claims.
func TestLightweightProgressSyncNeverClaims(t *testing.T) {
	claimable := map[string]interface{}{
		"id":   "camp-1",
		"name": "Camp",
		"game": map[string]interface{}{"id": "g1", "name": "Game"},
		"timeBasedDrops": []interface{}{
			invEntry("d1", 60, 60, map[string]interface{}{"dropInstanceID": "inst-1"}),
		},
	}
	client := &statusClient{
		fakeDropsClient: &fakeDropsClient{inventory: inventoryWithInProgress(claimable)},
		status:          api.ClaimStatusAccepted,
	}
	tr, successes := trackerWithHook(t, client)
	// Seed a tracked campaign so syncProgress has something to refresh.
	tr.campaigns = []*models.Campaign{campaignWithDrop("d1", 60)}

	tr.syncProgress()

	if client.callCount() != 0 {
		t.Errorf("lightweight progress sync must never claim, got %d claim calls", client.callCount())
	}
	if *successes != 0 {
		t.Errorf("lightweight progress sync must not emit success events, got %d", *successes)
	}
}
