package watcher

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// settleOnline confirms a streamer online and backdates OnlineAt past the 30s
// settle guard so it is immediately a watch candidate.
func settleOnline(s *models.Streamer) {
	s.SetConfirmedOnline()
	s.OnlineAt = time.Now().Add(-time.Minute)
}

// TestUnknownSlotRetention proves the watcher continuity policy: a streamer that
// was holding a slot and goes online→unknown keeps its candidacy (so it retains
// the slot), while an unknown streamer that was NOT slotted is not selected, and
// last-confirmed-offline / initial-unknown streamers are never candidates.
func TestUnknownSlotRetention(t *testing.T) {
	w, _ := newTestWatcher(3)
	held := w.streamers[0]  // was slotted, goes unknown -> must be retained
	other := w.streamers[1] // confirmed online, normal candidate
	fresh := w.streamers[2] // unknown, never slotted -> must NOT be a candidate

	settleOnline(held)
	settleOnline(other)

	// Simulate that `held` and `other` occupied slots on the previous tick.
	w.lastConfiguredWatched = map[string]*models.Streamer{
		held.Username:  held,
		other.Username: other,
	}

	// `held` now goes online -> unknown (a transient check failure).
	held.SetUnknown(models.ReasonTransportError)
	// `fresh` is unknown too but was never slotted.
	fresh.SetUnknown(models.ReasonTransportError)

	online := w.getOnlineStreamers(nil)
	set := map[int]bool{}
	for _, idx := range online {
		set[idx] = true
	}

	if !set[0] {
		t.Error("a slotted streamer that went online->unknown must be retained as a candidate")
	}
	if !set[1] {
		t.Error("the confirmed-online streamer must remain a candidate")
	}
	if set[2] {
		t.Error("an unknown streamer that was never slotted must NOT be a candidate")
	}
}

// TestUnknownRetentionExpiresBeyondGrace proves retention is bounded: once the
// unknown has persisted beyond unknownSlotRetentionGrace, the slotted streamer is
// released so a confirmed-online channel can take the slot.
func TestUnknownRetentionExpiresBeyondGrace(t *testing.T) {
	w, _ := newTestWatcher(1)
	held := w.streamers[0]
	settleOnline(held)
	w.lastConfiguredWatched = map[string]*models.Streamer{held.Username: held}

	held.SetUnknown(models.ReasonTransportError)
	// Age the unknown well beyond the retention grace.
	held.UnknownSince = time.Now().Add(-unknownSlotRetentionGrace - time.Minute)

	if online := w.getOnlineStreamers(nil); len(online) != 0 {
		t.Fatalf("a streamer unknown beyond the retention grace must be released, got %v", online)
	}
}

// TestLastConfirmedOfflineUnknownNotRetained proves retention requires the last
// confirmed status to be ONLINE: an unknown streamer whose last confirmed status
// was offline is never retained, even if it somehow appears in the prior slot set.
func TestLastConfirmedOfflineUnknownNotRetained(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.SetConfirmedOffline()
	s.SetUnknown(models.ReasonTransportError) // unknown, last-confirmed offline
	w.lastConfiguredWatched = map[string]*models.Streamer{s.Username: s}

	if online := w.getOnlineStreamers(nil); len(online) != 0 {
		t.Fatalf("an unknown streamer last confirmed offline must not be retained, got %v", online)
	}
}

// TestAuthoritativeOfflineReleasesSlot proves an authoritative offline (not an
// unknown) drops the streamer from the candidate set immediately, releasing its slot.
func TestAuthoritativeOfflineReleasesSlot(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	settleOnline(s)
	w.lastConfiguredWatched = map[string]*models.Streamer{s.Username: s}

	s.SetConfirmedOffline() // authoritative stream-down

	if online := w.getOnlineStreamers(nil); len(online) != 0 {
		t.Fatalf("an authoritatively offline streamer must be released, got %v", online)
	}
}
