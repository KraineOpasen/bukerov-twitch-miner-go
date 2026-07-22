package drops

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// These tests exercise the PRODUCTION assignment path (updateStreamerCampaigns
// -> eligibility.EvaluateDrops), not the pure evaluator, so a campaign is
// assigned to a streamer only when there is a genuinely eligible current drop
// AND a coherent channel-side availability.

func unrestrictedACL() models.CampaignACL {
	return models.CampaignACL{State: models.ACLUnrestricted, Complete: true, Source: models.ACLSourceCampaignDetails}
}

func assignActiveDrop(id string) *models.Drop {
	return &models.Drop{ID: id, Name: "Reward " + id, MinutesRequired: 60, CurrentMinutesWatched: 10,
		StartAt: time.Now().Add(-time.Hour), EndAt: time.Now().Add(10 * time.Hour)}
}

func campaignFor(id string, acl models.CampaignACL, drops ...*models.Drop) *models.Campaign {
	return &models.Campaign{
		ID: id, Name: "C-" + id, Game: &models.Game{ID: "g1"}, ACL: acl,
		StartAt: time.Now().Add(-time.Hour), EndAt: time.Now().Add(10 * time.Hour),
		Drops: drops, ClaimStatus: models.CampaignClaimStatusInProgress,
	}
}

// assignmentTracker builds a one-streamer/one-campaign tracker and returns both.
func assignmentTracker(cap models.CapabilityState, campaign *models.Campaign, setup func(*models.Streamer)) (*DropsTracker, *models.Streamer) {
	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "chan-1"
	s.SetConfirmedOnline()
	s.Stream.Game = &models.Game{ID: "g1", Name: "Game"}
	if cap != models.CapabilityUnknown {
		s.SetChannelPointsCapability(cap, models.CapReasonConfirmedContext)
	}
	if setup != nil {
		setup(s)
	}
	return &DropsTracker{streamers: []*models.Streamer{s}, campaigns: []*models.Campaign{campaign}}, s
}

func assignedIDs(s *models.Streamer) []string {
	var ids []string
	for _, c := range s.Stream.GetCampaigns() {
		ids = append(ids, c.ID)
	}
	return ids
}

func assertAssigned(t *testing.T, s *models.Streamer, wantID string) {
	t.Helper()
	ids := assignedIDs(s)
	if len(ids) != 1 || ids[0] != wantID {
		t.Fatalf("expected campaign %q assigned, got %v", wantID, ids)
	}
}

func assertNotAssigned(t *testing.T, s *models.Streamer) {
	t.Helper()
	if ids := assignedIDs(s); len(ids) != 0 {
		t.Fatalf("expected no assignment, got %v", ids)
	}
}

// 1 & 2: online + unknown/disabled points + a feasible active drop with a
// resolved availability (Yes) -> assigned (Drops are not points-gated).
func TestAssignEligibleRegardlessOfPointsCapability(t *testing.T) {
	for _, cap := range []models.CapabilityState{models.CapabilityUnknown, models.CapabilityDisabled} {
		c := campaignFor("camp-1", unrestrictedACL(), assignActiveDrop("d1"))
		d, s := assignmentTracker(cap, c, func(s *models.Streamer) {
			s.Stream.SetCampaignIDs([]string{"camp-1"}) // Known + listed -> AvailabilityYes
		})
		d.updateStreamerCampaigns()
		assertAssigned(t, s, "camp-1")
	}
}

// 4: impossible-before-deadline -> not assigned.
func TestAssignImpossibleDeadline(t *testing.T) {
	drop := &models.Drop{ID: "d1", Name: "R", MinutesRequired: 600, CurrentMinutesWatched: 0,
		StartAt: time.Now().Add(-time.Hour), EndAt: time.Now().Add(30 * time.Minute)}
	c := campaignFor("camp-1", unrestrictedACL(), drop)
	c.EndAt = time.Now().Add(30 * time.Minute)
	d, s := assignmentTracker(models.CapabilityEnabled, c, func(s *models.Streamer) {
		s.Stream.SetCampaignIDs([]string{"camp-1"})
	})
	d.updateStreamerCampaigns()
	assertNotAssigned(t, s)
}

// 5: expired entitlement -> not assigned.
func TestAssignExpired(t *testing.T) {
	drop := &models.Drop{ID: "d1", Name: "R", MinutesRequired: 60,
		StartAt: time.Now().Add(-10 * time.Hour), EndAt: time.Now().Add(-time.Hour)}
	c := campaignFor("camp-1", unrestrictedACL(), drop)
	c.StartAt, c.EndAt = time.Now().Add(-10*time.Hour), time.Now().Add(-time.Hour)
	d, s := assignmentTracker(models.CapabilityEnabled, c, func(s *models.Streamer) {
		s.Stream.SetCampaignIDs([]string{"camp-1"})
	})
	d.updateStreamerCampaigns()
	assertNotAssigned(t, s)
}

