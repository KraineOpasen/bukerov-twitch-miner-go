package notifications

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
)

// Discord notification embed colors
const (
	ColorMention            = 0x9146FF // Twitch purple
	ColorPoints             = 0xFFD700 // Gold
	ColorOnline             = 0x00FF00 // Green
	ColorOffline            = 0xFF4545 // Red
	ColorReauthRequired     = 0xFF0000 // Bright red
	ColorConnectionLost     = 0xFFA500 // Orange
	ColorConnectionRestored = 0x00FF00 // Green
	ColorUpdateAvailable    = 0x1E90FF // Dodger blue
)

// DiscordProvider implements the Provider interface for Discord notifications.
type DiscordProvider struct {
	botToken string
	guildID  string
	session  *discordgo.Session

	// Channel cache
	channelCache     []Channel
	channelCacheTime time.Time
	channelCacheTTL  time.Duration

	mu sync.RWMutex
}

// NewDiscordProvider creates a new Discord notification provider.
func NewDiscordProvider(botToken, guildID string) *DiscordProvider {
	return &DiscordProvider{
		botToken:        botToken,
		guildID:         guildID,
		channelCacheTTL: 5 * time.Minute,
	}
}

// Name returns the provider's identifier.
func (d *DiscordProvider) Name() string {
	return "discord"
}

// IsConfigured returns true if the provider has valid configuration.
func (d *DiscordProvider) IsConfigured() bool {
	return d.botToken != "" && d.guildID != ""
}

// Connect establishes connection to Discord.
func (d *DiscordProvider) Connect(ctx context.Context) error {
	if !d.IsConfigured() {
		return fmt.Errorf("discord not configured: missing bot token or guild ID")
	}

	session, err := discordgo.New("Bot " + d.botToken)
	if err != nil {
		return fmt.Errorf("failed to create Discord session: %w", err)
	}

	session.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages

	if err := session.Open(); err != nil {
		return fmt.Errorf("failed to open Discord connection: %w", err)
	}

	d.mu.Lock()
	d.session = session
	d.mu.Unlock()

	slog.Info("Discord notification provider connected", "guildID", d.guildID)
	return nil
}

// IsConnected reports whether the provider currently holds an open gateway
// session. It is a local lifecycle check (no network round-trip, no Discord API
// call) that the manager uses to decide whether an unchanged config still needs
// a Connect. A nil session means "not connected".
func (d *DiscordProvider) IsConnected() bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.session != nil
}

// Disconnect closes the Discord connection.
func (d *DiscordProvider) Disconnect() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.session != nil {
		if err := d.session.Close(); err != nil {
			return err
		}
		d.session = nil
	}
	return nil
}

// Send sends a notification to Discord.
func (d *DiscordProvider) Send(ctx context.Context, notification Notification) error {
	d.mu.RLock()
	session := d.session
	d.mu.RUnlock()

	if session == nil {
		return fmt.Errorf("discord not connected")
	}

	if notification.ChannelID == "" {
		return fmt.Errorf("no channel ID specified for notification")
	}

	color := notification.Color
	if color == 0 {
		switch notification.Type {
		case NotificationTypeMention:
			color = ColorMention
		case NotificationTypePointsReached:
			color = ColorPoints
		case NotificationTypeOnline:
			color = ColorOnline
		case NotificationTypeOffline:
			color = ColorOffline
		case NotificationTypeReauthRequired:
			color = ColorReauthRequired
		case NotificationTypeConnectionLost:
			color = ColorConnectionLost
		case NotificationTypeConnectionRestored:
			color = ColorConnectionRestored
		case NotificationTypeHealthDegraded:
			color = ColorConnectionLost
		case NotificationTypeHealthRecovered:
			color = ColorConnectionRestored
		case NotificationTypeDropStalled:
			color = ColorConnectionLost
		case NotificationTypeDropRecovered:
			color = ColorConnectionRestored
		case NotificationTypeUpdateAvailable:
			color = ColorUpdateAvailable
		case NotificationTypeUpdateFailed:
			color = ColorConnectionLost
		default:
			color = ColorMention
		}
	}

	embed := &discordgo.MessageEmbed{
		Title:       notification.Title,
		Description: notification.Message,
		Color:       color,
		Timestamp:   time.Now().Format(time.RFC3339),
		Footer: &discordgo.MessageEmbedFooter{
			Text: "Twitch Points Miner",
		},
	}

	if notification.Streamer != "" {
		embed.Author = &discordgo.MessageEmbedAuthor{
			Name: notification.Streamer,
			URL:  fmt.Sprintf("https://twitch.tv/%s", notification.Streamer),
		}
	}

	_, err := session.ChannelMessageSendEmbed(notification.ChannelID, embed)
	if err != nil {
		slog.Error("Failed to send Discord notification",
			"channel", notification.ChannelID,
			"type", notification.Type,
			"error", err,
		)
		return fmt.Errorf("failed to send Discord message: %w", err)
	}

	slog.Debug("Discord notification sent",
		"channel", notification.ChannelID,
		"type", notification.Type,
		"streamer", notification.Streamer,
	)
	return nil
}

// GetChannels returns available text channels in the configured guild.
func (d *DiscordProvider) GetChannels(ctx context.Context, forceRefresh bool) ([]Channel, error) {
	d.mu.RLock()
	session := d.session
	guildID := d.guildID
	cachedChannels := d.channelCache
	cacheTime := d.channelCacheTime
	cacheTTL := d.channelCacheTTL
	d.mu.RUnlock()

	if session == nil {
		return nil, fmt.Errorf("discord not connected")
	}

	// Return cached channels if still valid and not forcing refresh
	if !forceRefresh && cachedChannels != nil && time.Since(cacheTime) < cacheTTL {
		return cachedChannels, nil
	}

	channels, err := session.GuildChannels(guildID)
	if err != nil {
		return nil, fmt.Errorf("failed to get guild channels: %w", err)
	}

	var result []Channel
	for _, ch := range channels {
		if ch.Type == discordgo.ChannelTypeGuildText {
			result = append(result, Channel{
				ID:   ch.ID,
				Name: ch.Name,
				Type: "text",
			})
		}
	}

	// Update cache
	d.mu.Lock()
	d.channelCache = result
	d.channelCacheTime = time.Now()
	d.mu.Unlock()

	return result, nil
}

// UpdateConfig updates the Discord provider configuration.
func (d *DiscordProvider) UpdateConfig(botToken, guildID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.botToken = botToken
	d.guildID = guildID
}

// ValidateConfig checks if the Discord configuration is valid by attempting a connection.
func (d *DiscordProvider) ValidateConfig(ctx context.Context) error {
	if !d.IsConfigured() {
		return fmt.Errorf("missing bot token or guild ID")
	}

	session, err := discordgo.New("Bot " + d.botToken)
	if err != nil {
		return fmt.Errorf("invalid bot token: %w", err)
	}

	if err := session.Open(); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer func() { _ = session.Close() }()

	_, err = session.Guild(d.guildID)
	if err != nil {
		return fmt.Errorf("cannot access guild (check bot permissions): %w", err)
	}

	return nil
}
