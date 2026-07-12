package config

import (
	"encoding/json"
	"os"

	"github.com/PatrickWalther/twitch-miner-go/internal/models"
)

type Priority string

const (
	PriorityStreak           Priority = "STREAK"
	PriorityDrops            Priority = "DROPS"
	PriorityOrder            Priority = "ORDER"
	PrioritySubscribed       Priority = "SUBSCRIBED"
	PriorityPointsAscending  Priority = "POINTS_ASCENDING"
	PriorityPointsDescending Priority = "POINTS_DESCENDING"
)

type Config struct {
	Username            string                  `json:"username"`
	ClaimDropsOnStartup bool                    `json:"claimDropsOnStartup"`
	EnableAnalytics     bool                    `json:"enableAnalytics"`
	Priority            []Priority              `json:"priority"`
	StreamerSettings    models.StreamerSettings `json:"streamerSettings"`
	Streamers           []StreamerConfig        `json:"streamers"`
	RateLimits          RateLimitSettings       `json:"rateLimits"`
	Logger              LoggerSettings          `json:"logger"`
	Analytics           AnalyticsSettings       `json:"analytics"`
	Discord             DiscordSettings         `json:"discord"`
}

type StreamerConfig struct {
	Username string                   `json:"username"`
	Settings *models.StreamerSettings `json:"settings,omitempty"`
}

type RateLimitSettings struct {
	WebsocketPingInterval int     `json:"websocketPingInterval"`
	CampaignSyncInterval  int     `json:"campaignSyncInterval"`
	MinuteWatchedInterval int     `json:"minuteWatchedInterval"`
	RequestDelay          float64 `json:"requestDelay"`
	ReconnectDelay        int     `json:"reconnectDelay"`
	StreamCheckInterval   int     `json:"streamCheckInterval"`
	RotationInterval      int     `json:"rotationInterval"`
}

type LoggerSettings struct {
	Save         bool   `json:"save"`
	Less         bool   `json:"less"`
	ConsoleLevel string `json:"consoleLevel"`
	FileLevel    string `json:"fileLevel"`
	Colored      bool   `json:"colored"`
	AutoClear    bool   `json:"autoClear"`
	TimeZone     string `json:"timeZone,omitempty"`
}

type AnalyticsSettings struct {
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Refresh        int    `json:"refresh"`
	DaysAgo        int    `json:"daysAgo"`
	EnableChatLogs bool   `json:"enableChatLogs"`
}

// DiscordSettings contains Discord integration configuration.
// Only connection settings are stored in config; notification rules are in the database.
type DiscordSettings struct {
	Enabled  bool   `json:"enabled"`
	BotToken string `json:"botToken"`
	GuildID  string `json:"guildId"`
}

func DefaultConfig() Config {
	return Config{
		ClaimDropsOnStartup: false,
		EnableAnalytics:     true,
		Priority:            []Priority{PriorityStreak, PriorityDrops, PriorityOrder},
		StreamerSettings:    models.DefaultStreamerSettings(),
		RateLimits:          DefaultRateLimitSettings(),
		Logger:              DefaultLoggerSettings(),
		Analytics:           DefaultAnalyticsSettings(),
		Discord:             DefaultDiscordSettings(),
	}
}

func DefaultDiscordSettings() DiscordSettings {
	return DiscordSettings{
		Enabled:  false,
		BotToken: "",
		GuildID:  "",
	}
}

func DefaultRateLimitSettings() RateLimitSettings {
	return RateLimitSettings{
		WebsocketPingInterval: 27,
		CampaignSyncInterval:  60,
		MinuteWatchedInterval: 60,
		RequestDelay:          0.5,
		ReconnectDelay:        60,
		StreamCheckInterval:   600,
		// 15 minutes: long enough for a channel to clear the ~7-minute watch-streak
		// window and make meaningful drop progress before losing its slot, short
		// enough that a typical online lineup cycles through everyone within a
		// couple of hours. See internal/watcher for the rotation algorithm.
		RotationInterval: 900,
	}
}

func DefaultLoggerSettings() LoggerSettings {
	return LoggerSettings{
		Save:         true,
		Less:         false,
		ConsoleLevel: "INFO",
		FileLevel:    "DEBUG",
		Colored:      false,
		AutoClear:    true,
	}
}

func DefaultAnalyticsSettings() AnalyticsSettings {
	return AnalyticsSettings{
		Host:           "0.0.0.0",
		Port:           5000,
		Refresh:        5,
		DaysAgo:        7,
		EnableChatLogs: false,
	}
}

func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	config := DefaultConfig()
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	ValidateConfig(&config)
	return &config, nil
}

func SaveConfig(path string, config *Config) error {
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// ValidateConfig enforces min/max bounds on rate limits and other configurable values.
// It mutates the config in place, clamping out-of-range values to valid bounds.
func ValidateConfig(config *Config) {
	if config.RateLimits.WebsocketPingInterval < 20 {
		config.RateLimits.WebsocketPingInterval = 20
	} else if config.RateLimits.WebsocketPingInterval > 60 {
		config.RateLimits.WebsocketPingInterval = 60
	}

	if config.RateLimits.CampaignSyncInterval < 5 {
		config.RateLimits.CampaignSyncInterval = 5
	} else if config.RateLimits.CampaignSyncInterval > 120 {
		config.RateLimits.CampaignSyncInterval = 120
	}

	if config.RateLimits.MinuteWatchedInterval < 30 {
		config.RateLimits.MinuteWatchedInterval = 30
	} else if config.RateLimits.MinuteWatchedInterval > 120 {
		config.RateLimits.MinuteWatchedInterval = 120
	}

	if config.RateLimits.RequestDelay < 0.1 {
		config.RateLimits.RequestDelay = 0.1
	} else if config.RateLimits.RequestDelay > 2.0 {
		config.RateLimits.RequestDelay = 2.0
	}

	if config.RateLimits.ReconnectDelay < 30 {
		config.RateLimits.ReconnectDelay = 30
	} else if config.RateLimits.ReconnectDelay > 300 {
		config.RateLimits.ReconnectDelay = 300
	}

	if config.RateLimits.StreamCheckInterval < 60 {
		config.RateLimits.StreamCheckInterval = 60
	} else if config.RateLimits.StreamCheckInterval > 900 {
		config.RateLimits.StreamCheckInterval = 900
	}

	if config.RateLimits.RotationInterval < 120 {
		config.RateLimits.RotationInterval = 120
	} else if config.RateLimits.RotationInterval > 3600 {
		config.RateLimits.RotationInterval = 3600
	}
}
