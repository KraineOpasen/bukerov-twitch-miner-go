package models

import "time"

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

func (c *Campaign) ClearClaimedDrops() {
	validDrops := make([]*Drop, 0)
	for _, drop := range c.Drops {
		if drop.DateTimeMatch() && !drop.IsClaimed {
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
