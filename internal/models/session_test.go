package models

import (
	"sync"
	"testing"
)

// --- Group B: observation ordering & coherent snapshot ---

// TestSessionSnapshotCoherentAndCallerOwned: a snapshot reads spade URL, payload,
// broadcast, and generation together, and is immutable against later live
// mutations (an old payload can never be paired with a newer spade URL).
func TestSessionSnapshotCoherentAndCallerOwned(t *testing.T) {
	s := NewStream()
	s.Update("b1", "t", &Game{ID: "g", Name: "G"}, nil, 1)
	s.SetSpadeURL("https://spade.twitch.tv/track")
	s.SetPayload("cid", "b1", "uid", "chan", &Game{ID: "g", Name: "G"})

	snap := s.SessionSnapshot()
	if snap.BroadcastID != "b1" || snap.SpadeURL != "https://spade.twitch.tv/track" || !snap.HasPayload() {
		t.Fatalf("snapshot must carry a coherent session, got %+v", snap)
	}
	if !snap.Initialized() {
		t.Fatal("a brought-online session must be Initialized")
	}
	gen := snap.Generation

	// Live mutations after capture must not touch the caller-owned snapshot.
	s.SetSpadeURL("https://spade.twitch.tv/other")
	s.SetPayload("cid", "b2", "uid", "chan", nil)
	if snap.SpadeURL != "https://spade.twitch.tv/track" || snap.Generation != gen {
		t.Fatalf("snapshot must be immutable after capture, got %+v", snap)
	}
	if s.SessionGeneration() == gen {
		t.Fatal("live generation must advance after session mutations")
	}
}

// TestZeroSessionIsUninitialized: a fresh Stream's snapshot is the "unknown"
// zero value.
func TestZeroSessionIsUninitialized(t *testing.T) {
	s := NewStream()
	snap := s.SessionSnapshot()
	if snap.Initialized() || snap.HasSpadeURL() || snap.HasPayload() || snap.Generation != 0 {
		t.Fatalf("a fresh stream must be an uninitialized session, got %+v", snap)
	}
}

// TestNewBroadcastBumpsGeneration: a changed broadcast is a new session, so the
// generation advances and any beacon captured against the old one is stale.
func TestNewBroadcastBumpsGeneration(t *testing.T) {
	s := NewStream()
	s.Update("b1", "t", nil, nil, 1)
	g1 := s.SessionGeneration()
	s.Update("b1", "t", nil, nil, 2) // same broadcast: no new session
	if s.SessionGeneration() != g1 {
		t.Fatal("a same-broadcast refresh must not bump the generation")
	}
	s.Update("b2", "t", nil, nil, 3) // new broadcast
	if s.SessionGeneration() == g1 {
		t.Fatal("a new broadcast must bump the generation")
	}
}

// TestBeginSessionObservationUniqueUnder64Goroutines (B7): 64 concurrent Begin
// calls each get a unique, non-zero id.
func TestBeginSessionObservationUniqueUnder64Goroutines(t *testing.T) {
	s := NewStream()
	const n = 64
	ids := make([]uint64, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i] = s.BeginSessionObservation()
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[uint64]bool, n)
	for _, id := range ids {
		if id == 0 {
			t.Fatal("observation id must be non-zero")
		}
		if seen[id] {
			t.Fatalf("duplicate observation id %d", id)
		}
		seen[id] = true
	}
}

// TestNewestStartedObservationWins (B1/B2/B4): regardless of completion order,
// only the latest-BEGUN observation may publish the spade URL; a stale success
// cannot overwrite the newer session.
func TestNewestStartedObservationWins(t *testing.T) {
	t.Run("old completes first", func(t *testing.T) {
		s := NewStream()
		s.SetSpadeURL("https://spade.twitch.tv/orig")
		oldObs := s.BeginSessionObservation()
		newObs := s.BeginSessionObservation()

		// Old finishes first, then new.
		if s.PublishSpadeURLIfCurrent(oldObs, "https://spade.twitch.tv/old") {
			t.Fatal("the superseded older observation must not publish")
		}
		if !s.PublishSpadeURLIfCurrent(newObs, "https://spade.twitch.tv/new") {
			t.Fatal("the newest observation must publish")
		}
		if got := s.GetSpadeURL(); got != "https://spade.twitch.tv/new" {
			t.Fatalf("expected the newer session, got %q", got)
		}
	})

	t.Run("new completes first", func(t *testing.T) {
		s := NewStream()
		s.SetSpadeURL("https://spade.twitch.tv/orig")
		oldObs := s.BeginSessionObservation()
		newObs := s.BeginSessionObservation()

		// New finishes first, then old tries to overwrite it.
		if !s.PublishSpadeURLIfCurrent(newObs, "https://spade.twitch.tv/new") {
			t.Fatal("the newest observation must publish")
		}
		if s.PublishSpadeURLIfCurrent(oldObs, "https://spade.twitch.tv/old") {
			t.Fatal("a stale success cannot overwrite the newer session")
		}
		if got := s.GetSpadeURL(); got != "https://spade.twitch.tv/new" {
			t.Fatalf("expected the newer session to survive, got %q", got)
		}
	})
}

// TestNewBroadcastDuringRefreshInvalidates (B6): a new broadcast that lands
// mid-refresh bumps the generation, so a beacon captured before it is detectable
// as stale (generation changed) — the coherence gate the sender relies on.
func TestNewBroadcastDuringRefreshInvalidates(t *testing.T) {
	s := NewStream()
	s.Update("b1", "t", nil, nil, 1)
	s.SetSpadeURL("https://spade.twitch.tv/track")
	s.SetPayload("cid", "b1", "uid", "chan", nil)

	snap := s.SessionSnapshot()
	// A new broadcast arrives while a send holds `snap`.
	s.Update("b2", "t", nil, nil, 2)
	if s.SessionGeneration() == snap.Generation {
		t.Fatal("a new broadcast during a send must change the generation so the beacon is stale")
	}
}
