package models

import (
	"sync"
	"testing"
	"time"
)

// TestAvailabilityNewestStartedWins verifies the observation ordering: the
// latest-BEGUN observation is authoritative regardless of completion order. An
// older observation that finishes first (or later) never publishes over a newer
// one. Uses channel barriers, no sleeps.
func TestAvailabilityNewestStartedWins(t *testing.T) {
	t0 := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

	t.Run("old finishes first, new finishes after", func(t *testing.T) {
		s := NewStream()
		old := s.BeginCampaignAvailabilityObservation()
		newer := s.BeginCampaignAvailabilityObservation()

		// old completes first with a stale result...
		rOld := s.ApplyCampaignAvailability(old, true, []string{"stale"}, t0)
		if !rOld.Stale {
			t.Fatal("older-begun observation must be Stale even when it finishes first")
		}
		// ...then the newer one completes and wins.
		rNew := s.ApplyCampaignAvailability(newer, true, []string{"fresh"}, t0.Add(time.Second))
		if !rNew.Applied {
			t.Fatal("newest-begun observation must publish")
		}
		state, ids := s.CampaignAvailability()
		if state != CampaignAvailabilityKnown || len(ids) != 1 || ids[0] != "fresh" {
			t.Fatalf("final snapshot must be from the newest observation, got %v %v", state, ids)
		}
	})

	t.Run("new finishes first, old finishes after", func(t *testing.T) {
		s := NewStream()
		old := s.BeginCampaignAvailabilityObservation()
		newer := s.BeginCampaignAvailabilityObservation()

		rNew := s.ApplyCampaignAvailability(newer, true, []string{"fresh"}, t0)
		if !rNew.Applied {
			t.Fatal("newest observation must publish")
		}
		// old completes afterwards and must NOT clobber the newer snapshot.
		rOld := s.ApplyCampaignAvailability(old, true, []string{"stale"}, t0.Add(time.Second))
		if !rOld.Stale {
			t.Fatal("older observation completing later must be Stale")
		}
		state, ids := s.CampaignAvailability()
		if state != CampaignAvailabilityKnown || len(ids) != 1 || ids[0] != "fresh" {
			t.Fatalf("final snapshot must remain the newest, got %v %v", state, ids)
		}
	})
}

// TestAvailabilityStaleCannotOverwrite verifies the directional guards: a stale
// result cannot flip state in EITHER direction, nor clear a newer populated list.
func TestAvailabilityStaleCannotOverwrite(t *testing.T) {
	t0 := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

	t.Run("stale Unknown cannot overwrite newer Known", func(t *testing.T) {
		s := NewStream()
		old := s.BeginCampaignAvailabilityObservation()
		newer := s.BeginCampaignAvailabilityObservation()
		s.ApplyCampaignAvailability(newer, true, []string{"c1"}, t0)
		if r := s.ApplyCampaignAvailability(old, false, nil, t0.Add(time.Second)); !r.Stale {
			t.Fatal("stale Unknown must be dropped")
		}
		if state, _ := s.CampaignAvailability(); state != CampaignAvailabilityKnown {
			t.Fatal("newer Known must survive a stale Unknown")
		}
	})

	t.Run("stale Known cannot overwrite newer Unknown", func(t *testing.T) {
		s := NewStream()
		old := s.BeginCampaignAvailabilityObservation()
		newer := s.BeginCampaignAvailabilityObservation()
		s.ApplyCampaignAvailability(newer, false, nil, t0)
		if r := s.ApplyCampaignAvailability(old, true, []string{"c1"}, t0.Add(time.Second)); !r.Stale {
			t.Fatal("stale Known must be dropped")
		}
		if state, _ := s.CampaignAvailability(); state != CampaignAvailabilityUnknown {
			t.Fatal("newer Unknown must survive a stale Known")
		}
	})

	t.Run("stale Known-empty cannot clear newer populated list", func(t *testing.T) {
		s := NewStream()
		old := s.BeginCampaignAvailabilityObservation()
		newer := s.BeginCampaignAvailabilityObservation()
		s.ApplyCampaignAvailability(newer, true, []string{"c1", "c2"}, t0)
		if r := s.ApplyCampaignAvailability(old, true, nil, t0.Add(time.Second)); !r.Stale {
			t.Fatal("stale Known-empty must be dropped")
		}
		if _, ids := s.CampaignAvailability(); len(ids) != 2 {
			t.Fatalf("newer populated list must survive a stale empty, got %v", ids)
		}
	})
}

