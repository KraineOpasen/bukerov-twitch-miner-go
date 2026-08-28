package watcher

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func watchSlotTestCampaign(id, channelID string, restricted bool) *models.Campaign {
	campaign := &models.Campaign{
		ID:          id,
		Name:        id,
		ClaimStatus: models.CampaignClaimStatusInProgress,
		Drops: []*models.Drop{{
			ID:              id + "-drop",
			Name:            "Reward",
			MinutesRequired: 60,
		}},
	}
	if restricted {
		campaign.Channels = []string{channelID}
	}
	return campaign
}

func newWatchSlotGateFixture() (*MinuteWatcher, []int, [2]int) {
	w, online := newTestWatcher(3)
	w.rotation.lastWatched = make(map[int]time.Time, len(w.streamers))
	now := time.Now()
	for i, s := range w.streamers {
		s.ChannelID = "channel-" + s.GetUsername()
		s.Settings.WatchStreak = false
		s.SetConfirmedOnline()
		w.rotation.lastWatched[i] = now.Add(time.Duration(i-3) * time.Minute)
	}
	w.selectionMode = ModeRotation
	w.reconcileLeastWatchedPair(online, now)
	return w, online, w.rotation.activePair
}

func TestWatchSlotDropAssignmentGate_BareAdvertisedIDCannotBoostOrDisplace(t *testing.T) {
	w, online, fairPair := newWatchSlotGateFixture()
	c := w.streamers[2]
	c.Stream.SetCampaignIDs([]string{"campaign-advertised-only"})
	if fairPair != [2]int{0, 1} {
		t.Fatalf("precondition: production fairness pair=%v, want [0 1]", fairPair)
	}

	if c.HasEligibleAssignedDropCampaign() {
		t.Fatal("precondition: an advertised ID without Stream.Campaigns must not be an assigned campaign")
	}

	reasonCode, _, _ := w.classifyWithCampaignPolicy(c, OriginConfigured, 2, CandidateCampaignPolicy{}, false)
	if reasonCode == ReasonActiveDrop || reasonCode == ReasonRestrictedDrop {
		t.Errorf("advertised-only campaign emitted false drop reason %q", reasonCode)
	}

	selected := w.applyPriorityBoost(fairPair, online)
	if selected != fairPair {
		t.Errorf("advertised-only campaign changed fair pair: got %v, want %v", selected, fairPair)
	}

	slots, _ := w.arbitrate([]int{selected[0], selected[1]}, nil, time.Now())
	if len(slots) != 2 {
		t.Fatalf("broker slot count=%d, want 2", len(slots))
	}
	for _, slot := range slots {
		if slot.streamer == c {
			t.Errorf("advertised-only channel displaced a fair-pair member: slots=%v", loginsOf(slots))
		}
	}
}

