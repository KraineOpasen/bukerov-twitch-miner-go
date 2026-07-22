package models

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// --- Corrective pass: atomic session publication (models level) ---

// sessionTuple is the coherent triple a snapshot must always represent.
type sessionTuple struct {
	broadcast string
	spade     string
	payloadBc string // the payload's embedded broadcast_id
}

func snapTuple(s *Stream) sessionTuple {
	snap := s.SessionSnapshot()
	pbc := ""
	if len(snap.payload) > 0 {
		pbc, _ = snap.payload[0].Properties["broadcast_id"].(string)
	}
	return sessionTuple{broadcast: snap.BroadcastID, spade: snap.SpadeURL, payloadBc: pbc}
}

// applyTuple publishes a full session for generation marker n atomically.
func applyTuple(s *Stream, n int) SessionApplyResult {
	obs := s.BeginSessionObservation()
	b := fmt.Sprintf("b%d", n)
	cand := PlaybackSessionCandidate{BroadcastID: b, Title: "t"}.
		WithSpadeURL(fmt.Sprintf("https://spade.twitch.tv/%d", n)).
		WithPayload("cid", b, "uid", "chan", nil)
	return s.ApplyPlaybackSessionIfCurrent(obs, cand, ExpectedSession{})
}

// TestApplyPlaybackSessionSingleTransition (Group A): the whole tuple flips from
// old to new in ONE step — never an intermediate mix like B2+U1+P1.
func TestApplyPlaybackSessionSingleTransition(t *testing.T) {
	s := NewStream()
	applyTuple(s, 1)
	if got := snapTuple(s); got != (sessionTuple{"b1", "https://spade.twitch.tv/1", "b1"}) {
		t.Fatalf("initial tuple not coherent: %+v", got)
	}

	res := applyTuple(s, 2)
	if !res.Applied {
		t.Fatalf("expected the new tuple to apply, got %+v", res)
	}
	if got := snapTuple(s); got != (sessionTuple{"b2", "https://spade.twitch.tv/2", "b2"}) {
		t.Fatalf("post-apply tuple not the new coherent one: %+v", got)
	}
}

// TestApplyPlaybackSessionNeverVisiblePartial (Group A, concurrent): while a
// reader continuously snapshots, a stream of atomic applies must never expose a
// tuple whose broadcast, spade URL, and payload broadcast_id disagree. Run under
// -race. A non-atomic (split-write) publication would surface a mixed tuple.
func TestApplyPlaybackSessionNeverVisiblePartial(t *testing.T) {
	s := NewStream()
	applyTuple(s, 0)

	var stop atomic.Bool
	var mixed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			tup := snapTuple(s)
			// Every field encodes the same generation marker: broadcast "bN", spade
			// ".../N", payload broadcast_id "bN". A mix means a partial publication
			// was observed.
			if tup.broadcast == "" {
				continue
			}
			n := tup.broadcast[1:]
			if tup.payloadBc != "b"+n || tup.spade != "https://spade.twitch.tv/"+n {
				mixed.Add(1)
			}
		}
	}()

	for i := 1; i <= 2000; i++ {
		applyTuple(s, i)
	}
	stop.Store(true)
	wg.Wait()

	if mixed.Load() != 0 {
		t.Fatalf("observed %d incoherent (partially-published) snapshots", mixed.Load())
	}
}

