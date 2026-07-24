package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
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

// DiscoveryMode selects which channels directory discovery may farm.
type DiscoveryMode string

const (
	// DiscoveryModeAll (the zero value "" normalizes here) preserves the original
	// behavior: farm any live drops-enabled directory channel EXCEPT ones already
	// on the configured streamer list (the rotation covers those).
	DiscoveryModeAll DiscoveryMode = "all"
	// DiscoveryModeTrackedOnly inverts the exclusion: farm ONLY channels that are
	// on the configured streamer list, and never one the rotation is already
	// watching (avoids a duplicate watch of an already-watched channel).
	DiscoveryModeTrackedOnly DiscoveryMode = "tracked_only"
)

// DefaultDiscoveryMode is the behavior-preserving default.
const DefaultDiscoveryMode = DiscoveryModeAll

// Valid reports whether d is a known mode.
func (d DiscoveryMode) Valid() bool {
	switch d {
	case DiscoveryModeAll, DiscoveryModeTrackedOnly:
		return true
	default:
		return false
	}
}

// NormalizeDiscoveryMode lower-cases/trims s and validates it, falling back to
// DefaultDiscoveryMode so an empty or unknown value means the current "all"
// behavior. Mirrors policy.Normalize's contract for CampaignPolicy.
func NormalizeDiscoveryMode(s string) DiscoveryMode {
	d := DiscoveryMode(strings.ToLower(strings.TrimSpace(s)))
	if d.Valid() {
		return d
	}
	return DefaultDiscoveryMode
}

