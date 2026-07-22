package models

import (
	"strings"
	"time"
)

type CampaignStatus string

const (
	CampaignActive  CampaignStatus = "ACTIVE"
	CampaignExpired CampaignStatus = "EXPIRED"
)

// CampaignClaimStatus distinguishes a campaign that's still watchable from
// one whose reward has already been fully granted, so the dashboard can
// eventually surface the two states separately.
type CampaignClaimStatus string

const (
	CampaignClaimStatusInProgress     CampaignClaimStatus = "in_progress"
	CampaignClaimStatusAlreadyClaimed CampaignClaimStatus = "already_claimed"
)

type Campaign struct {
	ID      string
	Name    string
	Game    *Game
	Status  CampaignStatus
	StartAt time.Time
	EndAt   time.Time
	// Channels is a DERIVED compatibility mirror of ACL.ChannelIDs, retained so
	// legacy readers/tests that construct a Campaign with Channels directly (and
	// the ones that read it) keep working. New code should read ACL / the
	// restriction helpers (IsChannelRestricted, AllowsChannel, ACLState), which
	// treat the typed ACL as the single source of truth and fall back to this
	// slice only for directly-constructed campaigns.
	Channels []string
	// ACL is the authoritative, typed channel access-control state, populated by
	// NewCampaignFromGQL from the campaign's `allow` block. See CampaignACL and
	// campaign_acl.go for the response semantics.
	ACL         CampaignACL
	InInventory bool
	Drops       []*Drop
	DateMatch   bool

	// ClaimStatus and ClaimedDropNames are populated by ApplyClaimHistory
	// and reflect the account's Twitch-wide claim history rather than just
	// this campaign instance's own progress.
	ClaimStatus      CampaignClaimStatus
	ClaimedDropNames []string
}

func NewCampaignFromGQL(data map[string]interface{}) *Campaign {
	c := &Campaign{
		Drops:       make([]*Drop, 0),
		ClaimStatus: CampaignClaimStatusInProgress,
	}

	if id, ok := data["id"].(string); ok {
		c.ID = id
	}
	if name, ok := data["name"].(string); ok {
		c.Name = name
	}
	if status, ok := data["status"].(string); ok {
		c.Status = CampaignStatus(status)
	}

	if gameData, ok := data["game"].(map[string]interface{}); ok {
		c.Game = &Game{}
		if id, ok := gameData["id"].(string); ok {
			c.Game.ID = id
		}
		if name, ok := gameData["name"].(string); ok {
			c.Game.Name = name
		}
		if displayName, ok := gameData["displayName"].(string); ok {
			c.Game.DisplayName = displayName
		}
	}

	if startAt, ok := data["startAt"].(string); ok {
		if t, err := time.Parse(time.RFC3339, startAt); err == nil {
			c.StartAt = t
		}
	}
	if endAt, ok := data["endAt"].(string); ok {
		if t, err := time.Parse(time.RFC3339, endAt); err == nil {
			c.EndAt = t
		}
	}

	now := time.Now()
	c.DateMatch = c.StartAt.Before(now) && c.EndAt.After(now)

	// Build the authoritative typed ACL from the raw `allow` block (handles
	// isEnabled, missing/malformed channels, dedup and deterministic order), then
	// mirror the restricted channel set into the legacy Channels slice for
	// backward-compatible readers.
	c.ACL = buildCampaignACL(data["allow"], now)
	if c.ACL.State == ACLRestricted {
		c.Channels = append([]string(nil), c.ACL.ChannelIDs...)
	}

	if drops, ok := data["timeBasedDrops"].([]interface{}); ok {
		for _, d := range drops {
			if dropData, ok := d.(map[string]interface{}); ok {
				c.Drops = append(c.Drops, NewDropFromGQL(dropData))
			}
		}
	}

	return c
}

// IsChannelRestricted reports whether this campaign is authoritatively known to
// credit watch time only on a specific set of channels. It is true ONLY for
// ACLRestricted — an ACLUnknown campaign is not reported as restricted (its
// restriction is unproven), so it never gains restricted-drop slot priority.
// Crediting eligibility itself is decided by AllowsChannel, which fails closed
// on unknown, so an unknown ACL can never widen access.
func (c *Campaign) IsChannelRestricted() bool {
	return c.effectiveACLState() == ACLRestricted
}

