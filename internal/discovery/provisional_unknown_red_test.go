package discovery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

func boolPointer(v bool) *bool { return &v }

type fencedProvisionalCampaigns struct {
	campaigns       []*models.Campaign
	generation      uint64
	sourceRevision  uint64
	currentRevision uint64
}

func (f *fencedProvisionalCampaigns) Campaigns() []*models.Campaign { return f.campaigns }

func (f *fencedProvisionalCampaigns) BrokerCampaignSnapshot() drops.BrokerCampaignSnapshot {
	return drops.BrokerCampaignSnapshot{
		Generation:      f.generation,
		SourceRevision:  f.sourceRevision,
		CurrentRevision: f.currentRevision,
		Campaigns:       append([]*models.Campaign(nil), f.campaigns...),
	}
}

type provisionalScopeCapture struct {
	fakeSlotStatus
	generations        []uint64
	directoryAuthority []watcher.ProvisionalDirectoryAuthority
	scopes             [][]watcher.ProvisionalQuarantineOwnerScope
}

type perGameDirectoryResult struct {
	streams []twitch.DirectoryStream
	err     error
}

type perGameDirectoryClient struct {
	results map[string]perGameDirectoryResult
}

func (f *provisionalScopeCapture) RequireProvisionalScope() {}

func (f *perGameDirectoryClient) CheckStreamerOnlineContext(_ context.Context, streamer *models.Streamer) models.StatusTransition {
	return streamer.SetConfirmedOnline()
}

func (f *perGameDirectoryClient) GetDirectoryStreams(game string, _ int) ([]twitch.DirectoryStream, error) {
	result := f.results[game]
	return result.streams, result.err
}

func (f *provisionalScopeCapture) ReconcileProvisionalQuarantine(
	generation uint64,
	directoryAuthority watcher.ProvisionalDirectoryAuthority,
	_ []watcher.ProvisionalAccountWork,
	scopes []watcher.ProvisionalQuarantineOwnerScope,
) bool {
	cloned := make([]watcher.ProvisionalQuarantineOwnerScope, len(scopes))
	for i, scope := range scopes {
		cloned[i] = scope
		cloned[i].Candidates = append([]models.ProvisionalDropCandidate(nil), scope.Candidates...)
		for j := range cloned[i].Candidates {
			cloned[i].Candidates[j].RestrictedACL = append(
				[]string(nil), cloned[i].Candidates[j].RestrictedACL...,
			)
		}
	}
	f.generations = append(f.generations, generation)
	directoryAuthority.UncertainGameIDs = append(
		[]string(nil), directoryAuthority.UncertainGameIDs...,
	)
	f.directoryAuthority = append(f.directoryAuthority, directoryAuthority)
	f.scopes = append(f.scopes, cloned)
	return true
}

// provisionalUnknownFixture builds the production cold-start shape without
// inventing channel availability: the account campaign is known, the channel
// came from a successful DROPS_ENABLED directory observation, its live session
// and game are exact, and the channel-side campaign lookup is UNKNOWN.
func provisionalUnknownFixture(t *testing.T, campaign *models.Campaign) (*Manager, *Channel) {
	t.Helper()
	client := &fakeClient{streams: []twitch.DirectoryStream{{
		Login:        "candidate",
		ChannelID:    "channel-1",
		GameID:       "g1",
		GameName:     "World of Tanks",
		Viewers:      100,
		DropsEnabled: true,
	}}}
	m := newTestManager([]string{"World of Tanks"}, &fakeCampaigns{campaigns: []*models.Campaign{campaign}}, client)
	m.syncOnce()
	if len(m.pool) != 1 {
		t.Fatalf("directory fixture produced %d candidates, want 1", len(m.pool))
	}
	ch := m.pool[0]
	ch.Streamer.SetConfirmedOnline()
	ch.Streamer.Stream.Update("broadcast-1", "title", &models.Game{ID: "g1", Name: "World of Tanks"}, nil, 100)
	obs := ch.Streamer.Stream.BeginCampaignAvailabilityObservation()
	ch.Streamer.Stream.ApplyCampaignAvailability(obs, false, nil, time.Now())
	return m, ch
}

func provisionalCampaign(restricted bool) *models.Campaign {
	c := activeCampaign("g1", "World of Tanks")
	c.Drops[0].HasPreconditionsMet = boolPointer(true)
	if restricted {
		c.ACL = models.CampaignACL{
			State:      models.ACLRestricted,
			ChannelIDs: []string{"channel-1"},
			Complete:   true,
			Source:     models.ACLSourceCampaignDetails,
		}
	} else {
		c.ACL = models.CampaignACL{
			State:    models.ACLUnrestricted,
			Complete: true,
			Source:   models.ACLSourceCampaignDetails,
		}
	}
	return c
}

func TestProvisionalQuarantineScopeIncludesQuarantinedAndUnselectedTuples(t *testing.T) {
	campaignA := provisionalCampaign(false)
	campaignB := provisionalCampaign(false)
	campaignB.ID = "camp-g1-b"
	campaignB.Name = "World of Tanks Campaign B"
	campaignB.Drops[0].ID = "drop-b"
	campaignB.Drops[0].Name = "Reward B"

	m, channel := provisionalUnknownFixture(t, campaignA)
	provider := &fencedProvisionalCampaigns{
		campaigns:       []*models.Campaign{campaignA, campaignB},
		generation:      7,
		sourceRevision:  11,
		currentRevision: 11,
	}
	m.campaigns = provider
	source := provider.BrokerCampaignSnapshot()
	all := m.provisionalCandidatesForChannelAtSource(channel, nil, source, true)
	if len(all) != 2 {
		t.Fatalf("pre-filter scope has %d tuples, want 2", len(all))
	}

	capture := &provisionalScopeCapture{fakeSlotStatus: fakeSlotStatus{
		quarantined: map[string]bool{all[0].candidate.QuarantineKey(): true},
	}}
	m.SetSlotStatus(capture)
	got := m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].ProvisionalDrop == nil ||
		got[0].ProvisionalDrop.CampaignID != campaignB.ID {
		t.Fatalf("quarantined A did not fall through to B: %+v", got)
	}
	if len(capture.scopes) != 1 || len(capture.scopes[0]) != 1 ||
		capture.scopes[0][0].Streamer != channel.Streamer || len(capture.scopes[0][0].Candidates) != 2 {
		t.Fatalf("broker did not receive the full pre-filter scope: %+v", capture.scopes)
	}
	seen := make(map[string]bool)
	for _, candidate := range capture.scopes[0][0].Candidates {
		seen[candidate.CampaignID+"/"+candidate.DropID] = true
	}
	if !seen[campaignA.ID+"/"+campaignA.Drops[0].ID] ||
		!seen[campaignB.ID+"/"+campaignB.Drops[0].ID] {
		t.Fatalf("full scope lost A or B: %+v", seen)
	}

	firstGeneration := capture.generations[0]
	m.WatchCandidates(context.Background())
	if len(capture.generations) != 2 || capture.generations[1] <= firstGeneration {
		t.Fatalf("complete scope generation did not advance strictly: %v", capture.generations)
	}
	provider.currentRevision++
	beforeStaleSource := len(capture.generations)
	m.WatchCandidates(context.Background())
	if len(capture.generations) != beforeStaleSource {
		t.Fatal("stale broker campaign source emitted a prune scope")
	}
	provider.currentRevision = provider.sourceRevision

	// A retained pool after any Directory listing error remains useful for
	// ordinary continuity. Its fenced account/roster scope may prune stale
	// RestrictedACL negatives, while the incomplete marker makes the broker
	// preserve every Directory-evidence negative.
	m.client = &fakeClient{err: errors.New("directory unavailable")}
	m.syncOnce()
	before := len(capture.generations)
	m.WatchCandidates(context.Background())
	if len(capture.generations) != before+1 {
		t.Fatal("errored Directory enumeration did not emit a fenced scope")
	}
	lastAuthority := capture.directoryAuthority[len(capture.directoryAuthority)-1]
	if lastAuthority.AllUncertain ||
		len(lastAuthority.UncertainGameIDs) != 1 || lastAuthority.UncertainGameIDs[0] != "g1" {
		t.Fatal("errored Directory enumeration did not emit an explicitly incomplete fenced scope")
	}
}

