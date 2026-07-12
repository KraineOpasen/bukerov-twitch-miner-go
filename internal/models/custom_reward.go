package models

import "time"

// CustomReward is a channel-specific channel-points reward that a viewer can
// redeem by spending points (the personal rewards a streamer configures, not
// Community Goals — those are handled separately by CommunityGoal). It mirrors
// the fields Twitch returns for each entry in
// community.channel.communityPointsSettings.customRewards.
type CustomReward struct {
	ID    string
	Title string
	// Prompt is the streamer-provided description shown to viewers. Twitch
	// requires it to be echoed back unchanged when redeeming (part of the
	// anti-tamper PROPERTIES_MISMATCH check), so it is always tracked.
	Prompt string
	Cost   int

	// IsUserInputRequired marks rewards that ask the viewer for a free-text
	// message. These are only ever redeemable manually — the miner never
	// auto-redeems them because it cannot meaningfully author that text.
	IsUserInputRequired bool

	IsEnabled bool
	IsPaused  bool
	IsInStock bool
	IsSubOnly bool

	BackgroundColor string
	ImageURL        string

	// CooldownExpiresAt is when a global-cooldown reward becomes redeemable
	// again; zero when the reward is not currently on cooldown.
	CooldownExpiresAt time.Time
}

// IsAvailable reports whether the reward can be redeemed right now: it must be
// enabled, not paused, in stock, and past any active global cooldown. Sub-only
// gating and per-stream/per-user limits are enforced by Twitch at redeem time
// (surfaced as error codes) rather than pre-filtered here.
func (r *CustomReward) IsAvailable() bool {
	if !r.IsEnabled || r.IsPaused || !r.IsInStock {
		return false
	}
	if !r.CooldownExpiresAt.IsZero() && r.CooldownExpiresAt.After(time.Now()) {
		return false
	}
	return true
}

// CustomRewardFromGQL builds a CustomReward from a single customRewards entry
// of a ChannelPointsContext GraphQL response. It defends against every field
// being absent or the wrong type so a partial/unexpected payload yields a
// zero-valued reward rather than panicking.
func CustomRewardFromGQL(data map[string]interface{}) *CustomReward {
	reward := &CustomReward{}

	if id, ok := data["id"].(string); ok {
		reward.ID = id
	}
	if title, ok := data["title"].(string); ok {
		reward.Title = title
	}
	if prompt, ok := data["prompt"].(string); ok {
		reward.Prompt = prompt
	}
	if cost, ok := data["cost"].(float64); ok {
		reward.Cost = int(cost)
	}
	if v, ok := data["isUserInputRequired"].(bool); ok {
		reward.IsUserInputRequired = v
	}
	if v, ok := data["isEnabled"].(bool); ok {
		reward.IsEnabled = v
	}
	if v, ok := data["isPaused"].(bool); ok {
		reward.IsPaused = v
	}
	if v, ok := data["isInStock"].(bool); ok {
		reward.IsInStock = v
	}
	if v, ok := data["isSubOnly"].(bool); ok {
		reward.IsSubOnly = v
	}
	if v, ok := data["backgroundColor"].(string); ok {
		reward.BackgroundColor = v
	}
	if expires, ok := data["cooldownExpiresAt"].(string); ok && expires != "" {
		if t, err := time.Parse(time.RFC3339, expires); err == nil {
			reward.CooldownExpiresAt = t
		}
	}

	reward.ImageURL = customRewardImageURL(data)

	return reward
}

// customRewardImageURL picks the best available icon URL for a reward,
// preferring the streamer's custom image over Twitch's default set, and the
// 2x resolution when present.
func customRewardImageURL(data map[string]interface{}) string {
	for _, key := range []string{"image", "defaultImage"} {
		img, ok := data[key].(map[string]interface{})
		if !ok || img == nil {
			continue
		}
		for _, res := range []string{"url2x", "url4x", "url1x"} {
			if u, ok := img[res].(string); ok && u != "" {
				return u
			}
		}
	}
	return ""
}
