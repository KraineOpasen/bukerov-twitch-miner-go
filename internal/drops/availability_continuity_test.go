package drops

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/eligibility"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// These tests drive the PRODUCTION assignment path (updateStreamerCampaigns) with
// an injectable clock to exercise the bounded-continuity grace around an Unknown
// channel-side availability. Campaign/drop windows are built relative to the same
// clock so eligibility and continuity are judged consistently.

// continuitySetup returns a tracker (with an injectable clock backed by *now), the
// streamer, and a campaign whose window comfortably contains now.
func continuitySetup(now *time.Time) (*DropsTracker, *models.Streamer, *models.Campaign) {
	// A drops-ONLY channel: Channel Points capability left Unknown, so the ONLY
	// basis for a watch slot is an eligible assigned Drops campaign. Drops are not
	// points-gated, so assignment still works.
	s := models.NewStreamer("streamer", models.StreamerSettings{ClaimDrops: true})
	s.ChannelID = "chan-1"
	s.SetConfirmedOnline()
	s.Stream.Game = &models.Game{ID: "g1", Name: "Game"}

	base := *now
	drop := &models.Drop{ID: "d1", Name: "R", MinutesRequired: 60, CurrentMinutesWatched: 10,
		StartAt: base.Add(-time.Hour), EndAt: base.Add(100 * time.Hour)}
	campaign := &models.Campaign{
		ID: "camp-1", Name: "C", Game: &models.Game{ID: "g1"},
		ACL:     models.CampaignACL{State: models.ACLUnrestricted, Complete: true, Source: models.ACLSourceCampaignDetails},
		StartAt: base.Add(-time.Hour), EndAt: base.Add(100 * time.Hour),
		Drops: []*models.Drop{drop}, ClaimStatus: models.CampaignClaimStatusInProgress,
	}

	d := &DropsTracker{
		streamers: []*models.Streamer{s},
		campaigns: []*models.Campaign{campaign},
		clock:     models.Clock(func() time.Time { return *now }),
	}
	return d, s, campaign
}

func setAvailability(s *models.Streamer, known bool, ids []string, at time.Time) {
	obs := s.Stream.BeginCampaignAvailabilityObservation()
	s.Stream.ApplyCampaignAvailability(obs, known, ids, at)
}

func hasAssignment(s *models.Streamer) bool {
	return len(s.Stream.GetCampaigns()) == 1
}

func TestContinuityUnknownWithinGraceRetains(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	now := base
	d, s, _ := continuitySetup(&now)

	// Known+Yes -> assigned.
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	if !hasAssignment(s) {
		t.Fatal("expected initial assignment under Known+Yes")
	}

	// Availability goes Unknown; advance to just under the grace.
	setAvailability(s, false, nil, now)
	now = base.Add(models.CampaignAvailabilityGrace - time.Second)
	d.updateStreamerCampaigns()
	if !hasAssignment(s) {
		t.Fatal("assignment must be RETAINED while Unknown is within the grace")
	}
	if !s.HasEligibleAssignedDropCampaign() {
		t.Fatal("watcher Drops signal must still hold within grace")
	}
}

func TestContinuityUnknownAfterGraceRemoves(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	now := base
	d, s, _ := continuitySetup(&now)

	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	if !hasAssignment(s) {
		t.Fatal("expected initial assignment")
	}

	unknownAt := now
	setAvailability(s, false, nil, unknownAt)
	now = unknownAt.Add(models.CampaignAvailabilityGrace)
	d.updateStreamerCampaigns()
	if hasAssignment(s) {
		t.Fatal("assignment must be RELEASED once the Unknown grace has elapsed")
	}
	if s.HasEligibleAssignedDropCampaign() {
		t.Fatal("watcher must lose the Drops signal after the continuity grace")
	}
	// A drops-only channel (capability Unknown) with no eligible assigned drop is
	// not a slot candidate: the watcher new-slot gate now yields false.
	if ok, _ := (eligibility.Evaluator{}).SlotCandidateEligible(s, s.HasEligibleAssignedDropCampaign()); ok {
		t.Fatal("drops-only channel must lose its watch slot after the continuity grace")
	}
}

func TestContinuityRepeatedUnknownDoesNotExtendGrace(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	now := base
	d, s, _ := continuitySetup(&now)

	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()

	firstUnknown := now
	setAvailability(s, false, nil, firstUnknown)
	// A later Unknown observation must NOT push the grace deadline out.
	now = firstUnknown.Add(models.CampaignAvailabilityGrace / 2)
	setAvailability(s, false, nil, now)
	now = firstUnknown.Add(models.CampaignAvailabilityGrace)
	d.updateStreamerCampaigns()
	if hasAssignment(s) {
		t.Fatal("grace is measured from the FIRST Unknown; a repeated Unknown must not extend it")
	}
}

func TestContinuityRecoveryReinstates(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	now := base
	d, s, _ := continuitySetup(&now)

	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()

	// Go Unknown past the grace -> dropped.
	setAvailability(s, false, nil, now)
	now = base.Add(models.CampaignAvailabilityGrace)
	d.updateStreamerCampaigns()
	if hasAssignment(s) {
		t.Fatal("precondition: assignment should be dropped after grace")
	}

	// Recover to Known+Yes -> reinstated deterministically.
	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	if !hasAssignment(s) {
		t.Fatal("a Known+Yes recovery must reinstate the assignment")
	}
}

func TestContinuityKnownNoClearsImmediately(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	now := base
	d, s, _ := continuitySetup(&now)

	setAvailability(s, true, []string{"camp-1"}, now)
	d.updateStreamerCampaigns()
	if !hasAssignment(s) {
		t.Fatal("expected initial assignment")
	}

	// Authoritative Known that does NOT list the campaign -> immediate clear.
	setAvailability(s, true, []string{"other"}, now)
	d.updateStreamerCampaigns()
	if hasAssignment(s) {
		t.Fatal("an authoritative Known+No must clear the assignment immediately (no grace)")
	}
}

func TestContinuityNoNewAssignmentDuringGrace(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	now := base
	d, s, _ := continuitySetup(&now)

	// A prior Known recorded a last-known ID, but the campaign was never assigned;
	// the lookup is now Unknown (the ID is retained only as stale continuity data).
	// The stale ID must NOT create a NEW assignment.
	setAvailability(s, true, []string{"camp-1"}, now) // last-known ID recorded
	setAvailability(s, false, nil, now)               // now Unknown, ID retained as stale
	d.updateStreamerCampaigns()
	if hasAssignment(s) {
		t.Fatal("Unknown availability must never create a NEW assignment off a stale ID")
	}
}
