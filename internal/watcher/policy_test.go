package watcher

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
)

// dropsEligible marks a configured test streamer as a DROPS-priority candidate
// (online, claim-drops on, carrying a campaign).
func dropsEligible(w *MinuteWatcher, idx int) {
	s := w.streamers[idx]
	s.Settings.ClaimDrops = true
	s.SetConfirmedOnline()
	s.OnlineAt = time.Now().Add(-time.Minute)
	s.Stream.SetCampaignIDs([]string{"camp-" + s.Username})
}

// TestLegacyCampaignScoreCompatibilityIsAllocationInactive keeps the restored
// session.go compatibility helper covered without restoring it as a scheduling
// caller. The legacy helper can still order its explicit input, but the actual
// Campaign Policy ordering path consults only typed semantic classes and is
// therefore independent of raw SMART points.
func TestLegacyCampaignScoreCompatibilityIsAllocationInactive(t *testing.T) {
	w, _ := newTestWatcher(3)
	w.SetCampaignScores(map[string]int{
		w.streamers[0].GetUsername(): -100,
		w.streamers[1].GetUsername(): 0,
		w.streamers[2].GetUsername(): 100,
	})
	legacy := w.orderByCampaignScore([]int{0, 1, 2})
	if len(legacy) != 3 || legacy[0] != 2 {
		t.Fatalf("legacy compatibility ordering = %v, want raw-score leader 2", legacy)
	}

	w.SetCampaignSemanticClasses(map[string]policy.SemanticClass{
		w.streamers[0].GetUsername(): 0,
		w.streamers[1].GetUsername(): 0,
		w.streamers[2].GetUsername(): 0,
	})
	actual := w.orderByCampaignSemanticClass([]int{2, 1, 0})
	if len(actual) != 3 || actual[0] != 0 || actual[1] != 1 || actual[2] != 2 {
		t.Fatalf("actual semantic ordering was influenced by raw scores: %v", actual)
	}
}

// TestDropsPriorityHonorsCampaignClasses verifies the policy tie-break: with
// more DROPS-eligible streamers than slots, the published semantic classes
// decide which ones fill the two slots — and with no classes published the
// configured order is preserved (bit-identical to pre-policy behavior).
func TestDropsPriorityHonorsCampaignClasses(t *testing.T) {
	w, _ := newTestWatcher(3)
	w.priorities = []config.Priority{config.PriorityDrops}
	online := []int{}
	for i := 0; i < 3; i++ {
		dropsEligible(w, i)
		online = append(online, i)
	}

	// No semantic facts: the first two configured streamers win (unchanged order).
	got := w.selectByPriority(online)
	if len(got) != 2 || !contains(got, 0) || !contains(got, 1) {
		t.Fatalf("without scores, expected the first two configured streamers [0 1], got %v", got)
	}

	// Publish classes favoring streamers 2 and 1 over 0.
	w.SetCampaignSemanticClasses(map[string]policy.SemanticClass{
		w.streamers[0].Username: 2,
		w.streamers[1].Username: 1,
		w.streamers[2].Username: 0,
	})
	got = w.selectByPriority(online)
	if len(got) != 2 || !contains(got, 2) || !contains(got, 1) {
		t.Fatalf("with scores, expected the two highest-scored streamers [2 1], got %v", got)
	}
	if contains(got, 0) {
		t.Fatalf("lowest-scored streamer 0 must not be selected, got %v", got)
	}

	// Clearing the scores restores the configured order exactly.
	w.SetCampaignSemanticClasses(nil)
	got = w.selectByPriority(online)
	if len(got) != 2 || !contains(got, 0) || !contains(got, 1) {
		t.Fatalf("after clearing scores, expected [0 1] again, got %v", got)
	}
}

// TestDropsRestrictedStillFirstUnderClasses confirms the restricted-first
// invariant survives the policy tie-break: with 3 eligible streamers and 2
// slots, a channel-restricted campaign is picked even when two unrestricted
// campaigns carry a better semantic class.
func TestDropsRestrictedStillFirstUnderClasses(t *testing.T) {
	w, _ := newTestWatcher(3)
	w.priorities = []config.Priority{config.PriorityDrops}
	for i := 0; i < 3; i++ {
		dropsEligible(w, i)
	}
	// Streamer 2 holds a channel-restricted campaign (Channels non-empty).
	w.streamers[2].Stream.SetCampaigns([]*models.Campaign{
		{ID: "camp-" + w.streamers[2].Username, Channels: []string{w.streamers[2].ChannelID}},
	})
	w.SetCampaignSemanticClasses(map[string]policy.SemanticClass{
		w.streamers[0].Username: 0,
		w.streamers[1].Username: 1,
		w.streamers[2].Username: 9, // weaker class, but restricted
	})
	got := w.selectByPriority([]int{0, 1, 2})
	if !contains(got, 2) {
		t.Fatalf("restricted streamer 2 must be selected regardless of score, got %v", got)
	}
}

