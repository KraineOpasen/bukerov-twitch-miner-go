package notifications

// NotificationConfig represents notification settings stored in the database.
type NotificationConfig struct {
	// Channel mappings
	MentionsChannelID string `json:"mentionsChannelId"`
	PointsChannelID   string `json:"pointsChannelId"`
	OnlineChannelID   string `json:"onlineChannelId"`
	OfflineChannelID  string `json:"offlineChannelId"`

	// Mention settings
	MentionsEnabled   bool     `json:"mentionsEnabled"`
	MentionsAllChats  bool     `json:"mentionsAllChats"`
	MentionsStreamers []string `json:"mentionsStreamers"`

	// Online notification settings
	OnlineEnabled      bool     `json:"onlineEnabled"`
	OnlineAllStreamers bool     `json:"onlineAllStreamers"`
	OnlineStreamers    []string `json:"onlineStreamers"`

	// Offline notification settings
	OfflineEnabled      bool     `json:"offlineEnabled"`
	OfflineAllStreamers bool     `json:"offlineAllStreamers"`
	OfflineStreamers    []string `json:"offlineStreamers"`

	// System notification settings - reauthorization required, connection
	// lost/restored. These are system-wide (not tied to a specific streamer).
	SystemChannelID string `json:"systemChannelId"`
	SystemEnabled   bool   `json:"systemEnabled"`

	// UpcomingDropsEnabled opts in to a one-time alert when Twitch first reports
	// a NEW relevant upcoming drop campaign. It is OFF by default so an update
	// never starts sending an event nobody asked for; the alert reuses the
	// system channel (SystemChannelID), so no Discord token/guild/channel
	// settings are duplicated.
	UpcomingDropsEnabled bool `json:"upcomingDropsEnabled"`
}

// PointRule represents a point threshold notification rule.
type PointRule struct {
	ID              int64  `json:"id"`
	Streamer        string `json:"streamer"`
	Threshold       int    `json:"threshold"`
	DeleteOnTrigger bool   `json:"deleteOnTrigger"`
	Triggered       bool   `json:"triggered"`
}

// DefaultNotificationConfig returns sensible defaults for new users.
func DefaultNotificationConfig() NotificationConfig {
	return NotificationConfig{
		MentionsEnabled:     false,
		MentionsAllChats:    true,
		OnlineEnabled:       false,
		OnlineAllStreamers:  true,
		OfflineEnabled:      false,
		OfflineAllStreamers: true,
		SystemEnabled:       true,
	}
}
