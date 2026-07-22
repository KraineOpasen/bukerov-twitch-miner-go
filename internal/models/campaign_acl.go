package models

import (
	"sort"
	"strings"
	"time"
)

// CampaignACLState is the authoritative, typed access-control state of a drop
// campaign. It replaces the old "len(Channels) > 0" heuristic, which could not
// tell an explicitly-unrestricted campaign from one whose allowlist failed to
// load. The zero value is ACLUnknown by construction so an un-populated ACL is
// never mistaken for "unrestricted".
type CampaignACLState uint8

const (
	// ACLUnknown: the allowlist could not be authoritatively determined
	// (missing/malformed allow, enabled-but-incomplete channels, transport
	// failure). Fail closed — never widen eligibility on unknown.
	ACLUnknown CampaignACLState = iota
	// ACLUnrestricted: the campaign credits progress on ANY channel streaming
	// its game (no allow node, or allow.isEnabled == false).
	ACLUnrestricted
	// ACLRestricted: only the exact ChannelIDs in the ACL may progress the
	// campaign.
	ACLRestricted
)

func (s CampaignACLState) String() string {
	switch s {
	case ACLUnrestricted:
		return "unrestricted"
	case ACLRestricted:
		return "restricted"
	default:
		return "unknown"
	}
}

// ACLSource records how an ACL was derived, chiefly so the derived
// compatibility layer can tell "never populated from GQL" (ACLSourceNone, the
// zero value) apart from an explicitly-computed ACLUnknown.
type ACLSource uint8

const (
	// ACLSourceNone means the ACL was never populated from a Twitch response
	// (e.g. a directly-constructed Campaign in a test or legacy path). The
	// derived helpers fall back to the legacy Channels slice in this case.
	ACLSourceNone ACLSource = iota
	// ACLSourceCampaignDetails means the ACL came from a campaign's `allow`
	// block (DropCampaignDetails / dashboard summary).
	ACLSourceCampaignDetails
)

// CampaignACL is the single authoritative representation of a campaign's
// channel access-control. ChannelIDs is deduplicated and sorted deterministically
// and is only meaningful for ACLRestricted. Complete reports whether the channel
// set was fully loaded (false when a paginated/partial load could not be
// finished — such an ACL must never be published as a complete allowlist).
type CampaignACL struct {
	State      CampaignACLState
	ChannelIDs []string
	Complete   bool
	ObservedAt time.Time
	Source     ACLSource
}

// clone returns a deep copy so a published ACL snapshot is never mutated in
// place by a later sync.
func (a CampaignACL) clone() CampaignACL {
	c := a
	if a.ChannelIDs != nil {
		c.ChannelIDs = append([]string(nil), a.ChannelIDs...)
	}
	return c
}

