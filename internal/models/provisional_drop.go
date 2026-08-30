package models

import (
	"strconv"
	"strings"
)

// ProvisionalDropEvidence names the independently-known channel fact that
// permits a lower-authority observation while AvailableDrops is UNKNOWN.
// Neither value is positive AvailableDrops evidence.
type ProvisionalDropEvidence string

const (
	ProvisionalEvidenceDirectory     ProvisionalDropEvidence = "directory_drops_enabled"
	ProvisionalEvidenceRestrictedACL ProvisionalDropEvidence = "restricted_acl"
)

// ProvisionalDropCandidate is an immutable, positive-only observation proposal.
// It is deliberately separate from Stream.CampaignIDs and Stream.Campaigns:
// those remain channel-advertised availability and confirmed assignment
// authority respectively. The broker may use this tuple only to reserve an
// exclusive observation lease and the progress watchdog may prove it only from
// a fresh exact Inventory delta after that reservation.
type ProvisionalDropCandidate struct {
	CampaignID string
	Campaign   string
	DropID     string
	Drop       string
	GameID     string
	Login      string
	ChannelID  string

	BroadcastID          string
	SessionGeneration    uint64
	AvailabilityObs      uint64
	AvailabilityKnownGen uint64
	DirectoryObs         uint64

	Evidence      ProvisionalDropEvidence
	RestrictedACL []string
}

// ProvisionalStreamSnapshot captures every mutable Stream fact used to mint a
// provisional candidate under one lock. It prevents a candidate from combining
// an availability observation from one refresh with a game or playback session
// from another.
type ProvisionalStreamSnapshot struct {
	Availability CampaignAvailabilitySnapshot
	GameID       string
	BroadcastID  string
	// ConfirmedCampaignIDs is the sorted set of campaigns previously assigned
	// during this exact current BroadcastID. It is copied with the other stream
	// facts under one lock, so final discovery/broker admission cannot observe a
	// session snapshot from before a concurrent confirmed assignment.
	ConfirmedCampaignIDs []string
	SessionGeneration    uint64
}

func (s *Stream) ProvisionalDropSnapshot() ProvisionalStreamSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	ids := append([]string(nil), s.CampaignIDs...)
	gameID := ""
	if s.Game != nil {
		gameID = s.Game.ID
	}
	var confirmedCampaignIDs []string
	if s.confirmedCampaignBroadcastID == s.BroadcastID {
		confirmedCampaignIDs = append(confirmedCampaignIDs, s.confirmedCampaignIDs...)
	}
	return ProvisionalStreamSnapshot{
		Availability: CampaignAvailabilitySnapshot{
			State:           s.campaignAvailability,
			CampaignIDs:     ids,
			ObservationID:   s.campaignAvailObs,
			KnownGeneration: s.campaignKnownGen,
			ObservedAt:      s.campaignAvailObservedAt,
			UnknownSince:    s.campaignUnknownSince,
			LastKnownAt:     s.campaignLastKnownAt,
		},
		GameID:               gameID,
		BroadcastID:          s.BroadcastID,
		ConfirmedCampaignIDs: confirmedCampaignIDs,
		SessionGeneration:    s.sessionGen,
	}
}

// HasConfirmedCampaign reports whether the campaign was assigned during this
// snapshot's exact current broadcast. The snapshot intentionally carries no
// IDs from an earlier BroadcastID.
func (s ProvisionalStreamSnapshot) HasConfirmedCampaign(campaignID string) bool {
	if !canonicalProvisionalIdentity(campaignID) {
		return false
	}
	for _, confirmedID := range s.ConfirmedCampaignIDs {
		if confirmedID == campaignID {
			return true
		}
	}
	return false
}

