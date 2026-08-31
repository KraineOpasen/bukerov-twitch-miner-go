package models

import "testing"

// Canonical identity exactness: the decision matches only the exact
// normalized reward identity — the same reward name under another game must
// never collide — while case and outer whitespace never split identities.
func TestRewardSkipsIdentityExactness(t *testing.T) {
	skips := NewRewardSkips([]string{NormalizeRewardKey("game-1", "Legendary Skin")})

	if !skips.SkipsReward("game-1", "Legendary Skin") {
		t.Fatal("exact identity must be skipped")
	}
	if !skips.SkipsReward("Game-1", "  legendary skin ") {
		t.Fatal("case/whitespace variants of the same identity must be skipped")
	}
	if skips.SkipsReward("game-2", "Legendary Skin") {
		t.Fatal("same reward name under another game must NOT collide")
	}
	if skips.SkipsReward("game-1", "Legendary Skin II") {
		t.Fatal("a different reward name must not match")
	}
	if skips.SkipsReward("", "Legendary Skin") {
		t.Fatal("a game-less identity is distinct from a game-scoped one")
	}
}

// Verbatim key storage: a legacy non-canonical config key stays inert here
// exactly as it is inert for the policy ranker's lookup — parity, not a
// second rule system.
func TestRewardSkipsLegacyNonCanonicalKeyStaysInert(t *testing.T) {
	skips := NewRewardSkips([]string{"Game-1::Legendary Skin"}) // not normalized
	if skips.SkipsReward("game-1", "Legendary Skin") {
		t.Fatal("non-canonical stored key must stay inert (policy-lookup parity)")
	}
	if !skips.SkipsKey("Game-1::Legendary Skin") {
		t.Fatal("SkipsKey is an exact-key lookup")
	}
}

// Nil-safety: a nil decision (and a nil campaign) excludes nothing.
func TestRewardSkipsNilSafety(t *testing.T) {
	var skips *RewardSkips
	if skips.SkipsKey("k") || skips.SkipsReward("g", "n") || skips.SkipsCampaignCurrentDrop(&Campaign{}) {
		t.Fatal("nil RewardSkips must exclude nothing")
	}
	if NewRewardSkips(nil).SkipsReward("g", "n") {
		t.Fatal("empty RewardSkips must exclude nothing")
	}
	if NewRewardSkips([]string{"g::n"}).SkipsCampaignCurrentDrop(nil) {
		t.Fatal("nil campaign is never excluded")
	}
}

// Campaign-level interpretation mirrors the policy ranker: the campaign is
// excluded exactly while its CURRENT drop is skipped. Once the skipped drop
// is finished/claimed and CurrentDrop moves on, the campaign is farmable
// again — a completed skipped reward never blocks unrelated later work.
func TestRewardSkipsCampaignCurrentDropInterpretation(t *testing.T) {
	skips := NewRewardSkips([]string{NormalizeRewardKey("g1", "First Reward")})

	first := &Drop{ID: "d1", Name: "First Reward", MinutesRequired: 30}
	second := &Drop{ID: "d2", Name: "Second Reward", MinutesRequired: 60}
	c := &Campaign{ID: "c1", Game: &Game{ID: "g1"}, Drops: []*Drop{first, second}}

	if !skips.SkipsCampaignCurrentDrop(c) {
		t.Fatal("campaign must be excluded while the skipped drop is current")
	}

	// The skipped drop completes (threshold met, claimed): current moves to
	// the unskipped second drop and the campaign becomes farmable again.
	first.CurrentMinutesWatched = 30
	first.IsClaimed = true
	if skips.SkipsCampaignCurrentDrop(c) {
		t.Fatal("campaign must be farmable again once the skipped drop is no longer current")
	}

	// A skip rule on a NON-current later drop does not exclude the campaign
	// (policy-lookup parity: only the current drop keys the rule).
	laterSkips := NewRewardSkips([]string{NormalizeRewardKey("g1", "Second Reward")})
	fresh := &Campaign{ID: "c2", Game: &Game{ID: "g1"}, Drops: []*Drop{
		{ID: "d1", Name: "First Reward", MinutesRequired: 30},
		{ID: "d2", Name: "Second Reward", MinutesRequired: 60},
	}}
	if laterSkips.SkipsCampaignCurrentDrop(fresh) {
		t.Fatal("a rule on a non-current drop must not exclude the campaign (parity with the ranker)")
	}
}

// The streamer's drop authority honors the exclusions: an assigned campaign
// whose current drop is skipped contributes no drop justification, while an
// unskipped sibling assignment still does.
func TestStreamerDropAuthorityExcludingSkips(t *testing.T) {
	skipped := &Campaign{ID: "c-skip", Game: &Game{ID: "g1"}, ClaimStatus: CampaignClaimStatusInProgress,
		Drops: []*Drop{{ID: "d1", Name: "Skipped Reward", MinutesRequired: 60, CurrentMinutesWatched: 10}}}
	wanted := &Campaign{ID: "c-want", Game: &Game{ID: "g1"}, ClaimStatus: CampaignClaimStatusInProgress,
		Drops: []*Drop{{ID: "d2", Name: "Wanted Reward", MinutesRequired: 60, CurrentMinutesWatched: 10}}}
	skips := NewRewardSkips([]string{NormalizeRewardKey("g1", "Skipped Reward")})

	s := NewStreamer("chan", StreamerSettings{ClaimDrops: true})
	s.Stream.SetCampaigns([]*Campaign{skipped})
	if !s.HasEligibleAssignedDropCampaign() {
		t.Fatal("baseline: plain authority must see the assignment")
	}
	if s.HasEligibleAssignedDropCampaignExcluding(skips) {
		t.Fatal("a skipped-only assignment must contribute no drop authority")
	}

	s.Stream.SetCampaigns([]*Campaign{skipped, wanted})
	if !s.HasEligibleAssignedDropCampaignExcluding(skips) {
		t.Fatal("an unskipped sibling assignment must keep drop authority")
	}
	if !s.HasEligibleAssignedDropCampaignExcluding(nil) {
		t.Fatal("nil skips must behave exactly like the plain method")
	}
}
