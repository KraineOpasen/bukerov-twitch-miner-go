package settings

import (
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// normalizeBlacklist trims each keyword and drops blank entries so the stored
// drop-name blacklist stays clean regardless of how the UI splits the input
// (commas, newlines, stray whitespace). Returns nil when nothing remains so the
// field is omitted from config.json rather than serialized as an empty list.
func normalizeBlacklist(keywords []string) []string {
	cleaned := make([]string, 0, len(keywords))
	for _, kw := range keywords {
		if trimmed := strings.TrimSpace(kw); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}

// BuildRuntimeSettings constructs a RuntimeSettings DTO from the current config.
func BuildRuntimeSettings(cfg *config.Config) RuntimeSettings {
	priority := make([]string, len(cfg.Priority))
	for i, p := range cfg.Priority {
		priority[i] = string(p)
	}

	streamers := make([]StreamerConfig, len(cfg.Streamers))
	for i, sc := range cfg.Streamers {
		streamers[i] = StreamerConfig{
			Username: sc.Username,
			Settings: StreamerSettingsPtrToDTO(sc.Settings),
		}
	}

	return RuntimeSettings{
		Streamers:       streamers,
		DefaultSettings: StreamerSettingsToDTO(cfg.StreamerSettings),
		Priority:        priority,
		RateLimits: RateLimitSettings{
			WebsocketPingInterval: cfg.RateLimits.WebsocketPingInterval,
			CampaignSyncInterval:  cfg.RateLimits.CampaignSyncInterval,
			MinuteWatchedInterval: cfg.RateLimits.MinuteWatchedInterval,
			RequestDelay:          cfg.RateLimits.RequestDelay,
			ReconnectDelay:        cfg.RateLimits.ReconnectDelay,
			StreamCheckInterval:   cfg.RateLimits.StreamCheckInterval,

			ConnectionTimeoutMinutes: cfg.RateLimits.ConnectionTimeoutMinutes,

			RotationIntervalMinMinutes: cfg.RateLimits.RotationIntervalMinMinutes,
			RotationIntervalMaxMinutes: cfg.RateLimits.RotationIntervalMaxMinutes,
		},
		Logger: LoggerSettings{
			ConsoleLevel: cfg.Logger.ConsoleLevel,
			FileLevel:    cfg.Logger.FileLevel,
			Less:         cfg.Logger.Less,
			Colored:      cfg.Logger.Colored,
		},
		Analytics: AnalyticsUIConfig{
			Refresh:        cfg.Analytics.Refresh,
			DaysAgo:        cfg.Analytics.DaysAgo,
			EnableChatLogs: cfg.Analytics.EnableChatLogs,
		},
		Discord: DiscordUIConfig{
			Enabled:  cfg.Discord.Enabled,
			BotToken: cfg.Discord.BotToken,
			GuildID:  cfg.Discord.GuildID,
		},
		DropBlacklist: cfg.DropBlacklist,
	}
}

// BuildDefaultSettings constructs a RuntimeSettings DTO from defaults, preserving current streamers.
func BuildDefaultSettings(currentStreamers []config.StreamerConfig) RuntimeSettings {
	streamers := make([]StreamerConfig, len(currentStreamers))
	for i, sc := range currentStreamers {
		streamers[i] = StreamerConfig{
			Username: sc.Username,
			Settings: nil,
		}
	}

	defaults := config.DefaultConfig()
	priority := make([]string, len(defaults.Priority))
	for i, p := range defaults.Priority {
		priority[i] = string(p)
	}

	return RuntimeSettings{
		Streamers:       streamers,
		DefaultSettings: StreamerSettingsToDTO(defaults.StreamerSettings),
		Priority:        priority,
		RateLimits: RateLimitSettings{
			WebsocketPingInterval: defaults.RateLimits.WebsocketPingInterval,
			CampaignSyncInterval:  defaults.RateLimits.CampaignSyncInterval,
			MinuteWatchedInterval: defaults.RateLimits.MinuteWatchedInterval,
			RequestDelay:          defaults.RateLimits.RequestDelay,
			ReconnectDelay:        defaults.RateLimits.ReconnectDelay,
			StreamCheckInterval:   defaults.RateLimits.StreamCheckInterval,

			ConnectionTimeoutMinutes: defaults.RateLimits.ConnectionTimeoutMinutes,

			RotationIntervalMinMinutes: defaults.RateLimits.RotationIntervalMinMinutes,
			RotationIntervalMaxMinutes: defaults.RateLimits.RotationIntervalMaxMinutes,
		},
		Logger: LoggerSettings{
			ConsoleLevel: defaults.Logger.ConsoleLevel,
			FileLevel:    defaults.Logger.FileLevel,
			Less:         defaults.Logger.Less,
			Colored:      defaults.Logger.Colored,
		},
		Analytics: AnalyticsUIConfig{
			Refresh:        defaults.Analytics.Refresh,
			DaysAgo:        defaults.Analytics.DaysAgo,
			EnableChatLogs: defaults.Analytics.EnableChatLogs,
		},
		Discord: DiscordUIConfig{
			Enabled:  defaults.Discord.Enabled,
			BotToken: defaults.Discord.BotToken,
			GuildID:  defaults.Discord.GuildID,
		},
		DropBlacklist: defaults.DropBlacklist,
	}
}

// ApplyToConfig updates a config with values from a RuntimeSettings DTO.
// Returns the converted streamer configs (for caller to apply to running streamers).
func ApplyToConfig(cfg *config.Config, s RuntimeSettings) {
	cfg.Streamers = make([]config.StreamerConfig, len(s.Streamers))
	for i, sc := range s.Streamers {
		cfg.Streamers[i] = config.StreamerConfig{
			Username: sc.Username,
			Settings: StreamerSettingsPtrFromDTO(sc.Settings),
		}
	}

	cfg.StreamerSettings = StreamerSettingsFromDTO(s.DefaultSettings)

	cfg.Priority = make([]config.Priority, len(s.Priority))
	for i, p := range s.Priority {
		cfg.Priority[i] = config.Priority(p)
	}

	cfg.RateLimits.WebsocketPingInterval = s.RateLimits.WebsocketPingInterval
	cfg.RateLimits.CampaignSyncInterval = s.RateLimits.CampaignSyncInterval
	cfg.RateLimits.MinuteWatchedInterval = s.RateLimits.MinuteWatchedInterval
	cfg.RateLimits.RequestDelay = s.RateLimits.RequestDelay
	cfg.RateLimits.ReconnectDelay = s.RateLimits.ReconnectDelay
	cfg.RateLimits.StreamCheckInterval = s.RateLimits.StreamCheckInterval
	cfg.RateLimits.ConnectionTimeoutMinutes = s.RateLimits.ConnectionTimeoutMinutes
	cfg.RateLimits.RotationIntervalMinMinutes = s.RateLimits.RotationIntervalMinMinutes
	cfg.RateLimits.RotationIntervalMaxMinutes = s.RateLimits.RotationIntervalMaxMinutes

	cfg.Logger.ConsoleLevel = s.Logger.ConsoleLevel
	cfg.Logger.FileLevel = s.Logger.FileLevel
	cfg.Logger.Less = s.Logger.Less
	cfg.Logger.Colored = s.Logger.Colored

	cfg.Analytics.Refresh = s.Analytics.Refresh
	cfg.Analytics.DaysAgo = s.Analytics.DaysAgo
	cfg.Analytics.EnableChatLogs = s.Analytics.EnableChatLogs

	cfg.Discord.Enabled = s.Discord.Enabled
	cfg.Discord.BotToken = s.Discord.BotToken
	cfg.Discord.GuildID = s.Discord.GuildID

	cfg.DropBlacklist = normalizeBlacklist(s.DropBlacklist)

	config.ValidateConfig(cfg)
}

// GetStreamerSettings retrieves effective settings for a streamer from config.
// Returns per-streamer override if set, otherwise returns the default settings.
func GetStreamerSettings(cfg *config.Config, username string) models.StreamerSettings {
	for _, sc := range cfg.Streamers {
		if sc.Username == username && sc.Settings != nil {
			return *sc.Settings
		}
	}
	return cfg.StreamerSettings
}
