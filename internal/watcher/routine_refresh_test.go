package watcher

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func TestRoutineRefreshLinearizesWithProvisionalLeaseAdmission(t *testing.T) {
	t.Run("refresh wins before monitoring enable", func(t *testing.T) {
		w, _ := newTestWatcher(1)
		owner := w.streamers[0]
		candidate := configuredProvisionalFixture(t, owner)

		entered := make(chan struct{})
		release := make(chan struct{})
		done := make(chan bool, 1)
		go func() {
			done <- w.RunRoutineRefresh(owner, func() {
				close(entered)
				<-release
			})
		}()
		<-entered

		// The in-flight registration exists even though monitoring was disabled
		// when the refresh started. Enabling it cannot admit the exact owner until
		// the callback drains.
		w.SetProvisionalMonitoringEnabled(true)
		blocked, _ := w.reconcileProvisionalSlots(
			[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
		)
		if len(blocked) != 0 {
			t.Fatalf("in-flight routine refresh admitted exact-owner slot: %+v", blocked)
		}
		if _, ok := w.ProvisionalLease(); ok {
			t.Fatal("in-flight routine refresh admitted an exact-owner lease")
		}

		close(release)
		if executed := <-done; !executed {
			t.Fatal("registered routine refresh did not execute")
		}
		admitted, _ := w.reconcileProvisionalSlots(
			[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
		)
		if len(admitted) != 1 {
			t.Fatalf("lease did not resume after routine refresh drained: %+v", admitted)
		}
		if lease, ok := w.ProvisionalLease(); !ok || !lease.Candidate.SameLeaseIdentity(candidate) {
			t.Fatalf("post-refresh lease = %+v, ok=%v", lease, ok)
		}
	})

	t.Run("lease wins before refresh", func(t *testing.T) {
		w, _ := newTestWatcher(1)
		w.SetProvisionalMonitoringEnabled(true)
		owner := w.streamers[0]
		candidate := configuredProvisionalFixture(t, owner)
		admitted, _ := w.reconcileProvisionalSlots(
			[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
		)
		if len(admitted) != 1 {
			t.Fatal("setup: provisional lease not admitted")
		}
		lease, ok := w.ProvisionalLease()
		if !ok {
			t.Fatal("setup: provisional lease missing")
		}

		called := false
		if w.RunRoutineRefresh(owner, func() { called = true }) || called {
			t.Fatal("exact-owner routine refresh ran while its lease was current")
		}
		if current, currentOK := w.ProvisionalLease(); !currentOK || current.LeaseID != lease.LeaseID {
			t.Fatalf("denied routine refresh changed lease: %+v, ok=%v", current, currentOK)
		}

		if !w.ReleaseProvisionalLease(lease.LeaseID) {
			t.Fatal("setup: lease release failed")
		}
		if !w.RunRoutineRefresh(owner, func() { called = true }) || !called {
			t.Fatal("routine refresh did not resume immediately after release")
		}
	})
}

func TestRoutineRefreshPermitUsesExactOwnerPointer(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.SetProvisionalMonitoringEnabled(true)
	owner := w.streamers[0]
	candidate := configuredProvisionalFixture(t, owner)
	if slots, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
	); len(slots) != 1 {
		t.Fatal("setup: exact-owner lease not admitted")
	}

	clone := models.NewStreamer(owner.GetUsername(), owner.GetSettings())
	clone.ChannelID = owner.ChannelID
	called := false
	if !w.RunRoutineRefresh(clone, func() { called = true }) || !called {
		t.Fatal("scalar-identical replacement object was incorrectly treated as the private lease owner")
	}
}

func TestRoutineRefreshRegistrationReleasesAfterPanic(t *testing.T) {
	w, _ := newTestWatcher(1)
	owner := w.streamers[0]
	candidate := configuredProvisionalFixture(t, owner)

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("setup: routine callback did not panic")
			}
		}()
		w.RunRoutineRefresh(owner, func() { panic("synthetic routine refresh panic") })
	}()

	w.SetProvisionalMonitoringEnabled(true)
	if slots, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
	); len(slots) != 1 {
		t.Fatal("panic leaked a routine-refresh registration and blocked lease admission")
	}
}

