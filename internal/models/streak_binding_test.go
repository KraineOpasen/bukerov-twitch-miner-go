package models

import (
	"testing"
	"time"
)

func onlineStreamerWithBroadcast(bid string) *Streamer {
	s := NewStreamer("orzanel", DefaultStreamerSettings())
	s.Stream.Update(bid, "title", &Game{ID: "g", Name: "G"}, nil, 10)
	s.SetOnline()
	return s
}

// T1: a short blip (< watchStreakContinuityGrace) on the same broadcast never
// re-arms — unchanged historical behavior.
func TestStreakBlipUnderGraceKeepsGrant(t *testing.T) {
	s := onlineStreamerWithBroadcast("bid-1")
	s.UpdateHistory("WATCH_STREAK", 300) // grant on bid-1

	s.SetOffline()
	s.SetOnline() // seconds later: same broadcast, no re-arm

	if s.Stream.StreakPending() {
		t.Fatal("short blip on the same broadcast must not re-arm the pursuit")
	}
}

// T2 (the production orzanel case): a blip LONGER than the grace re-arms the
// missing flag, but the broadcast-ID binding must still block the pursuit —
// the streak was already granted on this very broadcast.
func TestStreakLongBlipSameBroadcastDoesNotRePursue(t *testing.T) {
	s := onlineStreamerWithBroadcast("bid-1")
	s.UpdateHistory("WATCH_STREAK", 300)

	s.SetOffline()
	s.OfflineAt = time.Now().Add(-3 * time.Minute) // blip > 2min grace
	s.SetOnline()                                  // time-based heuristic re-arms (InitWatchStreak)

	if !s.Stream.GetWatchStreakMissing() {
		t.Fatal("precondition: long blip must re-arm the missing flag")
	}
	// The re-arm must NOT have forgotten the grant's broadcast binding.
	if bid, _ := s.Stream.StreakEarnedGrant(); bid != "bid-1" {
		t.Fatalf("InitWatchStreak erased the grant binding: %q", bid)
	}
	if s.Stream.StreakPending() {
		t.Fatal("pursuit re-armed on the SAME broadcast the streak was already granted on (orzanel case)")
	}
}

// T3: a NEW broadcast ID re-arms via Stream.Update and the pursuit is on.
func TestStreakNewBroadcastRePursues(t *testing.T) {
	s := onlineStreamerWithBroadcast("bid-1")
	s.UpdateHistory("WATCH_STREAK", 300)
	if s.Stream.StreakPending() {
		t.Fatal("precondition: granted broadcast must not be pending")
	}

	s.Stream.Update("bid-2", "title", &Game{ID: "g", Name: "G"}, nil, 10)

	if !s.Stream.GetWatchStreakMissing() {
		t.Fatal("a changed broadcast ID must re-arm the streak via Update")
	}
	if !s.Stream.StreakPending() {
		t.Fatal("new broadcast must be pursued")
	}
}

// T6: with an UNIDENTIFIED broadcast (empty ID) behavior is the historical
// one when nothing is known, and DEFERRED when a grant is remembered.
func TestStreakEmptyBroadcastFallback(t *testing.T) {
	fresh := NewStreamer("fresh", DefaultStreamerSettings())
	fresh.SetOnline()
	if !fresh.Stream.StreakPending() {
		t.Fatal("no grant + unidentified broadcast must fall back to pursuing (historical behavior)")
	}

	hydrated := NewStreamer("hydrated", DefaultStreamerSettings())
	hydrated.Stream.HydrateStreakGrant("bid-1", time.Now())
	hydrated.SetOnline()
	if hydrated.Stream.StreakPending() {
		t.Fatal("remembered grant + unidentified broadcast must DEFER pursuit until the broadcast is identified")
	}

	// Identification resolves the deferral in both directions.
	hydrated.Stream.Update("bid-1", "t", nil, nil, 1)
	if hydrated.Stream.StreakPending() {
		t.Fatal("same broadcast as the grant: still blocked")
	}
	hydrated.Stream.Update("bid-2", "t", nil, nil, 1)
	if !hydrated.Stream.StreakPending() {
		t.Fatal("different broadcast than the grant: pursuit must start")
	}
}

// Update must never clobber a known broadcast ID with an empty one.
func TestStreamUpdateKeepsKnownBroadcastID(t *testing.T) {
	s := NewStreamer("keep", DefaultStreamerSettings())
	s.Stream.Update("bid-1", "t", nil, nil, 1)
	s.Stream.Update("", "t2", nil, nil, 2)
	if got := s.Stream.GetBroadcastID(); got != "bid-1" {
		t.Fatalf("empty broadcastID clobbered a known one: %q", got)
	}
}
