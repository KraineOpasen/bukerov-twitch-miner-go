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

func TestActiveCampaignProgressPicksFurthestAlong(t *testing.T) {
	s := NewStreamer("teststreamer", DefaultStreamerSettings())

	if s.ActiveCampaignProgress() != nil {
		t.Error("streamer with no campaigns should report no active campaign progress")
	}

	s.Stream.Campaigns = []*Campaign{
		{
			Name: "Behind",
			Game: &Game{Name: "Game A"},
			Drops: []*Drop{
				{Name: "Reward A", MinutesRequired: 100, CurrentMinutesWatched: 10},
			},
		},
		{
			Name: "Ahead",
			Game: &Game{Name: "Game B"},
			Drops: []*Drop{
				{Name: "Reward B", MinutesRequired: 100, CurrentMinutesWatched: 80},
			},
		},
	}

	prog := s.ActiveCampaignProgress()
	if prog == nil {
		t.Fatal("expected active campaign progress")
	}
	if prog.CampaignName != "Ahead" {
		t.Errorf("expected the furthest-along campaign (Ahead), got %q", prog.CampaignName)
	}
	if prog.Percent != 80 {
		t.Errorf("expected 80%% progress, got %d", prog.Percent)
	}
	if prog.DropName != "Reward B" || prog.MinutesRequired != 100 {
		t.Errorf("unexpected drop details: %+v", prog)
	}
}
