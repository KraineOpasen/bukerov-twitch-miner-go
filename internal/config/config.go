package config

import (
	"encoding/json"
	"os"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
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
	Notifications       NotificationsSettings   `json:"notifications"`
	Debug               DebugSettings           `json:"debug"`

	// DropBlacklist is a list of case-insensitive keywords. Any drop campaign
	// whose drop or reward name contains one of them is skipped during drop
	// rotation prioritization, in addition to the claim-history dedup.
	DropBlacklist []string `json:"dropBlacklist,omitempty"`

	// DirectoryGames lists game names (as shown on Twitch, e.g. "World of
	// Tanks") for which directory-based channel discovery is enabled: the
	// miner periodically queries the game's Twitch directory for live
	// drops-enabled channels and farms the best one in an extra watch slot,
	// separate from the fixed streamer list and its 2-slot rotation. Empty
	// (the default) disables the whole subsystem.
	DirectoryGames []string `json:"directoryGames,omitempty"`

	// AutoRedeem holds per-streamer auto-redeem configuration for custom
	// channel-points rewards, keyed by lowercase streamer username. It is a
	// top-level map (rather than a StreamerSettings field) so it survives the
	// Settings-page save round-trip untouched and is edited independently from
	// the streamer-page Rewards panel. Absent/disabled entries mean no
	// auto-redeem.
	AutoRedeem map[string]AutoRedeemConfig `json:"autoRedeem,omitempty"`
}

// AutoRedeemConfig is the per-streamer opt-in for automatically redeeming
// custom channel-points rewards. Nothing is ever auto-redeemed unless Enabled
// is true, a positive Budget is set, and the specific reward is whitelisted in
// RewardIDs — the miner never spends on rewards the user did not explicitly
// pick, and never on user-input rewards regardless of whitelist.
type AutoRedeemConfig struct {
	Enabled bool `json:"enabled"`
	// Budget is the maximum total points the miner may spend auto-redeeming on
	// this streamer for the current process lifetime. Spending is tracked in
	// memory and resets on restart or when the config is edited.
	Budget int `json:"budget"`
	// RewardIDs is the whitelist of custom-reward IDs the miner is allowed to
	// auto-redeem for this streamer.
	RewardIDs []string `json:"rewardIds,omitempty"`
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

	// DropProgressSyncInterval is how often (in minutes) the drops tracker runs
	// a lightweight, inventory-only refresh of the watched-minute progress of
	// the already-tracked campaigns. Unlike CampaignSyncInterval it issues a
	// single cheap Inventory query and touches neither the ViewerDropsDashboard
	// listing nor the per-campaign DropCampaignDetails calls, so it can run far
	// more often to keep the Drops page within a minute or two of Twitch's real
	// progress instead of up to a full CampaignSyncInterval behind. Campaign
	// discovery, claiming, and blacklist/claim-history filtering all stay on the
	// slower CampaignSyncInterval.
	DropProgressSyncInterval int `json:"dropProgressSyncInterval"`

	// ConnectionTimeoutMinutes is the watchdog threshold: if neither the
	// Twitch API nor the PubSub websocket have seen any successful activity
	// for this many minutes, the connection is considered lost (dashboard
	// banner + Discord notification + ERROR log) until activity resumes.
	ConnectionTimeoutMinutes int `json:"connectionTimeoutMinutes"`

	// RotationIntervalMinMinutes/RotationIntervalMaxMinutes bound how long
	// (in minutes) the watched pair dwells before rotating when more than
	// constants.MaxSimultaneousStreams streamers are online: a new random
	// duration within this range is drawn every time the pair actually
	// changes, so rotations don't happen on one predictable fixed timer.
	RotationIntervalMinMinutes int `json:"rotationIntervalMinMinutes"`
	RotationIntervalMaxMinutes int `json:"rotationIntervalMaxMinutes"`

	// RotationInterval is deprecated: superseded by RotationIntervalMinMinutes/
	// RotationIntervalMaxMinutes above (a single fixed timer defeated the
	// point of a *fair* rotation by making it fully predictable). It's kept
	// only so config.json files written before this change still parse -
	// LoadConfig migrates it into the new Min/Max fields when present and
	// they're absent, then clears it. Never read anywhere else.
	RotationInterval int `json:"rotationInterval,omitempty"`
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

	// RetentionDays bounds how long per-event points/annotation history is
	// kept before automatic pruning. 0 disables pruning (keep forever); values
	// are clamped to [0, 365] by ValidateConfig. Default 60.
	RetentionDays int `json:"retentionDays"`
}

// DebugSettings configures the localhost-only diagnostic HTTP server
// (GET /debug/snapshot and GET /debug/log). It always binds to 127.0.0.1 -
// only the port is configurable - so the internal state it exposes is never
// reachable from outside the machine (or container) running the miner.
type DebugSettings struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port"`
}

// DiscordSettings contains Discord integration configuration.
// Only connection settings are stored in config; notification rules are in the database.
type DiscordSettings struct {
	Enabled  bool   `json:"enabled"`
	BotToken string `json:"botToken"`
	GuildID  string `json:"guildId"`
}

// NotificationsSettings holds provider-agnostic notification settings that are
// not per-provider connection secrets (those live in environment variables for
// the push providers). Currently it carries the event batching configuration.
type NotificationsSettings struct {
	// Batching is the global batching configuration applied to every push
	// provider unless overridden per-provider.
	Batching BatchingSettings `json:"batching"`

	// ProviderBatching optionally overrides the global batching config for a
	// specific push provider, keyed by provider name ("matrix", "pushover",
	// "gotify", "webhook"). A missing key means the global config applies.
	ProviderBatching map[string]BatchingSettings `json:"providerBatching,omitempty"`
}