// TestCampaignPolicyWinnerReachesBrokerWithPersistedDeficit exercises the
// production watcher seam used after miner.refreshPolicy. All four channels are
// eligible active-drop contenders, but persisted fairness and recency favor a
// different pair than ENDING_SOONEST. The semantic winner must still reach the
// broker's actual two-slot allocation.
func TestCampaignPolicyWinnerReachesBrokerWithPersistedDeficit(t *testing.T) {
	w := newPolicyBrokerWatcher(t)
	now := time.Now()

	inputs := []policy.CampaignInput{
		{CampaignID: "campaign-a", EndAt: now.Add(time.Hour), Drops: []policy.DropStep{{MinutesRequired: 30, CurrentMinutesWatched: 29}}},
		{CampaignID: "campaign-b", EndAt: now.Add(4 * time.Hour), Drops: []policy.DropStep{{MinutesRequired: 30, CurrentMinutesWatched: 29}}},
		{CampaignID: "campaign-c", EndAt: now.Add(8 * time.Hour), Drops: []policy.DropStep{{MinutesRequired: 30, CurrentMinutesWatched: 29}}},
		{CampaignID: "campaign-d", EndAt: now.Add(12 * time.Hour), Drops: []policy.DropStep{{MinutesRequired: 30, CurrentMinutesWatched: 29}}},
	}
	decisions := policy.Rank(policy.ModeEndingSoonest, inputs, now)
	if got := decisions[0].CampaignID; got != "campaign-a" {
		t.Fatalf("test setup: ENDING_SOONEST winner = %q, want campaign-a", got)
	}

	w.SetCampaignSemanticClasses(policyClassesForWatcher(w, decisions))

	seedPolicyBrokerWeights(t, w, now, []float64{100, 0, 1, 200})
	w.rotation.lastWatched = map[int]time.Time{
		0: now.Add(-time.Hour),
		3: now.Add(-2 * time.Hour),
	}
	w.processWatching()

	snap := w.BrokerSnapshot()
	if !brokerHasChannel(snap, w.streamers[0].GetUsername()) {
		t.Fatalf("policy order [campaign-a campaign-b campaign-c campaign-d], broker allocation %v: expected ENDING_SOONEST winner %q; raw totals collapsed and persisted deficit/recency selected another channel", brokerChannels(snap), w.streamers[0].GetUsername())
	}
	if !brokerHasChannel(snap, w.streamers[1].GetUsername()) {
		t.Fatalf("policy winner displaced the most-owed fair seat: allocation %v, want semantic winner %q plus persisted-deficit winner %q", brokerChannels(snap), w.streamers[0].GetUsername(), w.streamers[1].GetUsername())
	}
}

// TestCampaignPolicyEqualClassUsesPersistedDeficit is the control: policy
// facts are exactly equal, so the persisted deficit — not campaign ID or input
// order — owns the allocation.
func TestCampaignPolicyEqualClassUsesPersistedDeficit(t *testing.T) {
	w := newPolicyBrokerWatcher(t)
	now := time.Now()
	endAt := now.Add(12 * time.Hour)
	inputs := make([]policy.CampaignInput, len(w.streamers))
	for i := range inputs {
		inputs[i] = policy.CampaignInput{
			CampaignID: "campaign-" + string(rune('a'+i)),
			EndAt:      endAt,
			Drops:      []policy.DropStep{{MinutesRequired: 30, CurrentMinutesWatched: 29}},
		}
	}
	decisions := policy.Rank(policy.ModeEndingSoonest, inputs, now)
	w.SetCampaignSemanticClasses(policyClassesForWatcher(w, decisions))

	seedPolicyBrokerWeights(t, w, now, []float64{100, 0, 1, 200})
	w.processWatching()

	snap := w.BrokerSnapshot()
	for _, idx := range []int{1, 2} {
		if !brokerHasChannel(snap, w.streamers[idx].GetUsername()) {
			t.Fatalf("equal policy class broker allocation %v: expected persisted-deficit pair [%s %s]", brokerChannels(snap), w.streamers[1].GetUsername(), w.streamers[2].GetUsername())
		}
	}
}

func newPolicyBrokerWatcher(t *testing.T) *MinuteWatcher {
	t.Helper()
	sender := &countingSender{sent: make(chan string, 16)}
	w, _ := newLoopWatcher(4, sender, &staticChecker{checked: make(chan string, 16)})
	w.priorities = []config.Priority{config.PriorityDrops}
	for i, s := range w.streamers {
		s.Settings.ClaimDrops = true
		s.Settings.WatchStreak = false
		campaignID := "campaign-" + string(rune('a'+i))
		s.Stream.SetCampaignIDs([]string{campaignID})
		s.Stream.SetCampaigns([]*models.Campaign{{
			ID: campaignID,
			Drops: []*models.Drop{{
				ID:                 "drop-" + string(rune('a'+i)),
				Name:               "Reward",
				MinutesRequired:    30,
				PercentageProgress: 10,
				Claimability:       models.ClaimabilityKnownFalse,
			}},
		}})
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.ctx = ctx
	return w
}

func seedPolicyBrokerWeights(t *testing.T, w *MinuteWatcher, now time.Time, weights []float64) {
	t.Helper()
	store, db := openWatchTimeStore(t, filepath.Join(t.TempDir(), "watch.db"))
	t.Cleanup(func() { _ = db.Close() })
	w.store = store
	for idx, minutes := range weights {
		if minutes == 0 {
			continue
		}
		if err := store.RecordMinutes(w.streamers[idx].GetUsername(), minutes, now); err != nil {
			t.Fatalf("seed watch-time deficit for streamer %d: %v", idx, err)
		}
	}
}

func brokerHasChannel(snap BrokerSnapshot, login string) bool {
	for _, slot := range snap.Slots {
		if slot.Channel == login {
			return true
		}
	}
	return false
}

func brokerChannels(snap BrokerSnapshot) []string {
	channels := make([]string, 0, len(snap.Slots))
	for _, slot := range snap.Slots {
		channels = append(channels, slot.Channel)
	}
	return channels
}

func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
