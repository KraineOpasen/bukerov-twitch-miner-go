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
