package drops

// Regression tests for the operator farming exclusion (DropRule.Skip →
// models.RewardSkips → UpdateRewardSkips): both automatic claim sites and the
// production assignment path must honor the exclusion, with exact canonical
// reward identity and no blocking of unrelated work. Base-defect
// reproduction: before this fix the tracker had no rules input at all, so
// every case below that asserts suppression claimed/assigned instead.

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// claimableInProgress builds an inventory dropCampaignsInProgress entry with
// one authoritatively claimable drop (instance minted, threshold met,
// unclaimed).
func claimableInProgress(campaignID, gameID, gameName, dropID, dropName string) map[string]interface{} {
	return map[string]interface{}{
		"id":   campaignID,
		"name": campaignID + " Campaign",
		"game": map[string]interface{}{"id": gameID, "name": gameName},
		"timeBasedDrops": []interface{}{
			map[string]interface{}{
				"id":                     dropID,
				"name":                   dropName,
				"requiredMinutesWatched": float64(120),
				"self": map[string]interface{}{
					"currentMinutesWatched": float64(120),
					"dropInstanceID":        "inst-" + dropID,
					"isClaimed":             false,
				},
			},
		},
	}
}

// The raw-inventory claim sweep must suppress OUR claim of a Skip-ruled
// reward — CanClaim alone never makes it auto-claimable — while an unskipped
// claimable sibling in the SAME sweep is still claimed (the suppression never
// blocks unrelated work). Identity is exact: the skipped name under another
// game is the sibling that must still be claimed. (Slow: the sibling claim
// runs the production 5s post-claim sleep.)
func TestClaimSweepSuppressesSkippedRewardButClaimsSibling(t *testing.T) {
	client := &claimRecordingClient{fakeDropsClient: &fakeDropsClient{
		inventory: inventoryWithInProgress(
			claimableInProgress("camp-skip", "game-skip", "Skip Game", "d-skip", "Twin Reward"),
			claimableInProgress("camp-want", "game-want", "Want Game", "d-want", "Twin Reward"),
		),
	}}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.UpdateRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("game-skip", "Twin Reward"),
	}))

	tracker.claimAllDropsFromInventory()

	if len(client.claimed) != 1 || client.claimed[0] != "Twin Reward" {
		t.Fatalf("exactly the unskipped same-named sibling must be claimed, got %v", client.claimed)
	}
}

// Idempotency/repeatability: repeated sweeps over a still-claimable skipped
// reward keep suppressing and never accumulate claims or errors.
func TestClaimSweepSuppressionIsRepeatable(t *testing.T) {
	client := &claimRecordingClient{fakeDropsClient: &fakeDropsClient{
		inventory: inventoryWithInProgress(
			claimableInProgress("camp-skip", "game-skip", "Skip Game", "d-skip", "Skipped Reward"),
		),
	}}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.UpdateRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("game-skip", "Skipped Reward"),
	}))

	for i := 0; i < 3; i++ {
		tracker.claimAllDropsFromInventory()
	}
	if len(client.claimed) != 0 {
		t.Fatalf("suppression must hold on every sweep, got claims %v", client.claimed)
	}
}

// The tracked-campaign claim callback (Campaign.SyncDrops → claimDropFnFor)
// is the second claim site and must suppress identically, without marking the
// drop claimed locally (Skip never rewrites server state; the reward stays
// observable and operator-claimable on Twitch).
func TestClaimDropFnSuppressesSkippedReward(t *testing.T) {
	client := &statusClient{fakeDropsClient: &fakeDropsClient{}}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	tracker.UpdateRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("game-1", "Reward d1"),
	}))

	camp := campaignWithDrop("d1", 60)
	claimed := tracker.claimDropFnFor(camp)(camp.Drops[0])
	if claimed {
		t.Fatal("a suppressed claim must not report a reconciled-claimed state")
	}
	if client.callCount() != 0 {
		t.Fatalf("ClaimDrop must not be called for a skipped reward, got %d calls", client.callCount())
	}

	// Control: clearing the rule restores the normal claim at this site.
	tracker.UpdateRewardSkips(nil)
	client.status = twitch.ClaimStatusAccepted
	if got := tracker.claimDropFnFor(camp)(camp.Drops[0]); !got {
		t.Fatal("control: the unskipped reward must claim normally")
	}
	if client.callCount() != 1 {
		t.Fatalf("control: expected exactly one claim call, got %d", client.callCount())
	}
}