// buildCampaignACL derives the typed ACL from a campaign's raw `allow` value,
// applying the response semantics (documented on Campaign):
//
//   - allow absent            => ACLUnrestricted (proven contract: no allow node
//     means the campaign is creditable on any channel; see the
//     TestNewCampaignFromGQLNoAllowMeansUnrestricted fixture).
//   - allow.isEnabled == false => ACLUnrestricted, channel list cleared
//     (authoritative: the allowlist is disabled).
//   - allow.isEnabled == true (or absent) + a usable channels list
//     => ACLRestricted with the exact, deduped IDs.
//   - allow present but channels missing/empty/malformed
//     => ACLUnknown (never treated as unrestricted).
//
// isEnabled is parsed only when Twitch actually supplies it; the current proven
// contract omits it, in which case a present channels list still yields
// ACLRestricted (preserving legacy behavior) and a missing one yields
// ACLUnknown.
func buildCampaignACL(allowRaw interface{}, observedAt time.Time) CampaignACL {
	acl := CampaignACL{Source: ACLSourceCampaignDetails, ObservedAt: observedAt}

	allow, ok := allowRaw.(map[string]interface{})
	if !ok {
		if allowRaw == nil {
			// No allow node at all: unrestricted (creditable anywhere). Matches
			// the repository's proven fixture contract and preserves backward
			// behavior.
			acl.State = ACLUnrestricted
			acl.Complete = true
			return acl
		}
		// allow present but the wrong shape (malformed): cannot prove the
		// allowlist — unknown, never widened to unrestricted.
		acl.State = ACLUnknown
		acl.Complete = false
		return acl
	}
	if allow == nil {
		acl.State = ACLUnrestricted
		acl.Complete = true
		return acl
	}

	if enabled, present := allow["isEnabled"].(bool); present && !enabled {
		// Authoritative disabled allowlist: unrestricted, and any previously
		// published channel list must be dropped (the snapshot swap does this).
		acl.State = ACLUnrestricted
		acl.Complete = true
		return acl
	}

	channels, ok := allow["channels"].([]interface{})
	if !ok || channels == nil {
		// allow present but channels missing/malformed: cannot prove the
		// allowlist, so unknown (fail closed) — never widen to unrestricted.
		acl.State = ACLUnknown
		acl.Complete = false
		return acl
	}

	ids := make([]string, 0, len(channels))
	seen := make(map[string]struct{}, len(channels))
	malformed := false
	for _, ch := range channels {
		chMap, ok := ch.(map[string]interface{})
		if !ok {
			malformed = true
			continue
		}
		id, ok := chMap["id"].(string)
		if !ok || strings.TrimSpace(id) == "" {
			malformed = true
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	if len(ids) == 0 {
		// Enabled but no usable channels: incomplete/unknown, not unrestricted.
		acl.State = ACLUnknown
		acl.Complete = false
		return acl
	}

	sort.Strings(ids)
	acl.State = ACLRestricted
	acl.ChannelIDs = ids
	// The verified DropCampaignDetails operation returns allow.channels as a
	// FLAT array (no pageInfo/cursor in the proven contract), so a successfully
	// parsed non-empty list is complete. A malformed element does not silently
	// truncate a "complete" allowlist — mark it incomplete so it is treated as
	// not-fully-loaded rather than a shorter authoritative list.
	acl.Complete = !malformed
	return acl
}

// ReconcileACL returns the ACL that should remain published given the
// previously-published ACL and a freshly-observed one, encoding the lifecycle
// rules so a transient failure or a stale race can never widen or erode access:
//
//   - a strictly OLDER observation never overwrites a newer one (stale guard);
//   - an incoming ACLUnknown never replaces a known (restricted/unrestricted)
//     published ACL — the last-known-good is preserved rather than dropped or
//     widened on a transient failure;
//   - an incoming INCOMPLETE restricted ACL never replaces a complete published
//     one (no partial publish of a half-loaded allowlist);
//   - otherwise the fresh observation wins, INCLUDING an authoritative
//     isEnabled=false (ACLUnrestricted) that must clear a stale restricted list.
func ReconcileACL(published, incoming CampaignACL) CampaignACL {
	if !published.ObservedAt.IsZero() && !incoming.ObservedAt.IsZero() &&
		incoming.ObservedAt.Before(published.ObservedAt) {
		return published
	}
	if published.State != ACLUnknown || published.Source != ACLSourceNone {
		if incoming.State == ACLUnknown {
			return published
		}
		if incoming.State == ACLRestricted && !incoming.Complete && published.Complete {
			return published
		}
	}
	return incoming
}

// effectiveACLState returns the campaign's decisive ACL state, consulting the
// authoritative typed ACL when it was populated from a Twitch response and
// falling back to the legacy Channels slice for directly-constructed campaigns
// (tests / legacy call sites). This is the single arbiter the restriction
// helpers use.
func (c *Campaign) effectiveACLState() CampaignACLState {
	if c.ACL.Source != ACLSourceNone || c.ACL.State != ACLUnknown {
		return c.ACL.State
	}
	if len(c.Channels) > 0 {
		return ACLRestricted
	}
	return ACLUnrestricted
}

// effectiveACLChannels returns the authoritative channel-ID set when the ACL was
// populated, otherwise the legacy Channels slice.
func (c *Campaign) effectiveACLChannels() []string {
	if c.ACL.Source != ACLSourceNone {
		return c.ACL.ChannelIDs
	}
	return c.Channels
}

// ACLState exposes the campaign's authoritative ACL state for diagnostics and
// eligibility gating.
func (c *Campaign) ACLState() CampaignACLState { return c.effectiveACLState() }

// ACLComplete reports whether the campaign's allowlist is fully loaded. An
// unrestricted or legacy campaign is trivially complete; a restricted one is
// complete only when its channel set was fully parsed.
func (c *Campaign) ACLComplete() bool {
	if c.ACL.Source == ACLSourceNone {
		return true
	}
	return c.ACL.Complete
}

// AllowedChannelCount returns the number of channels in a restricted ACL (0 for
// unrestricted/unknown), for privacy-safe diagnostics.
func (c *Campaign) AllowedChannelCount() int {
	if c.effectiveACLState() != ACLRestricted {
		return 0
	}
	return len(c.effectiveACLChannels())
}
