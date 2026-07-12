package web

import "github.com/PatrickWalther/twitch-miner-go/internal/analytics"

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
}

type DashboardData struct {
	Username       string
	RefreshMinutes int
	Version        string
	TotalPoints    string
	StreamerCount  int
	PointsToday    string
	DiscordEnabled bool
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
}

type NotificationsPageData struct {
	Username       string
	RefreshMinutes int
	Version        string
	DiscordEnabled bool
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
