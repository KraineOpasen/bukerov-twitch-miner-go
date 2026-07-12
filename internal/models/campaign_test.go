package models

import "testing"

func TestCampaignIsChannelRestricted(t *testing.T) {
	unrestricted := &Campaign{}
	if unrestricted.IsChannelRestricted() {
		t.Error("campaign with no Channels should not be considered channel-restricted")
	}

	restricted := &Campaign{Channels: []string{"channel-1"}}
	if !restricted.IsChannelRestricted() {
		t.Error("campaign with a non-empty Channels list should be considered channel-restricted")
	}
}

func TestCampaignAllowsChannel(t *testing.T) {
	unrestricted := &Campaign{}
	if !unrestricted.AllowsChannel("any-channel") {
		t.Error("unrestricted campaign should allow any channel")
	}

	restricted := &Campaign{Channels: []string{"channel-1", "channel-2"}}
	if !restricted.AllowsChannel("channel-1") {
		t.Error("restricted campaign should allow a channel in its list")
	}
	if restricted.AllowsChannel("channel-3") {
		t.Error("restricted campaign should not allow a channel outside its list")
	}
}

func TestNewCampaignFromGQLParsesAllowedChannels(t *testing.T) {
	data := map[string]interface{}{
		"id":     "campaign-1",
		"name":   "Test Campaign",
		"status": "ACTIVE",
		"allow": map[string]interface{}{
			"channels": []interface{}{
				map[string]interface{}{"id": "channel-1"},
				map[string]interface{}{"id": "channel-2"},
			},
		},
	}

	c := NewCampaignFromGQL(data)
	if !c.IsChannelRestricted() {
		t.Fatal("expected campaign with allow.channels to be channel-restricted")
	}
	if len(c.Channels) != 2 || c.Channels[0] != "channel-1" || c.Channels[1] != "channel-2" {
		t.Errorf("unexpected Channels: %v", c.Channels)
	}
}

func TestCampaignApplyClaimHistoryStripsMatchedDrops(t *testing.T) {
	c := &Campaign{
		ID:   "campaign-regional-eu",
		Name: "Cool Skin Drop",
		Game: &Game{ID: "game-1"},
		Drops: []*Drop{
			{ID: "drop-a", Name: "Legendary Skin"},
			{ID: "drop-b", Name: "Emote Pack"},
		},
	}

	claimed := map[string]bool{
		NormalizeRewardKey("game-1", "Legendary Skin"): true,
	}

	c.ApplyClaimHistory(claimed)

	if len(c.Drops) != 1 || c.Drops[0].Name != "Emote Pack" {
		t.Fatalf("expected only unclaimed drop to remain, got %+v", c.Drops)
	}
	if len(c.ClaimedDropNames) != 1 || c.ClaimedDropNames[0] != "Legendary Skin" {
		t.Errorf("expected ClaimedDropNames to record the skipped drop, got %v", c.ClaimedDropNames)
	}
	if c.ClaimStatus != CampaignClaimStatusInProgress {
		t.Errorf("campaign still has an unclaimed drop, expected in_progress, got %s", c.ClaimStatus)
	}
}

func TestCampaignApplyClaimHistoryMarksFullyClaimed(t *testing.T) {
	c := &Campaign{
		ID:   "campaign-regional-na",
		Name: "Cool Skin Drop",
		Game: &Game{ID: "game-1"},
		Drops: []*Drop{
			{ID: "drop-a-na", Name: "Legendary Skin"},
		},
	}

	// Same game + reward name as a different campaign/drop ID: a
	// recurring/regional variant of an already-claimed campaign.
	claimed := map[string]bool{
		NormalizeRewardKey("game-1", "Legendary Skin"): true,
	}

	c.ApplyClaimHistory(claimed)

	if len(c.Drops) != 0 {
		t.Fatalf("expected all drops to be stripped, got %+v", c.Drops)
	}
	if c.ClaimStatus != CampaignClaimStatusAlreadyClaimed {
		t.Errorf("expected already_claimed status, got %s", c.ClaimStatus)
	}
}

func TestNormalizeRewardKeyIgnoresCaseAndWhitespace(t *testing.T) {
	a := NormalizeRewardKey("Game-1", "  Legendary Skin ")
	b := NormalizeRewardKey("game-1", "legendary skin")
	if a != b {
		t.Errorf("expected normalized keys to match regardless of case/whitespace: %q != %q", a, b)
	}
}

func TestCampaignCurrentDropPicksNextUnmetMilestone(t *testing.T) {
	c := &Campaign{
		Drops: []*Drop{
			{Name: "Tier 3", MinutesRequired: 180, CurrentMinutesWatched: 40},
			{Name: "Tier 1", MinutesRequired: 60, CurrentMinutesWatched: 40},
			{Name: "Tier 2", MinutesRequired: 120, CurrentMinutesWatched: 40},
		},
	}

	current := c.CurrentDrop()
	if current == nil || current.Name != "Tier 1" {
		t.Fatalf("expected the lowest unmet milestone (Tier 1), got %+v", current)
	}
}

func TestCampaignCurrentDropFallsBackToFinalWhenAllMet(t *testing.T) {
	c := &Campaign{
		Drops: []*Drop{
			{Name: "Tier 1", MinutesRequired: 60, CurrentMinutesWatched: 60},
			{Name: "Tier 2", MinutesRequired: 120, CurrentMinutesWatched: 130},
		},
	}

	current := c.CurrentDrop()
	if current == nil || current.Name != "Tier 2" {
		t.Fatalf("expected fallback to the furthest milestone (Tier 2), got %+v", current)
	}
}

func TestCampaignCurrentDropNilWhenNoDrops(t *testing.T) {
	c := &Campaign{}
	if c.CurrentDrop() != nil {
		t.Error("campaign with no drops should have no current drop")
	}
}

func TestCampaignOverallProgressPercent(t *testing.T) {
	c := &Campaign{
		Drops: []*Drop{
			{MinutesRequired: 60, CurrentMinutesWatched: 60},
			{MinutesRequired: 120, CurrentMinutesWatched: 30},
		},
	}
	// Measured against the furthest milestone: 30/120 = 25%.
	if pct := c.OverallProgressPercent(); pct != 25 {
		t.Errorf("expected 25%%, got %d", pct)
	}

	claimed := &Campaign{ClaimStatus: CampaignClaimStatusAlreadyClaimed}
	if pct := claimed.OverallProgressPercent(); pct != 100 {
		t.Errorf("already-claimed campaign should report 100%%, got %d", pct)
	}
}

func TestNewCampaignFromGQLNoAllowMeansUnrestricted(t *testing.T) {
	data := map[string]interface{}{
		"id":     "campaign-1",
		"name":   "Test Campaign",
		"status": "ACTIVE",
	}

	c := NewCampaignFromGQL(data)
	if c.IsChannelRestricted() {
		t.Error("campaign with no allow field should not be channel-restricted")
	}
}
