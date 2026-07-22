package models

import "time"

// CapabilityState is the tri-state availability of a per-channel Twitch feature
// (currently Channel Points). It answers ONLY "has Twitch authoritatively
// confirmed this feature is available for this channel/account?" — a different
// question from liveness (StreamerStatus), from the campaign ACL, and from the
// operator's user settings. Its zero value is CapabilityUnknown by construction,
// so a never-checked channel is UNKNOWN, never a false Disabled.
type CapabilityState uint8

const (
	// CapabilityUnknown: not authoritatively determined (transport/timeout/
	// PQNF/auth error, malformed or unproven response shape, cancellation). Must
	// never be coerced to Enabled or Disabled.
	CapabilityUnknown CapabilityState = iota
	// CapabilityEnabled: Twitch authoritatively confirmed the feature is
	// available (a structurally valid response actually carrying the feature's
	// context).
	CapabilityEnabled
	// CapabilityDisabled: Twitch authoritatively confirmed the feature is off.
	// Reached only from a proven disabled signal — never inferred from a merely
	// missing/absent field.
	CapabilityDisabled
)

func (c CapabilityState) String() string {
	switch c {
	case CapabilityEnabled:
		return "enabled"
	case CapabilityDisabled:
		return "disabled"
	default:
		return "unknown"
	}
}

// CapabilityReason is a compact, privacy-safe classification of WHY a
// capability is in its current state (chiefly why it is unknown). It carries no
// raw Twitch payload, token, cookie, header, or claim identifier.
type CapabilityReason string

const (
	CapReasonInitial           CapabilityReason = "initial"
	CapReasonConfirmedContext  CapabilityReason = "confirmed_context"
	CapReasonConfirmedDisabled CapabilityReason = "confirmed_disabled"
	CapReasonTransportError    CapabilityReason = "transport_error"
	CapReasonTimeout           CapabilityReason = "timeout"
	CapReasonGraphQLError      CapabilityReason = "graphql_error"
	CapReasonPQNF              CapabilityReason = "persisted_query_not_found"
	CapReasonUnauthorized      CapabilityReason = "unauthorized"
	CapReasonMalformed         CapabilityReason = "malformed_response"
	// CapReasonMissingContext is used when a structurally valid channel response
	// simply lacks the feature's context node. Per the proven contract this is
	// classified UNKNOWN (not Disabled) — Twitch is not known to signal "off"
	// by omission, so we refuse to invent a disabled meaning for it.
	CapReasonMissingContext CapabilityReason = "missing_context"
	CapReasonCancelled      CapabilityReason = "context_cancelled"
)

// SetChannelPointsCapability applies a capability observation with monotonic,
// event-safe semantics:
//
//   - Enabled/Disabled (a confirmation): sets the state, records it as the last
//     confirmed capability, stamps ObservedAt/reason, and bumps capSeq.
//   - Unknown (an inconclusive observation): sets the state to Unknown and
//     records the reason, but PRESERVES LastConfirmed and does NOT bump capSeq,
//     and never touches the point balance. A transient failure therefore never
//     erases what was last confirmed.
//
// It returns whether the state actually changed.
func (s *Streamer) SetChannelPointsCapability(state CapabilityState, reason CapabilityReason) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyChannelPointsCapabilityLocked(state, reason)
}

func (s *Streamer) applyChannelPointsCapabilityLocked(state CapabilityState, reason CapabilityReason) bool {
	prev := s.channelPointsCap
	s.channelPointsCap = state
	s.capReason = reason
	s.capObservedAt = time.Now()
	if state == CapabilityEnabled || state == CapabilityDisabled {
		s.lastConfirmedChannelPtCap = state
		s.capSeq++
	}
	return prev != state
}

// CapabilityTransition is the immutable result of an atomic capability/context
// application. It lets callers distinguish "dropped as stale" from "applied but
// unchanged" (a plain bool cannot).
type CapabilityTransition struct {
	Previous CapabilityState
	Current  CapabilityState
	// Applied is true when the observation was accepted (not stale) and written.
	Applied bool
	// Changed is true when Applied and the state actually moved.
	Changed bool
	// Stale is true when the observation was discarded because a newer confirmed
	// transition had already landed since obsSeq was captured.
	Stale bool
}

// ApplyChannelPointsContextIfCurrent atomically applies a Channel Points
// observation (capability + optionally the balance) under a SINGLE lock, but
// only when no newer CONFIRMED capability transition landed since obsSeq was
// captured (before the network I/O). A stale observation is dropped WHOLE —
// neither capability nor balance is written — so an old slow response can never
// overwrite a newer capability or a newer balance, nor trigger a bonus claim off
// a stale context. balance is written only when hasBalance is true and the
// observation is accepted; an Unknown observation preserves LastConfirmed and
// the balance and never bumps the sequence.
func (s *Streamer) ApplyChannelPointsContextIfCurrent(obsSeq uint64, state CapabilityState, reason CapabilityReason, balance int, hasBalance bool) CapabilityTransition {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev := s.channelPointsCap
	if obsSeq != s.capSeq {
		return CapabilityTransition{Previous: prev, Current: prev, Stale: true}
	}
	changed := s.applyChannelPointsCapabilityLocked(state, reason)
	if hasBalance {
		s.ChannelPoints = balance
	}
	return CapabilityTransition{Previous: prev, Current: state, Applied: true, Changed: changed}
}

// ChannelPointsCapabilitySnapshot returns the current capability state and the
// capability sequence, read under the lock. A network caller captures this
// BEFORE its I/O and passes the sequence to ApplyChannelPointsCapabilityIfCurrent
// so a stale result cannot overwrite a newer confirmation.
func (s *Streamer) ChannelPointsCapabilitySnapshot() (CapabilityState, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.channelPointsCap, s.capSeq
}

// ApplyChannelPointsCapabilityIfCurrent applies a capability observation only
// when no newer CONFIRMED transition has landed since obsSeq was captured. A
// stale confirmation is dropped (returns false); an Unknown never bumps the
// sequence, so a genuine confirmation always wins over a racing inconclusive
// check.
func (s *Streamer) ApplyChannelPointsCapabilityIfCurrent(obsSeq uint64, state CapabilityState, reason CapabilityReason) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if obsSeq != s.capSeq {
		return false
	}
	return s.applyChannelPointsCapabilityLocked(state, reason)
}

// GetChannelPointsCapability returns the current tri-state Channel Points
// capability.
func (s *Streamer) GetChannelPointsCapability() CapabilityState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.channelPointsCap
}

// LastConfirmedChannelPointsCapability returns the last authoritatively
// confirmed Channel Points capability (CapabilityUnknown until the first
// confirmation). It survives transitions into Unknown.
func (s *Streamer) LastConfirmedChannelPointsCapability() CapabilityState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastConfirmedChannelPtCap
}

// ChannelPointsCapabilityReason returns the privacy-safe reason code for the
// current capability state.
func (s *Streamer) ChannelPointsCapabilityReason() CapabilityReason {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.capReason
}
