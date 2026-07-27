package notifications

import (
	"context"
	"database/sql"
	"errors"
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
	// notifConfig is stored BY VALUE, with its ProviderBatching map (and every
	// entry's ImmediateEvents slice) deep-copied at construction — never the
	// caller's pointer. resolveBatchingSettings reads it only during
	// construction, but keeping the copy immune to the caller's config.Config
	// mutating in place (settings-apply reassigns config.Config wholesale)
	// means this Manager's resolved batching settings can never be
	// retroactively changed by an edit that has nothing to do with it.
	notifConfig config.NotificationsSettings
	username    string
	discord     discordProvider
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

	// upcomingMu serializes the read-decide-send-record cycle of the
	// upcoming-campaign alert so a campaign is delivered at most once even if
	// two full-sync results are published concurrently. It is a notifications-
	// internal lock (never the drops mutex) and is held across the bounded send,
	// which is safe because the send has a hard context timeout.
	upcomingMu sync.Mutex

	// displayLoc is the time zone operator-facing alerts render absolute times
	// in (set from config LoggerSettings.TimeZone via SetDisplayLocation, so the
	// Discord message matches the dashboard). nil falls back to UTC.
	displayLoc *time.Location

	// stopOnce makes Stop idempotent (M4, I5): the miner's stop() and a
	// caller-driven test can both end up calling Stop on the same Manager
	// (e.g. a shutdown racing an already-stopped path), and without this
	// guard a second call would re-run Disconnect/Flush/repo.Close, which is
	// harmless today only by accident (Repository.Close is a deliberate
	// no-op) — the guard makes "call it more than once" a defined no-op
	// rather than an unreviewed coincidence.
	stopOnce sync.Once
	// stopErr is the first Stop's dispatch-drain outcome, written once inside
	// stopOnce and returned to every caller so a repeated Stop observes the
	// same explicit shutdown error.
	stopErr error

	// dispatchMu guards dispatchDraining and serializes every dispatchWG.Add
	// against drainDispatch's Wait — the same shutdown/admission interlock as
	// the miner's beginApply/applyWG (S1): goDispatch refuses a NEW dispatch
	// goroutine once dispatchDraining is set, and registers an accepted one in
	// dispatchWG on the CALLER's goroutine before the spawn, so an Add can
	// never race a Wait that already observed zero.
	//
	// LOCK ORDER: dispatchMu is a leaf. It is never held while acquiring mu or
	// discordLifecycleMu, and drainDispatch must never Wait while either of
	// those is held — a dispatch goroutine is free to take mu, so waiting for
	// it under mu would deadlock the shutdown.
	dispatchMu       sync.Mutex
	dispatchDraining bool
	dispatchWG       sync.WaitGroup

	// dispatchCtx bounds the network half of every dispatched goroutine and is
	// cancelled at the start of the drain, so a wedged provider send aborts
	// instead of holding shutdown open for the full drain timeout. Derived
	// from context.Background(), not from the run context: at drain time the
	// run context is already cancelled, and an admitted dispatch's DB-writing
	// tail must still be allowed to finish. Created in NewManager (Notify* is
	// legal without Start); cancelled exactly once, by drainDispatch.
	dispatchCtx    context.Context
	dispatchCancel context.CancelFunc
}

// dispatchDrainTimeout bounds how long Stop waits for in-flight notification
// dispatch goroutines (which may be persisting point-rule state after their
// network send) to finish. The expected real drain is one SQLite write:
// dispatchCtx is cancelled before the wait begins, so a send in flight
// returns early instead of consuming the budget. Package variable so tests
// can shrink it (the watcher/drops stopJoinTimeout precedent). 5s keeps the
// aggregate shutdown drain budget documented in miner.stop() under the
// process-level shutdown deadline.
var dispatchDrainTimeout = 5 * time.Second

// errDispatchDrainTimeout is the explicit shutdown error Stop returns when
// admitted dispatch goroutines did not drain inside dispatchDrainTimeout.
var errDispatchDrainTimeout = errors.New("notifications: dispatch drain timed out")

