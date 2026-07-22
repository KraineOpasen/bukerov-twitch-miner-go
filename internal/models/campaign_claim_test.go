package models

import (
	"testing"
)

func claimedBenefit(gameID, benefitID, name string) ClaimedReward {
	return ClaimedReward{Identity: NewRewardIdentity(gameID, benefitID, "", "", "", name, 0, EntitlementWindow{})}
}
func claimedName(gameID, name string) ClaimedReward {
	return ClaimedReward{Identity: NewRewardIdentity(gameID, "", "", "", "", name, 0, EntitlementWindow{})}
}

// 5 & 16: a claimed lower tier (identified by its own benefit ID) must not
// remove a different, same-named higher tier.
func TestApplyClaimHistoryRecordsKeepsOtherTier(t *testing.T) {
	c := &Campaign{
		ID:   "camp-1",
		Name: "Coins",
		Game: &Game{ID: "g1"},
		Drops: []*Drop{
			{ID: "drop-30", Name: "Coin Reward", BenefitID: "ben-30", MinutesRequired: 30},
			{ID: "drop-60", Name: "Coin Reward", BenefitID: "ben-60", MinutesRequired: 60},
		},
	}
	out := c.ApplyClaimHistoryRecords([]ClaimedReward{claimedBenefit("g1", "ben-30", "Coin Reward")}, fixedClock(testNow))

	if len(c.Drops) != 1 || c.Drops[0].BenefitID != "ben-60" {
		t.Fatalf("expected only the unclaimed 60m tier to remain, got %+v", c.Drops)
	}
	if len(out.ConfirmedNames) != 1 {
		t.Fatalf("expected one confirmed removal, got %+v", out.ConfirmedNames)
	}
	if c.ClaimStatus != CampaignClaimStatusInProgress {
		t.Fatalf("campaign still has an unclaimed tier, want in_progress, got %s", c.ClaimStatus)
	}
}

// 4: same display name, different DropID and different tier minutes, no strong
// benefit id -> ambiguous -> BOTH retained (fail open, never falsely claimed).
func TestApplyClaimHistoryRecordsAmbiguousTiersRetained(t *testing.T) {
	c := &Campaign{
		ID:   "camp-1",
		Name: "Coins",
		Game: &Game{ID: "g1"},
		Drops: []*Drop{
			{ID: "drop-30", Name: "Coin Reward", MinutesRequired: 30},
			{ID: "drop-60", Name: "Coin Reward", MinutesRequired: 60},
		},
	}
	out := c.ApplyClaimHistoryRecords([]ClaimedReward{claimedName("g1", "Coin Reward")}, fixedClock(testNow))

	if len(c.Drops) != 2 {
		t.Fatalf("ambiguous name-only claim must retain both tiers, got %+v", c.Drops)
	}
	if len(out.ConfirmedNames) != 0 {
		t.Fatalf("nothing should be confirmed-claimed, got %+v", out.ConfirmedNames)
	}
	if len(out.AmbiguousNames) != 2 {
		t.Fatalf("expected both drops flagged ambiguous, got %+v", out.AmbiguousNames)
	}
	if len(c.ClaimedDropNames) != 0 {
		t.Fatalf("ambiguous drops must not be labelled claimed, got %v", c.ClaimedDropNames)
	}
}

// 6: same game/name but ambiguous identity -> retained, not claimed.
func TestApplyClaimHistoryRecordsAmbiguousRetained(t *testing.T) {
	c := &Campaign{
		ID:    "camp-1",
		Name:  "Skin",
		Game:  &Game{ID: "g1"},
		Drops: []*Drop{{ID: "drop-a", Name: "Legendary Skin", MinutesRequired: 60}},
	}
	c.ApplyClaimHistoryRecords([]ClaimedReward{claimedName("g1", "Legendary Skin")}, fixedClock(testNow))
	if len(c.Drops) != 1 {
		t.Fatalf("ambiguous match must retain the drop, got %+v", c.Drops)
	}
	if c.ClaimStatus == CampaignClaimStatusAlreadyClaimed {
		t.Fatal("ambiguous match must not mark the campaign already-claimed")
	}
}

// Strong benefit-id match confirms and (when it clears the campaign) marks it.
func TestApplyClaimHistoryRecordsConfirmedFullyClaimed(t *testing.T) {
	c := &Campaign{
		ID:    "camp-1",
		Name:  "Skin",
		Game:  &Game{ID: "g1"},
		Drops: []*Drop{{ID: "drop-a", Name: "Legendary Skin", BenefitID: "ben-1", MinutesRequired: 60}},
	}
	c.ApplyClaimHistoryRecords([]ClaimedReward{claimedBenefit("g1", "ben-1", "Legendary Skin")}, fixedClock(testNow))
	if len(c.Drops) != 0 {
		t.Fatalf("confirmed benefit match should strip the drop, got %+v", c.Drops)
	}
	if c.ClaimStatus != CampaignClaimStatusAlreadyClaimed {
		t.Fatalf("expected already_claimed, got %s", c.ClaimStatus)
	}
}

