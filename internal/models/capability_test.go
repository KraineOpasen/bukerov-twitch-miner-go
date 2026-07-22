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
