package models

import "testing"

func TestHasChannelRestrictedCampaign(t *testing.T) {
	s := NewStreamer("teststreamer", DefaultStreamerSettings())

	if s.HasChannelRestrictedCampaign() {
		t.Error("streamer with no campaigns should not have a channel-restricted campaign")
	}

	s.Stream.Campaigns = []*Campaign{{ID: "unrestricted"}}
	if s.HasChannelRestrictedCampaign() {
		t.Error("streamer with only unrestricted campaigns should not report a channel-restricted campaign")
	}

	s.Stream.Campaigns = append(s.Stream.Campaigns, &Campaign{ID: "restricted", Channels: []string{"channel-1"}})
	if !s.HasChannelRestrictedCampaign() {
		t.Error("streamer with a channel-restricted campaign should report it")
	}
}
