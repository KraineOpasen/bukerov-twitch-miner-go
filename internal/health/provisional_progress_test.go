package health

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

func newProvisionalWatchdogHarness(t *testing.T) *watchdogHarness {
	t.Helper()
	h := newWatchdogHarness(t)
	met := true
	h.campaign.Drops[0].HasPreconditionsMet = &met
	h.campaign.ACL = models.CampaignACL{
		State:      models.ACLUnrestricted,
		Complete:   true,
		ObservedAt: h.now,
		Source:     models.ACLSourceCampaignDetails,
	}
	h.campaign.Channels = nil
	h.streamer.Stream.SetCampaigns(nil)
	h.streamer.SetConfirmedOnline()
	h.streamer.Stream.MarkCampaignAvailabilityUnknown()
	snapshot := h.streamer.Stream.ProvisionalDropSnapshot()
	candidate := models.ProvisionalDropCandidate{
		CampaignID:           h.campaign.ID,
		Campaign:             h.campaign.Name,
		DropID:               h.campaign.Drops[0].ID,
		Drop:                 h.campaign.Drops[0].Name,
		GameID:               snapshot.GameID,
		Login:                h.streamer.GetUsername(),
		ChannelID:            h.streamer.ChannelID,
		BroadcastID:          snapshot.BroadcastID,
		SessionGeneration:    snapshot.SessionGeneration,
		AvailabilityObs:      snapshot.Availability.ObservationID,
		AvailabilityKnownGen: snapshot.Availability.KnownGeneration,
		DirectoryObs:         1,
		Evidence:             models.ProvisionalEvidenceDirectory,
	}
	h.watch.mu.Lock()
	h.watch.provisionalOwner = h.streamer
	h.watch.lease = &watcher.ProvisionalLease{
		LeaseID:    1,
		Candidate:  candidate,
		State:      watcher.ProvisionalLeasePending,
		ReservedAt: h.now.Add(-time.Second),
	}
	h.watch.mu.Unlock()
	h.drops.observe(h.now, "")
	return h
}

func (h *watchdogHarness) provisionalObservation(advance time.Duration, minutes int, errText string, reports int) {
	h.now = h.now.Add(advance)
	h.campaign.Drops[0].CurrentMinutesWatched = minutes
	h.drops.observe(h.now, errText)
	if reports > 0 {
		h.watch.addSuccesses("chan", reports)
	}
	h.w.evaluate(h.now)
}

func (h *watchdogHarness) armProvisional(t *testing.T) watcher.ProvisionalLease {
	t.Helper()
	h.w.evaluate(h.now) // capture current run and request one exact progress sync
	h.provisionalObservation(time.Minute, 100, "", 0)
	lease, ok := h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeaseObserving {
		t.Fatalf("expected observing lease after post-reservation baseline, got %+v, ok=%v", lease, ok)
	}
	return lease
}

func (h *watchdogHarness) promoteProvisionalProof(t *testing.T) watcher.ProvisionalProof {
	t.Helper()
	h.armProvisional(t)
	h.provisionalObservation(time.Minute, 101, "", 0)
	lease, ok := h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeaseProven {
		t.Fatalf("expected proven lease before promotion, got %+v, ok=%v", lease, ok)
	}
	proofs := h.watch.ProvisionalProofs()
	if len(proofs) != 1 {
		t.Fatalf("expected one exact promoted proof, got %+v", proofs)
	}
	h.watch.ReleaseProvisionalLease(lease.LeaseID) // broker consumes proven lease
	return proofs[0]
}

func TestProvisionalRequiresFreshExactBaselineThenServerDelta(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)

	h.w.evaluate(h.now)
	lease, ok := h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeasePending {
		t.Fatalf("beacons must remain suppressed before a fresh clean observation, got %+v, ok=%v", lease, ok)
	}
	if _, triggered := h.drops.counts(); triggered != 1 {
		t.Fatalf("pending lease must request exactly one progress sync, got %d", triggered)
	}
	if got := h.prober.callCount(); got != 0 {
		t.Fatalf("baseline acquisition must not run a transport probe, got %d", got)
	}

	h.provisionalObservation(time.Minute, 100, "", 0)
	lease, ok = h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeaseObserving || lease.BaselineMinutes != 100 {
		t.Fatalf("fresh exact baseline must arm, not prove, the lease: %+v, ok=%v", lease, ok)
	}

	h.provisionalObservation(time.Minute, 101, "", 0)
	lease, ok = h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeaseProven || lease.MaxMinutes != 101 {
		t.Fatalf("fresh exact monotone server delta must prove the lease: %+v, ok=%v", lease, ok)
	}
	if campaigns := h.streamer.Stream.GetCampaigns(); len(campaigns) != 0 {
		t.Fatalf("provisional proof must not mutate confirmed assignments: %+v", campaigns)
	}
	state, ids := h.streamer.Stream.CampaignAvailability()
	if state != models.CampaignAvailabilityUnknown || len(ids) != 0 {
		t.Fatalf("provisional proof must not fabricate AvailableDrops authority: state=%v ids=%v", state, ids)
	}
}

func TestProvisionalRejectsCurrentCampaignEnvelopeDrift(t *testing.T) {
	makeRestrictedCandidate := func(h *watchdogHarness) {
		h.campaign.ACL = models.CampaignACL{
			State:      models.ACLRestricted,
			ChannelIDs: []string{h.streamer.ChannelID},
			Complete:   true,
			ObservedAt: h.now,
			Source:     models.ACLSourceCampaignDetails,
		}
		h.campaign.Channels = []string{h.streamer.ChannelID}
		h.watch.mu.Lock()
		h.watch.lease.Candidate.Evidence = models.ProvisionalEvidenceRestrictedACL
		h.watch.lease.Candidate.DirectoryObs = 0
		h.watch.lease.Candidate.RestrictedACL = []string{h.streamer.ChannelID}
		h.watch.mu.Unlock()
	}

	tests := map[string]struct {
		prepare func(*watchdogHarness)
		drift   func(*watchdogHarness)
	}{
		"restricted ACL allowed to excluded": {
			prepare: makeRestrictedCandidate,
			drift: func(h *watchdogHarness) {
				h.campaign.ACL.ChannelIDs = []string{"different-channel"}
				h.campaign.Channels = []string{"different-channel"}
			},
		},
		"directory open to restricted": {
			drift: func(h *watchdogHarness) {
				h.campaign.ACL.State = models.ACLRestricted
				h.campaign.ACL.ChannelIDs = []string{h.streamer.ChannelID}
				h.campaign.Channels = []string{h.streamer.ChannelID}
			},
		},
		"restricted ACL becomes incomplete": {
			prepare: makeRestrictedCandidate,
			drift: func(h *watchdogHarness) {
				h.campaign.ACL.Complete = false
			},
		},
		"campaign game changes": {
			drift: func(h *watchdogHarness) {
				h.campaign.Game = &models.Game{ID: "different-game", Name: "Different Game"}
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			h := newProvisionalWatchdogHarness(t)
			if tc.prepare != nil {
				tc.prepare(h)
			}
			h.armProvisional(t)
			tc.drift(h)

			h.provisionalObservation(time.Minute, 101, "", 0)
			if lease, ok := h.watch.ProvisionalLease(); ok {
				t.Fatalf("stale campaign envelope retained lease: %+v", lease)
			}
			h.watch.mu.Lock()
			released := append([]uint64(nil), h.watch.released...)
			quarantined := append([]models.ProvisionalDropCandidate(nil), h.watch.quarantined...)
			h.watch.mu.Unlock()
			if len(released) != 1 || len(quarantined) != 0 {
				t.Fatalf("campaign envelope drift must release without negative: released=%v quarantined=%+v", released, quarantined)
			}
			if proofs := h.watch.ProvisionalProofs(); len(proofs) != 0 {
				t.Fatalf("stale campaign envelope promoted on exact delta: %+v", proofs)
			}
		})
	}
}

func TestProvisionalCompleteAbsenceAllowsObservationButNotBaselineOrProof(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	h.w.evaluate(h.now) // capture pre-reservation run

	h.now = h.now.Add(time.Minute)
	h.drops.observeExact(h.now, "", nil) // explicit complete array, target absent
	h.w.evaluate(h.now)
	lease, ok := h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeasePending || lease.MaxRun == 0 || lease.BaselineRun != 0 {
		t.Fatalf("authoritative absence must authorize observation without inventing a zero baseline: %+v, ok=%v", lease, ok)
	}
	h.watch.mu.Lock()
	absenceCalls := h.watch.absenceObservations
	h.watch.mu.Unlock()
	if absenceCalls != 1 {
		t.Fatalf("fresh absence must authorize exactly one observation, calls=%d", absenceCalls)
	}
	h.w.evaluate(h.now)
	h.watch.mu.Lock()
	absenceCalls = h.watch.absenceObservations
	h.watch.mu.Unlock()
	if absenceCalls != 1 {
		t.Fatalf("same absence run reopened an observation, calls=%d", absenceCalls)
	}

	h.watch.addSuccesses("chan", stallMinReports)
	h.now = h.now.Add(10 * time.Minute)
	h.drops.observeExact(h.now, "", nil)
	h.w.evaluate(h.now)
	lease, ok = h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeasePending || lease.BaselineRun != 0 {
		t.Fatalf("repeated absence must remain pending and baseline-free: %+v, ok=%v", lease, ok)
	}

	key := h.campaign.ID + "\x00" + h.campaign.Drops[0].ID
	h.now = h.now.Add(time.Minute)
	h.drops.observeExact(h.now, "", map[string]int{key: 100})
	h.w.evaluate(h.now)
	lease, ok = h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeaseObserving || lease.BaselineMinutes != 100 {
		t.Fatalf("first later exact Found row must become the numeric baseline only: %+v, ok=%v", lease, ok)
	}

	h.now = h.now.Add(time.Minute)
	h.campaign.Drops[0].CurrentMinutesWatched = 101
	h.drops.observeExact(h.now, "", map[string]int{key: 101})
	h.w.evaluate(h.now)
	lease, ok = h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeaseProven || lease.MaxMinutes != 101 {
		t.Fatalf("only a post-baseline exact Found delta may prove: %+v, ok=%v", lease, ok)
	}
}