// Runtime flip: a rule set through UpdateRewardSkips after construction
// (mirroring SetDropRule at runtime) takes effect on the next sweep without a
// restart; clearing it restores claiming.
func TestClaimSweepHonorsRuntimeRuleFlip(t *testing.T) {
	client := &claimRecordingClient{fakeDropsClient: &fakeDropsClient{
		inventory: inventoryWithInProgress(
			claimableInProgress("camp-1", "game-1", "Game One", "d1", "Flip Reward"),
		),
	}}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)

	tracker.UpdateRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("game-1", "Flip Reward"),
	}))
	tracker.claimAllDropsFromInventory()
	if len(client.claimed) != 0 {
		t.Fatalf("rule active: no claim expected, got %v", client.claimed)
	}

	tracker.UpdateRewardSkips(nil)
	tracker.claimAllDropsFromInventory() // Slow: real claim + 5s production sleep
	if len(client.claimed) != 1 || client.claimed[0] != "Flip Reward" {
		t.Fatalf("rule cleared: the reward must claim again, got %v", client.claimed)
	}
}

// Concurrent rule replacement races neither the claim sweep nor the
// assignment pass (-race): the tracker snapshots the immutable decision once
// per pass under its lock.
func TestRewardSkipsConcurrentUpdateWithSweepAndAssignment(t *testing.T) {
	client := &claimRecordingClient{fakeDropsClient: &fakeDropsClient{
		inventory: inventoryWithInProgress(
			claimableInProgress("camp-skip", "game-skip", "Skip Game", "d-skip", "Skipped Reward"),
		),
	}}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	skips := models.NewRewardSkips([]string{models.NormalizeRewardKey("game-skip", "Skipped Reward")})
	tracker.UpdateRewardSkips(skips)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			tracker.UpdateRewardSkips(skips)
		}
	}()
	for i := 0; i < 5; i++ {
		tracker.claimAllDropsFromInventory()
		tracker.updateStreamerCampaigns()
	}
	<-done

	if len(client.claimed) != 0 {
		t.Fatalf("the skipped reward must stay suppressed under concurrent updates, got %v", client.claimed)
	}
}

// The production assignment path must not assign a campaign whose current
// drop is Skip-ruled (the channel loses its drop justification), while an
// unskipped campaign on the same streamer stays assigned.
func TestAssignmentExcludesSkippedCampaignKeepsSibling(t *testing.T) {
	skippedDrop := assignActiveDrop("d-skip")
	skippedDrop.Name = "Skipped Reward"
	skipped := campaignFor("camp-skip", unrestrictedACL(), skippedDrop)

	wantedDrop := assignActiveDrop("d-want")
	wantedDrop.Name = "Wanted Reward"
	wanted := campaignFor("camp-want", unrestrictedACL(), wantedDrop)

	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "chan-1"
	s.SetConfirmedOnline()
	s.Stream.Game = &models.Game{ID: "g1", Name: "Game"}
	s.Stream.SetCampaignIDs([]string{"camp-skip", "camp-want"})

	d := &DropsTracker{streamers: []*models.Streamer{s}, campaigns: []*models.Campaign{skipped, wanted}}
	d.UpdateRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("g1", "Skipped Reward"),
	}))
	d.updateStreamerCampaigns()

	if ids := assignedIDs(s); len(ids) != 1 || ids[0] != "camp-want" {
		t.Fatalf("only the unskipped campaign may be assigned, got %v", ids)
	}
}

