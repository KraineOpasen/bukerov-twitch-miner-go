package watcher

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// pointsOnlyCapabilityBlocked unit coverage.
func TestPointsOnlyCapabilityBlocked(t *testing.T) {
	mk := func(cap models.CapabilityState, drops bool) *models.Streamer {
		s := models.NewStreamer("x", models.DefaultStreamerSettings())
		s.SetConfirmedOnline()
		if cap != models.CapabilityUnknown {
			s.SetChannelPointsCapability(cap, models.CapReasonConfirmedContext)
		}
		if drops {
			s.Stream.SetCampaignIDs([]string{"camp-1"})
		}
		return s
	}

	if !pointsOnlyCapabilityBlocked(mk(models.CapabilityDisabled, false)) {
		t.Error("disabled points-only must be blocked from a slot")
	}
	// Disabled points but has active drops -> NOT blocked (drops independent).
	if pointsOnlyCapabilityBlocked(mk(models.CapabilityDisabled, true)) {
		t.Error("disabled points + active drops must not be blocked")
	}
	if pointsOnlyCapabilityBlocked(mk(models.CapabilityUnknown, false)) {
		t.Error("unknown capability must not exclude a configured streamer")
	}
	if pointsOnlyCapabilityBlocked(mk(models.CapabilityEnabled, false)) {
		t.Error("enabled capability must not be blocked")
	}
}

// Integration: a points-only streamer with confirmed-disabled Channel Points is
// excluded from the online watch-candidate set; the two others still get slots.
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

// A disabled-points streamer that DOES have an active drop entitlement keeps its
// candidacy (Drops are never gated by points capability).
func TestGetOnlineStreamersKeepsDisabledPointsWithDrops(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.SetConfirmedOnline()
	s.OnlineAt = time.Now().Add(-time.Minute)
	s.SetChannelPointsCapability(models.CapabilityDisabled, models.CapReasonConfirmedDisabled)
	s.Stream.SetCampaignIDs([]string{"camp-1"}) // DropsCondition -> true

	if online := w.getOnlineStreamers(nil); len(online) != 1 {
		t.Fatalf("disabled-points streamer with active drops should keep its slot, got %v", online)
	}
}

// Unknown capability never excludes a configured streamer (default watch preserved),
// and the two-slot cap is unchanged.
func TestGetOnlineStreamersUnknownCapabilityKept(t *testing.T) {
	w, _ := newTestWatcher(2)
	for _, s := range w.streamers {
		s.SetConfirmedOnline()
		s.OnlineAt = time.Now().Add(-time.Minute)
		// capability left Unknown
	}
	if online := w.getOnlineStreamers(nil); len(online) != 2 {
		t.Fatalf("unknown-capability configured streamers must remain candidates, got %v", online)
	}
}