func TestProvisionalRepeatedCompleteAbsenceUsesBoundedNarrowRecovery(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	h.w.evaluate(h.now)
	for i := 0; i < 50; i++ {
		h.now = h.now.Add(10 * time.Minute)
		h.drops.observeExact(h.now, "", nil)
		h.watch.addSuccesses("chan", 3)
		h.w.evaluate(h.now)
		h.watch.mu.Lock()
		done := len(h.watch.quarantined) != 0
		h.watch.mu.Unlock()
		if done {
			break
		}
	}
	h.watch.mu.Lock()
	quarantined := append([]models.ProvisionalDropCandidate(nil), h.watch.quarantined...)
	h.watch.mu.Unlock()
	if len(quarantined) != 1 {
		t.Fatalf("baseline-free authoritative absence did not reach bounded exact quarantine: %+v", quarantined)
	}
	if len(h.notifier.byKind("stalled")) != 0 || len(h.w.AvoidEntries()) != 0 {
		t.Fatal("baseline-free provisional falsification leaked ordinary alert/avoid semantics")
	}
}

func TestProvisionalStateRebaselinesWhenTupleBecomesOrdinary(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	h.armProvisional(t)
	h.provisionalObservation(time.Minute, 101, "", 0)
	lease, ok := h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeaseProven {
		t.Fatalf("expected proved provisional lease, got %+v, ok=%v", lease, ok)
	}

	// Discovery/broker consumes the proof and the normal assignment owner later
	// publishes the same campaign. The watchdog map key is unchanged, but its
	// authority and recovery semantics must be rebuilt as ordinary.
	h.watch.ReleaseProvisionalLease(lease.LeaseID)
	h.streamer.Stream.SetCampaigns([]*models.Campaign{h.campaign})
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)

	key := h.campaign.ID + "\x00" + h.campaign.Drops[0].ID
	h.w.mu.Lock()
	st := h.w.states[key]
	h.w.mu.Unlock()
	if st == nil || st.provisional || st.provisionalProof || st.provisionalLeaseID != 0 || st.provisionalProofID != 0 {
		t.Fatalf("ordinary assignment reused provisional state: %+v", st)
	}
	if st.LastMinutes != 101 || !st.LastProgressAt.Equal(h.now) {
		t.Fatalf("ordinary state was not freshly rebaselined: %+v", st.DropProgress)
	}
}

func TestPromotedProvisionalProofContinuesExactProgressMonitoring(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	proof := h.promoteProvisionalProof(t)

	h.provisionalObservation(time.Minute, 102, "", 0)
	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	h.w.mu.Lock()
	st := h.w.states[key]
	h.w.mu.Unlock()
	if st == nil || !st.provisionalProof || st.provisionalProofID != proof.ProofID ||
		st.provisionalOwner != h.streamer || st.LastMinutes != 102 {
		t.Fatalf("promoted proof was not monitored from its exact server watermark: %+v", st)
	}
	if len(h.watch.ProvisionalProofs()) != 1 {
		t.Fatal("ordinary progress monitoring must not consume the broker's live proof")
	}
	h.provisionalObservation(time.Minute, 102, "", 1)
	h.w.mu.Lock()
	st = h.w.states[key]
	h.w.mu.Unlock()
	if st == nil || st.evidenceSince.IsZero() {
		t.Fatalf("post-progress proof lost broker owner and could not re-enter exact monitoring: %+v", st)
	}
}

func TestPromotedProofRebaselinesDeliveryEvidence(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	h.armProvisional(t)
	h.watch.addSuccesses("chan", stallMinReports+20)
	h.provisionalObservation(time.Minute, 101, "", 0)
	lease, ok := h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeaseProven {
		t.Fatalf("expected proven lease, got %+v, ok=%v", lease, ok)
	}
	proofs := h.watch.ProvisionalProofs()
	if len(proofs) != 1 {
		t.Fatalf("expected promoted proof, got %+v", proofs)
	}
	h.watch.ReleaseProvisionalLease(lease.LeaseID)

	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	key := proofs[0].Candidate.CampaignID + "\x00" + proofs[0].Candidate.DropID
	h.w.mu.Lock()
	st := h.w.states[key]
	h.w.mu.Unlock()
	if st == nil || st.ReportsSinceProgress != 0 || st.NoProgressObs != 0 {
		t.Fatalf("observing-lease ACK/no-progress evidence leaked across proof handoff: %+v", st)
	}
}

func TestPromotedProofObservationErrorIsNotNegative(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	proof := h.promoteProvisionalProof(t)

	h.provisionalObservation(30*time.Minute, 101, "inventory timeout", stallMinReports+5)
	if proofs := h.watch.ProvisionalProofs(); len(proofs) != 1 || proofs[0].ProofID != proof.ProofID {
		t.Fatalf("UNKNOWN progress acquisition must not remove/quarantine proof: %+v", proofs)
	}
	h.watch.mu.Lock()
	quarantined := len(h.watch.quarantined)
	h.watch.mu.Unlock()
	if quarantined != 0 {
		t.Fatalf("UNKNOWN progress acquisition must not become negative evidence, quarantined=%d", quarantined)
	}
}

func TestPromotedProofAdoptsRoutineUnknownEnvelopeWithoutReset(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	proof := h.promoteProvisionalProof(t)
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)

	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	h.w.mu.Lock()
	before := h.w.states[key]
	before.RecoveryStage = 2
	before.NoProgressObs = 1
	h.w.mu.Unlock()

	// A repeated UNKNOWN lookup and a fresh Directory row are a new source
	// envelope, but they do not undo the exact Inventory delta already proved.
	h.streamer.Stream.MarkCampaignAvailabilityUnknown()
	snapshot := h.streamer.Stream.ProvisionalDropSnapshot()
	updated := proof
	updated.Candidate.AvailabilityObs = snapshot.Availability.ObservationID
	updated.Candidate.AvailabilityKnownGen = snapshot.Availability.KnownGeneration
	updated.Candidate.DirectoryObs++
	h.watch.mu.Lock()
	h.watch.proofs = []watcher.ProvisionalProof{updated}
	h.watch.mu.Unlock()

	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	after := h.w.states[key]
	h.w.mu.Unlock()
	if after != before || after.RecoveryStage != 2 || after.provisionalCandidate.DirectoryObs != updated.Candidate.DirectoryObs ||
		after.provisionalCandidate.AvailabilityObs != updated.Candidate.AvailabilityObs {
		t.Fatalf("routine proof-envelope refresh reset state or retained a stale candidate: before=%p after=%+v", before, after)
	}
}

func TestPromotedProofKnownEpochStartsFreshState(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	proof := h.promoteProvisionalProof(t)
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	h.w.mu.Lock()
	before := h.w.states[key]
	before.RecoveryStage = 4
	h.w.mu.Unlock()

	// Even a transient authoritative Known-empty publication advances the
	// non-zero Known epoch. Returning to UNKNOWN cannot revive the old proof
	// recovery episode under a cosmetically similar candidate.
	obsID := h.streamer.Stream.BeginCampaignAvailabilityObservation()
	h.streamer.Stream.ApplyCampaignAvailability(obsID, true, nil, h.now)
	h.streamer.Stream.MarkCampaignAvailabilityUnknown()
	snapshot := h.streamer.Stream.ProvisionalDropSnapshot()
	updated := proof
	updated.ProofID++
	updated.Candidate.AvailabilityObs = snapshot.Availability.ObservationID
	updated.Candidate.AvailabilityKnownGen = snapshot.Availability.KnownGeneration
	h.watch.mu.Lock()
	h.watch.proofs = []watcher.ProvisionalProof{updated}
	h.watch.mu.Unlock()

	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	after := h.w.states[key]
	h.w.mu.Unlock()
	if after == nil || after == before || after.RecoveryStage != 0 ||
		after.provisionalCandidate.AvailabilityKnownGen != snapshot.Availability.KnownGeneration {
		t.Fatalf("Known-empty authority epoch did not break the prior proof state: before=%p after=%+v", before, after)
	}
}

func TestPromotedProofNoProgressUsesOrdinaryRecovery(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	h.promoteProvisionalProof(t)

	for i := 0; i < 30; i++ {
		h.provisionalObservation(10*time.Minute, 101, "", 3)
		if len(h.notifier.byKind("stalled")) != 0 {
			break
		}
	}
	if proofs := h.watch.ProvisionalProofs(); len(proofs) != 1 {
		t.Fatalf("ordinary recovery must not retroactively revoke proven authority: %+v", proofs)
	}
	h.watch.mu.Lock()
	quarantined := append([]models.ProvisionalDropCandidate(nil), h.watch.quarantined...)
	h.watch.mu.Unlock()
	if len(quarantined) != 0 {
		t.Fatalf("proved candidate must never be quarantined as unproved: %+v", quarantined)
	}
	if len(h.notifier.byKind("stalled")) != 1 || len(h.w.AvoidEntries()) != 1 {
		t.Fatal("promoted proof must hand off to ordinary channel-switch/notification recovery")
	}
	if signal := h.w.ProgressSignal(h.now); signal.Status != StatusStalled {
		t.Fatalf("ordinary proof recovery exhaustion must publish STALLED health: %+v", signal)
	}
}

