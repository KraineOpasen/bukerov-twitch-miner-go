package models

import (
	"testing"
	"time"
)

func gqlCampaign(allow interface{}) *Campaign {
	data := map[string]interface{}{"id": "camp-1", "name": "C", "status": "ACTIVE"}
	if allow != nil {
		data["allow"] = allow
	}
	return NewCampaignFromGQL(data)
}

func channelList(ids ...string) []interface{} {
	out := make([]interface{}, 0, len(ids))
	for _, id := range ids {
		out = append(out, map[string]interface{}{"id": id})
	}
	return out
}

// 1: no allow node -> unrestricted (and backward-compatible Channels empty).
func TestACLNoAllowUnrestricted(t *testing.T) {
	c := gqlCampaign(nil)
	if c.ACLState() != ACLUnrestricted {
		t.Fatalf("want unrestricted, got %s", c.ACLState())
	}
	if c.IsChannelRestricted() {
		t.Error("no-allow campaign must not be restricted")
	}
	if !c.AllowsChannel("anything") {
		t.Error("unrestricted campaign should allow any channel")
	}
}

// 2 & 3: restricted with channels, duplicates deduped, deterministic order.
func TestACLRestrictedDedupAndOrder(t *testing.T) {
	c := gqlCampaign(map[string]interface{}{
		"channels": channelList("c2", "c1", "c2", "c1", "c3"),
	})
	if c.ACLState() != ACLRestricted {
		t.Fatalf("want restricted, got %s", c.ACLState())
	}
	got := c.ACL.ChannelIDs
	want := []string{"c1", "c2", "c3"}
	if len(got) != len(want) {
		t.Fatalf("dedup failed: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order not deterministic: %v", got)
		}
	}
	// Legacy Channels mirror is populated.
	if len(c.Channels) != 3 {
		t.Errorf("legacy Channels mirror wrong: %v", c.Channels)
	}
}

// 6 & 22: isEnabled=false -> unrestricted, clearing any channel list.
func TestACLDisabledClearsChannels(t *testing.T) {
	c := gqlCampaign(map[string]interface{}{
		"isEnabled": false,
		"channels":  channelList("c1", "c2"),
	})
	if c.ACLState() != ACLUnrestricted {
		t.Fatalf("isEnabled=false must be unrestricted, got %s", c.ACLState())
	}
	if len(c.ACL.ChannelIDs) != 0 || len(c.Channels) != 0 {
		t.Errorf("disabled ACL must carry no channels: acl=%v legacy=%v", c.ACL.ChannelIDs, c.Channels)
	}
	if !c.AllowsChannel("random") {
		t.Error("disabled (unrestricted) ACL should allow any channel")
	}
}

// isEnabled=true with a valid list -> restricted.
func TestACLEnabledWithChannelsRestricted(t *testing.T) {
	c := gqlCampaign(map[string]interface{}{
		"isEnabled": true,
		"channels":  channelList("c1"),
	})
	if c.ACLState() != ACLRestricted {
		t.Fatalf("want restricted, got %s", c.ACLState())
	}
}

// 7: isEnabled true + missing channels -> unknown (not unrestricted).
func TestACLEnabledMissingChannelsUnknown(t *testing.T) {
	c := gqlCampaign(map[string]interface{}{"isEnabled": true})
	if c.ACLState() != ACLUnknown {
		t.Fatalf("enabled+missing channels must be unknown, got %s", c.ACLState())
	}
	if c.AllowsChannel("c1") {
		t.Error("unknown ACL must fail closed (allow nobody)")
	}
	if c.IsChannelRestricted() {
		t.Error("unknown ACL is not reported as restricted")
	}
}

// 8: malformed allow -> unknown.
func TestACLMalformedUnknown(t *testing.T) {
	c := gqlCampaign("not-an-object")
	if c.ACLState() != ACLUnknown {
		t.Fatalf("malformed allow must be unknown, got %s", c.ACLState())
	}
	if c.AllowsChannel("c1") {
		t.Error("unknown ACL must fail closed")
	}
}

// enabled + empty channel list -> unknown (incomplete), not restricted-to-nobody.
func TestACLEnabledEmptyChannelsUnknown(t *testing.T) {
	c := gqlCampaign(map[string]interface{}{"isEnabled": true, "channels": []interface{}{}})
	if c.ACLState() != ACLUnknown {
		t.Fatalf("enabled+empty channels must be unknown, got %s", c.ACLState())
	}
}

// A malformed channel element makes the WHOLE ACL unknown (fail closed): the
// valid subset must never be usable as a complete allowlist, so no channel is
// creditable from a partially-parsed list.
func TestACLMalformedElementFailsClosed(t *testing.T) {
	c := gqlCampaign(map[string]interface{}{
		"channels": []interface{}{
			map[string]interface{}{"id": "c1"},
			map[string]interface{}{"noid": true},
			"garbage",
		},
	})
	if c.ACLState() != ACLUnknown {
		t.Fatalf("a malformed element must yield ACLUnknown, got %s", c.ACLState())
	}
	if c.ACL.Complete {
		t.Error("a malformed ACL must not be marked complete")
	}
	if c.AllowsChannel("c1") {
		t.Error("no channel from a partially-parsed list may be credited (fail closed)")
	}
}

