package notifications

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// Manager handles notification dispatching across multiple providers.
type Manager struct {
	discordConfig *config.DiscordSettings
	discord       *DiscordProvider
	repo          *Repository
	streamers     []string

	pointsPreviousValues map[string]int
	mu                   sync.RWMutex
}

// NewManager creates a new notification manager.
func NewManager(discordCfg *config.DiscordSettings, db *database.DB, streamers []string) (*Manager, error) {
	repo, err := NewRepository(db)
	if err != nil {
		return nil, fmt.Errorf("failed to create notification repository: %w", err)
	}

	m := &Manager{
		discordConfig:        discordCfg,
		streamers:            streamers,
		repo:                 repo,
		pointsPreviousValues: make(map[string]int),
	}

	if discordCfg.Enabled {
		m.discord = NewDiscordProvider(discordCfg.BotToken, discordCfg.GuildID)
	}

	return m, nil
}

// Start initializes and connects all enabled providers.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.discord != nil && m.discordConfig.Enabled {
		if err := m.discord.Connect(ctx); err != nil {
			slog.Error("Failed to connect Discord provider", "error", err)
			return err
		}
	}

	return nil
}

// Stop disconnects all providers and closes the repository.
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.discord != nil {
		if err := m.discord.Disconnect(); err != nil {
			slog.Error("Failed to disconnect Discord provider", "error", err)
		}
	}

	if m.repo != nil {
		_ = m.repo.Close()
	}
}

// IsEnabled returns true if Discord notifications are enabled.
func (m *Manager) IsEnabled() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.discordConfig.Enabled
}

// IsConfigValid returns true and empty string if config is valid,
// otherwise returns false and an error message.
func (m *Manager) IsConfigValid() (bool, string) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.discordConfig.Enabled {
		return true, ""
	}

	if m.discordConfig.BotToken == "" {
		return false, "Discord bot token is not configured"
	}

	if m.discordConfig.GuildID == "" {
		return false, "Discord guild (server) ID is not configured"
	}

	return true, ""
}

// GetConfig returns the notification configuration from the database.
func (m *Manager) GetConfig() (*NotificationConfig, error) {
	return m.repo.GetConfig()
}

// SaveConfig saves the notification configuration to the database.
func (m *Manager) SaveConfig(cfg *NotificationConfig) error {
	return m.repo.SaveConfig(cfg)
}

// GetPointRules returns all point notification rules.
func (m *Manager) GetPointRules() ([]PointRule, error) {
	return m.repo.GetPointRules()
}

// AddPointRule adds a new point notification rule.
func (m *Manager) AddPointRule(rule *PointRule) error {
	return m.repo.AddPointRule(rule)
}

// UpdatePointRule updates an existing point rule.
func (m *Manager) UpdatePointRule(rule *PointRule) error {
	return m.repo.UpdatePointRule(rule)
}

// DeletePointRule removes a point notification rule.
func (m *Manager) DeletePointRule(id int64) error {
	return m.repo.DeletePointRule(id)
}

// NotifyMention sends a mention notification.
func (m *Manager) NotifyMention(streamer, fromUser, message string) {
	m.mu.RLock()
	discord := m.discord
	enabled := m.discordConfig.Enabled
	m.mu.RUnlock()

	if !enabled || discord == nil {
		return
	}

	cfg, err := m.repo.GetConfig()
	if err != nil {
		slog.Error("Failed to get notification config", "error", err)
		return
	}

	if !cfg.MentionsEnabled {
		return
	}

	if !cfg.MentionsAllChats {
		found := false
		for _, s := range cfg.MentionsStreamers {
			if s == streamer {
				found = true
				break
			}
		}
		if !found {
			return
		}
	}

	if cfg.MentionsChannelID == "" {
		slog.Debug("Mention notification skipped: no channel configured")
		return
	}

	notification := Notification{
		Type:      NotificationTypeMention,
		Title:     fmt.Sprintf("💬 Mentioned in %s's chat", streamer),
		Message:   fmt.Sprintf("**%s** mentioned you:\n> %s", fromUser, message),
		Streamer:  streamer,
		ChannelID: cfg.MentionsChannelID,
	}

	go func() {
		if err := discord.Send(context.Background(), notification); err != nil {
			slog.Error("Failed to send mention notification", "error", err)
		}
	}()
}

