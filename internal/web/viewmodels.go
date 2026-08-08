package web

import (
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type StreamerInfo struct {
	Name                  string `json:"name"`
	Points                int    `json:"points"`
	PointsFormatted       string `json:"points_formatted"`
	LastActivity          int64  `json:"last_activity"`
	LastActivityFormatted string `json:"last_activity_formatted"`
	// Status is the tri-state liveness: "unknown" | "online" | "offline". IsLive
	// is kept as a DERIVED, backward-compatible field: is_live == (status ==
	// "online"). Clients that only read is_live keep working; clients that need to
	// tell "offline" apart from "status could not be confirmed" read status.
	Status string `json:"status"`
	IsLive bool   `json:"is_live"`
	// Unconfirmed marks a streamer whose live status is currently UNKNOWN
	// (transport/GQL failure) — including one still holding a watch slot during a
	// transient blip. Rendered as a distinct "unconfirmed" indication, never as a
	// red offline state.
	Unconfirmed     bool   `json:"unconfirmed,omitempty"`
	LiveDuration    string `json:"live_duration,omitempty"`
	OfflineDuration string `json:"offline_duration,omitempty"`
	GameName        string `json:"game_name,omitempty"`
	Title           string `json:"title,omitempty"`
	// Tags carries up to maxCardTags of the stream's Twitch tags (localized
	// names), rendered as compact chips on the live card.
	Tags                  []string `json:"tags,omitempty"`
	ViewersCount          int      `json:"viewers_count,omitempty"`
	ViewersCountFormatted string   `json:"viewers_count_formatted,omitempty"`
	ChannelRestrictedDrop bool     `json:"channel_restricted_drop,omitempty"`
	Preference            string   `json:"preference,omitempty"`

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

	// StreakPending/StreakMinutes/StreakCapMinutes describe watch-streak progress
	// across the bounded 20-minute pursuit window for the current broadcast (not a
	// day count). StreakCapMinutes is the progress-bar denominator (the watcher's
	// hard pursuit cap), so the template never hardcodes its own copy.
	StreakPending    bool `json:"streak_pending,omitempty"`
	StreakMinutes    int  `json:"streak_minutes,omitempty"`
	StreakCapMinutes int  `json:"streak_cap_minutes,omitempty"`
	StreakPercent    int  `json:"streak_percent,omitempty"`

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
	Name             string
	Points           string
	Game             string
	StreakPending    bool
	StreakMinutes    int
	StreakCapMinutes int
	StreakPercent    int
	HasGain          bool
	GainPerHour      string
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

	// EmptyPad is the S5-3 C12 padding (task Phase 4): exactly enough empty
	// slot boxes to bring the sidebar's total up to two, built from the safe
	// watchSlotEvidence adapter. Len is 0 (two real Slots already), 1, or 2 -
	// never causes more than two slot-shaped boxes to render in total.
	EmptyPad []c12SlotData
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
	// NetState drives the Overview network indicator (wifi icon): "ok" (green),
	// "degraded" (yellow — impaired link), or "lost" (red). Computed with "lost"
	// taking precedence so a fully-lost link never renders as merely degraded.
	NetState string

	TotalPoints   string
	StreamerCount int
	LiveCount     int
	PointsToday   string

	Ticker      []TickerItem
	Predictions []PredictionView
	NowWatching NowWatchingView

	TrackedLive    []StreamerInfo
	TrackedUnknown []StreamerInfo
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
	TrackedUnknown []StreamerInfo
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

	// PolicyMode is the active campaign-policy mode; PolicyModes are the
	// selectable options for the mode selector.
	PolicyMode  string
	PolicyModes []string
}

// LogLineView is one rendered log line: Class is the semantic color class and
// Emoji the decorative icon, both assigned by classifyLogLine (logclass.go);
// Text is the raw log line, rendered untouched. Emoji is empty when the raw
// line already starts with its own emoji, so icons are never doubled.
type LogLineView struct {
	Class string
	Emoji string
	Text  string
}

// LogsPageData feeds both /logs and its /system/logs alias (task S5-5) via
// the identical logs.html template and buildLogsPageData builder. The
// redacted support-bundle download used to live here
// (SupportBundleAvailable); task S5-5 gave that link a single owner
// (/system/diagnostics — see SystemDiagnosticsPageData) and removed the
// field along with its {{if}} block in logs.html.
type LogsPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string
	Lines          []LogLineView
	// FileLogging is false when the log file doesn't exist (file logging off),
	// so the page can explain how to enable it.
	FileLogging bool
}