func TestProvisionalQuarantineScopeTemporaryAvoidanceDoesNotPruneLiveTuple(t *testing.T) {
	campaign := provisionalCampaign(false)
	m, channel := provisionalUnknownFixture(t, campaign)
	provider := &fencedProvisionalCampaigns{
		campaigns:       []*models.Campaign{campaign},
		generation:      1,
		sourceRevision:  1,
		currentRevision: 1,
	}
	m.campaigns = provider
	capture := &provisionalScopeCapture{}
	m.SetSlotStatus(capture)
	m.SetAvoidChecker(&staticAvoidChecker{avoided: map[string]bool{channel.Streamer.GetUsername(): true}})

	if got := m.WatchCandidates(context.Background()); len(got) != 0 {
		t.Fatalf("temporarily avoided login remained selectable: %+v", got)
	}
	if len(capture.scopes) != 1 || len(capture.scopes[0]) != 1 ||
		capture.scopes[0][0].Streamer != channel.Streamer ||
		len(capture.scopes[0][0].Candidates) != 1 {
		t.Fatalf("temporary avoidance removed live A from authoritative scope: %+v", capture.scopes)
	}
}

func TestProvisionalQuarantineScopeCarriesPerGameDirectoryUncertainty(t *testing.T) {
	gameOne := provisionalCampaign(false)
	gameOne.Game.Name = "Game One"
	gameOne.Game.DisplayName = "Game One"
	gameTwo := provisionalCampaign(false)
	gameTwo.ID = "camp-g2"
	gameTwo.Game = &models.Game{ID: "g2", Name: "Game Two", DisplayName: "Game Two"}
	gameTwo.Drops[0].ID = "drop-g2"
	provider := &fencedProvisionalCampaigns{
		campaigns:       []*models.Campaign{gameOne, gameTwo},
		generation:      1,
		sourceRevision:  1,
		currentRevision: 1,
	}
	client := &perGameDirectoryClient{results: map[string]perGameDirectoryResult{
		"Game One": {streams: []twitch.DirectoryStream{{
			Login: "successful", ChannelID: "channel-g1", GameID: "g1", GameName: "Game One", DropsEnabled: true,
		}}},
		"Game Two": {err: errors.New("game-two directory unavailable")},
	}}
	m := NewManager(
		nil, provider, &fakeTracked{}, testRateLimits(),
		[]string{"Game One", "Game Two"}, config.DiscoveryModeAll, false,
	)
	m.client = client
	m.syncOnce()
	if len(m.pool) != 1 {
		t.Fatalf("mixed Directory fixture produced %d successful rows, want 1", len(m.pool))
	}
	channel := m.pool[0]
	channel.Streamer.SetConfirmedOnline()
	channel.Streamer.Stream.Update(
		"broadcast-g1", "", &models.Game{ID: "g1", Name: "Game One"}, nil, 1,
	)
	observation := channel.Streamer.Stream.BeginCampaignAvailabilityObservation()
	channel.Streamer.Stream.ApplyCampaignAvailability(observation, false, nil, time.Now())
	capture := &provisionalScopeCapture{}
	m.SetSlotStatus(capture)
	if candidates := m.provisionalCandidatesForChannelAtSource(
		channel, nil, provider.BrokerCampaignSnapshot(), true,
	); len(candidates) != 1 {
		t.Fatalf("successful g1 row derived %d provisional candidates, want 1", len(candidates))
	}

	m.reconcileProvisionalQuarantineScope(nil)
	if len(capture.directoryAuthority) != 1 {
		t.Fatalf("mixed Directory publication emitted %d scopes, want 1", len(capture.directoryAuthority))
	}
	authority := capture.directoryAuthority[0]
	if authority.AllUncertain || len(authority.UncertainGameIDs) != 1 ||
		authority.UncertainGameIDs[0] != "g2" {
		t.Fatalf("per-game Directory authority = %+v, want only g2 uncertain", authority)
	}
	if len(capture.scopes) != 1 || len(capture.scopes[0]) != 1 ||
		len(capture.scopes[0][0].Candidates) != 1 ||
		capture.scopes[0][0].Candidates[0].GameID != "g1" {
		t.Fatalf("successful g1 scope was lost beside errored g2: %+v", capture.scopes)
	}
}

func configuredProvisionalFixture(
	t *testing.T,
	mode config.DiscoveryMode,
	campaign *models.Campaign,
	withDirectory bool,
) (*Manager, *models.Streamer) {
	t.Helper()
	streamer := models.NewStreamer("configured", models.StreamerSettings{ClaimDrops: true})
	streamer.ChannelID = "channel-1"
	streamer.SetConfirmedOnline()
	streamer.Stream.Update("broadcast-configured", "title", &models.Game{ID: "g1", Name: "World of Tanks"}, nil, 10)
	observation := streamer.Stream.BeginCampaignAvailabilityObservation()
	streamer.Stream.ApplyCampaignAvailability(observation, false, nil, time.Now())

	tracked := &fakeTracked{
		names:     []string{"configured"},
		streamers: []*models.Streamer{streamer},
	}
	client := &fakeClient{}
	games := []string{"World of Tanks"}
	if withDirectory {
		client.streams = []twitch.DirectoryStream{{
			Login: "configured", ChannelID: "channel-1", GameID: "g1",
			GameName: "World of Tanks", Viewers: 10, DropsEnabled: true,
		}}
	}
	m := NewManager(nil, &fakeCampaigns{campaigns: []*models.Campaign{campaign}}, tracked,
		testRateLimits(), games, mode, false)
	m.client = client
	if withDirectory {
		m.syncOnce()
	}
	return m, streamer
}

func TestConfiguredProvisionalUnknown_DefaultAndTrackedOnly(t *testing.T) {
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAll, config.DiscoveryModeTrackedOnly} {
		for _, restricted := range []bool{false, true} {
			name := string(mode) + "/open"
			if restricted {
				name = string(mode) + "/restricted"
			}
			t.Run(name, func(t *testing.T) {
				// Open campaigns require the retained successful Directory row.
				// Restricted campaigns are also exercised with one here so both
				// discovery modes share the same exact configured pointer path.
				m, streamer := configuredProvisionalFixture(t, mode, provisionalCampaign(restricted), true)
				before := streamer.Stream.GetCampaigns()
				got := m.WatchCandidates(context.Background())
				if len(got) != 1 || got[0].Streamer != streamer || got[0].ProvisionalDrop == nil {
					t.Fatalf("configured proposal = %+v, want one exact-pointer provisional candidate", got)
				}
				if restricted && got[0].ProvisionalDrop.Evidence != models.ProvisionalEvidenceRestrictedACL {
					t.Fatalf("restricted evidence = %+v", got[0].ProvisionalDrop)
				}
				if !restricted && (got[0].ProvisionalDrop.Evidence != models.ProvisionalEvidenceDirectory ||
					got[0].ProvisionalDrop.DirectoryObs == 0) {
					t.Fatalf("open evidence = %+v", got[0].ProvisionalDrop)
				}
				if after := streamer.Stream.GetCampaigns(); len(before) != len(after) || len(after) != 0 {
					t.Fatalf("configured provisional scan mutated Stream.Campaigns: before=%v after=%v", before, after)
				}
			})
		}
	}
}