// 11: strict fallback — unique name + decidable overlapping windows confirms
// even without strong IDs; a second same-named drop would make it ambiguous.
func TestApplyClaimHistoryRecordsStrictFallback(t *testing.T) {
	win := knownWindow(hoursFromNow(-2), hoursFromNow(2))
	recWin := ClaimedReward{Identity: NewRewardIdentity("g1", "", "", "", "", "Unique Skin", 0, win)}

	// Unique name in the campaign -> confirmed via strict fallback.
	unique := &Campaign{
		ID: "camp-1", Game: &Game{ID: "g1"},
		StartAt: win.Start, EndAt: win.End,
		Drops: []*Drop{{ID: "d1", Name: "Unique Skin", StartAt: win.Start, EndAt: win.End, MinutesRequired: 60}},
	}
	unique.ApplyClaimHistoryRecords([]ClaimedReward{recWin}, fixedClock(testNow))
	if len(unique.Drops) != 0 {
		t.Fatalf("unique name + overlapping window should confirm, got %+v", unique.Drops)
	}

	// Non-unique name -> strict fallback must NOT fire (retain both).
	dup := &Campaign{
		ID: "camp-2", Game: &Game{ID: "g1"},
		StartAt: win.Start, EndAt: win.End,
		Drops: []*Drop{
			{ID: "d1", Name: "Unique Skin", StartAt: win.Start, EndAt: win.End, MinutesRequired: 30},
			{ID: "d2", Name: "Unique Skin", StartAt: win.Start, EndAt: win.End, MinutesRequired: 60},
		},
	}
	dup.ApplyClaimHistoryRecords([]ClaimedReward{recWin}, fixedClock(testNow))
	if len(dup.Drops) != 2 {
		t.Fatalf("non-unique name must not confirm via strict fallback, got %+v", dup.Drops)
	}
}

// 14 at campaign level: a repeatable reward's later occurrence (disjoint window)
// stays farmable even though claim history has the earlier benefit id.
func TestApplyClaimHistoryRecordsRepeatableWindowRetained(t *testing.T) {
	earlier := knownWindow(hoursFromNow(-72), hoursFromNow(-48))
	later := knownWindow(hoursFromNow(-2), hoursFromNow(24))
	claimed := ClaimedReward{Identity: NewRewardIdentity("g1", "ben-week", "", "", "", "Weekly Coin", 0, earlier)}

	c := &Campaign{
		ID: "camp-week-2", Game: &Game{ID: "g1"},
		StartAt: later.Start, EndAt: later.End,
		Drops: []*Drop{{ID: "d-new", Name: "Weekly Coin", BenefitID: "ben-week", StartAt: later.Start, EndAt: later.End, MinutesRequired: 60}},
	}
	c.ApplyClaimHistoryRecords([]ClaimedReward{claimed}, fixedClock(testNow))
	if len(c.Drops) != 1 {
		t.Fatalf("new entitlement occurrence must remain farmable, got %+v", c.Drops)
	}
}

// 20: Clone performs a deep copy of drops, channels, ACL and claimed names.
func TestCampaignCloneDeepCopiesACLAndIdentity(t *testing.T) {
	c := &Campaign{
		ID:       "camp-1",
		Channels: []string{"c1", "c2"},
		ACL: CampaignACL{
			State:      ACLRestricted,
			ChannelIDs: []string{"c1", "c2"},
			Complete:   true,
			Source:     ACLSourceCampaignDetails,
		},
		Drops:            []*Drop{{ID: "d1", Name: "Skin", BenefitID: "ben-1"}},
		ClaimedDropNames: []string{"old"},
	}
	clone := c.Clone()
	clone.ACL.ChannelIDs[0] = "MUTATED"
	clone.Channels[0] = "MUTATED"
	clone.Drops[0].BenefitID = "MUTATED"
	clone.ClaimedDropNames[0] = "MUTATED"

	if c.ACL.ChannelIDs[0] != "c1" {
		t.Error("Clone shared ACL.ChannelIDs backing array")
	}
	if c.Channels[0] != "c1" {
		t.Error("Clone shared Channels backing array")
	}
	if c.Drops[0].BenefitID != "ben-1" {
		t.Error("Clone shared Drop pointer")
	}
	if c.ClaimedDropNames[0] != "old" {
		t.Error("Clone shared ClaimedDropNames backing array")
	}
}

// Idempotence: applying the same confirmed claim twice does not duplicate the
// claimed-name annotation (drop already removed) — reconciliation stays clean.
func TestApplyClaimHistoryRecordsIdempotent(t *testing.T) {
	mk := func() *Campaign {
		return &Campaign{
			ID: "camp-1", Game: &Game{ID: "g1"},
			Drops: []*Drop{{ID: "d1", Name: "Skin", BenefitID: "ben-1", MinutesRequired: 60}},
		}
	}
	c := mk()
	records := []ClaimedReward{claimedBenefit("g1", "ben-1", "Skin")}
	c.ApplyClaimHistoryRecords(records, fixedClock(testNow))
	c.ApplyClaimHistoryRecords(records, fixedClock(testNow))
	if len(c.ClaimedDropNames) != 1 {
		t.Fatalf("expected exactly one claimed-name record after re-apply, got %v", c.ClaimedDropNames)
	}
}
