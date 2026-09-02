package watcher

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func provisionalWatcherFixture(t *testing.T, login, channelID, gameID string) (*models.Streamer, models.ProvisionalDropCandidate) {
	t.Helper()
	streamer := models.NewStreamer(login, models.StreamerSettings{ClaimDrops: true})
	streamer.ChannelID = channelID
	streamer.SetConfirmedOnline()
	game := &models.Game{ID: gameID, Name: gameID}
	streamer.Stream.Update("broadcast-"+login, "", game, nil, 1)
	streamer.Stream.SetSpadeURL("https://spade.invalid/" + login)
	streamer.Stream.SetPayload(channelID, "broadcast-"+login, "account", login, game)
	streamer.Stream.MarkCampaignAvailabilityUnknown()
	snapshot := streamer.Stream.ProvisionalDropSnapshot()
	return streamer, models.ProvisionalDropCandidate{
		CampaignID:           "campaign-1",
		Campaign:             "Campaign One",
		DropID:               "drop-1",
		Drop:                 "Drop One",
		GameID:               gameID,
		Login:                login,
		ChannelID:            channelID,
		BroadcastID:          snapshot.BroadcastID,
		SessionGeneration:    snapshot.SessionGeneration,
		AvailabilityObs:      snapshot.Availability.ObservationID,
		AvailabilityKnownGen: snapshot.Availability.KnownGeneration,
		DirectoryObs:         1,
		Evidence:             models.ProvisionalEvidenceDirectory,
	}
}

func conflictWatcherFixture(login, channelID, gameID string, known bool, campaignIDs ...string) *models.Streamer {
	streamer := models.NewStreamer(login, models.StreamerSettings{ClaimDrops: true})
	streamer.ChannelID = channelID
	streamer.SetConfirmedOnline()
	if gameID != "" {
		streamer.Stream.Update("broadcast-"+login, "", &models.Game{ID: gameID}, nil, 1)
	}
	if known {
		streamer.Stream.SetCampaignIDs(campaignIDs)
	} else {
		streamer.Stream.MarkCampaignAvailabilityUnknown()
	}
	return streamer
}

func provisionalSlot(streamer *models.Streamer, candidate models.ProvisionalDropCandidate) slotOccupant {
	return slotOccupant{
		streamer:        streamer,
		origin:          OriginDiscovery,
		idx:             -1,
		reasonCode:      ReasonDiscoveryFill,
		reason:          "provisional",
		selectedAt:      time.Now(),
		provisionalDrop: cloneProvisionalCandidate(&candidate),
	}
}

func configuredProvisionalFixture(t *testing.T, streamer *models.Streamer) models.ProvisionalDropCandidate {
	t.Helper()
	streamer.ChannelID = "configured-channel"
	streamer.SetConfirmedOnline()
	streamer.Stream.Update("configured-broadcast", "", &models.Game{ID: "game-1"}, nil, 1)
	streamer.Stream.MarkCampaignAvailabilityUnknown()
	snapshot := streamer.Stream.ProvisionalDropSnapshot()
	return models.ProvisionalDropCandidate{
		CampaignID: "campaign-1", Campaign: "Campaign One",
		DropID: "drop-1", Drop: "Drop One", GameID: "game-1",
		Login: streamer.GetUsername(), ChannelID: streamer.ChannelID,
		BroadcastID: snapshot.BroadcastID, SessionGeneration: snapshot.SessionGeneration,
		AvailabilityObs: snapshot.Availability.ObservationID, AvailabilityKnownGen: snapshot.Availability.KnownGeneration,
		DirectoryObs: 1, Evidence: models.ProvisionalEvidenceDirectory,
	}
}

func TestConfiguredProvisionalCandidateRequiresExactOwnerPointer(t *testing.T) {
	w, _ := newTestWatcher(1)
	owner := w.streamers[0]
	candidate := configuredProvisionalFixture(t, owner)

	clone := models.NewStreamer(owner.GetUsername(), models.DefaultStreamerSettings())
	clone.ChannelID = owner.ChannelID
	clone.SetConfirmedOnline()
	clone.Stream.Update(candidate.BroadcastID, "", &models.Game{ID: candidate.GameID}, nil, 1)
	clone.Stream.MarkCampaignAvailabilityUnknown()
	cloneSnapshot := clone.Stream.ProvisionalDropSnapshot()
	cloneCandidate := candidate
	cloneCandidate.SessionGeneration = cloneSnapshot.SessionGeneration
	cloneCandidate.AvailabilityObs = cloneSnapshot.Availability.ObservationID

	tests := []struct {
		name      string
		streamer  *models.Streamer
		candidate models.ProvisionalDropCandidate
		want      bool
	}{
		{name: "exact owner", streamer: owner, candidate: candidate, want: true},
		{name: "same login clone", streamer: clone, candidate: cloneCandidate},
		{name: "retargeted channel scalar", streamer: owner, candidate: func() models.ProvisionalDropCandidate {
			changed := candidate
			changed.ChannelID = "other-channel"
			return changed
		}()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &staticSource{name: OriginDiscovery, cand: []Candidate{{
				Streamer: test.streamer, ProvisionalDrop: &test.candidate,
			}}}
			got := w.gatherCandidates(tickCtx(w), []CandidateSource{source}, nil)
			if (len(got) == 1) != test.want {
				t.Fatalf("gathered=%+v, want accepted=%v", got, test.want)
			}
			if test.want && (got[0].Streamer != owner || got[0].ProvisionalDrop == &test.candidate) {
				t.Fatal("exact owner or candidate snapshot was not retained independently")
			}
		})
	}
}

func TestConfiguredProvisionalOverlaysPhaseAWithOrdinaryFallback(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.SetProvisionalMonitoringEnabled(true)
	owner := w.streamers[0]
	candidate := configuredProvisionalFixture(t, owner)

	slots, _ := w.arbitrate([]int{0}, []Candidate{{
		Streamer: owner, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
	}}, time.Now())
	if len(slots) != 1 || slots[0].streamer != owner || slots[0].idx != 0 ||
		slots[0].origin != OriginConfigured || slots[0].provisionalDrop == nil ||
		slots[0].provisionalFallback == nil || slots[0].provisionalFallback.provisionalDrop != nil {
		t.Fatalf("configured Phase-A overlay = %+v", slots)
	}

	admitted, _ := w.reconcileProvisionalSlots(slots, nil, time.Now())
	if len(admitted) != 1 || admitted[0].streamer != owner || admitted[0].idx != 0 ||
		admitted[0].provisionalDrop == nil {
		t.Fatalf("exact configured overlay was not admitted in place: %+v", admitted)
	}
	if lease, ok := w.ProvisionalLease(); !ok || !lease.Candidate.SameLeaseIdentity(candidate) {
		t.Fatalf("configured overlay lease = %+v, ok=%v", lease, ok)
	}

	// A broker-side exact negative makes the overlay inadmissible, but cannot
	// remove the independently selected points/fairness slot.
	w.ReleaseProvisionalLease(w.provisionalLease.LeaseID)
	w.observationMu.Lock()
	w.recordProvisionalQuarantineLocked(owner, candidate)
	w.observationMu.Unlock()
	slots, _ = w.arbitrate([]int{0}, []Candidate{{
		Streamer: owner, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
	}}, time.Now())
	fallback, _ := w.reconcileProvisionalSlots(slots, nil, time.Now())
	if len(fallback) != 1 || fallback[0].streamer != owner || fallback[0].idx != 0 ||
		fallback[0].origin != OriginConfigured || fallback[0].provisionalDrop != nil {
		t.Fatalf("failed configured overlay did not preserve ordinary slot: %+v", fallback)
	}
	if _, ok := w.ProvisionalLease(); ok {
		t.Fatal("quarantined configured overlay retained a lease")
	}
}

func TestProvisionalQuarantineQueryUsesNarrowSessionKey(t *testing.T) {
	w, _ := newTestWatcher(1)
	owner := w.streamers[0]
	candidate := configuredProvisionalFixture(t, owner)
	w.observationMu.Lock()
	w.recordProvisionalQuarantineLocked(owner, candidate)
	w.observationMu.Unlock()

	if !w.IsProvisionalQuarantined(owner, candidate) {
		t.Fatal("exact quarantined tuple was not reported")
	}
	refreshed := candidate
	owner.Stream.MarkCampaignAvailabilityUnknown()
	refreshed.AvailabilityObs = owner.Stream.ProvisionalDropSnapshot().Availability.ObservationID
	if !w.IsProvisionalQuarantined(owner, refreshed) {
		t.Fatal("routine UNKNOWN refresh escaped the narrow quarantine key")
	}
	owner.Stream.SetSpadeURL("https://spade.invalid/new-session")
	newSession := refreshed
	newSession.SessionGeneration = owner.Stream.ProvisionalDropSnapshot().SessionGeneration
	if w.IsProvisionalQuarantined(owner, newSession) {
		t.Fatal("new playback session inherited the prior session's quarantine")
	}
}

func TestProvisionalQuarantineIsExactOwnerAndStructurallyReconciled(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.SetProvisionalMonitoringEnabled(true)
	owner := w.streamers[0]
	candidateA := configuredProvisionalFixture(t, owner)
	candidateB := candidateA
	candidateB.CampaignID = "campaign-b"
	candidateB.Campaign = "Campaign B"
	candidateB.DropID = "drop-b"
	candidateB.Drop = "Drop B"

	w.observationMu.Lock()
	w.recordProvisionalQuarantineLocked(owner, candidateA)
	w.observationMu.Unlock()
	if !w.IsProvisionalQuarantined(owner, candidateA) {
		t.Fatal("exact private owner did not retain its terminal negative")
	}

	clone := models.NewStreamer(owner.GetUsername(), owner.GetSettings())
	clone.ChannelID = owner.ChannelID
	if w.IsProvisionalQuarantined(clone, candidateA) {
		t.Fatal("scalar-identical replacement Streamer inherited another object's negative")
	}

	refreshed := candidateA
	refreshed.AvailabilityObs++
	refreshed.DirectoryObs++
	if !w.IsProvisionalQuarantined(owner, refreshed) {
		t.Fatal("routine UNKNOWN/Directory envelope refresh escaped a terminal negative")
	}
	removedOwner, removedCandidate := provisionalWatcherFixture(t, "removed", "removed-id", "game-removed")
	w.observationMu.Lock()
	w.recordProvisionalQuarantineLocked(removedOwner, removedCandidate)
	w.observationMu.Unlock()

	full := []ProvisionalQuarantineOwnerScope{{
		Streamer: owner, BroadcastID: candidateA.BroadcastID, SessionGeneration: candidateA.SessionGeneration,
		Candidates: []models.ProvisionalDropCandidate{candidateA, candidateB},
	}}
	if !w.ReconcileProvisionalQuarantine(1, ProvisionalDirectoryAuthority{}, nil, full) {
		t.Fatal("fresh complete scope was rejected")
	}
	if !w.IsProvisionalQuarantined(owner, candidateA) {
		t.Fatal("quarantined A was deleted merely because selectable B shared its scope")
	}
	if w.IsProvisionalQuarantined(removedOwner, removedCandidate) {
		t.Fatal("complete scope retained a private owner that left authoritative enumeration")
	}

	onlyB := []ProvisionalQuarantineOwnerScope{{
		Streamer: owner, BroadcastID: candidateB.BroadcastID, SessionGeneration: candidateB.SessionGeneration,
		Candidates: []models.ProvisionalDropCandidate{candidateB},
	}}
	if w.ReconcileProvisionalQuarantine(1, ProvisionalDirectoryAuthority{}, nil, onlyB) {
		t.Fatal("equal complete-scope generation was accepted for pruning")
	}
	if w.ReconcileProvisionalQuarantine(0, ProvisionalDirectoryAuthority{}, nil, onlyB) {
		t.Fatal("unfenced scope was accepted for pruning")
	}
	if !w.IsProvisionalQuarantined(owner, candidateA) {
		t.Fatal("stale/unfenced scope deleted an existing negative")
	}
	if !w.ReconcileProvisionalQuarantine(2, ProvisionalDirectoryAuthority{}, nil, onlyB) {
		t.Fatal("strictly newer complete scope was rejected")
	}
	if w.IsProvisionalQuarantined(owner, candidateA) {
		t.Fatal("new complete scope did not prune an absent campaign/drop slot")
	}
}

