package models

import "testing"

// 1: a freshly created streamer's capability is Unknown (zero value).
func TestCapabilityStartsUnknown(t *testing.T) {
	s := NewStreamer("bob", DefaultStreamerSettings())
	if s.GetChannelPointsCapability() != CapabilityUnknown {
		t.Fatalf("new streamer capability = %v, want unknown", s.GetChannelPointsCapability())
	}
	if s.LastConfirmedChannelPointsCapability() != CapabilityUnknown {
		t.Fatalf("new streamer last-confirmed = %v, want unknown", s.LastConfirmedChannelPointsCapability())
	}
}

// 2, 3, 9, 10: confirmations and transitions.
func TestCapabilityTransitions(t *testing.T) {
	s := NewStreamer("bob", DefaultStreamerSettings())

	if !s.SetChannelPointsCapability(CapabilityEnabled, CapReasonConfirmedContext) {
		t.Fatal("unknown->enabled should report a change")
	}
	if s.GetChannelPointsCapability() != CapabilityEnabled {
		t.Fatal("expected enabled")
	}

	// 10: enabled -> disabled confirmation.
	s.SetChannelPointsCapability(CapabilityDisabled, CapReasonConfirmedDisabled)
	if s.GetChannelPointsCapability() != CapabilityDisabled {
		t.Fatal("expected disabled")
	}
	if s.LastConfirmedChannelPointsCapability() != CapabilityDisabled {
		t.Fatal("last confirmed should follow the confirmation")
	}

	// 9: unknown -> enabled recovery.
	s.SetChannelPointsCapability(CapabilityUnknown, CapReasonTransportError)
	s.SetChannelPointsCapability(CapabilityEnabled, CapReasonConfirmedContext)
	if s.GetChannelPointsCapability() != CapabilityEnabled {
		t.Fatal("expected recovery to enabled")
	}
}

// 8: enabled -> unknown preserves last-confirmed and never touches the balance.
func TestCapabilityUnknownPreservesLastConfirmedAndBalance(t *testing.T) {
	s := NewStreamer("bob", DefaultStreamerSettings())
	s.SetChannelPoints(1234)
	s.SetChannelPointsCapability(CapabilityEnabled, CapReasonConfirmedContext)

	changed := s.SetChannelPointsCapability(CapabilityUnknown, CapReasonTimeout)
	if !changed {
		t.Fatal("enabled->unknown is a change")
	}
	if s.GetChannelPointsCapability() != CapabilityUnknown {
		t.Fatal("expected unknown")
	}
	if s.LastConfirmedChannelPointsCapability() != CapabilityEnabled {
		t.Fatal("unknown must preserve the last confirmed capability")
	}
	if s.GetChannelPoints() != 1234 {
		t.Fatalf("unknown must not clear the balance, got %d", s.GetChannelPoints())
	}
}

// Stale guard: an inconclusive (unknown) observation captured before a newer
// confirmation must not overwrite it. A confirmed transition bumps the sequence.
func TestCapabilityStaleGuard(t *testing.T) {
	s := NewStreamer("bob", DefaultStreamerSettings())

	_, seq := s.ChannelPointsCapabilitySnapshot()
	// A newer confirmation lands first.
	s.SetChannelPointsCapability(CapabilityEnabled, CapReasonConfirmedContext)

	// The stale unknown, using the OLD sequence, must be dropped.
	if s.ApplyChannelPointsCapabilityIfCurrent(seq, CapabilityUnknown, CapReasonTimeout) {
		t.Fatal("stale unknown must not apply over a newer confirmation")
	}
	if s.GetChannelPointsCapability() != CapabilityEnabled {
		t.Fatalf("capability should stay enabled, got %v", s.GetChannelPointsCapability())
	}
}

