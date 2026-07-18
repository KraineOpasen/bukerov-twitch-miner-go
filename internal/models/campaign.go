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
	ID          string
	Name        string
	Game        *Game
	Status      CampaignStatus
	StartAt     time.Time
	EndAt       time.Time
	Channels    []string
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

	if allow, ok := data["allow"].(map[string]interface{}); ok {
		if channels, ok := allow["channels"].([]interface{}); ok && channels != nil {
			for _, ch := range channels {
				if chMap, ok := ch.(map[string]interface{}); ok {
					if id, ok := chMap["id"].(string); ok {
						c.Channels = append(c.Channels, id)
					}
				}
			}
		}
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

// IsChannelRestricted reports whether this campaign only credits watch time
// on a specific set of channels (Twitch's `allow.channels`), as opposed to
// crediting progress on any channel streaming the campaign's game.
func (c *Campaign) IsChannelRestricted() bool {
	return len(c.Channels) > 0
}

// AllowsChannel reports whether channelID is eligible to progress this
// campaign. Unrestricted campaigns (IsChannelRestricted() == false) allow
// any channel.
func (c *Campaign) AllowsChannel(channelID string) bool {
	if !c.IsChannelRestricted() {
		return true
	}
	for _, id := range c.Channels {
		if id == channelID {
			return true
		}
	}
	return false
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

// ApplyClaimHistory drops any reward already present in claimedRewards
// (keyed by Drop.RewardKey) from this campaign's watchable drops. Those keys
// represent rewards Twitch's account-wide claim history has already
// confirmed as granted, which covers recurring/regional variants of this
// same campaign -- ones sharing the same reward name and game but a
// different campaign or drop ID -- so their watch time isn't wasted again.
// It records what was skipped and marks the campaign fully claimed once
// nothing watchable is left.
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
				if drop.IsClaimable && claimFunc != nil {
					drop.IsClaimed = claimFunc(drop)
				}
				break
			}
		}
	}
}
