package watcher

// Watcher-side integration tests for the operator farming exclusion on the
// provisional observation path (#270): a Skip-ruled reward must never be
// admitted to, retained in, or send from a provisional lease/proof — even
// when discovery (or a stale broker snapshot) still proposes it — and the
// veto must act through the ordinary release paths, never by minting a
// quarantine negative or fabricating observation state.

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func provisionalSkipsFor(candidate models.ProvisionalDropCandidate) *models.RewardSkips {
	return models.NewRewardSkips([]string{models.NormalizeRewardKey(candidate.GameID, candidate.Drop)})
}

// Watcher fail-safe: a provisional contender for a Skip-ruled reward is
// never admitted to a lease, even though the source proposed it — for both
// evidence envelopes.
func TestProvisionalAdmissionRefusesSkippedReward(t *testing.T) {
	for _, evidence := range []models.ProvisionalDropEvidence{
		models.ProvisionalEvidenceDirectory,
		models.ProvisionalEvidenceRestrictedACL,
	} {
		t.Run(string(evidence), func(t *testing.T) {
			w, _ := newTestWatcher(1)
			w.SetProvisionalMonitoringEnabled(true)
			streamer, candidate := provisionalWatcherFixture(t, "skiponly", "channel-1", "game-1")
			if evidence == models.ProvisionalEvidenceRestrictedACL {
				candidate.Evidence = models.ProvisionalEvidenceRestrictedACL
				candidate.DirectoryObs = 0
				candidate.RestrictedACL = []string{"channel-1"}
			}
			w.SetRewardSkips(provisionalSkipsFor(candidate))

			admitted, waiting := w.reconcileProvisionalSlots(
				[]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
			if len(admitted) != 0 {
				t.Fatalf("skipped reward must not hold a provisional slot, got %+v", admitted)
			}
			if len(waiting) != 1 {
				t.Fatalf("refused provisional slot must be reported waiting, got %+v", waiting)
			}
			if _, ok := w.ProvisionalLease(); ok {
				t.Fatal("skipped reward must not mint a provisional lease")
			}
			if w.IsProvisionalQuarantined(streamer, candidate) {
				t.Fatal("the Skip veto must not record a quarantine negative")
			}
		})
	}
}

// A runtime Skip flip while a provisional lease is HELD (and the stale
// source still proposes the tuple) clears the lease on the next reconcile and
// refuses every permit and lease transition — through release paths only.
func TestProvisionalRuntimeSkipFlipClearsHeldLease(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "skiponly", "channel-1", "game-1")

	admitted, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	lease, ok := w.ProvisionalLease()
	if len(admitted) != 1 || !ok || !lease.Candidate.SameLeaseIdentity(candidate) {
		t.Fatalf("baseline lease was not minted: admitted=%+v ok=%v", admitted, ok)
	}

	w.SetRewardSkips(provisionalSkipsFor(candidate))

	// Every lease transition and permit is refused immediately.
	if w.ArmProvisionalLease(lease.LeaseID, 1, time.Now().Add(time.Second), 5) {
		t.Fatal("Arm must refuse a just-skipped reward")
	}
	if w.ObserveProvisionalAbsence(lease.LeaseID, 1, time.Now().Add(time.Second)) {
		t.Fatal("ObserveAbsence must refuse a just-skipped reward")
	}
	if _, ok := w.acquireProvisionalBootstrapPermit(streamer, lease.LeaseID); ok {
		t.Fatal("bootstrap permit must refuse a just-skipped reward")
	}
	if _, ok := w.AcquireObservationPermit(streamer, lease.LeaseID); ok {
		t.Fatal("observation permit must refuse a just-skipped reward")
	}
	// The recovery-permit refusal above already cleared the dead lease (the
	// existing release path); a stale source proposal cannot resurrect it.
	if _, ok := w.ProvisionalLease(); ok {
		t.Fatal("skipped lease must be cleared via the release path")
	}
	admitted, _ = w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	if len(admitted) != 0 {
		t.Fatalf("stale proposal re-admitted a skipped reward: %+v", admitted)
	}
	if w.IsProvisionalQuarantined(streamer, candidate) {
		t.Fatal("the Skip flip must not record a quarantine negative")
	}
	if w.OwnsProvisionalObservation(streamer) || w.OwnsProvisionalCandidate(streamer, candidate) {
		t.Fatal("ownership queries must not report a skipped lease")
	}
}

