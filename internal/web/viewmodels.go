package web

import "github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"

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
