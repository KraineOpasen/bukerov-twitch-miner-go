package notifications

import (
	"context"
	"errors"
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
	// channelCacheGeneration is the configGeneration the cached channels belong
	// to; a cache hit is honoured only while it still equals configGeneration.
	channelCacheGeneration uint64

	// configGeneration starts at 0 and increments on every real credential
	// change (see UpdateConfig). It lets an in-flight GetChannels detect that its
	// result belongs to a superseded bot/guild and must be discarded instead of
	// returned or cached.
	configGeneration uint64

	// closeSession closes a gateway session. It is a seam so tests can drive the
	// Disconnect lifecycle (including injecting a Close error) without a real
	// network connection; production closes the real session.
	closeSession func(*discordgo.Session) error
	// fetchGuildChannels fetches a guild's channels over the network. Seam so
	// tests can control an in-flight request; production calls the REST endpoint.
	fetchGuildChannels func(*discordgo.Session, string) ([]*discordgo.Channel, error)
	// validateConfig runs the actual credential validation against Discord. Seam
	// for tests; production dials the gateway and checks guild access.
	validateConfig func(ctx context.Context, botToken, guildID string) error

	mu sync.RWMutex
}

// NewDiscordProvider creates a new Discord notification provider.
func NewDiscordProvider(botToken, guildID string) *DiscordProvider {
	return &DiscordProvider{
		botToken:        botToken,
		guildID:         guildID,
		channelCacheTTL: 5 * time.Minute,
		closeSession:    func(s *discordgo.Session) error { return s.Close() },
		fetchGuildChannels: func(s *discordgo.Session, guildID string) ([]*discordgo.Channel, error) {
			return s.GuildChannels(guildID)
		},
		validateConfig: defaultValidateConfig,
	}
}

// defaultValidateConfig is the production credential validator: it opens a
// throwaway session and confirms the bot can access the guild. It runs with no
// provider lock held (ValidateConfig snapshots the credentials first).
func defaultValidateConfig(_ context.Context, botToken, guildID string) error {
	session, err := discordgo.New("Bot " + botToken)
	if err != nil {
		return fmt.Errorf("invalid bot token: %w", err)
	}
	if err := session.Open(); err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer func() { _ = session.Close() }()

	if _, err := session.Guild(guildID); err != nil {
		return fmt.Errorf("cannot access guild (check bot permissions): %w", err)
	}
	return nil
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
	// Snapshot the credentials under the lock, then run the network Open with no
	// lock held; publish the live session only after a successful Open.
	d.mu.RLock()
	botToken := d.botToken
	guildID := d.guildID
	d.mu.RUnlock()

	if botToken == "" || guildID == "" {
		return fmt.Errorf("discord not configured: missing bot token or guild ID")
	}

	session, err := discordgo.New("Bot " + botToken)
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

	slog.Info("Discord notification provider connected", "guildID", guildID)
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

// Disconnect closes the Discord connection. It detaches the session under the
// lock BEFORE closing it, so IsConnected() reports false immediately and a
// Close error can never resurrect a false "connected" state. The network Close
// runs with no lock held; its error is returned to the caller.
func (d *DiscordProvider) Disconnect() error {
	d.mu.Lock()
	session := d.session
	d.session = nil
	d.mu.Unlock()

	if session == nil {
		return nil
	}
	return d.closeSession(session)
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

// errChannelConfigChanged is returned when the Discord connection config kept
// changing while the channel list was being fetched, so no fresh result could be
// committed. It is retryable and safe to log: a later GetChannels call fetches
// again from scratch (the error is never cached). It carries no credentials.
var errChannelConfigChanged = errors.New("discord channel list unavailable: connection config changed during load")

// GetChannels returns available text channels in the configured guild.
//
// A result from a request that was in flight while the connection config changed
// (token/guild — tracked by configGeneration — or a session replacement) is
// never returned or cached: the post-fetch snapshot check is atomic with the
// cache write, so UpdateConfig cannot slip in between. On a stale result it
// retries once with a fresh snapshot; if that is stale too it returns a
// retryable error rather than ever surfacing the previous bot/guild's channels.
func (d *DiscordProvider) GetChannels(ctx context.Context, forceRefresh bool) ([]Channel, error) {
	const maxAttempts = 2
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		d.mu.RLock()
		session := d.session
		guildID := d.guildID
		generation := d.configGeneration
		cache := d.channelCache
		cacheGen := d.channelCacheGeneration
		cacheTime := d.channelCacheTime
		cacheTTL := d.channelCacheTTL
		d.mu.RUnlock()

		if session == nil {
			return nil, fmt.Errorf("discord not connected")
		}

		// Cache hit only for the current generation and within the TTL.
		if !forceRefresh && cache != nil && cacheGen == generation && time.Since(cacheTime) < cacheTTL {
			return cache, nil
		}

		channels, err := d.fetchGuildChannels(session, guildID)
		if err != nil {
			return nil, fmt.Errorf("failed to get guild channels: %w", err)
		}
		result := textChannels(channels)

		// Commit the result ONLY if the snapshot is still current — same
		// generation (token/guild) and the same session pointer. The check and
		// the cache write share one critical section so UpdateConfig cannot
		// interleave. A stale result is dropped (never cached, never returned).
		d.mu.Lock()
		current := d.configGeneration == generation && d.session == session
		if current {
			d.channelCache = result
			d.channelCacheTime = time.Now()
			d.channelCacheGeneration = generation
		}
		d.mu.Unlock()

		if current {
			return result, nil
		}
	}
	return nil, errChannelConfigChanged
}

// textChannels maps the discordgo channels to the provider's text-channel view.
func textChannels(channels []*discordgo.Channel) []Channel {
	var result []Channel
	for _, ch := range channels {
		if ch.Type == discordgo.ChannelTypeGuildText {
			result = append(result, Channel{ID: ch.ID, Name: ch.Name, Type: "text"})
		}
	}
	return result
}

// UpdateConfig updates the Discord provider configuration. Changing either
// credential invalidates the channel cache, whose entries belong to the old
// bot/guild and would otherwise be served for the new connection.
func (d *DiscordProvider) UpdateConfig(botToken, guildID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if botToken != d.botToken || guildID != d.guildID {
		// Bump the generation so any in-flight GetChannels for the old bot/guild
		// is recognised as stale, and drop the now-mismatched cache.
		d.configGeneration++
		d.channelCache = nil
		d.channelCacheTime = time.Time{}
	}
	d.botToken = botToken
	d.guildID = guildID
}

// ValidateConfig checks if the Discord configuration is valid by attempting a
// connection. It snapshots the credentials once under the lock, then runs the
// validation (which does network I/O) with no lock held, so a concurrent
// UpdateConfig can neither race the reads nor observe a torn token/guild pair.
func (d *DiscordProvider) ValidateConfig(ctx context.Context) error {
	d.mu.RLock()
	botToken := d.botToken
	guildID := d.guildID
	d.mu.RUnlock()

	if botToken == "" || guildID == "" {
		return fmt.Errorf("missing bot token or guild ID")
	}

	return d.validateConfig(ctx, botToken, guildID)
}