func TestPromotedProofSourcePrunePreservesBoundedOrdinaryEpisode(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	proof := h.promoteProvisionalProof(t)
	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID

	var before *dropState
	for i := 0; i < 50; i++ {
		h.provisionalObservation(10*time.Minute, 101, "", 3)
		h.w.mu.Lock()
		before = h.w.states[key]
		atSwitch := before != nil && before.provisionalProof && before.RecoveryStage == len(recoveryStages)-1
		h.w.mu.Unlock()
		if atSwitch {
			break
		}
	}
	if before == nil || before.RecoveryStage != len(recoveryStages)-1 || len(h.w.AvoidEntries()) != 1 {
		t.Fatalf("proof recovery did not reach channel-switch handoff: state=%+v avoid=%+v", before, h.w.AvoidEntries())
	}
	if len(h.notifier.byKind("stalled")) != 0 {
		t.Fatal("setup crossed terminal notification before source-prune handoff")
	}

	// Production discovery omits the avoided source and the broker prunes its
	// proof. The account Drop remains current, so this is an authority handoff,
	// not the end of the recovery episode.
	h.watch.mu.Lock()
	h.watch.proofs = nil
	h.watch.slots = nil
	h.watch.watching = map[string]bool{"chan": false}
	h.watch.mu.Unlock()
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	after := h.w.states[key]
	h.w.mu.Unlock()
	if after != before || after.provisionalProof || after.provisionalOwner != nil ||
		after.RecoveryStage != len(recoveryStages)-1 || len(h.w.AvoidEntries()) != 1 {
		t.Fatalf("proof prune reset/leaked the ordinary recovery handoff: before=%p after=%+v avoid=%+v", before, after, h.w.AvoidEntries())
	}
	if len(h.notifier.byKind("stalled")) != 0 || len(h.notifier.byKind("recovered")) != 0 {
		t.Fatalf("proof prune emitted a spurious transition: %+v", h.notifier.calls)
	}

	// With no confirmed farmer the gate pauses and cannot duplicate/advance the
	// terminal stage. When account truth genuinely removes the Drop, generic
	// cleanup owns both the retained state and its avoid side effect.
	h.now = h.now.Add(30 * time.Minute)
	h.w.evaluate(h.now)
	if after.RecoveryStage != len(recoveryStages)-1 || len(h.notifier.byKind("stalled")) != 0 {
		t.Fatalf("unowned ordinary episode advanced without a confirmed farmer: %+v", after)
	}
	h.drops.mu.Lock()
	h.drops.campaigns = nil
	h.drops.brokerCampaigns = nil
	h.drops.brokerViewOverride = true
	h.drops.mu.Unlock()
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	if len(h.w.AvoidEntries()) != 0 || len(h.notifier.byKind("recovered")) != 0 {
		t.Fatalf("true Drop removal leaked avoid or fabricated recovery: avoid=%+v notes=%+v", h.w.AvoidEntries(), h.notifier.calls)
	}
}

func TestPromotedProofBrokerSourceAbsenceSurvivesStaleProofSnapshots(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	proof := h.promoteProvisionalProof(t)
	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	before := h.w.states[key]
	before.RecoveryStage = 4
	before.RecoveryStageName = recoveryStages[3].name
	before.evidenceSince = h.now.Add(-time.Hour)
	before.NoProgressObs = 3
	before.ReportsSinceProgress = stallMinReports
	before.avoidedChannel = proof.Candidate.Login
	before.notifiedStalled = true
	before.exhaustedAt = h.now.Add(-time.Minute)
	h.w.mu.Unlock()
	h.w.avoid.Avoid(proof.Candidate.Login, h.now.Add(time.Hour), "test stale-proof source prune")

	// The exact broker campaign publication prunes D1 first, while the
	// independently published proof slice remains stale for multiple health
	// evaluations and the dashboard/account snapshot still contains D1.
	h.drops.mu.Lock()
	h.drops.brokerViewOverride = true
	h.drops.brokerCampaigns = nil
	h.drops.mu.Unlock()
	for i := 0; i < 2; i++ {
		h.now = h.now.Add(time.Minute)
		h.w.evaluate(h.now)
		h.w.mu.Lock()
		held := h.w.states[key]
		h.w.mu.Unlock()
		if held != before || held.provisionalProof || held.provisionalOwner != nil ||
			held.RecoveryStage != 4 || !held.evidenceSince.IsZero() || held.NoProgressObs != 0 ||
			held.ReportsSinceProgress != 0 || held.avoidedChannel != proof.Candidate.Login ||
			!held.notifiedStalled || held.exhaustedAt.IsZero() {
			t.Fatalf("stale proof snapshot %d reset/deleted source-pruned episode: before=%p held=%+v", i+1, before, held)
		}
	}

	h.watch.mu.Lock()
	h.watch.proofs = nil
	h.watch.mu.Unlock()
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	after := h.w.states[key]
	h.w.mu.Unlock()
	if after != before || after.RecoveryStage != 4 || after.provisionalProof ||
		after.avoidedChannel != proof.Candidate.Login || !after.notifiedStalled || after.exhaustedAt.IsZero() {
		t.Fatalf("final proof prune recreated the retained ordinary episode: %+v", after)
	}
	if len(h.w.AvoidEntries()) != 1 || len(h.notifier.calls) != 0 {
		t.Fatalf("multi-pass source prune leaked effects or emitted transitions: avoid=%+v notes=%+v", h.w.AvoidEntries(), h.notifier.calls)
	}
}

func TestPromotedProofSnapshotOwnerPruneRacePreservesHandoff(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	proof := h.promoteProvisionalProof(t)
	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now) // establish the promoted-proof health episode
	h.w.mu.Lock()
	before := h.w.states[key]
	before.RecoveryStage = 4
	before.evidenceSince = h.now.Add(-time.Hour)
	before.NoProgressObs = 3
	before.ReportsSinceProgress = stallMinReports
	before.avoidedChannel = proof.Candidate.Login
	before.notifiedStalled = true
	before.exhaustedAt = h.now.Add(-time.Minute)
	h.w.mu.Unlock()
	h.w.avoid.Avoid(proof.Candidate.Login, h.now.Add(time.Hour), "test retained proof recovery")

	// evaluate captures the still-published proof, then the exact broker owner
	// disappears before its per-proof check. This is a reconciliation race, not
	// authority to erase the already ordinary recovery episode.
	h.watch.mu.Lock()
	h.watch.provisionalOwner = nil
	h.watch.mu.Unlock()
	for i := 0; i < 2; i++ {
		h.now = h.now.Add(time.Minute)
		h.w.evaluate(h.now)
		h.w.mu.Lock()
		held := h.w.states[key]
		h.w.mu.Unlock()
		if held != before || !held.provisionalProof || held.RecoveryStage != 4 ||
			!held.evidenceSince.IsZero() || held.NoProgressObs != 0 || held.ReportsSinceProgress != 0 ||
			held.avoidedChannel != proof.Candidate.Login || !held.notifiedStalled || held.exhaustedAt.IsZero() {
			t.Fatalf("proof/owner snapshot race pass %d erased or advanced bounded state: before=%p held=%+v", i+1, before, held)
		}
	}

	// The next broker snapshot omits the pruned proof. The same state transitions
	// in place to ordinary monitoring rather than generic cleanup.
	h.watch.mu.Lock()
	h.watch.proofs = nil
	h.watch.mu.Unlock()
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	after := h.w.states[key]
	h.w.mu.Unlock()
	if after != before || after.provisionalProof || after.RecoveryStage != 4 ||
		after.avoidedChannel != proof.Candidate.Login || !after.notifiedStalled || after.exhaustedAt.IsZero() {
		t.Fatalf("pruned proof did not hand the retained episode to ordinary monitoring: %+v", after)
	}
	if len(h.w.AvoidEntries()) != 1 || len(h.notifier.calls) != 0 {
		t.Fatalf("reconciliation race leaked effects or emitted transitions: avoid=%+v notes=%+v", h.w.AvoidEntries(), h.notifier.calls)
	}
}

