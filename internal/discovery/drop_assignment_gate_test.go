package discovery

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func TestDiscoveryDropAssignmentGate_RequiresKnownEligibleUnfinishedIntersection(t *testing.T) {
	tests := []struct {
		name           string
		campaign       *models.Campaign
		advertise      string
		unknown        bool
		keepGameAlive  bool
		want           bool
		wantRestricted bool
	}{
		{
			name: "known exact eligible unfinished",
			campaign: func() *models.Campaign {
				campaign := activeCampaign("g1", "World of Tanks")
				campaign.Channels = []string{"channel-1"}
				return campaign
			}(),
			advertise:      "camp-g1",
			want:           true,
			wantRestricted: true,
		},
		{
			name:      "unknown retains last known ID",
			campaign:  activeCampaign("g1", "World of Tanks"),
			advertise: "camp-g1",
			unknown:   true,
		},
		{
			name: "known completed campaign",
			campaign: func() *models.Campaign {
				campaign := activeCampaign("g1", "World of Tanks")
				campaign.Channels = []string{"channel-1"}
				campaign.Drops[0].CurrentMinutesWatched = campaign.Drops[0].MinutesRequired
				return campaign
			}(),
			advertise: "camp-g1",
		},
		{
			name: "known campaign already claimed",
			campaign: func() *models.Campaign {
				campaign := activeCampaign("g1", "World of Tanks")
				campaign.ClaimStatus = models.CampaignClaimStatusAlreadyClaimed
				return campaign
			}(),
			advertise: "camp-g1",
		},
		{
			name: "known reward already claimed",
			campaign: func() *models.Campaign {
				campaign := activeCampaign("g1", "World of Tanks")
				campaign.Drops[0].IsClaimed = true
				return campaign
			}(),
			advertise: "camp-g1",
		},
		{
			name: "known campaign missing game proof",
			campaign: func() *models.Campaign {
				campaign := activeCampaign("g1", "World of Tanks")
				campaign.Game = nil
				return campaign
			}(),
			advertise:     "camp-g1",
			keepGameAlive: true,
		},
		{
			name: "known campaign game mismatch",
			campaign: func() *models.Campaign {
				campaign := activeCampaign("g1", "World of Tanks")
				campaign.Game.ID = "different-game"
				return campaign
			}(),
			advertise:     "camp-g1",
			keepGameAlive: true,
		},
		{
			name: "known expired unfinished campaign",
			campaign: func() *models.Campaign {
				campaign := activeCampaign("g1", "World of Tanks")
				campaign.Drops[0].StartAt = time.Now().Add(-2 * time.Hour)
				campaign.Drops[0].EndAt = time.Now().Add(-time.Hour)
				return campaign
			}(),
			advertise: "camp-g1",
		},
		{
			name:      "advertised ID absent from account pool",
			campaign:  activeCampaign("g1", "World of Tanks"),
			advertise: "not-in-account-pool",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			campaigns := []*models.Campaign{tc.campaign}
			if tc.keepGameAlive {
				sentinel := activeCampaign("g1", "World of Tanks")
				sentinel.ID = "sentinel-g1"
				campaigns = append(campaigns, sentinel)
			}
			provider := &fakeCampaigns{campaigns: campaigns}
			manager := newTestManager([]string{"World of Tanks"}, provider, &fakeClient{})
			candidate := onlineCandidate("candidate", "channel-1", "World of Tanks", "g1", 100)
			candidate.Streamer.Stream.SetCampaignIDs([]string{tc.advertise})
			if tc.unknown {
				candidate.Streamer.Stream.MarkCampaignAvailabilityUnknown()
			}
			manager.pool = []*Channel{candidate}

			got := manager.WatchCandidates()
			if (len(got) == 1) != tc.want {
				t.Fatalf("proposal count=%d, want proposal=%v", len(got), tc.want)
			}
			if tc.want {
				if got[0].Streamer != candidate.Streamer {
					t.Fatalf("proposal streamer=%p, want candidate=%p", got[0].Streamer, candidate.Streamer)
				}
				if !candidate.Streamer.HasEligibleAssignedDropCampaign() {
					t.Fatal("valid discovery proposal did not publish its eligible campaign intersection")
				}
				if got := candidate.Streamer.HasChannelRestrictedCampaign(); got != tc.wantRestricted {
					t.Fatalf("restricted authority=%v, want %v", got, tc.wantRestricted)
				}
			} else if assigned := candidate.Streamer.Stream.GetCampaigns(); len(assigned) != 0 {
				t.Fatalf("ineligible discovery candidate retained assigned campaigns: %+v", assigned)
			} else if candidate.Streamer.HasChannelRestrictedCampaign() {
				t.Fatal("ineligible discovery candidate retained restricted authority")
			}
		})
	}
}
