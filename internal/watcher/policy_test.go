package watcher

import (
	"context"
	"path/filepath"
	"strings"
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

func TestCampaignPolicyFinalLoginTieIsNormalizedAndDeterministic(t *testing.T) {
	if cmp := compareNormalizedLogins(" StreamerB ", "streamera"); cmp <= 0 {
		t.Fatalf("normalized login order cmp=%d, want streamera before StreamerB", cmp)
	}
	if cmp := compareNormalizedLogins("Streamer", "streamer"); cmp == 0 {
		t.Fatal("case-variant malformed logins lost deterministic raw fallback")
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

// TestCampaignPolicyEqualPrimaryPrefersOneAdditionalCampaign is the
// executable regression for PR-06. Persisted fairness deliberately disfavors
// streamer A, so the base implementation (which publishes only one primary
// SemanticClass per login) treats all four channels as equal and leaves A out.
// A bounded secondary campaign fact must break that semantic tie before
// fairness without changing the two-slot cap.
func TestCampaignPolicyEqualPrimaryPrefersOneAdditionalCampaign(t *testing.T) {
	w := newPolicyBrokerWatcher(t)
	now := time.Now()
	overlap := w.streamers[0]
	extra := &models.Campaign{
		ID: "campaign-extra",
		Drops: []*models.Drop{{
			ID:                 "drop-extra",
			Name:               "Extra Reward",
			MinutesRequired:    30,
			PercentageProgress: 10,
			Claimability:       models.ClaimabilityKnownFalse,
		}},
	}
	overlap.Stream.SetCampaignIDs([]string{"campaign-a", extra.ID})
	overlap.Stream.SetCampaigns(append(overlap.Stream.GetCampaigns(), extra))

	byLogin := make(map[string]policy.SemanticUtility, len(w.streamers))
	byCampaign := make(map[string]policy.CampaignSemantic, len(w.streamers)+1)
	for i, s := range w.streamers {
		primaryID := "campaign-" + string(rune('a'+i))
		byLogin[s.GetUsername()] = policy.SemanticUtility{
			SemanticClass:     0,
			PrimaryCampaignID: primaryID,
		}
		byCampaign[primaryID] = policy.CampaignSemantic{SemanticClass: 0, SecondaryEligible: true}
	}
	byLogin[overlap.GetUsername()] = policy.SemanticUtility{
		SemanticClass:          0,
		SecondarySemanticClass: 2,
		HasSecondary:           true,
		PrimaryCampaignID:      "campaign-a",
		SecondaryCampaignID:    extra.ID,
	}
	byCampaign[extra.ID] = policy.CampaignSemantic{SemanticClass: 2, SecondaryEligible: true}
	w.SetCampaignSemanticPolicy(byLogin, byCampaign, nil)

	seedPolicyBrokerWeights(t, w, now, []float64{100, 0, 1, 200})
	w.processWatching()

	snap := w.BrokerSnapshot()
	if len(snap.Slots) != 2 {
		t.Fatalf("bounded overlap allocation used %d slots, want hard cap 2: %v", len(snap.Slots), brokerChannels(snap))
	}
	if !brokerHasChannel(snap, overlap.GetUsername()) {
		t.Fatalf("equal primary semantics with one additional distinct campaign allocated %v; want overlap channel %q before persisted fairness", brokerChannels(snap), overlap.GetUsername())
	}
	for _, slot := range snap.Slots {
		if slot.Channel == overlap.GetUsername() && !strings.Contains(slot.Reason, "bounded secondary semantic class 2") {
			t.Fatalf("overlap winner reason does not expose bounded secondary utility: %q", slot.Reason)
		}
	}
}

// TestCampaignPolicyFullBoundedUtilityTieUsesPersistedDeficit protects the
// final semantic-tie boundary: equal primary and equal best-secondary facts
// must still fall through to the existing persisted fairness mechanism.
func TestCampaignPolicyFullBoundedUtilityTieUsesPersistedDeficit(t *testing.T) {
	w := newPolicyBrokerWatcher(t)
	now := time.Now()
	byLogin := make(map[string]policy.SemanticUtility, len(w.streamers))
	byCampaign := make(map[string]policy.CampaignSemantic, len(w.streamers)*2)
	for i, s := range w.streamers {
		primaryID := "campaign-" + string(rune('a'+i))
		secondaryID := "secondary-" + string(rune('a'+i))
		extra := &models.Campaign{
			ID: secondaryID,
			Drops: []*models.Drop{{
				ID:                 "secondary-drop-" + string(rune('a'+i)),
				MinutesRequired:    30,
				PercentageProgress: 10,
				Claimability:       models.ClaimabilityKnownFalse,
			}},
		}
		s.Stream.SetCampaignIDs([]string{primaryID, secondaryID})
		s.Stream.SetCampaigns(append(s.Stream.GetCampaigns(), extra))
		byLogin[s.GetUsername()] = policy.SemanticUtility{
			SemanticClass:          0,
			SecondarySemanticClass: 2,
			HasSecondary:           true,
			PrimaryCampaignID:      primaryID,
			SecondaryCampaignID:    secondaryID,
		}
		byCampaign[primaryID] = policy.CampaignSemantic{SemanticClass: 0, SecondaryEligible: true}
		byCampaign[secondaryID] = policy.CampaignSemantic{SemanticClass: 2, SecondaryEligible: true}
	}
	w.SetCampaignSemanticPolicy(byLogin, byCampaign, nil)

	seedPolicyBrokerWeights(t, w, now, []float64{100, 0, 1, 200})
	w.processWatching()

	snap := w.BrokerSnapshot()
	if len(snap.Slots) != 2 {
		t.Fatalf("full semantic tie allocated %d slots, want hard cap 2: %v", len(snap.Slots), brokerChannels(snap))
	}
	for _, idx := range []int{1, 2} {
		if !brokerHasChannel(snap, w.streamers[idx].GetUsername()) {
			t.Fatalf("full primary+secondary tie allocation %v; want persisted-deficit pair [%s %s]", brokerChannels(snap), w.streamers[1].GetUsername(), w.streamers[2].GetUsername())
		}
	}
}

func TestConfiguredCampaignUtilityReprojectsCurrentRemainingWork(t *testing.T) {
	w := newPolicyBrokerWatcher(t)
	s := w.streamers[0]
	primary := s.Stream.GetCampaigns()[0]
	secondary := &models.Campaign{
		ID: "secondary-live",
		Drops: []*models.Drop{{
			ID:                    "secondary-live-drop",
			MinutesRequired:       30,
			CurrentMinutesWatched: 10,
		}},
	}
	s.Stream.SetCampaignIDs([]string{primary.ID, secondary.ID})
	s.Stream.SetCampaigns([]*models.Campaign{primary, secondary})
	w.SetCampaignSemanticPolicy(
		map[string]policy.SemanticUtility{
			s.GetUsername(): {
				SemanticClass:          0,
				SecondarySemanticClass: 2,
				HasSecondary:           true,
				PrimaryCampaignID:      primary.ID,
				SecondaryCampaignID:    secondary.ID,
			},
		},
		map[string]policy.CampaignSemantic{
			primary.ID:   {SemanticClass: 0, SecondaryEligible: true},
			secondary.ID: {SemanticClass: 2, SecondaryEligible: true},
		},
		nil,
	)
	if utility, ok := w.campaignSemanticUtilityForStreamer(s); !ok || !utility.HasSecondary {
		t.Fatalf("test setup utility = %+v, ranked=%v, want live secondary", utility, ok)
	}

	secondary.Drops[0].CurrentMinutesWatched = secondary.Drops[0].MinutesRequired
	if utility, ok := w.campaignSemanticUtilityForStreamer(s); !ok || utility.HasSecondary {
		t.Fatalf("completed assigned campaign retained stale secondary utility: utility=%+v ranked=%v", utility, ok)
	}

	secondary.Drops[0].CurrentMinutesWatched = 10
	s.Stream.SetCampaigns([]*models.Campaign{primary})
	if utility, ok := w.campaignSemanticUtilityForStreamer(s); !ok || utility.HasSecondary {
		t.Fatalf("removed assigned campaign retained stale secondary utility: utility=%+v ranked=%v", utility, ok)
	}
}

func TestCampaignSemanticEvidenceFailsClosedAcrossDuplicateCampaignID(t *testing.T) {
	primary := &models.Campaign{ID: "primary", Drops: []*models.Drop{{ID: "primary-drop", MinutesRequired: 30}}}
	secondaryLive := &models.Campaign{ID: "secondary", Drops: []*models.Drop{{ID: "live", MinutesRequired: 30}}}
	secondaryCompleted := &models.Campaign{ID: "secondary", Drops: []*models.Drop{{
		ID:                    "completed",
		MinutesRequired:       30,
		CurrentMinutesWatched: 30,
	}}}
	ids, remaining := CampaignSemanticEvidence([]*models.Campaign{primary, secondaryLive, secondaryCompleted})
	utility, ok := policy.BuildSemanticUtilityWithRemainingWork(ids, remaining, map[string]policy.CampaignSemantic{
		primary.ID:       {SemanticClass: 0, SecondaryEligible: true},
		secondaryLive.ID: {SemanticClass: 2, SecondaryEligible: true},
	})
	if !ok || utility.HasSecondary {
		t.Fatalf("conflicting duplicate CampaignID retained secondary utility: ids=%v remaining=%v utility=%+v ranked=%v", ids, remaining, utility, ok)
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

func brokerReason(snap BrokerSnapshot, login string) string {
	for _, slot := range snap.Slots {
		if slot.Channel == login {
			return slot.Reason
		}
	}
	return ""
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