func TestProvisionalQuarantineIncompleteDirectoryPrunesOnlyRestrictedEvidence(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	owner, open := provisionalWatcherFixture(t, "owner", "channel-1", "game-1")
	restricted := open
	restricted.CampaignID = "restricted-campaign"
	restricted.Campaign = "Restricted Campaign"
	restricted.DropID = "restricted-drop"
	restricted.Drop = "Restricted Drop"
	restricted.DirectoryObs = 0
	restricted.Evidence = models.ProvisionalEvidenceRestrictedACL
	restricted.RestrictedACL = []string{restricted.ChannelID}

	w.observationMu.Lock()
	w.recordProvisionalQuarantineLocked(owner, open)
	w.recordProvisionalQuarantineLocked(owner, restricted)
	w.observationMu.Unlock()

	empty := []ProvisionalQuarantineOwnerScope{{
		Streamer: owner, BroadcastID: open.BroadcastID, SessionGeneration: open.SessionGeneration,
	}}
	if !w.ReconcileProvisionalQuarantine(1, ProvisionalDirectoryAuthority{
		UncertainGameIDs: []string{open.GameID},
	}, []ProvisionalAccountWork{{CampaignID: open.CampaignID, DropID: open.DropID, GameID: open.GameID}}, empty) {
		t.Fatal("fresh account-complete scope with incomplete Directory was rejected")
	}
	if w.IsProvisionalQuarantined(owner, restricted) {
		t.Fatal("absent RestrictedACL negative survived complete account/roster scope")
	}
	if !w.IsProvisionalQuarantined(owner, open) {
		t.Fatal("incomplete Directory scope deleted an open-campaign negative")
	}

	if !w.ReconcileProvisionalQuarantine(2, ProvisionalDirectoryAuthority{}, nil, empty) {
		t.Fatal("new complete Directory scope was rejected")
	}
	if w.IsProvisionalQuarantined(owner, open) {
		t.Fatal("complete Directory scope retained an absent open-campaign negative")
	}
}

func TestProvisionalAcceptedScopeFailsClosedAfterRejectedReconcile(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	owner, candidate := provisionalWatcherFixture(t, "owner", "channel-1", "game-1")

	w.RequireProvisionalScope()
	admitted, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
	)
	if len(admitted) != 0 {
		t.Fatal("scope requirement without a committed namespace admitted a candidate")
	}
	if w.ReconcileProvisionalQuarantine(1, ProvisionalDirectoryAuthority{}, nil, []ProvisionalQuarantineOwnerScope{{}}) {
		t.Fatal("malformed fenced scope was accepted")
	}
	admitted, _ = w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
	)
	if len(admitted) != 0 {
		t.Fatal("candidate outside an accepted exact scope acquired a lease")
	}
	if _, ok := w.ProvisionalLease(); ok {
		t.Fatal("rejected scope published provisional ownership")
	}
	w.observationMu.Lock()
	stored := w.recordProvisionalQuarantineLocked(owner, candidate)
	w.observationMu.Unlock()
	if stored || len(w.provisionalQuarantine.owners) != 0 {
		t.Fatal("candidate outside accepted scope became a terminal negative")
	}

	scope := []ProvisionalQuarantineOwnerScope{{
		Streamer: owner, BroadcastID: candidate.BroadcastID, SessionGeneration: candidate.SessionGeneration,
		Candidates: []models.ProvisionalDropCandidate{candidate},
	}}
	if !w.ReconcileProvisionalQuarantine(2, ProvisionalDirectoryAuthority{}, nil, scope) {
		t.Fatal("fresh exact scope was rejected")
	}
	admitted, _ = w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
	)
	if len(admitted) != 1 {
		t.Fatal("candidate was not admitted after its exact scope committed")
	}
}

func TestProvisionalAcceptedScopeRejectsLaterEnvelopeAfterStaleReconcile(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	initialOwner, initial := provisionalWatcherFixture(t, "initial", "initial-channel", "game-1")
	initialScope := []ProvisionalQuarantineOwnerScope{{
		Streamer: initialOwner, BroadcastID: initial.BroadcastID, SessionGeneration: initial.SessionGeneration,
		Candidates: []models.ProvisionalDropCandidate{initial},
	}}
	if !w.ReconcileProvisionalQuarantine(1, ProvisionalDirectoryAuthority{}, nil, initialScope) {
		t.Fatal("initial exact scope was rejected")
	}

	for i := 0; i < 100; i++ {
		owner, candidate := provisionalWatcherFixture(
			t, "later-"+strconv.Itoa(i), "later-channel-"+strconv.Itoa(i), "game-1",
		)
		candidate.CampaignID = "later-campaign-" + strconv.Itoa(i)
		candidate.DropID = "later-drop-" + strconv.Itoa(i)
		laterScope := []ProvisionalQuarantineOwnerScope{{
			Streamer: owner, BroadcastID: candidate.BroadcastID, SessionGeneration: candidate.SessionGeneration,
			Candidates: []models.ProvisionalDropCandidate{candidate},
		}}
		if w.ReconcileProvisionalQuarantine(1, ProvisionalDirectoryAuthority{}, nil, laterScope) {
			t.Fatalf("equal generation %d replaced the accepted scope", i)
		}
		admitted, _ := w.reconcileProvisionalSlots(
			[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
		)
		if len(admitted) != 0 {
			t.Fatalf("later envelope %d bypassed failed scope reconciliation", i)
		}
		w.observationMu.Lock()
		stored := w.recordProvisionalQuarantineLocked(owner, candidate)
		w.observationMu.Unlock()
		if stored || len(w.provisionalQuarantine.owners) != 0 {
			t.Fatalf("later envelope %d grew quarantine outside accepted scope", i)
		}
	}
	if len(w.provisionalQuarantine.accepted) != 1 {
		t.Fatalf("failed reconciliations grew accepted owner scope to %d", len(w.provisionalQuarantine.accepted))
	}
}

func TestProvisionalAdmissionRejectsConfirmedAssignmentAfterScopePublication(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	owner, candidate := provisionalWatcherFixture(t, "owner", "channel-1", "game-1")
	scope := []ProvisionalQuarantineOwnerScope{{
		Streamer: owner, BroadcastID: candidate.BroadcastID, SessionGeneration: candidate.SessionGeneration,
		Candidates: []models.ProvisionalDropCandidate{candidate},
	}}
	if !w.ReconcileProvisionalQuarantine(1, ProvisionalDirectoryAuthority{}, nil, scope) {
		t.Fatal("initial exact scope was rejected")
	}

	// The confirmed assignment lands after discovery's scope publication and is
	// then cleared by ordinary UNKNOWN evaluation. The same-broadcast marker in
	// the broker's atomic stream snapshot must still close admission.
	owner.Stream.SetCampaigns([]*models.Campaign{{ID: candidate.CampaignID}})
	owner.Stream.SetCampaigns(nil)
	admitted, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
	)
	if len(admitted) != 0 {
		t.Fatal("same-broadcast confirmed assignment lost the publication-to-admission race")
	}
	if _, ok := w.ProvisionalLease(); ok {
		t.Fatal("broker retained a lease after the same-broadcast confirmed assignment")
	}
}

func TestProvisionalAcceptedScopeSurvivesStopOnlyAsFailClosedPolicy(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	owner, candidate := provisionalWatcherFixture(t, "owner", "channel-1", "game-1")
	scope := []ProvisionalQuarantineOwnerScope{{
		Streamer: owner, BroadcastID: candidate.BroadcastID, SessionGeneration: candidate.SessionGeneration,
		Candidates: []models.ProvisionalDropCandidate{candidate},
	}}
	if !w.ReconcileProvisionalQuarantine(1, ProvisionalDirectoryAuthority{}, nil, scope) {
		t.Fatal("initial exact scope was rejected")
	}
	w.observationMu.Lock()
	if !w.recordProvisionalQuarantineLocked(owner, candidate) {
		w.observationMu.Unlock()
		t.Fatal("accepted terminal negative was not stored")
	}
	namespace := w.provisionalQuarantine.namespace
	w.observationMu.Unlock()

	_ = w.Stop()
	w.SetProvisionalMonitoringEnabled(true)
	if w.provisionalQuarantine.namespace == namespace {
		t.Fatal("Stop/re-enable reused the prior monitoring namespace")
	}
	if len(w.provisionalQuarantine.owners) != 0 || len(w.provisionalQuarantine.accepted) != 0 ||
		w.IsProvisionalQuarantined(owner, candidate) {
		t.Fatal("Stop/re-enable retained prior scope or negative state")
	}
	admitted, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
	)
	if len(admitted) != 0 {
		t.Fatal("Stop/re-enable disabled exact-scope enforcement")
	}
	if !w.ReconcileProvisionalQuarantine(2, ProvisionalDirectoryAuthority{}, nil, scope) {
		t.Fatal("fresh post-restart exact scope was rejected")
	}
	admitted, _ = w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
	)
	if len(admitted) != 1 {
		t.Fatal("fresh post-restart exact scope did not restore admission")
	}
}

func TestProvisionalQuarantineSessionChurnAndMonitoringLifecycleStayBounded(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.SetProvisionalMonitoringEnabled(true)
	owner := w.streamers[0]
	candidate := configuredProvisionalFixture(t, owner)
	namespace := w.provisionalQuarantine.namespace

	for i := 0; i < 100; i++ {
		previous := candidate
		owner.Stream.SetSpadeURL("https://spade.invalid/session-" + strconv.Itoa(i))
		stream := owner.Stream.ProvisionalDropSnapshot()
		candidate.SessionGeneration = stream.SessionGeneration
		candidate.BroadcastID = stream.BroadcastID

		// A new session is reconsiderable before it independently reaches a
		// terminal decision; terminal storage then replaces the same structural
		// campaign/drop slot instead of appending a history entry.
		if w.IsProvisionalQuarantined(owner, candidate) {
			t.Fatalf("new session %d inherited the prior negative", i)
		}
		w.observationMu.Lock()
		w.recordProvisionalQuarantineLocked(owner, candidate)
		ownerState := w.provisionalQuarantine.owners[owner]
		entries := len(ownerState.candidates)
		w.observationMu.Unlock()
		if entries != 1 {
			t.Fatalf("session churn grew one structural slot to %d entries", entries)
		}
		if w.IsProvisionalQuarantined(owner, previous) {
			t.Fatalf("superseded session %d remained the active structural negative", i)
		}
		if !w.IsProvisionalQuarantined(owner, candidate) {
			t.Fatalf("latest terminal session %d was not retained", i)
		}
	}

	w.SetProvisionalMonitoringEnabled(true)
	if w.provisionalQuarantine.namespace != namespace || !w.IsProvisionalQuarantined(owner, candidate) {
		t.Fatal("redundant monitoring enable reset the live authority namespace")
	}
	w.SetProvisionalMonitoringEnabled(false)
	if len(w.provisionalQuarantine.owners) != 0 {
		t.Fatal("monitoring disable retained private owner pointers")
	}
	w.SetProvisionalMonitoringEnabled(true)
	if w.provisionalQuarantine.namespace == namespace {
		t.Fatal("disable/re-enable reused the prior monitoring namespace")
	}
	if w.IsProvisionalQuarantined(owner, candidate) {
		t.Fatal("disable/re-enable inherited a prior lifecycle negative")
	}
}