func TestConfiguredProvisionalUnknown_RestrictedNeedsNoDirectory(t *testing.T) {
	for _, mode := range []config.DiscoveryMode{config.DiscoveryModeAll, config.DiscoveryModeTrackedOnly} {
		t.Run(string(mode), func(t *testing.T) {
			m, streamer := configuredProvisionalFixture(t, mode, provisionalCampaign(true), false)
			got := m.WatchCandidates(context.Background())
			if len(got) != 1 || got[0].Streamer != streamer || got[0].ProvisionalDrop == nil ||
				got[0].ProvisionalDrop.Evidence != models.ProvisionalEvidenceRestrictedACL ||
				got[0].ProvisionalDrop.DirectoryObs != 0 {
				t.Fatalf("restricted no-Directory proposal = %+v", got)
			}
		})
	}
}

func TestConfiguredProvisionalUnknown_EmptyDirectoryGamesRemainsDormant(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		name := "open"
		if restricted {
			name = "restricted"
		}
		t.Run(name, func(t *testing.T) {
			m, _ := configuredProvisionalFixture(t, config.DiscoveryModeAll, provisionalCampaign(restricted), true)
			m.UpdateSettings(nil, config.DiscoveryModeAll, false, testRateLimits())
			if got := m.WatchCandidates(context.Background()); len(got) != 0 {
				t.Fatalf("configured %s proposal escaped dormant DirectoryGames scope: %+v", name, got)
			}
			m.mu.RLock()
			retainedEvidence := len(m.configuredDirectory)
			m.mu.RUnlock()
			if retainedEvidence != 0 {
				t.Fatalf("removed DirectoryGames retained %d open-authority rows", retainedEvidence)
			}
		})
	}
}

func TestConfiguredProvisionalUnknown_OpenRequiresExactFreshDirectory(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		m, _ := configuredProvisionalFixture(t, config.DiscoveryModeAll, provisionalCampaign(false), false)
		if got := m.WatchCandidates(context.Background()); len(got) != 0 {
			t.Fatalf("open configured campaign without Directory evidence = %+v", got)
		}
	})

	t.Run("channel id mismatch", func(t *testing.T) {
		m, streamer := configuredProvisionalFixture(t, config.DiscoveryModeAll, provisionalCampaign(false), true)
		streamer.ChannelID = "replacement-channel"
		if got := m.WatchCandidates(context.Background()); len(got) != 0 {
			t.Fatalf("open configured campaign reused another channel identity = %+v", got)
		}
	})

	t.Run("retained row after error is stale", func(t *testing.T) {
		m, _ := configuredProvisionalFixture(t, config.DiscoveryModeAll, provisionalCampaign(false), true)
		m.client = &fakeClient{err: errors.New("directory unavailable")}
		m.syncOnce()
		if got := m.WatchCandidates(context.Background()); len(got) != 0 {
			t.Fatalf("open configured campaign reused stale Directory evidence = %+v", got)
		}
	})
}

func TestConfiguredProvisionalUnknown_KnownStatesRemainOrdinary(t *testing.T) {
	for _, test := range []struct {
		name string
		ids  []string
	}{
		{name: "known positive", ids: []string{"camp-g1"}},
		{name: "known empty"},
	} {
		t.Run(test.name, func(t *testing.T) {
			campaign := provisionalCampaign(false)
			m, streamer := configuredProvisionalFixture(t, config.DiscoveryModeAll, campaign, true)
			observation := streamer.Stream.BeginCampaignAvailabilityObservation()
			streamer.Stream.ApplyCampaignAvailability(observation, true, test.ids, time.Now())
			assigned := []*models.Campaign{campaign}
			if len(test.ids) == 0 {
				assigned = nil
			}
			streamer.Stream.SetCampaigns(assigned)

			if got := m.WatchCandidates(context.Background()); len(got) != 0 {
				t.Fatalf("configured %s leaked into provisional source: %+v", test.name, got)
			}
			after := streamer.Stream.GetCampaigns()
			if len(after) != len(assigned) || (len(after) == 1 && after[0] != campaign) {
				t.Fatalf("configured %s assignment mutated: before=%v after=%v", test.name, assigned, after)
			}
		})
	}
}

func TestMergedAvailableDropsUnknownContinuityUsesOnlyProvisionalAuthority(t *testing.T) {
	campaign := provisionalCampaign(false)
	m, streamer := configuredProvisionalFixture(t, config.DiscoveryModeAll, campaign, true)
	now := time.Now()

	known := streamer.Stream.BeginCampaignAvailabilityObservation()
	if result := streamer.Stream.ApplyCampaignAvailability(
		known, true, []string{campaign.ID}, now,
	); !result.Applied || result.State != models.CampaignAvailabilityKnown {
		t.Fatalf("initial Known publication = %+v", result)
	}
	unknown := streamer.Stream.BeginCampaignAvailabilityObservation()
	if result := streamer.Stream.ApplyCampaignAvailability(
		unknown, false, nil, now.Add(time.Second),
	); !result.Applied || result.State != models.CampaignAvailabilityUnknown {
		t.Fatalf("merged parser UNKNOWN publication = %+v", result)
	}

	state, ids := streamer.Stream.CampaignAvailability()
	if state != models.CampaignAvailabilityUnknown || len(ids) != 1 || ids[0] != campaign.ID {
		t.Fatalf("UNKNOWN continuity = state=%s ids=%v, want retained diagnostic ID", state, ids)
	}
	got := m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].Streamer != streamer || got[0].ProvisionalDrop == nil {
		t.Fatalf("UNKNOWN continuity proposal = %+v, want one provisional candidate", got)
	}
	proposal := got[0].ProvisionalDrop
	if proposal.CampaignID != campaign.ID || proposal.Evidence != models.ProvisionalEvidenceDirectory ||
		proposal.DirectoryObs == 0 {
		t.Fatalf("UNKNOWN continuity authority = %+v, want fresh Directory evidence", proposal)
	}
	if assigned := streamer.Stream.GetCampaigns(); len(assigned) != 0 {
		t.Fatalf("retained CampaignIDs became confirmed assignment: %+v", assigned)
	}

	knownEmpty := streamer.Stream.BeginCampaignAvailabilityObservation()
	if result := streamer.Stream.ApplyCampaignAvailability(
		knownEmpty, true, nil, now.Add(2*time.Second),
	); !result.Applied || result.State != models.CampaignAvailabilityKnown {
		t.Fatalf("Known-empty publication = %+v", result)
	}
	if got := m.WatchCandidates(context.Background()); len(got) != 0 {
		t.Fatalf("Known-empty produced provisional candidates: %+v", got)
	}
	state, ids = streamer.Stream.CampaignAvailability()
	if state != models.CampaignAvailabilityKnown || len(ids) != 0 {
		t.Fatalf("Known-empty authority = state=%s ids=%v", state, ids)
	}
}

func TestProvisionalQuarantineHandsOffWithinSameChannel(t *testing.T) {
	first := provisionalCampaign(false)
	first.ID = "campaign-a"
	first.Drops[0].ID = "drop-a"
	second := provisionalCampaign(false)
	second.ID = "campaign-b"
	second.Drops[0].ID = "drop-b"
	m, _ := provisionalUnknownFixture(t, first)
	m.campaigns.(*fakeCampaigns).campaigns = []*models.Campaign{first, second}

	initial := m.WatchCandidates(context.Background())
	if len(initial) != 1 || initial[0].ProvisionalDrop == nil || initial[0].ProvisionalDrop.CampaignID != first.ID {
		t.Fatalf("initial same-channel choice = %+v", initial)
	}
	m.SetSlotStatus(&fakeSlotStatus{quarantined: map[string]bool{
		initial[0].ProvisionalDrop.QuarantineKey(): true,
	}})

	got := m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].ProvisionalDrop == nil || got[0].ProvisionalDrop.CampaignID != second.ID {
		t.Fatalf("quarantined first Drop hid same-channel fallback: %+v", got)
	}
}