// NotifyPointsReached checks and sends point threshold notifications.
func (m *Manager) NotifyPointsReached(streamer string, points int) {
	m.mu.Lock()
	prevPoints := m.pointsPreviousValues[streamer]
	m.pointsPreviousValues[streamer] = points
	discord := m.discord
	enabled := m.discordConfig.Enabled
	m.mu.Unlock()

	if !enabled || discord == nil {
		return
	}

	if err := m.repo.ResetPointRuleIfBelow(streamer, points); err != nil {
		slog.Error("Failed to reset point rules", "error", err)
	}

	rules, err := m.repo.GetPointRules()
	if err != nil {
		slog.Error("Failed to get point rules", "error", err)
		return
	}

	cfg, err := m.repo.GetConfig()
	if err != nil {
		slog.Error("Failed to get notification config", "error", err)
		return
	}

	if cfg.PointsChannelID == "" {
		return
	}

	for _, rule := range rules {
		if rule.Streamer != streamer {
			continue
		}

		if rule.Triggered {
			continue
		}

		if prevPoints < rule.Threshold && points >= rule.Threshold {
			notification := Notification{
				Type:      NotificationTypePointsReached,
				Title:     fmt.Sprintf("🎯 Point Goal Reached: %s", streamer),
				Message:   fmt.Sprintf("You've reached **%d** points in **%s**'s channel!\nCurrent: **%d** points", rule.Threshold, streamer, points),
				Streamer:  streamer,
				ChannelID: cfg.PointsChannelID,
			}

			go func(n Notification, ruleID int64, deleteOnTrigger bool) {
				if err := discord.Send(context.Background(), n); err != nil {
					slog.Error("Failed to send points notification", "error", err)
					return
				}

				if deleteOnTrigger {
					if err := m.repo.DeletePointRule(ruleID); err != nil {
						slog.Error("Failed to delete point rule", "error", err)
					}
				} else {
					if err := m.repo.MarkPointRuleTriggered(ruleID, true); err != nil {
						slog.Error("Failed to mark point rule triggered", "error", err)
					}
				}
			}(notification, rule.ID, rule.DeleteOnTrigger)
		}
	}
}

// NotifyOnline sends a streamer online notification.
func (m *Manager) NotifyOnline(streamer string) {
	m.mu.RLock()
	discord := m.discord
	enabled := m.discordConfig.Enabled
	m.mu.RUnlock()

	if !enabled || discord == nil {
		return
	}

	cfg, err := m.repo.GetConfig()
	if err != nil {
		slog.Error("Failed to get notification config", "error", err)
		return
	}

	if !cfg.OnlineEnabled {
		return
	}

	if !cfg.OnlineAllStreamers {
		found := false
		for _, s := range cfg.OnlineStreamers {
			if s == streamer {
				found = true
				break
			}
		}
		if !found {
			return
		}
	}

	if cfg.OnlineChannelID == "" {
		slog.Debug("Online notification skipped: no channel configured")
		return
	}

	notification := Notification{
		Type:      NotificationTypeOnline,
		Title:     fmt.Sprintf("🟢 %s is now live!", streamer),
		Message:   fmt.Sprintf("**%s** just went live on Twitch!\n\nhttps://twitch.tv/%s", streamer, streamer),
		Streamer:  streamer,
		ChannelID: cfg.OnlineChannelID,
	}

	go func() {
		if err := discord.Send(context.Background(), notification); err != nil {
			slog.Error("Failed to send online notification", "error", err)
		}
	}()
}

// NotifyOffline sends a streamer offline notification.
func (m *Manager) NotifyOffline(streamer string) {
	m.mu.RLock()
	discord := m.discord
	enabled := m.discordConfig.Enabled
	m.mu.RUnlock()

	if !enabled || discord == nil {
		return
	}

	cfg, err := m.repo.GetConfig()
	if err != nil {
		slog.Error("Failed to get notification config", "error", err)
		return
	}

	if !cfg.OfflineEnabled {
		return
	}

	if !cfg.OfflineAllStreamers {
		found := false
		for _, s := range cfg.OfflineStreamers {
			if s == streamer {
				found = true
				break
			}
		}
		if !found {
			return
		}
	}

	if cfg.OfflineChannelID == "" {
		slog.Debug("Offline notification skipped: no channel configured")
		return
	}

	notification := Notification{
		Type:      NotificationTypeOffline,
		Title:     fmt.Sprintf("⚫ %s went offline", streamer),
		Message:   fmt.Sprintf("**%s** has ended their stream.", streamer),
		Streamer:  streamer,
		ChannelID: cfg.OfflineChannelID,
	}

	go func() {
		if err := discord.Send(context.Background(), notification); err != nil {
			slog.Error("Failed to send offline notification", "error", err)
		}
	}()
}