func TestPromotedProofCampaignEnvelopeAheadOfWatcherAdoptionPreservesEpisode(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	proof := h.promoteProvisionalProof(t)
	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	before := h.w.states[key]
	before.RecoveryStage = 4
	before.evidenceSince = h.now.Add(-time.Hour)
	before.NoProgressObs = 3
	before.ReportsSinceProgress = stallMinReports
	before.avoidedChannel = proof.Candidate.Login
	before.notifiedStalled = true
	before.exhaustedAt = h.now.Add(-time.Minute)
	h.w.mu.Unlock()
	h.w.avoid.Avoid(proof.Candidate.Login, h.now.Add(time.Hour), "test proof envelope adoption")

	// Campaign publication changes the exact D1 from open-directory evidence to
	// a complete restricted ACL before the watcher adopts the matching proof
	// envelope. D1 is still the broker's current Drop, so this is UNKNOWN
	// publication skew, not source absence and not an ordinary handoff boundary.
	h.campaign.ACL = models.CampaignACL{
		State:      models.ACLRestricted,
		ChannelIDs: []string{h.streamer.ChannelID},
		Complete:   true,
		ObservedAt: h.now,
		Source:     models.ACLSourceCampaignDetails,
	}
	h.campaign.Channels = []string{h.streamer.ChannelID}
	for i := 0; i < 2; i++ {
		h.now = h.now.Add(time.Minute)
		h.w.evaluate(h.now)
		h.w.mu.Lock()
		held := h.w.states[key]
		h.w.mu.Unlock()
		if held != before || !held.provisionalProof || held.RecoveryStage != 4 ||
			!held.evidenceSince.IsZero() || held.NoProgressObs != 0 || held.ReportsSinceProgress != 0 ||
			held.avoidedChannel != proof.Candidate.Login || !held.notifiedStalled || held.exhaustedAt.IsZero() {
			t.Fatalf("campaign/proof adoption gap pass %d reset or advanced episode: before=%p held=%+v", i+1, before, held)
		}
	}

	// The watcher then adopts the revalidated restricted envelope under the same
	// causal proof identity. Health must adopt it in place without restarting the
	// bounded recovery episode or leaking its ordinary effects.
	updated := proof
	updated.Candidate.Evidence = models.ProvisionalEvidenceRestrictedACL
	updated.Candidate.DirectoryObs = 0
	updated.Candidate.RestrictedACL = []string{h.streamer.ChannelID}
	h.watch.mu.Lock()
	h.watch.proofs = []watcher.ProvisionalProof{updated}
	h.watch.mu.Unlock()
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	after := h.w.states[key]
	h.w.mu.Unlock()
	if after != before || !after.provisionalProof || after.RecoveryStage != 4 ||
		after.provisionalCandidate.Evidence != models.ProvisionalEvidenceRestrictedACL ||
		len(after.provisionalCandidate.RestrictedACL) != 1 ||
		after.provisionalCandidate.RestrictedACL[0] != h.streamer.ChannelID ||
		after.avoidedChannel != proof.Candidate.Login || !after.notifiedStalled || after.exhaustedAt.IsZero() {
		t.Fatalf("watcher proof-envelope adoption recreated or weakened episode: %+v", after)
	}
	if len(h.w.AvoidEntries()) != 1 || len(h.notifier.calls) != 0 {
		t.Fatalf("proof-envelope adoption leaked effects or emitted transitions: avoid=%+v notes=%+v", h.w.AvoidEntries(), h.notifier.calls)
	}
}

func TestPromotedProofConfirmedAssignmentTransitionsEstablishedStateInPlace(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	proof := h.promoteProvisionalProof(t)
	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now) // establish a real proof-owned state first

	h.w.mu.Lock()
	before := h.w.states[key]
	before.RecoveryStage = 4
	before.RecoveryStageName = recoveryStages[3].name
	before.evidenceSince = h.now.Add(-time.Hour)
	before.NoProgressObs = 3
	before.ReportsSinceProgress = stallMinReports
	before.avoidedChannel = proof.Candidate.Login
	before.notifiedStalled = true
	before.exhaustedAt = h.now.Add(-time.Minute)
	h.w.mu.Unlock()
	h.w.avoid.Avoid(proof.Candidate.Login, h.now.Add(time.Hour), "test established proof recovery")

	// A real confirmed assignment is higher authority even while the broker's
	// proof snapshot is stale. It must immediately adopt the existing ordinary
	// episode rather than create a new map-key state.
	h.streamer.Stream.SetCampaigns([]*models.Campaign{h.campaign})
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	after := h.w.states[key]
	h.w.mu.Unlock()
	if after != before || after.provisionalProof || after.provisionalOwner != nil ||
		after.provisionalProofID != 0 || after.RecoveryStage != 4 || after.LastMinutes != proof.ProvenMinutes ||
		after.avoidedChannel != proof.Candidate.Login || !after.notifiedStalled || after.exhaustedAt.IsZero() {
		t.Fatalf("confirmed assignment reset proof-owned ordinary recovery effects: before=%p after=%+v", before, after)
	}
	if after.NoProgressObs != 0 || after.ReportsSinceProgress != 0 ||
		(after.evidenceSince.IsZero() || !after.evidenceSince.Equal(h.now)) {
		t.Fatalf("confirmed handoff did not start a fresh ordinary evidence window: %+v", after)
	}
	if len(h.w.AvoidEntries()) != 1 || len(h.notifier.calls) != 0 {
		t.Fatalf("confirmed handoff leaked effects or emitted a transition: avoid=%+v notes=%+v", h.w.AvoidEntries(), h.notifier.calls)
	}
}

func TestPromotedProofConfirmedAssignmentDecisionIsStableForPass(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	proof := h.promoteProvisionalProof(t)
	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	before := h.w.states[key]
	before.RecoveryStage = 4
	h.w.mu.Unlock()

	h.streamer.Stream.SetCampaigns([]*models.Campaign{h.campaign})
	h.watch.mu.Lock()
	h.watch.reportHook = func() { h.streamer.Stream.SetCampaigns(nil) }
	h.watch.mu.Unlock()
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	after := h.w.states[key]
	h.w.mu.Unlock()
	if after != before || after.provisionalProof || after.RecoveryStage != 4 {
		t.Fatalf("assignment clear recreated stale proof state in the same pass: before=%p after=%+v", before, after)
	}
}