// LogsLinesData feeds the logs_lines partial refreshed by htmx.
type LogsLinesData struct {
	Lines       []LogLineView
	FileLogging bool
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
	// BetStrategies are the strategies that appear in recorded prediction bets,
	// used to populate the ROI strategy filter (empty when no bets exist yet).
	BetStrategies []string
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

	// Percent is the drop's overall reward-progress percentage (kept for
	// existing readers). Progress carries the SAME value through the C11
	// component (task S5-4/R17): Mode is "unknown" when Twitch has never
	// supplied an authoritative inventory observation for this still-earnable
	// drop (no minted DropInstanceID yet AND no watched-minute observation
	// yet) — rendered as C11's dash+"unknown" state, never a fabricated 0%.
	// Already-claimed drops (from ClaimedDropNames) always carry a
	// determinate 100% in both fields.
	Percent  int
	Progress ProgressData

	// HasMinuteProgress and the minute fields mirror the card's precise
	// watch-time bar; populated only for still-earnable drops with a known
	// minute requirement.
	HasMinuteProgress bool
	MinutesWatched    int
	MinutesRequired   int
}

// DropHealthView is the drop-progress watchdog's badge on a campaign card:
// HEALTHY / RECOVERING / STALLED plus up to three explanatory lines (last
// progress age, farming channel, delivered reports, current recovery stage).
type DropHealthView struct {
	Status     string // health.ProgressHealthy / ProgressRecovering / ProgressStalled
	Label      string // "HEALTHY" / "RECOVERING" / "STALLED"
	BadgeColor string // inline hex, mirrors healthStatusDisplay's palette
	Lines      []string
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

	// MinutePercent is the watch-time percentage (kept for existing readers).
	// MinuteProgress carries the SAME value through the C11 component (task
	// S5-4/R17): Mode is "unknown" — never a fabricated 0% — when Twitch has
	// never supplied an authoritative inventory observation for this drop (no
	// minted DropInstanceID yet AND no watched-minute observation yet). Zero
	// value when HasMinuteProgress is false (the drop has no minute
	// requirement at all, a distinct case from "unknown").
	MinutePercent  int
	MinuteProgress ProgressData

	// Health is the progress watchdog's state for the campaign's current drop
	// (nil when the watchdog is disabled or does not track this campaign).
	Health *DropHealthView

	// Policy is the campaign-policy engine's decision + per-drop controls for
	// this campaign's current drop (nil when no decision was published).
	Policy *DropPolicyView

	// Upcoming marks a display-only campaign that has not started yet;
	// StartsInLabel is its "starts in Xh" / "starts on <date>" text.
	Upcoming      bool
	StartsInLabel string

	// AccountLinkBadge is the B11 evidence gate (task S5-4): zero value (empty
	// Label) when Twitch never authoritatively reported the campaign's
	// account-link state — the card renders nothing at all (S-NOBACK), never
	// a placeholder. Set only from Campaign.AccountConnection's proven
	// Connected/Disconnected values, never inferred.
	AccountLinkBadge BadgeData

	// DPCBadge is the DP-C outline badge (Stage 4 §13): shown on every
	// populated Current card until Group C evidence upgrades it, by a
	// separate future task — never presented as parity.
	DPCBadge BadgeData

	// Chip is the C0 freshness footer shared by every populated card on the
	// Current tab (task R17 item 2): the last SUCCESSFUL full-sync clock,
	// distinct from any failed-attempt strip shown above the list. Zero value
	// when there has never been a successful sync (card list is then empty
	// anyway, so this only matters once campaigns exist).
	Chip ProvenanceChipData
}