// B7: ApplyChannelPointsContextIfCurrent applies capability + balance atomically
// under the stale guard.
func TestApplyChannelPointsContextIfCurrentAtomic(t *testing.T) {
	s := NewStreamer("bob", DefaultStreamerSettings())
	s.SetChannelPoints(50)

	_, seq := s.ChannelPointsCapabilitySnapshot()
	tr := s.ApplyChannelPointsContextIfCurrent(seq, CapabilityEnabled, CapReasonConfirmedContext, 200, true)
	if !tr.Applied || tr.Stale {
		t.Fatalf("current observation should apply: %+v", tr)
	}
	if s.GetChannelPointsCapability() != CapabilityEnabled || s.GetChannelPoints() != 200 {
		t.Fatalf("capability+balance not applied atomically: cap=%v bal=%d", s.GetChannelPointsCapability(), s.GetChannelPoints())
	}

	// The captured seq is now stale (a confirmation bumped it). A stale apply is
	// dropped whole — neither capability nor balance changes.
	tr2 := s.ApplyChannelPointsContextIfCurrent(seq, CapabilityUnknown, CapReasonTimeout, 999, true)
	if !tr2.Stale || tr2.Applied {
		t.Fatalf("stale observation should be dropped: %+v", tr2)
	}
	if s.GetChannelPointsCapability() != CapabilityEnabled || s.GetChannelPoints() != 200 {
		t.Fatalf("stale apply must not change state: cap=%v bal=%d", s.GetChannelPointsCapability(), s.GetChannelPoints())
	}
}

// B7 scenarios 1-8: a newer Enabled+balance wins; an older result (Unknown, or a
// stale Enabled with an old balance) that completes afterward is dropped.
func TestApplyChannelPointsContextOrdering(t *testing.T) {
	// old Unknown completing after newer Enabled+balance=200 -> final Enabled/200.
	s := NewStreamer("bob", DefaultStreamerSettings())
	_, oldSeq := s.ChannelPointsCapabilitySnapshot()
	s.ApplyChannelPointsContextIfCurrent(oldSeq, CapabilityEnabled, CapReasonConfirmedContext, 200, true) // newer lands
	if tr := s.ApplyChannelPointsContextIfCurrent(oldSeq, CapabilityUnknown, CapReasonTimeout, 0, false); !tr.Stale {
		t.Fatal("stale unknown must be dropped")
	}
	if s.GetChannelPointsCapability() != CapabilityEnabled || s.GetChannelPoints() != 200 {
		t.Fatalf("final should be enabled/200, got %v/%d", s.GetChannelPointsCapability(), s.GetChannelPoints())
	}

	// old Enabled+balance=100 completing after newer Enabled+balance=200 -> 200.
	s2 := NewStreamer("bob", DefaultStreamerSettings())
	_, oldSeq2 := s2.ChannelPointsCapabilitySnapshot()
	s2.ApplyChannelPointsContextIfCurrent(oldSeq2, CapabilityEnabled, CapReasonConfirmedContext, 200, true)
	s2.ApplyChannelPointsContextIfCurrent(oldSeq2, CapabilityEnabled, CapReasonConfirmedContext, 100, true) // stale
	if s2.GetChannelPoints() != 200 {
		t.Fatalf("stale balance must not overwrite newer: got %d", s2.GetChannelPoints())
	}

	// genuine latest Unknown preserves last-confirmed and balance.
	s3 := NewStreamer("bob", DefaultStreamerSettings())
	s3.SetChannelPoints(77)
	_, seq3 := s3.ChannelPointsCapabilitySnapshot()
	s3.ApplyChannelPointsContextIfCurrent(seq3, CapabilityEnabled, CapReasonConfirmedContext, 300, true)
	_, seq3b := s3.ChannelPointsCapabilitySnapshot()
	s3.ApplyChannelPointsContextIfCurrent(seq3b, CapabilityUnknown, CapReasonTimeout, 0, false)
	if s3.GetChannelPointsCapability() != CapabilityUnknown {
		t.Fatal("latest unknown should be current state")
	}
	if s3.LastConfirmedChannelPointsCapability() != CapabilityEnabled || s3.GetChannelPoints() != 300 {
		t.Fatalf("unknown must preserve last-confirmed enabled and balance 300, got %v/%d",
			s3.LastConfirmedChannelPointsCapability(), s3.GetChannelPoints())
	}
}

// 23: capability and liveness are independent tri-states.
func TestCapabilityIndependentFromLiveness(t *testing.T) {
	s := NewStreamer("bob", DefaultStreamerSettings())
	s.SetConfirmedOnline()
	s.SetChannelPointsCapability(CapabilityDisabled, CapReasonConfirmedDisabled)
	if s.GetStatus() != StatusOnline {
		t.Fatal("capability change must not affect liveness")
	}
	if s.GetChannelPointsCapability() != CapabilityDisabled {
		t.Fatal("liveness must not affect capability")
	}
	// Liveness unknown does not change capability.
	s.SetUnknown(ReasonTransportError)
	if s.GetChannelPointsCapability() != CapabilityDisabled {
		t.Fatal("liveness->unknown must leave capability unchanged")
	}
}

