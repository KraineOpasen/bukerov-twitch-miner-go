package notifications

import "context"

// NotificationType represents the type of notification being sent.
type NotificationType string

const (
	NotificationTypeMention            NotificationType = "mention"
	NotificationTypePointsReached      NotificationType = "points"
	NotificationTypeOnline             NotificationType = "online"
	NotificationTypeOffline            NotificationType = "offline"
	NotificationTypeReauthRequired     NotificationType = "reauth_required"
	NotificationTypeConnectionLost     NotificationType = "connection_lost"
	NotificationTypeConnectionRestored NotificationType = "connection_restored"
	NotificationTypeUpdateAvailable    NotificationType = "update_available"
	NotificationTypeHealthDegraded     NotificationType = "health_degraded"
	NotificationTypeHealthRecovered    NotificationType = "health_recovered"
)

// Notification represents a notification to be sent.
type Notification struct {
	Type      NotificationType
	Title     string
	Message   string
	Streamer  string
	ChannelID string
	Color     int
}

// Provider defines the interface for notification providers.
// This allows easy extension to support other providers (e.g., Telegram, Slack, etc.)
type Provider interface {
	// Name returns the provider's identifier.
	Name() string

	// IsConfigured returns true if the provider has valid configuration.
	IsConfigured() bool

	// Connect establishes connection to the notification service.
	Connect(ctx context.Context) error

	// Disconnect closes the connection.
	Disconnect() error

	// Send sends a notification.
	Send(ctx context.Context, notification Notification) error

	// GetChannels returns available channels for the user to select from.
	GetChannels(ctx context.Context) ([]Channel, error)
}

// Channel represents a notification destination channel.
type Channel struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}