func TestProvisionalQuarantineAuthoritativeCampaignAndOwnerChurnStayBounded(t *testing.T) {
	t.Run("campaign and drop progression", func(t *testing.T) {
		w, _ := newTestWatcher(1)
		w.SetProvisionalMonitoringEnabled(true)
		owner := w.streamers[0]
		candidate := configuredProvisionalFixture(t, owner)
		var previous models.ProvisionalDropCandidate
		for i := 1; i <= 100; i++ {
			candidate.CampaignID = "campaign-" + strconv.Itoa(i)
			candidate.DropID = "drop-" + strconv.Itoa(i)
			scope := []ProvisionalQuarantineOwnerScope{{
				Streamer: owner, BroadcastID: candidate.BroadcastID, SessionGeneration: candidate.SessionGeneration,
				Candidates: []models.ProvisionalDropCandidate{candidate},
			}}
			if !w.ReconcileProvisionalQuarantine(uint64(i), ProvisionalDirectoryAuthority{}, nil, scope) {
				t.Fatalf("complete campaign scope %d was rejected", i)
			}
			w.observationMu.Lock()
			stored := w.recordProvisionalQuarantineLocked(owner, candidate)
			w.observationMu.Unlock()
			if !stored {
				t.Fatalf("accepted campaign/drop %d was not stored", i)
			}
			if entries := len(w.provisionalQuarantine.owners[owner].candidates); entries != 1 {
				t.Fatalf("campaign/drop churn retained %d structural entries", entries)
			}
			if i > 1 && w.IsProvisionalQuarantined(owner, previous) {
				t.Fatalf("removed campaign/drop %d survived authoritative scope", i-1)
			}
			previous = candidate
		}
	})

	t.Run("ephemeral owner replacement", func(t *testing.T) {
		w := &MinuteWatcher{}
		w.SetProvisionalMonitoringEnabled(true)
		var previousOwner *models.Streamer
		var previousCandidate models.ProvisionalDropCandidate
		for i := 1; i <= 100; i++ {
			owner, candidate := provisionalWatcherFixture(
				t, "owner-"+strconv.Itoa(i), "channel-"+strconv.Itoa(i), "game-1",
			)
			scope := []ProvisionalQuarantineOwnerScope{{
				Streamer: owner, BroadcastID: candidate.BroadcastID, SessionGeneration: candidate.SessionGeneration,
				Candidates: []models.ProvisionalDropCandidate{candidate},
			}}
			if !w.ReconcileProvisionalQuarantine(uint64(i), ProvisionalDirectoryAuthority{}, nil, scope) {
				t.Fatalf("complete owner scope %d was rejected", i)
			}
			w.observationMu.Lock()
			stored := w.recordProvisionalQuarantineLocked(owner, candidate)
			w.observationMu.Unlock()
			if !stored {
				t.Fatalf("accepted owner scope %d was not stored", i)
			}
			if owners := len(w.provisionalQuarantine.owners); owners != 1 {
				t.Fatalf("ephemeral owner churn retained %d private pointers", owners)
			}
			if previousOwner != nil && w.IsProvisionalQuarantined(previousOwner, previousCandidate) {
				t.Fatalf("removed owner %d survived authoritative scope", i-1)
			}
			previousOwner, previousCandidate = owner, candidate
		}
	})

	t.Run("restricted churn during persistent Directory error", func(t *testing.T) {
		w := &MinuteWatcher{}
		w.SetProvisionalMonitoringEnabled(true)
		var previousOwner *models.Streamer
		var previousCandidate models.ProvisionalDropCandidate
		for i := 1; i <= 100; i++ {
			owner, candidate := provisionalWatcherFixture(
				t, "restricted-owner-"+strconv.Itoa(i), "restricted-channel-"+strconv.Itoa(i), "game-1",
			)
			candidate.CampaignID = "restricted-campaign-" + strconv.Itoa(i)
			candidate.DropID = "restricted-drop-" + strconv.Itoa(i)
			candidate.DirectoryObs = 0
			candidate.Evidence = models.ProvisionalEvidenceRestrictedACL
			candidate.RestrictedACL = []string{candidate.ChannelID}
			scope := []ProvisionalQuarantineOwnerScope{{
				Streamer: owner, BroadcastID: candidate.BroadcastID, SessionGeneration: candidate.SessionGeneration,
				Candidates: []models.ProvisionalDropCandidate{candidate},
			}}
			if !w.ReconcileProvisionalQuarantine(uint64(i), ProvisionalDirectoryAuthority{
				UncertainGameIDs: []string{candidate.GameID},
			}, nil, scope) {
				t.Fatalf("account-complete restricted scope %d was rejected", i)
			}
			w.observationMu.Lock()
			stored := w.recordProvisionalQuarantineLocked(owner, candidate)
			w.observationMu.Unlock()
			if !stored {
				t.Fatalf("accepted restricted scope %d was not stored", i)
			}
			if owners := len(w.provisionalQuarantine.owners); owners != 1 {
				t.Fatalf("persistent Directory error retained %d restricted private owners", owners)
			}
			if entries := len(w.provisionalQuarantine.owners[owner].candidates); entries != 1 {
				t.Fatalf("persistent Directory error retained %d restricted campaign/drop slots", entries)
			}
			if previousOwner != nil && w.IsProvisionalQuarantined(previousOwner, previousCandidate) {
				t.Fatalf("removed restricted owner/campaign %d survived complete account scope", i-1)
			}
			previousOwner, previousCandidate = owner, candidate
		}
	})

	t.Run("open churn in successful game beside persistent errored game", func(t *testing.T) {
		w := &MinuteWatcher{}
		w.SetProvisionalMonitoringEnabled(true)
		erroredOwner, errored := provisionalWatcherFixture(
			t, "errored-owner", "errored-channel", "game-errored",
		)
		initialScope := []ProvisionalQuarantineOwnerScope{{
			Streamer:          erroredOwner,
			BroadcastID:       errored.BroadcastID,
			SessionGeneration: errored.SessionGeneration,
			Candidates:        []models.ProvisionalDropCandidate{errored},
		}}
		if !w.ReconcileProvisionalQuarantine(1, ProvisionalDirectoryAuthority{}, nil, initialScope) {
			t.Fatal("initial complete errored-game scope was rejected")
		}
		w.observationMu.Lock()
		stored := w.recordProvisionalQuarantineLocked(erroredOwner, errored)
		w.observationMu.Unlock()
		if !stored {
			t.Fatal("initial errored-game negative was not stored")
		}

		var previousOwner *models.Streamer
		var previousCandidate models.ProvisionalDropCandidate
		for i := 1; i <= 100; i++ {
			owner, candidate := provisionalWatcherFixture(
				t, "successful-owner-"+strconv.Itoa(i), "successful-channel-"+strconv.Itoa(i), "game-success",
			)
			candidate.CampaignID = "successful-campaign-" + strconv.Itoa(i)
			candidate.DropID = "successful-drop-" + strconv.Itoa(i)
			scope := []ProvisionalQuarantineOwnerScope{{
				Streamer:          owner,
				BroadcastID:       candidate.BroadcastID,
				SessionGeneration: candidate.SessionGeneration,
				Candidates:        []models.ProvisionalDropCandidate{candidate},
			}}
			if !w.ReconcileProvisionalQuarantine(uint64(i+1), ProvisionalDirectoryAuthority{
				UncertainGameIDs: []string{errored.GameID},
			}, []ProvisionalAccountWork{
				{CampaignID: errored.CampaignID, DropID: errored.DropID, GameID: errored.GameID},
				{CampaignID: candidate.CampaignID, DropID: candidate.DropID, GameID: candidate.GameID},
			}, scope) {
				t.Fatalf("mixed per-game scope %d was rejected", i)
			}
			w.observationMu.Lock()
			stored = w.recordProvisionalQuarantineLocked(owner, candidate)
			w.observationMu.Unlock()
			if !stored {
				t.Fatalf("successful-game candidate %d was outside accepted scope", i)
			}
			if !w.IsProvisionalQuarantined(erroredOwner, errored) {
				t.Fatalf("errored-game open A was pruned during successful-game churn %d", i)
			}
			if owners := len(w.provisionalQuarantine.owners); owners != 2 {
				t.Fatalf("mixed Directory churn retained %d negative owners, want errored A + current success", owners)
			}
			if accepted := len(w.provisionalQuarantine.accepted); accepted != 2 {
				t.Fatalf("mixed Directory churn retained %d accepted owners, want errored A + current success", accepted)
			}
			if previousOwner != nil && w.IsProvisionalQuarantined(previousOwner, previousCandidate) {
				t.Fatalf("completed-game owner/campaign %d survived its next complete listing", i-1)
			}
			previousOwner, previousCandidate = owner, candidate
		}

		replacementWork := []ProvisionalAccountWork{{
			CampaignID: "replacement-campaign",
			DropID:     "replacement-drop",
			GameID:     errored.GameID,
		}}
		if !w.ReconcileProvisionalQuarantine(102, ProvisionalDirectoryAuthority{
			UncertainGameIDs: []string{errored.GameID},
		}, replacementWork, nil) {
			t.Fatal("same-game replacement account work was rejected")
		}
		if w.IsProvisionalQuarantined(erroredOwner, errored) {
			t.Fatal("removed campaign A survived merely because campaign B kept the same errored game active")
		}
		if len(w.provisionalQuarantine.owners) != 0 || len(w.provisionalQuarantine.accepted) != 0 {
			t.Fatalf("same-game account churn retained owners=%d accepted=%d",
				len(w.provisionalQuarantine.owners), len(w.provisionalQuarantine.accepted))
		}
	})
}

func TestRestrictedProvisionalDirectoryEvidenceIsRejectedAtBrokerBoundary(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "restricted", "restricted-id", "game-1")
	candidate.Evidence = models.ProvisionalEvidenceRestrictedACL
	candidate.RestrictedACL = []string{candidate.ChannelID}
	// Restricted authority must be self-contained; a non-zero Directory
	// observation is a mixed envelope even if every other field is current.
	candidate.DirectoryObs = 1
	slots, _ := w.reconcileProvisionalSlots(
		[]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now(),
	)
	if len(slots) != 0 {
		t.Fatal("broker admitted restricted ACL mixed with Directory evidence")
	}
	if _, ok := w.ProvisionalLease(); ok {
		t.Fatal("invalid restricted envelope reserved an observation lease")
	}
}

func TestConfiguredProvisionalProofReclassifiesAndRestoresExactFallback(t *testing.T) {
	for _, restricted := range []bool{false, true} {
		name := "open"
		if restricted {
			name = "restricted"
		}
		t.Run(name, func(t *testing.T) {
			w, _ := newTestWatcher(1)
			w.SetProvisionalMonitoringEnabled(true)
			w.selectionReasons = map[int]string{0: "original configured selection"}
			owner := w.streamers[0]
			candidate := configuredProvisionalFixture(t, owner)
			if restricted {
				candidate.Evidence = models.ProvisionalEvidenceRestrictedACL
				candidate.DirectoryObs = 0
				candidate.RestrictedACL = []string{owner.ChannelID}
			}

			initial, _ := w.arbitrate([]int{0}, []Candidate{{
				Streamer: owner, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
			}}, time.Now())
			admitted, _ := w.reconcileProvisionalSlots(initial, nil, time.Now())
			if len(admitted) != 1 {
				t.Fatal("failed to admit configured overlay")
			}
			lease, _ := w.ProvisionalLease()
			baselineAt := lease.ReservedAt.Add(time.Second)
			if !w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 1) ||
				!w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 2) {
				t.Fatal("failed to prove configured overlay")
			}

			promoted, _ := w.arbitrate([]int{0}, []Candidate{{
				Streamer: owner, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
			}}, time.Now())
			wantReason := ReasonActiveDrop
			if restricted {
				wantReason = ReasonRestrictedDrop
			}
			if len(promoted) != 1 || !promoted[0].provisionalProven || promoted[0].reasonCode != wantReason ||
				!promoted[0].hasCampaignPolicy || len(promoted[0].campaignPolicy.CampaignIDs) != 1 ||
				promoted[0].campaignPolicy.CampaignIDs[0] != candidate.CampaignID ||
				promoted[0].provisionalFallback == nil || promoted[0].provisionalFallback.reasonCode != ReasonPriority ||
				w.selectionReasons[0] != promoted[0].reason {
				t.Fatalf("configured proof promotion = %+v", promoted)
			}

			owner.Stream.SetSpadeURL("https://spade.invalid/proof-invalidated")
			restored, _ := w.reconcileProvisionalSlots(promoted, nil, time.Now())
			if len(restored) != 1 || restored[0].streamer != owner || restored[0].idx != 0 ||
				restored[0].origin != OriginConfigured || restored[0].reasonCode != ReasonPriority ||
				restored[0].provisionalDrop != nil || w.selectionReasons[0] != "original configured selection" {
				t.Fatalf("invalidated configured proof did not restore exact fallback: %+v reason=%q",
					restored, w.selectionReasons[0])
			}
		})
	}
}

func TestOrderedProvisionalContendersFallThroughBrokerConflictSameTick(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.SetProvisionalMonitoringEnabled(true)
	ordinary := w.streamers[0]
	ordinary.ChannelID = "ordinary-channel"
	ordinary.SetConfirmedOnline()
	ordinary.Stream.Update("ordinary-broadcast", "", &models.Game{ID: "game-1"}, nil, 1)
	ordinary.Stream.SetCampaignIDs([]string{"campaign-1"})

	conflicting, first := provisionalWatcherFixture(t, "first", "first-channel", "game-1")
	clean, second := provisionalWatcherFixture(t, "second", "second-channel", "game-2")
	slots, waiting, contenders := w.arbitrateWithProvisionalContenders([]int{0}, []Candidate{
		{Streamer: conflicting, Origin: OriginDiscovery, ProvisionalDrop: &first},
		{Streamer: clean, Origin: OriginDiscovery, ProvisionalDrop: &second},
	}, time.Now())
	if len(slots) != constants.MaxSimultaneousStreams || len(contenders) != 2 {
		t.Fatalf("arbitration lost ordered contenders: slots=%d contenders=%d", len(slots), len(contenders))
	}
	final, _ := w.reconcileProvisionalSlots(slots, waiting, time.Now(), contenders)
	if len(final) != constants.MaxSimultaneousStreams || final[0].streamer != ordinary || final[1].streamer != clean {
		t.Fatalf("conflicting first contender hid clean second contender: %+v", final)
	}
	lease, ok := w.ProvisionalLease()
	if !ok || !lease.Candidate.SameLeaseIdentity(second) {
		t.Fatalf("clean second contender did not own lease: %+v ok=%v", lease, ok)
	}
}