// goDispatch runs fn on a background goroutine that Stop drains, handing it
// the Manager's dispatch context for any network I/O. It returns false —
// running nothing — once Stop has begun draining, so no new dispatch that
// could reach the database can start after the drain point. Admission
// (dispatchWG.Add under dispatchMu, on the caller's goroutine) is what makes
// the drain account for every accepted dispatch exactly once. This is the
// only sanctioned way for the Manager to spawn a notification goroutine.
func (m *Manager) goDispatch(fn func(ctx context.Context)) bool {
	m.dispatchMu.Lock()
	if m.dispatchDraining {
		m.dispatchMu.Unlock()
		return false
	}
	m.dispatchWG.Add(1)
	ctx := m.dispatchCtx
	m.dispatchMu.Unlock()

	go func() {
		defer m.dispatchWG.Done()
		fn(ctx)
	}()
	return true
}

// drainDispatch closes admission for new dispatch goroutines, cancels the
// network half of the ones in flight, and waits — bounded by
// dispatchDrainTimeout — for every admitted dispatch to return, so an
// in-flight point-rule persistence lands before the caller proceeds toward
// closing the shared database handle. On timeout it returns the explicit
// errDispatchDrainTimeout and proceeds — it never hangs. Idempotent. MUST be
// called with neither mu nor discordLifecycleMu held (see dispatchMu's
// lock-order note).
func (m *Manager) drainDispatch() error {
	m.dispatchMu.Lock()
	already := m.dispatchDraining
	m.dispatchDraining = true
	cancel := m.dispatchCancel
	m.dispatchMu.Unlock()
	if already {
		return nil
	}
	if cancel != nil {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		m.dispatchWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(dispatchDrainTimeout):
		slog.Warn("Notification dispatch did not drain within the stop timeout; proceeding with shutdown",
			"timeout", dispatchDrainTimeout)
		return fmt.Errorf("%w after %s", errDispatchDrainTimeout, dispatchDrainTimeout)
	}
}

// SetDisplayLocation sets the time zone operator-facing notifications render
// absolute times in (the same location the dashboard uses). Safe to call at
// wiring time before Start.
func (m *Manager) SetDisplayLocation(loc *time.Location) {
	m.mu.Lock()
	m.displayLoc = loc
	m.mu.Unlock()
}

