package watcher

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// dropsEligible marks a configured test streamer as a DROPS-priority candidate
// (online, claim-drops on, carrying a campaign).
func dropsEligible(w *MinuteWatcher, idx int) {
	s := w.streamers[idx]
	s.Settings.ClaimDrops = true
	s.SetOnline()
	s.OnlineAt = time.Now().Add(-time.Minute)
	s.Stream.SetCampaignIDs([]string{"camp-" + s.Username})
}

// TestDropsPriorityHonorsCampaignScores verifies the policy tie-break: with
// more DROPS-eligible streamers than slots, the published campaign scores
// decide which ones fill the two slots — and with no scores published the
// configured order is preserved (bit-identical to pre-policy behavior).
func TestDropsPriorityHonorsCampaignScores(t *testing.T) {
	w, _ := newTestWatcher(3)
	w.priorities = []config.Priority{config.PriorityDrops}
	online := []int{}
	for i := 0; i < 3; i++ {
		dropsEligible(w, i)
		online = append(online, i)
	}

	// No scores: the first two configured streamers win (unchanged order).
	got := w.selectByPriority(online)
	if len(got) != 2 || !contains(got, 0) || !contains(got, 1) {
		t.Fatalf("without scores, expected the first two configured streamers [0 1], got %v", got)
	}

	// Publish scores favoring streamers 2 and 1 over 0.
	w.SetCampaignScores(map[string]int{
		w.streamers[0].Username: 10,
		w.streamers[1].Username: 50,
		w.streamers[2].Username: 90,
	})
	got = w.selectByPriority(online)
	if len(got) != 2 || !contains(got, 2) || !contains(got, 1) {
		t.Fatalf("with scores, expected the two highest-scored streamers [2 1], got %v", got)
	}
	if contains(got, 0) {
		t.Fatalf("lowest-scored streamer 0 must not be selected, got %v", got)
	}

	// Clearing the scores restores the configured order exactly.
	w.SetCampaignScores(nil)
	got = w.selectByPriority(online)
	if len(got) != 2 || !contains(got, 0) || !contains(got, 1) {
		t.Fatalf("after clearing scores, expected [0 1] again, got %v", got)
	}
}

// TestDropsRestrictedStillFirstUnderScores confirms the restricted-first
// invariant survives the policy tie-break: with 3 eligible streamers and 2
// slots, a channel-restricted campaign is picked even when two unrestricted
// campaigns carry a higher raw score.
func TestDropsRestrictedStillFirstUnderScores(t *testing.T) {
	w, _ := newTestWatcher(3)
	w.priorities = []config.Priority{config.PriorityDrops}
	for i := 0; i < 3; i++ {
		dropsEligible(w, i)
	}
	// Streamer 2 holds a channel-restricted campaign (Channels non-empty).
	w.streamers[2].Stream.SetCampaigns([]*models.Campaign{
		{ID: "camp-" + w.streamers[2].Username, Channels: []string{w.streamers[2].ChannelID}},
	})
	w.SetCampaignScores(map[string]int{
		w.streamers[0].Username: 100,
		w.streamers[1].Username: 90,
		w.streamers[2].Username: 1, // low score, but restricted
	})
	got := w.selectByPriority([]int{0, 1, 2})
	if !contains(got, 2) {
		t.Fatalf("restricted streamer 2 must be selected regardless of score, got %v", got)
	}
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
