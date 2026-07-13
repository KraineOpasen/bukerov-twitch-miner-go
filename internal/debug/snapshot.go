// Package debug serves a localhost-only diagnostic HTTP API: a JSON snapshot
// of what the miner is doing (and why) at GET /debug/snapshot, and a tail of
// the log file at GET /debug/log. It is transport only - the snapshot itself
// is assembled by the miner, which is the one place that sees every
// component. Nothing security-sensitive (tokens, cookies) belongs in these
// types.
package debug

import (
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
)

// Snapshot is the JSON document served by GET /debug/snapshot.
type Snapshot struct {
	GeneratedAt   time.Time `json:"generatedAt"`
	Version       string    `json:"version"`
	Username      string    `json:"username"`
	Status        string    `json:"status"`
	StatusDetail  string    `json:"statusDetail,omitempty"`
	UptimeSeconds int64     `json:"uptimeSeconds"`

	Watching         WatchingInfo          `json:"watching"`
	Streamers        []StreamerState       `json:"streamers"`
	Drops            *DropsSyncInfo        `json:"drops,omitempty"`
	Discovery        *DiscoveryInfo        `json:"discovery,omitempty"`
	Health           *HealthInfo           `json:"health,omitempty"`
	ProgressWatchdog *ProgressWatchdogInfo `json:"progressWatchdog,omitempty"`
	RecentEvents     []events.Event        `json:"recentEvents"`
}

// ProgressWatchdogInfo is the drop-progress watchdog's per-drop state plus
// the active channel exclusions. Redacted by construction: statuses, counters,
// stage names, and channel logins only — never URLs or tokens.
type ProgressWatchdogInfo struct {
	Enabled     bool                `json:"enabled"`
	EvaluatedAt time.Time           `json:"evaluatedAt,omitzero"`
	Drops       []DropProgressState `json:"drops,omitempty"`
	Avoided     []AvoidedChannel    `json:"avoided,omitempty"`
}

type DropProgressState struct {
	Campaign             string    `json:"campaign"`
	Drop                 string    `json:"drop"`
	Channel              string    `json:"channel,omitempty"`
	Status               string    `json:"status"`
	LastMinutes          int       `json:"lastMinutes"`
	LastProgressAt       time.Time `json:"lastProgressAt,omitzero"`
	ReportsSinceProgress int       `json:"reportsSinceProgress"`
	NoProgressObs        int       `json:"noProgressObservations"`
	RecoveryStage        int       `json:"recoveryStage,omitempty"`
	RecoveryStageName    string    `json:"recoveryStageName,omitempty"`
	LastRecoveryAt       time.Time `json:"lastRecoveryAt,omitzero"`
	Detail               string    `json:"detail,omitempty"`
}

type AvoidedChannel struct {
	Login  string    `json:"login"`
	Until  time.Time `json:"until"`
	Reason string    `json:"reason,omitempty"`
}

// HealthInfo is the Health Center's aggregated signals in the debug snapshot.
// It is redacted by construction: no tokens, cookies, signed URLs, or headers.
type HealthInfo struct {
	ActiveClientID string         `json:"activeClientId,omitempty"`
	Signals        []HealthSignal `json:"signals"`
}