func (m *Manager) displayLocation() *time.Location {
	m.mu.RLock()
	loc := m.displayLoc
	m.mu.RUnlock()
	if loc == nil {
		return time.UTC
	}
	return loc
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

	// Deep-copy the batching config the same way: a shallow `*notifCfg` copy
	// would still alias the caller's ProviderBatching map and each entry's
	// ImmediateEvents slice, so a later in-place edit of the caller's
	// config.Config (settings-apply reassigns it wholesale, but a caller
	// could still hold and mutate the old value) could otherwise change what
	// this Manager already resolved at construction. A nil notifCfg (no
	// batching config supplied — some tests) falls back to the same
	// built-in defaults resolveBatchingSettings used to return for a nil
	// pointer.
	nc := config.DefaultNotificationsSettings()
	if notifCfg != nil {
		nc = *notifCfg
		nc.Batching = cloneBatchingSettings(nc.Batching)
		if notifCfg.ProviderBatching != nil {
			nc.ProviderBatching = make(map[string]config.BatchingSettings, len(notifCfg.ProviderBatching))
			for name, bs := range notifCfg.ProviderBatching {
				nc.ProviderBatching[name] = cloneBatchingSettings(bs)
			}
		}
	}

	dispatchCtx, dispatchCancel := context.WithCancel(context.Background())
	m := &Manager{
		discordConfig:        dc,
		notifConfig:          nc,
		username:             username,
		streamers:            streamers,
		newDiscord:           defaultDiscordFactory,
		repo:                 repo,
		pointsPreviousValues: make(map[string]int),
		batchers:             make(map[string]*Batcher),
		dispatchCtx:          dispatchCtx,
		dispatchCancel:       dispatchCancel,
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

// cloneBatchingSettings deep-copies a BatchingSettings value, including its
// ImmediateEvents slice, so storing it inside the Manager's notifConfig
// snapshot can never alias a slice the caller's config later mutates in
// place.
func cloneBatchingSettings(s config.BatchingSettings) config.BatchingSettings {
	if s.ImmediateEvents != nil {
		cp := make([]string, len(s.ImmediateEvents))
		copy(cp, s.ImmediateEvents)
		s.ImmediateEvents = cp
	}
	return s
}

// resolveBatchingSettings returns the batching settings for a provider, applying
// the per-provider override when present and falling back to the global config.
func (m *Manager) resolveBatchingSettings(providerName string) config.BatchingSettings {
	if override, ok := m.notifConfig.ProviderBatching[providerName]; ok {
		return override
	}
	return m.notifConfig.Batching
}

// Start initializes and connects all enabled providers. The per-provider
// batch flush loops are started UNCONDITIONALLY, independent of whether the
// Discord connect below succeeds (M4, A3/6.5): a prior version returned
// early on a Discord Connect failure, before ever reaching the batcher-start
// loop, which left every push provider's batcher permanently un-started —
// any batched (non-immediate) event handed to Add would then buffer forever
// with no loop ever flushing it, until the next full Start (i.e. process
// restart). Discord being unreachable at startup no longer holds the push
// providers hostage; the Discord connect error is still returned so the
// caller can log it.
func (m *Manager) Start(ctx context.Context) error {
	m.discordLifecycleMu.Lock()
	defer m.discordLifecycleMu.Unlock()

	m.mu.Lock()
	provider := m.discord
	enabled := m.discordConfig.Enabled
	m.mu.Unlock()

	// Connect with no Manager lock held so the network Open never blocks
	// notification paths.
	var connectErr error
	if provider != nil && enabled {
		if err := provider.Connect(ctx); err != nil {
			slog.Error("Failed to connect Discord provider", "error", err)
			connectErr = err
		}
	}

	// Start the per-provider batch flush loops regardless of the Discord
	// outcome above. Each loop performs a final flush when ctx is cancelled
	// (graceful shutdown).
	m.mu.RLock()
	batchers := make([]*Batcher, 0, len(m.batchers))
	for _, b := range m.batchers {
		batchers = append(batchers, b)
	}
	m.mu.RUnlock()
	for _, b := range batchers {
		b.Start(ctx)
	}

	return connectErr
}

// Stop drains the in-flight dispatch goroutines, disconnects all providers,
// flushes any pending batches, and closes the repository. The dispatch drain
// runs FIRST (S1): a point-goal dispatch persists point_rule.triggered after
// its network send, and Stop returning is what lets Miner.Run return and the
// composition root close the shared SQLite handle — so Stop must not return
// while such a write can still be in flight. On a drain timeout Stop returns
// the explicit drain error (and still performs the remaining teardown).
// Idempotent (M4, I5): guarded by stopOnce, so a second call (the miner's
// stop() and, separately, a test or a second shutdown path both reaching
// Stop) is a safe no-op that returns the first call's error rather than a
// redundant Disconnect/Flush/repo.Close.
func (m *Manager) Stop() error {
	m.stopOnce.Do(func() {
		// Drain with NO Manager lock held: a dispatch goroutine may take mu,
		// and discordLifecycleMu sits above mu in the documented order, so
		// waiting under either would invert the lock order. The drain first
		// CANCELS in-flight network sends (a cancelled send delivers nothing
		// and correctly persists nothing) and then waits for the admitted
		// goroutines' DB-writing tails. Draining before the batcher flush
		// lets a pending gated Batcher.Add land in the final flush; draining
		// before Disconnect bounds how long a still-live session is held.
		m.stopErr = m.drainDispatch()

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
		// events are lost. Each Batcher.Stop joins its own loop (if started)
		// before performing this flush, bounded by flushCtx, so a wedged batcher
		// loop cannot wedge the whole Manager shutdown.
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
	})
	return m.stopErr
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
		if !m.goDispatch(func(ctx context.Context) {
			if err := batcher.Add(ctx, ev); err != nil {
				// safeSendErrorAttr is the only way a send error may be logged
				// here — see the identical rationale at batch.go's Flush.
				slog.Error("Failed to dispatch push notification",
					"provider", batcher.name, "type", eventType, safeSendErrorAttr(err))
			}
		}) {
			slog.Debug("Push dispatch skipped: notification manager is shutting down",
				"provider", batcher.name, "type", eventType)
		}
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
// provider. Error/Stage/Class/Status are always a safe, already-classified
// summary — never a raw error string — because this DTO crosses the JSON API
// boundary in internal/web/handlers_notifications.go, an endpoint reachable
// without authentication whenever Basic Auth is unconfigured. See
// newProviderTestFailure, the only place a failed result is built.
type ProviderTestResult struct {
	Provider string `json:"provider"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Stage    string `json:"stage,omitempty"`
	Class    string `json:"class,omitempty"`
	Status   int    `json:"status,omitempty"`
}

// newProviderTestFailure builds a failed ProviderTestResult from a provider
// Send error WITHOUT ever calling err.Error() on anything but a *SendError:
// only a *SendError's already-classified, safe fields are copied out. A
// provider that returns something else (a regression — every push provider
// and the Discord path must fail closed here) is reduced to a fixed message;
// its raw text is never read, not even for an instant, since the DTO
// immediately crosses the JSON API boundary.
func newProviderTestFailure(name string, err error) ProviderTestResult {
	var se *SendError
	if errors.As(err, &se) {
		return ProviderTestResult{
			Provider: name,
			Error:    se.Error(),
			Stage:    string(se.Stage()),
			Class:    string(se.Class()),
			Status:   se.StatusCode(),
		}
	}
	return ProviderTestResult{Provider: name, Error: "delivery failed"}
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
			// A SQLite/filesystem error here is not M5/M6 material, but it is
			// still not for the network response: log the detail server-side
			// and return a static message.
			slog.Error("Failed to load notification config for provider test", "error", err)
			res.OK = false
			res.Error = "failed to load config"
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
				// discordgo errors may embed discord.com URLs; route through the
				// same fail-closed helper as the push providers rather than
				// trusting err.Error() (accepted diagnostic reduction).
				res = newProviderTestFailure("discord", err)
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
			res = newProviderTestFailure(p.Name(), err)
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

// DeleteStreamerTx scrubs one login's notification state (point rules + config
// login-lists) within the caller's transaction, so the streamer-deletion
// coordinator can purge every store atomically. Returns true when anything was
// removed.
func (m *Manager) DeleteStreamerTx(tx *sql.Tx, login string) (bool, error) {
	return m.repo.DeleteStreamerTx(tx, login)
}

// Tombstone / Reinstate arm and clear the notification resurrection fence for a
// login (see Repository), so a rule creation racing a deletion cannot recreate a
// record and a later re-add starts clean.
func (m *Manager) Tombstone(login string) { m.repo.Tombstone(login) }
func (m *Manager) Reinstate(login string) { m.repo.Reinstate(login) }

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

	m.goDispatch(func(ctx context.Context) {
		if err := discord.Send(ctx, notification); err != nil {
			slog.Error("Failed to send mention notification", "error", err)
		}
	})
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

			n, ruleID, deleteOnTrigger := notification, rule.ID, rule.DeleteOnTrigger
			started := m.goDispatch(func(ctx context.Context) {
				if err := discord.Send(ctx, n); err != nil {
					// Includes a send cancelled by shutdown: nothing was
					// delivered, so the rule correctly stays untriggered and
					// re-fires after the next start (at-least-once, matching
					// the pre-existing crash semantics).
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
			})
			if !started {
				slog.Debug("Point-goal notification skipped: notification manager is shutting down",
					"streamer", streamer, "threshold", rule.Threshold)
			}
		}
	}
}

// nothingToNotify reports whether NO provider could possibly receive an
// event: no Discord provider, Discord disabled, AND no push providers
// configured. NotifyOnline/NotifyOffline use it to skip the repo.GetConfig
// read entirely for that hot path — every online/offline stream-status
// transition otherwise reaches here regardless of whether anything is
// actually configured, now that the Manager is constructed unconditionally
// whenever a database exists (M4, A8). Deliberately NOT used by
// NotifyPointsReached: its pointsPreviousValues bookkeeping (the read-then-
// store above cfg.PointsChannelID's check) must keep running even with
// everything off, so a LATER runtime Discord enable does not fire a
// spurious "goal reached" from stale/uninitialized tracking. Also
// deliberately NOT HasAnyProvider (wrong semantics: HasAnyProvider ignores
// discordConfig.Enabled, so a constructed-but-disabled Discord provider
// would wrongly count as "something to notify").
func (m *Manager) nothingToNotify() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.discord == nil && !m.discordConfig.Enabled && len(m.messageProviders) == 0
}

// NotifyOnline sends a streamer online notification.
func (m *Manager) NotifyOnline(streamer string) {
	if m.nothingToNotify() {
		return
	}

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

	m.goDispatch(func(ctx context.Context) {
		if err := discord.Send(ctx, notification); err != nil {
			slog.Error("Failed to send online notification", "error", err)
		}
	})
}

// NotifyOffline sends a streamer offline notification.
func (m *Manager) NotifyOffline(streamer string) {
	if m.nothingToNotify() {
		return
	}

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

	m.goDispatch(func(ctx context.Context) {
		if err := discord.Send(ctx, notification); err != nil {
			slog.Error("Failed to send offline notification", "error", err)
		}
	})
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

	m.goDispatch(func(ctx context.Context) {
		if err := discord.Send(ctx, notification); err != nil {
			slog.Error("Failed to send reauth-required notification", "error", err)
		}
	})
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
		fmt.Sprintf("🔌 Connection lost. %s", detail))

	if !enabled || discord == nil || cfg.SystemChannelID == "" {
		slog.Debug("Connection-lost Discord notification skipped: system channel not configured")
		return
	}

	notification := Notification{
		Type:      NotificationTypeConnectionLost,
		Title:     "🔌 Connection lost",
		Message:   detail,
		ChannelID: cfg.SystemChannelID,
	}

	m.goDispatch(func(ctx context.Context) {
		if err := discord.Send(ctx, notification); err != nil {
			slog.Error("Failed to send connection-lost notification", "error", err)
		}
	})
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
		"✅ Connection restored. Twitch API and PubSub connectivity is back.")

	if !enabled || discord == nil || cfg.SystemChannelID == "" {
		slog.Debug("Connection-restored Discord notification skipped: system channel not configured")
		return
	}

	notification := Notification{
		Type:      NotificationTypeConnectionRestored,
		Title:     "✅ Connection restored",
		Message:   "Twitch API and PubSub connectivity is back.",
		ChannelID: cfg.SystemChannelID,
	}

	m.goDispatch(func(ctx context.Context) {
		if err := discord.Send(ctx, notification); err != nil {
			slog.Error("Failed to send connection-restored notification", "error", err)
		}
	})
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

	m.goDispatch(func(ctx context.Context) {
		if err := discord.Send(ctx, notification); err != nil {
			slog.Error("Failed to send health-transition notification", "error", err)
		}
	})
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
	m.goDispatch(func(ctx context.Context) {
		if err := discord.Send(ctx, notification); err != nil {
			slog.Error("Failed to send drop-transition notification", "error", err)
		}
	})
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

	m.goDispatch(func(ctx context.Context) {
		if err := discord.Send(ctx, notification); err != nil {
			slog.Error("Failed to send update-available notification", "error", err)
		}
	})
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

	m.goDispatch(func(ctx context.Context) {
		if err := discord.Send(ctx, notification); err != nil {
			slog.Error("Failed to send update-failed notification", "error", err)
		}
	})
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
