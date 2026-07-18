package models

import (
	"strings"
	"time"
)

type Drop struct {
	ID                    string
	Name                  string
	Benefit               string
	ImageURL              string
	MinutesRequired       int
	CurrentMinutesWatched int
	PercentageProgress    int
	HasPreconditionsMet   *bool
	DropInstanceID        string
	IsClaimable           bool
	IsClaimed             bool
	StartAt               time.Time
	EndAt                 time.Time

	// SubscriberOnly is Twitch's best-effort subscriber-only flag for the drop;
	// SubscriberOnlyKnown records whether Twitch actually reported it. The
	// campaign policy's "Ignore subscriber-only" control honestly shows "no
	// effect" when the flag is not known, rather than pretending to gate on
	// data the API never provided.
	SubscriberOnly      bool
	SubscriberOnlyKnown bool
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
				// imageAssetURL is the reward's own artwork; the Drops-page
				// modal uses it as each drop's icon, falling back to the
				// campaign box art when Twitch omits it.
				if img, ok := benefit["imageAssetURL"].(string); ok {
					drop.ImageURL = img
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

	// Best-effort subscriber-only flag: Twitch does not reliably expose it on
	// time-based drops, so try a couple of plausible keys and only mark it
	// "known" when one is actually present.
	for _, key := range []string{"isSubscriberOnly", "isSubscriptionOnly", "subscriberOnly"} {
		if v, ok := data[key].(bool); ok {
			drop.SubscriberOnly = v
			drop.SubscriberOnlyKnown = true
			break
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

// HasDateWindow reports whether Twitch supplied a per-drop date window (either
// bound present). Inventory dropCampaignsInProgress entries frequently omit
// per-drop startAt/endAt, so a drop can be legitimately live with no window.
func (d *Drop) HasDateWindow() bool {
	return !d.StartAt.IsZero() || !d.EndAt.IsZero()
}

// InActiveWindow reports whether the drop should be treated as currently active.
// A drop whose window Twitch actually supplied must be inside it; a drop with NO
// window (both bounds zero — the common inventory-recovery shape) is treated as
// active. Membership in the inventory's in-progress list already proves Twitch
// considers the drop live, so discarding it merely for a missing window is the
// silent loss that made inventory-recovered campaigns flap in and out of the
// tracked set between the full sync (which keeps them) and the lightweight
// progress sync (which used to strip them via ClearClaimedDrops).
func (d *Drop) InActiveWindow() bool {
	if !d.HasDateWindow() {
		return true
	}
	return d.DateTimeMatch()
}

// ClampedProgress returns this drop's watch progress as a 0-100 percentage,
// clamped so an over-watched drop (Twitch occasionally reports more minutes
// than required) never renders past a full bar.
func (d *Drop) ClampedProgress() int {
	if d.MinutesRequired <= 0 {
		return 0
	}
	pct := (d.CurrentMinutesWatched * 100) / d.MinutesRequired
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}

// MinutesRemaining reports how many more watched minutes this drop needs
// before its reward unlocks (never negative).
func (d *Drop) MinutesRemaining() int {
	remaining := d.MinutesRequired - d.CurrentMinutesWatched
	if remaining < 0 {
		return 0
	}
	return remaining
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