// TestApplyStaleObservationDoesNotPublish (Group C): once a newer observation
// applies, an older-started observation's apply is rejected as stale — it
// publishes nothing and returns a typed stale result, never a silent success.
func TestApplyStaleObservationDoesNotPublish(t *testing.T) {
	s := NewStream()
	applyTuple(s, 1)

	oldObs := s.BeginSessionObservation()
	newObs := s.BeginSessionObservation()

	newCand := PlaybackSessionCandidate{BroadcastID: "b2"}.
		WithSpadeURL("https://spade.twitch.tv/2").WithPayload("cid", "b2", "uid", "chan", nil)
	if r := s.ApplyPlaybackSessionIfCurrent(newObs, newCand, ExpectedSession{}); !r.Applied {
		t.Fatalf("newer observation must apply, got %+v", r)
	}

	oldCand := PlaybackSessionCandidate{BroadcastID: "b3"}.
		WithSpadeURL("https://spade.twitch.tv/3").WithPayload("cid", "b3", "uid", "chan", nil)
	r := s.ApplyPlaybackSessionIfCurrent(oldObs, oldCand, ExpectedSession{})
	if r.Applied || !r.Stale || r.Reason != SessionStaleSupersededObs {
		t.Fatalf("stale older observation must not publish, got %+v", r)
	}
	if got := snapTuple(s); got != (sessionTuple{"b2", "https://spade.twitch.tv/2", "b2"}) {
		t.Fatalf("newer session must survive the stale apply, got %+v", got)
	}
}

// TestApplyExpectedBroadcastMismatchStale (Group D): a full refresh started for
// broadcast b1 whose broadcast changed to b2 during the I/O (here via a legacy
// Update, standing in for a PubSub new-broadcast) is rejected as stale — the b2
// session is preserved without partial overwrite.
func TestApplyExpectedBroadcastMismatchStale(t *testing.T) {
	s := NewStream()
	applyTuple(s, 1) // b1

	obs := s.BeginSessionObservation()
	// A new broadcast lands during the refresh's I/O.
	s.Update("b2", "t", nil, nil, 0)

	cand := PlaybackSessionCandidate{BroadcastID: "b1"}.
		WithSpadeURL("https://spade.twitch.tv/1b").WithPayload("cid", "b1", "uid", "chan", nil)
	r := s.ApplyPlaybackSessionIfCurrent(obs, cand, ExpectedSession{BroadcastID: "b1"})
	if r.Applied || !r.Stale || r.Reason != SessionStaleBroadcast {
		t.Fatalf("expected a stale broadcast_changed result, got %+v", r)
	}
	if s.GetBroadcastID() != "b2" {
		t.Fatalf("the new broadcast must be preserved, got %q", s.GetBroadcastID())
	}
}

// TestApplyExpectedGenerationDriftStale (Group E, models): a refresh started at
// generation G whose generation drifted (a spade change bumped it) during the I/O
// is rejected as stale by the expected-generation guard.
func TestApplyExpectedGenerationDriftStale(t *testing.T) {
	s := NewStream()
	applyTuple(s, 1)

	obs := s.BeginSessionObservation()
	gen := s.SessionGeneration()
	// A concurrent change bumps the generation during the refresh.
	s.SetSpadeURL("https://spade.twitch.tv/drift")

	cand := PlaybackSessionCandidate{BroadcastID: "b1"}.
		WithPayload("cid", "b1", "uid", "chan", nil)
	r := s.ApplyPlaybackSessionIfCurrent(obs, cand, ExpectedSession{Generation: gen})
	if r.Applied || !r.Stale || r.Reason != SessionStaleGeneration {
		t.Fatalf("expected a stale generation_drift result, got %+v", r)
	}
}

// TestApplyExpectedMatchApplies (Group E, models): when the expected broadcast and
// generation still match, the apply proceeds and bumps the generation once.
func TestApplyExpectedMatchApplies(t *testing.T) {
	s := NewStream()
	applyTuple(s, 1)
	gen := s.SessionGeneration()
	bc := s.GetBroadcastID()

	obs := s.BeginSessionObservation()
	cand := PlaybackSessionCandidate{BroadcastID: bc}.
		WithSpadeURL("https://spade.twitch.tv/refreshed").WithPayload("cid", bc, "uid", "chan", nil)
	r := s.ApplyPlaybackSessionIfCurrent(obs, cand, ExpectedSession{BroadcastID: bc, Generation: gen})
	if !r.Applied || r.Stale {
		t.Fatalf("a matching expected session must apply, got %+v", r)
	}
	if r.Generation != gen+1 {
		t.Fatalf("apply must bump the generation exactly once: got %d want %d", r.Generation, gen+1)
	}
}