func TestPromotedProofWrongAssignedCurrentDropDoesNotSupersedeOrCount(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	proof := h.promoteProvisionalProof(t)
	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	before := h.w.states[key]
	before.RecoveryStage = 4
	before.evidenceSince = h.now.Add(-time.Hour)
	before.NoProgressObs = 3
	h.w.mu.Unlock()

	met := true
	d2 := &models.Drop{
		ID: "drop-2", Name: "Drop Two", MinutesRequired: 480,
		CurrentMinutesWatched: 0, HasPreconditionsMet: &met,
		StartAt: h.now.Add(-time.Hour), EndAt: h.now.Add(time.Hour),
	}
	h.campaign.Drops = append(h.campaign.Drops, d2) // dashboard/account current remains D1
	assigned := h.campaign.Clone()
	assigned.Drops = []*models.Drop{assigned.Drops[1]} // slotted assignment is filtered/current D2
	h.streamer.Stream.SetCampaigns([]*models.Campaign{assigned})
	h.watch.addSuccesses(proof.Candidate.Login, stallMinReports+5)

	h.now = h.now.Add(10 * time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	held := h.w.states[key]
	h.w.mu.Unlock()
	if held != before || !held.provisionalProof || held.RecoveryStage != 4 ||
		!held.evidenceSince.IsZero() || held.NoProgressObs != 0 || held.ReportsSinceProgress != 0 {
		t.Fatalf("D2 assignment superseded or credited D1 proof episode: before=%p held=%+v", before, held)
	}

	// Once the broker prunes the proof, the retained ordinary episode may hand
	// off structurally, but D2 deliveries still cannot satisfy D1's farming gate
	// or advance its recovery stage.
	h.watch.mu.Lock()
	h.watch.proofs = nil
	h.watch.mu.Unlock()
	h.now = h.now.Add(10 * time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	after := h.w.states[key]
	h.w.mu.Unlock()
	if after != before || after.provisionalProof || after.RecoveryStage != 4 ||
		after.NoProgressObs != 0 || after.ReportsSinceProgress != 0 {
		t.Fatalf("D2 deliveries advanced pruned D1 recovery: %+v", after)
	}
}

func TestProvisionalUsesSkipFilteredBrokerCampaignCurrentDrop(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	met := true
	ghost := &models.Drop{
		ID: "ghost-skipped", Name: "Ghost", MinutesRequired: 60,
		HasPreconditionsMet: &met, StartAt: h.now.Add(-time.Hour), EndAt: h.now.Add(time.Hour),
	}
	original := h.campaign.Drops[0]
	h.campaign.Drops = []*models.Drop{ghost, original}
	filtered := h.campaign.Clone()
	filtered.Drops = []*models.Drop{original}
	h.drops.mu.Lock()
	h.drops.brokerViewOverride = true
	h.drops.brokerCampaigns = []*models.Campaign{filtered}
	h.drops.mu.Unlock()

	// The dashboard campaign's CurrentDrop is the ghost entry, but discovery
	// minted the lease from BrokerCampaigns where the skip ledger removed it.
	// Health must validate against that same broker-facing authority view.
	h.w.evaluate(h.now)
	h.now = h.now.Add(time.Minute)
	h.drops.observe(h.now, "")
	h.w.evaluate(h.now)

	lease, ok := h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeaseObserving || lease.Candidate.DropID != original.ID {
		t.Fatalf("valid broker-filtered candidate was released/misidentified: %+v, ok=%v", lease, ok)
	}
}

func TestProvisionalDeliveryACKAloneCannotProve(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	h.armProvisional(t)

	h.watch.addSuccesses("chan", stallMinReports+10)
	h.now = h.now.Add(30 * time.Minute)
	h.w.evaluate(h.now)

	lease, ok := h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeaseObserving || lease.MaxMinutes != lease.BaselineMinutes {
		t.Fatalf("delivery ACKs must not prove or mutate server progress: %+v, ok=%v", lease, ok)
	}
}

func TestProvisionalUnrelatedDropProgressCannotProve(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	h.armProvisional(t)

	other := &models.Campaign{
		ID: "camp-other", Name: "Other Campaign", Game: h.campaign.Game,
		Status: models.CampaignActive, StartAt: h.now.Add(-time.Hour), EndAt: h.now.Add(time.Hour),
		Drops: []*models.Drop{{
			ID: "drop-other", Name: "Other Drop", MinutesRequired: 60,
			CurrentMinutesWatched: 25, StartAt: h.now.Add(-time.Hour), EndAt: h.now.Add(time.Hour),
		}},
	}
	h.drops.mu.Lock()
	h.drops.campaigns = append(h.drops.campaigns, other)
	h.drops.mu.Unlock()

	other.Drops[0].CurrentMinutesWatched++
	h.provisionalObservation(time.Minute, 100, "", 0)
	lease, ok := h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeaseObserving || lease.MaxMinutes != 100 {
		t.Fatalf("unrelated Drop progress must not prove the exact leased Drop: %+v, ok=%v", lease, ok)
	}
}

func TestProvisionalBaselineErrorReleasesWithoutNegative(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	h.w.evaluate(h.now)
	h.provisionalObservation(time.Minute, 100, "inventory timeout", 0)

	if _, ok := h.watch.ProvisionalLease(); ok {
		t.Fatal("failed baseline acquisition should conservatively release the lease")
	}
	h.watch.mu.Lock()
	defer h.watch.mu.Unlock()
	if len(h.watch.released) != 1 || len(h.watch.quarantined) != 0 {
		t.Fatalf("UNKNOWN acquisition is not negative evidence: released=%v quarantined=%v", h.watch.released, h.watch.quarantined)
	}
}

func TestProvisionalTupleSpecificUnknownIsNotZero(t *testing.T) {
	t.Run("post-reservation baseline", func(t *testing.T) {
		h := newProvisionalWatchdogHarness(t)
		h.w.evaluate(h.now)
		h.now = h.now.Add(time.Minute)
		h.drops.observeExactUnknown(h.now, h.campaign.ID, h.campaign.Drops[0].ID)
		h.w.evaluate(h.now)
		lease, ok := h.watch.ProvisionalLease()
		if !ok || lease.State != watcher.ProvisionalLeasePending ||
			lease.PendingObservation != watcher.ProvisionalPendingObservationTupleUnknown || lease.BaselineRun != 0 {
			t.Fatalf("fresh tuple-unknown must retain a baseline-free Pending lease: %+v, ok=%v", lease, ok)
		}
		h.watch.mu.Lock()
		calls := h.watch.tupleUnknownObservations
		released, quarantined := len(h.watch.released), len(h.watch.quarantined)
		h.watch.mu.Unlock()
		if calls != 1 || released != 0 || quarantined != 0 {
			t.Fatalf("tuple-unknown did not authorize exactly one non-negative observation: calls=%d released=%d quarantined=%d", calls, released, quarantined)
		}

		// Re-evaluating the same completed run cannot mint another normal-send
		// authorization; only a strictly newer clean tuple observation may do so.
		h.w.evaluate(h.now)
		h.watch.mu.Lock()
		calls = h.watch.tupleUnknownObservations
		h.watch.mu.Unlock()
		if calls != 1 {
			t.Fatalf("same Inventory run reopened tuple-unknown observation: calls=%d", calls)
		}
	})

	t.Run("pending evidence pauses", func(t *testing.T) {
		h := newProvisionalWatchdogHarness(t)
		key := h.campaign.ID + "\x00" + h.campaign.Drops[0].ID
		h.w.evaluate(h.now)
		h.now = h.now.Add(time.Minute)
		h.drops.observeExact(h.now, "", nil)
		h.w.evaluate(h.now)
		h.watch.addSuccesses("chan", 2)
		h.now = h.now.Add(10 * time.Minute)
		h.drops.observeExact(h.now, "", nil)
		h.w.evaluate(h.now)
		h.w.mu.Lock()
		st := h.w.states[key]
		h.w.mu.Unlock()
		if st == nil || st.evidenceSince.IsZero() || st.ReportsSinceProgress == 0 {
			t.Fatalf("setup did not accrue absence evidence: %+v", st)
		}

		h.now = h.now.Add(time.Minute)
		h.drops.observeExactUnknown(h.now, h.campaign.ID, h.campaign.Drops[0].ID)
		h.w.evaluate(h.now)
		if !st.evidenceSince.IsZero() || st.NoProgressObs != 0 || st.ReportsSinceProgress != 0 {
			t.Fatalf("tuple-unknown must pause/reset accumulated absence evidence: %+v", st.DropProgress)
		}
	})

	t.Run("observing lease", func(t *testing.T) {
		h := newProvisionalWatchdogHarness(t)
		h.armProvisional(t)
		h.now = h.now.Add(time.Minute)
		h.drops.observeExactUnknown(h.now, h.campaign.ID, h.campaign.Drops[0].ID)
		h.w.evaluate(h.now)
		if _, ok := h.watch.ProvisionalLease(); ok {
			t.Fatal("newer valid Inventory run missing the tuple should release an unproved lease")
		}
		h.watch.mu.Lock()
		defer h.watch.mu.Unlock()
		if len(h.watch.released) != 1 || len(h.watch.quarantined) != 0 {
			t.Fatalf("missing observing tuple became negative evidence: released=%v quarantined=%v", h.watch.released, h.watch.quarantined)
		}
	})

	t.Run("promoted proof", func(t *testing.T) {
		h := newProvisionalWatchdogHarness(t)
		proof := h.promoteProvisionalProof(t)
		h.now = h.now.Add(time.Minute)
		h.drops.observeExactUnknown(h.now, h.campaign.ID, h.campaign.Drops[0].ID)
		h.w.evaluate(h.now)
		proofs := h.watch.ProvisionalProofs()
		if len(proofs) != 1 || proofs[0].ProofID != proof.ProofID {
			t.Fatalf("missing tuple must pause, not revoke, promoted authority: %+v", proofs)
		}
	})
}

func TestProvisionalRequiresOneCampaignRevisionFence(t *testing.T) {
	tests := map[string]func(*fakeDropsView){
		"broker source never published": func(view *fakeDropsView) {
			view.brokerGenerationZero = true
		},
		"campaign pool advanced past broker source": func(view *fakeDropsView) {
			view.status.Revision = 1
			view.brokerRevisionOverride = true
			view.brokerSourceRevision = 1
			view.brokerCurrentOverride = true
			view.brokerCurrentRevision = 2
		},
		"broker view lags sync": func(view *fakeDropsView) {
			view.status.Revision = 2
			view.brokerRevisionOverride = true
			view.brokerSourceRevision = 1
		},
		"progress run lags broker": func(view *fakeDropsView) {
			view.status.Revision = 2
			view.brokerRevisionOverride = true
			view.brokerSourceRevision = 2
			stale := uint64(1)
			view.observationRevision = &stale
		},
	}
	for name, mismatch := range tests {
		t.Run(name, func(t *testing.T) {
			h := newProvisionalWatchdogHarness(t)
			h.w.evaluate(h.now)
			h.drops.mu.Lock()
			mismatch(h.drops)
			h.drops.mu.Unlock()
			h.provisionalObservation(time.Minute, 100, "", 0)
			if _, ok := h.watch.ProvisionalLease(); ok {
				t.Fatal("revision-raced baseline must release its unproved lease")
			}
			h.watch.mu.Lock()
			defer h.watch.mu.Unlock()
			if len(h.watch.quarantined) != 0 {
				t.Fatalf("revision race became a negative: %+v", h.watch.quarantined)
			}
		})
	}

	t.Run("promoted proof pauses", func(t *testing.T) {
		h := newProvisionalWatchdogHarness(t)
		proof := h.promoteProvisionalProof(t)
		h.drops.mu.Lock()
		h.drops.status.Revision = 2
		h.drops.brokerRevisionOverride = true
		h.drops.brokerSourceRevision = 2
		stale := uint64(1)
		h.drops.observationRevision = &stale
		h.drops.mu.Unlock()
		h.provisionalObservation(time.Minute, 102, "", stallMinReports)
		proofs := h.watch.ProvisionalProofs()
		if len(proofs) != 1 || proofs[0].ProofID != proof.ProofID {
			t.Fatalf("revision-raced proof observation revoked authority: %+v", proofs)
		}
		key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
		h.w.mu.Lock()
		st := h.w.states[key]
		h.w.mu.Unlock()
		if st == nil || st.LastMinutes != proof.ProvenMinutes || st.NoProgressObs != 0 {
			t.Fatalf("revision-raced proof observation was counted as progress/no-progress: %+v", st)
		}
	})
}

func TestProvisionalObservingUnknownOrRegressedReleasesWithoutNegative(t *testing.T) {
	tests := []struct {
		name    string
		minutes int
		errText string
	}{
		{name: "inventory error", minutes: 100, errText: "inventory timeout"},
		{name: "regressed snapshot", minutes: 99},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newProvisionalWatchdogHarness(t)
			h.armProvisional(t)
			h.provisionalObservation(time.Minute, tc.minutes, tc.errText, 3)

			if _, ok := h.watch.ProvisionalLease(); ok {
				t.Fatal("unproved lower-authority lease should be released on a newer unknown/incoherent observation")
			}
			h.watch.mu.Lock()
			defer h.watch.mu.Unlock()
			if len(h.watch.released) != 1 || len(h.watch.quarantined) != 0 {
				t.Fatalf("release must remain non-negative: released=%v quarantined=%v", h.watch.released, h.watch.quarantined)
			}
		})
	}
}