func TestOrderedProvisionalContendersControlLeaseReplacement(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	a, candidateA := provisionalWatcherFixture(t, "a", "channel-a", "game-a")
	b, candidateB := provisionalWatcherFixture(t, "b", "channel-b", "game-b")

	_, _, contenders := w.arbitrateWithProvisionalContenders(nil, []Candidate{{
		Streamer: a, Origin: OriginDiscovery, ProvisionalDrop: &candidateA,
	}}, time.Now())
	w.reconcileProvisionalSlots(nil, nil, time.Now(), contenders)
	first, ok := w.ProvisionalLease()
	if !ok || !first.Candidate.SameLeaseIdentity(candidateA) {
		t.Fatalf("initial lease = %+v ok=%v", first, ok)
	}

	// The source puts a strict-stronger tuple first; the broker must not preserve
	// the old lease merely because it is still present later in the full list.
	_, _, contenders = w.arbitrateWithProvisionalContenders(nil, []Candidate{
		{Streamer: b, Origin: OriginDiscovery, ProvisionalDrop: &candidateB},
		{Streamer: a, Origin: OriginDiscovery, ProvisionalDrop: &candidateA},
	}, time.Now())
	w.reconcileProvisionalSlots(nil, nil, time.Now(), contenders)
	second, ok := w.ProvisionalLease()
	if !ok || second.LeaseID == first.LeaseID || !second.Candidate.SameLeaseIdentity(candidateB) {
		t.Fatalf("ordered stronger contender did not replace lease: first=%+v second=%+v", first, second)
	}

	// On a complete source tie, discovery orders the exact active tuple first;
	// rescanning that order must retain its lease/baseline identity.
	_, _, contenders = w.arbitrateWithProvisionalContenders(nil, []Candidate{
		{Streamer: b, Origin: OriginDiscovery, ProvisionalDrop: &candidateB},
		{Streamer: a, Origin: OriginDiscovery, ProvisionalDrop: &candidateA},
	}, time.Now())
	w.reconcileProvisionalSlots(nil, nil, time.Now(), contenders)
	retained, ok := w.ProvisionalLease()
	if !ok || retained.LeaseID != second.LeaseID {
		t.Fatalf("first ordered active contender churned lease: before=%+v after=%+v", second, retained)
	}
}

func TestProvisionalConflictTruthTable(t *testing.T) {
	_, open := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	restricted := open
	restricted.Evidence = models.ProvisionalEvidenceRestrictedACL
	restricted.DirectoryObs = 0
	restricted.RestrictedACL = []string{"candidate-id", "member-id"}

	tests := []struct {
		name      string
		candidate models.ProvisionalDropCandidate
		streamer  *models.Streamer
		conflict  bool
	}{
		{"open different game", open, conflictWatcherFixture("other", "other-id", "game-2", false), false},
		{"open missing game fails closed", open, conflictWatcherFixture("other", "other-id", "", true), true},
		{"open same game unknown", open, conflictWatcherFixture("other", "other-id", "game-1", false), true},
		{"open known empty excludes", open, conflictWatcherFixture("other", "other-id", "game-1", true), false},
		{"open known other campaign excludes", open, conflictWatcherFixture("other", "other-id", "game-1", true, "campaign-2"), false},
		{"open known exact conflicts", open, conflictWatcherFixture("other", "other-id", "game-1", true, "campaign-1"), true},
		{"restricted nonmember unknown excluded by complete ACL", restricted, conflictWatcherFixture("other", "outside-id", "game-1", false), false},
		{"restricted nonmember known exact excluded by complete ACL", restricted, conflictWatcherFixture("other", "outside-id", "game-1", true, "campaign-1"), false},
		{"restricted member unknown conflicts", restricted, conflictWatcherFixture("other", "member-id", "game-1", false), true},
		{"restricted member known empty excludes", restricted, conflictWatcherFixture("other", "member-id", "game-1", true), false},
		{"restricted member known exact conflicts", restricted, conflictWatcherFixture("other", "member-id", "game-1", true, "campaign-1"), true},
		{"restricted missing channel identity fails closed", restricted, conflictWatcherFixture("other", "", "game-1", true), true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := provisionalConflictsWithStreamer(test.candidate, test.streamer); got != test.conflict {
				t.Fatalf("conflict=%v, want %v", got, test.conflict)
			}
		})
	}
}

func TestProvisionalArbitrationIsFillOnlyAndDefensivelyCapped(t *testing.T) {
	w, _ := newTestWatcher(3)
	provisionalStreamer, candidate := provisionalWatcherFixture(t, "provisional", "provisional-id", "game-1")
	ordinary := discoveryStreamer("ordinary", false)

	// Even when the source lists it first, the provisional proposal cannot take
	// the only free slot ahead of an ordinary proposal.
	slots, _ := w.arbitrate([]int{0}, []Candidate{
		{Streamer: provisionalStreamer, Origin: OriginDiscovery, ProvisionalDrop: &candidate},
		{Streamer: ordinary, Origin: OriginDiscovery},
	}, time.Now())
	if len(slots) != constants.MaxSimultaneousStreams {
		t.Fatalf("slots=%d, want %d", len(slots), constants.MaxSimultaneousStreams)
	}
	for _, slot := range slots {
		if slot.provisionalDrop != nil {
			t.Fatal("provisional proposal took capacity ahead of an ordinary proposal")
		}
	}

	// A malformed upstream configured list and a provisional contender still
	// cannot make the broker publish a third slot.
	slots, _ = w.arbitrate([]int{0, 1, 2}, []Candidate{{
		Streamer: provisionalStreamer, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
	}}, time.Now())
	if len(slots) != constants.MaxSimultaneousStreams {
		t.Fatalf("defensive cap: slots=%d, want %d", len(slots), constants.MaxSimultaneousStreams)
	}

	// The final reconciliation boundary independently refuses to reserve a lease
	// for a provisional occupant that would be truncated as a third slot.
	w.SetProvisionalMonitoringEnabled(true)
	first := conflictWatcherFixture("first", "first-id", "other-game", true)
	second := conflictWatcherFixture("second", "second-id", "other-game", true)
	slots, _ = w.reconcileProvisionalSlots([]slotOccupant{
		{streamer: first, origin: OriginConfigured, idx: 0},
		{streamer: second, origin: OriginConfigured, idx: 1},
		provisionalSlot(provisionalStreamer, candidate),
	}, nil, time.Now())
	if len(slots) != constants.MaxSimultaneousStreams {
		t.Fatalf("reconciliation cap: slots=%d, want %d", len(slots), constants.MaxSimultaneousStreams)
	}
	if _, ok := w.ProvisionalLease(); ok {
		t.Fatal("reconciliation reserved a lease for a truncated third slot")
	}
}

func TestProvisionalLeaseRequiresBaselineAndOnlyServerDeltaProves(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	reservedAt := time.Now()
	slots, _ := w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, reservedAt)
	if len(slots) != 1 {
		t.Fatalf("admitted slots=%d, want 1", len(slots))
	}
	lease, ok := w.ProvisionalLease()
	if !ok || lease.State != ProvisionalLeasePending || !lease.Candidate.SameLeaseIdentity(candidate) {
		t.Fatalf("pending lease mismatch: ok=%v lease=%+v", ok, lease)
	}

	if _, ok := w.acquireProvisionalBootstrapPermit(streamer, lease.LeaseID); ok {
		t.Fatal("Pending lease sent before a fresh complete Inventory observation")
	}
	permit, ok := w.AcquireObservationPermit(streamer, lease.LeaseID)
	if !ok {
		t.Fatal("Pending exact owner was denied its bounded health recovery permit")
	}
	w.completeObservationPermit(permit, true)
	lease, _ = w.ProvisionalLease()
	if lease.State != ProvisionalLeasePending || len(w.ProvisionalProofs()) != 0 {
		t.Fatal("bootstrap beacon ACK armed or proved a Pending lease")
	}
	if w.ArmProvisionalLease(lease.LeaseID, 1, reservedAt.Add(-time.Second), 10) {
		t.Fatal("accepted a pre-reservation baseline")
	}
	if w.ArmProvisionalLease(lease.LeaseID, 1, reservedAt, 10) {
		t.Fatal("accepted a baseline completed exactly at reservation")
	}
	baselineAt := reservedAt.Add(time.Second)
	if !w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 10) {
		t.Fatal("fresh post-reservation baseline was rejected")
	}
	permit, ok = w.AcquireObservationPermit(streamer, lease.LeaseID)
	if !ok {
		t.Fatal("observing lease owner was denied its beacon permit")
	}
	w.ReleaseObservationPermit(permit)

	if w.ObserveProvisionalProgress(lease.LeaseID, 1, baselineAt.Add(time.Second), 11) {
		t.Fatal("same progress run was accepted")
	}
	if w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt, 10) {
		t.Fatal("newer run with an equal observation timestamp was accepted")
	}
	if w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 9) {
		t.Fatal("non-monotone server minutes were accepted")
	}
	if !w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 10) {
		t.Fatal("newer equal no-progress observation was rejected")
	}
	lease, _ = w.ProvisionalLease()
	if lease.State != ProvisionalLeaseObserving {
		t.Fatalf("equal server minutes proved lease: state=%s", lease.State)
	}
	if !w.ObserveProvisionalProgress(lease.LeaseID, 3, baselineAt.Add(2*time.Second), 11) {
		t.Fatal("newer positive server delta was rejected")
	}
	lease, _ = w.ProvisionalLease()
	if lease.State != ProvisionalLeaseProven || lease.MaxMinutes != 11 {
		t.Fatalf("positive exact-Drop delta did not prove lease: %+v", lease)
	}
	proofs := w.ProvisionalProofs()
	if len(proofs) != 1 || proofs[0].ProofID != lease.LeaseID ||
		!proofs[0].Candidate.SameLeaseIdentity(candidate) || proofs[0].ProvenMinutes != 11 {
		t.Fatalf("exact server proof was not published coherently: %+v", proofs)
	}

	// Lease snapshots own their ACL memory.
	lease.Candidate.RestrictedACL = append(lease.Candidate.RestrictedACL, "mutated")
	again, _ := w.ProvisionalLease()
	if len(again.Candidate.RestrictedACL) != len(candidate.RestrictedACL) {
		t.Fatal("caller mutated broker-owned candidate ACL through lease snapshot")
	}
	proofs[0].Candidate.RestrictedACL = append(proofs[0].Candidate.RestrictedACL, "mutated")
	if got := len(w.ProvisionalProofs()[0].Candidate.RestrictedACL); got != len(candidate.RestrictedACL) {
		t.Fatal("caller mutated broker-owned candidate ACL through proof snapshot")
	}
}

