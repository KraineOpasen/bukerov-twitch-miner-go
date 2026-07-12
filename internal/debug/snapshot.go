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

	Watching     WatchingInfo    `json:"watching"`
	Streamers    []StreamerState `json:"streamers"`
	RecentEvents []events.Event  `json:"recentEvents"`
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