// --- Corrective pass 4: atomic observation-pair allocator (models level) ---

// TestBeginPlaybackRefreshObservationAllocatesAtomicOrderedPair proves the single
// model-level allocator reserves both observation domains atomically and keeps
// them ordered together: including availability bumps BOTH counters, excluding it
// bumps ONLY the session counter (availability id stays zero and campaignAvailObs
// is untouched), and across many concurrent callers the pairs never invert —
// sorting by session id yields strictly increasing availability ids.
func TestBeginPlaybackRefreshObservationAllocatesAtomicOrderedPair(t *testing.T) {
	t.Run("include availability bumps both", func(t *testing.T) {
		s := NewStream()
		obs := s.BeginPlaybackRefreshObservation(true)
		if obs.SessionID != 1 || obs.CampaignAvailabilityID != 1 || !obs.HasCampaignAvailability {
			t.Fatalf("first pair must be session=1 avail=1 has=true, got %+v", obs)
		}
		if got := s.SessionObservation(); got != 1 {
			t.Fatalf("session observation must be 1, got %d", got)
		}
		snap, _ := s.CampaignAvailabilitySnapshotAt(time.Now())
		if snap.ObservationID != 1 {
			t.Fatalf("campaign availability observation must be 1, got %d", snap.ObservationID)
		}
	})

	t.Run("exclude availability bumps only session", func(t *testing.T) {
		s := NewStream()
		obs := s.BeginPlaybackRefreshObservation(false)
		if obs.SessionID != 1 {
			t.Fatalf("session id must be 1, got %d", obs.SessionID)
		}
		if obs.CampaignAvailabilityID != 0 || obs.HasCampaignAvailability {
			t.Fatalf("excluded availability must leave id zero/has false, got %+v", obs)
		}
		snap, _ := s.CampaignAvailabilitySnapshotAt(time.Now())
		if snap.ObservationID != 0 {
			t.Fatalf("campaignAvailObs must be untouched (0) when availability excluded, got %d", snap.ObservationID)
		}
	})

	t.Run("concurrent callers get unique, non-inverting pairs", func(t *testing.T) {
		const n = 200
		s := NewStream()
		pairs := make([]PlaybackRefreshObservation, n)
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func(i int) {
				defer wg.Done()
				pairs[i] = s.BeginPlaybackRefreshObservation(true)
			}(i)
		}
		wg.Wait()

		seenSess := make(map[uint64]bool, n)
		seenAvail := make(map[uint64]bool, n)
		for _, p := range pairs {
			if p.SessionID == 0 || p.CampaignAvailabilityID == 0 {
				t.Fatalf("every pair must be non-zero, got %+v", p)
			}
			if seenSess[p.SessionID] {
				t.Fatalf("duplicate session id %d", p.SessionID)
			}
			if seenAvail[p.CampaignAvailabilityID] {
				t.Fatalf("duplicate availability id %d", p.CampaignAvailabilityID)
			}
			seenSess[p.SessionID], seenAvail[p.CampaignAvailabilityID] = true, true
		}

		// Sort by session id; availability ids must be strictly increasing in lockstep
		// — the pair never inverts.
		sort.Slice(pairs, func(i, j int) bool { return pairs[i].SessionID < pairs[j].SessionID })
		for i := 1; i < len(pairs); i++ {
			if pairs[i-1].SessionID >= pairs[i].SessionID {
				t.Fatalf("session ids not strictly increasing at %d: %d !< %d", i, pairs[i-1].SessionID, pairs[i].SessionID)
			}
			if pairs[i-1].CampaignAvailabilityID >= pairs[i].CampaignAvailabilityID {
				t.Fatalf("availability ids inverted at %d: sess %d<%d but avail %d !< %d",
					i, pairs[i-1].SessionID, pairs[i].SessionID,
					pairs[i-1].CampaignAvailabilityID, pairs[i].CampaignAvailabilityID)
			}
		}
	})
}