func TestProvisionalAbsenceAdvancesPendingCursorButCannotArmOrProve(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	reservedAt := time.Now()
	w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, reservedAt)
	lease, _ := w.ProvisionalLease()
	if w.ObserveProvisionalAbsence(lease.LeaseID, 1, reservedAt) {
		t.Fatal("accepted a Pending observation completed exactly at reservation")
	}
	absentAt := reservedAt.Add(time.Second)
	if !w.ObserveProvisionalAbsence(lease.LeaseID, 1, absentAt) {
		t.Fatal("fresh exhaustive exact-tuple absence was rejected")
	}
	lease, _ = w.ProvisionalLease()
	if lease.State != ProvisionalLeasePending || lease.MaxRun != 1 || !lease.MaxAt.Equal(absentAt) ||
		lease.BaselineRun != 0 || !lease.BaselineAt.IsZero() || len(w.ProvisionalProofs()) != 0 {
		t.Fatalf("absence became a fabricated zero-minute baseline/proof: %+v", lease)
	}
	if w.ObserveProvisionalAbsence(lease.LeaseID, 1, absentAt.Add(time.Second)) {
		t.Fatal("same exhaustive Inventory run advanced the Pending cursor twice")
	}
	if w.ObserveProvisionalAbsence(lease.LeaseID, 2, absentAt) {
		t.Fatal("non-newer exhaustive observation time advanced the Pending cursor")
	}
	if w.ArmProvisionalLease(lease.LeaseID, 1, absentAt.Add(time.Second), 0) {
		t.Fatal("the same run that reported absence was reused as an exact baseline")
	}
	if w.ArmProvisionalLease(lease.LeaseID, 2, absentAt, 0) {
		t.Fatal("an exact row without a strictly newer observation time armed the lease")
	}
	foundAt := absentAt.Add(time.Second)
	if !w.ArmProvisionalLease(lease.LeaseID, 2, foundAt, 0) {
		t.Fatal("strictly newer exact-Found row did not arm the lease")
	}
	lease, _ = w.ProvisionalLease()
	if lease.State != ProvisionalLeaseObserving || lease.BaselineRun != 2 || !lease.BaselineAt.Equal(foundAt) {
		t.Fatalf("exact-Found baseline mismatch after absence: %+v", lease)
	}
	if !w.ObserveProvisionalProgress(lease.LeaseID, 3, foundAt.Add(time.Second), 1) {
		t.Fatal("later exact server delta did not prove the armed lease")
	}
	lease, _ = w.ProvisionalLease()
	if lease.State != ProvisionalLeaseProven || len(w.ProvisionalProofs()) != 1 {
		t.Fatal("exact absence/found/delta lifecycle did not end in proof")
	}
}

func TestPendingBootstrapPermitRequiresFreshCompleteObservation(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	slot := provisionalSlot(streamer, candidate)
	w.reconcileProvisionalSlots([]slotOccupant{slot}, nil, time.Now())
	lease, _ := w.ProvisionalLease()

	if _, ok := w.acquireProvisionalBootstrapPermit(streamer, lease.LeaseID); ok {
		t.Fatal("new Pending lease sent before a complete post-reservation observation")
	}
	recoveryPermit, ok := w.AcquireObservationPermit(streamer, lease.LeaseID)
	if !ok {
		t.Fatal("bounded health recovery permit was denied")
	}
	w.ReleaseObservationPermit(recoveryPermit)
	if _, ok := w.acquireProvisionalBootstrapPermit(streamer, lease.LeaseID); ok {
		t.Fatal("public recovery permit opened a normal bootstrap send")
	}

	absentAt := lease.ReservedAt.Add(time.Second)
	if !w.ObserveProvisionalAbsence(lease.LeaseID, 1, absentAt) {
		t.Fatal("fresh complete absence did not open a bootstrap send")
	}
	lease, _ = w.ProvisionalLease()
	if lease.State != ProvisionalLeasePending || lease.PendingObservation != ProvisionalPendingObservationAbsence ||
		lease.MaxRun != 1 || !lease.MaxAt.Equal(absentAt) || lease.BaselineRun != 0 {
		t.Fatalf("absence fabricated or lost Pending authority: %+v", lease)
	}
	permit, ok := w.acquireProvisionalBootstrapPermit(streamer, lease.LeaseID)
	if !ok {
		t.Fatal("fresh absence token was denied")
	}
	w.completeObservationPermit(permit, true)
	if _, ok := w.acquireProvisionalBootstrapPermit(streamer, lease.LeaseID); ok {
		t.Fatal("one complete absence opened more than one normal send")
	}
	if w.ObserveProvisionalAbsence(lease.LeaseID, 1, absentAt.Add(time.Second)) {
		t.Fatal("same run reopened a consumed bootstrap token")
	}
	lease, _ = w.ProvisionalLease()
	if lease.State != ProvisionalLeasePending || len(w.ProvisionalProofs()) != 0 {
		t.Fatal("successful bootstrap ACK armed or proved the Pending lease")
	}

	secondAt := absentAt.Add(time.Second)
	if !w.ObserveProvisionalAbsence(lease.LeaseID, 2, secondAt) {
		t.Fatal("newer complete absence did not reopen exactly one send")
	}
	permit, ok = w.acquireProvisionalBootstrapPermit(streamer, lease.LeaseID)
	if !ok {
		t.Fatal("newer absence token was denied")
	}
	w.completeObservationPermit(permit, false)
	if _, ok := w.acquireProvisionalBootstrapPermit(streamer, lease.LeaseID); ok {
		t.Fatal("failed transport refunded a consumed bootstrap token")
	}

	unknownAt := secondAt.Add(time.Second)
	if !w.ObserveProvisionalTupleUnknown(lease.LeaseID, 3, unknownAt) {
		t.Fatal("fresh exact-tuple UNKNOWN did not open a bootstrap send")
	}
	lease, _ = w.ProvisionalLease()
	if lease.PendingObservation != ProvisionalPendingObservationTupleUnknown || lease.BaselineRun != 0 {
		t.Fatalf("tuple UNKNOWN became absence or exact baseline: %+v", lease)
	}
	permit, ok = w.acquireProvisionalBootstrapPermit(streamer, lease.LeaseID)
	if !ok {
		t.Fatal("tuple-UNKNOWN token was denied")
	}
	w.completeObservationPermit(permit, false)
	if _, ok := w.acquireProvisionalBootstrapPermit(streamer, lease.LeaseID); ok {
		t.Fatal("tuple-UNKNOWN observation opened more than one send")
	}
	// An error/partial result makes no Observe call and therefore cannot open a
	// token; only a strictly newer clean exact Found row may arm the lease.
	if w.ArmProvisionalLease(lease.LeaseID, 3, unknownAt.Add(time.Second), 0) {
		t.Fatal("tuple-UNKNOWN run was reused as an exact baseline")
	}
	foundAt := unknownAt.Add(time.Second)
	if !w.ArmProvisionalLease(lease.LeaseID, 4, foundAt, 0) {
		t.Fatal("strictly newer exact Found row did not arm the lease")
	}
	lease, _ = w.ProvisionalLease()
	if lease.State != ProvisionalLeaseObserving || lease.PendingObservation != ProvisionalPendingObservationNone {
		t.Fatalf("exact Found did not clear Pending observation state: %+v", lease)
	}
	if !w.ObserveProvisionalProgress(lease.LeaseID, 5, foundAt.Add(time.Second), 1) {
		t.Fatal("later fresh exact delta did not prove the armed lease")
	}
}

func TestPendingBootstrapTokenCannotCrossLeaseIdentity(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	oldLease, _ := w.ProvisionalLease()
	if !w.ObserveProvisionalAbsence(oldLease.LeaseID, 1, oldLease.ReservedAt.Add(time.Second)) {
		t.Fatal("failed to open old lease token")
	}

	streamer.Stream.SetSpadeURL("https://spade.invalid/replacement-session")
	snapshot := streamer.Stream.ProvisionalDropSnapshot()
	replacement := candidate
	replacement.BroadcastID = snapshot.BroadcastID
	replacement.SessionGeneration = snapshot.SessionGeneration
	w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, replacement)}, nil, time.Now())
	newLease, ok := w.ProvisionalLease()
	if !ok || newLease.LeaseID == oldLease.LeaseID {
		t.Fatalf("session replacement did not publish a new lease: old=%+v new=%+v", oldLease, newLease)
	}
	if _, ok := w.acquireProvisionalBootstrapPermit(streamer, newLease.LeaseID); ok {
		t.Fatal("old lease's unspent token leaked into its replacement")
	}
	newAt := newLease.ReservedAt.Add(time.Second)
	if !w.ObserveProvisionalTupleUnknown(newLease.LeaseID, 1, newAt) {
		t.Fatal("replacement lease rejected its own fresh complete observation")
	}
	permit, ok := w.acquireProvisionalBootstrapPermit(streamer, newLease.LeaseID)
	if !ok {
		t.Fatal("replacement lease's own token was denied")
	}
	w.completeObservationPermit(permit, true)
}

func TestProvisionalProofPromotesIntoExistingBrokerPriority(t *testing.T) {
	w, _ := newTestWatcher(2)
	for i, configured := range w.streamers {
		configured.ChannelID = "configured-id-" + string(rune('a'+i))
		configured.Stream.Update("configured-broadcast", "", &models.Game{ID: "other-game"}, nil, 1)
		configured.Stream.SetCampaignIDs(nil)
	}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")

	admitted, _ := w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	if len(admitted) != 1 {
		t.Fatal("failed to reserve provisional lease")
	}
	lease, _ := w.ProvisionalLease()
	baselineAt := lease.ReservedAt.Add(time.Second)
	if !w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 4) ||
		!w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 5) {
		t.Fatal("failed to publish exact server proof")
	}

	slots, _ := w.arbitrate([]int{0, 1}, []Candidate{{
		Streamer: streamer, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
	}}, time.Now())
	if len(slots) != constants.MaxSimultaneousStreams {
		t.Fatalf("promoted arbitration slots=%d, want %d", len(slots), constants.MaxSimultaneousStreams)
	}
	found := false
	for _, slot := range slots {
		if slot.streamer == streamer {
			found = true
			if !slot.provisionalProven || slot.reasonCode != ReasonActiveDrop ||
				!slot.hasCampaignPolicy || len(slot.campaignPolicy.CampaignIDs) != 1 ||
				slot.campaignPolicy.CampaignIDs[0] != candidate.CampaignID {
				t.Fatalf("promoted slot did not use exact existing broker authority: %+v", slot)
			}
		}
	}
	if !found {
		t.Fatal("server-proven candidate did not compete with and displace an ordinary configured slot")
	}
	slots, _ = w.reconcileProvisionalSlots(slots, nil, time.Now())
	if len(slots) != constants.MaxSimultaneousStreams {
		t.Fatalf("promoted reconciliation lost a slot: %d", len(slots))
	}
	if _, ok := w.ProvisionalLease(); ok {
		t.Fatal("promoted broker authority retained its observation lease")
	}
	if len(w.ProvisionalProofs()) != 1 {
		t.Fatal("promoted broker authority lost its direct server proof")
	}
}

func TestProvisionalPermitExcludesConflictingInflightBeacon(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	conflicting := conflictWatcherFixture("other", "other-id", "game-1", false)

	inflight, ok := w.AcquireObservationPermit(conflicting, 0)
	if !ok {
		t.Fatal("ordinary permit unexpectedly denied before a lease existed")
	}
	slots, _ := w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	if len(slots) != 0 {
		t.Fatal("lease started against a conflicting in-flight beacon")
	}
	w.ReleaseObservationPermit(inflight)

	slots, _ = w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	if len(slots) != 1 {
		t.Fatal("lease did not start after conflicting permit released")
	}
	lease, _ := w.ProvisionalLease()
	if !w.ArmProvisionalLease(lease.LeaseID, 1, lease.ReservedAt.Add(time.Second), 0) {
		t.Fatal("failed to arm lease")
	}
	if _, ok := w.AcquireObservationPermit(conflicting, 0); ok {
		t.Fatal("active lease allowed a conflicting ordinary beacon")
	}
	nonconflicting := conflictWatcherFixture("safe", "safe-id", "game-1", true)
	permit, ok := w.AcquireObservationPermit(nonconflicting, 0)
	if !ok {
		t.Fatal("Known exact-campaign absence did not permit a causally excluded beacon")
	}
	slots, _ = w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	if len(slots) != 1 {
		t.Fatal("matching lease was lost while its approved ordinary permit remained in flight")
	}
	w.ReleaseObservationPermit(permit)
	if current, ok := w.ProvisionalLease(); !ok || current.LeaseID != lease.LeaseID {
		t.Fatal("unchanged nonconflicting beacon incorrectly revoked the active lease")
	}
}