// AllowsChannel reports whether channelID may progress this campaign. It is the
// single crediting gate:
//   - ACLUnrestricted => any channel;
//   - ACLRestricted   => exact membership in the allowlist;
//   - ACLUnknown      => false (fail closed: never credit toward a campaign
//     whose allowlist could not be authoritatively loaded).
func (c *Campaign) AllowsChannel(channelID string) bool {
	switch c.effectiveACLState() {
	case ACLUnrestricted:
		return true
	case ACLRestricted:
		for _, id := range c.effectiveACLChannels() {
			if id == channelID {
				return true
			}
		}
		return false
	default: // ACLUnknown
		return false
	}
}

// Clone returns a copy of the campaign safe to mutate independently of the
// original: the Drops slice and each Drop are copied (as are the Channels and
// ClaimedDropNames slices) so watch progress can be advanced on the copy
// without touching a campaign object other goroutines may still be reading
// lock-free (the Drops page and directory discovery rely on published
// campaigns staying immutable after they're swapped into the tracker). Game is
// shared: it is treated as immutable and is never mutated in place.
func (c *Campaign) Clone() *Campaign {
	clone := *c
	if c.Drops != nil {
		clone.Drops = make([]*Drop, len(c.Drops))
		for i, d := range c.Drops {
			dc := *d
			clone.Drops[i] = &dc
		}
	}
	if c.Channels != nil {
		clone.Channels = append([]string(nil), c.Channels...)
	}
	if c.ClaimedDropNames != nil {
		clone.ClaimedDropNames = append([]string(nil), c.ClaimedDropNames...)
	}
	// Deep-copy the typed ACL slice so a published snapshot's allowlist is never
	// mutated through a clone (immutable-snapshot guarantee).
	clone.ACL = c.ACL.clone()
	return &clone
}

// ClearClaimedDrops keeps only the campaign's still-active, unclaimed drops. A
// drop is kept when it is unclaimed AND within its active window — where a drop
// with no window supplied (the common inventory shape) counts as active (see
// Drop.InActiveWindow). Using InActiveWindow rather than the bare DateTimeMatch
// is what stops the lightweight progress sync from stripping an inventory-
// recovered campaign's date-less drops and dropping the whole campaign out of
// the tracked set (and out of directory discovery) between full syncs.
func (c *Campaign) ClearClaimedDrops() {
	validDrops := make([]*Drop, 0, len(c.Drops))
	for _, drop := range c.Drops {
		if drop.InActiveWindow() && !drop.IsClaimed {
			validDrops = append(validDrops, drop)
		}
	}
	c.Drops = validDrops
}

// campaignWindow returns the campaign-level entitlement window used as the
// occurrence fallback for drops that carry no per-drop dates (the common
// inventory shape). A campaign present in the inventory but carrying no dates of
// its own yields an inventory-sourced, not-Known window: server evidence it is
// live now, never proof of a historical occurrence identity.
func (c *Campaign) campaignWindow() EntitlementWindow {
	if c.StartAt.IsZero() && c.EndAt.IsZero() {
		src := WindowSourceNone
		if c.InInventory {
			src = WindowSourceInventory
		}
		return EntitlementWindow{Source: src, Known: false}
	}
	return EntitlementWindow{
		Start:  c.StartAt,
		End:    c.EndAt,
		Source: WindowSourceCampaign,
		Known:  true,
	}
}

// ClaimHistoryOutcome summarizes what an ApplyClaimHistoryRecords pass did, so
// the pipeline can log confirmed removals and (throttled) ambiguous retentions
// without the domain layer taking on any logging responsibility.
type ClaimHistoryOutcome struct {
	// ConfirmedNames are drops removed because claim history authoritatively
	// matched them for the same entitlement occurrence.
	ConfirmedNames []string
	// AmbiguousNames are drops KEPT despite a weak (name-only / unknown-window)
	// claim-history signal — fail open. These must never be labelled claimed.
	AmbiguousNames []string
}