func TestRoutineRefreshRegistrationReleasesOnlyAfterCancelledCallbackReturns(t *testing.T) {
	w, _ := newTestWatcher(1)
	owner := w.streamers[0]
	candidate := configuredProvisionalFixture(t, owner)
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	cancelled := make(chan struct{})
	returnCallback := make(chan struct{})
	done := make(chan bool, 1)

	go func() {
		done <- w.RunRoutineRefresh(owner, func() {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			<-returnCallback
		})
	}()
	<-entered
	cancel()
	<-cancelled

	// Cancellation alone cannot release the registration: CheckStreamerOnline
	// is context-unaware in production, so a callback may still publish a late
	// mutation until it has actually returned.
	w.SetProvisionalMonitoringEnabled(true)
	if slots, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
	); len(slots) != 0 {
		t.Fatal("callback cancellation released routine ownership before the callback returned")
	}

	close(returnCallback)
	if ran := <-done; !ran {
		t.Fatal("cancelled callback was not reported as executed after returning")
	}
	if slots, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
	); len(slots) != 1 {
		t.Fatal("cancelled callback leaked routine ownership after returning")
	}
}

func promotedRoutineRefreshProofFixture(
	t *testing.T,
) (*MinuteWatcher, *models.Streamer, models.ProvisionalDropCandidate, ProvisionalLease, ProvisionalProof) {
	t.Helper()
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	owner, candidate := provisionalWatcherFixture(t, "proof-routine", "proof-routine-id", "game-1")
	if slots, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
	); len(slots) != 1 {
		t.Fatal("setup: provisional proof lease not admitted")
	}
	lease, ok := w.ProvisionalLease()
	if !ok {
		t.Fatal("setup: provisional proof lease missing")
	}
	baselineAt := lease.ReservedAt.Add(time.Second)
	if !w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 0) ||
		!w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 1) {
		t.Fatal("setup: exact server delta did not promote provisional proof")
	}
	proofs := w.ProvisionalProofs()
	if len(proofs) != 1 {
		t.Fatalf("setup: promoted proofs=%d, want 1", len(proofs))
	}
	return w, owner, candidate, lease, proofs[0]
}

func TestRoutineRefreshLinearizesWithPromotedProofPermit(t *testing.T) {
	t.Run("routine refresh wins", func(t *testing.T) {
		w, owner, candidate, lease, proof := promotedRoutineRefreshProofFixture(t)
		// Model the normal standalone-proof state after the Proven lease handoff.
		if !w.ReleaseProvisionalLease(lease.LeaseID) {
			t.Fatal("setup: proven lease release failed")
		}

		entered := make(chan struct{})
		release := make(chan struct{})
		done := make(chan bool, 1)
		go func() {
			done <- w.RunRoutineRefresh(owner, func() {
				close(entered)
				<-release
			})
		}()
		<-entered

		if permit, ok := w.AcquireProvisionalProofPermit(owner, proof.ProofID, candidate); ok {
			w.ReleaseObservationPermit(permit)
			t.Fatal("promoted proof permit crossed an already-active exact-owner routine refresh")
		}

		close(release)
		if ran := <-done; !ran {
			t.Fatal("setup: registered routine refresh did not execute")
		}
		permit, ok := w.AcquireProvisionalProofPermit(owner, proof.ProofID, candidate)
		if !ok {
			t.Fatal("promoted proof permit did not resume after routine refresh drained")
		}
		w.ReleaseObservationPermit(permit)
	})

	t.Run("promoted proof permit wins", func(t *testing.T) {
		w, owner, candidate, _, proof := promotedRoutineRefreshProofFixture(t)
		permit, ok := w.AcquireProvisionalProofPermit(owner, proof.ProofID, candidate)
		if !ok {
			t.Fatal("setup: promoted proof permit was denied")
		}
		if _, leaseStillPresent := w.ProvisionalLease(); leaseStillPresent {
			w.ReleaseObservationPermit(permit)
			t.Fatal("setup: promoted proof permit did not consume its Proven lease handoff")
		}

		called := false
		if w.RunRoutineRefresh(owner, func() { called = true }) || called {
			w.ReleaseObservationPermit(permit)
			t.Fatal("routine refresh crossed an already-active exact-owner promoted proof permit")
		}

		w.ReleaseObservationPermit(permit)
		if !w.RunRoutineRefresh(owner, func() { called = true }) || !called {
			t.Fatal("routine refresh did not resume after promoted proof permit release")
		}
	})
}

