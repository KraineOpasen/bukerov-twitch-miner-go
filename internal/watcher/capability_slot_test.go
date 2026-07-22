package watcher

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// The watcher's new-slot candidacy is decided by the single centralized policy
// (eligibility.SlotCandidateEligible). These integration tests drive the real
// getOnlineStreamers so there is no second, divergent capability policy.

// Confirmed-disabled points-only channel is excluded from a new slot.
func TestGetOnlineStreamersExcludesDisabledPointsOnly(t *testing.T) {
	w, _ := newTestWatcher(3)
	for _, s := range w.streamers {
		s.SetConfirmedOnline()
		s.OnlineAt = time.Now().Add(-time.Minute)
	}
	w.streamers[1].SetChannelPointsCapability(models.CapabilityDisabled, models.CapReasonConfirmedDisabled)

	online := w.getOnlineStreamers(nil)
	if len(online) != 2 {
		t.Fatalf("expected 2 candidates (disabled points-only excluded), got %d (%v)", len(online), online)
	}
	for _, idx := range online {
		if idx == 1 {
			t.Fatal("disabled points-only streamer must be excluded")
		}
	}
}

// Unknown-capability points-only channel gets NO new slot (strict policy).
func TestGetOnlineStreamersExcludesUnknownPointsOnly(t *testing.T) {
	w, _ := newTestWatcher(3)
	for _, s := range w.streamers {
		s.SetConfirmedOnline()
		s.OnlineAt = time.Now().Add(-time.Minute)
	}
	// Force streamer 1 back to Unknown capability (newTestWatcher marks Enabled).
	w.streamers[1].SetChannelPointsCapability(models.CapabilityUnknown, models.CapReasonTransportError)

	online := w.getOnlineStreamers(nil)
	for _, idx := range online {
		if idx == 1 {
			t.Fatal("unknown-capability points-only streamer must get no new slot")
		}
	}
	if len(online) != 2 {
		t.Fatalf("expected 2 candidates, got %d (%v)", len(online), online)
	}
}

// A disabled/unknown-points channel that DOES have an active drop entitlement
// keeps its candidacy (Drops are never gated by points capability).
func TestGetOnlineStreamersKeepsDisabledPointsWithDrops(t *testing.T) {
	for _, cap := range []models.CapabilityState{models.CapabilityDisabled, models.CapabilityUnknown} {
		w, _ := newTestWatcher(1)
		s := w.streamers[0]
		s.SetConfirmedOnline()
		s.OnlineAt = time.Now().Add(-time.Minute)
		s.SetChannelPointsCapability(cap, models.CapReasonConfirmedDisabled)
		// An ELIGIBLE ASSIGNED drop campaign (with a watchable current drop) — the
		// production-evaluated signal, not a bare advertised ID.
		s.Stream.SetCampaigns([]*models.Campaign{{
			ID:    "camp-1",
			Drops: []*models.Drop{{ID: "d1", Name: "Reward", MinutesRequired: 60}},
		}})

		if online := w.getOnlineStreamers(nil); len(online) != 1 {
			t.Fatalf("cap=%v: streamer with an eligible assigned drop should keep its slot, got %v", cap, online)
		}
	}
}

// A bare advertised campaign ID (no eligible ASSIGNED campaign) does NOT earn a
// slot for a disabled/unknown-points channel — only a production-evaluated
// assignment does.
func TestGetOnlineStreamersStaleCampaignIDGetsNoSlot(t *testing.T) {
	for _, cap := range []models.CapabilityState{models.CapabilityDisabled, models.CapabilityUnknown} {
		w, _ := newTestWatcher(1)
		s := w.streamers[0]
		s.SetConfirmedOnline()
		s.OnlineAt = time.Now().Add(-time.Minute)
		s.SetChannelPointsCapability(cap, models.CapReasonConfirmedDisabled)
		s.Stream.SetCampaignIDs([]string{"camp-1"}) // advertised only, never assigned

		if online := w.getOnlineStreamers(nil); len(online) != 0 {
			t.Fatalf("cap=%v: a stale advertised ID must not earn a slot, got %v", cap, online)
		}
	}
}

// Enabled points-only channel keeps its default slot; the two-slot cap holds.
func TestGetOnlineStreamersEnabledPointsKept(t *testing.T) {
	w, _ := newTestWatcher(2) // newTestWatcher marks both Enabled
	for _, s := range w.streamers {
		s.SetConfirmedOnline()
		s.OnlineAt = time.Now().Add(-time.Minute)
	}
	if online := w.getOnlineStreamers(nil); len(online) != 2 {
		t.Fatalf("enabled points-only configured streamers must remain candidates, got %v", online)
	}
}

// Two-slot invariant holds under a mix of points-enabled and drops-eligible
// (disabled-points) candidates: selection never exceeds MaxSimultaneousStreams.
func TestMaxSlotsUnderMixedCandidates(t *testing.T) {
	w, _ := newTestWatcher(3)
	// streamer 0,1: enabled points. streamer 2: disabled points but an eligible
	// assigned drop.
	for _, s := range w.streamers {
		s.SetConfirmedOnline()
		s.OnlineAt = time.Now().Add(-time.Minute)
	}
	w.streamers[2].SetChannelPointsCapability(models.CapabilityDisabled, models.CapReasonConfirmedDisabled)
	w.streamers[2].Stream.SetCampaigns([]*models.Campaign{{
		ID: "camp-1", Drops: []*models.Drop{{ID: "d1", Name: "R", MinutesRequired: 60}},
	}})

	online := w.getOnlineStreamers(nil)
	if len(online) != 3 {
		t.Fatalf("all three should be eligible candidates, got %d", len(online))
	}
	watched := w.selectStreamersToWatch(online)
	if len(watched) > 2 {
		t.Fatalf("watch selection must never exceed two slots, got %d", len(watched))
	}
}