type Config struct {
	// Username is the account's CURRENT canonical Twitch login — the MUTABLE
	// authoritative attribute (BKM-006 Corrective Pass 1, COR-2). Every
	// Twitch-facing and user-facing use reads it live: owner channel-ID
	// resolution, the IRC NICK, notification labels, and the dashboard. On a
	// CONFIRMED owner rename (same OwnerUserID, new login) the miner adopts the
	// new login here and persists it, so a renamed owner shows and operates
	// under their new name. It is NOT the storage key — see ProfileKey /
	// StorageKey().
	Username string `json:"username"`
	// ProfileKey is the IMMUTABLE local storage key for this profile's
	// cookies/database/log paths (BKM-006 Corrective Pass 1, COR-2). It is
	// decoupled from the mutable Twitch login so a rename never re-keys
	// credentials or history (which would force a fresh Device Flow / split
	// history). Empty on legacy configs and until the FIRST owner rename;
	// StorageKey() falls back to Username while empty (so existing installs
	// keep their exact cookies/db/log paths — no migration). It is pinned to
	// the pre-rename login exactly once, at the moment a confirmed rename is
	// about to move Username to the new canonical login.
	ProfileKey string `json:"profileKey,omitempty"`
	// OwnerUserID is a TRUSTED, operator-controlled pin of the Twitch account
	// this profile is bound to (BKM-006 Corrective Pass 1, C3): once set, an
	// /oauth2/validate identity check anchors on THIS user ID rather than on
	// the login, so a renamed owner (same account, new login) is tolerated on
	// every restart with no fresh Device Flow — storage stays keyed by the
	// stable ProfileKey while Username tracks the new canonical login. It
	// carries the SAME trust level as Username (both are config-file content
	// the operator controls) and is NEVER adopted from a cookie/credential
	// file: only a CONFIRMED (login-matching, at the time the pin was still
	// empty) validate result ever backfills it automatically. Empty preserves
	// the exact pre-C3 login-anchor behavior.
	OwnerUserID         string                  `json:"ownerUserId,omitempty"`
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

	// DiscordTokenFromEnv records that Discord.BotToken was supplied via the
	// DISCORD_BOT_TOKEN environment variable at load time. While set, the
	// environment is the source of truth: SaveConfig clears the on-disk
	// token, and the Settings page neither shows nor overwrites it. Never
	// serialized.
	DiscordTokenFromEnv bool                 `json:"-"`
	Debug               DebugSettings        `json:"debug"`
	Health              HealthSettings       `json:"health"`
	DailySummary        DailySummarySettings `json:"dailySummary"`

	// PredictionRisk holds the GLOBAL, stateless auto-bet risk gates. It is
	// deliberately top-level (not per-streamer): as a top-level key absent from an
	// existing config.json, LoadConfig keeps the DefaultConfig value (health gate
	// on) regardless of any per-streamer settings blocks.
	PredictionRisk PredictionRiskSettings `json:"predictionRisk"`

	// DropBlacklist is a list of case-insensitive keywords. Any drop campaign
	// whose drop or reward name contains one of them is skipped during drop
	// rotation prioritization, in addition to the claim-history dedup.
	DropBlacklist []string `json:"dropBlacklist,omitempty"`

	// DropCampaignGameIDs is a strict, GLOBAL allowlist of exact Twitch game IDs
	// whose drop campaigns are tracked. Unlike DropCampaignGames it is
	// candidate-independent: it filters correctly even on a sync where the allowed
	// game has no live campaigns (the production all-foreign-inventory case). Game
	// IDs are opaque strings compared with exact, case-sensitive equality (no
	// lowercasing, regex, or substring match). Empty — together with an empty
	// DropCampaignGames — tracks every game, the backward-compatible default.
	DropCampaignGameIDs []string `json:"dropCampaignGameIDs,omitempty"`

	// DropCampaignGames is a GLOBAL, best-effort list of game names (or Twitch
	// displayNames) whose drop campaigns are tracked. Each name is resolved to a
	// game ID against the campaigns present in the current sync (case-insensitive,
	// whole-string; never substring or whitespace-collapsed), after which
	// filtering is strictly by game ID. A name that is ambiguous or absent from
	// the current sync fails open. For filtering that holds regardless of what is
	// live, also list the game ID in DropCampaignGameIDs. Empty — together with an
	// empty DropCampaignGameIDs — tracks every game, the backward-compatible
	// default.
	DropCampaignGames []string `json:"dropCampaignGames,omitempty"`

	// DirectoryGames lists game names (as shown on Twitch, e.g. "World of
	// Tanks") for which directory-based channel discovery is enabled: the
	// miner periodically queries the game's Twitch directory for live
	// drops-enabled channels and farms the best one in an extra watch slot,
	// separate from the fixed streamer list and its 2-slot rotation. Empty
	// (the default) disables the whole subsystem.
	DirectoryGames []string `json:"directoryGames,omitempty"`

	// DiscoveryPreferTracked, when true, forbids a directory-discovered channel
	// from displacing a configured (tracked) streamer that already holds a watch
	// slot: your tracked streamers always keep their slot and discovery only
	// fills otherwise-idle ones. It has no effect while DirectoryGames is empty
	// (discovery off). The default (false) preserves the prior slot arbitration,
	// where a discovered channel farming an active drop could bump a configured
	// streamer held only by points or fair-rotation priority.
	DiscoveryPreferTracked bool `json:"discoveryPreferTracked,omitempty"`

	// DiscoveryMode selects which channels directory discovery farms: "all"
	// (default) farms any drops-enabled directory channel except ones already on
	// the configured streamer list; "tracked_only" inverts this — it farms only
	// configured-list channels, and never one the rotation is already watching
	// (no duplicate watch). Empty/unknown normalizes to "all" in ValidateConfig,
	// so an unset field preserves the prior behavior. Independent of
	// DiscoveryPreferTracked, which governs slot arbitration, not candidacy.
	DiscoveryMode DiscoveryMode `json:"discoveryMode,omitempty"`

	// DiscoveryPreferSubscribed, when true, floats a directory-discovered channel
	// the authenticated account is subscribed to above a non-subscribed one when
	// picking which candidate to farm — a tertiary preference layered over the
	// existing viewer-count ordering. Subscription is detected by proxy (an active
	// channel-points multiplier, the same signal the SUBSCRIBED watch priority
	// uses) via a slow periodic ChannelPointsContext probe of the candidate pool.
	// The default (false) preserves the prior viewer-count ordering, and it has no
	// effect while DirectoryGames is empty (discovery off).
	DiscoveryPreferSubscribed bool `json:"discoveryPreferSubscribed,omitempty"`

	// AutoRedeem holds per-streamer auto-redeem configuration for custom
	// channel-points rewards, keyed by lowercase streamer username. It is a
	// top-level map (rather than a StreamerSettings field) so it survives the
	// Settings-page save round-trip untouched and is edited independently from
	// the streamer-page Rewards panel. Absent/disabled entries mean no
	// auto-redeem.
	AutoRedeem map[string]AutoRedeemConfig `json:"autoRedeem,omitempty"`

	// CampaignPolicy selects the drop-campaign prioritization strategy
	// (GAME_ORDER / ENDING_SOONEST / CLOSEST_TO_REWARD / LOW_AVAILABILITY /
	// SMART). Empty or unknown normalizes to GAME_ORDER, which preserves the
	// pre-policy behavior (configured game order), so the engine changes
	// nothing until the operator opts into another mode.
	CampaignPolicy string `json:"campaignPolicy,omitempty"`

	// DropRules holds per-drop operator overrides keyed by the NORMALIZED
	// reward identity (models.NormalizeRewardKey = lowercased "gameID::dropName"),
	// not a transient Twitch drop ID, so a rule survives recurring/regional
	// campaign variants that grant the identical reward. Top-level map (like
	// AutoRedeem) so it round-trips through Settings untouched and is edited
	// from the Drops-page per-drop controls. Absent key = no override.
	DropRules map[string]DropRule `json:"dropRules,omitempty"`
}