// TestAvailabilityConcurrentBeginUnique verifies that two concurrently-begun
// observations receive DISTINCT ids, so the newest-started ordering is
// well-defined even under a race (the pre-fix seq model handed both the same
// value). Race-detector clean.
func TestAvailabilityConcurrentBeginUnique(t *testing.T) {
	s := NewStream()
	const n = 64
	ids := make([]uint64, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			ids[i] = s.BeginCampaignAvailabilityObservation()
		}(i)
	}
	wg.Wait()
	seen := make(map[uint64]struct{}, n)
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate observation id %d handed to two concurrent Begin calls", id)
		}
		seen[id] = struct{}{}
	}
}

// TestAvailabilityBoundedContinuity verifies the grace lifecycle around Unknown:
// within grace an Unknown streak is not "expired"; after grace it is; repeated
// Unknowns do not extend the grace; a Known+Yes recovery reinstates and resets
// the streak; and a fresh Known-empty is never expired.
func TestAvailabilityBoundedContinuity(t *testing.T) {
	base := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	grace := CampaignAvailabilityGrace

	newKnownStream := func() *Stream {
		s := NewStream()
		o := s.BeginCampaignAvailabilityObservation()
		s.ApplyCampaignAvailability(o, true, []string{"c1"}, base)
		return s
	}

	t.Run("unknown within grace not expired", func(t *testing.T) {
		s := newKnownStream()
		o := s.BeginCampaignAvailabilityObservation()
		s.ApplyCampaignAvailability(o, false, nil, base.Add(time.Minute))
		_, expired := s.CampaignAvailabilitySnapshotAt(base.Add(grace)) // exactly UnknownSince+grace? UnknownSince=base+1m
		if expired {
			t.Fatal("within grace must not be expired")
		}
	})

	t.Run("unknown after grace expired", func(t *testing.T) {
		s := newKnownStream()
		unknownAt := base.Add(time.Minute)
		o := s.BeginCampaignAvailabilityObservation()
		s.ApplyCampaignAvailability(o, false, nil, unknownAt)
		_, expired := s.CampaignAvailabilitySnapshotAt(unknownAt.Add(grace))
		if !expired {
			t.Fatal("at UnknownSince+grace the continuity must be expired")
		}
	})

	t.Run("repeated unknowns do not extend grace", func(t *testing.T) {
		s := newKnownStream()
		firstUnknown := base.Add(time.Minute)
		o1 := s.BeginCampaignAvailabilityObservation()
		s.ApplyCampaignAvailability(o1, false, nil, firstUnknown)
		// a later Unknown must NOT move UnknownSince forward.
		o2 := s.BeginCampaignAvailabilityObservation()
		s.ApplyCampaignAvailability(o2, false, nil, firstUnknown.Add(grace/2))
		snap, expired := s.CampaignAvailabilitySnapshotAt(firstUnknown.Add(grace))
		if !snap.UnknownSince.Equal(firstUnknown) {
			t.Fatalf("UnknownSince must stay the first Unknown (%v), got %v", firstUnknown, snap.UnknownSince)
		}
		if !expired {
			t.Fatal("grace measured from the FIRST unknown must have expired")
		}
	})

	t.Run("recovery Known resets streak", func(t *testing.T) {
		s := newKnownStream()
		o1 := s.BeginCampaignAvailabilityObservation()
		s.ApplyCampaignAvailability(o1, false, nil, base.Add(time.Minute))
		o2 := s.BeginCampaignAvailabilityObservation()
		s.ApplyCampaignAvailability(o2, true, []string{"c1"}, base.Add(2*time.Minute))
		snap, expired := s.CampaignAvailabilitySnapshotAt(base.Add(time.Hour))
		if snap.State != CampaignAvailabilityKnown || !snap.UnknownSince.IsZero() {
			t.Fatal("recovery to Known must reset the Unknown streak")
		}
		if expired {
			t.Fatal("a Known state is never continuity-expired")
		}
	})

	t.Run("known empty never expired", func(t *testing.T) {
		s := NewStream()
		o := s.BeginCampaignAvailabilityObservation()
		s.ApplyCampaignAvailability(o, true, nil, base) // authoritative empty
		if _, expired := s.CampaignAvailabilitySnapshotAt(base.Add(time.Hour)); expired {
			t.Fatal("a resolved Known-empty must never be continuity-expired")
		}
	})
}
