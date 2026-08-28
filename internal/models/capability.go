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

// ChannelPointsContextSnapshot is a fully-parsed Channel Points context, built
// from a response BEFORE any streamer write, so the whole observation can be
// published in ONE atomic step (capability + balance + multipliers + goals)
// rather than piecemeal. Each Has* flag distinguishes "field present and valid"
// (apply) from "absent or malformed" (preserve the prior value) — so a
// missing/malformed activeMultipliers never clears known-good multipliers, while
// a valid empty list authoritatively clears them.
//
// Goals are the deliberate exception to the "valid empty clears" rule: HasGoals
// governs whether the listed goals are UPSERTED into the streamer's goal map,
// but a valid-empty goals list does NOT clear existing goals. Goal
// removal/lifecycle is owned by the PubSub community-points delete path, not by a
// periodic context snapshot (which can legitimately omit a goal without it having
// ended).
type ChannelPointsContextSnapshot struct {
	Capability CapabilityState
	Reason     CapabilityReason

	Balance    int
	HasBalance bool

	Multipliers    []Multiplier
	HasMultipliers bool

	// Goals is upsert-merged (never cleared on empty) — see the type doc.
	Goals    []*CommunityGoal
	HasGoals bool

	AvailableClaimID string
}

// ContextApplyResult is the immutable outcome of ApplyChannelPointsContext.
type ContextApplyResult struct {
	// Applied is true when the observation was the latest-begun one and the whole
	// snapshot was published.
	Applied bool
	// Stale is true when a newer observation had already begun, so nothing was
	// written (neither state nor a bonus-claim opportunity).
	Stale bool
	// Capability / AvailableClaimID echo the published values (only when Applied).
	Capability       CapabilityState
	AvailableClaimID string
}

// BeginChannelPointsContextObservation starts a new Channel Points context
// observation and returns its monotonic ID. Callers invoke it BEFORE their
// network I/O; the highest ID (latest begun) is the authoritative observation,
// so ApplyChannelPointsContext/ReserveBonusClaimIfEligible publish only for the
// newest request regardless of completion order.
func (s *Streamer) BeginChannelPointsContextObservation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.capObs++
	return s.capObs
}

// ApplyChannelPointsContext atomically publishes a fully-parsed context snapshot
// under a SINGLE lock, but only when obsID is still the latest-begun observation.
// If a newer observation has begun, the whole snapshot is dropped (Stale) — no
// capability/balance/multiplier/goal write and no bonus-claim opportunity — so an
// old slow response can never overwrite newer state nor interleave a partial
// write between a newer response's fields. Optional fields are applied only when
// their Has* flag is set (absent/malformed => preserved).
func (s *Streamer) ApplyChannelPointsContext(obsID uint64, snap ChannelPointsContextSnapshot) ContextApplyResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if obsID != s.capObs {
		return ContextApplyResult{Stale: true}
	}
	// Record exactly which authoritative current observation advertised a bonus.
	// Observed reservations must match both fields; passing an arbitrary current
	// generation and claim ID cannot manufacture retry authority.
	s.bonusObservation = obsID
	s.bonusObservedClaimID = snap.AvailableClaimID
	s.applyChannelPointsCapabilityLocked(snap.Capability, snap.Reason)
	if snap.HasBalance {
		s.ChannelPoints = snap.Balance
	}
	if snap.HasMultipliers {
		s.ActiveMultipliers = snap.Multipliers
	}
	if snap.HasGoals {
		// Upsert-only: a context snapshot can legitimately omit an active goal, so
		// we never treat a shrinking/empty list as a removal. Goal deletion is
		// owned by the PubSub delete path (see the snapshot type doc).
		if s.CommunityGoals == nil {
			s.CommunityGoals = make(map[string]*CommunityGoal)
		}
		for _, g := range snap.Goals {
			if g != nil {
				s.CommunityGoals[g.GoalID] = g
			}
		}
	}
	return ContextApplyResult{Applied: true, Capability: snap.Capability, AvailableClaimID: snap.AvailableClaimID}
}

// BonusReservationReason is a stable, privacy-safe code explaining why a bonus
// claim reservation was granted or refused. It carries no claim identifier.
type BonusReservationReason uint8

const (
	BonusReservationOK BonusReservationReason = iota
	BonusReservationStaleObservation
	BonusReservationOffline
	BonusReservationStatusUnknown
	BonusReservationCapabilityDisabled
	BonusReservationCapabilityUnknown
	BonusReservationEmptyClaim
	BonusReservationClaimNotObserved
	BonusReservationInFlight
	BonusReservationCompleted
	BonusReservationTerminalRejected
	BonusReservationIndeterminate
	BonusReservationRetryNeedsObservation
	BonusReservationRetryExhausted
)