// PastCampaignGroup is a recurring campaign identity in the "Past" tab: all
// expired instances that share a campaign_key (game + campaign name), grouped
// under one heading with a per-instance breakdown.
type PastCampaignGroup struct {
	CampaignKey  string
	Name         string
	GameName     string
	BoxArtURL    string
	Count        int
	ClaimedCount int
	LastEnded    string // date the most recent instance ended
	Instances    []PastInstanceView
}

// PastInstanceView is one expired campaign instance within a group.
type PastInstanceView struct {
	CampaignID  string
	StartLabel  string
	EndLabel    string
	Claimed     bool
	StatusLabel string
}

// DropsPastData is the payload for the "Past" tab partial.
type DropsPastData struct {
	Groups []PastCampaignGroup
}

// PolicyFactorView is one line of a policy decision's score breakdown.
type PolicyFactorView struct {
	Points int
	Label  string
}

// DropPolicyView is the campaign-policy badge and per-drop controls on a Drops
// card. Feasibility is an ESTIMATE, never a guaranteed drop.
type DropPolicyView struct {
	Status        string // SAFE / AT_RISK / NEXT_REWARD_ONLY / IMPOSSIBLE
	StatusColor   string // inline hex
	StatusLabel   string // human label
	Total         int
	Excluded      bool
	ExcludeReason string
	Factors       []PolicyFactorView

	TimeUntilEnd          string
	MinutesToNextReward   int
	CanCompleteNextReward bool
	CanCompleteAll        bool

	// Per-drop controls for the campaign's current drop, keyed by reward key.
	RewardKey            string
	Skip                 bool
	HighPriority         bool
	AlwaysFinishStarted  bool
	NextRewardOnly       bool
	IgnoreSubscriberOnly bool
	// SubscriberOnlyKnown is false when Twitch never reported the flag, so the
	// UI shows the "Ignore subscriber-only" control as having no effect.
	SubscriberOnlyKnown bool
}

type DropsListData struct {
	Campaigns []DropCampaignView

	// R17 sync-status evidence for the Current tab (task S5-4 Phase 4), each
	// nil when not applicable. Built only from CampaignsProvider.SyncStatus()
	// fields already returned today (LastSyncAt/LastSuccessAt/LastError/
	// IntervalMinutes) — no fabricated backend state. At most one of
	// NeverSyncedState/EmptyState is ever set; DegradedStrip is independent
	// (a failed LAST attempt can co-exist with retained cards from an earlier
	// success).
	//
	// NeverSyncedState (S-UNK) shows when no sync has ever completed or
	// failed (LastSyncAt and LastSuccessAt both zero).
	NeverSyncedState *StateBlockData
	// DegradedStrip (S-DEGR) shows the FAILED-ATTEMPT clock (LastSyncAt +
	// LastError) — distinct from the freshness chip's SUCCESS clock — above
	// whatever cards remain retained from the last-known-good pool.
	DegradedStrip *StateBlockData
	// EmptyState (S-EMPTY) shows only when the last sync succeeded and
	// legitimately found zero campaigns — never confused with a failure.
	EmptyState *StateBlockData
}

// UpcomingState is the honest lifecycle state of the Drops "Upcoming" tab, so it
// never reuses the active tab's empty message or active-progress UI.
type UpcomingState string

const (
	// UpcomingStateNeverSynced: no successful full campaign sync has completed yet.
	UpcomingStateNeverSynced UpcomingState = "never_synced"
	// UpcomingStateEmpty: the last sync succeeded and Twitch reported no upcoming
	// campaigns (for this account, at that moment) — not a claim none exist.
	UpcomingStateEmpty UpcomingState = "empty"
	// UpcomingStateStale: the last refresh failed. Any cached upcoming cards are
	// still shown (Campaigns non-empty) with a stale/error note; when there is no
	// cache the tab shows the refresh-failed message instead.
	UpcomingStateStale UpcomingState = "stale"
	// UpcomingStatePopulated: the last sync succeeded and real upcoming campaigns
	// are shown.
	UpcomingStatePopulated UpcomingState = "populated"
)

