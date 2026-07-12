package settings

import "github.com/PatrickWalther/twitch-miner-go/internal/models"

// StreamerSettingsToDTO converts model settings to the DTO format (all fields populated).
func StreamerSettingsToDTO(s models.StreamerSettings) StreamerSettingsConfig {
	chat := string(s.Chat)
	strategy := string(s.Bet.Strategy)
	delayMode := string(s.Bet.DelayMode)
	preference := string(s.Preference)

	return StreamerSettingsConfig{
		MakePredictions:          &s.MakePredictions,
		FollowRaid:               &s.FollowRaid,
		ClaimDrops:               &s.ClaimDrops,
		ClaimMoments:             &s.ClaimMoments,
		WatchStreak:              &s.WatchStreak,
		CommunityGoals:           &s.CommunityGoals,
		CommunityGoalsMaxPercent: &s.CommunityGoalsMaxPercent,
		CommunityGoalsMaxAmount:  &s.CommunityGoalsMaxAmount,
		Chat:                     &chat,
		Preference:               &preference,
		Bet: &BetSettingsJSON{
			Strategy:      &strategy,
			Percentage:    &s.Bet.Percentage,
			PercentageGap: &s.Bet.PercentageGap,
			MaxPoints:     &s.Bet.MaxPoints,
			MinimumPoints: &s.Bet.MinimumPoints,
			StealthMode:   &s.Bet.StealthMode,
			Delay:         &s.Bet.Delay,
			DelayMode:     &delayMode,
		},
	}
}

// StreamerSettingsPtrToDTO converts a pointer to model settings to a DTO pointer.
func StreamerSettingsPtrToDTO(s *models.StreamerSettings) *StreamerSettingsConfig {
	if s == nil {
		return nil
	}
	dto := StreamerSettingsToDTO(*s)
	return &dto
}

// StreamerSettingsFromDTO converts a DTO to model settings, starting from defaults.
func StreamerSettingsFromDTO(s StreamerSettingsConfig) models.StreamerSettings {
	settings := models.DefaultStreamerSettings()
	ApplyStreamerSettingsFromDTO(&settings, s)
	return settings
}

// StreamerSettingsPtrFromDTO converts a DTO pointer to model settings pointer.
func StreamerSettingsPtrFromDTO(s *StreamerSettingsConfig) *models.StreamerSettings {
	if s == nil {
		return nil
	}
	settings := StreamerSettingsFromDTO(*s)
	return &settings
}

// ApplyStreamerSettingsFromDTO applies non-nil fields from the DTO to model settings.
func ApplyStreamerSettingsFromDTO(dst *models.StreamerSettings, src StreamerSettingsConfig) {
	if src.MakePredictions != nil {
		dst.MakePredictions = *src.MakePredictions
	}
	if src.FollowRaid != nil {
		dst.FollowRaid = *src.FollowRaid
	}
	if src.ClaimDrops != nil {
		dst.ClaimDrops = *src.ClaimDrops
	}
	if src.ClaimMoments != nil {
		dst.ClaimMoments = *src.ClaimMoments
	}
	if src.WatchStreak != nil {
		dst.WatchStreak = *src.WatchStreak
	}
	if src.CommunityGoals != nil {
		dst.CommunityGoals = *src.CommunityGoals
	}
	if src.CommunityGoalsMaxPercent != nil {
		dst.CommunityGoalsMaxPercent = *src.CommunityGoalsMaxPercent
	}
	if src.CommunityGoalsMaxAmount != nil {
		dst.CommunityGoalsMaxAmount = *src.CommunityGoalsMaxAmount
	}
	if src.Chat != nil {
		dst.Chat = models.ChatPresence(*src.Chat)
	}
	if src.Preference != nil {
		dst.Preference = models.Preference(*src.Preference)
	}
	if src.Bet != nil {
		ApplyBetSettingsFromDTO(&dst.Bet, src.Bet)
	}
}

// ApplyBetSettingsFromDTO applies non-nil bet fields from the DTO to model settings.
func ApplyBetSettingsFromDTO(dst *models.BetSettings, src *BetSettingsJSON) {
	if src.Strategy != nil {
		dst.Strategy = models.Strategy(*src.Strategy)
	}
	if src.Percentage != nil {
		dst.Percentage = *src.Percentage
	}
	if src.PercentageGap != nil {
		dst.PercentageGap = *src.PercentageGap
	}
	if src.MaxPoints != nil {
		dst.MaxPoints = *src.MaxPoints
	}
	if src.MinimumPoints != nil {
		dst.MinimumPoints = *src.MinimumPoints
	}
	if src.StealthMode != nil {
		dst.StealthMode = *src.StealthMode
	}
	if src.Delay != nil {
		dst.Delay = *src.Delay
	}
	if src.DelayMode != nil {
		dst.DelayMode = models.DelayMode(*src.DelayMode)
	}
}