func TestProvisionalQuarantineHandsOffToNextChannelSameTick(t *testing.T) {
	campaign := provisionalCampaign(false)
	m, current := provisionalUnknownFixture(t, campaign)
	initial := m.WatchCandidates(context.Background())
	if len(initial) != 1 || initial[0].ProvisionalDrop == nil {
		t.Fatalf("initial proposal = %+v", initial)
	}

	backup := &Channel{
		Streamer:             newEphemeralStreamer("backup", "channel-2"),
		Game:                 "World of Tanks",
		GameID:               "g1",
		DropsEnabled:         true,
		directoryGameID:      "g1",
		directoryObservation: current.directoryObservation,
	}
	backup.Streamer.SetConfirmedOnline()
	backup.Streamer.Stream.Update("broadcast-backup", "", &models.Game{ID: "g1", Name: "World of Tanks"}, nil, 1)
	observation := backup.Streamer.Stream.BeginCampaignAvailabilityObservation()
	backup.Streamer.Stream.ApplyCampaignAvailability(observation, false, nil, time.Now())
	m.mu.Lock()
	m.pool = append(m.pool, backup)
	m.mu.Unlock()
	m.SetSlotStatus(&fakeSlotStatus{quarantined: map[string]bool{
		initial[0].ProvisionalDrop.QuarantineKey(): true,
	}})

	got := m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].Streamer != backup.Streamer || got[0].ProvisionalDrop == nil {
		t.Fatalf("quarantined current did not hand off to next channel in the same source call: %+v", got)
	}
}

func TestProvisionalQuarantineSurvivesRefreshButNotNewSession(t *testing.T) {
	m, current := provisionalUnknownFixture(t, provisionalCampaign(false))
	initial := m.WatchCandidates(context.Background())
	if len(initial) != 1 || initial[0].ProvisionalDrop == nil {
		t.Fatalf("initial proposal = %+v", initial)
	}
	m.SetSlotStatus(&fakeSlotStatus{quarantined: map[string]bool{
		initial[0].ProvisionalDrop.QuarantineKey(): true,
	}})

	observation := current.Streamer.Stream.BeginCampaignAvailabilityObservation()
	current.Streamer.Stream.ApplyCampaignAvailability(observation, false, nil, time.Now())
	if got := m.WatchCandidates(context.Background()); len(got) != 0 {
		t.Fatalf("routine UNKNOWN refresh escaped exact session quarantine: %+v", got)
	}

	current.Streamer.Stream.Update("broadcast-new", "", &models.Game{ID: "g1", Name: "World of Tanks"}, nil, 1)
	got := m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].ProvisionalDrop == nil ||
		got[0].ProvisionalDrop.BroadcastID != "broadcast-new" ||
		got[0].ProvisionalDrop.QuarantineKey() == initial[0].ProvisionalDrop.QuarantineKey() {
		t.Fatalf("new playback session was not reconsidered: %+v", got)
	}
}

func configuredUnknownStreamer(login, channelID string) *models.Streamer {
	streamer := models.NewStreamer(login, models.StreamerSettings{ClaimDrops: true})
	streamer.ChannelID = channelID
	streamer.SetConfirmedOnline()
	streamer.Stream.Update("broadcast-"+login, "", &models.Game{ID: "g1", Name: "World of Tanks"}, nil, 1)
	observation := streamer.Stream.BeginCampaignAvailabilityObservation()
	streamer.Stream.ApplyCampaignAvailability(observation, false, nil, time.Now())
	return streamer
}

func restrictedCampaignFor(id, dropID, channelID string) *models.Campaign {
	campaign := provisionalCampaign(true)
	campaign.ID = id
	campaign.Drops[0].ID = dropID
	campaign.ACL.ChannelIDs = []string{channelID}
	return campaign
}

func TestConfiguredProvisionalOrdering_StrictStrongerBeatsLeaseContinuityAcrossRosterPermutations(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := "forward"
		if reverse {
			name = "reverse"
		}
		t.Run(name, func(t *testing.T) {
			a := configuredUnknownStreamer("configured-a", "channel-a")
			b := configuredUnknownStreamer("configured-b", "channel-b")
			campaignA := restrictedCampaignFor("campaign-a", "drop-a", a.ChannelID)
			campaignB := restrictedCampaignFor("campaign-b", "drop-b", b.ChannelID)
			roster := []*models.Streamer{a, b}
			if reverse {
				roster = []*models.Streamer{b, a}
			}
			m := NewManager(nil, &fakeCampaigns{campaigns: []*models.Campaign{campaignA, campaignB}},
				&fakeTracked{names: []string{"configured-a", "configured-b"}, streamers: roster},
				testRateLimits(), []string{"World of Tanks"}, config.DiscoveryModeAll, false)
			m.client = &fakeClient{}
			m.SetCampaignPolicy(nil, map[string]policy.CampaignSemantic{
				campaignA.ID: {SemanticClass: 5, SecondaryEligible: true},
				campaignB.ID: {SemanticClass: 1, SecondaryEligible: true},
			})
			m.SetSlotStatus(&observationOwnerSlotStatus{owner: a})

			got := m.WatchCandidates(context.Background())
			if len(got) < 1 || got[0].Streamer != b || got[0].ProvisionalDrop == nil ||
				got[0].ProvisionalDrop.CampaignID != campaignB.ID {
				t.Fatalf("strict stronger configured proposal lost under %s roster: %+v", name, got)
			}

			m.SetCampaignPolicy(nil, map[string]policy.CampaignSemantic{
				campaignA.ID: {SemanticClass: 1, SecondaryEligible: true},
				campaignB.ID: {SemanticClass: 1, SecondaryEligible: true},
			})
			got = m.WatchCandidates(context.Background())
			if len(got) < 1 || got[0].Streamer != a {
				t.Fatalf("equal configured proposal displaced active lease continuity under %s roster: %+v", name, got)
			}
		})
	}
}

func TestConfiguredAndDirectoryProvisionalUseOnePolicyOrdering(t *testing.T) {
	directoryCampaign := restrictedCampaignFor("campaign-directory", "drop-directory", "channel-1")
	m, current := provisionalUnknownFixture(t, directoryCampaign)
	configured := configuredUnknownStreamer("configured", "channel-2")
	configuredCampaign := restrictedCampaignFor("campaign-configured", "drop-configured", configured.ChannelID)
	m.campaigns.(*fakeCampaigns).campaigns = []*models.Campaign{directoryCampaign, configuredCampaign}
	m.tracked = &fakeTracked{names: []string{"configured"}, streamers: []*models.Streamer{configured}}
	m.SetSlotStatus(&observationOwnerSlotStatus{owner: current.Streamer})

	m.SetCampaignPolicy(nil, map[string]policy.CampaignSemantic{
		directoryCampaign.ID:  {SemanticClass: 5, SecondaryEligible: true},
		configuredCampaign.ID: {SemanticClass: 1, SecondaryEligible: true},
	})
	got := m.WatchCandidates(context.Background())
	if len(got) < 1 || got[0].Streamer != configured {
		t.Fatalf("strict stronger configured proposal did not preempt weaker directory lease: %+v", got)
	}

	m.SetCampaignPolicy(nil, map[string]policy.CampaignSemantic{
		directoryCampaign.ID:  {SemanticClass: 1, SecondaryEligible: true},
		configuredCampaign.ID: {SemanticClass: 1, SecondaryEligible: true},
	})
	got = m.WatchCandidates(context.Background())
	if len(got) < 1 || got[0].Streamer != current.Streamer {
		t.Fatalf("equal configured proposal displaced directory continuity: %+v", got)
	}

	observation := current.Streamer.Stream.BeginCampaignAvailabilityObservation()
	current.Streamer.Stream.ApplyCampaignAvailability(observation, true, []string{directoryCampaign.ID}, time.Now())
	got = m.WatchCandidates(context.Background())
	if len(got) != 2 || got[0].Streamer != current.Streamer || got[0].ProvisionalDrop != nil ||
		got[1].Streamer != configured || got[1].ProvisionalDrop == nil {
		t.Fatalf("unproved configured proposal preempted ordinary confirmed current: %+v", got)
	}
}