// The atomically published broker view — the account-campaign authority the
// provisional UNKNOWN bootstrap consumes for candidate minting, quarantine
// scope, and lease/proof retention — excludes a campaign whose current drop
// is Skip-ruled, exactly like the assignment it is published with, while the
// tracked source pool stays unfiltered (the reward remains observable).
func TestBrokerCampaignSnapshotExcludesSkippedCurrentDrop(t *testing.T) {
	skippedDrop := assignActiveDrop("d-skip")
	skippedDrop.Name = "Skipped Reward"
	skipped := campaignFor("camp-skip", unrestrictedACL(), skippedDrop)

	wantedDrop := assignActiveDrop("d-want")
	wantedDrop.Name = "Wanted Reward"
	wanted := campaignFor("camp-want", unrestrictedACL(), wantedDrop)

	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "chan-1"
	s.SetConfirmedOnline()
	s.Stream.Game = &models.Game{ID: "g1", Name: "Game"}
	s.Stream.SetCampaignIDs([]string{"camp-skip", "camp-want"})

	d := &DropsTracker{streamers: []*models.Streamer{s}, campaigns: []*models.Campaign{skipped, wanted}}
	d.UpdateRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("g1", "Skipped Reward"),
	}))
	d.updateStreamerCampaigns()

	broker := d.BrokerCampaigns()
	if len(broker) != 1 || broker[0].ID != "camp-want" {
		t.Fatalf("broker view must exclude the skipped campaign, got %+v", broker)
	}
	if source := d.Campaigns(); len(source) != 2 {
		t.Fatalf("the tracked source pool must stay unfiltered, got %d campaigns", len(source))
	}

	// Runtime flip back: the next assignment pass republishes the campaign.
	d.UpdateRewardSkips(nil)
	d.updateStreamerCampaigns()
	if broker := d.BrokerCampaigns(); len(broker) != 2 {
		t.Fatalf("cleared rule must restore the broker view, got %+v", broker)
	}
}

// Availability-continuity retention under UNKNOWN never bypasses the Skip
// veto: a previously assigned campaign whose current drop becomes Skip-ruled
// is released on the next pass even while the channel-side lookup is UNKNOWN
// (retained last-known IDs are continuity, not farming authority).
func TestAssignmentUnknownContinuityDoesNotBypassSkip(t *testing.T) {
	c := campaignFor("camp-1", unrestrictedACL(), assignActiveDrop("d1"))
	c.Drops[0].Name = "Skipped Reward"
	d, s := assignmentTracker(models.CapabilityEnabled, c, func(s *models.Streamer) {
		s.Stream.SetCampaigns([]*models.Campaign{c}) // already assigned
		s.Stream.SetCampaignIDs([]string{"camp-1"})
		s.Stream.MarkCampaignAvailabilityUnknown() // lookup now failing
	})
	// Control first: continuity retention keeps the unskipped assignment.
	d.updateStreamerCampaigns()
	assertAssigned(t, s, "camp-1")

	d.UpdateRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("g1", "Skipped Reward"),
	}))
	d.updateStreamerCampaigns()
	assertNotAssigned(t, s)
}

// A previously-assigned campaign is UNASSIGNED once its current drop becomes
// Skip-ruled at runtime — and a completed (claimed) skipped drop stops
// excluding the campaign, so later unskipped rewards keep farming (no
// false-positive blocking).
func TestAssignmentRuleFlipAndCompletedSkippedDrop(t *testing.T) {
	first := assignActiveDrop("d-first")
	first.Name = "First Reward"
	second := &models.Drop{ID: "d-second", Name: "Second Reward", MinutesRequired: 120, CurrentMinutesWatched: 10,
		StartAt: first.StartAt, EndAt: first.EndAt}
	c := campaignFor("camp-1", unrestrictedACL(), first, second)

	d, s := assignmentTracker(models.CapabilityEnabled, c, func(s *models.Streamer) {
		s.Stream.SetCampaignIDs([]string{"camp-1"})
	})
	d.updateStreamerCampaigns()
	assertAssigned(t, s, "camp-1")

	// Rule lands at runtime: the assignment is cleared on the next pass.
	d.UpdateRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("g1", "First Reward"),
	}))
	d.updateStreamerCampaigns()
	assertNotAssigned(t, s)

	// The skipped drop completes (threshold met + claimed): CurrentDrop moves
	// to the unskipped second reward and the campaign is assignable again.
	first.CurrentMinutesWatched = first.MinutesRequired
	first.IsClaimed = true
	d.updateStreamerCampaigns()
	assertAssigned(t, s, "camp-1")
}