// Proof retention: a stored server proof for a just-skipped reward stops
// justifying anything — proof queries, proof permits, and the per-tick proven
// candidate set — while the record dies through the existing source-veto
// deletion, never through quarantine.
func TestProvisionalProofStopsJustifyingSkippedReward(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "skiponly", "channel-1", "game-1")

	w.observationMu.Lock()
	w.provisionalProofs = map[string]provisionalProofRecord{
		candidate.QuarantineKey(): {
			proof: ProvisionalProof{ProofID: 7, Candidate: cloneProvisionalCandidateValue(candidate)},
			owner: streamer,
		},
	}
	w.observationMu.Unlock()

	if !w.HasProvisionalProof(streamer, candidate) {
		t.Fatal("baseline: the stored proof must be visible before the flip")
	}

	w.SetRewardSkips(provisionalSkipsFor(candidate))

	if w.HasProvisionalProof(streamer, candidate) {
		t.Fatal("proof query must refuse a skipped reward")
	}
	if _, ok := w.AcquireProvisionalProofPermit(streamer, 7, candidate); ok {
		t.Fatal("proof permit must refuse a skipped reward")
	}
	if got := w.provenProvisionalCandidates([]Candidate{{
		Streamer: streamer, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
	}}); len(got) != 0 {
		t.Fatalf("proven candidate set must exclude a skipped reward, got %+v", got)
	}
	if _, owned := w.ProvisionalOwner(7, candidate); owned {
		t.Fatal("proof ownership must refuse a skipped reward")
	}
}

// An independently justified ordinary slot on the
// same channel survives while the provisional contender for the skipped
// reward is refused — the veto suppresses exactly the skipped justification.
func TestProvisionalSkipKeepsIndependentOrdinarySlot(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "skiponly", "channel-1", "game-1")
	pointsChannel := w.streamers[0]

	w.SetRewardSkips(provisionalSkipsFor(candidate))

	ordinary := slotOccupant{
		streamer:   pointsChannel,
		origin:     OriginConfigured,
		idx:        0,
		reasonCode: ReasonFairRotation,
		reason:     "points",
		selectedAt: time.Now(),
	}
	admitted, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{ordinary, provisionalSlot(streamer, candidate)}, nil, time.Now())
	if len(admitted) != 1 || admitted[0].streamer != pointsChannel || admitted[0].provisionalDrop != nil {
		t.Fatalf("independent ordinary slot must survive the veto, got %+v", admitted)
	}
	if _, ok := w.ProvisionalLease(); ok {
		t.Fatal("no lease may exist for the skipped contender")
	}
}

// Concurrent rule replacement races neither provisional reconciliation nor
// the permit/proven paths (-race): every grant site takes one coherent
// decision under its own lock, and with the rule published the skipped
// contender converges to refusal.
func TestSetRewardSkipsConcurrentWithProvisionalReconcile(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "skiponly", "channel-1", "game-1")
	skips := provisionalSkipsFor(candidate)
	w.SetRewardSkips(skips)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			w.SetRewardSkips(skips)
		}
	}()
	for i := 0; i < 20; i++ {
		admitted, _ := w.reconcileProvisionalSlots(
			[]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
		if len(admitted) != 0 {
			t.Fatalf("skipped contender admitted under concurrent updates: %+v", admitted)
		}
		w.provenProvisionalCandidates([]Candidate{{
			Streamer: streamer, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
		}})
		if _, ok := w.AcquireObservationPermit(streamer, 0); !ok {
			t.Fatal("ordinary lease-free permit must stay grantable during the race")
		}
	}
	<-done
	if _, ok := w.ProvisionalLease(); ok {
		t.Fatal("no lease may survive the concurrent-flip run")
	}
}

// Identity exactness plus control: a rule for the same reward
// name under another game leaves admission untouched.
func TestProvisionalAdmissionExactIdentity(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "skiponly", "channel-1", "game-1")
	w.SetRewardSkips(models.NewRewardSkips([]string{
		models.NormalizeRewardKey("other-game", candidate.Drop),
	}))

	admitted, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	if len(admitted) != 1 || admitted[0].provisionalDrop == nil {
		t.Fatalf("foreign-game rule must not refuse admission, got %+v", admitted)
	}
	if _, ok := w.ProvisionalLease(); !ok {
		t.Fatal("control: the unskipped candidate must mint its lease")
	}
}