// Valid reports whether the candidate carries every identity needed for a
// session-fenced, exact-Drop lease. Evidence-specific semantic validation stays
// with discovery, which owns the Directory and ACL facts.
func (p ProvisionalDropCandidate) Valid() bool {
	if !canonicalProvisionalIdentity(p.CampaignID) || !canonicalProvisionalIdentity(p.DropID) ||
		!canonicalProvisionalIdentity(p.GameID) || !canonicalProvisionalIdentity(p.Login) ||
		!canonicalProvisionalIdentity(p.ChannelID) || !canonicalProvisionalIdentity(p.BroadcastID) ||
		p.SessionGeneration == 0 || p.AvailabilityObs == 0 {
		return false
	}
	switch p.Evidence {
	case ProvisionalEvidenceDirectory:
		return p.DirectoryObs != 0 && len(p.RestrictedACL) == 0
	case ProvisionalEvidenceRestrictedACL:
		// Restricted ACL authority is self-contained. Carrying a Directory
		// observation on the same tuple would mix two evidence envelopes and let a
		// stale open-campaign row survive a later ACL reclassification.
		if p.DirectoryObs != 0 || len(p.RestrictedACL) == 0 {
			return false
		}
		for i, channelID := range p.RestrictedACL {
			if !canonicalProvisionalIdentity(channelID) || i > 0 && p.RestrictedACL[i-1] >= channelID {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func canonicalProvisionalIdentity(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

func (p ProvisionalDropCandidate) Restricted() bool {
	return p.Evidence == ProvisionalEvidenceRestrictedACL
}

// SameLeaseIdentity compares every authority and freshness fence. A changed
// availability observation, directory observation, broadcast, or playback
// generation requires a new post-reservation baseline.
func (p ProvisionalDropCandidate) SameLeaseIdentity(other ProvisionalDropCandidate) bool {
	if p.CampaignID != other.CampaignID || p.DropID != other.DropID || p.GameID != other.GameID ||
		p.Login != other.Login || p.ChannelID != other.ChannelID ||
		p.BroadcastID != other.BroadcastID || p.SessionGeneration != other.SessionGeneration ||
		p.AvailabilityObs != other.AvailabilityObs || p.AvailabilityKnownGen != other.AvailabilityKnownGen ||
		p.DirectoryObs != other.DirectoryObs ||
		p.Evidence != other.Evidence || len(p.RestrictedACL) != len(other.RestrictedACL) {
		return false
	}
	for i := range p.RestrictedACL {
		if p.RestrictedACL[i] != other.RestrictedACL[i] {
			return false
		}
	}
	return true
}

// SameProofIdentity compares the causal target a positive Inventory delta proved.
// Routine UNKNOWN/Directory refresh generations and the discovery evidence
// envelope are deliberately omitted: every current proposal revalidates those
// source facts before the broker adopts its fresh envelope, while they cannot
// undo a causal server delta. The Known authority epoch remains fenced, so even
// a transient Known+empty result invalidates the old proof before a later UNKNOWN.
func (p ProvisionalDropCandidate) SameProofIdentity(other ProvisionalDropCandidate) bool {
	if p.CampaignID != other.CampaignID || p.DropID != other.DropID || p.GameID != other.GameID ||
		p.Login != other.Login || p.ChannelID != other.ChannelID ||
		p.BroadcastID != other.BroadcastID || p.SessionGeneration != other.SessionGeneration ||
		p.AvailabilityKnownGen != other.AvailabilityKnownGen {
		return false
	}
	return true
}

// QuarantineKey is the narrow in-memory negative identity. Directory and
// availability observation generations are intentionally omitted: a failed
// exact channel/drop/session must not be selected again merely because the
// same UNKNOWN lookup or directory row was refreshed. A new broadcast or
// playback-session generation creates a different key and permits
// reconsideration as required.
func (p ProvisionalDropCandidate) QuarantineKey() string {
	return strings.Join([]string{
		p.Login, p.ChannelID, p.CampaignID, p.DropID, p.GameID,
		p.BroadcastID, strconv.FormatUint(p.SessionGeneration, 10),
	}, "\x00")
}
