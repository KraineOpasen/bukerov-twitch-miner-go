package web

import (
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
)

type StreamerInfo struct {
	Name                  string `json:"name"`
	Points                int    `json:"points"`
	PointsFormatted       string `json:"points_formatted"`
	LastActivity          int64  `json:"last_activity"`
	LastActivityFormatted string `json:"last_activity_formatted"`
	IsLive                bool   `json:"is_live"`
	LiveDuration          string `json:"live_duration,omitempty"`
	OfflineDuration       string `json:"offline_duration,omitempty"`
	GameName              string `json:"game_name,omitempty"`
	Title                 string `json:"title,omitempty"`
	ViewersCount          int    `json:"viewers_count,omitempty"`
	ViewersCountFormatted string `json:"viewers_count_formatted,omitempty"`
	ChannelRestrictedDrop bool   `json:"channel_restricted_drop,omitempty"`
	Preference            string `json:"preference,omitempty"`

	// HasCampaign and the Campaign* fields drive the compact drop-progress
	// mini bar on the streamer card; populated only for live streamers with
	// an assigned, in-progress campaign.
	HasCampaign         bool   `json:"has_campaign,omitempty"`
	CampaignName        string `json:"campaign_name,omitempty"`
	CampaignDropName    string `json:"campaign_drop_name,omitempty"`
	CampaignPercent     int    `json:"campaign_percent,omitempty"`
	CampaignMinutesInfo string `json:"campaign_minutes_info,omitempty"`

	// --- Overview redesign additions (all sourced from in-memory state) ---

	// State is the single card lifecycle state used by the Overview card, one
	// of: "offline", "online", "queued", "watching", "disabled". It drives the
	// card's indicator shape/label/border (never colour alone).
	State string `json:"state,omitempty"`

	// Watching is true when this streamer currently occupies one of the two
	// Twitch watch slots; Queued is true when online but waiting its rotation
	// turn. DisableWatch mirrors the hard watch opt-out setting.
	Watching     bool `json:"watching,omitempty"`
	Queued       bool `json:"queued,omitempty"`
	DisableWatch bool `json:"disable_watch,omitempty"`

	// WatchReason is the watcher's human explanation for the current watch
	// decision (tooltip on the state indicator).
	WatchReason string `json:"watch_reason,omitempty"`

	// PointsPerHour is an approximate gain rate computed from the analytics
	// point series over the display window (empty when not computable).
	PointsPerHour string `json:"points_per_hour,omitempty"`
	PointsToday   string `json:"points_today,omitempty"`

	// StreakPending/StreakMinutes describe watch-streak progress toward the
	// ~7-minute threshold for the current broadcast (not a day count).
	StreakPending bool `json:"streak_pending,omitempty"`
	StreakMinutes int  `json:"streak_minutes,omitempty"`
	StreakPercent int  `json:"streak_percent,omitempty"`

	// LastEventText/LastEventAgo summarise the most recent notable event for
	// this streamer from the in-memory ring buffer.
	LastEventText string `json:"last_event_text,omitempty"`
	LastEventAgo  string `json:"last_event_ago,omitempty"`

	// Goal* fields carry the streamer's furthest-along active community goal.
	HasGoal     bool   `json:"has_goal,omitempty"`
	GoalTitle   string `json:"goal_title,omitempty"`
	GoalPercent int    `json:"goal_percent,omitempty"`

	// HasActivePrediction flags that a live prediction for this streamer is on
	// the board (so the card can show a subtle marker).
	HasActivePrediction bool `json:"has_active_prediction,omitempty"`
}

// TickerItem is one entry in the Overview events ticker (community goals and
// other notable, in-progress streamer events).
type TickerItem struct {
	Streamer string
	Kind     string // e.g. "goal", "moment", "drop"
	Label    string
	Percent  int
	HasPct   bool
}

// PredictionOutcomeView is one outcome row on the live-predictions board.
type PredictionOutcomeView struct {
	ID          string
	Title       string
	Color       string
	Percent     int
	Odds        string
	PointsLabel string
	Chosen      bool
	// Selectable is true when this outcome can be picked for a manual bet.
	Selectable bool
}