func (r BonusReservationReason) String() string {
	switch r {
	case BonusReservationOK:
		return "ok"
	case BonusReservationStaleObservation:
		return "stale_observation"
	case BonusReservationOffline:
		return "status_offline"
	case BonusReservationStatusUnknown:
		return "status_unknown"
	case BonusReservationCapabilityDisabled:
		return "capability_disabled"
	case BonusReservationCapabilityUnknown:
		return "capability_unknown"
	case BonusReservationEmptyClaim:
		return "empty_claim"
	case BonusReservationClaimNotObserved:
		return "claim_not_observed"
	case BonusReservationInFlight:
		return "claim_in_flight"
	case BonusReservationCompleted:
		return "claim_completed"
	case BonusReservationTerminalRejected:
		return "claim_terminal_rejected"
	case BonusReservationIndeterminate:
		return "claim_indeterminate"
	case BonusReservationRetryNeedsObservation:
		return "retry_needs_current_observation"
	case BonusReservationRetryExhausted:
		return "claim_retry_exhausted"
	default:
		return "unknown"
	}
}

type bonusClaimPhase uint8

const (
	bonusClaimInFlight bonusClaimPhase = iota
	bonusClaimSucceeded
	bonusClaimTerminalRejected
	bonusClaimIndeterminate
	bonusClaimRetryableAwaitingObservation
	bonusClaimRetryExhausted
)

// One initial mutation plus one later retry is the smallest useful bounded
// policy. A retry is admitted only after the first attempt was proved not to
// have executed and a fresh current context still advertises the same claim ID.
const bonusClaimMaxAttempts = 2

type bonusClaimRecord struct {
	phase                 bonusClaimPhase
	attempt               uint64
	attempts              uint8
	retryAfterObservation uint64
}

// BonusClaimCompletion is the privacy-safe outcome a network owner commits to
// the Streamer arbitration ledger. It carries no claim ID, payload, or token.
type BonusClaimCompletion uint8

const (
	BonusClaimCompletionInvalid BonusClaimCompletion = iota
	BonusClaimCompletionSucceeded
	BonusClaimCompletionReconciled
	BonusClaimCompletionRejected
	BonusClaimCompletionProvenNotExecuted
	BonusClaimCompletionIndeterminate
)

// BonusClaimCompletionResult reports whether a completion token was current and
// whether this transition owns the one permitted local fresh-success event.
type BonusClaimCompletionResult struct {
	Applied      bool
	FreshSuccess bool
}

// BonusClaimReservation is the immutable result of a bonus-claim reservation.
type BonusClaimReservation struct {
	// Authorized is true only when the caller may proceed to the single Twitch
	// ClaimBonus mutation for this claim id.
	Authorized bool
	Reason     BonusReservationReason

	// claimID/attempt form an opaque completion token. Callers can only pass the
	// reservation back to CompleteBonusClaim; they cannot forge or inspect it.
	claimID string
	attempt uint64
}

// ReserveBonusClaimIfEligible atomically confirms the CURRENT bonus-claim
// prerequisites AND reserves the claim, all under one lock, so nothing can change
// between the eligibility check and the reservation (the streamer cannot go
// Offline or lose the Channel Points capability in the gap). It grants the
// reservation only when, at this instant:
//
//   - obsID is still the latest-begun Channel Points observation (not superseded);
//   - the streamer is confirmed StatusOnline;
//   - the Channel Points capability is confirmed Enabled;
//   - the claim id is non-empty AND not already in-flight/terminal;
//   - a proved non-execution may retry only from this fresh observation, once.
//
// The prerequisites intentionally mirror EvaluatePointsTask(TaskBonusClaim) for
// the liveness/capability axis (a parity test locks this equivalence). The
// reservation is taken under the lock; the caller releases the lock before the
// network ClaimBonus. Every production path shares this exact per-ID ledger.
func (s *Streamer) ReserveBonusClaimIfEligible(obsID uint64, claimID string) BonusClaimReservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if obsID != s.capObs {
		return BonusClaimReservation{Reason: BonusReservationStaleObservation}
	}
	if reason := s.bonusClaimEligibilityReasonLocked(); reason != BonusReservationOK {
		return BonusClaimReservation{Reason: reason}
	}
	if obsID != s.bonusObservation || claimID != s.bonusObservedClaimID {
		return BonusClaimReservation{Reason: BonusReservationClaimNotObserved}
	}
	return s.reserveBonusClaimLocked(claimID, obsID, true)
}

