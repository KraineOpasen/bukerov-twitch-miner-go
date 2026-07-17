package notifications

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// discordProvider is the narrow behaviour the Manager needs from the Discord
// gateway client. It is deliberately limited to the methods the Manager
// actually calls so tests can inject a network-free fake that counts
// connect/disconnect/update-config calls and reports connection state.
// Production always uses *DiscordProvider.
type discordProvider interface {
	Connect(ctx context.Context) error
	Disconnect() error
	UpdateConfig(botToken, guildID string)
	IsConnected() bool
	Send(ctx context.Context, notification Notification) error
	GetChannels(ctx context.Context, forceRefresh bool) ([]Channel, error)
}

// Manager handles notification dispatching across multiple providers.
type Manager struct {
	// discordConfig is stored BY VALUE, not as the caller's pointer: it is the
	// authoritative snapshot of the desired Discord connection settings. Keeping
	// a value copy makes UpdateDiscordConfig's change detection immune to a
	// caller mutating the original struct in place (pointer aliasing).
	discordConfig config.DiscordSettings
	notifConfig   *config.NotificationsSettings
	username      string
	discord       discordProvider
	// newDiscord builds a Discord provider. Production wires the real
	// *DiscordProvider constructor; tests inject a fake. It is the single place
	// Discord providers are created so both paths go through the same seam.
	newDiscord func(botToken, guildID string) discordProvider
	repo       *Repository
	streamers  []string

	// messageProviders are the configured push providers (Matrix, Pushover,
	// Gotify, webhook). batchers maps each provider name to the Batcher that
	// wraps its Send call.
	messageProviders []MessageProvider
	batchers         map[string]*Batcher

	pointsPreviousValues map[string]int

	// mu guards the Manager's own fields (config snapshot, provider reference,
	// batchers, ...) and is held only for short, non-blocking critical sections.
	mu sync.RWMutex

	// discordLifecycleMu serializes the Discord connection lifecycle (Start,
	// Stop, UpdateDiscordConfig). It is always acquired BEFORE mu, and every
	// network operation (Connect/Disconnect -> session.Open/Close) runs while
	// only this lock — never mu — is held, so notification paths that take mu
	// are never blocked on Discord network I/O.
	discordLifecycleMu sync.Mutex
}

// defaultDiscordFactory is the production Discord provider factory: it returns
// a real *DiscordProvider wired to the given credentials.
func defaultDiscordFactory(botToken, guildID string) discordProvider {
	return NewDiscordProvider(botToken, guildID)
}

// NewManager creates a new notification manager. discordCfg carries the Discord
// connection settings, notifCfg carries the provider-agnostic batching
// configuration, and username is used for the per-account environment-variable
// override of the push providers (empty for a single-account setup).
func NewManager(discordCfg *config.DiscordSettings, notifCfg *config.NotificationsSettings, db *database.DB, streamers []string, username string) (*Manager, error) {
	repo, err := NewRepository(db)
	if err != nil {
		return nil, fmt.Errorf("failed to create notification repository: %w", err)
	}

	// Copy the incoming Discord settings by value immediately; never retain the
	// caller's pointer as the authoritative config. nil is treated as the
	// zero value (Discord disabled) rather than panicking.
	var dc config.DiscordSettings
	if discordCfg != nil {
		dc = *discordCfg
	}

	m := &Manager{
		discordConfig:        dc,
		notifConfig:          notifCfg,
		username:             username,
		streamers:            streamers,
		newDiscord:           defaultDiscordFactory,
		repo:                 repo,
		pointsPreviousValues: make(map[string]int),
		batchers:             make(map[string]*Batcher),
	}

	if dc.Enabled {
		m.discord = m.newDiscord(dc.BotToken, dc.GuildID)
	}

	for _, p := range NewMessageProvidersFromEnv(username) {
		if !p.IsConfigured() {
			continue
		}
		provider := p
		m.messageProviders = append(m.messageProviders, provider)
		bc := NewBatchConfig(m.resolveBatchingSettings(provider.Name()))
		m.batchers[provider.Name()] = NewBatcher(provider.Name(), bc, provider.Send)
		slog.Info("Push notification provider configured",
			"provider", provider.Name(), "batching", bc.Enabled)
	}

	return m, nil
}

// resolveBatchingSettings returns the batching settings for a provider, applying
// the per-provider override when present and falling back to the global config
// (or built-in defaults when no notification config was supplied).
func (m *Manager) resolveBatchingSettings(providerName string) config.BatchingSettings {
	if m.notifConfig == nil {
		return config.DefaultBatchingSettings()
	}
	if override, ok := m.notifConfig.ProviderBatching[providerName]; ok {
		return override
	}
	return m.notifConfig.Batching
}

