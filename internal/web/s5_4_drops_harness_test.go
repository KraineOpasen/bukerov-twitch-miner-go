package web

// S5-4 Drops test harness: a configurable CampaignsProvider stub that lets
// each test control SyncStatus independently (task Phase 4's R17 state
// matrix needs never-synced/failed/aged/fresh permutations that the shared
// f3Campaigns fixture in f3_harness_test.go — always fresh, always
// successful — cannot express), plus small Drop/Campaign builders for the
// authoritative-evidence fields (DropInstanceID, Claimability,
// AccountConnection) the R17/DP-C/B11/Claims logic reads.

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// s54Campaigns is a CampaignsProvider whose SyncStatus (and campaign list) a
// test sets directly, so each R17 state can be exercised in isolation.
type s54Campaigns struct {
	campaigns []*models.Campaign
	status    drops.SyncStatus
}

func (s *s54Campaigns) Campaigns() []*models.Campaign { return s.campaigns }
func (s *s54Campaigns) SyncStatus() drops.SyncStatus  { return s.status }
func (s *s54Campaigns) RequestManualSync() drops.ManualSyncResult {
	return drops.ManualSyncResult{Triggered: true, Status: s.status}
}

// s54ServerWith builds the shared F3 page server and swaps in an s54Campaigns
// stub reporting exactly the given campaigns/status — every other provider
// (discovery, policy, health, catalog, ...) stays the shared f3 fixture.
func s54ServerWith(t *testing.T, campaigns []*models.Campaign, status drops.SyncStatus) *Server {
	t.Helper()
	srv := buildF3PageServer(t)
	srv.SetCampaignsProvider(&s54Campaigns{campaigns: campaigns, status: status})
	return srv
}

// s54UnobservedDrop is a drop Twitch has never returned an inventory
// observation for at all: no minted instance, zero watched minutes. This is
// the exact R17/unknown-progress and Claims-unknown fixture — genuinely no
// authoritative evidence exists yet, so it must never render as a fabricated
// 0% or a fabricated claim state.
func s54UnobservedDrop(name string, required int) *models.Drop {
	return &models.Drop{
		Name:            name,
		Benefit:         name + " benefit",
		MinutesRequired: required,
		Claimability:    models.ClaimabilityUnknown,
	}
}

// s54ClaimableDrop is a drop Twitch authoritatively marked ready to claim (a
// minted instance, ClaimabilityKnownTrue), not yet claimed.
func s54ClaimableDrop(name string, watched, required int) *models.Drop {
	return &models.Drop{
		Name:                  name,
		Benefit:               name + " benefit",
		MinutesRequired:       required,
		CurrentMinutesWatched: watched,
		DropInstanceID:        "inst-" + name,
		Claimability:          models.ClaimabilityKnownTrue,
	}
}

// s54InProgressDrop is a drop Twitch HAS observed (a minted instance, a real
// watched-minute reading) but has NOT authoritatively marked claimable yet —
// still earning. Distinct from s54UnobservedDrop: this one has real evidence,
// just not a positive claimable signal.
func s54InProgressDrop(name string, watched, required int) *models.Drop {
	return &models.Drop{
		Name:                  name,
		Benefit:               name + " benefit",
		MinutesRequired:       required,
		CurrentMinutesWatched: watched,
		DropInstanceID:        "inst-" + name,
		Claimability:          models.ClaimabilityKnownFalse,
	}
}

// s54ClaimedDrop is a drop Twitch has authoritatively confirmed claimed.
func s54ClaimedDrop(name string, required int) *models.Drop {
	return &models.Drop{
		Name:            name,
		Benefit:         name + " benefit",
		MinutesRequired: required,
		IsClaimed:       true,
		Claimability:    models.ClaimabilityKnownFalse,
	}
}

// s54Campaign builds a campaign with the given drops, in progress (not yet
// claimed at the campaign level) and unrestricted (no Channels), so tests
// only need to set the fields their scenario actually cares about.
func s54Campaign(id, name, game string, ds []*models.Drop) *models.Campaign {
	return &models.Campaign{
		ID:          id,
		Name:        name,
		Game:        &models.Game{ID: "g-" + id, Name: game, DisplayName: game},
		StartAt:     time.Now().Add(-24 * time.Hour),
		EndAt:       time.Now().Add(24 * time.Hour),
		Drops:       ds,
		ClaimStatus: models.CampaignClaimStatusInProgress,
	}
}