// DropRule is the per-drop operator override, keyed by normalized reward
// identity. The zero value (all false) is a no-op, so an absent or reset rule
// leaves the policy engine's defaults in force.
type DropRule struct {
	// Skip excludes the reward from farming entirely (like a targeted
	// blacklist entry, but keyed by reward identity rather than a keyword).
	Skip bool `json:"skip,omitempty"`
	// HighPriority floats the campaign to the top under every policy mode.
	HighPriority bool `json:"highPriority,omitempty"`
	// AlwaysFinishStarted keeps an already-started campaign prioritized so it
	// is carried to completion instead of being abandoned mid-chain.
	AlwaysFinishStarted bool `json:"alwaysFinishStarted,omitempty"`
	// NextRewardOnly limits the feasibility goal (and farming intent) to the
	// next reward rather than the whole drop chain.
	NextRewardOnly bool `json:"nextRewardOnly,omitempty"`
	// IgnoreSubscriberOnly opts into farming a subscriber-only reward. It has
	// no effect unless Twitch reports the subscriber-only flag for the drop
	// (surfaced honestly in the UI as "no effect" when the data is absent).
	IgnoreSubscriberOnly bool `json:"ignoreSubscriberOnly,omitempty"`
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

	// ChannelID is the stable Twitch channel identity behind Username, kept
	// in sync by the miner's ID-first reconciliation (BKM-006) after every
	// settings apply. A NON-EMPTY value is an EXPECTED, IMMUTABLE identity
	// anchor (BKM-006 Corrective Pass 1, C1), not a hint: if Username now
	// resolves to a DIFFERENT ChannelID, reconciliation refuses to adopt the
	// foreign identity — no streamer is added/renamed under it and this
	// field is never silently overwritten with the mismatched value. Only an
	// EMPTY ChannelID is ever backfilled (first-bind path). Absent on config
	// files written before this field existed — fully backward compatible,
	// the entry is simply treated as unconfirmed until the next successful
	// resolution.
	ChannelID string `json:"channelId,omitempty"`
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

// HealthSettings configures the watch-transport accrual canary and the
// drop-progress watchdog (see the Health Center).
//
// Note the deliberate default asymmetry: the canary is OPT-IN (it costs one
// real beacon per check and needs an operator-chosen channel), while the
// watchdog is OPT-OUT (its detection is purely passive reads of state the
// miner already has, and its recovery stages run only after a conservatively
// confirmed stall — accrual correctness is the miner's first priority).
type HealthSettings struct {
	// CanaryEnabled turns on the scheduled canary. Even when off, a channel can
	// still be probed on demand via "Run canary now".
	CanaryEnabled bool `json:"canaryEnabled"`
	// CanaryChannel is the login of a reliable, near-always-live channel (e.g. a
	// 24/7 channel) the canary sends one real minute-watched beacon to. Empty
	// disables the canary entirely.
	CanaryChannel string `json:"canaryChannel,omitempty"`
	// CanaryIntervalMinutes is the target cadence for a successful confirmation
	// when a watch slot is free (clamped to [60, 1440]).
	CanaryIntervalMinutes int `json:"canaryIntervalMinutes"`
	// CanaryMaxStalenessHours forces a probe (regardless of slot occupancy) once
	// the watch transport has not been confirmed for this long (clamped to
	// [1, 168]).
	CanaryMaxStalenessHours int `json:"canaryMaxStalenessHours"`

	// WatchdogEnabled turns on the drop-progress watchdog: stall detection for
	// tracked drops plus the staged automatic recovery pipeline. Enabled by
	// default (see the asymmetry note above).
	WatchdogEnabled bool `json:"watchdogEnabled"`
	// WatchdogStallDelayMinutes is the minimum wall time a drop's minutes must
	// be flat before a stall can confirm (clamped to [10, 120]). Twitch credits
	// minutes in ~15-minute batches, so values below that invite false alarms.
	WatchdogStallDelayMinutes int `json:"watchdogStallDelayMinutes"`
	// WatchdogStallConfirmations is how many consecutive completed inventory
	// observations must report no progress before a stall confirms (clamped to
	// [2, 10]).
	WatchdogStallConfirmations int `json:"watchdogStallConfirmations"`
	// WatchdogRecoveryCooldownMinutes is the minimum gap between two recovery
	// stage executions (clamped to [1, 60]).
	WatchdogRecoveryCooldownMinutes int `json:"watchdogRecoveryCooldownMinutes"`
	// WatchdogAvoidTTLMinutes is how long the channel-switch recovery stage
	// excludes a channel from watch selection (clamped to [10, 360]).
	WatchdogAvoidTTLMinutes int `json:"watchdogAvoidTTLMinutes"`
	// WatchdogRearmHours is how long an exhausted (STALLED) episode waits
	// before the recovery pipeline may run again (clamped to [1, 48]).
	WatchdogRearmHours int `json:"watchdogRearmHours"`
}

// DailySummarySettings configures the once-a-day operator digest sent through
// the notification system's system channel (earned points, drops, prediction
// net, streaks, recovery incidents, lost mining time for the previous local
// day). Opt-in: off by default so it never surprises existing installs.
type DailySummarySettings struct {
	// Enabled turns the daily summary on. It additionally requires the
	// notification system channel to be configured (same gate as the other
	// operator alerts).
	Enabled bool `json:"enabled"`
	// Time is the local wall-clock time to send the summary, "HH:MM" (24h).
	// Invalid values fall back to the default (09:00).
	Time string `json:"time"`
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

// PredictionRiskSettings are the GLOBAL, stateless prediction risk gates applied
// to automatic bets only (post-Stealth); they never affect manual bets. They are
// intentionally NOT per-streamer — a per-streamer override would silently load
// HealthGateEnabled=false for any streamer that already has a settings block,
// disabling a default-on protection. The absolute per-streamer cap stays
// BetSettings.MaxPoints and is not duplicated here.
//
//   - MaxStakePercent: cap the stake to this percent of balance (0 = off, else 1..100).
//   - ReservePoints: keep at least this many points unbet — a bet that would push
//     the balance below it is SKIPPED (0 = off; >= 0).
//   - HealthGateEnabled: block auto-bets while the GQL API or PubSub transport is
//     degraded/failed; an unknown health state fails open.
type PredictionRiskSettings struct {
	MaxStakePercent   int  `json:"maxStakePercent"`
	ReservePoints     int  `json:"reservePoints"`
	HealthGateEnabled bool `json:"healthGateEnabled"`
}

// DefaultPredictionRiskSettings returns the behaviour-preserving defaults: both
// size gates off, the health gate on (fail-open on unknown).
func DefaultPredictionRiskSettings() PredictionRiskSettings {
	return PredictionRiskSettings{
		MaxStakePercent:   0,
		ReservePoints:     0,
		HealthGateEnabled: true,
	}
}

// StorageKey returns the stable local storage key for this profile's
// cookies/database/log paths (BKM-006 Corrective Pass 1, COR-2). It is
// ProfileKey when set, otherwise Username — so legacy configs (and every
// install before the FIRST owner rename) keep their exact existing paths
// with no migration, while a renamed owner keeps loading credentials and
// history from the original, pinned location even though Username has moved
// to the new canonical login.
func (c *Config) StorageKey() string {
	if key := strings.TrimSpace(c.ProfileKey); key != "" {
		return key
	}
	return c.Username
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
		Health:              DefaultHealthSettings(),
		DailySummary:        DefaultDailySummarySettings(),
		PredictionRisk:      DefaultPredictionRiskSettings(),
	}
}

// DefaultDailySummarySettings returns the opt-in daily summary defaults: off,
// scheduled for 09:00 local time.
func DefaultDailySummarySettings() DailySummarySettings {
	return DailySummarySettings{
		Enabled: false,
		Time:    "09:00",
	}
}

// NormalizeDailySummaryTime validates an "H:M"/"HH:MM" 24-hour string and returns
// it canonicalized as zero-padded "HH:MM". Invalid input yields the default
// "09:00" and ok=false so the caller can log the fallback. Parsing is manual
// (not time.Parse) so single-digit fields like "9:5" are accepted and padded.
func NormalizeDailySummaryTime(s string) (canonical string, ok bool) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) != 2 {
		return "09:00", false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return "09:00", false
	}
	return fmt.Sprintf("%02d:%02d", h, m), true
}