func TestSameChannelCampaignPermutationRetainsExactLeaseOnlyOnTie(t *testing.T) {
	first := provisionalCampaign(false)
	first.ID = "campaign-a"
	first.Drops[0].ID = "drop-a"
	second := provisionalCampaign(false)
	second.ID = "campaign-b"
	second.Drops[0].ID = "drop-b"
	m, current := provisionalUnknownFixture(t, first)
	m.campaigns.(*fakeCampaigns).campaigns = []*models.Campaign{first, second}
	m.SetCampaignPolicy(nil, map[string]policy.CampaignSemantic{
		first.ID:  {SemanticClass: 1, SecondaryEligible: true},
		second.ID: {SemanticClass: 1, SecondaryEligible: true},
	})
	initial := m.WatchCandidates(context.Background())
	if len(initial) != 1 || initial[0].ProvisionalDrop == nil || initial[0].ProvisionalDrop.CampaignID != first.ID {
		t.Fatalf("initial equal campaign selection = %+v", initial)
	}
	owned := *initial[0].ProvisionalDrop
	status := &observationOwnerSlotStatus{owner: current.Streamer, candidate: &owned}
	m.SetSlotStatus(status)
	m.campaigns.(*fakeCampaigns).campaigns = []*models.Campaign{second, first}

	got := m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].ProvisionalDrop == nil ||
		!got[0].ProvisionalDrop.SameLeaseIdentity(owned) {
		t.Fatalf("equal campaign permutation churned exact active lease: %+v", got)
	}

	m.SetCampaignPolicy(nil, map[string]policy.CampaignSemantic{
		first.ID:  {SemanticClass: 5, SecondaryEligible: true},
		second.ID: {SemanticClass: 1, SecondaryEligible: true},
	})
	got = m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].ProvisionalDrop == nil || got[0].ProvisionalDrop.CampaignID != second.ID {
		t.Fatalf("exact lease continuity blocked strict stronger same-channel campaign: %+v", got)
	}

	status.quarantined = map[string]bool{owned.QuarantineKey(): true}
	m.SetCampaignPolicy(nil, map[string]policy.CampaignSemantic{
		first.ID:  {SemanticClass: 1, SecondaryEligible: true},
		second.ID: {SemanticClass: 1, SecondaryEligible: true},
	})
	got = m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].ProvisionalDrop == nil || got[0].ProvisionalDrop.CampaignID != second.ID {
		t.Fatalf("quarantined owned campaign hid same-channel fallback: %+v", got)
	}
}

func TestTrackedOnlyConfiguredExactOwnerReplacesKnownEphemeralClone(t *testing.T) {
	campaign := provisionalCampaign(false)
	m, owner := configuredProvisionalFixture(t, config.DiscoveryModeTrackedOnly, campaign, true)
	if len(m.pool) != 1 || m.pool[0].Streamer == owner {
		t.Fatalf("tracked_only fixture did not retain separate ephemeral clone: %+v", m.pool)
	}
	clone := m.pool[0].Streamer
	clone.SetConfirmedOnline()
	clone.Stream.Update("clone-broadcast", "", &models.Game{ID: "g1", Name: "World of Tanks"}, nil, 1)
	clone.Stream.SetCampaignIDs([]string{campaign.ID})

	got := m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].Streamer != owner || got[0].ProvisionalDrop == nil {
		t.Fatalf("Known ephemeral configured clone hid exact UNKNOWN owner: %+v", got)
	}
	if assigned := owner.Stream.GetCampaigns(); len(assigned) != 0 {
		t.Fatalf("exact configured owner assignment was mutated: %+v", assigned)
	}
}

func TestProvisionalUnknownColdStart_OpenCampaignCanReachCandidateSource(t *testing.T) {
	m, ch := provisionalUnknownFixture(t, provisionalCampaign(false))
	got := m.WatchCandidates(context.Background())
	if len(got) != 1 {
		t.Fatalf("UNKNOWN valid open campaign produced %d candidates, want one provisional observation candidate", len(got))
	}
	if assigned := ch.Streamer.Stream.GetCampaigns(); len(assigned) != 0 {
		t.Fatalf("provisional candidacy mutated confirmed assignment: %+v", assigned)
	}
	if state, ids := ch.Streamer.Stream.CampaignAvailability(); state != models.CampaignAvailabilityUnknown || len(ids) != 0 {
		t.Fatalf("provisional candidacy mutated availability authority: state=%s ids=%v", state, ids)
	}
	proposal := got[0].ProvisionalDrop
	if proposal == nil || !proposal.Valid() || proposal.Evidence != models.ProvisionalEvidenceDirectory {
		t.Fatalf("open proposal metadata = %+v, want a valid directory-evidence tuple", proposal)
	}
	if proposal.DirectoryObs == 0 || proposal.DirectoryObs != ch.directoryObservation ||
		proposal.AvailabilityObs == 0 || proposal.BroadcastID != "broadcast-1" || proposal.SessionGeneration == 0 {
		t.Fatalf("open proposal freshness fence is incomplete: %+v", proposal)
	}
}

func TestProvisionalUnknownColdStart_RestrictedCampaignCanReachCandidateSource(t *testing.T) {
	m, ch := provisionalUnknownFixture(t, provisionalCampaign(true))
	got := m.WatchCandidates(context.Background())
	if len(got) != 1 {
		t.Fatalf("UNKNOWN exact restricted ACL produced %d candidates, want one provisional observation candidate", len(got))
	}
	if assigned := ch.Streamer.Stream.GetCampaigns(); len(assigned) != 0 {
		t.Fatalf("provisional candidacy mutated confirmed assignment: %+v", assigned)
	}
	proposal := got[0].ProvisionalDrop
	if proposal == nil || !proposal.Valid() || proposal.Evidence != models.ProvisionalEvidenceRestrictedACL ||
		len(proposal.RestrictedACL) != 1 || proposal.RestrictedACL[0] != "channel-1" || proposal.DirectoryObs != 0 {
		t.Fatalf("restricted proposal metadata = %+v, want exact typed ACL evidence", proposal)
	}
}