func TestWatchSlotDropAssignmentGate_ClassificationControls(t *testing.T) {
	tests := []struct {
		name           string
		configure      func(*models.Streamer)
		wantAssigned   bool
		wantDrop       bool
		wantRestricted bool
		wantReason     string
	}{
		{
			name: "assigned unfinished unrestricted",
			configure: func(s *models.Streamer) {
				s.Stream.SetCampaignIDs([]string{"assigned-active"})
				s.Stream.SetCampaigns([]*models.Campaign{watchSlotTestCampaign("assigned-active", s.ChannelID, false)})
			},
			wantAssigned: true,
			wantDrop:     true,
			wantReason:   ReasonActiveDrop,
		},
		{
			name: "advertised only known",
			configure: func(s *models.Streamer) {
				s.Stream.SetCampaignIDs([]string{"advertised-known"})
			},
			wantReason: ReasonFairRotation,
		},
		{
			name: "advertised only unknown",
			configure: func(s *models.Streamer) {
				s.Stream.SetCampaignIDs([]string{"advertised-last-known"})
				s.Stream.MarkCampaignAvailabilityUnknown()
			},
			wantReason: ReasonFairRotation,
		},
		{
			name: "assigned all thresholds met",
			configure: func(s *models.Streamer) {
				campaign := watchSlotTestCampaign("assigned-completed", s.ChannelID, false)
				campaign.Drops[0].CurrentMinutesWatched = campaign.Drops[0].MinutesRequired
				s.Stream.SetCampaignIDs([]string{campaign.ID})
				s.Stream.SetCampaigns([]*models.Campaign{campaign})
			},
			wantReason: ReasonFairRotation,
		},
		{
			name: "assigned campaign already claimed",
			configure: func(s *models.Streamer) {
				campaign := watchSlotTestCampaign("campaign-already-claimed", s.ChannelID, false)
				campaign.ClaimStatus = models.CampaignClaimStatusAlreadyClaimed
				s.Stream.SetCampaignIDs([]string{campaign.ID})
				s.Stream.SetCampaigns([]*models.Campaign{campaign})
			},
			wantReason: ReasonFairRotation,
		},
		{
			name: "assigned reward already claimed",
			configure: func(s *models.Streamer) {
				campaign := watchSlotTestCampaign("reward-already-claimed", s.ChannelID, false)
				campaign.Drops[0].IsClaimed = true
				s.Stream.SetCampaignIDs([]string{campaign.ID})
				s.Stream.SetCampaigns([]*models.Campaign{campaign})
			},
			wantReason: ReasonFairRotation,
		},
		{
			name: "completed restricted campaign",
			configure: func(s *models.Streamer) {
				campaign := watchSlotTestCampaign("completed-restricted", s.ChannelID, true)
				campaign.Drops[0].CurrentMinutesWatched = campaign.Drops[0].MinutesRequired
				s.Stream.SetCampaignIDs([]string{campaign.ID})
				s.Stream.SetCampaigns([]*models.Campaign{campaign})
			},
			wantReason: ReasonFairRotation,
		},
		{
			name: "assigned unfinished restricted",
			configure: func(s *models.Streamer) {
				s.Stream.SetCampaignIDs([]string{"assigned-restricted"})
				s.Stream.SetCampaigns([]*models.Campaign{watchSlotTestCampaign("assigned-restricted", s.ChannelID, true)})
			},
			wantAssigned:   true,
			wantDrop:       true,
			wantRestricted: true,
			wantReason:     ReasonRestrictedDrop,
		},
		{
			name: "assigned unfinished restricted with drops disabled",
			configure: func(s *models.Streamer) {
				campaign := watchSlotTestCampaign("restricted-drops-disabled", s.ChannelID, true)
				s.Stream.SetCampaignIDs([]string{campaign.ID})
				s.Stream.SetCampaigns([]*models.Campaign{campaign})
				s.Settings.ClaimDrops = false
			},
			wantAssigned: true,
			wantReason:   ReasonFairRotation,
		},
		{
			name: "assigned unfinished retained under unknown availability",
			configure: func(s *models.Streamer) {
				campaign := watchSlotTestCampaign("assigned-retained-unknown", s.ChannelID, false)
				s.Stream.SetCampaignIDs([]string{campaign.ID})
				s.Stream.SetCampaigns([]*models.Campaign{campaign})
				s.Stream.MarkCampaignAvailabilityUnknown()
			},
			wantAssigned: true,
			wantDrop:     true,
			wantReason:   ReasonActiveDrop,
		},
		{
			name:       "unrelated points candidate",
			configure:  func(*models.Streamer) {},
			wantReason: ReasonFairRotation,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w, online, fairPair := newWatchSlotGateFixture()
			c := w.streamers[2]
			tc.configure(c)

			if got := c.HasEligibleAssignedDropCampaign(); got != tc.wantAssigned {
				t.Errorf("HasEligibleAssignedDropCampaign()=%v, want %v", got, tc.wantAssigned)
			}
			if got := c.DropsCondition(); got != tc.wantDrop {
				t.Errorf("DropsCondition()=%v, want %v", got, tc.wantDrop)
			}
			if got := c.HasChannelRestrictedCampaign(); got != tc.wantRestricted {
				t.Errorf("HasChannelRestrictedCampaign()=%v, want %v", got, tc.wantRestricted)
			}

			reasonCode, _, _ := w.classifyWithCampaignPolicy(c, OriginConfigured, 2, CandidateCampaignPolicy{}, false)
			if reasonCode != tc.wantReason {
				t.Errorf("reasonCode=%q, want %q", reasonCode, tc.wantReason)
			}

			selected := w.applyPriorityBoost(fairPair, online)
			selectedC := selected[0] == 2 || selected[1] == 2
			if selectedC != tc.wantDrop {
				t.Errorf("selected pair=%v, target selected=%v, want %v", selected, selectedC, tc.wantDrop)
			}

			slots, _ := w.arbitrate([]int{selected[0], selected[1]}, nil, time.Now())
			if len(slots) != 2 {
				t.Fatalf("broker slot count=%d, want 2", len(slots))
			}
			if got := loginsOf(slots)[c.GetUsername()]; got != tc.wantDrop {
				t.Errorf("target in broker slots=%v, want %v; slots=%v", got, tc.wantDrop, loginsOf(slots))
			}
		})
	}
}