// A present-but-malformed isEnabled (wrong type) is unknown, not silently
// treated as absent.
func TestACLMalformedIsEnabledFailsClosed(t *testing.T) {
	c := gqlCampaign(map[string]interface{}{
		"isEnabled": "yes", // not a bool
		"channels":  channelList("c1"),
	})
	if c.ACLState() != ACLUnknown {
		t.Fatalf("malformed isEnabled must yield ACLUnknown, got %s", c.ACLState())
	}
	if c.AllowsChannel("c1") {
		t.Error("unknown ACL must fail closed")
	}
}

// 15 & 16 & 21: channel outside a restricted ACL is rejected; unknown never widens.
func TestACLAllowsChannelMatrix(t *testing.T) {
	restricted := gqlCampaign(map[string]interface{}{"channels": channelList("c1", "c2")})
	if !restricted.AllowsChannel("c1") {
		t.Error("channel in ACL should be allowed")
	}
	if restricted.AllowsChannel("c3") {
		t.Error("channel outside ACL must be rejected (even if locally configured)")
	}

	unknown := gqlCampaign(map[string]interface{}{"isEnabled": true})
	if unknown.AllowsChannel("c1") {
		t.Error("ACLUnknown must never widen eligibility")
	}
}

// 24: AllowsChannel is independent of the streamer's advertised campaign IDs —
// the ACL answers a different question than GetCampaignIDsFromStreamer.
func TestACLIndependentOfChannelSideAvailability(t *testing.T) {
	// A restricted campaign only consults its allowlist; it has no notion of
	// what a channel currently advertises.
	c := &Campaign{ACL: CampaignACL{State: ACLRestricted, ChannelIDs: []string{"c1"}, Source: ACLSourceCampaignDetails, Complete: true}}
	if !c.AllowsChannel("c1") || c.AllowsChannel("c2") {
		t.Fatal("AllowsChannel must depend only on the ACL membership")
	}
}

// 5 & 12: ReconcileACL preserves last-known-good on unknown, and a stale older
// observation never overwrites a newer one; an authoritative disabled wins.
func TestReconcileACLLifecycle(t *testing.T) {
	t0 := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)

	published := CampaignACL{State: ACLRestricted, ChannelIDs: []string{"c1"}, Complete: true, ObservedAt: t1, Source: ACLSourceCampaignDetails}

	// Incoming unknown (transport failure) -> keep last-known-good.
	unknown := CampaignACL{State: ACLUnknown, ObservedAt: t1.Add(time.Minute), Source: ACLSourceCampaignDetails}
	if got := ReconcileACL(published, unknown); got.State != ACLRestricted {
		t.Error("unknown must not erode a known-good ACL")
	}

	// Stale older restricted result -> discarded.
	stale := CampaignACL{State: ACLRestricted, ChannelIDs: []string{"c9"}, Complete: true, ObservedAt: t0, Source: ACLSourceCampaignDetails}
	if got := ReconcileACL(published, stale); got.ChannelIDs[0] != "c1" {
		t.Error("stale older ACL must not overwrite a newer one")
	}

	// Incomplete restricted -> not published over a complete one.
	incomplete := CampaignACL{State: ACLRestricted, ChannelIDs: []string{"c1", "c2"}, Complete: false, ObservedAt: t1.Add(time.Minute), Source: ACLSourceCampaignDetails}
	if got := ReconcileACL(published, incomplete); got.Complete != true || len(got.ChannelIDs) != 1 {
		t.Error("incomplete ACL must not replace a complete one")
	}

	// Authoritative disabled (newer) -> wins and clears.
	disabled := CampaignACL{State: ACLUnrestricted, Complete: true, ObservedAt: t1.Add(time.Minute), Source: ACLSourceCampaignDetails}
	if got := ReconcileACL(published, disabled); got.State != ACLUnrestricted {
		t.Error("authoritative disabled must override a stale restricted list")
	}
}

// Directly-constructed campaigns (legacy) still honor the Channels slice.
func TestACLLegacyChannelsFallback(t *testing.T) {
	legacy := &Campaign{Channels: []string{"c1"}}
	if !legacy.IsChannelRestricted() || !legacy.AllowsChannel("c1") || legacy.AllowsChannel("c2") {
		t.Fatal("legacy Channels-only campaign must behave as restricted")
	}
	empty := &Campaign{}
	if empty.IsChannelRestricted() || !empty.AllowsChannel("any") {
		t.Fatal("empty legacy campaign must behave as unrestricted")
	}
}
