package models

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

// CampaignAvailability returns the current channel-side availability state and
// the last-known advertised campaign IDs. Callers MUST consult the state: on
// Unknown the IDs are stale continuity data, not a fresh authoritative Yes.
func (s *Stream) CampaignAvailability() (CampaignAvailabilityState, []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.campaignAvailability, s.CampaignIDs
}

// CampaignAvailabilitySeq returns the availability observation sequence. A
// network caller captures it BEFORE the lookup and passes it to
// SetCampaignAvailabilityIfCurrent so a stale result cannot overwrite a newer one.
func (s *Stream) CampaignAvailabilitySeq() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.campaignAvailSeq
}

// SetCampaignAvailabilityIfCurrent records a channel-side lookup result, but only
// when no newer availability observation landed since obsSeq was captured (stale
// guard). On known==true the advertised IDs are replaced (an empty list is the
// authoritative "not available here"); on known==false the state becomes Unknown
// and the previous IDs are KEPT as last-known continuity/diagnostic data — never
// promoted to a fresh authoritative Yes. Returns whether the result was applied.
func (s *Stream) SetCampaignAvailabilityIfCurrent(obsSeq uint64, known bool, ids []string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if obsSeq != s.campaignAvailSeq {
		return false
	}
	s.campaignAvailSeq++
	if known {
		s.CampaignIDs = ids
		s.campaignAvailability = CampaignAvailabilityKnown
	} else {
		s.campaignAvailability = CampaignAvailabilityUnknown
	}
	return true
}

// MarkCampaignAvailabilityUnknown records an unresolved lookup unconditionally
// (single-goroutine test setup), keeping the previous IDs as last-known data.
func (s *Stream) MarkCampaignAvailabilityUnknown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.campaignAvailSeq++
	s.campaignAvailability = CampaignAvailabilityUnknown
}
