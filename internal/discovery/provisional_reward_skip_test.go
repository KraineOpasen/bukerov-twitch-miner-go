package discovery

// Integration tests for the operator farming exclusion (models.RewardSkips)
// on the provisional UNKNOWN bootstrap path (#270): a Skip-ruled current
// reward must never seed, keep, or re-justify a provisional observation
// candidate, while every #270 invariant (Known-empty veto, UNKNOWN-only cold
// start, evidence typing) stays untouched for unskipped rewards.

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func skipRuleFor(gameID, dropName string) *models.RewardSkips {
	return models.NewRewardSkips([]string{models.NormalizeRewardKey(gameID, dropName)})
}

// UNKNOWN availability with otherwise-valid provisional authority
// (Directory evidence for the open campaign, exact restricted ACL for the
// restricted one) must yield NO provisional candidate when the current reward
// is Skip-ruled — and the candidacy attempt must not disturb availability or
// assignment state.
func TestProvisionalUnknownSkippedRewardIsNotACandidate(t *testing.T) {
	for _, tc := range []struct {
		name       string
		restricted bool
	}{
		{name: "open campaign with directory evidence", restricted: false},
		{name: "restricted campaign with exact ACL", restricted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			campaign := provisionalCampaign(tc.restricted)
			m, ch := provisionalUnknownFixture(t, campaign)
			m.UpdateRewardSkips(skipRuleFor("g1", campaign.Drops[0].Name))

			if got := m.WatchCandidates(); len(got) != 0 {
				t.Fatalf("skipped reward produced %d provisional candidates, want 0", len(got))
			}
			if assigned := ch.Streamer.Stream.GetCampaigns(); len(assigned) != 0 {
				t.Fatalf("skip veto mutated confirmed assignment: %+v", assigned)
			}
			if state, ids := ch.Streamer.Stream.CampaignAvailability(); state != models.CampaignAvailabilityUnknown || len(ids) != 0 {
				t.Fatalf("skip veto mutated availability authority: state=%s ids=%v", state, ids)
			}

			// Removing the rule from the same exact reward restores
			// provisional eligibility with all other authority unchanged.
			m.UpdateRewardSkips(nil)
			got := m.WatchCandidates()
			if len(got) != 1 || got[0].ProvisionalDrop == nil {
				t.Fatalf("un-skipped reward did not regain provisional candidacy: %+v", got)
			}

			// The same reward name under ANOTHER game never collides.
			m.UpdateRewardSkips(skipRuleFor("other-game", campaign.Drops[0].Name))
			if got := m.WatchCandidates(); len(got) != 1 || got[0].ProvisionalDrop == nil {
				t.Fatalf("foreign-game rule wrongly excluded the candidate: %+v", got)
			}
		})
	}
}

// An authoritative Known-empty availability remains a hard provisional
// veto entirely independent of Skip — with the rule, without it, and with an
// unrelated rule.
func TestProvisionalKnownEmptyVetoIndependentOfSkip(t *testing.T) {
	for _, tc := range []struct {
		name  string
		skips *models.RewardSkips
	}{
		{name: "no rule", skips: nil},
		{name: "reward skipped", skips: skipRuleFor("g1", "Reward")},
		{name: "unrelated rule", skips: skipRuleFor("g1", "Other Reward")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, ch := provisionalUnknownFixture(t, provisionalCampaign(false))
			obs := ch.Streamer.Stream.BeginCampaignAvailabilityObservation()
			ch.Streamer.Stream.ApplyCampaignAvailability(obs, true, nil, time.Now())
			m.UpdateRewardSkips(tc.skips)
			if got := m.WatchCandidates(); len(got) != 0 {
				t.Fatalf("Known-empty must veto provisional candidacy regardless of Skip, got %d", len(got))
			}
		})
	}
}

// Last-known CampaignIDs retained under a later UNKNOWN are continuity
// only — they must not bypass the Skip veto (and, as the control shows, they
// do not block an unskipped provisional candidacy either).
func TestProvisionalRetainedCampaignIDsDoNotBypassSkip(t *testing.T) {
	campaign := provisionalCampaign(false)
	m, ch := provisionalUnknownFixture(t, campaign)
	// A previously Known advertisement is retained through a later UNKNOWN
	// refresh (continuity, not proof).
	obs := ch.Streamer.Stream.BeginCampaignAvailabilityObservation()
	ch.Streamer.Stream.ApplyCampaignAvailability(obs, true, []string{campaign.ID}, time.Now())
	ch.Streamer.Stream.MarkCampaignAvailabilityUnknown()
	if _, ids := ch.Streamer.Stream.CampaignAvailability(); len(ids) != 1 {
		t.Fatalf("fixture must retain last-known IDs under UNKNOWN, got %v", ids)
	}

	m.UpdateRewardSkips(skipRuleFor("g1", campaign.Drops[0].Name))
	if got := m.WatchCandidates(); len(got) != 0 {
		t.Fatalf("retained IDs bypassed the Skip veto: %d candidates", len(got))
	}

	m.UpdateRewardSkips(nil)
	if got := m.WatchCandidates(); len(got) != 1 {
		t.Fatalf("control: retained IDs must not block unskipped candidacy, got %d", len(got))
	}
}

// Concurrent rule replacement races neither the provisional proposal pass
// nor the publication fence (-race): the manager snapshots the immutable
// decision per evaluation, and with the rule published the skipped campaign
// stays excluded on every pass.
func TestProvisionalConcurrentRewardSkipsUpdate(t *testing.T) {
	campaign := provisionalCampaign(false)
	m, _ := provisionalUnknownFixture(t, campaign)
	skips := skipRuleFor("g1", campaign.Drops[0].Name)
	m.UpdateRewardSkips(skips)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			m.UpdateRewardSkips(skips)
		}
	}()
	for i := 0; i < 20; i++ {
		if got := m.WatchCandidates(); len(got) != 0 {
			t.Fatalf("skipped campaign proposed under concurrent updates: %d candidates", len(got))
		}
	}
	<-done
}

// Source side: a runtime Skip flip while the provisional proposal already
// exists stops the proposal on the very next pass, and the publication fence
// alone (provisionalCandidateStillCurrentAtSource) also refuses a candidate
// derived just before the flip.
func TestProvisionalRuntimeSkipFlipStopsProposalAndFence(t *testing.T) {
	campaign := provisionalCampaign(false)
	m, ch := provisionalUnknownFixture(t, campaign)

	got := m.WatchCandidates()
	if len(got) != 1 || got[0].ProvisionalDrop == nil {
		t.Fatalf("baseline proposal missing: %+v", got)
	}
	candidate := *got[0].ProvisionalDrop

	m.UpdateRewardSkips(skipRuleFor("g1", campaign.Drops[0].Name))
	if got := m.WatchCandidates(); len(got) != 0 {
		t.Fatalf("flip did not stop the provisional proposal: %+v", got)
	}
	if m.provisionalCandidateStillCurrentAtSource(ch, candidate, 0, false) {
		t.Fatal("publication fence accepted a candidate for a just-skipped reward")
	}
}
