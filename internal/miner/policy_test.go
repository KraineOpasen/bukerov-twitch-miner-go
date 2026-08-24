package miner

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
)

// TestPolicyGameRanks maps campaign semantic classes onto per-game ranks (best
// class wins, excluded decisions ignored) for discovery cross-game ordering.
func TestPolicyGameRanks(t *testing.T) {
	campaigns := []*models.Campaign{
		{ID: "c1", Game: &models.Game{Name: "Alpha"}},
		{ID: "c2", Game: &models.Game{Name: "Bravo"}},
		{ID: "c3", Game: &models.Game{Name: "Alpha"}}, // same game as c1
		{ID: "c4", Game: &models.Game{Name: "Charlie"}},
	}
	decisions := []policy.Decision{
		{CampaignID: "c2", SemanticClass: 0}, // Bravo first
		{CampaignID: "c4", Excluded: true},   // excluded → ignored
		{CampaignID: "c3", SemanticClass: 1}, // Alpha
		{CampaignID: "c1", SemanticClass: 2}, // Alpha again → best rank remains 1
	}
	ranks := policyGameRanks(decisions, campaigns)

	if ranks["bravo"] != 0 {
		t.Errorf("bravo rank = %d, want 0", ranks["bravo"])
	}
	if ranks["alpha"] != 1 {
		t.Errorf("alpha rank = %d, want 1", ranks["alpha"])
	}
	if _, ok := ranks["charlie"]; ok {
		t.Error("excluded campaign's game must not be ranked")
	}
}

func TestPolicyCampaignClassesPublishExactNonExcludedIDs(t *testing.T) {
	classes := policyCampaignClasses([]policy.Decision{
		{CampaignID: "urgent", SemanticClass: 0},
		{CampaignID: "weak", SemanticClass: 4},
		{CampaignID: "excluded", SemanticClass: 1, Excluded: true},
	})
	if len(classes) != 2 || classes["urgent"] != 0 || classes["weak"] != 4 {
		t.Fatalf("exact campaign class publication = %v", classes)
	}
	if _, ok := classes["excluded"]; ok {
		t.Fatal("excluded campaign gained a published semantic class")
	}
}

func TestBestAssignedPolicyClassUsesEligibleAssignmentsNotRawTotals(t *testing.T) {
	class, ok := bestAssignedPolicyClass([]*models.Campaign{
		{ID: "slow", Drops: []*models.Drop{{ID: "slow-drop", MinutesRequired: 30}}},
		{ID: "best", Drops: []*models.Drop{{ID: "best-drop", MinutesRequired: 30}}},
		{ID: "excluded", Drops: []*models.Drop{{ID: "excluded-drop", MinutesRequired: 30}}},
	}, map[string]policy.Decision{
		"slow":     {CampaignID: "slow", Total: 999, SemanticClass: 4},
		"best":     {CampaignID: "best", Total: -999, SemanticClass: 1},
		"excluded": {CampaignID: "excluded", SemanticClass: 0, Excluded: true},
	})
	if !ok || class != 1 {
		t.Fatalf("best assigned class = %d, ok=%v, want semantic class 1 rather than raw Total or excluded class", class, ok)
	}
}