// PredictionView is one card on the live-predictions board, including the
// compact manual-betting controls and their states.
type PredictionView struct {
	Streamer         string
	Title            string
	Status           string // ACTIVE | LOCKED
	Locked           bool
	SecondsLeft      int
	SecondsLeftLabel string
	BetPlaced        bool
	BetConfirmed     bool
	BetAmount        string
	PoolLabel        string
	Outcomes         []PredictionOutcomeView
	WindowEndUnix    int64

	// --- manual-control fields ---

	// EventID is the stable round identifier the manual-bet / skip actions are
	// keyed on (never the streamer name or title).
	EventID string
	// ManualAllowed is true when a manual bet can be offered: the round is open,
	// the streamer is online, no bet is placed yet, and the balance covers the
	// minimum stake.
	ManualAllowed bool
	// ManualBet marks that the placed bet came from a manual action (vs
	// auto-bet); BetOutcomeTitle names the chosen outcome once a bet is placed.
	ManualBet       bool
	BetOutcomeTitle string
	// AutoBetSkipped is true when auto-bet is suppressed for this round;
	// SkipUndoable is true when that skip can still be undone (round open, no
	// bet placed). ManualPending reflects an in-flight manual placement and
	// ManualError the last human-readable manual failure.
	AutoBetSkipped bool
	SkipUndoable   bool
	ManualPending  bool
	ManualError    string
	// Balance is the streamer's current channel-point balance, shown as the
	// available amount and used for the quick-fill chips.
	Balance      int
	BalanceLabel string
	MinBet       int
}

// WatchSlotView is one of the (max two) active watch slots rendered in the
// pinned "Now Watching" sidebar block.
type WatchSlotView struct {
	Name          string
	Points        string
	Game          string
	StreakPending bool
	StreakMinutes int
	StreakPercent int
	HasGain       bool
	GainPerHour   string
	// Origin is the watch-slot source: "configured" (fixed streamer list) or
	// "discovery" (directory discovery). Discovery-occupied slots render a
	// badge and omit configured-only detail (points/streak/gain).
	Origin string
}

// NowWatchingView feeds the pinned sidebar block.
type NowWatchingView struct {
	Slots            []WatchSlotView
	QueuedNames      []string
	HasNextRotation  bool
	NextRotationUnix int64
	Mode             string
	Stale            bool
}

// OverviewData is the top-level view model for the redesigned Overview page.
type OverviewData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string

	BotStatus      string
	BotStatusLabel string
	Connected      bool
	Stale          bool
	ReauthRequired bool
	ConnectionLost bool

	TotalPoints   string
	StreamerCount int
	LiveCount     int
	PointsToday   string

	Ticker      []TickerItem
	Predictions []PredictionView
	NowWatching NowWatchingView

	TrackedLive    []StreamerInfo
	TrackedOffline []StreamerInfo
	Untracked      []StreamerInfo

	GeneratedUnix int64
}

// --- Provider view types (assembled by the miner from in-memory state) ---

// WatchSlotsView is the live watch-selection state supplied by the miner:
// which channels occupy the two watch slots (configured OR discovered), which
// are queued, and when the next rotation is due. Built from the unified slot
// broker's snapshot plus the watcher's debug state.
type WatchSlotsView struct {
	ActivePair []string
	Watching   map[string]bool
	Reason     map[string]string
	// Origin maps a watched channel to its slot source ("configured" or
	// "discovery").
	Origin map[string]string
	// Games maps a discovery-occupied channel to its game name (discovery
	// channels are not in the configured streamer list, so the sidebar cannot
	// look their game up there).
	Games          map[string]string
	Queued         []string
	NextRotationAt time.Time
	Mode           string
}

// LivePredictionOutcome mirrors one prediction outcome for the board.
type LivePredictionOutcome struct {
	ID              string
	Title           string
	Color           string
	PercentageUsers float64
	Odds            float64
	TotalPoints     int
	Chosen          bool
}