// UpcomingCampaignView is one display-only upcoming-campaign card. It carries
// NO active-farming fields (progress bar, watched minutes, health, priority):
// an upcoming campaign has not started and is never farmed.
type UpcomingCampaignView struct {
	ID        string
	Name      string
	GameName  string
	BoxArtURL string
	// StartLocal / EndLocal are absolute local date+time strings (rendered in the
	// dashboard's configured time zone); EndLocal is empty when unknown.
	StartLocal string
	EndLocal   string
	// StartsIn is the relative "starts in …" label.
	StartsIn string
	// Rewards are the campaign's reward names, when Twitch provided them.
	Rewards []string
}

// DropsUpcomingData feeds the dedicated Upcoming tab partial with its honest
// state and the display-only cards.
type DropsUpcomingData struct {
	State UpcomingState
	// HasError is true when the last refresh failed (drives the stale/error note
	// even when cached cards are shown).
	HasError bool
	// LastSuccessText is the localized absolute time of the last successful sync
	// (empty when there has never been one).
	LastSuccessText string
	Campaigns       []UpcomingCampaignView
}

// DropsUpcomingPageData is the /drops/upcoming direct-render page shell (task
// S5-4 Phase 3/5): a thin wrapper around the existing drops_upcoming partial,
// fed by the existing /api/drops/upcoming endpoint exactly as the Current
// page's former inline tab already did — no new endpoint, no new polling
// cadence.
type DropsUpcomingPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string
}

// DropsPastPageData is the /drops/past direct-render page shell (task S5-4
// Phase 3/5): a thin wrapper around the existing drops_past partial, fed by
// the existing /api/drops/past endpoint, load-only (no polling), exactly as
// the Current page's former inline tab already did.
type DropsPastPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string
}

// ClaimRowView is one drop-level claim row on /drops/claims (task S5-4 Phase
// 5), built entirely from the SAME in-memory CampaignsProvider.Campaigns()
// evidence the Current tab already reads — no new provider method, no
// persistence (B2 remains a backend dependency). State is one of "claimed" /
// "claimable" / "in_progress" / "unknown", derived only from
// models.Drop.Claimability (the authoritative, server-derived, never-inferred
// field) and Campaign.ClaimedDropNames — an "unknown" row is NEVER promoted
// to claimed/failed/completed/delivered. There is deliberately no per-row
// timestamp: no authoritative claim-time evidence exists yet, and inventing
// one would be exactly the fabrication this task forbids.
type ClaimRowView struct {
	CampaignID   string
	CampaignName string
	GameName     string
	DropName     string
	Benefit      string

	State       string // "claimed" | "claimable" | "in_progress" | "unknown"
	StatusLabel string
	Badge       BadgeData
}

// DropsClaimsPageData is the /drops/claims direct-render page (task S5-4
// Phase 5): the sole owner of claim lifecycle across the Drops section.
// Fully server-rendered at request time — no htmx, no polling, no new API
// endpoint — since there is no live claim event stream to poll, only the
// current in-memory snapshot. Unavailable is true when no campaigns provider
// is wired at all (a distinct S-SESS/no-evidence case, never rendered as "no
// claims").
type DropsClaimsPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string

	Rows        []ClaimRowView
	Unavailable bool

	// CampaignOptions is the distinct, order-preserving set of campaigns
	// appearing in Rows (ID for filtering/matching, Name for display),
	// precomputed once so the campaign filter <select> never needs a
	// template-side dict/grouping helper. Keyed by CampaignID rather than
	// name so two recurring/regional campaigns that happen to share a name
	// never collapse into one filter option.
	CampaignOptions []ClaimCampaignOption
}