func DefaultDebugSettings() DebugSettings {
	return DebugSettings{
		Enabled: false,
		Port:    5757,
	}
}

func DefaultHealthSettings() HealthSettings {
	return HealthSettings{
		CanaryEnabled:           false,
		CanaryChannel:           "",
		CanaryIntervalMinutes:   360, // 6h
		CanaryMaxStalenessHours: 48,

		// Watchdog defaults are conservative on purpose: Twitch credits drop
		// minutes in ~15-minute batches, so 20 minutes of silence across 3
		// clean inventory observations is required before recovery starts.
		WatchdogEnabled:                 true,
		WatchdogStallDelayMinutes:       20,
		WatchdogStallConfirmations:      3,
		WatchdogRecoveryCooldownMinutes: 5,
		WatchdogAvoidTTLMinutes:         60,
		WatchdogRearmHours:              6,
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

// DefaultAnalyticsSettings binds the dashboard to loopback by default:
// exposing it beyond the local machine must be an explicit choice (set
// analytics.host in config.json or the DASHBOARD_HOST env var), and a
// non-loopback bind additionally requires DASHBOARD_USERNAME/
// DASHBOARD_PASSWORD (or the explicit DASHBOARD_INSECURE_NO_AUTH=true
// opt-out) - see internal/web.
func DefaultAnalyticsSettings() AnalyticsSettings {
	return AnalyticsSettings{
		Host:           "127.0.0.1",
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

	LogPredictionRiskClamps(config.PredictionRisk, "config.json")
	ValidateConfig(&config)
	applyEnvOverrides(&config)
	tightenConfigPermissions(path)
	return &config, nil
}

// applyEnvOverrides layers environment-supplied secrets over the loaded
// config. DISCORD_BOT_TOKEN wins over the file's discord.botToken and is
// never written back to disk (see SaveConfig) — the same env-over-config,
// never-persisted precedence DASHBOARD_HOST uses for the bind address.
func applyEnvOverrides(cfg *Config) {
	if token := strings.TrimSpace(os.Getenv("DISCORD_BOT_TOKEN")); token != "" {
		cfg.Discord.BotToken = token
		cfg.DiscordTokenFromEnv = true
	}
}

// tightenConfigPermissions migrates a pre-hardening config file (written
// 0644 by older versions) to owner-only permissions on load, so the fix
// takes effect on the first start of the new code instead of waiting for
// the next save. Best-effort: a failed chmod only warns — this is
// hardening, not a correctness gate.
func tightenConfigPermissions(path string) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o077 == 0 {
		return
	}
	if err := os.Chmod(path, 0o600); err != nil {
		slog.Warn("Could not tighten config file permissions to 0600", "path", path, "error", err)
		return
	}
	slog.Info("Tightened config file permissions to 0600 (it may contain the Discord bot token)", "path", path)
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
	// With DISCORD_BOT_TOKEN set the environment is the source of truth and
	// the token is deliberately NOT persisted: the on-disk copy is cleared.
	// Documented in README — removing the env var later does not restore a
	// file value; the token must be re-entered.
	toWrite := *config
	if config.DiscordTokenFromEnv {
		toWrite.Discord.BotToken = ""
	}

	data, err := json.MarshalIndent(&toWrite, "", "  ")
	if err != nil {
		return err
	}
	// config.json may carry the Discord bot token, and it is rewritten at
	// runtime by dashboard saves: owner-only permissions, and an atomic
	// temp+rename swap so a crash mid-save can never truncate the live file.
	return util.WriteFileAtomic(path, data, 0o600)
}

// LogPredictionRiskClamps emits an explicit WARN for each prediction-risk value
// that is out of range and will therefore be clamped by ValidateConfig, so a
// clamp is never silent: a negative that becomes an off (0) gate, or a >100
// percent capped to 100, is surfaced with the field name, the original and
// applied values, and the source that supplied it (the config file or the
// Settings API). It does not mutate — ValidateConfig performs the actual clamp
// immediately afterwards. Call it from an ingestion point that knows its source.
func LogPredictionRiskClamps(pr PredictionRiskSettings, source string) {
	switch {
	case pr.MaxStakePercent < 0:
		slog.Warn("Clamped out-of-range prediction risk setting",
			"field", "maxStakePercent", "original", pr.MaxStakePercent, "applied", 0, "source", source)
	case pr.MaxStakePercent > 100:
		slog.Warn("Clamped out-of-range prediction risk setting",
			"field", "maxStakePercent", "original", pr.MaxStakePercent, "applied", 100, "source", source)
	}
	if pr.ReservePoints < 0 {
		slog.Warn("Clamped out-of-range prediction risk setting",
			"field", "reservePoints", "original", pr.ReservePoints, "applied", 0, "source", source)
	}
}

// ValidateConfig enforces min/max bounds on rate limits and other configurable values.
// It mutates the config in place, clamping out-of-range values to valid bounds.
func ValidateConfig(config *Config) {
	// Prediction risk gates: store exactly what is applied. MaxStakePercent is
	// 0 (off) or 1..100 — a stored 150 is clamped to 100, never silently applied
	// as 100 while 150 sits in the file. A negative value is not a hidden "off":
	// it is clamped to 0. ReservePoints is clamped to >= 0.
	if config.PredictionRisk.MaxStakePercent < 0 {
		config.PredictionRisk.MaxStakePercent = 0
	} else if config.PredictionRisk.MaxStakePercent > 100 {
		config.PredictionRisk.MaxStakePercent = 100
	}
	if config.PredictionRisk.ReservePoints < 0 {
		config.PredictionRisk.ReservePoints = 0
	}

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

	// Health canary: interval in [60, 1440] minutes (1h–24h), max staleness in
	// [1, 168] hours (1h–7d). The minimum interval bounds how often a real
	// beacon is sent.
	if config.Health.CanaryIntervalMinutes < 60 {
		config.Health.CanaryIntervalMinutes = 60
	} else if config.Health.CanaryIntervalMinutes > 1440 {
		config.Health.CanaryIntervalMinutes = 1440
	}
	if config.Health.CanaryMaxStalenessHours < 1 {
		config.Health.CanaryMaxStalenessHours = 1
	} else if config.Health.CanaryMaxStalenessHours > 168 {
		config.Health.CanaryMaxStalenessHours = 168
	}

	// Max staleness must cover at least one interval. Otherwise the forced-probe
	// condition (staleness exceeded) would fire before the opportunistic
	// interval-elapsed condition, and the hybrid schedule would degenerate into
	// "always force" — sending the beacon regardless of slot occupancy every
	// interval. Clamp staleness up to the interval (rounded up to whole hours),
	// mirroring the rotation max>=min clamp above.
	if minStalenessHours := (config.Health.CanaryIntervalMinutes + 59) / 60; config.Health.CanaryMaxStalenessHours < minStalenessHours {
		config.Health.CanaryMaxStalenessHours = minStalenessHours
	}

	// Drop-progress watchdog: stall delay in [10, 120] minutes (below ~15 the
	// normal Twitch crediting batch cadence would trip false alarms),
	// confirmations in [2, 10], recovery cooldown in [1, 60] minutes, avoid TTL
	// in [10, 360] minutes, rearm in [1, 48] hours.
	if config.Health.WatchdogStallDelayMinutes < 10 {
		config.Health.WatchdogStallDelayMinutes = 10
	} else if config.Health.WatchdogStallDelayMinutes > 120 {
		config.Health.WatchdogStallDelayMinutes = 120
	}
	if config.Health.WatchdogStallConfirmations < 2 {
		config.Health.WatchdogStallConfirmations = 2
	} else if config.Health.WatchdogStallConfirmations > 10 {
		config.Health.WatchdogStallConfirmations = 10
	}
	if config.Health.WatchdogRecoveryCooldownMinutes < 1 {
		config.Health.WatchdogRecoveryCooldownMinutes = 1
	} else if config.Health.WatchdogRecoveryCooldownMinutes > 60 {
		config.Health.WatchdogRecoveryCooldownMinutes = 60
	}
	if config.Health.WatchdogAvoidTTLMinutes < 10 {
		config.Health.WatchdogAvoidTTLMinutes = 10
	} else if config.Health.WatchdogAvoidTTLMinutes > 360 {
		config.Health.WatchdogAvoidTTLMinutes = 360
	}
	if config.Health.WatchdogRearmHours < 1 {
		config.Health.WatchdogRearmHours = 1
	} else if config.Health.WatchdogRearmHours > 48 {
		config.Health.WatchdogRearmHours = 48
	}

	// Campaign policy: canonicalize to a known mode (upper-cased; empty or
	// unknown → GAME_ORDER, the behavior-preserving default). Reuses the
	// engine's own validator so the valid-mode set has a single source.
	config.CampaignPolicy = string(policy.Normalize(config.CampaignPolicy))

	// Directory discovery mode: canonicalize to a known value; empty or unknown
	// → "all", the behavior-preserving default (mirrors CampaignPolicy above).
	config.DiscoveryMode = NormalizeDiscoveryMode(string(config.DiscoveryMode))

	// Daily summary: canonicalize the send time to a valid "HH:MM"; anything
	// unparseable falls back to the 09:00 default.
	config.DailySummary.Time, _ = NormalizeDailySummaryTime(config.DailySummary.Time)
}