// LivePrediction is one tracked prediction event, supplied by the miner from
// the pubsub pool's in-memory snapshot.
type LivePrediction struct {
	Streamer                string
	EventID                 string
	Title                   string
	Status                  string
	CreatedAt               time.Time
	PredictionWindowSeconds float64
	BetPlaced               bool
	BetConfirmed            bool
	BetAmount               int
	TotalPoints             int
	Outcomes                []LivePredictionOutcome

	// --- manual-control state (mirrors pubsub.PredictionSnapshot) ---
	Online          bool
	Balance         int
	ManualBet       bool
	BetOutcomeTitle string
	AutoBetSkipped  bool
	ManualPending   bool
	ManualError     string
}

type DashboardData struct {
	Username       string
	RefreshMinutes int
	Version        string
	TotalPoints    string
	StreamerCount  int
	PointsToday    string
	DiscordEnabled bool
	DebugURL       string
}

type StreamerPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	Streamer       StreamerInfo
	PointsGained   string
	DataPoints     int
	DaysAgo        int
	DiscordEnabled bool
	DebugURL       string
}

type StreamerGridData struct {
	TrackedLive    []StreamerInfo
	TrackedOffline []StreamerInfo
	Untracked      []StreamerInfo
}

type SettingsPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string
}

type DropsPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string
}

type StatisticsPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string
	// Streamers is the list of streamer names with recorded history, used to
	// populate the page's streamer selector.
	Streamers []string
}

// DropDetailView is one drop within a campaign, rendered in the Drops-page
// modal so every reward in the campaign is visible individually (not just the
// current/final one shown on the card).
type DropDetailView struct {
	Name        string
	Benefit     string
	ImageURL    string
	Claimed     bool
	StatusLabel string
	Percent     int

	// HasMinuteProgress and the minute fields mirror the card's precise
	// watch-time bar; populated only for still-earnable drops with a known
	// minute requirement.
	HasMinuteProgress bool
	MinutesWatched    int
	MinutesRequired   int
}

// DropCampaignView is one row in the Drops-page campaign queue.
type DropCampaignView struct {
	ID                string
	Name              string
	GameName          string
	BoxArtURL         string
	DropName          string
	DropBenefit       string
	ChannelRestricted bool

	// Drops is the full per-drop breakdown for the campaign, shown in the
	// modal opened from the card.
	Drops []DropDetailView

	// Claimed marks an already-claimed campaign (Campaign.ClaimStatus);
	// StatusLabel is the human text shown as the status pill.
	Claimed     bool
	StatusLabel string

	// OverallPercent is the campaign's progress toward its full reward.
	OverallPercent int

	// HasMinuteProgress is true when Twitch reports exact watch minutes for
	// the current drop, enabling the precise minutes bar and remaining label.
	HasMinuteProgress bool
	MinutesWatched    int
	MinutesRequired   int
	MinutesRemaining  int
	MinutePercent     int
}

type DropsListData struct {
	Campaigns []DropCampaignView
}

// DiscoveredChannelView is one row in the Drops-page "Discovered Channels"
// section (the directory-discovery candidate pool).
type DiscoveredChannelView struct {
	Login            string
	Game             string
	Status           string
	ViewersFormatted string
	Watching         bool
	Offline          bool

	// MinutesWatched/HasMinutesWatched show accumulated watch time for the
	// channel currently occupying the discovery slot.
	MinutesWatched    int
	HasMinutesWatched bool
}

// DiscoveryListData feeds the discovery_list partial. Enabled is false while
// no directory games are configured, in which case the section renders a
// pointer to Settings instead of a channel table.
type DiscoveryListData struct {
	Enabled  bool
	Games    []string
	Channels []DiscoveredChannelView
}

type NotificationsPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string
	ConfigValid    bool
	ConfigError    string
	Streamers      []string
}

func convertStreamerInfo(info analytics.StreamerInfo) StreamerInfo {
	return StreamerInfo{
		Name:                  info.Name,
		Points:                info.Points,
		PointsFormatted:       info.PointsFormatted,
		LastActivity:          info.LastActivity,
		LastActivityFormatted: info.LastActivityFormatted,
		IsLive:                info.IsLive,
		LiveDuration:          info.LiveDuration,
		OfflineDuration:       info.OfflineDuration,
	}
}

func convertStreamerInfoList(infos []analytics.StreamerInfo) []StreamerInfo {
	result := make([]StreamerInfo, len(infos))
	for i, info := range infos {
		result[i] = convertStreamerInfo(info)
	}
	return result
}