// ClaimCampaignOption is one entry in the Claims page's campaign filter.
type ClaimCampaignOption struct {
	ID   string
	Name string
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

// --- S5-2 component library view types (C0/C1/C10/C11, Stage 4 §6) ---

// ProvenanceChipData feeds the C0 provenance-chip component: freshness age,
// source and session evidence for exactly one live data region.
type ProvenanceChipData struct {
	AgeLabel string
	Source   string
	Session  bool
	Aged     bool
	Unknown  bool
}

// StateBlockData feeds the C1 state-block component: the single host for the
// non-loading, non-ready S-states. State is one of the 9 C1 state codes
// (EMPTY/PART/STALE/UNK/DEGR/FAIL/BLOCK/DENY/DEFER); an empty State (or
// "NOBACK") renders nothing — S-NOBACK is the absence of a control, never a
// greyed-out placeholder.
type StateBlockData struct {
	State        string
	Variant      string // "block" | "strip" | "inline"
	Message      string
	Cause        string
	Time         string
	ActionLabel  string
	ActionTarget string
}

// BadgeData feeds the C10 badge component: a compact icon+text+tier status
// encoding (channel status, claim state, reason code, session marker, etc).
type BadgeData struct {
	Tier  string // "ok" | "info" | "caution" | "danger" | "neutral"
	Icon  string
	Label string
}

// ProgressData feeds the C11 progress component. Mode "unknown" renders no
// bar at all (S-UNK must never read as 0%); "indeterminate" is the thin
// sliding-stripe variant reserved for in-flight sync attempts; "determinate"
// is the normal percent bar with the mono percent rendered beside it.
type ProgressData struct {
	Mode    string // "determinate" | "unknown" | "indeterminate"
	Percent int
	Label   string
}

// EventsPageData feeds the minimal /events compatibility landing (S5-2): a
// direct-rendered page that honestly states the Discord-availability of the
// current notification configuration without implementing the future event
// journal (deferred to S5-7).
type EventsPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string
	// DisabledState is the C1 state-block payload shown when Discord is
	// disabled (a neutral/empty state pointing at /settings); zero-valued
	// and unused when DiscordEnabled is true.
	DisabledState StateBlockData
}

// HelpPageData feeds the minimal /help/getting-started compatibility
// landing (S5-2): orientation to the seven live sections only, no glossary
// or troubleshooting content (deferred to S5-9).
type HelpPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string
}

// SystemStatusPageData feeds /system/status (task S5-5): a pure
// server-rendered snapshot built only from data already collected in
// memory by existing providers — no htmx, no polling, no new API endpoint.
// A same-route manual refresh link is the page's only "live" affordance.
// Signals lists the lifecycle + OAuth/GQL-API/PubSub/drops-sync rows in
// fixed display order (see handlers_system.go for SystemStatusRowView and
// the builders).
type SystemStatusPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string

	Signals   []SystemStatusRowView
	Resources SystemResourcesView
}

// SystemDiagnosticsPageData feeds /system/diagnostics (task S5-5): deeper,
// still purely server-rendered operational evidence — the watch-transport
// canary signal, the drops-progress watchdog's per-status drop counts,
// best-effort update-availability evidence, and the canonical support-bundle
// download link (shown only when the dashboard has real authentication
// configured — see handlers_logs.go's identical AuthEnabled() gate, which
// this page now solely owns instead of the old /logs-page control).
type SystemDiagnosticsPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
	DebugURL       string

	Signals      []SystemStatusRowView
	DropProgress SystemDropProgressView
	Update       SystemUpdateView

	SupportBundleAvailable bool
}

func convertStreamerInfo(info analytics.StreamerInfo) StreamerInfo {
	// analytics.StreamerInfo carries only a boolean IsLive (from stored history),
	// so its tri-state status is derived: online when live, otherwise offline.
	// The live-tracked overview cards get their real tri-state from the streamer
	// model in buildCards; this DB-derived path preserves the existing is_live
	// grouping for backward compatibility.
	status := models.StatusOffline.String()
	if info.IsLive {
		status = models.StatusOnline.String()
	}
	return StreamerInfo{
		Name:                  info.Name,
		Points:                info.Points,
		PointsFormatted:       info.PointsFormatted,
		LastActivity:          info.LastActivity,
		LastActivityFormatted: info.LastActivityFormatted,
		Status:                status,
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
