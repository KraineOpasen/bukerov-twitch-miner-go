package models

import "strings"

// AccountConnection is the tri-state result of decoding a drop campaign's
// `self.isAccountConnected` flag — whether the operator's Twitch account is
// linked to the campaign publisher's account (required to earn that publisher's
// in-game drop entitlements).
//
// It is deliberately NOT a plain bool: a plain bool cannot distinguish a proven
// "not connected" (authoritative false) from "Twitch did not tell us" (a null,
// an absent field, a malformed value, or a partial/error response). Conflating
// the two would let a missing field masquerade as a proven disconnection and
// silently drop a campaign the operator is in fact eligible for. Unknown always
// fails open: it is never treated as a disconnection.
type AccountConnection uint8

const (
	// AccountConnectionUnknown means Twitch did not authoritatively report the
	// connection state for this campaign (null, absent, malformed, or simply not
	// present in a partial response). It ALWAYS fails open — never a basis to
	// exclude a reward.
	AccountConnectionUnknown AccountConnection = iota
	// AccountConnectionConnected means Twitch authoritatively reported the account
	// as linked (isAccountConnected == true).
	AccountConnectionConnected
	// AccountConnectionDisconnected means Twitch authoritatively reported the
	// account as NOT linked (isAccountConnected == false). This is the only value
	// that can, combined with a reward that requires the publisher link, exclude a
	// reward.
	AccountConnectionDisconnected
)

func (a AccountConnection) String() string {
	switch a {
	case AccountConnectionConnected:
		return "connected"
	case AccountConnectionDisconnected:
		return "disconnected"
	default:
		return "unknown"
	}
}

// ParseAccountConnection decodes the tri-state connection status from a campaign
// GQL object (a `dropCampaign`/`dropCampaignsInProgress` map). The authoritative
// value lives at `self.isAccountConnected`. Only a real JSON boolean is
// authoritative:
//
//   - `self.isAccountConnected == true`  -> Connected
//   - `self.isAccountConnected == false` -> Disconnected
//   - `self` absent/null, `isAccountConnected` absent/null, or ANY non-boolean
//     value (a malformed optional, a number, a string) -> Unknown
//
// A decode failure of this single optional field can therefore never become a
// proven disconnection — it degrades to Unknown, which fails open.
func ParseAccountConnection(data map[string]interface{}) AccountConnection {
	self, ok := data["self"].(map[string]interface{})
	if !ok || self == nil {
		return AccountConnectionUnknown
	}
	connected, ok := self["isAccountConnected"].(bool)
	if !ok {
		return AccountConnectionUnknown
	}
	if connected {
		return AccountConnectionConnected
	}
	return AccountConnectionDisconnected
}

// BenefitType is the typed classification of a drop reward's benefit, decoded
// from Twitch's `benefit.distributionType`. It is used to decide, per reward,
// whether that reward requires the publisher/account link. Free-text is never a
// business decision input — only this typed classification is.
//
// Values mirror Twitch's distributionType enum (as used by the canonical
// reference miner DevilXD/TwitchDropsMiner, whose persisted-query hashes this
// project already tracks): BADGE, EMOTE, DIRECT_ENTITLEMENT, plus an explicit
// Unknown bucket for an absent/null/unrecognized value.
type BenefitType uint8

const (
	// BenefitTypeUnknown covers an absent, null, malformed, or unrecognized
	// distributionType (including any new type Twitch may add). It fails open:
	// an unknown reward type is never treated as requiring the publisher link.
	BenefitTypeUnknown BenefitType = iota
	// BenefitTypeBadge is a chat/profile badge reward. Granted by watching; it
	// does NOT require linking a publisher account.
	BenefitTypeBadge
	// BenefitTypeEmote is an emote reward. Granted by watching; it does NOT
	// require linking a publisher account.
	BenefitTypeEmote
	// BenefitTypeDirectEntitlement is an in-game item entitlement delivered
	// directly to the linked publisher account. It is the reward class that
	// authoritatively requires the account link.
	BenefitTypeDirectEntitlement
)

func (b BenefitType) String() string {
	switch b {
	case BenefitTypeBadge:
		return "badge"
	case BenefitTypeEmote:
		return "emote"
	case BenefitTypeDirectEntitlement:
		return "direct_entitlement"
	default:
		return "unknown"
	}
}

// ParseBenefitType maps a raw `distributionType` string to a typed BenefitType.
// Matching is case-insensitive and whitespace-trimmed. Anything not recognized
// — including an empty string — becomes BenefitTypeUnknown (fail open).
func ParseBenefitType(distributionType string) BenefitType {
	switch strings.ToUpper(strings.TrimSpace(distributionType)) {
	case "BADGE":
		return BenefitTypeBadge
	case "EMOTE":
		return BenefitTypeEmote
	case "DIRECT_ENTITLEMENT":
		return BenefitTypeDirectEntitlement
	default:
		return BenefitTypeUnknown
	}
}

// RequiresPublisherLink reports whether a reward of this benefit type can only
// be earned with a linked publisher account. It is authoritatively true ONLY for
// a direct in-game entitlement. Badges, emotes, and any unknown/absent type fail
// open (false) — so a BADGE or EMOTE reward is never blocked, and a new reward
// type Twitch may add is never wrongly excluded.
func (b BenefitType) RequiresPublisherLink() bool {
	return b == BenefitTypeDirectEntitlement
}

// parseDropBenefitType reads the drop's benefit type from the first benefit edge
// (`benefitEdges[0].benefit.distributionType`), mirroring how NewDropFromGQL
// already reads the benefit name/id/image from the same first edge. A missing or
// malformed edge/benefit/field yields BenefitTypeUnknown (fail open), never a
// false requirement.
func parseDropBenefitType(benefitEdges []interface{}) BenefitType {
	if len(benefitEdges) == 0 {
		return BenefitTypeUnknown
	}
	edge, ok := benefitEdges[0].(map[string]interface{})
	if !ok {
		return BenefitTypeUnknown
	}
	benefit, ok := edge["benefit"].(map[string]interface{})
	if !ok {
		return BenefitTypeUnknown
	}
	dt, _ := benefit["distributionType"].(string)
	return ParseBenefitType(dt)
}