func TestRoutineRefreshDoesNotHoldObservationMuAcrossCallback(t *testing.T) {
	w, _ := newTestWatcher(1)
	owner := w.streamers[0]
	lockWasHeld := false

	if !w.RunRoutineRefresh(owner, func() {
		if !w.observationMu.TryLock() {
			lockWasHeld = true
			return
		}
		w.observationMu.Unlock()
	}) {
		t.Fatal("unowned routine refresh was unexpectedly denied")
	}
	if lockWasHeld {
		t.Fatal("RunRoutineRefresh held observationMu across its callback")
	}
}

func TestWatcherRoutineStaleRefreshDefersForOwnedLeaseAndResumes(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 8)}
	checker := &staticChecker{checked: make(chan string, 8)}
	w, streamers := newLoopWatcher(1, sender, checker)
	w.routineRefreshAfter = -1 // deterministic: every non-negative elapsed value is stale.
	w.ctx = context.Background()
	owner := streamers[0]
	candidate := configuredProvisionalFixture(t, owner)
	w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{{
		Streamer: owner, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
	}}})
	w.SetProvisionalMonitoringEnabled(true)
	if slots, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
	); len(slots) != 1 {
		t.Fatal("setup: provisional lease not admitted")
	}
	lease, _ := w.ProvisionalLease()
	generation := owner.Stream.SessionGeneration()

	w.processWatching(tickCtx(w))
	select {
	case checked := <-checker.checked:
		t.Fatalf("watcher routine stale path refreshed active owner %q", checked)
	default:
	}
	if got := owner.Stream.SessionGeneration(); got != generation {
		t.Fatalf("owned routine path changed session generation: got %d, want %d", got, generation)
	}
	if current, ok := w.ProvisionalLease(); !ok || current.LeaseID != lease.LeaseID {
		t.Fatalf("owned routine path churned lease: %+v, ok=%v", current, ok)
	}

	if !w.ReleaseProvisionalLease(lease.LeaseID) {
		t.Fatal("setup: lease release failed")
	}
	w.processWatching(tickCtx(w))
	select {
	case checked := <-checker.checked:
		if checked != owner.GetUsername() {
			t.Fatalf("resumed routine refresh checked %q, want %q", checked, owner.GetUsername())
		}
	default:
		t.Fatal("watcher routine stale refresh did not resume after lease release")
	}
}

func TestProvisionalSendFailureRecoveryBypassesRoutineGuard(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 8), err: errors.New("synthetic send failure")}
	checker := &staticChecker{checked: make(chan string, 8)}
	w, streamers := newLoopWatcher(1, sender, checker)
	w.ctx = context.Background()
	owner := streamers[0]
	candidate := configuredProvisionalFixture(t, owner)
	w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{{
		Streamer: owner, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
	}}})
	w.SetProvisionalMonitoringEnabled(true)
	if slots, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
	); len(slots) != 1 {
		t.Fatal("setup: provisional lease not admitted")
	}
	lease, _ := w.ProvisionalLease()
	baselineAt := lease.ReservedAt.Add(time.Second)
	if !w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 0) {
		t.Fatal("setup: provisional baseline not armed")
	}

	w.processWatching(tickCtx(w))
	select {
	case checked := <-checker.checked:
		if checked != owner.GetUsername() {
			t.Fatalf("send-failure recovery checked %q, want %q", checked, owner.GetUsername())
		}
	default:
		t.Fatal("active provisional lease suppressed send-failure recovery refresh")
	}
}