func TestProvisionalNoProgressUsesRecoveryThenNarrowQuarantine(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	h.armProvisional(t)

	for i := 0; i < 30; i++ {
		h.provisionalObservation(10*time.Minute, 100, "", 3)
		h.watch.mu.Lock()
		quarantined := len(h.watch.quarantined)
		h.watch.mu.Unlock()
		if quarantined != 0 {
			break
		}
	}

	h.watch.mu.Lock()
	quarantined := append([]models.ProvisionalDropCandidate(nil), h.watch.quarantined...)
	h.watch.mu.Unlock()
	if len(quarantined) != 1 {
		t.Fatalf("bounded no-progress recovery must quarantine exactly one current tuple, got %+v", quarantined)
	}
	if quarantined[0].QuarantineKey() == "" {
		t.Fatal("quarantine must be fenced by the exact candidate identity")
	}
	if got := len(h.notifier.byKind("stalled")); got != 0 {
		t.Fatalf("provisional falsification must not emit a generic critical alert, got %d", got)
	}
	if entries := h.w.AvoidEntries(); len(entries) != 0 {
		t.Fatalf("provisional falsification must not create a broad channel avoid: %+v", entries)
	}
	if signal := h.w.ProgressSignal(h.now); signal.Status == StatusStalled {
		t.Fatalf("provisional quarantine must not publish a generic critical Drops Progress alert: %+v", signal)
	}
}

func TestProvisionalRecoveryRefreshRebindsAndStillTerminates(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	initial := h.armProvisional(t)
	h.watch.mu.Lock()
	h.watch.refreshStreamer = h.streamer
	h.watch.rebindLeaseOnRefresh = true
	h.watch.mu.Unlock()

	for i := 0; i < 90; i++ {
		h.provisionalObservation(10*time.Minute, 100, "", 3)
		h.watch.mu.Lock()
		done := len(h.watch.quarantined) != 0
		h.watch.mu.Unlock()
		if done {
			break
		}
	}

	h.watch.mu.Lock()
	quarantined := append([]models.ProvisionalDropCandidate(nil), h.watch.quarantined...)
	h.watch.mu.Unlock()
	if len(quarantined) != 1 {
		t.Fatalf("session-refresh generation bumps must not self-loop recovery forever: quarantined=%+v refreshes=%+v", quarantined, h.watch.refreshCalls())
	}
	if quarantined[0].SessionGeneration <= initial.Candidate.SessionGeneration {
		t.Fatalf("terminal negative targeted the pre-refresh episode, got generation %d <= %d",
			quarantined[0].SessionGeneration, initial.Candidate.SessionGeneration)
	}
	calls := h.watch.refreshCalls()
	if len(calls) != 2 || calls[0].mode != watcher.RefreshStreamInfo || calls[1].mode != watcher.RefreshSession {
		t.Fatalf("rebound recovery did not remain finite and ordered: %+v", calls)
	}
	if len(h.notifier.byKind("stalled")) != 0 || len(h.w.AvoidEntries()) != 0 {
		t.Fatal("unproved rebound lease must retain narrow provisional terminal semantics")
	}
}

func TestProvisionalRecoveryReboundAcceptsAbsenceWithoutLosingStage(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	initial := h.armProvisional(t)
	key := initial.Candidate.CampaignID + "\x00" + initial.Candidate.DropID
	h.watch.mu.Lock()
	h.watch.refreshStreamer = h.streamer
	h.watch.rebindLeaseOnRefresh = true
	h.watch.mu.Unlock()

	h.w.mu.Lock()
	st := h.w.states[key]
	st.RecoveryStage = 2
	h.w.mu.Unlock()
	if !h.w.advanceRecovery(st, h.w.snapshotCfg(), h.now) || st.pending == nil {
		t.Fatal("setup stream-info refresh did not stage a rebound lease")
	}

	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now) // consume exact refresh outcome and capture rebound cursor
	if st.pending != nil || st.RecoveryStage != 3 || !st.provisionalSyncAsked || st.LastMinutes != initial.MaxMinutes {
		t.Fatalf("rebound did not preserve the prior monotone watermark and stage: %+v", st)
	}

	h.now = h.now.Add(time.Minute)
	h.drops.observeExact(h.now, "", nil)
	h.w.evaluate(h.now)
	lease, ok := h.watch.ProvisionalLease()
	if !ok || lease.State != watcher.ProvisionalLeasePending || lease.BaselineRun != 0 || lease.MaxRun == 0 {
		t.Fatalf("rebound exhaustive absence must remain baseline-free Pending evidence: %+v, ok=%v", lease, ok)
	}
	h.w.mu.Lock()
	held := h.w.states[key]
	h.w.mu.Unlock()
	if held != st || held.RecoveryStage != 3 || held.LastMinutes != initial.MaxMinutes {
		t.Fatalf("rebound absence reset bounded recovery or regressed the watermark: old=%p held=%+v", st, held)
	}
}

func TestProvisionalRecoveryRetainsExactAppliedRefreshUntilReboundLease(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	initial := h.armProvisional(t)
	key := initial.Candidate.CampaignID + "\x00" + initial.Candidate.DropID
	h.watch.mu.Lock()
	h.watch.refreshStreamer = h.streamer
	h.watch.rebindLeaseOnRefresh = true
	h.watch.deferNextLeaseRebind = true
	h.watch.mu.Unlock()

	h.w.mu.Lock()
	st := h.w.states[key]
	st.RecoveryStage = 2 // next stage is the correlated stream-info refresh
	h.w.mu.Unlock()
	if !h.w.advanceRecovery(st, h.w.snapshotCfg(), h.now) {
		t.Fatal("stream-info recovery stage did not run")
	}
	if st.pending == nil || st.RecoveryStage != 3 {
		t.Fatalf("refresh stage did not park on its exact correlation: %+v", st)
	}
	if _, ok := h.watch.ProvisionalLease(); ok {
		t.Fatal("test setup expected the old lease invalidated before broker re-admission")
	}

	// Health lands in the real ordering gap: the exact successful outcome and
	// new Stream generation are visible, but arbitration has not published the
	// rebound Pending lease yet. The episode must remain correlated at stage 3.
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	held := h.w.states[key]
	h.w.mu.Unlock()
	if held != st || held.pending == nil || held.RecoveryStage != 3 {
		t.Fatalf("lease gap erased the bounded recovery episode: old=%p held=%+v", st, held)
	}

	h.watch.publishPendingReboundLease()
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now) // exact rebind + capture current run
	h.w.mu.Lock()
	rebound := h.w.states[key]
	h.w.mu.Unlock()
	if rebound != st || rebound.pending != nil || rebound.RecoveryStage != 3 || !rebound.provisionalSyncAsked {
		t.Fatalf("new Pending lease did not consume the exact outcome while preserving stage: %+v", rebound)
	}
	h.provisionalObservation(time.Minute, 100, "", 0) // new post-reservation baseline

	for i := 0; i < 90; i++ {
		h.provisionalObservation(10*time.Minute, 100, "", 3)
		h.watch.mu.Lock()
		done := len(h.watch.quarantined) != 0
		h.watch.mu.Unlock()
		if done {
			break
		}
	}
	h.watch.mu.Lock()
	defer h.watch.mu.Unlock()
	if len(h.watch.quarantined) != 1 || h.watch.quarantined[0].SessionGeneration <= initial.Candidate.SessionGeneration {
		t.Fatalf("retained gap did not terminate against the current rebound lease: %+v", h.watch.quarantined)
	}
}

func TestProvisionalRecoveryGapIgnoresSameLoginResolverClone(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	lease := h.armProvisional(t)
	key := lease.Candidate.CampaignID + "\x00" + lease.Candidate.DropID
	h.watch.mu.Lock()
	h.watch.refreshStreamer = h.streamer
	h.watch.rebindLeaseOnRefresh = true
	h.watch.deferNextLeaseRebind = true
	h.watch.mu.Unlock()

	h.w.mu.Lock()
	st := h.w.states[key]
	st.RecoveryStage = 2
	h.w.mu.Unlock()
	if !h.w.advanceRecovery(st, h.w.snapshotCfg(), h.now) || st.pending == nil {
		t.Fatal("setup refresh did not enter the lease gap")
	}
	if _, ok := h.watch.ProvisionalLease(); ok {
		t.Fatal("test setup expected the old lease to be invalidated")
	}

	// A configured/discovery replacement can legitimately share the normalized
	// login while being a different Streamer owner. The gap correlation belongs
	// to the broker-private pointer captured before refresh and must not consult
	// the ordinary login resolver.
	clone := models.NewStreamer(lease.Candidate.Login, models.StreamerSettings{ClaimDrops: true})
	clone.ChannelID = lease.Candidate.ChannelID
	clone.Stream.Update(lease.Candidate.BroadcastID, "clone", h.campaign.Game, nil, 100)
	clone.SetConfirmedOnline()
	clone.Stream.MarkCampaignAvailabilityUnknown()
	h.w.resolver = func(login string) *models.Streamer {
		if login == lease.Candidate.Login {
			return clone
		}
		return nil
	}

	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	held := h.w.states[key]
	h.w.mu.Unlock()
	if held != st || held.pending == nil || held.RecoveryStage != 3 {
		t.Fatalf("same-login resolver clone erased exact broker-owner gap: old=%p held=%+v", st, held)
	}

	h.watch.publishPendingReboundLease()
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	rebound := h.w.states[key]
	h.w.mu.Unlock()
	if rebound != st || rebound.pending != nil || rebound.RecoveryStage != 3 || !rebound.provisionalSyncAsked || rebound.provisionalOwner != h.streamer {
		t.Fatalf("exact owner rebound did not preserve the recovery episode: %+v", rebound)
	}
}

