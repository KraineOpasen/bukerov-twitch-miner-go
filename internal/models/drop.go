package models

import (
	"strings"
	"time"
)

type Drop struct {
	ID                    string
	Name                  string
	Benefit               string
	MinutesRequired       int
	CurrentMinutesWatched int
	PercentageProgress    int
	HasPreconditionsMet   *bool
	DropInstanceID        string
	IsClaimable           bool
	IsClaimed             bool
	StartAt               time.Time
	EndAt                 time.Time
}

func NewDropFromGQL(data map[string]interface{}) *Drop {
	drop := &Drop{}

	if id, ok := data["id"].(string); ok {
		drop.ID = id
	}
	if name, ok := data["name"].(string); ok {
		drop.Name = name
	}

	if benefitEdges, ok := data["benefitEdges"].([]interface{}); ok && len(benefitEdges) > 0 {
		if edge, ok := benefitEdges[0].(map[string]interface{}); ok {
			if benefit, ok := edge["benefit"].(map[string]interface{}); ok {
				if name, ok := benefit["name"].(string); ok {
					drop.Benefit = name
				}
			}
		}
	}

	if mins, ok := data["requiredMinutesWatched"].(float64); ok {
		drop.MinutesRequired = int(mins)
	}

	if startAt, ok := data["startAt"].(string); ok {
		if t, err := time.Parse(time.RFC3339, startAt); err == nil {
			drop.StartAt = t
		}
	}
	if endAt, ok := data["endAt"].(string); ok {
		if t, err := time.Parse(time.RFC3339, endAt); err == nil {
			drop.EndAt = t
		}
	}

	return drop
}

func (d *Drop) Update(selfData map[string]interface{}) {
	if mins, ok := selfData["currentMinutesWatched"].(float64); ok {
		d.CurrentMinutesWatched = int(mins)
	}
	if hasPre, ok := selfData["hasPreconditionsMet"].(bool); ok {
		d.HasPreconditionsMet = &hasPre
	}
	if instanceID, ok := selfData["dropInstanceID"].(string); ok {
		d.DropInstanceID = instanceID
	}
	if claimed, ok := selfData["isClaimed"].(bool); ok {
		d.IsClaimed = claimed
	}

	if d.MinutesRequired > 0 {
		d.PercentageProgress = (d.CurrentMinutesWatched * 100) / d.MinutesRequired
	}

	d.IsClaimable = d.DropInstanceID != "" &&
		!d.IsClaimed &&
		d.CurrentMinutesWatched >= d.MinutesRequired
}

func (d *Drop) DateTimeMatch() bool {
	now := time.Now()
	return d.StartAt.Before(now) && d.EndAt.After(now)
}

func (d *Drop) IsPrintable() bool {
	return !d.IsClaimed && d.CurrentMinutesWatched > 0 && d.CurrentMinutesWatched < d.MinutesRequired
}

// RewardKey normalizes this drop's reward identity by game + drop name, so
// recurring/regional campaign variants that grant the identical reward under
// a different (and sometimes colliding) campaign or drop ID are recognized
// as the same reward rather than relying on Twitch's raw IDs.
func (d *Drop) RewardKey(gameID string) string {
	return NormalizeRewardKey(gameID, d.Name)
}

// NormalizeRewardKey builds a stable, case/whitespace-insensitive identifier
// for a drop reward from its game and display name.
func NormalizeRewardKey(gameID, name string) string {
	return strings.ToLower(strings.TrimSpace(gameID)) + "::" + strings.ToLower(strings.TrimSpace(name))
}