func TestOrdinaryPermitDriftCannotPromoteActiveLease(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*models.Streamer)
		drift  bool
	}{
		{name: "unchanged facts", drift: false},
		{
			name: "availability observation drift",
			mutate: func(streamer *models.Streamer) {
				streamer.Stream.MarkCampaignAvailabilityUnknown()
			},
			drift: true,
		},
		{
			name: "game drift",
			mutate: func(streamer *models.Streamer) {
				streamer.Stream.Update("broadcast-ordinary", "", &models.Game{ID: "game-2"}, nil, 1)
			},
			drift: true,
		},
		{
			name: "playback session drift",
			mutate: func(streamer *models.Streamer) {
				streamer.Stream.SetSpadeURL("https://spade.invalid/ordinary-new-session")
			},
			drift: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w := &MinuteWatcher{}
			w.SetProvisionalMonitoringEnabled(true)
			streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
			w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
			lease, _ := w.ProvisionalLease()
			baselineAt := lease.ReservedAt.Add(time.Second)
			if !w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 0) {
				t.Fatal("failed to arm lease")
			}

			// Grant sees an authoritative nonconflict, but Sender has not captured
			// its session yet. No Inventory delta may be accepted until Release
			// validates the immutable grant-time fingerprint.
			ordinary := conflictWatcherFixture("ordinary", "ordinary-id", "game-1", true)
			permit, ok := w.AcquireObservationPermit(ordinary, 0)
			if !ok {
				t.Fatal("causally excluded ordinary permit was denied")
			}
			if test.mutate != nil {
				test.mutate(ordinary)
			}
			if w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 1) {
				t.Fatal("server delta was accepted while a lease-bound ordinary permit was unsettled")
			}
			if current, ok := w.ProvisionalLease(); !ok || current.LeaseID != lease.LeaseID {
				t.Fatal("unsettled ordinary permit eagerly removed the lease before release")
			}

			w.ReleaseObservationPermit(permit)
			_, retained := w.ProvisionalLease()
			if retained == test.drift {
				t.Fatalf("lease retained=%v after release, want %v", retained, !test.drift)
			}
			accepted := w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 1)
			if accepted == test.drift {
				t.Fatalf("post-release delta accepted=%v, want %v", accepted, !test.drift)
			}
			if got := len(w.ProvisionalProofs()); got != map[bool]int{false: 1, true: 0}[test.drift] {
				t.Fatalf("proof count=%d after drift=%v", got, test.drift)
			}
		})
	}
}

func TestLeaseQuarantineDefersUntilMatchingPermitDrains(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	lease, _ := w.ProvisionalLease()
	if !w.ArmProvisionalLease(lease.LeaseID, 1, lease.ReservedAt.Add(time.Second), 0) {
		t.Fatal("failed to arm lease")
	}
	permit, ok := w.AcquireObservationPermit(streamer, lease.LeaseID)
	if !ok {
		t.Fatal("failed to acquire matching lease permit")
	}
	if w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("lease was quarantined while its matching permit was in flight")
	}
	if current, ok := w.ProvisionalLease(); !ok || current.LeaseID != lease.LeaseID {
		t.Fatal("deferred quarantine removed active lease")
	}
	w.ReleaseObservationPermit(permit)
	ordinary := conflictWatcherFixture("ordinary", "ordinary-id", "game-1", true)
	permit, ok = w.AcquireObservationPermit(ordinary, 0)
	if !ok {
		t.Fatal("failed to acquire causally excluded ordinary permit")
	}
	if w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("lease was quarantined while its lease-bound ordinary permit was unsettled")
	}
	w.ReleaseObservationPermit(permit)
	if w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("lease was quarantined before a post-drain exact observation")
	}
	if !w.ObserveProvisionalProgress(lease.LeaseID, 2, time.Now().Add(2*time.Second), lease.MaxMinutes) {
		t.Fatal("clean post-drain equal-minutes observation was rejected")
	}
	if !w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("lease quarantine did not succeed after its post-drain observation")
	}
	slots, _ := w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	if len(slots) != 0 {
		t.Fatal("quarantined exact lease tuple remained admissible")
	}
}

func TestQuarantineRequiresObservationCompletedAfterPermitDrain(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	reservedAt := time.Now().Add(-time.Minute)
	w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, reservedAt)
	lease, _ := w.ProvisionalLease()
	baselineAt := reservedAt.Add(time.Second)
	if !w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 0) {
		t.Fatal("failed to arm lease")
	}
	permit, ok := w.AcquireObservationPermit(streamer, lease.LeaseID)
	if !ok {
		t.Fatal("failed to acquire exact lease permit")
	}
	if w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("lease quarantined while exact permit was active")
	}
	preDrainAt := time.Now()
	w.ReleaseObservationPermit(permit)

	// The run was not consumed by health until after Release, but its timestamp
	// proves it completed while the beacon was still in flight.
	if !w.ObserveProvisionalProgress(lease.LeaseID, 2, preDrainAt, 0) {
		t.Fatal("clean pre-drain observation was not accepted as monotone state")
	}
	if w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("pre-drain observation was misclassified as post-drain negative evidence")
	}
	if !w.ObserveProvisionalProgress(lease.LeaseID, 3, time.Now().Add(time.Millisecond), 0) {
		t.Fatal("clean post-drain observation was rejected")
	}
	if !w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("post-drain equal-minutes observation did not satisfy quarantine fence")
	}
}

func TestQuarantineFirstRequestFencesAlreadyDrainedTransport(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	reservedAt := time.Now().Add(-time.Minute)
	w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, reservedAt)
	lease, _ := w.ProvisionalLease()
	baselineAt := reservedAt.Add(time.Second)
	if !w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 0) {
		t.Fatal("failed to arm lease")
	}
	permit, ok := w.AcquireObservationPermit(streamer, lease.LeaseID)
	if !ok {
		t.Fatal("failed to acquire exact lease permit")
	}
	// The send is already drained by the time health first makes its terminal
	// decision. That decision must still wait for a clean post-decision read.
	w.ReleaseObservationPermit(permit)
	if w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("first terminal request quarantined using pre-fence evidence")
	}
	fenceAt := w.quarantineFenceDrainAt
	if fenceAt.IsZero() {
		t.Fatal("first terminal request did not publish a drain fence")
	}
	if _, ok := w.AcquireObservationPermit(streamer, lease.LeaseID); ok {
		t.Fatal("terminal fence allowed a new matching lease probe")
	}
	if !w.ObserveProvisionalProgress(lease.LeaseID, 2, fenceAt, 0) {
		t.Fatal("equal-to-fence observation was not accepted as monotone broker state")
	}
	if w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("observation completed exactly at the drain fence authorized quarantine")
	}
	if !w.ObserveProvisionalProgress(lease.LeaseID, 3, fenceAt.Add(time.Millisecond), 0) {
		t.Fatal("strictly post-fence observation was rejected")
	}
	if !w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("strictly post-fence equal-minutes evidence did not authorize quarantine")
	}
}

func TestQuarantineFenceBlocksPendingBootstrapToken(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	reservedAt := time.Now().Add(-time.Minute)
	w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, reservedAt)
	lease, _ := w.ProvisionalLease()
	if !w.ObserveProvisionalAbsence(lease.LeaseID, 1, reservedAt.Add(time.Second)) {
		t.Fatal("failed to open initial Pending bootstrap token")
	}
	if w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("first Pending terminal request skipped its freshness fence")
	}
	if _, ok := w.acquireProvisionalBootstrapPermit(streamer, lease.LeaseID); ok {
		t.Fatal("terminal fence allowed a matching Pending bootstrap send")
	}
	if _, ok := w.AcquireObservationPermit(streamer, lease.LeaseID); ok {
		t.Fatal("terminal fence allowed a matching Pending recovery probe")
	}
	fenceAt := w.quarantineFenceDrainAt
	if !w.ObserveProvisionalAbsence(lease.LeaseID, 2, fenceAt.Add(time.Millisecond)) {
		t.Fatal("post-fence complete absence was rejected")
	}
	if _, ok := w.acquireProvisionalBootstrapPermit(streamer, lease.LeaseID); ok {
		t.Fatal("post-fence observation reopened transport during terminal decision")
	}
	if !w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("post-fence Pending absence did not authorize quarantine")
	}
}

func TestPostPermitPositiveDeltaProvesInsteadOfQuarantining(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	lease, _ := w.ProvisionalLease()
	baselineAt := lease.ReservedAt.Add(time.Second)
	if !w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 0) {
		t.Fatal("failed to arm lease")
	}
	permit, ok := w.AcquireObservationPermit(streamer, lease.LeaseID)
	if !ok {
		t.Fatal("failed to acquire exact lease permit")
	}
	if w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("lease quarantined with its exact beacon in flight")
	}
	w.ReleaseObservationPermit(permit)
	if !w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 1) {
		t.Fatal("post-drain positive exact delta was rejected")
	}
	if w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("server-proven lease was converted into a terminal negative")
	}
	proofs := w.ProvisionalProofs()
	if len(proofs) != 1 || proofs[0].ProvenMinutes != 1 {
		t.Fatalf("positive post-drain delta did not remain authoritative: %+v", proofs)
	}
}

func TestStopPreservesInflightPermitAcrossReenable(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	conflicting := conflictWatcherFixture("other", "other-id", "game-1", false)
	permit, ok := w.AcquireObservationPermit(conflicting, 0)
	if !ok {
		t.Fatal("failed to acquire pre-stop ordinary permit")
	}
	// The outbound transport may have captured the old same-game UNKNOWN
	// session even if the mutable Stream changes before lease admission.
	conflicting.Stream.Update("other-broadcast", "", &models.Game{ID: "other-game"}, nil, 1)
	conflicting.Stream.SetCampaignIDs(nil)
	conflicting.Stream.SetSpadeURL("https://spade.invalid/other-session")
	_ = w.Stop()
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	slots, _ := w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	if len(slots) != 0 {
		t.Fatal("reenabled watcher forgot a conflicting pre-stop in-flight beacon")
	}
	w.ReleaseObservationPermit(permit)
	slots, _ = w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	if len(slots) != 1 {
		t.Fatal("lease was not admitted after preserved permit drained")
	}
}

func TestProvisionalProcessPendingSendACKDoesNotProve(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 5)}
	w, _ := newLoopWatcher(0, sender, nil)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	w.AddSource(&staticSource{name: OriginDiscovery, cand: []Candidate{{
		Streamer: streamer, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
	}}})
	w.SetProvisionalMonitoringEnabled(true)
	w.ctx = context.Background()
	hookCalls := 0
	w.SetOnMinuteWatched(func() { hookCalls++ })

	w.processWatching(tickCtx(w))
	if got := len(sender.sent); got != 0 {
		t.Fatalf("new Pending lease sent %d beacon(s) before complete Inventory authority", got)
	}
	lease, ok := w.ProvisionalLease()
	if !ok || lease.State != ProvisionalLeasePending || len(w.ProvisionalProofs()) != 0 {
		t.Fatal("process tick did not publish a clean Pending lease")
	}
	if _, ok := w.ReportStats(streamer.GetUsername()); ok || hookCalls != 0 {
		t.Fatalf("suppressed Pending send fabricated delivery accounting: ok=%v hookCalls=%d", ok, hookCalls)
	}

	absentAt := lease.ReservedAt.Add(time.Second)
	if !w.ObserveProvisionalAbsence(lease.LeaseID, 1, absentAt) {
		t.Fatal("failed to open one Pending bootstrap send")
	}
	w.processWatching(tickCtx(w))
	if got := len(sender.sent); got != 1 {
		t.Fatalf("fresh absence produced %d total beacon(s), want 1", got)
	}
	lease, _ = w.ProvisionalLease()
	if lease.State != ProvisionalLeasePending || lease.PendingObservation != ProvisionalPendingObservationAbsence ||
		len(w.ProvisionalProofs()) != 0 {
		t.Fatal("Pending beacon ACK armed or proved the lease")
	}
	stats, ok := w.ReportStats(streamer.GetUsername())
	if !ok || stats.Successes != 1 || stats.Failures != 0 || hookCalls != 1 {
		t.Fatalf("Pending delivery accounting mismatch: stats=%+v ok=%v hookCalls=%d", stats, ok, hookCalls)
	}
	w.processWatching(tickCtx(w))
	if got := len(sender.sent); got != 1 {
		t.Fatalf("consumed absence token produced %d total beacon(s), want 1", got)
	}
	stats, _ = w.ReportStats(streamer.GetUsername())
	if stats.Successes != 1 || hookCalls != 1 {
		t.Fatalf("suppressed repeat mutated delivery accounting: stats=%+v hookCalls=%d", stats, hookCalls)
	}

	unknownAt := absentAt.Add(time.Second)
	if !w.ObserveProvisionalTupleUnknown(lease.LeaseID, 2, unknownAt) {
		t.Fatal("failed to open one tuple-UNKNOWN bootstrap send")
	}
	w.processWatching(tickCtx(w))
	if got := len(sender.sent); got != 2 {
		t.Fatalf("tuple UNKNOWN produced %d total beacon(s), want 2", got)
	}
	lease, _ = w.ProvisionalLease()
	if lease.State != ProvisionalLeasePending || lease.PendingObservation != ProvisionalPendingObservationTupleUnknown ||
		len(w.ProvisionalProofs()) != 0 {
		t.Fatal("tuple-UNKNOWN ACK armed or proved the lease")
	}

	baselineAt := unknownAt.Add(time.Second)
	if !w.ArmProvisionalLease(lease.LeaseID, 3, baselineAt, 0) {
		t.Fatal("failed to arm process lease")
	}
	w.processWatching(tickCtx(w))
	if got := len(sender.sent); got != 3 {
		t.Fatalf("observing lease sent total %d beacon(s), want 3", got)
	}
	lease, _ = w.ProvisionalLease()
	if lease.State != ProvisionalLeaseObserving {
		t.Fatalf("delivery ACK promoted lease without server progress: %s", lease.State)
	}
	stats, ok = w.ReportStats(streamer.GetUsername())
	if !ok || stats.Successes != 3 || stats.Failures != 0 || hookCalls != 3 {
		t.Fatalf("honest delivery accounting mismatch: stats=%+v ok=%v hookCalls=%d", stats, ok, hookCalls)
	}

	// A fresh exact server delta promotes the candidate on the next broker tick.
	// That tick must use the proof-bound permit path and actually
	// send; retaining provisional metadata must not suppress it after the lease
	// is consumed.
	if !w.ObserveProvisionalProgress(lease.LeaseID, 4, lease.MaxAt.Add(time.Second), 1) {
		t.Fatal("failed to prove process lease from exact server delta")
	}
	w.processWatching(tickCtx(w))
	if got := len(sender.sent); got != 4 {
		t.Fatalf("promoted proof sent total %d beacon(s), want 4", got)
	}
	if _, ok := w.ProvisionalLease(); ok {
		t.Fatal("promoted process slot retained its observation lease")
	}
	if got := len(w.ProvisionalProofs()); got != 1 {
		t.Fatalf("promoted process proof count=%d, want 1", got)
	}
	snapshot := w.BrokerSnapshot()
	if len(snapshot.Slots) != 1 || snapshot.Slots[0].ReasonCode != ReasonActiveDrop ||
		!snapshot.Slots[0].provisionalProven {
		t.Fatalf("promoted process slot not published as active proof: %+v", snapshot.Slots)
	}
	stats, ok = w.ReportStats(streamer.GetUsername())
	if !ok || stats.Successes != 4 || stats.Failures != 0 || hookCalls != 4 {
		t.Fatalf("promoted delivery accounting mismatch: stats=%+v ok=%v hookCalls=%d", stats, ok, hookCalls)
	}
}