// NotifyReauthRequired sends a notification that Twitch authorization has
// expired or been revoked and the miner needs to be logged in again.
func (m *Manager) NotifyReauthRequired(detail string) {
	m.mu.RLock()
	discord := m.discord
	enabled := m.discordConfig.Enabled
	m.mu.RUnlock()

	if !enabled || discord == nil {
		return
	}

	cfg, err := m.repo.GetConfig()
	if err != nil {
		slog.Error("Failed to get notification config", "error", err)
		return
	}

	if !cfg.SystemEnabled || cfg.SystemChannelID == "" {
		slog.Debug("Reauth notification skipped: system notifications not configured")
		return
	}

	notification := Notification{
		Type:      NotificationTypeReauthRequired,
		Title:     "🔒 Twitch reauthorization required",
		Message:   fmt.Sprintf("Twitch rejected the miner's login token. %s\nRestart the miner and log in again to resume harvesting.", detail),
		ChannelID: cfg.SystemChannelID,
	}

	go func() {
		if err := discord.Send(context.Background(), notification); err != nil {
			slog.Error("Failed to send reauth-required notification", "error", err)
		}
	}()
}

// NotifyConnectionLost sends a notification that the miner has lost contact
// with Twitch (API and/or PubSub) for longer than the configured threshold.
func (m *Manager) NotifyConnectionLost(detail string) {
	m.mu.RLock()
	discord := m.discord
	enabled := m.discordConfig.Enabled
	m.mu.RUnlock()

	if !enabled || discord == nil {
		return
	}

	cfg, err := m.repo.GetConfig()
	if err != nil {
		slog.Error("Failed to get notification config", "error", err)
		return
	}

	if !cfg.SystemEnabled || cfg.SystemChannelID == "" {
		slog.Debug("Connection-lost notification skipped: system notifications not configured")
		return
	}

	notification := Notification{
		Type:      NotificationTypeConnectionLost,
		Title:     "🔌 Connection lost - harvesting paused",
		Message:   detail,
		ChannelID: cfg.SystemChannelID,
	}

	go func() {
		if err := discord.Send(context.Background(), notification); err != nil {
			slog.Error("Failed to send connection-lost notification", "error", err)
		}
	}()
}

// NotifyConnectionRestored sends a notification that connectivity to Twitch
// has resumed after a NotifyConnectionLost event.
func (m *Manager) NotifyConnectionRestored() {
	m.mu.RLock()
	discord := m.discord
	enabled := m.discordConfig.Enabled
	m.mu.RUnlock()

	if !enabled || discord == nil {
		return
	}

	cfg, err := m.repo.GetConfig()
	if err != nil {
		slog.Error("Failed to get notification config", "error", err)
		return
	}

	if !cfg.SystemEnabled || cfg.SystemChannelID == "" {
		slog.Debug("Connection-restored notification skipped: system notifications not configured")
		return
	}

	notification := Notification{
		Type:      NotificationTypeConnectionRestored,
		Title:     "✅ Connection restored",
		Message:   "Twitch API and PubSub connectivity is back. Harvesting has resumed.",
		ChannelID: cfg.SystemChannelID,
	}

	go func() {
		if err := discord.Send(context.Background(), notification); err != nil {
			slog.Error("Failed to send connection-restored notification", "error", err)
		}
	}()
}

// NotifyUpdateAvailable sends a notification that a newer miner release is
// available. It reuses the system-notifications channel (like reauth and
// connection-health alerts) since it is an operator-facing maintenance event.
func (m *Manager) NotifyUpdateAvailable(current, latest, releaseURL string) {
	m.mu.RLock()
	discord := m.discord
	enabled := m.discordConfig.Enabled
	m.mu.RUnlock()

	if !enabled || discord == nil {
		return
	}

	cfg, err := m.repo.GetConfig()
	if err != nil {
		slog.Error("Failed to get notification config", "error", err)
		return
	}

	if !cfg.SystemEnabled || cfg.SystemChannelID == "" {
		slog.Debug("Update-available notification skipped: system notifications not configured")
		return
	}

	notification := Notification{
		Type:      NotificationTypeUpdateAvailable,
		Title:     "⬆️ Miner update available",
		Message:   fmt.Sprintf("A new version is available: **%s** → **%s**.\n%s", current, latest, releaseURL),
		ChannelID: cfg.SystemChannelID,
	}

	go func() {
		if err := discord.Send(context.Background(), notification); err != nil {
			slog.Error("Failed to send update-available notification", "error", err)
		}
	}()
}

// GetDiscordChannels returns available Discord channels.
func (m *Manager) GetDiscordChannels(ctx context.Context, forceRefresh bool) ([]Channel, error) {
	m.mu.RLock()
	discord := m.discord
	m.mu.RUnlock()

	if discord == nil {
		return nil, fmt.Errorf("discord provider not initialized")
	}

	return discord.GetChannels(ctx, forceRefresh)
}