func TestProvisionalUnknownColdStart_HardVetoMatrix(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Manager, *Channel, *models.Campaign)
	}{
		{
			name: "known empty",
			mutate: func(_ *Manager, ch *Channel, _ *models.Campaign) {
				obs := ch.Streamer.Stream.BeginCampaignAvailabilityObservation()
				ch.Streamer.Stream.ApplyCampaignAvailability(obs, true, nil, time.Now())
			},
		},
		{
			name: "wrong live game",
			mutate: func(_ *Manager, ch *Channel, _ *models.Campaign) {
				ch.Streamer.Stream.Update("broadcast-1", "title", &models.Game{ID: "other", Name: "Other"}, nil, 100)
			},
		},
		{
			name:   "offline",
			mutate: func(_ *Manager, ch *Channel, _ *models.Campaign) { ch.Streamer.SetConfirmedOffline() },
		},
		{
			name:   "preconditions false",
			mutate: func(_ *Manager, _ *Channel, c *models.Campaign) { c.Drops[0].HasPreconditionsMet = boolPointer(false) },
		},
		{
			name:   "preconditions unknown",
			mutate: func(_ *Manager, _ *Channel, c *models.Campaign) { c.Drops[0].HasPreconditionsMet = nil },
		},
		{
			name:   "restricted ACL mismatch",
			mutate: func(_ *Manager, _ *Channel, c *models.Campaign) { c.ACL.ChannelIDs = []string{"other-channel"} },
		},
		{
			name: "ACL unknown",
			mutate: func(_ *Manager, _ *Channel, c *models.Campaign) {
				c.ACL = models.CampaignACL{State: models.ACLUnknown, Source: models.ACLSourceCampaignDetails}
			},
		},
		{
			name: "ACL incomplete",
			mutate: func(_ *Manager, _ *Channel, c *models.Campaign) {
				c.ACL.Complete = false
			},
		},
		{
			name: "legacy untyped ACL",
			mutate: func(_ *Manager, _ *Channel, c *models.Campaign) {
				c.ACL = models.CampaignACL{}
				c.Channels = []string{"channel-1"}
			},
		},
		{
			name: "open directory game identity absent",
			mutate: func(m *Manager, ch *Channel, c *models.Campaign) {
				c.ACL = models.CampaignACL{
					State: models.ACLUnrestricted, Complete: true, Source: models.ACLSourceCampaignDetails,
				}
				m.mu.Lock()
				ch.directoryGameID = ""
				m.mu.Unlock()
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			campaign := provisionalCampaign(true)
			m, ch := provisionalUnknownFixture(t, campaign)
			tc.mutate(m, ch, campaign)
			if got := m.WatchCandidates(context.Background()); len(got) != 0 {
				t.Fatalf("hard veto produced %d candidates, want none", len(got))
			}
			if assigned := ch.Streamer.Stream.GetCampaigns(); len(assigned) != 0 {
				t.Fatalf("hard veto mutated confirmed assignment: %+v", assigned)
			}
		})
	}
}

func TestProvisionalUnknownColdStart_KnownPositiveBehaviorUnchanged(t *testing.T) {
	campaign := provisionalCampaign(true)
	m, ch := provisionalUnknownFixture(t, campaign)
	obs := ch.Streamer.Stream.BeginCampaignAvailabilityObservation()
	ch.Streamer.Stream.ApplyCampaignAvailability(obs, true, []string{campaign.ID}, time.Now())

	got := m.WatchCandidates(context.Background())
	if len(got) != 1 {
		t.Fatalf("known-positive campaign produced %d candidates, want 1", len(got))
	}
	assigned := ch.Streamer.Stream.GetCampaigns()
	if len(assigned) != 1 || assigned[0].ID != campaign.ID {
		t.Fatalf("known-positive assignment=%v, want %s", assigned, campaign.ID)
	}
	if got[0].ProvisionalDrop != nil {
		t.Fatalf("known-positive candidate was mislabeled provisional: %+v", got[0].ProvisionalDrop)
	}
}

func TestProvisionalUnknownColdStart_OpenDirectoryEvidenceStalesOnListingError(t *testing.T) {
	m, ch := provisionalUnknownFixture(t, provisionalCampaign(false))
	original := ch.directoryObservation
	m.client = &fakeClient{err: errors.New("directory unavailable")}
	m.syncOnce()

	if ch.directoryObservation != original || m.directoryObservation == original {
		t.Fatalf("retained row generation=%d manager=%d, want old row under a newer attempt", ch.directoryObservation, m.directoryObservation)
	}
	if got := m.WatchCandidates(context.Background()); len(got) != 0 {
		t.Fatalf("stale retained open Directory row produced %d candidates, want none", len(got))
	}
}

func TestProvisionalUnknownColdStart_RestrictedACLDoesNotNeedFreshDirectoryRow(t *testing.T) {
	m, _ := provisionalUnknownFixture(t, provisionalCampaign(true))
	m.client = &fakeClient{err: errors.New("directory unavailable")}
	m.syncOnce()

	got := m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].ProvisionalDrop == nil ||
		got[0].ProvisionalDrop.Evidence != models.ProvisionalEvidenceRestrictedACL {
		t.Fatalf("exact restricted ACL after Directory error = %+v, want one provisional candidate", got)
	}
}

func TestProvisionalUnknownColdStart_OpenCampaignRequiresDropsEnabledDirectoryRow(t *testing.T) {
	m, ch := provisionalUnknownFixture(t, provisionalCampaign(false))
	m.mu.Lock()
	ch.DropsEnabled = false
	m.mu.Unlock()

	if got := m.WatchCandidates(context.Background()); len(got) != 0 {
		t.Fatalf("open campaign without DROPS_ENABLED evidence produced %d candidates, want none", len(got))
	}
}

func TestProvisionalUnknownColdStart_OldSessionAndAvailabilityFencesCannotBeReused(t *testing.T) {
	m, ch := provisionalUnknownFixture(t, provisionalCampaign(false))
	first, ok := m.provisionalCandidateForChannel(ch, nil)
	if !ok {
		t.Fatal("initial provisional candidate was not derived")
	}

	obs := ch.Streamer.Stream.BeginCampaignAvailabilityObservation()
	ch.Streamer.Stream.ApplyCampaignAvailability(obs, false, nil, time.Now())
	if m.provisionalCandidateStillCurrent(ch, first.candidate) {
		t.Fatal("candidate survived a newer UNKNOWN availability observation")
	}
	second, ok := m.provisionalCandidateForChannel(ch, nil)
	if !ok || second.candidate.AvailabilityObs == first.candidate.AvailabilityObs {
		t.Fatalf("new availability observation did not mint a new fence: first=%+v second=%+v", first, second)
	}

	ch.Streamer.Stream.Update("broadcast-2", "title", &models.Game{ID: "g1", Name: "World of Tanks"}, nil, 100)
	if m.provisionalCandidateStillCurrent(ch, second.candidate) {
		t.Fatal("candidate survived a new broadcast/session generation")
	}
	third, ok := m.provisionalCandidateForChannel(ch, nil)
	if !ok || third.candidate.BroadcastID != "broadcast-2" ||
		third.candidate.SessionGeneration == second.candidate.SessionGeneration {
		t.Fatalf("new session did not mint a new fence: second=%+v third=%+v", second, third)
	}

	known := ch.Streamer.Stream.BeginCampaignAvailabilityObservation()
	ch.Streamer.Stream.ApplyCampaignAvailability(known, true, nil, time.Now())
	if m.provisionalCandidateStillCurrent(ch, third.candidate) {
		t.Fatal("provisional candidate survived authoritative Known-empty")
	}
}

func TestProvisionalUnknownColdStart_PreviousConfirmedAssignmentClearsAndDoesNotBootstrap(t *testing.T) {
	campaign := provisionalCampaign(false)
	m, ch := provisionalUnknownFixture(t, campaign)
	provisional, ok := m.provisionalCandidateForChannel(ch, nil)
	if !ok {
		t.Fatal("initial provisional candidate was not derived")
	}
	ch.Streamer.Stream.SetCampaigns([]*models.Campaign{campaign})
	if m.provisionalCandidateStillCurrent(ch, provisional.candidate) {
		t.Fatal("provisional candidate survived a concurrently-published confirmed assignment")
	}

	if got := m.WatchCandidates(context.Background()); len(got) != 0 {
		t.Fatalf("prior confirmed assignment produced %d provisional candidates, want none", len(got))
	}
	if assigned := ch.Streamer.Stream.GetCampaigns(); len(assigned) != 0 {
		t.Fatalf("old UNKNOWN invalidation did not clear prior assignment: %+v", assigned)
	}
}

type splitCampaignProvider struct {
	confirmed  []*models.Campaign
	broker     []*models.Campaign
	generation uint64
	revision   uint64
}

func (p *splitCampaignProvider) Campaigns() []*models.Campaign { return p.confirmed }
func (p *splitCampaignProvider) BrokerCampaignSnapshot() drops.BrokerCampaignSnapshot {
	generation, revision := p.generation, p.revision
	if generation == 0 {
		generation = 1
	}
	if revision == 0 {
		revision = 1
	}
	return drops.BrokerCampaignSnapshot{
		Generation:      generation,
		SourceRevision:  revision,
		CurrentRevision: revision,
		Campaigns:       append([]*models.Campaign(nil), p.broker...),
	}
}