// ApplyClaimHistoryRecords is the evidence-aware, authoritative replacement for
// the name-only ApplyClaimHistory. It removes a drop only on a positive,
// provable identity match (MatchIdentity == Confirmed, or the strict
// uniqueness+overlapping-window fallback below); every ambiguous signal fails
// open — the drop is retained and NEVER labelled already-claimed — and the
// actual claim mutation stays independently gated by Drop.CanClaim. clock is
// injectable so window classification is deterministic in tests.
//
// The strict fallback upgrades an otherwise-Ambiguous name match to Confirmed
// only when ALL hold: the claimed record's window and the candidate's window are
// both decidable and overlap (proving the same occurrence), and the candidate's
// normalized name is unique among this campaign's drops (so there is no tier
// ambiguity). This is the only way a name can confirm a claim — a bare game+name
// match, a missing game, or an unknown window can never remove a drop.
func (c *Campaign) ApplyClaimHistoryRecords(records []ClaimedReward, clock Clock) ClaimHistoryOutcome {
	gameID := ""
	if c.Game != nil {
		gameID = c.Game.ID
	}
	fallback := c.campaignWindow()

	nameCount := make(map[string]int, len(c.Drops))
	for _, d := range c.Drops {
		nameCount[canonicalName(d.Name)]++
	}

	var outcome ClaimHistoryOutcome
	kept := make([]*Drop, 0, len(c.Drops))
	for _, drop := range c.Drops {
		cand := drop.Identity(gameID, c.ID, fallback)
		confirmed, ambiguous := classifyAgainstClaimHistory(cand, records, nameCount, clock)
		if confirmed {
			c.ClaimedDropNames = append(c.ClaimedDropNames, drop.Name)
			outcome.ConfirmedNames = append(outcome.ConfirmedNames, drop.Name)
			continue
		}
		if ambiguous {
			outcome.AmbiguousNames = append(outcome.AmbiguousNames, drop.Name)
		}
		kept = append(kept, drop)
	}
	c.Drops = kept

	if len(c.Drops) == 0 && len(c.ClaimedDropNames) > 0 {
		c.ClaimStatus = CampaignClaimStatusAlreadyClaimed
	} else {
		c.ClaimStatus = CampaignClaimStatusInProgress
	}
	return outcome
}

// strictNameFallbackConfirms decides whether an otherwise-Ambiguous name match
// may be upgraded to Confirmed. It requires the FULL evidence set — anything
// missing keeps the drop retained (fail open):
//
//  1. both game IDs present and equal (a missing game on either side => never);
//  2. the candidate's normalized name is unique among the campaign's drops;
//  3. both windows decidable AND overlapping (same occurrence).
//
// This mirrors the domain contract that "missing game ID + name only" is always
// Ambiguous, never Confirmed.
func strictNameFallbackConfirms(claimed, cand RewardIdentity, nameCount map[string]int) bool {
	cg, dg := canonicalGame(claimed.GameID), canonicalGame(cand.GameID)
	if cg == "" || dg == "" || cg != dg {
		return false
	}
	if cand.CanonicalName == "" || nameCount[cand.CanonicalName] != 1 {
		return false
	}
	return claimed.Window.Decidable() && cand.Window.Decidable() &&
		claimed.Window.Overlaps(cand.Window)
}

// classifyAgainstClaimHistory returns whether a candidate drop is confirmed
// claimed by any record, and whether it saw an ambiguous (retained) signal.
func classifyAgainstClaimHistory(cand RewardIdentity, records []ClaimedReward, nameCount map[string]int, clock Clock) (confirmed, ambiguous bool) {
	for _, rec := range records {
		switch MatchIdentity(rec.Identity, cand, clock) {
		case IdentityMatchConfirmed:
			return true, false
		case IdentityMatchAmbiguous:
			if strictNameFallbackConfirms(rec.Identity, cand, nameCount) {
				return true, false
			}
			ambiguous = true
		}
	}
	return false, ambiguous
}

