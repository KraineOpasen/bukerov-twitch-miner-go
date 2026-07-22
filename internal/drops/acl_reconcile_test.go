package drops

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// inProgressWithAllow builds an inventory dropCampaignsInProgress entry with a
// caller-supplied raw `allow` value (map, malformed, or nil-for-absent), so a
// full sync can be driven through NewCampaignFromGQL's ACL parsing.
func inProgressWithAllow(id, gameID string, allow interface{}) map[string]interface{} {
	m := map[string]interface{}{
		"id":             id,
		"name":           "Camp " + id,
		"game":           map[string]interface{}{"id": gameID, "displayName": "Game"},
		"timeBasedDrops": []interface{}{inProgressDrop("d-"+id, "Reward "+id, 120, 30, false)},
	}
	if allow != nil {
		m["allow"] = allow
	}
	return m
}

func restrictedAllow(channelIDs ...string) map[string]interface{} {
	chans := make([]interface{}, 0, len(channelIDs))
	for _, c := range channelIDs {
		chans = append(chans, map[string]interface{}{"id": c})
	}
	return map[string]interface{}{"channels": chans}
}

func trackedByID(tr *DropsTracker, id string) *models.Campaign {
	for _, c := range tr.Campaigns() {
		if c.ID == id {
			return c
		}
	}
	return nil
}

// B5.1: a fresh sync whose ACL is UNKNOWN (malformed) must not erode the
// previously-published known-good restricted ACL — the partial/unknown list is
// never published as the allowlist.
func TestSyncReconcileUnknownPreservesKnownGoodACL(t *testing.T) {
	client := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: inventoryWithInProgress(inProgressWithAllow("camp-1", "g1", restrictedAllow("chan-1"))),
	}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.syncCampaigns()

	c := trackedByID(tr, "camp-1")
	if c == nil || c.ACLState() != models.ACLRestricted || !c.AllowsChannel("chan-1") {
		t.Fatalf("first sync should publish a restricted ACL allowing chan-1, got %+v", c)
	}

	// Second sync: the allow block is malformed (isEnabled wrong type) -> fresh
	// ACL is Unknown. It must NOT overwrite the known-good restricted ACL.
	client.inventory = inventoryWithInProgress(inProgressWithAllow("camp-1", "g1",
		map[string]interface{}{"isEnabled": "broken", "channels": restrictedAllow("chan-1")["channels"]}))
	tr.syncCampaigns()

	c = trackedByID(tr, "camp-1")
	if c == nil {
		t.Fatal("campaign should still be tracked")
	}
	if c.ACLState() != models.ACLRestricted || !c.AllowsChannel("chan-1") {
		t.Fatalf("unknown ACL must preserve the previous known-good restricted ACL, got state=%s allows=%v",
			c.ACLState(), c.AllowsChannel("chan-1"))
	}
}

// B5.2: an authoritative isEnabled=false in a fresh sync clears the stale
// restricted list (unrestricted wins).
func TestSyncReconcileDisabledClearsStaleACL(t *testing.T) {
	client := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: inventoryWithInProgress(inProgressWithAllow("camp-1", "g1", restrictedAllow("chan-1"))),
	}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.syncCampaigns()
	if c := trackedByID(tr, "camp-1"); c == nil || c.ACLState() != models.ACLRestricted {
		t.Fatalf("first sync should be restricted, got %+v", c)
	}

	client.inventory = inventoryWithInProgress(inProgressWithAllow("camp-1", "g1",
		map[string]interface{}{"isEnabled": false, "channels": restrictedAllow("chan-1")["channels"]}))
	tr.syncCampaigns()

	c := trackedByID(tr, "camp-1")
	if c == nil || c.ACLState() != models.ACLUnrestricted {
		t.Fatalf("isEnabled=false must clear to unrestricted, got %+v", c)
	}
	if !c.AllowsChannel("any-other-channel") {
		t.Fatal("unrestricted campaign must allow any channel after disable")
	}
	if len(c.Channels) != 0 {
		t.Fatalf("stale channel mirror must be cleared, got %v", c.Channels)
	}
}

// B5.3: a fresh COMPLETE restricted set replaces the previous one.
func TestSyncReconcileCompleteRestrictedReplaces(t *testing.T) {
	client := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: inventoryWithInProgress(inProgressWithAllow("camp-1", "g1", restrictedAllow("chan-1"))),
	}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.syncCampaigns()

	client.inventory = inventoryWithInProgress(inProgressWithAllow("camp-1", "g1", restrictedAllow("chan-2")))
	tr.syncCampaigns()

	c := trackedByID(tr, "camp-1")
	if c == nil || c.AllowsChannel("chan-1") || !c.AllowsChannel("chan-2") {
		t.Fatalf("a fresh complete restricted set must replace the old one, got %+v", c)
	}
}

// B5.4: a campaign that disappears from the fresh set is removed — we never keep
// it alive just to preserve its ACL.
func TestSyncReconcileDisappearedCampaignRemoved(t *testing.T) {
	client := &fakeDropsClient{
		dashboard: dashboardResponse(),
		inventory: inventoryWithInProgress(inProgressWithAllow("camp-1", "g1", restrictedAllow("chan-1"))),
	}
	tr := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tr.syncCampaigns()
	if trackedByID(tr, "camp-1") == nil {
		t.Fatal("camp-1 should be tracked after first sync")
	}

	client.inventory = emptyInventoryResponse()
	tr.syncCampaigns()
	if trackedByID(tr, "camp-1") != nil {
		t.Fatal("disappeared campaign must be removed, not preserved for its ACL")
	}
}