// BatchingSettings configures how notification events are grouped before being
// delivered to a push provider. When disabled, every event is sent immediately.
type BatchingSettings struct {
	// Enabled turns batching on. When false, events are delivered as they
	// arrive (one message per event).
	Enabled bool `json:"enabled"`

	// Interval is how often accumulated batches are flushed, expressed as a Go
	// duration string (e.g. "30m", "5m", "90s"). Invalid or empty values fall
	// back to the default interval.
	Interval string `json:"interval"`

	// MaxEntries caps how many event lines a single flushed message may
	// contain; batches larger than this are split across several messages.
	MaxEntries int `json:"maxEntries"`

	// ImmediateEvents lists event type identifiers that always bypass batching
	// and are delivered instantly (e.g. "drop_claim", "bet_win", "bet_lose").
	ImmediateEvents []string `json:"immediateEvents,omitempty"`
}

// DefaultNotificationsSettings returns the default (batching disabled)
// notification settings.
func DefaultNotificationsSettings() NotificationsSettings {
	return NotificationsSettings{
		Batching: DefaultBatchingSettings(),
	}
}

// DefaultBatchingSettings returns sensible defaults for event batching.
func DefaultBatchingSettings() BatchingSettings {
	return BatchingSettings{
		Enabled:         false,
		Interval:        "30m",
		MaxEntries:      20,
		ImmediateEvents: []string{"drop_claim", "bet_win", "bet_lose"},
	}
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
		Notifications:       DefaultNotificationsSettings(),
		Debug:               DefaultDebugSettings(),
	}
}

func DefaultDebugSettings() DebugSettings {
	return DebugSettings{
		Enabled: false,
		Port:    5757,
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
		WebsocketPingInterval:    27,
		CampaignSyncInterval:     60,
		DropProgressSyncInterval: 2,
		MinuteWatchedInterval:    60,
		RequestDelay:             0.5,
		ReconnectDelay:           60,
		StreamCheckInterval:      600,
		ConnectionTimeoutMinutes: 15,
		// 30-80 minutes: long enough that a channel clears the ~7-minute
		// watch-streak window and makes real drop progress before losing its
		// slot; randomized (redrawn on every actual pair switch) so the
		// rotation cadence isn't a single predictable timer; the spread still
		// keeps a typical online lineup cycling within a few hours. See
		// internal/watcher for the full rotation algorithm.
		RotationIntervalMinMinutes: 30,
		RotationIntervalMaxMinutes: 80,
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
		RetentionDays:  60,
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

	migrateRotationInterval(data, &config)

	ValidateConfig(&config)
	return &config, nil
}

// migrateRotationInterval provides backward compatibility for config.json
// files written before rotationInterval was replaced by the randomized
// rotationIntervalMinMinutes/rotationIntervalMaxMinutes range: if the old
// field is present but the new range fields are absent, the old fixed value
// (converted to minutes) becomes both bounds, preserving the previous fixed
// interval until the user adopts the new fields. Presence is checked against
// the raw JSON rather than the unmarshaled config, since DefaultConfig
// already populates the new fields with non-zero defaults and a plain value
// comparison couldn't tell "absent" from "explicitly set to the default".
func migrateRotationInterval(data []byte, config *Config) {
	var raw struct {
		RateLimits struct {
			RotationInterval           *int `json:"rotationInterval"`
			RotationIntervalMinMinutes *int `json:"rotationIntervalMinMinutes"`
			RotationIntervalMaxMinutes *int `json:"rotationIntervalMaxMinutes"`
		} `json:"rateLimits"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	if raw.RateLimits.RotationInterval == nil {
		return
	}
	if raw.RateLimits.RotationIntervalMinMinutes != nil || raw.RateLimits.RotationIntervalMaxMinutes != nil {
		return
	}

	minutes := *raw.RateLimits.RotationInterval / 60
	if minutes < 1 {
		minutes = 1
	}
	config.RateLimits.RotationIntervalMinMinutes = minutes
	config.RateLimits.RotationIntervalMaxMinutes = minutes
	config.RateLimits.RotationInterval = 0
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

	if config.RateLimits.DropProgressSyncInterval < 1 {
		config.RateLimits.DropProgressSyncInterval = 1
	} else if config.RateLimits.DropProgressSyncInterval > 60 {
		config.RateLimits.DropProgressSyncInterval = 60
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

	if config.RateLimits.ConnectionTimeoutMinutes < 5 {
		config.RateLimits.ConnectionTimeoutMinutes = 5
	} else if config.RateLimits.ConnectionTimeoutMinutes > 60 {
		config.RateLimits.ConnectionTimeoutMinutes = 60
	}

	if config.RateLimits.RotationIntervalMinMinutes < 5 {
		config.RateLimits.RotationIntervalMinMinutes = 5
	} else if config.RateLimits.RotationIntervalMinMinutes > 180 {
		config.RateLimits.RotationIntervalMinMinutes = 180
	}

	if config.RateLimits.RotationIntervalMaxMinutes < 5 {
		config.RateLimits.RotationIntervalMaxMinutes = 5
	} else if config.RateLimits.RotationIntervalMaxMinutes > 240 {
		config.RateLimits.RotationIntervalMaxMinutes = 240
	}

	if config.RateLimits.RotationIntervalMaxMinutes < config.RateLimits.RotationIntervalMinMinutes {
		config.RateLimits.RotationIntervalMaxMinutes = config.RateLimits.RotationIntervalMinMinutes
	}

	if config.Debug.Port < 1 || config.Debug.Port > 65535 {
		config.Debug.Port = DefaultDebugSettings().Port
	}

	if config.Analytics.RetentionDays < 0 {
		config.Analytics.RetentionDays = 0
	} else if config.Analytics.RetentionDays > 365 {
		config.Analytics.RetentionDays = 365
	}
}