// --- B3: Channel Points context observation snapshot ---------------------

func ctxSnap(cap CapabilityState, balance int, mult int) ChannelPointsContextSnapshot {
	ms := make([]Multiplier, mult)
	for i := range ms {
		ms[i] = Multiplier{Factor: 1.5}
	}
	return ChannelPointsContextSnapshot{
		Capability: cap, Reason: CapReasonConfirmedContext,
		Balance: balance, HasBalance: true,
		Multipliers: ms, HasMultipliers: true,
	}
}

// Scenarios 1-6: old begins, new begins later, new publishes, old publishes
// afterward -> final state is NEW; the old (stale) context is dropped whole.
func TestContextObservationNewestWinsForwardOrder(t *testing.T) {
	s := NewStreamer("bob", DefaultStreamerSettings())
	old := s.BeginChannelPointsContextObservation()
	newer := s.BeginChannelPointsContextObservation()

	if r := s.ApplyChannelPointsContext(newer, ctxSnap(CapabilityEnabled, 200, 2)); !r.Applied {
		t.Fatal("newer observation should apply")
	}
	if r := s.ApplyChannelPointsContext(old, ctxSnap(CapabilityEnabled, 100, 1)); !r.Stale {
		t.Fatal("older observation must be dropped as stale")
	}
	if s.GetChannelPoints() != 200 || len(s.ActiveMultipliers) != 2 {
		t.Fatalf("final must be newer: balance=%d mult=%d", s.GetChannelPoints(), len(s.ActiveMultipliers))
	}
}

// Scenarios 7-11: old begins, new begins later, OLD completes first, new
// completes afterward -> final is still NEW (newest-started-wins, NOT
// first-completion-wins).
func TestContextObservationNewestWinsReverseOrder(t *testing.T) {
	s := NewStreamer("bob", DefaultStreamerSettings())
	old := s.BeginChannelPointsContextObservation()
	newer := s.BeginChannelPointsContextObservation()

	// OLD completes first — but a newer observation already began, so it is stale.
	if r := s.ApplyChannelPointsContext(old, ctxSnap(CapabilityEnabled, 100, 1)); !r.Stale {
		t.Fatal("old completing first must still be stale (a newer obs began)")
	}
	if r := s.ApplyChannelPointsContext(newer, ctxSnap(CapabilityEnabled, 200, 2)); !r.Applied {
		t.Fatal("newer observation must apply")
	}
	if s.GetChannelPoints() != 200 || len(s.ActiveMultipliers) != 2 {
		t.Fatalf("final must be newer regardless of completion order: balance=%d mult=%d", s.GetChannelPoints(), len(s.ActiveMultipliers))
	}
}

// Scenarios 12-16: post-apply interleaving — old reaches publication AFTER new
// already published; old must not overwrite any field and must not claim.
func TestContextObservationPostApplyInterleaving(t *testing.T) {
	s := NewStreamer("bob", DefaultStreamerSettings())
	old := s.BeginChannelPointsContextObservation()
	newer := s.BeginChannelPointsContextObservation()

	newSnap := ctxSnap(CapabilityEnabled, 200, 2)
	newSnap.Goals = []*CommunityGoal{{GoalID: "g-new"}}
	newSnap.HasGoals = true
	newSnap.AvailableClaimID = "claim-new"
	s.ApplyChannelPointsContext(newer, newSnap)

	oldSnap := ctxSnap(CapabilityEnabled, 100, 1)
	oldSnap.Goals = []*CommunityGoal{{GoalID: "g-old"}}
	oldSnap.HasGoals = true
	oldSnap.AvailableClaimID = "claim-old"
	res := s.ApplyChannelPointsContext(old, oldSnap)

	if !res.Stale {
		t.Fatal("old post-apply must be stale")
	}
	if s.GetChannelPoints() != 200 || len(s.ActiveMultipliers) != 2 {
		t.Fatal("old must not overwrite balance/multipliers")
	}
	if _, ok := s.CommunityGoals["g-old"]; ok {
		t.Fatal("old goals must not be written")
	}
	// old bonus claim must not authorize (obs no longer current).
	if s.AuthorizeBonusClaim(old, "claim-old") {
		t.Fatal("stale observation must not authorize a bonus claim")
	}
}

