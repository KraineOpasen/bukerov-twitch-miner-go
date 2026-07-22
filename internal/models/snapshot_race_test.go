package models

import (
	"sync"
	"testing"
)

// A published campaign snapshot must be safely readable while independent clones
// are mutated on other goroutines — no shared backing arrays, no data race.
// Run under -race to be meaningful.
func TestCampaignSnapshotCloneRaceSafe(t *testing.T) {
	published := gqlCampaign(map[string]interface{}{"channels": channelList("c1", "c2", "c3")})
	published.Drops = []*Drop{
		{ID: "d1", Name: "Skin", BenefitID: "ben-1", MinutesRequired: 30},
		{ID: "d2", Name: "Emote", BenefitID: "ben-2", MinutesRequired: 60},
	}

	var wg sync.WaitGroup

	// Readers of the immutable published snapshot.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				_ = published.IsChannelRestricted()
				_ = published.AllowsChannel("c2")
				_ = published.ACLState()
				_ = published.AllowedChannelCount()
				if len(published.ACL.ChannelIDs) == 0 {
					t.Error("published ACL channel list mutated away")
				}
			}
		}()
	}

	// Mutators working on independent clones.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				clone := published.Clone()
				clone.ApplyClaimHistoryRecords([]ClaimedReward{
					{Identity: NewRewardIdentity("", "ben-1", "", "", "", "Skin", 0, EntitlementWindow{})},
				}, nil)
				if len(clone.ACL.ChannelIDs) > 0 {
					clone.ACL.ChannelIDs[0] = "mutated"
				}
			}
		}()
	}

	wg.Wait()

	// The published snapshot is intact.
	if len(published.ACL.ChannelIDs) != 3 || published.ACL.ChannelIDs[0] != "c1" {
		t.Fatalf("published ACL was mutated by clones: %v", published.ACL.ChannelIDs)
	}
	if len(published.Drops) != 2 {
		t.Fatalf("published drops were mutated by clones: %d", len(published.Drops))
	}
}