func TestProvisionalUnknownColdStart_UsesBrokerAccountCampaignViewAndSemanticOrder(t *testing.T) {
	confirmed := provisionalCampaign(false)
	m, _ := provisionalUnknownFixture(t, confirmed)
	first := provisionalCampaign(false)
	first.ID = "campaign-first"
	first.Drops[0].ID = "drop-first"
	preferred := provisionalCampaign(false)
	preferred.ID = "campaign-preferred"
	preferred.Drops[0].ID = "drop-preferred"
	m.campaigns = &splitCampaignProvider{
		confirmed: []*models.Campaign{confirmed},
		broker:    []*models.Campaign{first, preferred},
	}
	m.SetCampaignPolicy(nil, map[string]policy.CampaignSemantic{
		first.ID:     {SemanticClass: 2, SecondaryEligible: true},
		preferred.ID: {SemanticClass: 1, SecondaryEligible: true},
	})

	got := m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].ProvisionalDrop == nil || got[0].ProvisionalDrop.CampaignID != preferred.ID {
		t.Fatalf("broker campaign selection = %+v, want semantically preferred %s", got, preferred.ID)
	}
}

type sequencedBrokerProvider struct {
	confirmed []*models.Campaign
	snapshots []drops.BrokerCampaignSnapshot
	calls     int
}

func (p *sequencedBrokerProvider) Campaigns() []*models.Campaign { return p.confirmed }
func (p *sequencedBrokerProvider) BrokerCampaignSnapshot() drops.BrokerCampaignSnapshot {
	index := p.calls
	p.calls++
	if index >= len(p.snapshots) {
		index = len(p.snapshots) - 1
	}
	snapshot := p.snapshots[index]
	snapshot.Campaigns = append([]*models.Campaign(nil), snapshot.Campaigns...)
	return snapshot
}

func brokerSourceSnapshot(generation uint64, campaigns ...*models.Campaign) drops.BrokerCampaignSnapshot {
	return drops.BrokerCampaignSnapshot{
		Generation:      generation,
		SourceRevision:  generation,
		CurrentRevision: generation,
		Campaigns:       campaigns,
	}
}

func TestProvisionalUnknownColdStart_FinalBrokerSourceFenceRejectsConcurrentDrift(t *testing.T) {
	tests := []struct {
		name  string
		first *models.Campaign
		next  []*models.Campaign
	}{
		{
			name:  "campaign removed",
			first: provisionalCampaign(false),
		},
		{
			name:  "restricted ACL excludes channel",
			first: provisionalCampaign(true),
			next: func() []*models.Campaign {
				changed := provisionalCampaign(true)
				changed.ACL.ChannelIDs = []string{"other-channel"}
				return []*models.Campaign{changed}
			}(),
		},
		{
			name:  "open changes to restricted",
			first: provisionalCampaign(false),
			next:  []*models.Campaign{provisionalCampaign(true)},
		},
		{
			name:  "ACL becomes incomplete",
			first: provisionalCampaign(true),
			next: func() []*models.Campaign {
				changed := provisionalCampaign(true)
				changed.ACL.Complete = false
				return []*models.Campaign{changed}
			}(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := provisionalUnknownFixture(t, tc.first)
			m.campaigns = &sequencedBrokerProvider{
				confirmed: []*models.Campaign{tc.first},
				snapshots: []drops.BrokerCampaignSnapshot{
					brokerSourceSnapshot(1, tc.first),
					brokerSourceSnapshot(2, tc.next...),
				},
			}

			if got := m.WatchCandidates(context.Background()); len(got) != 0 {
				t.Fatalf("source changed between derivation and final publication, got %+v", got)
			}
		})
	}
}

func TestProvisionalUnknownColdStart_RejectsBrokerViewBehindCurrentCampaignRevision(t *testing.T) {
	campaign := provisionalCampaign(false)
	m, _ := provisionalUnknownFixture(t, campaign)
	m.campaigns = &sequencedBrokerProvider{
		confirmed: []*models.Campaign{campaign},
		snapshots: []drops.BrokerCampaignSnapshot{{
			Generation:      1,
			SourceRevision:  1,
			CurrentRevision: 2,
			Campaigns:       []*models.Campaign{campaign},
		}},
	}

	if got := m.WatchCandidates(context.Background()); len(got) != 0 {
		t.Fatalf("stale broker view produced a provisional proposal: %+v", got)
	}
}

func TestProvisionalUnknownColdStart_OuterPublicationFenceRejectsLateBrokerDrift(t *testing.T) {
	campaign := provisionalCampaign(false)
	m, _ := provisionalUnknownFixture(t, campaign)
	m.campaigns = &sequencedBrokerProvider{
		confirmed: []*models.Campaign{campaign},
		snapshots: []drops.BrokerCampaignSnapshot{
			brokerSourceSnapshot(1, campaign), // derivation
			brokerSourceSnapshot(1, campaign), // inner final fence
			brokerSourceSnapshot(2),           // WatchCandidates return boundary
		},
	}

	if got := m.WatchCandidates(context.Background()); len(got) != 0 {
		t.Fatalf("late broker-source removal escaped the outer publication fence: %+v", got)
	}
}

type proofAwareSlotStatus struct {
	fakeSlotStatus
	owner     *models.Streamer
	proof     models.ProvisionalDropCandidate
	semantics map[string]policy.CampaignSemantic
}

func (s *proofAwareSlotStatus) HasProvisionalProof(streamer *models.Streamer, candidate models.ProvisionalDropCandidate) bool {
	return streamer != nil && streamer == s.owner && streamer.GetUsername() == candidate.Login &&
		s.proof.Valid() && s.proof.SameProofIdentity(candidate)
}

func (s *proofAwareSlotStatus) DiscoveryCampaignPolicy() (map[string]int, map[string]policy.CampaignSemantic) {
	return nil, s.semantics
}

func appendConfirmedCandidate(m *Manager, campaign *models.Campaign, login, channelID string) *Channel {
	provider := m.campaigns.(*fakeCampaigns)
	provider.campaigns = append(provider.campaigns, campaign)
	ch := onlineCandidate(login, channelID, "World of Tanks", "g1", 50)
	ch.Streamer.Stream.SetCampaignIDs([]string{campaign.ID})
	m.mu.Lock()
	m.pool = append(m.pool, ch)
	m.mu.Unlock()
	return ch
}

func TestProvisionalProofRetainsStrongerCurrentAgainstWeakerConfirmedAlternative(t *testing.T) {
	provedCampaign := provisionalCampaign(true)
	m, current := provisionalUnknownFixture(t, provedCampaign)
	initial := m.WatchCandidates(context.Background())
	if len(initial) != 1 || initial[0].ProvisionalDrop == nil {
		t.Fatalf("initial provisional proposal = %+v, want one exact tuple", initial)
	}
	proof := *initial[0].ProvisionalDrop

	weaker := provisionalCampaign(false)
	weaker.ID = "campaign-weaker"
	weaker.Name = "Weaker confirmed campaign"
	weaker.Drops[0].ID = "drop-weaker"
	appendConfirmedCandidate(m, weaker, "weaker", "channel-2")
	m.SetSlotStatus(&proofAwareSlotStatus{
		owner: current.Streamer,
		proof: proof,
		semantics: map[string]policy.CampaignSemantic{
			provedCampaign.ID: {SemanticClass: 1, SecondaryEligible: true},
			weaker.ID:         {SemanticClass: 5, SecondaryEligible: true},
		},
	})

	got := m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].Streamer != current.Streamer || got[0].ProvisionalDrop == nil ||
		!got[0].ProvisionalDrop.SameLeaseIdentity(proof) {
		t.Fatalf("proved stronger current was switched or proof tuple omitted: %+v", got)
	}
	if assigned := current.Streamer.Stream.GetCampaigns(); len(assigned) != 0 {
		t.Fatalf("proof retention mutated confirmed assignment: %+v", assigned)
	}
	if state, ids := current.Streamer.Stream.CampaignAvailability(); state != models.CampaignAvailabilityUnknown || len(ids) != 0 {
		t.Fatalf("proof retention mutated availability authority: state=%s ids=%v", state, ids)
	}
}