type HealthSignal struct {
	Name      string    `json:"name"`
	Status    string    `json:"status"`
	CheckedAt time.Time `json:"checkedAt,omitzero"`
	Stage     string    `json:"stage,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	ErrorCode string    `json:"errorCode,omitempty"`
}

// Miner statuses reported in Snapshot.Status.
const (
	StatusRunning   = "running"
	StatusPaused    = "paused"
	StatusAuthError = "auth_error"
)

// WatchingInfo describes the currently active watch selection: which pair of
// channels occupies the two Twitch watch slots and why.
type WatchingInfo struct {
	// Mode: "idle" (nothing online), "direct" (few enough online that all
	// are watched, priority order applies), or "rotation" (fair watch-pair
	// rotation between more channels than slots).
	Mode string `json:"mode"`
	// EvaluatedAt is when the watch loop last made a selection; the snapshot
	// reports that decision, it does not re-run selection.
	EvaluatedAt time.Time `json:"evaluatedAt,omitzero"`

	// ActivePair is the base rotation pair (ranked by least accumulated
	// watch time). A DROPS/STREAK boost can override one seat for a tick -
	// the per-streamer reasons show when that happens.
	ActivePair     []string  `json:"activePair,omitempty"`
	PairSince      time.Time `json:"pairSince,omitzero"`
	NextRotationAt time.Time `json:"nextRotationAt,omitzero"`

	// WatchTimeWindowHours is the trailing window over which per-streamer
	// watchedMinutesWindow accumulates for rotation fairness ranking.
	WatchTimeWindowHours float64 `json:"watchTimeWindowHours,omitempty"`

	// PostponedSwapOuts lists channels whose rotation swap-out is currently
	// deferred (e.g. about to complete a watch streak) - the closest thing
	// the rotation has to a "temporarily pinned/benched" list.
	PostponedSwapOuts []PostponedSwapOut `json:"postponedSwapOuts,omitempty"`

	// Slots is the unified slot broker's final allocation: which channel holds
	// each of the (at most two) Twitch watch slots, where it came from
	// (configured list or directory discovery), and why. Waiting lists
	// channels proposed for a slot that did not get one this tick.
	Slots   []WatchSlot   `json:"slots,omitempty"`
	Waiting []WaitingSlot `json:"waiting,omitempty"`
}

// WatchSlot is one occupied watch slot in the broker's explainable allocation.
type WatchSlot struct {
	Slot       int    `json:"slot"`
	Channel    string `json:"channel"`
	Source     string `json:"source"`
	ReasonCode string `json:"reasonCode"`
	Reason     string `json:"reason"`
	Campaign   string `json:"campaign,omitempty"`
}

// WaitingSlot is a channel proposed for a slot that did not get one this tick.
type WaitingSlot struct {
	Channel    string `json:"channel"`
	Source     string `json:"source"`
	ReasonCode string `json:"reasonCode"`
	Reason     string `json:"reason"`
}

type PostponedSwapOut struct {
	Username string    `json:"username"`
	Until    time.Time `json:"until"`
}

// StreamerState is the per-streamer section of the snapshot.
type StreamerState struct {
	Username      string `json:"username"`
	Online        bool   `json:"online"`
	Watching      bool   `json:"watching"`
	Reason        string `json:"reason"`
	ChannelPoints int    `json:"channelPoints"`
	Preference    string `json:"preference,omitempty"`

	OnlineSince  time.Time `json:"onlineSince,omitzero"`
	OfflineSince time.Time `json:"offlineSince,omitzero"`
	Game         string    `json:"game,omitempty"`
	Title        string    `json:"title,omitempty"`

	// WatchedMinutesWindow is this channel's accumulated watch time within
	// the trailing watching.watchTimeWindowHours window - the metric the
	// rotation ranks by (lower = more "owed" a watch slot).
	WatchedMinutesWindow float64 `json:"watchedMinutesWindow"`

	DropCampaigns    []DropCampaignInfo `json:"dropCampaigns,omitempty"`
	ActivePrediction *PredictionInfo    `json:"activePrediction,omitempty"`
	WatchStreak      *WatchStreakInfo   `json:"watchStreak,omitempty"`
}

type DropCampaignInfo struct {
	Name              string    `json:"name"`
	Game              string    `json:"game,omitempty"`
	EndAt             time.Time `json:"endAt,omitzero"`
	ChannelRestricted bool      `json:"channelRestricted"`
	RemainingDrops    int       `json:"remainingDrops"`
}

type PredictionInfo struct {
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	CreatedAt    time.Time `json:"createdAt"`
	BetPlaced    bool      `json:"betPlaced"`
	BetConfirmed bool      `json:"betConfirmed"`
	BetAmount    int       `json:"betAmount,omitempty"`
}

// WatchStreakInfo is present while a watch-streak bonus is still pending for
// the current stream (streaks complete at ~7 watched minutes).
type WatchStreakInfo struct {
	Pending        bool    `json:"pending"`
	MinutesWatched float64 `json:"minutesWatched"`
}

// DiscoveryInfo describes the directory-discovery subsystem: the configured
// games, the discovered candidate pool, and which channel occupies the extra
// discovery watch slot. Present only while discovery is enabled.
type DiscoveryInfo struct {
	Games      []string           `json:"games"`
	Watching   string             `json:"watching,omitempty"`
	LastSyncAt time.Time          `json:"lastSyncAt,omitzero"`
	Channels   []DiscoveryChannel `json:"channels,omitempty"`
}

// DropsSyncInfo mirrors the drop-campaign sync pipeline's output and its
// health: the campaigns the tracker currently holds (the exact set the /drops
// page renders), when the last sync completed, how many runs have happened,
// what Twitch's dashboard listing returned, how many campaigns were recovered
// from the inventory's in-progress list, and the last error (if any). It makes
// an empty Drops page diagnosable without -debug — telling a campaign that was
// filtered out apart from a sync that never ran or one that errored.
type DropsSyncInfo struct {
	LastSyncAt             time.Time             `json:"lastSyncAt,omitzero"`
	SyncRuns               int                   `json:"syncRuns"`
	DashboardCampaigns     int                   `json:"dashboardCampaigns"`
	RecoveredFromInventory int                   `json:"recoveredFromInventory"`
	TrackedCampaigns       int                   `json:"trackedCampaigns"`
	LastError              string                `json:"lastError,omitempty"`
	Campaigns              []TrackedCampaignInfo `json:"campaigns,omitempty"`
}

// TrackedCampaignInfo is one campaign as held by the drops tracker, including
// whether it was recovered from the inventory's in-progress list.
type TrackedCampaignInfo struct {
	Name              string    `json:"name"`
	Game              string    `json:"game,omitempty"`
	EndAt             time.Time `json:"endAt,omitzero"`
	RemainingDrops    int       `json:"remainingDrops"`
	OverallPercent    int       `json:"overallPercent"`
	ClaimStatus       string    `json:"claimStatus,omitempty"`
	ChannelRestricted bool      `json:"channelRestricted"`
	InInventory       bool      `json:"inInventory"`
}

type DiscoveryChannel struct {
	Login   string `json:"login"`
	Game    string `json:"game"`
	Viewers int    `json:"viewers"`
	// Status: "watching", "available", or "offline".
	Status         string  `json:"status"`
	MinutesWatched float64 `json:"minutesWatched,omitempty"`
}
