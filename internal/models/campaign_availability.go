package models

import "time"

// CampaignAvailabilityState is the tri-state result of the channel-side
// per-channel drop-campaign lookup (GetCampaignIDsFromStreamer). It is
// deliberately distinct from the advertised ID list so a transient lookup
// failure (Unknown) is never conflated with an authoritative "no campaigns
// advertised here" (Known + empty list).
type CampaignAvailabilityState uint8

const (
	// CampaignAvailabilityUnknown: the last channel-side lookup could NOT be
	// resolved (transport/GQL/PQNF/malformed). The retained CampaignIDs are
	// last-known diagnostic/continuity data only — they must never be read as a
	// fresh authoritative "available here".
	CampaignAvailabilityUnknown CampaignAvailabilityState = iota
	// CampaignAvailabilityKnown: the last lookup resolved authoritatively. The
	// CampaignIDs list is exact (an empty list means "no campaigns available on
	// this channel").
	CampaignAvailabilityKnown
)

func (a CampaignAvailabilityState) String() string {
	if a == CampaignAvailabilityKnown {
		return "known"
	}
	return "unknown"
}

// CampaignAvailabilityGrace is the bounded-continuity window: how long an
// already-assigned drop campaign may be RETAINED while channel-side availability
// is Unknown before the assignment is dropped. It is DERIVED from the normal
// stream refresh cadence (two refresh cycles) rather than an arbitrary duration,
// so a single missed/failed refresh never severs an active farm but a persistent
// failure does eventually release the slot. No config knob — this is a safety
// bound, not a preference.
const CampaignAvailabilityGrace = 2 * StreamUpdateInterval

// CampaignAvailabilitySnapshot is an immutable, bounded-continuity-aware view of
// the channel-side availability, published atomically by ApplyCampaignAvailability.
type CampaignAvailabilitySnapshot struct {
	State         CampaignAvailabilityState
	CampaignIDs   []string
	ObservationID uint64
	ObservedAt    time.Time
	// UnknownSince is the instant the current Unknown streak began (zero while
	// Known). It is stamped once at the first Unknown after a Known and preserved
	// across subsequent Unknowns, so repeated failures never extend the grace.
	UnknownSince time.Time
	// LastKnownAt is the last instant the lookup resolved authoritatively (Known).
	LastKnownAt time.Time
}

// CampaignAvailabilityApplyResult is the immutable outcome of an
// ApplyCampaignAvailability publication.
type CampaignAvailabilityApplyResult struct {
	// Applied is true when obsID was still the latest-begun observation and the
	// result was published.
	Applied bool
	// Stale is true when a newer observation had already begun, so nothing was
	// written (neither state nor IDs nor the continuity timestamps).
	Stale bool
	// State echoes the state after the call (the published state when Applied, the
	// unchanged current state when Stale).
	State CampaignAvailabilityState
}

// BeginCampaignAvailabilityObservation starts a new channel-side availability
// observation and returns its monotonic ID. Callers invoke it BEFORE their
// network I/O; the highest ID (latest begun) is the authoritative observation, so
// ApplyCampaignAvailability publishes only for the newest request regardless of
// completion order. This is what makes availability newest-STARTED-wins rather
// than first-completion-wins.
func (s *Stream) BeginCampaignAvailabilityObservation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.campaignAvailObs++
	return s.campaignAvailObs
}

// ApplyCampaignAvailability atomically publishes a channel-side lookup result,
// but only when obsID is STILL the latest-begun observation. If a newer
// observation has begun, the whole result is dropped (Stale) — no state, IDs or
// continuity-timestamp write — so an old slow response can never overwrite a
// newer one (in either direction: stale Unknown cannot clobber a newer Known,
// stale Known cannot clobber a newer Unknown, and a stale Known-empty cannot
// clear a newer populated list).
//
// On known==true the advertised IDs are replaced (an empty list is the
// authoritative "not available here"), LastKnownAt is stamped and any Unknown
// streak ends. On known==false the state becomes Unknown, the previous IDs are
// KEPT as last-known continuity data, and UnknownSince is stamped only if not
// already in an Unknown streak (so repeated failures do not extend the grace).
func (s *Stream) ApplyCampaignAvailability(obsID uint64, known bool, ids []string, now time.Time) CampaignAvailabilityApplyResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if obsID != s.campaignAvailObs {
		return CampaignAvailabilityApplyResult{Stale: true, State: s.campaignAvailability}
	}
	s.campaignAvailObservedAt = now
	if known {
		s.CampaignIDs = ids
		s.campaignAvailability = CampaignAvailabilityKnown
		s.campaignLastKnownAt = now
		s.campaignUnknownSince = time.Time{}
	} else {
		if s.campaignAvailability != CampaignAvailabilityUnknown || s.campaignUnknownSince.IsZero() {
			s.campaignUnknownSince = now
		}
		s.campaignAvailability = CampaignAvailabilityUnknown
		// CampaignIDs deliberately preserved (last-known continuity/diagnostic).
	}
	return CampaignAvailabilityApplyResult{Applied: true, State: s.campaignAvailability}
}

// CampaignAvailability returns the current channel-side availability state and
// the last-known advertised campaign IDs. Callers MUST consult the state: on
// Unknown the IDs are stale continuity data, not a fresh authoritative Yes.
func (s *Stream) CampaignAvailability() (CampaignAvailabilityState, []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.campaignAvailability, s.CampaignIDs
}

// CampaignAvailabilitySnapshotAt returns an immutable snapshot of the channel-side
// availability plus whether an Unknown streak has exceeded the bounded-continuity
// grace as of now. unknownGraceExpired is true ONLY when the state is Unknown, a
// streak start is recorded, and now is at least CampaignAvailabilityGrace past it
// — the signal the drops assignment path uses to finally release a retained
// assignment. A Known state (or an Unknown with no recorded streak start, i.e. a
// never-resolved channel with nothing to retain) is never "expired".
func (s *Stream) CampaignAvailabilitySnapshotAt(now time.Time) (CampaignAvailabilitySnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snap := CampaignAvailabilitySnapshot{
		State:         s.campaignAvailability,
		CampaignIDs:   s.CampaignIDs,
		ObservationID: s.campaignAvailObs,
		ObservedAt:    s.campaignAvailObservedAt,
		UnknownSince:  s.campaignUnknownSince,
		LastKnownAt:   s.campaignLastKnownAt,
	}
	expired := snap.State == CampaignAvailabilityUnknown &&
		!snap.UnknownSince.IsZero() &&
		!now.Before(snap.UnknownSince.Add(CampaignAvailabilityGrace))
	return snap, expired
}

// MarkCampaignAvailabilityUnknown records an unresolved lookup unconditionally
// (single-goroutine test setup), keeping the previous IDs as last-known data and
// stamping the Unknown-streak start if one is not already in progress.
func (s *Stream) MarkCampaignAvailabilityUnknown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.campaignAvailObs++
	if s.campaignAvailability != CampaignAvailabilityUnknown || s.campaignUnknownSince.IsZero() {
		s.campaignUnknownSince = time.Now()
	}
	s.campaignAvailability = CampaignAvailabilityUnknown
}