// UpdateDiscordConfig updates the Discord configuration and reconnects if needed.
func (m *Manager) UpdateDiscordConfig(cfg *config.DiscordSettings) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	oldEnabled := m.discordConfig.Enabled
	m.discordConfig = cfg

	if !cfg.Enabled {
		if m.discord != nil {
			_ = m.discord.Disconnect()
			m.discord = nil
			slog.Info("Discord notifications disabled")
		}
		return nil
	}

	if m.discord == nil {
		m.discord = NewDiscordProvider(cfg.BotToken, cfg.GuildID)
	} else {
		_ = m.discord.Disconnect()
		m.discord.UpdateConfig(cfg.BotToken, cfg.GuildID)
	}

	if err := m.discord.Connect(context.Background()); err != nil {
		slog.Error("Failed to connect Discord provider", "error", err)
		return err
	}

	if !oldEnabled {
		slog.Info("Discord notifications enabled")
	} else {
		slog.Info("Discord configuration updated and reconnected")
	}

	return nil
}

// InitializePointsTracking sets the initial points values for all streamers.
func (m *Manager) InitializePointsTracking(streamerPoints map[string]int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for streamer, points := range streamerPoints {
		m.pointsPreviousValues[streamer] = points
	}
}

// GetStreamers returns the list of tracked streamers.
func (m *Manager) GetStreamers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.streamers
}

// SendTestNotifications sends a test notification for each notification type.
func (m *Manager) SendTestNotifications() (int, error) {
	m.mu.RLock()
	discord := m.discord
	m.mu.RUnlock()

	if discord == nil {
		return 0, fmt.Errorf("discord not connected")
	}

	cfg, err := m.GetConfig()
	if err != nil {
		return 0, fmt.Errorf("failed to get config: %w", err)
	}

	sent := 0
	ctx := context.Background()

	// Test mention notification
	if cfg.MentionsChannelID != "" {
		err := discord.Send(ctx, Notification{
			Type:      NotificationTypeMention,
			Title:     "Test Mention",
			Message:   "TestUser mentioned you in TestStreamer's chat:\n> Hey @you, this is a test mention notification!",
			Streamer:  "TestStreamer",
			ChannelID: cfg.MentionsChannelID,
			Color:     ColorMention,
		})
		if err != nil {
			slog.Error("Test mention notification failed", "error", err)
		} else {
			sent++
		}
	}

	// Test points notification
	if cfg.PointsChannelID != "" {
		err := discord.Send(ctx, Notification{
			Type:      NotificationTypePointsReached,
			Title:     "Test Points Goal",
			Message:   "You reached 100,000 points in TestStreamer's channel!",
			Streamer:  "TestStreamer",
			ChannelID: cfg.PointsChannelID,
			Color:     ColorPoints,
		})
		if err != nil {
			slog.Error("Test points notification failed", "error", err)
		} else {
			sent++
		}
	}

	// Test online notification
	if cfg.OnlineChannelID != "" {
		err := discord.Send(ctx, Notification{
			Type:      NotificationTypeOnline,
			Title:     "Test Online",
			Message:   "TestStreamer is now live!",
			Streamer:  "TestStreamer",
			ChannelID: cfg.OnlineChannelID,
			Color:     ColorOnline,
		})
		if err != nil {
			slog.Error("Test online notification failed", "error", err)
		} else {
			sent++
		}
	}

	// Test offline notification
	if cfg.OfflineChannelID != "" {
		err := discord.Send(ctx, Notification{
			Type:      NotificationTypeOffline,
			Title:     "Test Offline",
			Message:   "TestStreamer has gone offline.",
			Streamer:  "TestStreamer",
			ChannelID: cfg.OfflineChannelID,
			Color:     ColorOffline,
		})
		if err != nil {
			slog.Error("Test offline notification failed", "error", err)
		} else {
			sent++
		}
	}

	// Test system (reauth/connection-health) notification
	if cfg.SystemChannelID != "" {
		err := discord.Send(ctx, Notification{
			Type:      NotificationTypeConnectionRestored,
			Title:     "Test System Notification",
			Message:   "This channel will receive reauthorization and connection-health alerts.",
			ChannelID: cfg.SystemChannelID,
			Color:     ColorConnectionRestored,
		})
		if err != nil {
			slog.Error("Test system notification failed", "error", err)
		} else {
			sent++
		}
	}

	if sent == 0 {
		return 0, fmt.Errorf("no channels configured")
	}

	return sent, nil
}