// Start initializes and connects all enabled providers.
func (m *Manager) Start(ctx context.Context) error {
	m.discordLifecycleMu.Lock()
	defer m.discordLifecycleMu.Unlock()

	m.mu.Lock()
	provider := m.discord
	enabled := m.discordConfig.Enabled
	m.mu.Unlock()

	// Connect with no Manager lock held so the network Open never blocks
	// notification paths.
	if provider != nil && enabled {
		if err := provider.Connect(ctx); err != nil {
			slog.Error("Failed to connect Discord provider", "error", err)
			return err
		}
	}

	// Start the per-provider batch flush loops. Each loop performs a final
	// flush when ctx is cancelled (graceful shutdown).
	m.mu.RLock()
	batchers := make([]*Batcher, 0, len(m.batchers))
	for _, b := range m.batchers {
		batchers = append(batchers, b)
	}
	m.mu.RUnlock()
	for _, b := range batchers {
		b.Start(ctx)
	}

	return nil
}

// Stop disconnects all providers, flushes any pending batches, and closes the
// repository.
func (m *Manager) Stop() {
	m.discordLifecycleMu.Lock()
	defer m.discordLifecycleMu.Unlock()

	m.mu.RLock()
	batchers := make([]*Batcher, 0, len(m.batchers))
	for _, b := range m.batchers {
		batchers = append(batchers, b)
	}
	provider := m.discord
	repo := m.repo
	m.mu.RUnlock()

	// Force-flush every pending batch before shutting down so no accumulated
	// events are lost.
	if len(batchers) > 0 {
		flushCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		for _, b := range batchers {
			b.Stop(flushCtx)
		}
		cancel()
	}

	// Disconnect with no Manager lock held.
	if provider != nil {
		if err := provider.Disconnect(); err != nil {
			slog.Error("Failed to disconnect Discord provider", "error", err)
		}
	}

	if repo != nil {
		_ = repo.Close()
	}
}

// dispatchPush forwards an event to every configured push provider through its
// batcher. Immediate events (per the batching config) are sent instantly;
// everything else is accumulated and flushed on the batch interval. Sending
// happens on background goroutines so callers are never blocked on network I/O.
func (m *Manager) dispatchPush(eventType NotificationType, group, line string) {
	m.mu.RLock()
	batchers := make([]*Batcher, 0, len(m.batchers))
	for _, b := range m.batchers {
		batchers = append(batchers, b)
	}
	m.mu.RUnlock()

	if len(batchers) == 0 {
		return
	}

	ev := BatchEvent{Type: eventType, Group: group, Line: line}
	for _, b := range batchers {
		batcher := b
		go func() {
			if err := batcher.Add(context.Background(), ev); err != nil {
				slog.Error("Failed to dispatch push notification",
					"provider", batcher.name, "type", eventType, "error", err)
			}
		}()
	}
}

// NotifyEvent submits a generic, provider-agnostic event to the push providers.
// It is the extension point for batchable events produced elsewhere in the
// codebase (e.g. drop claims or bet outcomes): callers pass an event type
// (which the batching config may mark as immediate), a grouping key (streamer
// or campaign), and a one-line human-readable summary.
func (m *Manager) NotifyEvent(eventType NotificationType, group, line string) {
	m.dispatchPush(eventType, group, line)
}