func TestProvisionalRecoveryGapRejectsExpiredOrForeignOutcome(t *testing.T) {
	tests := map[string]func(*watchdogHarness, *dropState){
		"expired deadline": func(h *watchdogHarness, st *dropState) {
			st.pending.deadline = h.now.Add(-time.Second)
		},
		"foreign signature": func(h *watchdogHarness, st *dropState) {
			outcome, ok := h.watch.LastSessionRefresh(st.Channel)
			if !ok {
				t.Fatal("missing setup refresh outcome")
			}
			outcome.Signature = "foreign"
			h.watch.injectOutcome(st.Channel, outcome)
		},
	}
	for name, invalidate := range tests {
		t.Run(name, func(t *testing.T) {
			h := newProvisionalWatchdogHarness(t)
			lease := h.armProvisional(t)
			key := lease.Candidate.CampaignID + "\x00" + lease.Candidate.DropID
			h.watch.mu.Lock()
			h.watch.refreshStreamer = h.streamer
			h.watch.rebindLeaseOnRefresh = true
			h.watch.deferNextLeaseRebind = true
			h.watch.mu.Unlock()
			h.w.mu.Lock()
			old := h.w.states[key]
			old.RecoveryStage = 2
			h.w.mu.Unlock()
			if !h.w.advanceRecovery(old, h.w.snapshotCfg(), h.now) || old.pending == nil {
				t.Fatal("setup refresh did not enter the lease gap")
			}
			invalidate(h, old)

			h.now = h.now.Add(time.Minute)
			h.w.evaluate(h.now)
			h.w.mu.Lock()
			fresh := h.w.states[key]
			h.w.mu.Unlock()
			if fresh == nil || fresh == old || fresh.provisional || fresh.pending != nil || fresh.RecoveryStage != 0 {
				t.Fatalf("non-exact/bounded gap retained stale provisional recovery: old=%p fresh=%+v", old, fresh)
			}
		})
	}
}

func TestProvisionalExternalSessionChangeStartsNewEpisode(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	old := h.armProvisional(t)
	key := old.Candidate.CampaignID + "\x00" + old.Candidate.DropID
	h.w.mu.Lock()
	oldState := h.w.states[key]
	oldState.RecoveryStage = len(recoveryStages) - 1
	h.w.mu.Unlock()

	// No exact health RequestID/signature outcome exists for this generation
	// change, so it is genuine external session churn, not a recovery rebind.
	h.streamer.Stream.SetSpadeURL("https://external-session.invalid/new")
	snapshot := h.streamer.Stream.ProvisionalDropSnapshot()
	fresh := old
	fresh.LeaseID++
	fresh.State = watcher.ProvisionalLeasePending
	fresh.Candidate.SessionGeneration = snapshot.SessionGeneration
	fresh.Candidate.AvailabilityObs = snapshot.Availability.ObservationID
	fresh.Candidate.AvailabilityKnownGen = snapshot.Availability.KnownGeneration
	fresh.BaselineRun = 0
	fresh.BaselineAt = time.Time{}
	fresh.BaselineMinutes = 0
	fresh.MaxRun = 0
	fresh.MaxAt = time.Time{}
	fresh.MaxMinutes = 0
	h.watch.mu.Lock()
	h.watch.lease = &fresh
	h.watch.mu.Unlock()

	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	newState := h.w.states[key]
	quarantined := len(h.watch.quarantined)
	h.w.mu.Unlock()
	if newState == nil || newState == oldState || newState.RecoveryStage != 0 ||
		newState.provisionalLeaseID != fresh.LeaseID || !newState.provisionalSyncAsked {
		t.Fatalf("external session churn reused the stale recovery episode: old=%p new=%+v", oldState, newState)
	}
	if quarantined != 0 {
		t.Fatalf("stale old episode quarantined the genuine new session: %d negatives", quarantined)
	}
}

func TestProvisionalTerminalQuarantineWaitsForPermitDrainAndFreshObservation(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	lease := h.armProvisional(t)
	key := lease.Candidate.CampaignID + "\x00" + lease.Candidate.DropID
	h.watch.addSuccesses("chan", stallMinReports+3)
	cfg := h.w.snapshotCfg()
	h.w.mu.Lock()
	st := h.w.states[key]
	st.evidenceSince = h.now.Add(-cfg.StallDelay)
	st.NoProgressObs = cfg.StallConfirmations
	st.RecoveryStage = len(recoveryStages) - 1
	h.w.mu.Unlock()
	h.watch.mu.Lock()
	h.watch.quarantineBlocked = true // models a matching in-flight permit
	h.watch.mu.Unlock()

	if !h.w.advanceRecovery(st, cfg, h.now) {
		t.Fatal("held-permit terminal attempt should be deferred")
	}
	if !st.terminalDeferred || st.RecoveryStage != len(recoveryStages)-1 {
		t.Fatalf("deferred terminal stage was retired instead of retained: %+v", st)
	}
	h.watch.mu.Lock()
	if len(h.watch.quarantined) != 0 {
		h.watch.mu.Unlock()
		t.Fatal("held observation permit produced a negative quarantine")
	}
	h.watch.quarantineBlocked = false // permit drains
	h.watch.mu.Unlock()

	// Drain alone is not enough: require a subsequent authoritative exact-Drop
	// observation before retrying the terminal decision.
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.watch.mu.Lock()
	beforeFresh := len(h.watch.quarantined)
	h.watch.mu.Unlock()
	if beforeFresh != 0 || !st.terminalDeferred {
		t.Fatalf("terminal retried without a fresh exact observation: negatives=%d state=%+v", beforeFresh, st)
	}

	h.provisionalObservation(time.Minute, 100, "", 1)
	h.watch.mu.Lock()
	defer h.watch.mu.Unlock()
	if len(h.watch.quarantined) != 1 || h.watch.quarantined[0].QuarantineKey() != lease.Candidate.QuarantineKey() {
		t.Fatalf("fresh unchanged observation did not retry exact current quarantine: %+v", h.watch.quarantined)
	}
}

func TestProvisionalTerminalRejectsObservationCompletedBeforePermitDrain(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	lease := h.armProvisional(t)
	key := lease.Candidate.CampaignID + "\x00" + lease.Candidate.DropID
	h.watch.addSuccesses("chan", stallMinReports+3)
	cfg := h.w.snapshotCfg()
	h.w.mu.Lock()
	st := h.w.states[key]
	st.evidenceSince = h.now.Add(-cfg.StallDelay)
	st.NoProgressObs = cfg.StallConfirmations
	st.RecoveryStage = len(recoveryStages) - 1
	h.w.mu.Unlock()
	h.watch.mu.Lock()
	h.watch.quarantineBlocked = true
	h.watch.mu.Unlock()
	if !h.w.advanceRecovery(st, cfg, h.now) || !st.terminalDeferred {
		t.Fatal("setup terminal quarantine was not blocked by the held permit")
	}

	// Inventory completes while the conflicting transport still owns its permit,
	// but health does not consume that run until after release. The broker's
	// release-time fence must make the next quarantine attempt defer again.
	h.now = h.now.Add(time.Minute)
	h.campaign.Drops[0].CurrentMinutesWatched = 100
	h.drops.observe(h.now, "")
	drainAt := h.now.Add(time.Second)
	h.watch.mu.Lock()
	h.watch.quarantineBlocked = false
	h.watch.quarantineAfterAt = drainAt
	h.watch.mu.Unlock()
	h.now = drainAt.Add(time.Minute)
	h.w.evaluate(h.now)
	h.watch.mu.Lock()
	preDrainNegative := len(h.watch.quarantined)
	h.watch.mu.Unlock()
	if preDrainNegative != 0 || !st.terminalDeferred {
		t.Fatalf("pre-drain Inventory completion authorized a negative: quarantined=%d state=%+v", preDrainNegative, st)
	}

	h.provisionalObservation(time.Minute, 100, "", 1)
	h.watch.mu.Lock()
	defer h.watch.mu.Unlock()
	if len(h.watch.quarantined) != 1 {
		t.Fatalf("post-drain exact observation did not authorize terminal quarantine: %+v", h.watch.quarantined)
	}
}

func TestProvisionalStaleSessionCannotQuarantineFreshLease(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	old, ok := h.watch.ProvisionalLease()
	if !ok {
		t.Fatal("missing old lease")
	}
	st := h.w.provisionalState(old, h.streamer, h.now)
	st.RecoveryStage = len(recoveryStages) - 1

	fresh := old
	fresh.LeaseID++
	fresh.Candidate.SessionGeneration++
	h.watch.mu.Lock()
	h.watch.lease = &fresh
	h.watch.mu.Unlock()

	if !h.w.advanceRecovery(st, h.w.snapshotCfg(), h.now.Add(time.Minute)) {
		t.Fatal("terminal recovery stage should consume the pass")
	}
	h.watch.mu.Lock()
	defer h.watch.mu.Unlock()
	if len(h.watch.quarantined) != 0 || h.watch.lease == nil || h.watch.lease.LeaseID != fresh.LeaseID {
		t.Fatalf("stale episode must not quarantine/release a fresh session lease: quarantined=%v lease=%+v", h.watch.quarantined, h.watch.lease)
	}
}

