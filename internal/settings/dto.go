package settings

// RuntimeSettings is the JSON shape exchanged with the analytics UI for configuration.
// It contains all settings that can be modified at runtime, including streamers,
// priorities, rate limits, logger, and analytics display settings.
type RuntimeSettings struct {
	Streamers       []StreamerConfig       `json:"streamers"`
	DefaultSettings StreamerSettingsConfig `json:"defaultSettings"`
	Priority        []string               `json:"priority"`
	RateLimits      RateLimitSettings      `json:"rateLimits"`
	Logger          LoggerSettings         `json:"logger"`
	Analytics       AnalyticsUIConfig      `json:"analytics"`
	Discord         DiscordUIConfig        `json:"discord"`
}

// DiscordUIConfig contains Discord integration settings for the UI.
type DiscordUIConfig struct {
	Enabled  bool   `json:"enabled"`
	BotToken string `json:"botToken"`
	GuildID  string `json:"guildId"`
}

// RateLimitSettings contains timing intervals for various miner operations.
type RateLimitSettings struct {
	WebsocketPingInterval int     `json:"websocketPingInterval"`
	CampaignSyncInterval  int     `json:"campaignSyncInterval"`
	MinuteWatchedInterval int     `json:"minuteWatchedInterval"`
	RequestDelay          float64 `json:"requestDelay"`
	ReconnectDelay        int     `json:"reconnectDelay"`
	StreamCheckInterval   int     `json:"streamCheckInterval"`

	RotationIntervalMinMinutes int `json:"rotationIntervalMinMinutes"`
	RotationIntervalMaxMinutes int `json:"rotationIntervalMaxMinutes"`
}

// LoggerSettings contains logging configuration options.
type LoggerSettings struct {
	ConsoleLevel string `json:"consoleLevel"`
	FileLevel    string `json:"fileLevel"`
	Less         bool   `json:"less"`
	Colored      bool   `json:"colored"`
}

// AnalyticsUIConfig contains settings for the analytics dashboard display.
type AnalyticsUIConfig struct {
	Refresh        int  `json:"refresh"`
	DaysAgo        int  `json:"daysAgo"`
	EnableChatLogs bool `json:"enableChatLogs"`
}

// StreamerConfig represents a streamer in the configuration with optional per-streamer overrides.
type StreamerConfig struct {
	Username string                  `json:"username"`
	Settings *StreamerSettingsConfig `json:"settings,omitempty"`
}

// StreamerSettingsConfig is a partial override for a streamer's settings.
// Only non-nil fields are applied; others fall back to DefaultSettings.
// Pointer fields allow distinguishing between "unset" and "false"/zero values.
type StreamerSettingsConfig struct {
	MakePredictions *bool            `json:"makePredictions,omitempty"`
	FollowRaid      *bool            `json:"followRaid,omitempty"`
	ClaimDrops      *bool            `json:"claimDrops,omitempty"`
	ClaimMoments    *bool            `json:"claimMoments,omitempty"`
	WatchStreak     *bool            `json:"watchStreak,omitempty"`
	CommunityGoals  *bool            `json:"communityGoals,omitempty"`
	Chat            *string          `json:"chat,omitempty"`
	Bet             *BetSettingsJSON `json:"bet,omitempty"`
}

// BetSettingsJSON contains prediction betting configuration with pointer fields for partial overrides.
type BetSettingsJSON struct {
	Strategy      *string  `json:"strategy,omitempty"`
	Percentage    *int     `json:"percentage,omitempty"`
	PercentageGap *int     `json:"percentageGap,omitempty"`
	MaxPoints     *int     `json:"maxPoints,omitempty"`
	MinimumPoints *int     `json:"minimumPoints,omitempty"`
	StealthMode   *bool    `json:"stealthMode,omitempty"`
	Delay         *float64 `json:"delay,omitempty"`
	DelayMode     *string  `json:"delayMode,omitempty"`
}

// StreamersConfig is used for streamer-related API responses.
type StreamersConfig struct {
	DefaultSettings StreamerSettingsConfig `json:"defaultSettings"`
	Streamers       []StreamerConfig       `json:"streamers"`
}

// SettingsUpdateCallback is invoked when the user submits new settings from the UI.
// The callback should apply the changes atomically and persist them to the config file.
type SettingsUpdateCallback func(settings RuntimeSettings)

// SettingsProvider exposes current and default runtime settings for the analytics UI.
// It is implemented by the miner to provide read access to configuration.
type SettingsProvider interface {
	GetRuntimeSettings() RuntimeSettings
	GetDefaultSettings() RuntimeSettings
}