// ReserveCurrentBonusClaimIfEligible is the PubSub/direct counterpart of
// ReserveBonusClaimIfEligible. It re-checks liveness/capability atomically with
// the shared reservation, but it cannot re-arm a failed attempt: only a fresh
// current ChannelPointsContext proves that ID is still available.
func (s *Streamer) ReserveCurrentBonusClaimIfEligible(claimID string) BonusClaimReservation {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reason := s.bonusClaimEligibilityReasonLocked(); reason != BonusReservationOK {
		return BonusClaimReservation{Reason: reason}
	}
	return s.reserveBonusClaimLocked(claimID, 0, false)
}

func (s *Streamer) bonusClaimEligibilityReasonLocked() BonusReservationReason {
	switch s.Status {
	case StatusOffline:
		return BonusReservationOffline
	case StatusUnknown:
		return BonusReservationStatusUnknown
	}
	switch s.channelPointsCap {
	case CapabilityDisabled:
		return BonusReservationCapabilityDisabled
	case CapabilityUnknown:
		return BonusReservationCapabilityUnknown
	}
	return BonusReservationOK
}

func (s *Streamer) reserveBonusClaimLocked(claimID string, observationID uint64, allowObservedRetry bool) BonusClaimReservation {
	if claimID == "" {
		return BonusClaimReservation{Reason: BonusReservationEmptyClaim}
	}

	record, exists := s.bonusClaims[claimID]
	if exists {
		switch record.phase {
		case bonusClaimInFlight:
			return BonusClaimReservation{Reason: BonusReservationInFlight}
		case bonusClaimSucceeded:
			return BonusClaimReservation{Reason: BonusReservationCompleted}
		case bonusClaimTerminalRejected:
			return BonusClaimReservation{Reason: BonusReservationTerminalRejected}
		case bonusClaimIndeterminate:
			return BonusClaimReservation{Reason: BonusReservationIndeterminate}
		case bonusClaimRetryableAwaitingObservation:
			if !allowObservedRetry || observationID <= record.retryAfterObservation {
				return BonusClaimReservation{Reason: BonusReservationRetryNeedsObservation}
			}
			if record.attempts >= bonusClaimMaxAttempts {
				return BonusClaimReservation{Reason: BonusReservationRetryExhausted}
			}
		case bonusClaimRetryExhausted:
			return BonusClaimReservation{Reason: BonusReservationRetryExhausted}
		default:
			return BonusClaimReservation{Reason: BonusReservationIndeterminate}
		}
	}

	if s.bonusClaims == nil {
		s.bonusClaims = make(map[string]bonusClaimRecord)
	}
	s.bonusClaimSeq++
	attempts := uint8(1)
	if exists {
		attempts = record.attempts + 1
	}
	record = bonusClaimRecord{phase: bonusClaimInFlight, attempt: s.bonusClaimSeq, attempts: attempts}
	s.bonusClaims[claimID] = record
	return BonusClaimReservation{
		Authorized: true,
		Reason:     BonusReservationOK,
		claimID:    claimID,
		attempt:    record.attempt,
	}
}

// CompleteBonusClaim commits the network owner's outcome under the Streamer
// lock after network I/O has ended. A stale/forged reservation is ignored, so an
// old completion cannot alter a newer attempt or emit a second success event.
func (s *Streamer) CompleteBonusClaim(reservation BonusClaimReservation, completion BonusClaimCompletion) BonusClaimCompletionResult {
	if !reservation.Authorized || reservation.claimID == "" {
		return BonusClaimCompletionResult{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.bonusClaims[reservation.claimID]
	if !ok || record.phase != bonusClaimInFlight || record.attempt != reservation.attempt {
		return BonusClaimCompletionResult{}
	}

	result := BonusClaimCompletionResult{Applied: true}
	switch completion {
	case BonusClaimCompletionSucceeded:
		record.phase = bonusClaimSucceeded
		result.FreshSuccess = true
	case BonusClaimCompletionReconciled:
		record.phase = bonusClaimSucceeded
	case BonusClaimCompletionRejected:
		record.phase = bonusClaimTerminalRejected
	case BonusClaimCompletionProvenNotExecuted:
		if record.attempts < bonusClaimMaxAttempts {
			record.phase = bonusClaimRetryableAwaitingObservation
			record.retryAfterObservation = s.capObs
		} else {
			record.phase = bonusClaimRetryExhausted
		}
	case BonusClaimCompletionIndeterminate:
		record.phase = bonusClaimIndeterminate
	default:
		// Unknown completion values always fail closed.
		record.phase = bonusClaimIndeterminate
	}
	s.bonusClaims[reservation.claimID] = record
	return result
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