func TestProgressTransportProbeDefersUntilBrokerPermit(t *testing.T) {
	h := newWatchdogHarness(t)
	st := &dropState{DropProgress: DropProgress{
		CampaignID: "camp", CampaignName: "Campaign", DropID: "drop", DropName: "Drop", Channel: "chan",
	}}
	st.RecoveryStage = 3
	h.watch.permitDenied = true

	if !h.w.advanceRecovery(st, h.w.snapshotCfg(), h.now) {
		t.Fatal("permit denial should consume the pass while leaving the stage pending")
	}
	if st.RecoveryStage != 3 || h.prober.callCount() != 0 {
		t.Fatalf("denied permit must defer the real probe: stage=%d probes=%d", st.RecoveryStage, h.prober.callCount())
	}

	h.watch.mu.Lock()
	h.watch.permitDenied = false
	h.watch.mu.Unlock()
	if !h.w.advanceRecovery(st, h.w.snapshotCfg(), h.now.Add(time.Minute)) {
		t.Fatal("granted permit should run the pending stage")
	}
	if st.RecoveryStage != 4 || h.prober.callCount() != 1 {
		t.Fatalf("granted permit must run exactly one probe: stage=%d probes=%d", st.RecoveryStage, h.prober.callCount())
	}
	h.watch.mu.Lock()
	defer h.watch.mu.Unlock()
	if h.watch.permitAcquires != 2 || h.watch.permitReleases != 1 {
		t.Fatalf("only a granted permit is released: acquired=%d released=%d", h.watch.permitAcquires, h.watch.permitReleases)
	}
}

func TestPromotedProofProbePermitRejectsAuthorityFlip(t *testing.T) {
	tests := map[string]func(*watchdogHarness){
		"session changed": func(h *watchdogHarness) {
			h.streamer.Stream.SetSpadeURL("http://spade.test/new-session")
		},
		"availability became known": func(h *watchdogHarness) {
			obsID := h.streamer.Stream.BeginCampaignAvailabilityObservation()
			h.streamer.Stream.ApplyCampaignAvailability(obsID, true, nil, h.now)
		},
		"confirmed assignment appeared": func(h *watchdogHarness) {
			h.streamer.Stream.SetCampaigns([]*models.Campaign{h.campaign})
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			h := newProvisionalWatchdogHarness(t)
			proof := h.promoteProvisionalProof(t)
			st := h.w.provisionalProofState(proof, h.streamer)
			st.RecoveryStage = 3
			mutate(h) // authority flips after health captured proof state

			if !h.w.advanceRecovery(st, h.w.snapshotCfg(), h.now.Add(time.Minute)) {
				t.Fatal("denied proof permit should consume this pass")
			}
			if st.RecoveryStage != 3 || h.prober.callCount() != 0 {
				t.Fatalf("stale proof must not reach real Probe: stage=%d probes=%d", st.RecoveryStage, h.prober.callCount())
			}
		})
	}
}

func TestPromotedProofRecoveryUsesBrokerOwnerNotSameLoginClone(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	proof := h.promoteProvisionalProof(t)
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)

	// A configured and discovery Streamer may share a login and even the same
	// scalar session tuple. The broker-private owner pointer remains the causal
	// identity; login resolution must never retarget proof recovery to the clone.
	clone := models.NewStreamer("chan", models.StreamerSettings{ClaimDrops: true})
	clone.ChannelID = h.streamer.ChannelID
	clone.Stream.Update(proof.Candidate.BroadcastID, "clone", h.campaign.Game, nil, 100)
	clone.SetConfirmedOnline()
	clone.Stream.MarkCampaignAvailabilityUnknown()
	h.w.resolver = func(login string) *models.Streamer {
		if login == "chan" {
			return clone
		}
		return nil
	}

	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	h.w.mu.Lock()
	st := h.w.states[key]
	st.RecoveryStage = 3
	h.w.mu.Unlock()
	if !h.w.advanceRecovery(st, h.w.snapshotCfg(), h.now.Add(time.Minute)) {
		t.Fatal("proof transport stage did not run")
	}
	if st.RecoveryStage != 4 || h.prober.callCount() != 1 {
		t.Fatalf("same-login resolver clone displaced broker owner: state=%+v probes=%d", st, h.prober.callCount())
	}
}

func TestPromotedProofSameLoginCloneCannotClaimConfirmedSupersession(t *testing.T) {
	h := newProvisionalWatchdogHarness(t)
	proof := h.promoteProvisionalProof(t)
	key := proof.Candidate.CampaignID + "\x00" + proof.Candidate.DropID
	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	before := h.w.states[key]
	h.w.mu.Unlock()

	clone := models.NewStreamer(proof.Candidate.Login, models.StreamerSettings{ClaimDrops: true})
	clone.ChannelID = proof.Candidate.ChannelID
	clone.Stream.Update(proof.Candidate.BroadcastID, "clone", h.campaign.Game, nil, 100)
	clone.SetConfirmedOnline()
	clone.Stream.SetCampaigns([]*models.Campaign{h.campaign})
	h.w.resolver = func(login string) *models.Streamer {
		if login == proof.Candidate.Login {
			return clone
		}
		return nil
	}

	h.now = h.now.Add(time.Minute)
	h.w.evaluate(h.now)
	h.w.mu.Lock()
	after := h.w.states[key]
	h.w.mu.Unlock()
	if after != before || !after.provisionalProof || after.provisionalOwner != h.streamer {
		t.Fatalf("same-login clone stole confirmed supersession from exact proof owner: before=%p after=%+v", before, after)
	}
}

func TestProgressTransportProbeCancellationStillReleasesPermit(t *testing.T) {
	h := newWatchdogHarness(t)
	h.prober = &fakeProber{waitCtx: true}
	h.w.prober = h.prober
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.w.mu.Lock()
	h.w.ctx = ctx
	h.w.mu.Unlock()
	st := &dropState{DropProgress: DropProgress{
		CampaignID: "camp", CampaignName: "Campaign", DropID: "drop", DropName: "Drop", Channel: "chan",
	}}
	st.RecoveryStage = 3

	if !h.w.advanceRecovery(st, h.w.snapshotCfg(), h.now) {
		t.Fatal("cancelled probe stage should complete boundedly")
	}
	if h.prober.callCount() != 1 {
		t.Fatalf("expected one context-aware probe call, got %d", h.prober.callCount())
	}
	h.watch.mu.Lock()
	defer h.watch.mu.Unlock()
	if h.watch.permitAcquires != 1 || h.watch.permitReleases != 1 {
		t.Fatalf("cancelled probe leaked observation ownership: acquired=%d released=%d",
			h.watch.permitAcquires, h.watch.permitReleases)
	}
}

func TestProgressWatermarkNeverRegresses(t *testing.T) {
	h := newWatchdogHarness(t)
	h.w.evaluate(h.now)
	before := h.state(t)

	h.campaign.Drops[0].CurrentMinutesWatched = 90
	h.tick(time.Minute, true, 0)
	if got := h.state(t).LastMinutes; got != 100 {
		t.Fatalf("regressed observation lowered monotone watermark: got %d, want 100", got)
	}

	h.campaign.Drops[0].CurrentMinutesWatched = 100
	h.tick(time.Minute, true, 0)
	after := h.state(t)
	if !after.LastProgressAt.Equal(before.LastProgressAt) {
		t.Fatalf("100 -> 90 -> 100 must not look like recovery: before=%v after=%v", before.LastProgressAt, after.LastProgressAt)
	}
}

func TestProgressWatchdogPublishesMonitoringLifecycle(t *testing.T) {
	h := newWatchdogHarness(t)
	h.watch.mu.Lock()
	enabled := h.watch.monitorEnabled
	h.watch.mu.Unlock()
	if !enabled {
		t.Fatal("enabled watchdog must publish provisional-monitor ownership at construction")
	}

	h.w.Start(context.Background())
	h.w.Stop()
	h.watch.mu.Lock()
	defer h.watch.mu.Unlock()
	if h.watch.monitorEnabled {
		t.Fatal("stopped watchdog must release provisional-monitor ownership")
	}
}

func TestProvisionalCoordinatorUsesExistingEvaluationLoop(t *testing.T) {
	// Keep the bootstrap coordination inside the watchdog's existing loop. A
	// source-level assertion is deterministic (unlike runtime goroutine counts,
	// which fluctuate with the test runner) and prevents a future per-lease
	// worker/timer from silently changing lifecycle or cancellation semantics.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "progress.go", nil, 0)
	if err != nil {
		t.Fatalf("parse progress.go: %v", err)
	}
	targets := map[string]bool{
		"evaluateProvisional":       false,
		"evaluateProvisionalProof":  false,
		"provisionalState":          false,
		"provisionalRecoveryRebind": false,
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		if _, target := targets[fn.Name.Name]; !target {
			continue
		}
		targets[fn.Name.Name] = true
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.GoStmt:
				t.Errorf("%s starts an independent goroutine at %s", fn.Name.Name, fset.Position(n.Pos()))
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "time" {
					return true
				}
				switch sel.Sel.Name {
				case "After", "AfterFunc", "NewTicker", "NewTimer", "Tick":
					t.Errorf("%s creates an independent timer at %s", fn.Name.Name, fset.Position(n.Pos()))
				}
			}
			return true
		})
	}
	for name, found := range targets {
		if !found {
			t.Errorf("target provisional coordinator %s not found", name)
		}
	}
}