// ProviderTestResult reports the outcome of a test notification for a single
// provider.
type ProviderTestResult struct {
	Provider string `json:"provider"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// TestAllProviders sends a test notification to every enabled provider (Discord
// and all configured push providers), bypassing event filters and batching. It
// returns a per-provider result so callers can surface which providers
// succeeded and which failed.
func (m *Manager) TestAllProviders(ctx context.Context) []ProviderTestResult {
	m.mu.RLock()
	discord := m.discord
	providers := append([]MessageProvider(nil), m.messageProviders...)
	m.mu.RUnlock()

	const testTitle = "✅ Test notification"
	const testBody = "This is a test notification from Twitch Points Miner."

	var results []ProviderTestResult

	if discord != nil {
		res := ProviderTestResult{Provider: "discord", OK: true}
		cfg, err := m.repo.GetConfig()
		if err != nil {
			res.OK = false
			res.Error = "failed to load config: " + err.Error()
		} else {
			channelID := firstNonEmpty(cfg.SystemChannelID, cfg.OnlineChannelID,
				cfg.OfflineChannelID, cfg.MentionsChannelID, cfg.PointsChannelID)
			if channelID == "" {
				res.OK = false
				res.Error = "no Discord channel configured"
			} else if err := discord.Send(ctx, Notification{
				Type:      NotificationTypeConnectionRestored,
				Title:     testTitle,
				Message:   testBody,
				ChannelID: channelID,
				Color:     ColorConnectionRestored,
			}); err != nil {
				res.OK = false
				res.Error = err.Error()
			}
		}
		results = append(results, res)
	}

	for _, p := range providers {
		res := ProviderTestResult{Provider: p.Name(), OK: true}
		if err := p.Send(ctx, Message{
			Type:  NotificationTypeConnectionRestored,
			Title: testTitle,
			Body:  testBody,
		}); err != nil {
			res.OK = false
			res.Error = err.Error()
		}
		results = append(results, res)
	}

	return results
}

// HasAnyProvider reports whether at least one provider (Discord or a push
// provider) is available for delivering notifications.
func (m *Manager) HasAnyProvider() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.discord != nil || len(m.messageProviders) > 0
}

// firstNonEmpty returns the first non-empty string from the arguments.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
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

	// Push providers receive the event independently of the Discord channel
	// configuration (they route to their own preconfigured destinations).
	m.dispatchPush(NotificationTypeOnline, streamer,
		fmt.Sprintf("🟢 %s is now live! https://twitch.tv/%s", streamer, streamer))

	if !enabled || discord == nil || cfg.OnlineChannelID == "" {
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

	m.dispatchPush(NotificationTypeOffline, streamer,
		fmt.Sprintf("⚫ %s went offline.", streamer))

	if !enabled || discord == nil || cfg.OfflineChannelID == "" {
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

	cfg, err := m.repo.GetConfig()
	if err != nil {
		slog.Error("Failed to get notification config", "error", err)
		return
	}

	if !cfg.SystemEnabled {
		return
	}

	m.dispatchPush(NotificationTypeReauthRequired, "",
		fmt.Sprintf("🔒 Twitch reauthorization required. %s Restart the miner and log in again to resume harvesting.", detail))

	if !enabled || discord == nil || cfg.SystemChannelID == "" {
		slog.Debug("Reauth Discord notification skipped: system channel not configured")
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

	cfg, err := m.repo.GetConfig()
	if err != nil {
		slog.Error("Failed to get notification config", "error", err)
		return
	}

	if !cfg.SystemEnabled {
		return
	}

	m.dispatchPush(NotificationTypeConnectionLost, "",
		fmt.Sprintf("🔌 Connection lost - harvesting paused. %s", detail))

	if !enabled || discord == nil || cfg.SystemChannelID == "" {
		slog.Debug("Connection-lost Discord notification skipped: system channel not configured")
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

	cfg, err := m.repo.GetConfig()
	if err != nil {
		slog.Error("Failed to get notification config", "error", err)
		return
	}

	if !cfg.SystemEnabled {
		return
	}

	m.dispatchPush(NotificationTypeConnectionRestored, "",
		"✅ Connection restored. Twitch API and PubSub connectivity is back; harvesting has resumed.")

	if !enabled || discord == nil || cfg.SystemChannelID == "" {
		slog.Debug("Connection-restored Discord notification skipped: system channel not configured")
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

// NotifyHealthTransition sends an operator-facing alert when a health signal
// (currently the watch-transport canary) flips between healthy and failed. It
// reuses the system-notifications channel like the connection-health alerts,
// and is only ever called by the health center on an actual transition — never
// on repeated same-state results — so it does not spam.
func (m *Manager) NotifyHealthTransition(signal string, healthy bool, detail string) {
	m.mu.RLock()
	discord := m.discord
	enabled := m.discordConfig.Enabled
	m.mu.RUnlock()

	cfg, err := m.repo.GetConfig()
	if err != nil {
		slog.Error("Failed to get notification config", "error", err)
		return
	}

	if !cfg.SystemEnabled {
		return
	}

	label := healthSignalLabel(signal)
	var evType NotificationType
	var emoji, title, message string
	if healthy {
		evType = NotificationTypeHealthRecovered
		emoji, title = "✅", label+" recovered"
		message = detail
		if message == "" {
			message = label + " is confirmed working again."
		}
	} else {
		evType = NotificationTypeHealthDegraded
		emoji, title = "⚠️", label+" check failed"
		message = detail
		if message == "" {
			message = label + " is not being confirmed."
		}
	}

	m.dispatchPush(evType, "", fmt.Sprintf("%s %s. %s", emoji, title, message))

	if !enabled || discord == nil || cfg.SystemChannelID == "" {
		slog.Debug("Health-transition Discord notification skipped: system channel not configured")
		return
	}

	notification := Notification{
		Type:      evType,
		Title:     fmt.Sprintf("%s %s", emoji, title),
		Message:   message,
		ChannelID: cfg.SystemChannelID,
	}

	go func() {
		if err := discord.Send(context.Background(), notification); err != nil {
			slog.Error("Failed to send health-transition notification", "error", err)
		}
	}()
}

// healthSignalLabel maps a health signal name to a human label for alerts.
func healthSignalLabel(signal string) string {
	switch signal {
	case "watch_transport":
		return "Watch transport"
	case "gql_api":
		return "GQL API"
	case "pubsub":
		return "PubSub"
	case "oauth":
		return "OAuth"
	case "drops_inventory":
		return "Drops inventory sync"
	case "drops_progress":
		return "Drops progress"
	default:
		return signal
	}
}

// NotifyDropStalled is the drop-progress watchdog's critical alert: a
// specific drop's progress is confirmed stalled and the whole automatic
// recovery pipeline (forced syncs, session recreation, channel switch) is
// exhausted. Sent once per stall episode — the watchdog only calls this on
// the transition into the terminal STALLED state.
func (m *Manager) NotifyDropStalled(campaign, drop, channel, detail string) {
	title := fmt.Sprintf("🛑 Drop stalled: %q", drop)
	message := fmt.Sprintf("Progress of %q (%s) is not advancing and automatic recovery is exhausted.", drop, campaign)
	if channel != "" {
		message += fmt.Sprintf(" Last farmed on %s.", channel)
	}
	if detail != "" {
		message += " Last recovery step: " + detail
	}
	m.notifyDropTransition(NotificationTypeDropStalled, title, message)
}

// NotifyDropRecovered reports that a previously stall-notified drop is
// accruing minutes again. Only sent when a stalled notification went out for
// the same episode, so the pair never spams.
func (m *Manager) NotifyDropRecovered(campaign, drop, channel, detail string) {
	title := fmt.Sprintf("✅ Drop progressing again: %q", drop)
	message := fmt.Sprintf("Progress of %q (%s) resumed.", drop, campaign)
	if channel != "" {
		message += fmt.Sprintf(" Farming on %s.", channel)
	}
	if detail != "" {
		message += " " + detail
	}
	m.notifyDropTransition(NotificationTypeDropRecovered, title, message)
}

// notifyDropTransition shares the system-channel dispatch used by the other
// operator alerts (connection, health transitions): push providers plus the
// Discord system channel, gated on SystemEnabled.
func (m *Manager) notifyDropTransition(evType NotificationType, title, message string) {
	m.mu.RLock()
	discord := m.discord
	enabled := m.discordConfig.Enabled
	m.mu.RUnlock()

	cfg, err := m.repo.GetConfig()
	if err != nil {
		slog.Error("Failed to get notification config", "error", err)
		return
	}
	if !cfg.SystemEnabled {
		return
	}

	m.dispatchPush(evType, "", title+" — "+message)

	if !enabled || discord == nil || cfg.SystemChannelID == "" {
		slog.Debug("Drop-transition Discord notification skipped: system channel not configured")
		return
	}

	notification := Notification{
		Type:      evType,
		Title:     title,
		Message:   message,
		ChannelID: cfg.SystemChannelID,
	}
	go func() {
		if err := discord.Send(context.Background(), notification); err != nil {
			slog.Error("Failed to send drop-transition notification", "error", err)
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

// NotifyUpdateFailed sends a notification that installing an available miner
// update failed (fail-closed checksum refusal, download error, swap failure).
// Same system-notifications channel and gating as NotifyUpdateAvailable.
func (m *Manager) NotifyUpdateFailed(current, latest, reason string) {
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
		slog.Debug("Update-failed notification skipped: system notifications not configured")
		return
	}

	notification := Notification{
		Type:      NotificationTypeUpdateFailed,
		Title:     "⚠️ Miner update failed",
		Message:   fmt.Sprintf("Updating **%s** → **%s** was refused/failed and the miner keeps running on the current version.\nReason: %s", current, latest, reason),
		ChannelID: cfg.SystemChannelID,
	}

	go func() {
		if err := discord.Send(context.Background(), notification); err != nil {
			slog.Error("Failed to send update-failed notification", "error", err)
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

// UpdateDiscordConfig reconciles the desired Discord connection settings with
// the running provider. It is idempotent: applying the same settings while the
// provider is already connected is a true no-op (no disconnect, no reconnect,
// no log), which is what stops every unrelated settings save from tearing the
// Discord gateway session down and back up.
//
// The connection-relevant fields are Enabled, BotToken and GuildID; channel
// selection lives elsewhere and never requires reconnecting the session.
// Change detection compares the incoming settings by VALUE against the stored
// value snapshot, so a caller mutating its original struct in place cannot fool
// the comparison into a false no-op. Whether a reconnect is actually needed is
// decided by the provider's real lifecycle state (IsConnected), not by mere
// provider existence — so a disconnected or failed provider recovers instead of
// being wrongly skipped.
func (m *Manager) UpdateDiscordConfig(cfg *config.DiscordSettings) error {
	// Serialize the whole reconcile against Start/Stop/other updates, but do NOT
	// hold m.mu across the network operations below.
	m.discordLifecycleMu.Lock()
	defer m.discordLifecycleMu.Unlock()

	// Short critical section: snapshot the stored config and current provider.
	m.mu.Lock()
	old := m.discordConfig
	provider := m.discord
	m.mu.Unlock()

	// Local immutable copy of the desired config; a nil pointer is treated as
	// "Discord disabled". The caller's pointer is never retained.
	var next config.DiscordSettings
	if cfg != nil {
		next = *cfg
	}

	// --- Disabled ---
	if !next.Enabled {
		if provider == nil {
			// CASE A: already disabled, no provider -> commit config, no-op.
			m.setDiscordConfig(next)
			return nil
		}
		// CASE B: tear the provider down outside m.mu. On failure, leave the
		// state untouched (provider retained, config uncommitted) so the next
		// identical disable retries instead of falsely succeeding.
		if err := provider.Disconnect(); err != nil {
			slog.Error("Failed to disconnect Discord provider", "error", err)
			return err
		}
		m.mu.Lock()
		m.discordConfig = next
		if m.discord == provider {
			m.discord = nil
		}
		m.mu.Unlock()
		slog.Info("Discord notifications disabled")
		return nil
	}

	// --- Enabled --- connectionChanged is true when any field that requires
	// re-establishing the gateway session differs (including disabled->enabled).
	connectionChanged := old.Enabled != next.Enabled ||
		old.BotToken != next.BotToken ||
		old.GuildID != next.GuildID

	switch {
	case provider == nil:
		// CASE C / F: create the provider and publish it + the desired config
		// BEFORE connecting, so a failed Connect stays retryable.
		provider = m.newDiscord(next.BotToken, next.GuildID)
		m.mu.Lock()
		m.discord = provider
		m.discordConfig = next
		m.mu.Unlock()
		if err := provider.Connect(context.Background()); err != nil {
			slog.Error("Failed to connect Discord provider", "error", err)
			return err
		}
		if !old.Enabled {
			slog.Info("Discord notifications enabled")
		} else {
			slog.Debug("Discord provider reconnected after being disconnected")
		}
		return nil

	case connectionChanged:
		// CASE G / H: a real credential change. Disconnect the old session
		// outside m.mu FIRST; on failure do nothing else (retryable) — no
		// UpdateConfig, no Connect, no config commit, no success log.
		if err := provider.Disconnect(); err != nil {
			slog.Error("Failed to disconnect Discord provider", "error", err)
			return err
		}
		provider.UpdateConfig(next.BotToken, next.GuildID)
		m.setDiscordConfig(next)
		if err := provider.Connect(context.Background()); err != nil {
			slog.Error("Failed to connect Discord provider", "error", err)
			return err
		}
		slog.Info("Discord configuration updated and reconnected")
		return nil

	default:
		// Unchanged connection config. Decide by real lifecycle state, not by
		// mere provider existence.
		if provider.IsConnected() {
			// CASE D: true no-op (stored config already equals next).
			return nil
		}
		// CASE E: the session is down (never connected, dropped, or a prior
		// Connect failed) -> reconnect, no teardown, retryable on failure.
		m.setDiscordConfig(next)
		if err := provider.Connect(context.Background()); err != nil {
			slog.Error("Failed to connect Discord provider", "error", err)
			return err
		}
		slog.Debug("Discord provider reconnected after being disconnected")
		return nil
	}
}

// setDiscordConfig stores the desired Discord config under a short lock.
func (m *Manager) setDiscordConfig(cfg config.DiscordSettings) {
	m.mu.Lock()
	m.discordConfig = cfg
	m.mu.Unlock()
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
