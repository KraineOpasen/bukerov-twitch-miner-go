package watcher

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestSelectByPriorityFavorsChannelRestrictedDrop covers the case where more
// drops-eligible streamers are online than fit in the watch slots: a
// streamer holding a channel-restricted campaign (progress only countable on
// this exact channel) must win a slot over streamers that only have
// unrestricted campaigns (whose progress could in principle also be earned
// by watching a different configured streamer).
func TestSelectByPriorityFavorsChannelRestrictedDrop(t *testing.T) {
	w, online := newTestWatcher(3)
	w.priorities = []config.Priority{config.PriorityDrops}

	for _, idx := range online {
		w.streamers[idx].IsOnline = true
		w.streamers[idx].Stream.CampaignIDs = []string{"campaign-unrestricted"}
	}

	// Only streamer 2 also holds a channel-restricted campaign.
	w.streamers[2].Stream.Campaigns = []*models.Campaign{
		{ID: "campaign-restricted", Channels: []string{w.streamers[2].ChannelID}},
	}

	watching := w.selectByPriority(online)

	found := false
	for _, idx := range watching {
		if idx == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the channel-restricted-campaign streamer (index 2) to be selected, got %v", watching)
	}
}