func TestBestDiscoveredPolicyClassUsesExactAdvertisedAndAllowedCampaign(t *testing.T) {
	s := models.NewStreamer("disco", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "allowed-channel"
	s.Stream.SetCampaignIDs([]string{"urgent-disallowed", "weak-allowed", "excluded"})
	campaigns := map[string]*models.Campaign{
		"urgent-disallowed": {ID: "urgent-disallowed", Channels: []string{"other-channel"}, Drops: []*models.Drop{{ID: "urgent-drop", MinutesRequired: 30}}},
		"weak-allowed":      {ID: "weak-allowed", Drops: []*models.Drop{{ID: "weak-drop", MinutesRequired: 30}}},
		"excluded":          {ID: "excluded", Drops: []*models.Drop{{ID: "excluded-drop", MinutesRequired: 30}}},
	}
	class, ok := bestDiscoveredPolicyClass(s, map[string]policy.Decision{
		"urgent-disallowed": {CampaignID: "urgent-disallowed", SemanticClass: 0},
		"weak-allowed":      {CampaignID: "weak-allowed", SemanticClass: 2},
		"excluded":          {CampaignID: "excluded", SemanticClass: 1, Excluded: true},
	}, campaigns)
	if !ok || class != 2 {
		t.Fatalf("discovery class = %d, ok=%v, want exact carried+allowed class 2", class, ok)
	}
}

func TestBestDiscoveredPolicyClassFailsClosedOnUnknownCampaignACL(t *testing.T) {
	s := models.NewStreamer("disco", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "channel"
	s.Stream.SetCampaignIDs([]string{"unknown", "allowed"})
	campaigns := map[string]*models.Campaign{
		"unknown": {
			ID:    "unknown",
			ACL:   models.CampaignACL{State: models.ACLUnknown, Source: models.ACLSourceCampaignDetails},
			Drops: []*models.Drop{{ID: "unknown-drop", MinutesRequired: 30}},
		},
		"allowed": {ID: "allowed", Drops: []*models.Drop{{ID: "allowed-drop", MinutesRequired: 30}}},
	}
	class, ok := bestDiscoveredPolicyClass(s, map[string]policy.Decision{
		"unknown": {CampaignID: "unknown", SemanticClass: 0},
		"allowed": {CampaignID: "allowed", SemanticClass: 2},
	}, campaigns)
	if !ok || class != 2 {
		t.Fatalf("discovery class = %d, ok=%v, want allowed class 2 after ACLUnknown fails closed", class, ok)
	}
}

func TestPolicyGameRanksPreserveEqualSemanticClassAcrossGames(t *testing.T) {
	campaigns := []*models.Campaign{
		{ID: "a", Game: &models.Game{Name: "Alpha"}},
		{ID: "b", Game: &models.Game{Name: "Bravo"}},
	}
	ranks := policyGameRanks([]policy.Decision{
		{CampaignID: "a", SemanticClass: 0},
		{CampaignID: "b", SemanticClass: 0},
	}, campaigns)
	if ranks["alpha"] != 0 || ranks["bravo"] != 0 {
		t.Fatalf("campaign-ID presentation tie split equal game semantics: %v", ranks)
	}
}

func TestPolicyGameRanksCarryHighPriorityAcrossAllModes(t *testing.T) {
	now := time.Now()
	campaigns := []*models.Campaign{
		{ID: "high", Game: &models.Game{Name: "High Game"}},
		{ID: "ordinary", Game: &models.Game{Name: "Ordinary Game"}},
	}
	for _, mode := range []policy.Mode{
		policy.ModeGameOrder,
		policy.ModeEndingSoonest,
		policy.ModeClosestToReward,
		policy.ModeLowAvailability,
		policy.ModeSmart,
	} {
		t.Run(string(mode), func(t *testing.T) {
			decisions := policy.Rank(mode, []policy.CampaignInput{
				{
					CampaignID: "high", HighPriority: true, GameOrderIndex: 1,
					EndAt: now.Add(8 * time.Hour), EligibleLiveChannels: 10,
					Drops: []policy.DropStep{{MinutesRequired: 120, CurrentMinutesWatched: 0}},
				},
				{
					CampaignID: "ordinary", GameOrderIndex: 0,
					EndAt: now.Add(time.Hour), EligibleLiveChannels: 1,
					Drops: []policy.DropStep{{MinutesRequired: 30, CurrentMinutesWatched: 29}},
				},
			}, now)
			ranks := policyGameRanks(decisions, campaigns)
			if ranks["high game"] >= ranks["ordinary game"] {
				t.Fatalf("mode %s game ranks lost HighPriority: %v", mode, ranks)
			}
		})
	}
}

func TestPolicyGameRanksKeepUnknownDeadlineAfterKnown(t *testing.T) {
	now := time.Now()
	decisions := policy.Rank(policy.ModeEndingSoonest, []policy.CampaignInput{
		{CampaignID: "unknown", Drops: []policy.DropStep{{MinutesRequired: 30}}},
		{CampaignID: "known", EndAt: now.Add(2 * time.Hour), Drops: []policy.DropStep{{MinutesRequired: 30}}},
	}, now)
	ranks := policyGameRanks(decisions, []*models.Campaign{
		{ID: "unknown", Game: &models.Game{Name: "Unknown Game"}},
		{ID: "known", Game: &models.Game{Name: "Known Game"}},
	})
	if ranks["known game"] >= ranks["unknown game"] {
		t.Fatalf("known-before-unknown ending semantics were lost in discovery game ranks: %v", ranks)
	}
}

func TestPolicyGameRanksIncludeDisplayNameOnlyCampaign(t *testing.T) {
	ranks := policyGameRanks([]policy.Decision{
		{CampaignID: "display-only", SemanticClass: 0},
	}, []*models.Campaign{
		{ID: "display-only", Game: &models.Game{ID: "g1", DisplayName: "Display Game"}},
	})
	if rank, ok := ranks["display game"]; !ok || rank != 0 {
		t.Fatalf("DisplayName-only campaign rank = %d, present=%v, want semantic rank 0", rank, ok)
	}
}

func TestPolicyGameAliasesPreserveConfiguredGameOrderAndDiscoveryRanks(t *testing.T) {
	game := &models.Game{ID: "g1", Name: "Internal Name", DisplayName: "Display Name"}
	configured := map[string]int{"display name": 0}
	if got := campaignPolicyGameName(game, configured); got != "Display Name" {
		t.Fatalf("configured game alias = %q, want Display Name", got)
	}
	ranks := policyGameRanks([]policy.Decision{
		{CampaignID: "campaign", SemanticClass: 2},
	}, []*models.Campaign{{ID: "campaign", Game: game}})
	for _, alias := range []string{"internal name", "display name"} {
		if rank, ok := ranks[alias]; !ok || rank != 2 {
			t.Fatalf("rank for alias %q = %d, present=%v, want 2", alias, rank, ok)
		}
	}
}

func TestGameOrderIndexLookup(t *testing.T) {
	idx := map[string]int{"world of tanks": 0, "rust": 1}
	if got := gameOrderIndex(idx, "Rust"); got != 1 {
		t.Errorf("case-insensitive lookup = %d, want 1", got)
	}
	if got := gameOrderIndex(idx, "Unlisted Game"); got != -1 {
		t.Errorf("unconfigured game = %d, want -1", got)
	}
}
