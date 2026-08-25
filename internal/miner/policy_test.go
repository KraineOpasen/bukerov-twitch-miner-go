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

func TestPolicyCampaignSemanticsPublishExactFailClosedFacts(t *testing.T) {
	facts := policyCampaignSemantics([]policy.Decision{
		{CampaignID: "urgent", SemanticClass: 0, Status: policy.StatusSafe, Feasibility: policy.Feasibility{MinutesToNextReward: 10}},
		{CampaignID: "weak", SemanticClass: 4, Status: policy.StatusAtRisk, Feasibility: policy.Feasibility{MinutesToNextReward: 20}},
		{CampaignID: "unknown", SemanticClass: 2, Status: policy.StatusUnknown, Feasibility: policy.Feasibility{MinutesToNextReward: 10}},
		{CampaignID: "completed", SemanticClass: 3, Status: policy.StatusSafe},
		{CampaignID: "excluded", SemanticClass: 1, Status: policy.StatusImpossible, Excluded: true},
	})
	if len(facts) != 4 || facts["urgent"].SemanticClass != 0 || facts["weak"].SemanticClass != 4 {
		t.Fatalf("exact campaign semantic publication = %v", facts)
	}
	if !facts["urgent"].SecondaryEligible || !facts["weak"].SecondaryEligible {
		t.Fatalf("known feasible remaining work was not secondary-eligible: %v", facts)
	}
	if facts["unknown"].SecondaryEligible || facts["completed"].SecondaryEligible {
		t.Fatalf("UNKNOWN/completed campaign gained secondary utility: %v", facts)
	}
	if _, ok := facts["excluded"]; ok {
		t.Fatal("Skip/IMPOSSIBLE campaign gained a published semantic fact")
	}
}

func TestBestAssignedPolicyUtilityUsesEligibleAssignmentsNotRawTotals(t *testing.T) {
	utility, ok := bestAssignedPolicyUtility([]*models.Campaign{
		{ID: "slow", Drops: []*models.Drop{{ID: "slow-drop", MinutesRequired: 30}}},
		{ID: "best", Drops: []*models.Drop{{ID: "best-drop", MinutesRequired: 30}}},
		{ID: "excluded", Drops: []*models.Drop{{ID: "excluded-drop", MinutesRequired: 30}}},
	}, map[string]policy.CampaignSemantic{
		"slow": {SemanticClass: 4, SecondaryEligible: true},
		"best": {SemanticClass: 1, SecondaryEligible: true},
	})
	if !ok || utility.SemanticClass != 1 || !utility.HasSecondary || utility.SecondarySemanticClass != 4 {
		t.Fatalf("assigned utility = %+v, ok=%v, want primary class 1 plus one secondary class 4", utility, ok)
	}
}

func TestBestDiscoveredPolicyUtilityUsesExactAdvertisedAndAllowedCampaign(t *testing.T) {
	s := models.NewStreamer("disco", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "allowed-channel"
	s.Stream.SetCampaignIDs([]string{"urgent-disallowed", "weak-allowed", "excluded"})
	campaigns := map[string]*models.Campaign{
		"urgent-disallowed": {ID: "urgent-disallowed", Channels: []string{"other-channel"}, Drops: []*models.Drop{{ID: "urgent-drop", MinutesRequired: 30}}},
		"weak-allowed":      {ID: "weak-allowed", Drops: []*models.Drop{{ID: "weak-drop", MinutesRequired: 30}}},
		"excluded":          {ID: "excluded", Drops: []*models.Drop{{ID: "excluded-drop", MinutesRequired: 30}}},
	}
	utility, ok := bestDiscoveredPolicyUtility(s, map[string]policy.CampaignSemantic{
		"urgent-disallowed": {SemanticClass: 0, SecondaryEligible: true},
		"weak-allowed":      {SemanticClass: 2, SecondaryEligible: true},
	}, campaigns)
	if !ok || utility.SemanticClass != 2 || utility.HasSecondary {
		t.Fatalf("discovery utility = %+v, ok=%v, want exact carried+allowed primary class 2", utility, ok)
	}
}

func TestBestDiscoveredPolicyUtilityFailsClosedOnUnknownCampaignACL(t *testing.T) {
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
	utility, ok := bestDiscoveredPolicyUtility(s, map[string]policy.CampaignSemantic{
		"unknown": {SemanticClass: 0, SecondaryEligible: true},
		"allowed": {SemanticClass: 2, SecondaryEligible: true},
	}, campaigns)
	if !ok || utility.SemanticClass != 2 || utility.HasSecondary {
		t.Fatalf("discovery utility = %+v, ok=%v, want allowed primary class 2 after ACLUnknown fails closed", utility, ok)
	}
}

func TestAssignedPolicyUtilityDeduplicatesCampaignAndTiers(t *testing.T) {
	campaign := &models.Campaign{
		ID: "one-campaign",
		Drops: []*models.Drop{
			{ID: "tier-one", MinutesRequired: 30},
			{ID: "tier-two", MinutesRequired: 60},
		},
	}
	utility, ok := bestAssignedPolicyUtility(
		[]*models.Campaign{campaign, campaign},
		map[string]policy.CampaignSemantic{
			campaign.ID: {SemanticClass: 0, SecondaryEligible: true},
		},
	)
	if !ok || utility.HasSecondary {
		t.Fatalf("duplicate CampaignID or two reward tiers manufactured overlap: utility=%+v ranked=%v", utility, ok)
	}
}

func TestConfiguredAndDiscoveredPolicyUtilitiesMatch(t *testing.T) {
	primary := &models.Campaign{ID: "primary", Drops: []*models.Drop{{ID: "primary-drop", MinutesRequired: 30}}}
	secondary := &models.Campaign{ID: "secondary", Drops: []*models.Drop{{ID: "secondary-drop", MinutesRequired: 60}}}
	facts := map[string]policy.CampaignSemantic{
		primary.ID:   {SemanticClass: 0, SecondaryEligible: true},
		secondary.ID: {SemanticClass: 3, SecondaryEligible: true},
	}
	configured, configuredOK := bestAssignedPolicyUtility([]*models.Campaign{primary, secondary}, facts)

	discoveredStreamer := models.NewStreamer("disco", models.StreamerSettings{ClaimDrops: true})
	discoveredStreamer.ChannelID = "channel"
	discoveredStreamer.Stream.SetCampaignIDs([]string{primary.ID, secondary.ID})
	discovered, discoveredOK := bestDiscoveredPolicyUtility(discoveredStreamer, facts, map[string]*models.Campaign{
		primary.ID: primary, secondary.ID: secondary,
	})
	if !configuredOK || !discoveredOK || configured != discovered {
		t.Fatalf("configured/discovered semantic projection diverged: configured=%+v (%v) discovered=%+v (%v)", configured, configuredOK, discovered, discoveredOK)
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
