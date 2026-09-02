package discovery

// Regression tests for the operator farming exclusion on the discovery path:
// a channel whose only tracked campaign carries a Skip-ruled current drop is
// pure skipped-reward farming (no points justification exists for a discovery
// channel), so it must be neither proposed nor kept.

import (
	"context"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// A channel advertising only the skipped campaign is not proposed and gets no
// assignment published; identity is exact (a rule for the same reward name
// under another game leaves the channel eligible).
func TestDiscoveryExcludesSkippedCampaignExactIdentity(t *testing.T) {
	campaign := activeCampaign("g1", "World of Tanks")
	campaign.Drops[0].Name = "Twin Reward"
	provider := &fakeCampaigns{campaigns: []*models.Campaign{campaign}}

	manager := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})
	manager.UpdateRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("g1", "Twin Reward"),
	}))
	candidate := onlineCandidate("candidate", "channel-1", "World of Tanks", "g1", 100)
	manager.pool = []*Channel{candidate}

	if got := manager.WatchCandidates(context.Background()); len(got) != 0 {
		t.Fatalf("skipped-campaign channel must not be proposed, got %d proposals", len(got))
	}
	if assigned := candidate.Streamer.Stream.GetCampaigns(); len(assigned) != 0 {
		t.Fatalf("no assignment may be published for a skipped campaign, got %+v", assigned)
	}

	// Exact identity: a rule for the same name under ANOTHER game must not
	// exclude this channel.
	manager.UpdateRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("other-game", "Twin Reward"),
	}))
	if got := manager.WatchCandidates(context.Background()); len(got) != 1 {
		t.Fatalf("foreign-game rule must not exclude the channel, got %d proposals", len(got))
	}
	if !candidate.Streamer.HasEligibleAssignedDropCampaign() {
		t.Fatal("eligible channel must publish its assignment")
	}
}

// Concurrent rule replacement races neither the proposal pass nor the
// current-channel revalidation (-race): the manager snapshots the immutable
// decision once per channel evaluation under its lock.
func TestDiscoveryConcurrentRewardSkipsUpdate(t *testing.T) {
	campaign := activeCampaign("g1", "World of Tanks")
	campaign.Drops[0].Name = "Skipped Reward"
	provider := &fakeCampaigns{campaigns: []*models.Campaign{campaign}}
	manager := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})
	candidate := onlineCandidate("candidate", "channel-1", "World of Tanks", "g1", 100)
	manager.pool = []*Channel{candidate}

	skips := models.NewRewardSkips([]string{models.NormalizeRewardKey("g1", "Skipped Reward")})
	manager.UpdateRewardSkips(skips)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			manager.UpdateRewardSkips(skips)
		}
	}()
	for i := 0; i < 20; i++ {
		if got := manager.WatchCandidates(context.Background()); len(got) != 0 {
			t.Fatalf("the skipped-campaign channel must stay excluded under concurrent updates, got %d proposals", len(got))
		}
	}
	<-done
}

// A rule landing at runtime evicts the already-proposed discovery channel:
// the next WatchCandidates pass abandons it instead of keeping the skipped
// reward farming.
func TestDiscoveryRuntimeRuleFlipAbandonsCurrentChannel(t *testing.T) {
	campaign := activeCampaign("g1", "World of Tanks")
	campaign.Drops[0].Name = "Skipped Reward"
	provider := &fakeCampaigns{campaigns: []*models.Campaign{campaign}}

	manager := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})
	candidate := onlineCandidate("candidate", "channel-1", "World of Tanks", "g1", 100)
	manager.pool = []*Channel{candidate}

	if got := manager.WatchCandidates(context.Background()); len(got) != 1 {
		t.Fatalf("baseline: channel must be proposed before the rule lands, got %d", len(got))
	}

	manager.UpdateRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("g1", "Skipped Reward"),
	}))
	if got := manager.WatchCandidates(context.Background()); len(got) != 0 {
		t.Fatalf("rule active: the channel must be abandoned, got %d proposals", len(got))
	}
}
