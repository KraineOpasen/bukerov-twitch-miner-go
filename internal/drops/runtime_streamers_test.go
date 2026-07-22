package drops

import (
	"reflect"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// runtimeGame/runtimeCampaign/runtimeStreamer build the smallest fixture on
// which updateStreamerCampaigns assigns a campaign: an unrestricted in-progress
// campaign and online ClaimDrops streamers whose live stream matches its game
// and carries its campaign ID.
func runtimeGame() *models.Game {
	return &models.Game{ID: "game-wot", Name: "World of Tanks"}
}

func runtimeCampaign() *models.Campaign {
	return &models.Campaign{
		ID:          "campaign-amd",
		Name:        "AMD Summer Arena Drops#2",
		Game:        runtimeGame(),
		ClaimStatus: models.CampaignClaimStatusInProgress,
		Drops: []*models.Drop{
			{ID: "drop-1", Name: "Garage Slot", MinutesRequired: 120, CurrentMinutesWatched: 10},
		},
	}
}

func runtimeStreamer(name, channelID string) *models.Streamer {
	s := models.NewStreamer(name, models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = channelID
	s.SetConfirmedOnline()
	s.Stream.Game = runtimeGame()
	s.Stream.SetCampaignIDs([]string{"campaign-amd"})
	return s
}

func assignedCampaignIDs(s *models.Streamer) []string {
	var ids []string
	for _, c := range s.Stream.GetCampaigns() {
		ids = append(ids, c.ID)
	}
	return ids
}

// TestUpdateStreamersRosterAdoptedBySyncPass is the delivery guard for the
// runtime-add/remove bug on the drops side: after UpdateStreamers, the very
// next assignment pass must hand campaigns to an added streamer and stop
// touching a removed one — no restart. Reverting UpdateStreamers (or having
// the pass iterate a startup snapshot) makes this fail.
func TestUpdateStreamersRosterAdoptedBySyncPass(t *testing.T) {
	campaign := runtimeCampaign()
	alpha := runtimeStreamer("alpha", "chan-1")
	beta := runtimeStreamer("beta", "chan-2")

	d := &DropsTracker{
		streamers: []*models.Streamer{alpha},
		campaigns: []*models.Campaign{campaign},
	}

	d.updateStreamerCampaigns()
	if got := assignedCampaignIDs(alpha); len(got) != 1 {
		t.Fatalf("precondition: startup streamer should be assigned the campaign, got %v", got)
	}
	if got := assignedCampaignIDs(beta); len(got) != 0 {
		t.Fatalf("precondition: beta is not tracked yet, got %v", got)
	}

	// Runtime add: beta must receive the campaign on the next pass.
	d.UpdateStreamers([]*models.Streamer{alpha, beta})
	d.updateStreamerCampaigns()
	if got := assignedCampaignIDs(beta); len(got) != 1 {
		t.Fatalf("runtime-added streamer did not receive the campaign on the next pass, got %v", got)
	}

	// Runtime remove: alpha must no longer be (re)assigned.
	alpha.Stream.SetCampaigns(nil)
	d.UpdateStreamers([]*models.Streamer{beta})
	d.updateStreamerCampaigns()
	if got := assignedCampaignIDs(alpha); len(got) != 0 {
		t.Fatalf("removed streamer still receives campaign assignments, got %v", got)
	}
	if got := assignedCampaignIDs(beta); len(got) != 1 {
		t.Fatalf("remaining streamer lost its assignment, got %v", got)
	}
}

// TestUpdateStreamersPermutationDoesNotChangeAssignments feeds a pure
// permutation (same streamers, different order, no add/remove) through
// UpdateStreamers and asserts assignments and claim-relevant campaign state
// are identical. This is the behavioral proof — on top of the audit grep —
// that the tracker keys nothing by roster position.
func TestUpdateStreamersPermutationDoesNotChangeAssignments(t *testing.T) {
	campaign := runtimeCampaign()
	alpha := runtimeStreamer("alpha", "chan-1")
	beta := runtimeStreamer("beta", "chan-2")
	gamma := runtimeStreamer("gamma", "chan-3")
	all := []*models.Streamer{alpha, beta, gamma}

	d := &DropsTracker{
		streamers: []*models.Streamer{alpha, beta, gamma},
		campaigns: []*models.Campaign{campaign},
	}

	d.updateStreamerCampaigns()
	before := make(map[string][]string, len(all))
	for _, s := range all {
		before[s.Username] = assignedCampaignIDs(s)
	}
	claimBefore := campaign.ClaimStatus
	minutesBefore := campaign.Drops[0].CurrentMinutesWatched
	claimableBefore := campaign.Drops[0].IsClaimable

	// Pure permutation of the same roster.
	d.UpdateStreamers([]*models.Streamer{gamma, alpha, beta})
	for _, s := range all {
		s.Stream.SetCampaigns(nil)
	}
	d.updateStreamerCampaigns()

	after := make(map[string][]string, len(all))
	for _, s := range all {
		after[s.Username] = assignedCampaignIDs(s)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("permuting the roster changed campaign assignments:\nbefore: %v\nafter:  %v", before, after)
	}
	if campaign.ClaimStatus != claimBefore {
		t.Fatalf("permuting the roster changed claim status: %v -> %v", claimBefore, campaign.ClaimStatus)
	}
	if campaign.Drops[0].CurrentMinutesWatched != minutesBefore || campaign.Drops[0].IsClaimable != claimableBefore {
		t.Fatal("permuting the roster changed claim-relevant drop state")
	}
}