// 6: upcoming entitlement -> not assigned.
func TestAssignUpcoming(t *testing.T) {
	drop := &models.Drop{ID: "d1", Name: "R", MinutesRequired: 60,
		StartAt: time.Now().Add(time.Hour), EndAt: time.Now().Add(10 * time.Hour)}
	c := campaignFor("camp-1", unrestrictedACL(), drop)
	c.StartAt = time.Now().Add(time.Hour)
	d, s := assignmentTracker(models.CapabilityEnabled, c, func(s *models.Streamer) {
		s.Stream.SetCampaignIDs([]string{"camp-1"})
	})
	d.updateStreamerCampaigns()
	assertNotAssigned(t, s)
}

// 7: authoritatively claimed drop -> not assigned.
func TestAssignClaimed(t *testing.T) {
	drop := assignActiveDrop("d1")
	drop.IsClaimed = true
	c := campaignFor("camp-1", unrestrictedACL(), drop)
	d, s := assignmentTracker(models.CapabilityEnabled, c, func(s *models.Streamer) {
		s.Stream.SetCampaignIDs([]string{"camp-1"})
	})
	d.updateStreamerCampaigns()
	assertNotAssigned(t, s)
}

// 8: ACLUnknown -> not assigned (fail closed).
func TestAssignACLUnknown(t *testing.T) {
	c := campaignFor("camp-1", models.CampaignACL{State: models.ACLUnknown, Source: models.ACLSourceCampaignDetails}, assignActiveDrop("d1"))
	d, s := assignmentTracker(models.CapabilityEnabled, c, func(s *models.Streamer) {
		s.Stream.SetCampaignIDs([]string{"camp-1"})
	})
	d.updateStreamerCampaigns()
	assertNotAssigned(t, s)
}

// 9: restricted ACL that excludes the channel -> not assigned.
func TestAssignRestrictedOutsideChannel(t *testing.T) {
	acl := models.CampaignACL{State: models.ACLRestricted, ChannelIDs: []string{"other-chan"}, Complete: true, Source: models.ACLSourceCampaignDetails}
	c := campaignFor("camp-1", acl, assignActiveDrop("d1"))
	d, s := assignmentTracker(models.CapabilityEnabled, c, func(s *models.Streamer) {
		s.Stream.SetCampaignIDs([]string{"camp-1"})
	})
	d.updateStreamerCampaigns()
	assertNotAssigned(t, s)
}

// 10: a successful EMPTY channel-side list is authoritative "not available here"
// (AvailabilityNo) -> a prior assignment is removed.
func TestAssignEmptyAvailabilityRemovesAssignment(t *testing.T) {
	c := campaignFor("camp-1", unrestrictedACL(), assignActiveDrop("d1"))
	d, s := assignmentTracker(models.CapabilityEnabled, c, func(s *models.Streamer) {
		// Previously assigned, then a resolved-empty lookup lands.
		s.Stream.SetCampaigns([]*models.Campaign{c})
		s.Stream.SetCampaignIDs([]string{}) // Known + empty -> AvailabilityNo
	})
	d.updateStreamerCampaigns()
	assertNotAssigned(t, s)
}

// 11: a lookup error after previous IDs -> AvailabilityUnknown; stale IDs must
// not create a NEW assignment.
func TestAssignUnknownAvailabilityNoNewAssignment(t *testing.T) {
	c := campaignFor("camp-1", unrestrictedACL(), assignActiveDrop("d1"))
	d, s := assignmentTracker(models.CapabilityEnabled, c, func(s *models.Streamer) {
		s.Stream.SetCampaignIDs([]string{"camp-1"}) // last-known IDs
		s.Stream.MarkCampaignAvailabilityUnknown()  // lookup failed
		// Not previously assigned.
	})
	d.updateStreamerCampaigns()
	assertNotAssigned(t, s)
}

// 12: an existing assignment survives a transient Unknown availability (bounded
// continuity), with no widening/duplication.
func TestAssignUnknownAvailabilityRetainsExisting(t *testing.T) {
	c := campaignFor("camp-1", unrestrictedACL(), assignActiveDrop("d1"))
	d, s := assignmentTracker(models.CapabilityEnabled, c, func(s *models.Streamer) {
		s.Stream.SetCampaigns([]*models.Campaign{c}) // already assigned
		s.Stream.SetCampaignIDs([]string{"camp-1"})
		s.Stream.MarkCampaignAvailabilityUnknown() // lookup now failing
	})
	d.updateStreamerCampaigns()
	assertAssigned(t, s, "camp-1") // retained by continuity
}

// 13: recovery Unknown -> resolved Yes reinstates the assignment deterministically.
func TestAssignRecoveryFromUnknown(t *testing.T) {
	c := campaignFor("camp-1", unrestrictedACL(), assignActiveDrop("d1"))
	d, s := assignmentTracker(models.CapabilityEnabled, c, func(s *models.Streamer) {
		s.Stream.SetCampaignIDs([]string{"camp-1"})
		s.Stream.MarkCampaignAvailabilityUnknown()
	})
	d.updateStreamerCampaigns()
	assertNotAssigned(t, s) // unknown + not previously assigned -> none

	// Availability recovers to a resolved Yes.
	s.Stream.SetCampaignIDs([]string{"camp-1"})
	d.updateStreamerCampaigns()
	assertAssigned(t, s, "camp-1")
}