func TestProvisionalQuarantineIsExactSessionFenced(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	slot := provisionalSlot(streamer, candidate)
	slots, _ := w.reconcileProvisionalSlots([]slotOccupant{slot}, nil, time.Now())
	if len(slots) != 1 {
		t.Fatal("initial exact tuple was not admitted")
	}
	lease, _ := w.ProvisionalLease()
	if w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("first terminal request skipped its post-drain freshness fence")
	}
	if !w.ObserveProvisionalAbsence(lease.LeaseID, 1, w.quarantineFenceDrainAt.Add(time.Millisecond)) {
		t.Fatal("fresh exact absence after the terminal fence was rejected")
	}
	if !w.QuarantineProvisionalLease(lease.LeaseID, candidate) {
		t.Fatal("matching exact tuple was not quarantined after post-fence evidence")
	}
	slots, _ = w.reconcileProvisionalSlots([]slotOccupant{slot}, nil, time.Now())
	if len(slots) != 0 {
		t.Fatal("same exact quarantined tuple was immediately reconsidered")
	}

	streamer.Stream.SetSpadeURL("https://spade.invalid/new-session")
	snapshot := streamer.Stream.ProvisionalDropSnapshot()
	newSession := candidate
	newSession.SessionGeneration = snapshot.SessionGeneration
	newSession.BroadcastID = snapshot.BroadcastID
	slots, _ = w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, newSession)}, nil, time.Now())
	if len(slots) != 1 {
		t.Fatal("new playback-session generation was incorrectly quarantined")
	}
	newLease, ok := w.ProvisionalLease()
	if !ok || newLease.LeaseID == lease.LeaseID || !newLease.Candidate.SameLeaseIdentity(newSession) {
		t.Fatalf("new session did not receive a fresh lease: %+v", newLease)
	}
}

func TestProvisionalProofSurvivesRoutineEnvelopeRefreshButKnownEpochVetoes(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	lease, _ := w.ProvisionalLease()
	baselineAt := lease.ReservedAt.Add(time.Second)
	w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 0)
	w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 1)

	// Routine UNKNOWN and Directory observation refreshes do not revoke the
	// positive exact server delta. The cache adopts the newest source envelope.
	streamer.Stream.MarkCampaignAvailabilityUnknown()
	refreshed := candidate
	refreshSnapshot := streamer.Stream.ProvisionalDropSnapshot()
	refreshed.AvailabilityObs = refreshSnapshot.Availability.ObservationID
	refreshed.AvailabilityKnownGen = refreshSnapshot.Availability.KnownGeneration
	refreshed.DirectoryObs++
	slots, _ := w.arbitrate(nil, []Candidate{{
		Streamer: streamer, Origin: OriginDiscovery, ProvisionalDrop: &refreshed,
	}}, time.Now())
	if len(slots) != 1 || !slots[0].provisionalProven {
		t.Fatal("routine UNKNOWN/Directory refresh revoked a session-fenced proof")
	}
	proofs := w.ProvisionalProofs()
	if len(proofs) != 1 || !proofs[0].Candidate.SameLeaseIdentity(refreshed) {
		t.Fatalf("proof cache did not adopt refreshed source envelope: %+v", proofs)
	}

	// Any authoritative Known publication advances the proof epoch. A distinct,
	// later UNKNOWN may be observed provisionally again because retained Known
	// IDs are non-authoritative in UNKNOWN state, but it must start from Pending
	// with a new post-reservation baseline; the old proof cannot resurrect.
	streamer.Stream.SetCampaignIDs(nil)
	streamer.Stream.MarkCampaignAvailabilityUnknown()
	afterKnown := refreshed
	afterKnownSnapshot := streamer.Stream.ProvisionalDropSnapshot()
	afterKnown.AvailabilityObs = afterKnownSnapshot.Availability.ObservationID
	afterKnown.AvailabilityKnownGen = afterKnownSnapshot.Availability.KnownGeneration
	afterKnown.DirectoryObs++
	slots, _ = w.arbitrate(nil, []Candidate{{
		Streamer: streamer, Origin: OriginDiscovery, ProvisionalDrop: &afterKnown,
	}}, time.Now())
	if len(slots) != 1 || slots[0].provisionalProven {
		t.Fatal("old proof crossed a Known-empty authority epoch")
	}
	if len(w.ProvisionalProofs()) != 0 {
		t.Fatal("old proof remained cached after Known-empty -> UNKNOWN")
	}
	slots, _ = w.reconcileProvisionalSlots(slots, nil, time.Now())
	newLease, ok := w.ProvisionalLease()
	if !ok || newLease.LeaseID == lease.LeaseID || newLease.State != ProvisionalLeasePending {
		t.Fatalf("newer UNKNOWN did not require a fresh Pending lease: %+v", newLease)
	}

	// A concurrent authoritative assignment vetoes even fresh provisional
	// admission at the final boundary.
	streamer.Stream.SetCampaigns([]*models.Campaign{{ID: candidate.CampaignID}})
	slots, _ = w.reconcileProvisionalSlots(slots, nil, time.Now())
	if len(slots) != 0 {
		t.Fatal("provisional slot survived a concurrent authoritative assignment")
	}
}

func TestProvisionalProofIsBoundToPrivateStreamerOwner(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	owner, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now())
	lease, _ := w.ProvisionalLease()
	if resolved, ok := w.ProvisionalOwner(lease.LeaseID, candidate); !ok || resolved != owner {
		t.Fatal("exact lease did not resolve to its private Streamer owner")
	}
	baselineAt := lease.ReservedAt.Add(time.Second)
	w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 0)
	w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 1)
	proof := w.ProvisionalProofs()[0]

	clone, cloneCandidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	if !cloneCandidate.SameLeaseIdentity(candidate) {
		t.Fatalf("test clone did not reproduce visible scalar identity: %+v vs %+v", cloneCandidate, candidate)
	}
	if !w.HasProvisionalProof(owner, candidate) || w.HasProvisionalProof(clone, cloneCandidate) {
		t.Fatal("proof query did not enforce private Streamer owner identity")
	}
	if !w.OwnsProvisionalObservation(owner) || w.OwnsProvisionalObservation(clone) {
		t.Fatal("observation ownership query did not enforce private Streamer identity")
	}
	if _, ok := w.AcquireProvisionalProofPermit(clone, proof.ProofID, cloneCandidate); ok {
		t.Fatal("recreated Streamer inherited a scalar-identical proof permit")
	}
	if resolved, ok := w.ProvisionalOwner(proof.ProofID, cloneCandidate); !ok || resolved != owner || resolved == clone {
		t.Fatal("proof owner lookup replayed authority onto a scalar-identical replacement")
	}
	permit, ok := w.AcquireProvisionalProofPermit(owner, proof.ProofID, candidate)
	if !ok {
		t.Fatal("exact proof owner was denied its proof-bound permit")
	}
	w.ReleaseObservationPermit(permit)
	if _, ok := w.ProvisionalLease(); ok || len(w.ProvisionalProofs()) != 1 {
		t.Fatal("proof permit did not complete the Proven lease handoff")
	}
	if w.OwnsProvisionalObservation(owner) {
		t.Fatal("standalone proof suppressed the routine freshness recheck indefinitely")
	}
	proved := w.provenProvisionalCandidates([]Candidate{{
		Streamer: clone, Origin: OriginDiscovery, ProvisionalDrop: &cloneCandidate,
	}})
	if len(proved) != 0 || len(w.ProvisionalProofs()) != 0 {
		t.Fatal("recreated Streamer retained the prior object's proof")
	}
	if resolved, ok := w.ProvisionalOwner(proof.ProofID, cloneCandidate); ok || resolved != nil {
		t.Fatal("pruned proof continued resolving a private owner")
	}
}

func TestProvisionalProofPermitRejectsSessionFlip(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	lease, _ := w.ProvisionalLease()
	baselineAt := lease.ReservedAt.Add(time.Second)
	w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 0)
	w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 1)
	proof := w.ProvisionalProofs()[0]

	streamer.Stream.SetSpadeURL("https://spade.invalid/changed-before-probe")
	if _, ok := w.AcquireProvisionalProofPermit(streamer, proof.ProofID, candidate); ok {
		t.Fatal("session-drifted proof was permitted to send or probe")
	}
	if owner, ok := w.ProvisionalOwner(proof.ProofID, candidate); ok || owner != nil {
		t.Fatal("session-drifted proof still resolved its private owner")
	}
	if w.OwnsProvisionalObservation(streamer) {
		t.Fatal("session-drifted proof still suppressed routine source refresh")
	}
}

func TestInvalidatedPromotedProofRestoresDisplacedConfiguredVictim(t *testing.T) {
	w, _ := newTestWatcher(2)
	w.selectionReasons = map[int]string{0: "configured-a", 1: "configured-b"}
	for i, configured := range w.streamers {
		configured.ChannelID = "configured-id-" + string(rune('a'+i))
		configured.Stream.Update("configured-broadcast", "", &models.Game{ID: "other-game"}, nil, 1)
		configured.Stream.SetCampaignIDs(nil)
	}
	w.SetProvisionalMonitoringEnabled(true)
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
	w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
	lease, _ := w.ProvisionalLease()
	baselineAt := lease.ReservedAt.Add(time.Second)
	w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 0)
	w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 1)

	slots, waiting := w.arbitrate([]int{0, 1}, []Candidate{{
		Streamer: streamer, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
	}}, time.Now())
	if len(slots) != 2 || slots[0].provisionalFallback == nil && slots[1].provisionalFallback == nil {
		t.Fatal("promoted proof did not retain its displaced configured fallback")
	}
	streamer.Stream.SetSpadeURL("https://spade.invalid/session-flipped-after-arbitration")
	slots, waiting = w.reconcileProvisionalSlots(slots, waiting, time.Now())
	if len(slots) != 2 {
		t.Fatalf("invalidated promotion left an avoidable slot hole: %d", len(slots))
	}
	seen := make(map[string]bool)
	for _, slot := range slots {
		seen[slot.streamer.GetUsername()] = true
		if slot.streamer == streamer {
			t.Fatal("invalidated promoted proof survived final reconciliation")
		}
	}
	if !seen[w.streamers[0].GetUsername()] || !seen[w.streamers[1].GetUsername()] {
		t.Fatalf("displaced configured victim was not restored: %+v", seen)
	}
	for _, item := range waiting {
		if seen[item.Channel] {
			t.Fatalf("restored configured occupant remained in waiting snapshot: %+v", item)
		}
	}
}