func TestProvisionalProofYieldsToGenuinelyStrongerConfirmedAlternative(t *testing.T) {
	provedCampaign := provisionalCampaign(false)
	m, current := provisionalUnknownFixture(t, provedCampaign)
	initial := m.WatchCandidates(context.Background())
	if len(initial) != 1 || initial[0].ProvisionalDrop == nil {
		t.Fatalf("initial provisional proposal = %+v, want one exact tuple", initial)
	}
	proof := *initial[0].ProvisionalDrop

	stronger := provisionalCampaign(false)
	stronger.ID = "campaign-stronger"
	stronger.Name = "Stronger confirmed campaign"
	stronger.Drops[0].ID = "drop-stronger"
	confirmed := appendConfirmedCandidate(m, stronger, "stronger", "channel-2")
	m.SetSlotStatus(&proofAwareSlotStatus{
		owner: current.Streamer,
		proof: proof,
		semantics: map[string]policy.CampaignSemantic{
			provedCampaign.ID: {SemanticClass: 5, SecondaryEligible: true},
			stronger.ID:       {SemanticClass: 1, SecondaryEligible: true},
		},
	})

	got := m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].Streamer != confirmed.Streamer || got[0].ProvisionalDrop != nil {
		t.Fatalf("genuinely stronger confirmed alternative did not win existing policy ordering: %+v", got)
	}
}

func TestProvisionalProofSurvivesFreshUnknownAndDirectoryEnvelope(t *testing.T) {
	campaign := provisionalCampaign(false)
	m, current := provisionalUnknownFixture(t, campaign)
	initial := m.WatchCandidates(context.Background())
	if len(initial) != 1 || initial[0].ProvisionalDrop == nil {
		t.Fatalf("initial candidate = %+v, want provisional", initial)
	}
	proof := *initial[0].ProvisionalDrop
	m.SetSlotStatus(&proofAwareSlotStatus{owner: current.Streamer, proof: proof})

	unknown := current.Streamer.Stream.BeginCampaignAvailabilityObservation()
	if result := current.Streamer.Stream.ApplyCampaignAvailability(unknown, false, nil, time.Now()); !result.Applied {
		t.Fatalf("fresh Unknown was not applied: %+v", result)
	}
	m.syncOnce()

	got := m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].Streamer != current.Streamer || got[0].ProvisionalDrop == nil {
		t.Fatalf("fresh source envelope revoked the proved target: %+v", got)
	}
	if got[0].ProvisionalDrop.SameLeaseIdentity(proof) {
		t.Fatal("fresh source envelope incorrectly reused the old baseline lease")
	}
	if !got[0].ProvisionalDrop.SameProofIdentity(proof) {
		t.Fatalf("fresh envelope changed the causal proof target: old=%+v new=%+v", proof, got[0].ProvisionalDrop)
	}
}

type observationOwnerSlotStatus struct {
	fakeSlotStatus
	owner     *models.Streamer
	candidate *models.ProvisionalDropCandidate
}

func (s *observationOwnerSlotStatus) RunRoutineRefresh(streamer *models.Streamer, refresh func()) bool {
	if streamer == nil || streamer == s.owner {
		return false
	}
	refresh()
	return true
}

func (s *observationOwnerSlotStatus) OwnsProvisionalCandidate(streamer *models.Streamer, candidate models.ProvisionalDropCandidate) bool {
	return streamer != nil && streamer == s.owner &&
		(s.candidate == nil || s.candidate.SameLeaseIdentity(candidate))
}

func TestProvisionalObservationOwnerDefersOnlyRoutineStaleRecheck(t *testing.T) {
	tests := []struct {
		name       string
		owns       bool
		wantChecks int
		wantResult bool
	}{
		{name: "broker owns bounded observation", owns: true, wantChecks: 0, wantResult: true},
		{name: "no broker ownership", owns: false, wantChecks: 1, wantResult: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, current := provisionalUnknownFixture(t, provisionalCampaign(false))
			initial := m.WatchCandidates(context.Background())
			if len(initial) != 1 || initial[0].ProvisionalDrop == nil {
				t.Fatalf("initial candidate = %+v, want provisional", initial)
			}
			status := &observationOwnerSlotStatus{}
			if tc.owns {
				status.owner = current.Streamer
			}
			m.SetSlotStatus(status)
			client := m.client.(*fakeClient)

			previousThreshold := staleStreamRecheck
			staleStreamRecheck = -time.Nanosecond
			t.Cleanup(func() { staleStreamRecheck = previousThreshold })

			got := m.WatchCandidates(context.Background())
			if len(client.checked) != tc.wantChecks {
				t.Fatalf("routine stale check count=%d, want %d", len(client.checked), tc.wantChecks)
			}
			if (len(got) == 1) != tc.wantResult {
				t.Fatalf("candidate result=%+v, want retained=%v", got, tc.wantResult)
			}
			if tc.wantResult && (got[0].Streamer != current.Streamer || got[0].ProvisionalDrop == nil) {
				t.Fatalf("owned observation did not retain exact provisional current: %+v", got)
			}
		})
	}
}

func TestProvisionalUnknownColdStart_ConfirmedAssignmentVetoesSecondPassSameBroadcast(t *testing.T) {
	campaign := provisionalCampaign(false)
	m, current := provisionalUnknownFixture(t, campaign)

	first := m.WatchCandidates(context.Background())
	if len(first) != 1 || first[0].ProvisionalDrop == nil ||
		first[0].ProvisionalDrop.CampaignID != campaign.ID {
		t.Fatalf("initial provisional proposal = %+v, want campaign %q", first, campaign.ID)
	}

	current.Streamer.Stream.SetCampaigns([]*models.Campaign{campaign})
	current.Streamer.Stream.SetCampaigns(nil)
	if !current.Streamer.Stream.ProvisionalDropSnapshot().HasConfirmedCampaign(campaign.ID) {
		t.Fatal("empty intersection erased the confirmed campaign's same-broadcast fence")
	}
	if got := m.WatchCandidates(context.Background()); len(got) != 0 {
		t.Fatalf("confirmed campaign re-entered on the second same-broadcast pass: %+v", got)
	}
}

func TestProvisionalUnknownColdStart_ConfirmedAssignmentFenceAllowsNewBroadcast(t *testing.T) {
	campaign := provisionalCampaign(false)
	m, current := provisionalUnknownFixture(t, campaign)

	current.Streamer.Stream.SetCampaigns([]*models.Campaign{campaign})
	current.Streamer.Stream.SetCampaigns(nil)
	if got := m.WatchCandidates(context.Background()); len(got) != 0 {
		t.Fatalf("confirmed campaign re-entered during its original broadcast: %+v", got)
	}

	current.Streamer.Stream.Update(
		"broadcast-2",
		"title",
		&models.Game{ID: "g1", Name: "World of Tanks"},
		nil,
		100,
	)
	if current.Streamer.Stream.ProvisionalDropSnapshot().HasConfirmedCampaign(campaign.ID) {
		t.Fatal("prior-broadcast confirmed campaign fence transferred to the new broadcast")
	}

	got := m.WatchCandidates(context.Background())
	if len(got) != 1 || got[0].ProvisionalDrop == nil ||
		got[0].ProvisionalDrop.CampaignID != campaign.ID ||
		got[0].ProvisionalDrop.BroadcastID != "broadcast-2" {
		t.Fatalf("new broadcast did not reconsider the campaign provisionally: %+v", got)
	}
}