// Scenarios 17-19: optional-field preservation.
func TestContextObservationOptionalFieldPreservation(t *testing.T) {
	// 17: valid response WITHOUT activeMultipliers preserves previous multipliers.
	s := NewStreamer("bob", DefaultStreamerSettings())
	o1 := s.BeginChannelPointsContextObservation()
	s.ApplyChannelPointsContext(o1, ctxSnap(CapabilityEnabled, 100, 3)) // 3 multipliers
	o2 := s.BeginChannelPointsContextObservation()
	s.ApplyChannelPointsContext(o2, ChannelPointsContextSnapshot{
		Capability: CapabilityEnabled, Reason: CapReasonConfirmedContext,
		Balance: 150, HasBalance: true, // no multipliers field
	})
	if len(s.ActiveMultipliers) != 3 {
		t.Fatalf("absent multipliers must preserve previous: got %d", len(s.ActiveMultipliers))
	}
	if s.GetChannelPoints() != 150 {
		t.Fatal("balance should still update")
	}

	// 19: a valid EMPTY activeMultipliers authoritatively clears them.
	o3 := s.BeginChannelPointsContextObservation()
	s.ApplyChannelPointsContext(o3, ChannelPointsContextSnapshot{
		Capability: CapabilityEnabled, Reason: CapReasonConfirmedContext,
		Multipliers: []Multiplier{}, HasMultipliers: true,
	})
	if len(s.ActiveMultipliers) != 0 {
		t.Fatalf("valid empty multipliers must clear: got %d", len(s.ActiveMultipliers))
	}
}

// Scenarios 20-21: latest genuine Unknown preserves everything; a stale Unknown
// changes nothing.
func TestContextObservationUnknownPreservesAndStaleNoop(t *testing.T) {
	s := NewStreamer("bob", DefaultStreamerSettings())
	o1 := s.BeginChannelPointsContextObservation()
	s.ApplyChannelPointsContext(o1, ctxSnap(CapabilityEnabled, 300, 2))

	// 20: latest Unknown preserves LastConfirmed, balance, multipliers.
	o2 := s.BeginChannelPointsContextObservation()
	s.ApplyChannelPointsContext(o2, ChannelPointsContextSnapshot{Capability: CapabilityUnknown, Reason: CapReasonTimeout})
	if s.GetChannelPointsCapability() != CapabilityUnknown {
		t.Fatal("latest should be unknown")
	}
	if s.LastConfirmedChannelPointsCapability() != CapabilityEnabled || s.GetChannelPoints() != 300 || len(s.ActiveMultipliers) != 2 {
		t.Fatal("unknown must preserve last-confirmed, balance and multipliers")
	}

	// 21: a stale Unknown (old obs) changes nothing.
	if r := s.ApplyChannelPointsContext(o1, ChannelPointsContextSnapshot{Capability: CapabilityUnknown, Reason: CapReasonTransportError}); !r.Stale {
		t.Fatal("stale unknown must be dropped")
	}
}

// Scenario 22: a duplicate availableClaim across racing contexts authorizes at
// most one mutation.
func TestContextObservationBonusReservationDedup(t *testing.T) {
	s := NewStreamer("bob", DefaultStreamerSettings())
	obs := s.BeginChannelPointsContextObservation()
	s.ApplyChannelPointsContext(obs, ctxSnap(CapabilityEnabled, 100, 0))

	if !s.AuthorizeBonusClaim(obs, "claim-1") {
		t.Fatal("first authorization should succeed")
	}
	if s.AuthorizeBonusClaim(obs, "claim-1") {
		t.Fatal("duplicate claim id must not authorize twice")
	}
	// An empty claim id never authorizes.
	if s.AuthorizeBonusClaim(obs, "") {
		t.Fatal("empty claim id must not authorize")
	}
	// A stale observation never authorizes.
	stale := obs
	s.BeginChannelPointsContextObservation() // advance
	if s.AuthorizeBonusClaim(stale, "claim-2") {
		t.Fatal("stale observation must not authorize")
	}
}