func TestPromotedProofFallbackRollsBackOnlyItsOwnColdStartParityAdvance(t *testing.T) {
	tests := []struct {
		name        string
		firstValid  bool
		secondValid bool
		wantParity  uint64
	}{
		{name: "both valid", firstValid: true, secondValid: true, wantParity: 11},
		{name: "first valid second invalid", firstValid: true, secondValid: false, wantParity: 11},
		{name: "first invalid second valid", firstValid: false, secondValid: true, wantParity: 10},
		{name: "both invalid", firstValid: false, secondValid: false, wantParity: 10},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			w, _ := newTestWatcher(2)
			w.selectionReasons = map[int]string{0: "configured-a", 1: "configured-b"}
			for i, configured := range w.streamers {
				configured.Settings.WatchStreak = false
				configured.ChannelID = "configured-id-" + string(rune('a'+i))
				configured.Stream.Update("configured-broadcast", "", &models.Game{ID: "other-game"}, nil, 1)
				configured.Stream.SetCampaignIDs(nil)
			}
			w.SetProvisionalMonitoringEnabled(true)

			firstOwner, first := provisionalWatcherFixture(t, "first-proof", "first-channel", "game-1")
			secondOwner, second := provisionalWatcherFixture(t, "second-proof", "second-channel", "game-2")
			second.CampaignID = "campaign-2"
			second.Campaign = "Campaign Two"
			second.DropID = "drop-2"
			second.Drop = "Drop Two"

			prove := func(owner *models.Streamer, candidate models.ProvisionalDropCandidate, run uint64) {
				t.Helper()
				slots, _ := w.reconcileProvisionalSlots(
					[]slotOccupant{provisionalSlot(owner, candidate)}, nil, time.Now(),
				)
				if len(slots) != 1 {
					t.Fatalf("failed to reserve proof candidate %s", candidate.Login)
				}
				lease, ok := w.ProvisionalLease()
				if !ok {
					t.Fatalf("missing lease for proof candidate %s", candidate.Login)
				}
				baselineAt := lease.ReservedAt.Add(time.Second)
				if !w.ArmProvisionalLease(lease.LeaseID, run, baselineAt, 0) ||
					!w.ObserveProvisionalProgress(lease.LeaseID, run+1, baselineAt.Add(time.Second), 1) {
					t.Fatalf("failed to prove candidate %s", candidate.Login)
				}
			}
			prove(firstOwner, first, 1)
			prove(secondOwner, second, 3)

			w.displaceParity = 10
			slots, waiting := w.arbitrate([]int{0, 1}, []Candidate{
				{Streamer: firstOwner, Origin: OriginDiscovery, ProvisionalDrop: &first},
				{Streamer: secondOwner, Origin: OriginDiscovery, ProvisionalDrop: &second},
			}, time.Now())
			if len(slots) != 2 || w.displaceParity != 11 {
				t.Fatalf("two proof displacements produced slots=%d parity=%d, want 2/11", len(slots), w.displaceParity)
			}

			fallbacks := make(map[*models.Streamer]*models.Streamer, 2)
			parityChanged := make(map[*models.Streamer]bool, 2)
			for _, slot := range slots {
				if slot.provisionalFallback == nil {
					t.Fatalf("promoted proof %s had no configured fallback", slot.streamer.GetUsername())
				}
				fallbacks[slot.streamer] = slot.provisionalFallback.streamer
				parityChanged[slot.streamer] = slot.provisionalFallbackParityChanged
			}
			if !parityChanged[firstOwner] || parityChanged[secondOwner] {
				t.Fatalf("parity ownership first=%v second=%v, want true/false", parityChanged[firstOwner], parityChanged[secondOwner])
			}

			if !test.firstValid {
				firstOwner.Stream.SetSpadeURL("https://spade.invalid/first-invalidated")
			}
			if !test.secondValid {
				secondOwner.Stream.SetSpadeURL("https://spade.invalid/second-invalidated")
			}
			slots, _ = w.reconcileProvisionalSlots(slots, waiting, time.Now())
			if len(slots) != 2 {
				t.Fatalf("final slot count=%d, want 2", len(slots))
			}
			want := map[*models.Streamer]bool{
				firstOwner:  test.firstValid,
				secondOwner: test.secondValid,
			}
			if !test.firstValid {
				delete(want, firstOwner)
				want[fallbacks[firstOwner]] = true
			}
			if !test.secondValid {
				delete(want, secondOwner)
				want[fallbacks[secondOwner]] = true
			}
			for _, slot := range slots {
				if !want[slot.streamer] {
					t.Fatalf("unexpected final occupant %s", slot.streamer.GetUsername())
				}
				delete(want, slot.streamer)
			}
			if len(want) != 0 {
				t.Fatalf("missing final occupants: %+v", want)
			}
			if w.displaceParity != test.wantParity {
				t.Fatalf("final parity=%d, want %d", w.displaceParity, test.wantParity)
			}
		})
	}
}

func TestIndependentProvisionalProofsArePreserved(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)
	firstStreamer, first := provisionalWatcherFixture(t, "first", "first-id", "game-1")
	secondStreamer, second := provisionalWatcherFixture(t, "second", "second-id", "game-2")
	second.CampaignID = "campaign-2"
	second.Campaign = "Campaign Two"
	second.DropID = "drop-2"
	second.Drop = "Drop Two"

	prove := func(streamer *models.Streamer, candidate models.ProvisionalDropCandidate, minutes int) {
		t.Helper()
		slots, _ := w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
		if len(slots) != 1 {
			t.Fatalf("failed to reserve %s/%s", candidate.CampaignID, candidate.DropID)
		}
		lease, _ := w.ProvisionalLease()
		baselineAt := lease.ReservedAt.Add(time.Second)
		if !w.ArmProvisionalLease(lease.LeaseID, uint64(minutes*2+1), baselineAt, minutes) ||
			!w.ObserveProvisionalProgress(lease.LeaseID, uint64(minutes*2+2), baselineAt.Add(time.Second), minutes+1) {
			t.Fatalf("failed to prove %s/%s", candidate.CampaignID, candidate.DropID)
		}
	}

	prove(firstStreamer, first, 1)
	prove(secondStreamer, second, 10)
	proofs := w.ProvisionalProofs()
	if len(proofs) != 2 {
		t.Fatalf("independent proof count=%d, want 2: %+v", len(proofs), proofs)
	}
	seen := make(map[string]bool)
	for _, proof := range proofs {
		seen[proof.Candidate.CampaignID+"\x00"+proof.Candidate.DropID] = true
	}
	if !seen["campaign-1\x00drop-1"] || !seen["campaign-2\x00drop-2"] {
		t.Fatalf("independent proofs were overwritten: %+v", proofs)
	}
	for i := 0; i < 20; i++ {
		repeated := w.ProvisionalProofs()
		if repeated[0].Candidate.CampaignID != "campaign-1" || repeated[1].Candidate.CampaignID != "campaign-2" {
			t.Fatalf("proof traversal order was unstable: %+v", repeated)
		}
	}
}

func TestProvisionalProofCacheDefensivelyRejectsThirdAuthority(t *testing.T) {
	w := &MinuteWatcher{}
	w.SetProvisionalMonitoringEnabled(true)

	prove := func(index int) {
		t.Helper()
		login := "candidate-" + string(rune('a'+index))
		streamer, candidate := provisionalWatcherFixture(t, login, "channel-"+login, "game-"+string(rune('1'+index)))
		candidate.CampaignID = "campaign-" + string(rune('1'+index))
		candidate.DropID = "drop-" + string(rune('1'+index))
		slots, _ := w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
		if len(slots) != 1 {
			t.Fatalf("proof %d was not admitted", index+1)
		}
		lease, _ := w.ProvisionalLease()
		baselineAt := lease.ReservedAt.Add(time.Second)
		if !w.ArmProvisionalLease(lease.LeaseID, uint64(index*2+1), baselineAt, 0) ||
			!w.ObserveProvisionalProgress(lease.LeaseID, uint64(index*2+2), baselineAt.Add(time.Second), 1) {
			t.Fatalf("proof %d was not recorded", index+1)
		}
	}

	prove(0)
	prove(1)
	third, candidate := provisionalWatcherFixture(t, "candidate-c", "channel-candidate-c", "game-3")
	candidate.CampaignID = "campaign-3"
	candidate.DropID = "drop-3"
	slots, _ := w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(third, candidate)}, nil, time.Now())
	if len(slots) != 0 {
		t.Fatal("a third unproved lease was admitted against a full proof cache")
	}
	if _, ok := w.ProvisionalLease(); ok {
		t.Fatal("third candidate reserved a lease despite the proof-cache cap")
	}
	if got := len(w.ProvisionalProofs()); got != constants.MaxSimultaneousStreams {
		t.Fatalf("proof cache grew to %d, want hard cap %d", got, constants.MaxSimultaneousStreams)
	}
}

func TestProofEnvelopeAdoptionIsIndependentOfProposalOrder(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "refreshed-first", true: "original-first"}[reverse], func(t *testing.T) {
			w := &MinuteWatcher{}
			w.SetProvisionalMonitoringEnabled(true)
			streamer, candidate := provisionalWatcherFixture(t, "candidate", "candidate-id", "game-1")
			w.reconcileProvisionalSlots([]slotOccupant{provisionalSlot(streamer, candidate)}, nil, time.Now())
			lease, _ := w.ProvisionalLease()
			baselineAt := lease.ReservedAt.Add(time.Second)
			w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 0)
			w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 1)

			refreshed := candidate
			refreshed.DirectoryObs++
			if !w.HasProvisionalProof(streamer, refreshed) {
				t.Fatal("read-only proof query rejected a routine refreshed envelope")
			}
			before := w.ProvisionalProofs()
			if len(before) != 1 || !before[0].Candidate.SameLeaseIdentity(candidate) {
				t.Fatal("proof query adopted an envelope before final broker proposal fencing")
			}
			proposals := []Candidate{
				{Streamer: streamer, Origin: OriginDiscovery, ProvisionalDrop: &refreshed},
				{Streamer: streamer, Origin: OriginDiscovery, ProvisionalDrop: &candidate},
			}
			if reverse {
				proposals[0], proposals[1] = proposals[1], proposals[0]
			}
			proved := w.provenProvisionalCandidates(proposals)
			_, original := findProvisionalProofIdentity(proved, streamer, &candidate)
			_, newest := findProvisionalProofIdentity(proved, streamer, &refreshed)
			if original || !newest {
				t.Fatalf("newest proof envelope selection depended on proposal order: %+v", proved)
			}
			proofs := w.ProvisionalProofs()
			if len(proofs) != 1 || !proofs[0].Candidate.SameLeaseIdentity(refreshed) {
				t.Fatal("valid proof did not adopt the newest routine-refresh envelope")
			}
			if proved := w.provenProvisionalCandidates(nil); len(proved) != 0 || len(w.ProvisionalProofs()) != 0 {
				t.Fatal("proof survived omission from the final gathered proposal set")
			}
		})
	}
}

func TestBrokerSnapshotDoesNotSerializeProvisionalAuthority(t *testing.T) {
	w := &MinuteWatcher{}
	streamer, candidate := provisionalWatcherFixture(t, "candidate", "secret-channel-id", "game-1")
	slot := provisionalSlot(streamer, candidate)
	w.publishBrokerSnapshot([]slotOccupant{slot}, nil, time.Now())

	encoded, err := json.Marshal(w.BrokerSnapshot())
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	for _, secret := range []string{"secret-channel-id", "broadcast-candidate", "campaign-1", "drop-1"} {
		if stringContains(string(encoded), secret) {
			t.Fatalf("public broker snapshot leaked provisional authority %q: %s", secret, encoded)
		}
	}

	first := w.BrokerSnapshot()
	first.Slots[0].provisionalDrop.RestrictedACL = append(first.Slots[0].provisionalDrop.RestrictedACL, "mutated")
	second := w.BrokerSnapshot()
	if len(second.Slots[0].provisionalDrop.RestrictedACL) != len(candidate.RestrictedACL) {
		t.Fatal("BrokerSnapshot returned shared provisional ACL memory")
	}
}

func stringContains(value, fragment string) bool {
	for i := 0; i+len(fragment) <= len(value); i++ {
		if value[i:i+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