// ApplyClaimHistory is a retained NON-AUTHORITATIVE compatibility helper keyed
// by the lossy game+name RewardKey. New code MUST use ApplyClaimHistoryRecords,
// which keeps distinct tiers/benefits/occurrences apart and fails open on
// ambiguity. This shim survives only for legacy callers/tests that still pass a
// pre-built name key set; it must not be treated as the authoritative matcher.
func (c *Campaign) ApplyClaimHistory(claimedRewards map[string]bool) {
	gameID := ""
	if c.Game != nil {
		gameID = c.Game.ID
	}

	kept := make([]*Drop, 0, len(c.Drops))
	for _, drop := range c.Drops {
		if claimedRewards[drop.RewardKey(gameID)] {
			c.ClaimedDropNames = append(c.ClaimedDropNames, drop.Name)
			continue
		}
		kept = append(kept, drop)
	}
	c.Drops = kept

	if len(c.Drops) == 0 && len(c.ClaimedDropNames) > 0 {
		c.ClaimStatus = CampaignClaimStatusAlreadyClaimed
	} else {
		c.ClaimStatus = CampaignClaimStatusInProgress
	}
}

// MatchesBlacklist reports whether any of this campaign's remaining drops has a
// drop name or reward (benefit) name containing one of the given keywords,
// matched case-insensitively as a substring. Blank keywords are ignored. It
// returns the trimmed keyword that matched and the drop/reward name it matched
// against, so callers can log precisely why a campaign was excluded from drop
// rotation. This is an additional exclusion condition alongside the
// claim-history dedup in ApplyClaimHistory.
func (c *Campaign) MatchesBlacklist(keywords []string) (keyword, dropName string, matched bool) {
	normalized := make([]string, 0, len(keywords))
	for _, raw := range keywords {
		if kw := strings.ToLower(strings.TrimSpace(raw)); kw != "" {
			normalized = append(normalized, kw)
		}
	}
	if len(normalized) == 0 {
		return "", "", false
	}

	for _, drop := range c.Drops {
		name := strings.ToLower(drop.Name)
		benefit := strings.ToLower(drop.Benefit)
		for _, kw := range normalized {
			if strings.Contains(name, kw) {
				return kw, drop.Name, true
			}
			if strings.Contains(benefit, kw) {
				return kw, drop.Benefit, true
			}
		}
	}
	return "", "", false
}

// CurrentDrop returns the drop the campaign is actively working toward: the
// unclaimed drop with the lowest remaining watch requirement that isn't met
// yet (the next reward to unlock). If every remaining drop's threshold is
// already met, it returns the furthest milestone. Returns nil when there are
// no remaining drops (e.g. an already-claimed campaign).
func (c *Campaign) CurrentDrop() *Drop {
	if len(c.Drops) == 0 {
		return nil
	}

	var current *Drop
	for _, d := range c.Drops {
		if d.CurrentMinutesWatched < d.MinutesRequired {
			if current == nil || d.MinutesRequired < current.MinutesRequired {
				current = d
			}
		}
	}
	if current != nil {
		return current
	}

	// All thresholds met: fall back to the furthest milestone.
	return c.FinalDrop()
}

// FinalDrop returns the campaign's furthest milestone — the remaining drop
// with the largest required watch time — or nil when there are no drops.
func (c *Campaign) FinalDrop() *Drop {
	var final *Drop
	for _, d := range c.Drops {
		if final == nil || d.MinutesRequired > final.MinutesRequired {
			final = d
		}
	}
	return final
}

// OverallProgressPercent reports the campaign's progress toward its full
// reward as a 0-100 percentage, measured against the furthest milestone. An
// already-claimed campaign reports 100.
func (c *Campaign) OverallProgressPercent() int {
	if c.ClaimStatus == CampaignClaimStatusAlreadyClaimed {
		return 100
	}
	final := c.FinalDrop()
	if final == nil {
		return 0
	}
	return final.ClampedProgress()
}

func (c *Campaign) SyncDrops(inventoryDrops []interface{}, claimFunc func(*Drop) bool) {
	for _, invDrop := range inventoryDrops {
		dropData, ok := invDrop.(map[string]interface{})
		if !ok {
			continue
		}
		dropID, ok := dropData["id"].(string)
		if !ok {
			continue
		}

		for _, drop := range c.Drops {
			if drop.ID == dropID {
				if selfData, ok := dropData["self"].(map[string]interface{}); ok {
					drop.Update(selfData)
				}
				// Claim only on the authoritative server signal (CanClaim), never
				// on locally-counted watch minutes. claimFunc reports whether the
				// drop is now in a reconciled claimed state.
				if drop.CanClaim() && claimFunc != nil {
					drop.IsClaimed = claimFunc(drop)
				}
				break
			}
		}
	}
}
