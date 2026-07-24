package miner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/chat"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/debug"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/discovery"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/eligibility"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/logger"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/updater"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

type Miner struct {
	config     *config.Config
	configPath string
	auth       *auth.TwitchAuth
	client     *api.TwitchClient

	streamers *streamer.Manager

	db               *database.DB
	dbBasePath       string
	wsPool           *pubsub.WebSocketPool
	chatManager      *chat.ChatManager
	watcher          *watcher.MinuteWatcher
	dropsTracker     *drops.DropsTracker
	dropCatalog      *drops.CampaignCatalog
	discovery        *discovery.Manager
	healthCenter     *health.Center
	canary           *health.Canary
	avoidList        *health.AvoidList
	progressWatchdog *health.ProgressWatchdog
	policySnap       atomic.Pointer[policySnapshot]
	analyticsSvc     *analytics.Service
	webServer        *web.Server
	notifications    *notifications.Manager
	debugServer      *debug.Server

	// capabilityTopics/chatPresence are the runtime-capability reconciliation
	// seams: nil in production (the real wsPool/chatManager are used), injected
	// by tests to observe the desired-state plan without network side effects.
	// reconcileMu serializes whole reconciliation sweeps (see
	// reconcileRuntimeCapabilities); it is never held while mu is being taken.
	capabilityTopics topicReconciler
	chatPresence     chatToggler
	reconcileMu      sync.Mutex

	deviceID          string
	externalAnalytics bool
	// ownsDB is true only when initialize() opened the database itself
	// (library use); cmd/miner injects the handle via SetDatabase and keeps
	// ownership of its Close.
	ownsDB bool

	// autoUpdate holds the auto-update watcher configuration set via
	// ConfigureAutoUpdate before Run. When nil the watcher is not started.
	autoUpdate *autoUpdateConfig
	// shutdownFn cancels the run context so an applied binary update can ask
	// the miner to exit cleanly (exit 0) and let the supervisor restart it.
	shutdownFn context.CancelFunc
	// runCtx is the run-scoped context, captured in Run before any component
	// starts. Auth recovery triggered from long-lived consumers (PubSub
	// ERR_BADAUTH) is bound to it so shutdown releases recovery waiters.
	runCtx context.Context

	nextStreamCheck    time.Time
	streamCheckTrigger chan struct{}

	// startedAt/reauthRequired/connectionLost/connectionDetail feed the debug
	// snapshot's overall status; all guarded by mu.
	startedAt                time.Time
	reauthRequired           bool
	connectionLost           bool
	connectionDetail         string
	connectionDegraded       bool
	connectionDegradedDetail string

	// authRecoveryObserver bounds the consumer-triggered recovery path (see
	// recoverFromRejectedGeneration) to ONE observer goroutine at a time — a
	// goroutine-population guard only. Retry PACING is owned entirely by the
	// auth layer's per-generation backoff gate (auth.ErrRecoveryBackoff); the
	// miner imposes no cooldown of its own. authRecoverFn is the tests-only
	// seam over auth.Recover (nil in production).
	authRecoveryObserver atomic.Bool
	authRecoverFn        func(ctx context.Context, rejectedGen uint64) error

	// reauthNotified dedupes the operator "reauthorization required"
	// notification per outage (guarded by mu). Unlike the old sync.Once it is
	// RESET when a credential rotation succeeds, so a later, separate outage
	// notifies again and a recovered session never keeps a stale banner.
	reauthNotified bool

	// autoRedeemState tracks in-memory auto-redeem runtime per streamer
	// (points spent so far and which rewards were already redeemed in the
	// current availability window). Guarded by mu; reset on restart and
	// whenever the streamer's auto-redeem config is edited.
	autoRedeemState map[string]*autoRedeemRuntime

	mu sync.RWMutex

	// importMu serializes the read-modify-write in ImportStreamers so two
	// concurrent imports can't both read the pre-write snapshot and lose one
	// another's additions. GetRuntimeSettings (RLock) and ApplySettings (Lock)
	// are separate acquisitions, so mu alone does not make that pair atomic.
	importMu sync.Mutex
	// importApply is the apply step ImportStreamers runs after merging; nil in
	// production (falls back to ApplySettings). It exists only as a test seam so
	// the serialization can be exercised without the network/pubsub side effects
	// of the real apply path.
	importApply func(settings.RuntimeSettings)
}

// autoRedeemRuntime is the per-streamer in-memory budget/window bookkeeping for
// auto-redeeming custom rewards.
type autoRedeemRuntime struct {
	// spent is the total points auto-redeemed for this streamer this run.
	spent int
	// redeemed marks reward IDs already auto-redeemed while they were
	// continuously available, so a reward is redeemed once per availability
	// window (edge-triggered) instead of every poll. Cleared for a reward when
	// it is next seen unavailable (e.g. on cooldown), re-arming it.
	redeemed map[string]bool
}

func New(cfg *config.Config, configPath string) *Miner {
	deviceID := util.DeviceID()

	return &Miner{
		config:             cfg,
		configPath:         configPath,
		deviceID:           deviceID,
		streamCheckTrigger: make(chan struct{}, 1),
		autoRedeemState:    make(map[string]*autoRedeemRuntime),
	}
}

// autoUpdateConfig captures the auto-update settings resolved from CLI
// flags/env at startup.
type autoUpdateConfig struct {
	enabled  bool
	interval time.Duration
}

// ConfigureAutoUpdate enables the background release-update watcher. Called
// before Run; with enabled=false the watcher still checks periodically and
// logs/notifies when a newer release exists, but never replaces the binary.
func (m *Miner) ConfigureAutoUpdate(enabled bool, interval time.Duration) {
	m.autoUpdate = &autoUpdateConfig{enabled: enabled, interval: interval}
}

func (m *Miner) SetAnalyticsService(svc *analytics.Service) {
	m.analyticsSvc = svc
	m.externalAnalytics = true
}

// SetDatabase injects an externally-owned database handle (cmd/miner opens
// it and closes it after Run returns). When set, the miner neither opens nor
// closes the DB; without it (library use) initialize() opens the handle and
// stop() closes it — exactly one owner either way.
func (m *Miner) SetDatabase(db *database.DB) {
	m.db = db
}

func (m *Miner) SetWebServer(server *web.Server) {
	m.webServer = server
}

// Run starts the miner and blocks until the context is cancelled.
// The caller is responsible for handling OS signals and cancelling the context.
func (m *Miner) Run(ctx context.Context) error {
	// Derive a cancelable context so an applied auto-update can request a
	// clean shutdown (which returns nil from Run -> process exits 0).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	m.shutdownFn = cancel
	m.runCtx = ctx

	if err := m.initialize(); err != nil {
		return fmt.Errorf("initialization failed: %w", err)
	}

	if err := m.authenticate(ctx); err != nil {
		return fmt.Errorf("authentication failed: %w", err)
	}

	if err := m.loadStreamers(); err != nil {
		return fmt.Errorf("failed to load streamers: %w", err)
	}

	m.setupComponents(ctx)

	if err := m.subscribeToTopics(); err != nil {
		return fmt.Errorf("failed to subscribe to topics: %w", err)
	}

	m.startMining(ctx)

	<-ctx.Done()
	slog.Info("Shutting down...")

	m.stop()

	return nil
}

func (m *Miner) initialize() error {
	slog.Info("Initializing Twitch Channel Points Miner")

	if err := os.MkdirAll("cookies", 0755); err != nil {
		return fmt.Errorf("failed to create cookies directory: %w", err)
	}
	if err := os.MkdirAll("logs", 0755); err != nil {
		return fmt.Errorf("failed to create logs directory: %w", err)
	}

	m.dbBasePath = filepath.Join("database", m.config.Username)

	// cmd/miner injects the DB via SetDatabase and keeps ownership (its
	// deferred Close runs after stop()). Opening here is the library-use
	// fallback, and only then does the miner own the close in stop().
	if m.db == nil {
		db, err := database.Open(m.dbBasePath)
		if err != nil {
			return fmt.Errorf("failed to open database: %w", err)
		}
		m.db = db
		m.ownsDB = true
	}

	return nil
}

func (m *Miner) authenticate(ctx context.Context) error {
	slog.Info("Authenticating with Twitch")

	m.auth = auth.NewTwitchAuth(m.config.Username, m.deviceID)
	// Recovery-owner work (refresh, device-flow polling) is bounded by the run
	// context, not by whichever rejected request happened to trigger it.
	m.auth.SetLifecycleContext(ctx)

	if m.webServer != nil {
		broadcaster := m.webServer.GetStatusBroadcaster()
		m.auth.SetEventCallback(func(event auth.AuthEvent) {
			switch event.Type {
			case auth.AuthEventCode:
				broadcaster.SetAuthRequired(event.VerificationURI, event.UserCode, event.ExpiresIn)
			case auth.AuthEventCompleted:
				broadcaster.SetStatus(web.StatusLoadingStreamers, "Loading streamers...")
			case auth.AuthEventError:
				if event.Error != nil {
					broadcaster.SetStatus(web.StatusError, event.Error.Error())
				}
			}
		})
	}

	if err := m.auth.Login(ctx); err != nil {
		return err
	}

	m.client = api.NewTwitchClient(m.auth, m.deviceID)
	m.client.UpdateClientVersion()
	m.client.SetAuthErrorHandler(m.handleAuthError)

	// The miner cannot start without its own user ID (pubsub topics, watch
	// payloads), so a temporary Twitch outage here — stale persisted-query
	// hashes, a long 5xx spell — is retried in-process instead of exiting:
	// exiting would only trade this loop for a container crash-loop that also
	// takes the dashboard down. Token rejection and a genuinely unknown login
	// (config typo) still fail fast inside retryStartupLookup.
	var retryBroadcaster *web.StatusBroadcaster
	if m.webServer != nil {
		retryBroadcaster = m.webServer.GetStatusBroadcaster()
	}
	retried := false
	userID, err := retryStartupLookup(ctx, func() (string, error) {
		return m.client.GetChannelID(m.config.Username)
	}, func(attempt int, err error, next time.Duration) {
		retried = true
		slog.Warn("Startup: could not resolve own Twitch user ID; retrying",
			"attempt", attempt,
			"nextWaitSeconds", int(next.Seconds()),
			"error", err)
		if retryBroadcaster != nil {
			retryBroadcaster.SetStatus(web.StatusError,
				fmt.Sprintf("Twitch temporarily unavailable — retrying automatically (attempt %d, next try in %ds)",
					attempt, int(next.Seconds())))
		}
	})
	if err != nil {
		return fmt.Errorf("failed to get user ID: %w", err)
	}
	if retried && retryBroadcaster != nil {
		// Clear the retry banner now that Twitch answered; without this the
		// overlay would keep showing the last error until loadStreamers runs.
		retryBroadcaster.SetStatus(web.StatusLoadingStreamers, "Loading streamers...")
	}
	// Explicit identity-binding guard. The token session's user ID (validated
	// via /oauth2/validate at login/promotion, or loaded from the stored
	// record on a degraded startup) is the identity source; GetChannelID's
	// resolution of the configured username is only a CROSS-CHECK, never a
	// substitute for it — an empty session ID after a successful Login is an
	// invariant violation that fails startup rather than fabricating
	// authority from the lookup. Full rename reconciliation is BKM-006; a
	// mismatch fails closed (nothing is deleted — the operator decides).
	if err := verifyIdentityBinding(m.auth.GetUserID(), userID); err != nil {
		return fmt.Errorf("%w: session/profile identity binding failed for %q; remove cookies/%s.json or fix the configured username",
			err, m.config.Username, m.config.Username)
	}

	if err := m.auth.SaveAuth(); err != nil {
		slog.Warn("Failed to save auth", "error", err)
	}

	slog.Info("Authentication successful", "username", m.config.Username, "userID", userID)
	return nil
}

func (m *Miner) loadStreamers() error {
	var broadcaster *web.StatusBroadcaster
	if m.webServer != nil {
		broadcaster = m.webServer.GetStatusBroadcaster()
		broadcaster.SetStatus(web.StatusLoadingStreamers, "Loading streamers...")
	}

	var progressCallback streamer.ProgressCallback
	if broadcaster != nil {
		progressCallback = func(current, total int, username string) {
			broadcaster.SetStreamerProgress(current, total, username)
		}
	}

	m.streamers = streamer.NewManager(m.client, m.config.StreamerSettings)
	// Wire the persisted streak-grant cache BEFORE loading, so the initial
	// roster hydrates and a restart mid-broadcast does not re-pursue streaks
	// already granted on the still-live broadcast.
	m.streamers.SetStreakCache(streamer.NewStreakCache(filepath.Join(m.dbBasePath, "streak_cache.json")))
	return m.streamers.LoadFromConfig(m.config.Streamers, progressCallback)
}

func (m *Miner) setupComponents(ctx context.Context) {
	streamers := m.streamers.All()

	m.wsPool = pubsub.NewWebSocketPool(m.client, func() pubsub.AuthSnapshot {
		snap := m.auth.Snapshot()
		return pubsub.AuthSnapshot{Token: snap.AccessToken, Generation: snap.Generation}
	}, streamers, m.config.RateLimits)
	m.wsPool.SetMessageHandler(m.handlePubSubMessage)
	m.wsPool.SetBetHealthGate(minerBetHealthGate{m})
	m.wsPool.SetRiskSettings(m.config.PredictionRisk)
	m.wsPool.SetStatusHandler(m.handleStatusChange)
	m.wsPool.SetAuthErrorHandler(m.handlePubSubAuthError)
	if m.analyticsSvc != nil {
		m.wsPool.SetBetResultHandler(m.recordBetResult)
	}

	// Registered AFTER m.wsPool is assigned (SetRotationCallback's mutex
	// gives the flight goroutines a happens-before edge to that write, so the
	// callback's m.wsPool read is race-free). After a successful credential
	// rotation (refresh or device flow): clear any reauth-required state and
	// run the bounded PubSub user-topic re-authorization sweep. IRC and GQL
	// read the current token per dial/request and need no sweep. The callback
	// receives only the generation number, never token material.
	m.auth.SetRotationCallback(func(generation uint64) {
		slog.Info("Twitch credentials rotated; re-authorizing PubSub user topics", "generation", generation)
		m.clearReauthRequired()
		m.wsPool.ReauthorizeUserTopics()
	})

	if m.config.EnableAnalytics {
		if m.externalAnalytics && m.analyticsSvc != nil {
			if m.webServer != nil {
				m.webServer.AttachStreamers(streamers)
				m.webServer.SetSettingsProvider(m)
				m.webServer.SetSettingsUpdateCallback(m.ApplySettings)
				m.webServer.SetNextStreamCheckProvider(m)
				m.webServer.SetRewardsProvider(m)
				m.webServer.SetOverviewProvider(m)
				m.webServer.SetPredictionControlProvider(m)
			}
		} else {
			svc, err := analytics.NewService(m.db, m.dbBasePath, m.config.Analytics.RetentionDays)
			if err != nil {
				slog.Error("Failed to create analytics service", "error", err)
			} else {
				m.analyticsSvc = svc
			}

			m.webServer = web.NewServer(
				m.config.Analytics,
				m.config.Username,
				m.dbBasePath,
				m.analyticsSvc,
				streamers,
			)
			if m.webServer != nil {
				m.webServer.SetSettingsProvider(m)
				m.webServer.SetSettingsUpdateCallback(m.ApplySettings)
				m.webServer.SetNextStreamCheckProvider(m)
				m.webServer.SetRewardsProvider(m)
				m.webServer.SetOverviewProvider(m)
				m.webServer.SetPredictionControlProvider(m)
			}
		}
	}

	streamerNames := m.streamers.Names()

	if m.config.Discord.Enabled || notifications.AnyMessageProviderConfigured(m.config.Username) {
		notifMgr, err := notifications.NewManager(&m.config.Discord, &m.config.Notifications, m.db, streamerNames, m.config.Username)
		if err != nil {
			slog.Error("Failed to create notification manager; notifications stay DISABLED until the underlying problem is fixed", "error", err)
			events.Record(events.TypeModuleInitFailed, "", "notifications: "+err.Error())
		} else {
			m.notifications = notifMgr
			m.notifications.InitializePointsTracking(m.streamers.PointsMap())

			if err := m.notifications.Start(ctx); err != nil {
				slog.Error("Failed to start notification manager", "error", err)
			}
		}
	}

	if m.webServer != nil {
		m.webServer.SetDiscordEnabled(m.config.Discord.Enabled)
		if m.notifications != nil {
			m.webServer.SetNotificationManager(m.notifications)
		}
	}

	// Resolve the dashboard/notification display time zone once (from the logger
	// config; production sets Asia/Jerusalem) and share it, so absolute times on
	// the Drops "Upcoming" tab and in the upcoming-campaign alert match. Standard
	// time.Location handles DST; an empty/invalid zone falls back to local time.
	displayLoc := resolveDisplayLocation(m.config.Logger.TimeZone)
	if m.webServer != nil {
		m.webServer.SetDisplayLocation(displayLoc)
	}
	if m.notifications != nil {
		m.notifications.SetDisplayLocation(displayLoc)
	}

	var mentionHandler chat.MentionHandler
	if m.notifications != nil {
		mentionHandler = m.notifications.NotifyMention
	}

	var chatLogger chat.ChatLogger
	chatLogsEnabled := m.config.EnableAnalytics && m.config.Analytics.EnableChatLogs
	slog.Debug("Chat logging config", "enableAnalytics", m.config.EnableAnalytics, "enableChatLogs", m.config.Analytics.EnableChatLogs, "chatLogsEnabled", chatLogsEnabled)
	if chatLogsEnabled && m.analyticsSvc != nil {
		chatLogger = analytics.NewChatLoggerAdapter(m.analyticsSvc)
	}
	m.chatManager = chat.NewChatManager(m.config.Username, func() chat.TokenSnapshot {
		snap := m.auth.Snapshot()
		return chat.TokenSnapshot{Token: snap.AccessToken, Generation: snap.Generation}
	}, chatLogger, chatLogsEnabled, mentionHandler)
	// A documented IRC login-authentication-failed NOTICE joins the same
	// generation-keyed single-flight recovery as GQL 401s and PubSub BADAUTHs.
	m.chatManager.SetAuthErrorHandler(func(rejectedGeneration uint64) {
		m.recoverFromRejectedGeneration(rejectedGeneration, "irc")
	})

	var watchTimeStore *watcher.WatchTimeStore
	if m.db != nil {
		store, err := watcher.NewWatchTimeStore(m.db)
		if err != nil {
			slog.Error("Failed to create watch-time store, rotation fairness will not persist across restarts", "error", err)
			events.Record(events.TypeModuleInitFailed, "", "watch_time: "+err.Error())
		} else {
			watchTimeStore = store
		}
	}

	m.watcher = watcher.NewMinuteWatcher(
		m.client,
		streamers,
		m.config.Priority,
		m.config.RateLimits,
		watchTimeStore,
	)
	// When enabled, tracked streamers keep their watch slot ahead of any
	// directory-discovered channel (discovery only fills idle slots).
	m.watcher.SetPreferConfiguredOverDiscovery(m.config.DiscoveryPreferTracked)

	m.dropsTracker = drops.NewDropsTracker(
		m.client,
		streamers,
		m.config.RateLimits,
		m.config.DropBlacklist,
	)
	// Seed the drop-campaign game filter before the sync loops start, so the very
	// first sync already tracks only the allowed games.
	m.dropsTracker.UpdateGameFilter(m.config.DropCampaignGameIDs, m.config.DropCampaignGames)

	// Alert (opt-in, off by default) when Twitch first reports a new relevant
	// upcoming campaign. The adapter reads m.notifications at call time and the
	// manager owns the opt-in gate + durable dedupe, so no alert is ever sent
	// unless the operator enabled the event.
	m.dropsTracker.SetUpcomingNotifier(minerUpcomingNotifier{m})

	// Durably record each drop claim (under a hidden analytics bucket) so the
	// daily summary can count claims across restarts, not just from the
	// in-memory event ring buffer.
	if m.analyticsSvc != nil {
		m.dropsTracker.SetDropClaimedHook(m.recordDropClaimed)
	}

	// Wire the durable drop-campaign catalog (the "Past" tab's data source) so
	// every observed campaign is recorded and survives its expiry.
	if m.db != nil {
		if catalog, err := drops.NewCampaignCatalog(m.db); err != nil {
			slog.Error("Failed to initialize drop campaign catalog; the Past-campaigns catalog stays DISABLED", "error", err)
			events.Record(events.TypeModuleInitFailed, "", "drop_catalog: "+err.Error())
		} else {
			m.dropsTracker.SetCatalog(catalog)
			m.dropCatalog = catalog
		}
	}

	// A reported watched minute is real drop progress; nudge the drops tracker
	// to refresh its lightweight progress view promptly so the Drops page stays
	// within seconds of Twitch instead of lagging up to a full sync interval.
	m.watcher.SetOnMinuteWatched(m.dropsTracker.TriggerProgressSync)

	// The discovery manager is always constructed (so the Settings page can
	// enable it at runtime), but it stays dormant — no API calls, no watch
	// slot — while the configured game list is empty. It gets the streamer
	// manager so it never duplicates a channel the rotation already watches.
	m.discovery = discovery.NewManager(
		m.client,
		m.dropsTracker,
		m.streamers,
		m.config.RateLimits,
		m.config.DirectoryGames,
		m.config.DiscoveryMode,
		m.config.DiscoveryPreferSubscribed,
	)

	// Discovery is a candidate source for the unified slot broker, not an
	// independent watch slot: it proposes channels and the broker decides
	// whether they occupy one of the two Twitch watch slots (competing with the
	// configured list). SetSlotStatus lets discovery report whether its
	// proposal actually got a slot.
	m.watcher.AddSource(m.discovery)
	m.discovery.SetSlotStatus(m.watcher)

	// Health center aggregates operational signals; the canary verifies the
	// watch transport independently (one real beacon, opportunistically or once
	// past max staleness — never a permanent slot). Both are always constructed;
	// the canary stays inert until a channel is configured.
	m.healthCenter = health.NewCenter()
	m.canary = health.NewCanary(
		m.healthCenter,
		m.client,
		watcher.NewMinuteSender(m.client),
		minerHealthNotifier{m}, // reads m.notifications at call time (it may be created later)
		m.watcher,
		healthCanaryConfig(m.config.Health),
	)

	// The drop-progress watchdog detects a tracked drop whose minutes stop
	// accruing despite healthy-looking plumbing and runs the staged recovery
	// pipeline. Its channel-switch stage works through the avoid list — the
	// broker and discovery stop selecting an excluded channel — so the broker
	// keeps sole authority over slots, and its session-repair stages are staged
	// INTO the broker loop (RequestSessionRefresh), so the loop goroutine stays
	// the single writer of live watch sessions.
	m.avoidList = health.NewAvoidList()
	m.watcher.SetAvoidChecker(m.avoidList)
	m.discovery.SetAvoidChecker(m.avoidList)
	m.progressWatchdog = health.NewProgressWatchdog(
		m.healthCenter,
		m.dropsTracker,
		m.watcher,
		watcher.NewMinuteSender(m.client),
		minerDropNotifier{m}, // reads m.notifications at call time
		m.avoidList,
		m.resolveStreamer,
		healthWatchdogConfig(m.config.Health),
	)

	if m.webServer != nil {
		m.webServer.SetCampaignsProvider(m.dropsTracker)
		m.webServer.SetDropCatalogProvider(m)
		m.webServer.SetFollowedProvider(m)
		// Read-only Twitch game-ID lookup for the Settings "find game ID" helper;
		// the authenticated client resolves a name to its opaque game ID directly.
		m.webServer.SetGameIDResolver(m.client)
		m.webServer.SetDiscoveryProvider(m.discovery)
		m.webServer.SetHealthProvider(m)
		m.webServer.SetDropProgressProvider(m)
		m.webServer.SetPolicyProvider(m)
	}

	if m.config.ClaimDropsOnStartup {
		slog.Info("Claiming all drops from inventory on startup")
	}
}

func (m *Miner) subscribeToTopics() error {
	slog.Info("Subscribing to PubSub topics")

	userID := m.auth.GetUserID()

	if err := m.wsPool.Submit(pubsub.NewTopic(pubsub.TopicCommunityPointsUser, userID)); err != nil {
		return err
	}
	if err := m.wsPool.Submit(pubsub.NewTopic(pubsub.TopicPredictionsUser, userID)); err != nil {
		return err
	}

	for _, s := range m.streamers.All() {
		channelID := s.ChannelID
		desired := desiredCapabilityTopics(s.GetSettings())

		for _, tt := range capabilityTopicOrder {
			if !desired[tt] {
				continue
			}
			_ = m.wsPool.EnsureTopic(pubsub.NewTopic(tt, channelID), true)
		}
	}

	return nil
}

func (m *Miner) startMining(ctx context.Context) {
	slog.Info("Starting mining operations")

	m.mu.Lock()
	m.startedAt = time.Now()
	m.mu.Unlock()

	// The debug server starts here - after every component is wired up - so
	// its snapshot handler never observes half-initialized miner fields.
	if m.config.Debug.Enabled {
		logPath := ""
		if m.config.Logger.Save {
			logPath = logger.LogFilePath(m.config.Username)
		}
		m.debugServer = debug.NewServer(m.config.Debug.Port, m.BuildDebugSnapshot, logPath)
		if err := m.debugServer.Start(); err != nil {
			slog.Error("Failed to start debug server", "error", err)
			m.debugServer = nil
		}

		// Publish the same in-process snapshot builder on the main dashboard
		// (relative URL, full auth/middleware chain) so the Logs-page button
		// works from remote browsers — "localhost" there is the viewer's
		// machine, not this container. Wired here, alongside the debug server,
		// so the dashboard route also never observes half-initialized fields.
		if m.webServer != nil {
			m.webServer.SetDebugSnapshotProvider(m.BuildDebugSnapshot)
			m.webServer.SetDebugURL(web.DebugSnapshotPath)
		}
	}

	events.Record(events.TypeMinerStarted, "", "mining operations started")

	for _, s := range m.streamers.All() {
		m.client.CheckStreamerOnline(s)
		m.chatManager.ToggleChat(s)
	}

	m.watcher.Start(ctx)
	m.dropsTracker.Start(ctx)
	m.discovery.Start(ctx)
	if m.canary != nil {
		m.canary.Start(ctx)
	}
	if m.progressWatchdog != nil {
		m.progressWatchdog.Start(ctx)
	}

	if m.webServer != nil {
		if !m.externalAnalytics {
			// Fail-closed: on a non-loopback bind without credentials the
			// dashboard stays down (mining continues); the primary
			// cmd/miner path aborts the whole process for the same error.
			if err := m.webServer.Start(); err != nil {
				slog.Error("Web server NOT started", "error", err)
			}
		}
		m.webServer.GetStatusBroadcaster().SetStatus(web.StatusRunning, "Mining active")
	}

	go m.streamCheckLoop(ctx)
	go m.healthWatchdogLoop(ctx)
	go m.bonusPollLoop(ctx)
	go m.subscriptionProbeLoop(ctx)
	// Hourly token validation is a Twitch requirement (validate on startup —
	// done inside Login — and hourly thereafter). One validator per session;
	// it joins the shared single-flight recovery on an authoritative 401.
	go m.auth.RunHourlyValidation(ctx)
	if m.config.DailySummary.Enabled && m.analyticsSvc != nil {
		go m.dailySummaryLoop(ctx)
	}
	m.startAutoUpdater(ctx)
}

// startAutoUpdater launches the background release-update watcher when it has
// been configured via ConfigureAutoUpdate. It runs non-blocking: a failed
// check or a failed binary swap is logged and the miner keeps running.
func (m *Miner) startAutoUpdater(ctx context.Context) {
	if m.autoUpdate == nil {
		return
	}

	upd := updater.New(updater.Options{
		Repo:           version.Repo,
		CurrentVersion: version.Version,
		Enabled:        m.autoUpdate.enabled,
		CheckInterval:  m.autoUpdate.interval,
		Notify:         m.notifyUpdateAvailable,
		NotifyFailure:  m.notifyUpdateFailed,
		OnUpdate: func() {
			// Cancel the run context so every component shuts down cleanly and
			// the process exits 0; the container/service supervisor then
			// restarts on the freshly written binary.
			if m.shutdownFn != nil {
				m.shutdownFn()
			}
		},
	})

	go upd.Run(ctx)
}

// notifyUpdateAvailable logs and, when Discord is enabled, dispatches an
// update-available notification. Reads the notifications manager under lock so
// it works even if Discord was toggled on after startup.
func (m *Miner) notifyUpdateAvailable(current, latest, releaseURL string) {
	events.Record(events.TypeUpdateAvailable, "", fmt.Sprintf("%s -> %s", current, latest))

	m.mu.RLock()
	notifMgr := m.notifications
	m.mu.RUnlock()

	if notifMgr != nil {
		notifMgr.NotifyUpdateAvailable(current, latest, releaseURL)
	}
}

// notifyUpdateFailed logs and, when Discord is enabled, dispatches an
// update-failed notification (fail-closed checksum refusal, download error,
// or a failed binary swap). Mirrors notifyUpdateAvailable.
func (m *Miner) notifyUpdateFailed(current, latest, reason string) {
	events.Record(events.TypeUpdateFailed, "", fmt.Sprintf("%s -> %s: %s", current, latest, reason))

	m.mu.RLock()
	notifMgr := m.notifications
	m.mu.RUnlock()

	if notifMgr != nil {
		notifMgr.NotifyUpdateFailed(current, latest, reason)
	}
}

// bonusPollInterval is how often the GQL polling fallback re-checks each online
// streamer for an unclaimed channel-points bonus chest.
const bonusPollInterval = 60 * time.Second

// bonusPollLoop is the GQL polling fallback for channel-points bonus chests.
// The primary claim path reacts to the community-points-user PubSub
// "claim-available" event, but that event is not always delivered, so a chest
// can sit unclaimed until it expires. Every bonusPollInterval this re-reads
// each online streamer's channel-points context and claims any bonus PubSub
// missed. Claims made here are logged distinctly so it stays visible how often
// PubSub actually drops the event.
func (m *Miner) bonusPollLoop(ctx context.Context) {
	ticker := time.NewTicker(bonusPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.pollBonuses()
		}
	}
}

func (m *Miner) pollBonuses() {
	for _, s := range m.streamers.All() {
		if !s.GetIsOnline() {
			continue
		}

		// Centralized capability gate: the polling fallback must not claim a bonus
		// when Channel Points are confirmed disabled or not yet confirmed.
		if err := pointsActionGate(s, eligibility.TaskBonusClaim); err != nil {
			slog.Debug("Skipping bonus poll: not eligible", "streamer", s.Username, "reason", err.Error())
			m.evaluateAutoRedeem(s)
			continue
		}

		claimed, err := m.client.ClaimAvailableBonus(s)
		if err != nil {
			slog.Debug("Bonus poll failed", "streamer", s.Username, "error", err)
		} else if claimed {
			slog.Info("Claimed channel points bonus via GQL fallback poll (PubSub missed the claim-available event)",
				"streamer", s.Username)
			events.Record(events.TypeBonusClaimed, s.Username, "bonus claimed (GQL fallback)")
		}

		m.evaluateAutoRedeem(s)
	}
}

func (m *Miner) streamCheckLoop(ctx context.Context) {
	interval := time.Duration(m.config.RateLimits.StreamCheckInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	m.mu.Lock()
	m.nextStreamCheck = time.Now().Add(interval)
	m.mu.Unlock()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAllStreamers()
			m.mu.Lock()
			m.nextStreamCheck = time.Now().Add(interval)
			m.mu.Unlock()
		case <-m.streamCheckTrigger:
			m.checkUncheckedStreamers()
		}
	}
}

func (m *Miner) checkAllStreamers() {
	for _, s := range m.streamers.All() {
		m.client.CheckStreamerOnline(s)
		m.chatManager.ToggleChat(s)
	}
}

func (m *Miner) checkUncheckedStreamers() {
	interval := time.Duration(m.config.RateLimits.StreamCheckInterval) * time.Second
	now := time.Now()

	for _, s := range m.streamers.All() {
		lastChecked := s.GetLastChecked()
		if lastChecked.IsZero() || now.Sub(lastChecked) >= interval {
			m.client.CheckStreamerOnline(s)
			m.chatManager.ToggleChat(s)
		}
	}
}

func (m *Miner) triggerStreamCheck() {
	select {
	case m.streamCheckTrigger <- struct{}{}:
	default:
	}
}

func (m *Miner) GetNextStreamCheck() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.nextStreamCheck
}

func (m *Miner) handlePubSubMessage(msg *pubsub.PubSubMessage, s *models.Streamer) {
	switch msg.Topic.Type {
	case pubsub.TopicCommunityPointsUser:
		switch msg.Type {
		case "points-earned":
			if data := msg.Data; data != nil {
				if pointGain, ok := data["point_gain"].(map[string]interface{}); ok {
					if reasonCode, ok := pointGain["reason_code"].(string); ok {
						// Persist the grant->broadcast binding regardless of
						// analytics being enabled; runs on the pubsub handler
						// goroutine, outside any pool/watcher lock. The
						// in-memory MarkStreakEarned already happened in
						// UpdateHistory on the pool's own handling path.
						if reasonCode == "WATCH_STREAK" && m.streamers != nil {
							m.streamers.RecordStreakGrant(s.Username)
						}

						if m.analyticsSvc != nil {
							m.analyticsSvc.RecordPoints(s, reasonCode)

							switch reasonCode {
							case "WATCH_STREAK":
								if earned, ok := pointGain["total_points"].(float64); ok {
									m.analyticsSvc.RecordAnnotation(s, "WATCH_STREAK", fmt.Sprintf("+%d - Watch Streak", int(earned)))
								}
							case "RAID":
								if earned, ok := pointGain["total_points"].(float64); ok {
									m.analyticsSvc.RecordAnnotation(s, "RAID", fmt.Sprintf("+%d - Raid", int(earned)))
								}
							}
						}
					}
				}
			}

			if m.notifications != nil {
				m.notifications.NotifyPointsReached(s.Username, s.GetChannelPoints())
			}
		case "points-spent":
			if m.analyticsSvc != nil {
				m.analyticsSvc.RecordPoints(s, "Spent")
			}
		}

	case pubsub.TopicPredictionsUser:
		if m.analyticsSvc == nil {
			return
		}
		switch msg.Type {
		case "prediction-made":
			m.analyticsSvc.RecordAnnotation(s, "PREDICTION_MADE", "Prediction placed")
		case "prediction-result":
			if data := msg.Data; data != nil {
				if prediction, ok := data["prediction"].(map[string]interface{}); ok {
					if result, ok := prediction["result"].(map[string]interface{}); ok {
						if resultType, ok := result["type"].(string); ok {
							m.analyticsSvc.RecordAnnotation(s, resultType, "Prediction "+resultType)
						}
					}
				}
			}
		}
	}
}

// recordBetResult persists a settled prediction bet emitted by the pubsub pool
// into analytics for ROI reporting. It maps the pool's transport-local BetResult
// to analytics.BetRecord; the analytics write logs its own errors and never
// blocks the pool.
func (m *Miner) recordBetResult(r pubsub.BetResult) {
	if m.analyticsSvc == nil {
		return
	}
	m.analyticsSvc.RecordBet(analytics.BetRecord{
		EventID:    r.EventID,
		Streamer:   r.Streamer,
		Timestamp:  r.Timestamp.UnixMilli(),
		Strategy:   r.Strategy,
		ResultType: r.ResultType,
		Placed:     r.Placed,
		Won:        r.Won,
		Gained:     r.Gained,
		Odds:       r.Odds,
		Manual:     r.Manual,
	})
}

// verifyIdentityBinding refuses to bind credentials whose session user ID
// belongs to a different account than the one the configured username
// resolved to. An EMPTY session ID fails closed: a successful Login always
// leaves a session identity (validated at promotion, or disk-loaded on a
// degraded startup), so its absence means no identity was ever established —
// the username lookup must not be promoted into one.
func verifyIdentityBinding(sessionUserID, resolvedUserID string) error {
	if sessionUserID == "" {
		return fmt.Errorf("%w: login completed without a session user ID", auth.ErrIdentityMismatch)
	}
	if sessionUserID != resolvedUserID {
		return auth.ErrIdentityMismatch
	}
	return nil
}

// handlePubSubAuthError reacts to a PubSub ERR_BADAUTH: it funnels the
// rejected credential generation into the shared single-flight auth recovery
// on a separate goroutine (the pool invokes this on a read-loop goroutine that
// must not block on a refresh or a device flow). A stale BADAUTH for an
// already-rotated generation returns immediately from Recover without a second
// refresh; concurrent BADAUTHs from several sockets join the same flight. On
// success the rotation callback runs the bounded user-topic re-authorization
// sweep; only a DEFINITIVE recovery failure escalates to the reauth-required
// path — a transient endpoint failure or a shutdown cancellation does not.
func (m *Miner) handlePubSubAuthError(err error) {
	var authErr *pubsub.AuthError
	rejectedGen := m.auth.Generation()
	if errors.As(err, &authErr) {
		rejectedGen = authErr.Generation
	}
	m.recoverFromRejectedGeneration(rejectedGen, "pubsub")
}

// authConsumerRecoveryWait bounds how long one consumer-triggered observer
// goroutine waits on the shared recovery flight before giving up its watch (a
// device-flow flight runs for minutes; a later rejection re-arms a fresh
// observer). Keeps the goroutine population bounded at one.
const authConsumerRecoveryWait = 2 * time.Minute

// recoverFromRejectedGeneration funnels an authoritative rejection of the
// given credential generation (from any long-lived consumer) into the shared
// single-flight recovery. At most ONE observer goroutine exists at a time — a
// goroutine-population guard, nothing more: retry PACING is the auth layer's
// sole authority (its per-generation backoff gate answers a too-soon attempt
// with the retryable auth.ErrRecoveryBackoff and zero network traffic), so a
// fast rejection loop (IRC redial cycles) can never turn into an
// OAuth-endpoint storm or unbounded goroutine growth. Only a definitive
// failure escalates to the reauth-required path — a transient endpoint
// failure, an inconclusive outcome, a backoff refusal, or a
// shutdown/watch-timeout cancellation does not.
func (m *Miner) recoverFromRejectedGeneration(rejectedGen uint64, source string) {
	if m.auth.Generation() > rejectedGen {
		return // stale: already rotated past the rejected credentials
	}
	if !m.authRecoveryObserver.CompareAndSwap(false, true) {
		return // one observer is already watching the shared flight
	}

	parent := m.runCtx
	if parent == nil {
		parent = context.Background()
	}
	go func() {
		defer m.authRecoveryObserver.Store(false)
		ctx, cancel := context.WithTimeout(parent, authConsumerRecoveryWait)
		defer cancel()
		recoverFn := m.authRecoverFn
		if recoverFn == nil {
			recoverFn = func(ctx context.Context, gen uint64) error {
				_, err := m.auth.Recover(ctx, gen)
				return err
			}
		}
		if rerr := recoverFn(ctx, rejectedGen); rerr != nil {
			transient := errors.Is(rerr, auth.ErrAuthTransient) ||
				errors.Is(rerr, auth.ErrRecoveryInconclusive) ||
				errors.Is(rerr, auth.ErrRecoveryBackoff) ||
				errors.Is(rerr, context.Canceled) ||
				errors.Is(rerr, context.DeadlineExceeded)
			slog.Error("Auth rejection: recovery failed", "source", source, "error", rerr, "retryable", transient)
			if !transient {
				m.handleAuthError()
			}
		}
	}()
}

// handleAuthError marks the session as needing reauthorization after a
// DEFINITIVE recovery failure (recovery itself already ran and could not
// restore credentials). Notified at most once per outage; a subsequent
// successful rotation clears the state (see clearReauthRequired) so the banner
// never outlives the outage and a later separate outage notifies again.
func (m *Miner) handleAuthError() {
	m.mu.Lock()
	if m.reauthNotified {
		m.mu.Unlock()
		return
	}
	m.reauthNotified = true
	m.reauthRequired = true
	m.mu.Unlock()

	slog.Error("Twitch authorization expired or was revoked - reauthorization required")

	if m.notifications != nil {
		m.notifications.NotifyReauthRequired("Open the dashboard to complete the Twitch device login (or restart the miner).")
	}

	if m.webServer != nil {
		m.webServer.GetStatusBroadcaster().SetReauthRequired(true, "Twitch authorization expired or was revoked. Open the dashboard to complete the device login, or restart the miner.")
	}
}

// clearReauthRequired retracts the reauthorization banner/alert state after a
// successful credential rotation.
func (m *Miner) clearReauthRequired() {
	m.mu.Lock()
	wasRequired := m.reauthRequired
	m.reauthRequired = false
	m.reauthNotified = false
	m.mu.Unlock()

	if !wasRequired {
		return
	}
	slog.Info("Twitch authorization recovered; clearing the reauthorization-required state")
	if m.webServer != nil {
		m.webServer.GetStatusBroadcaster().SetReauthRequired(false, "")
	}
}

// subscriptionProbeInterval is the base cadence of the discovery subscription
// probe. It is deliberately slower than the 1-minute healthWatchdogLoop and far
// cheaper: each tick probes at most maxCandidateChecksPerTick+1 channels, and it
// no-ops entirely while DiscoveryPreferSubscribed is off. A ±20% jitter is
// applied so the probe cadence isn't a single predictable timer.
const subscriptionProbeInterval = 3 * time.Minute

// probeSubscribed reports whether the authenticated account is subscribed to
// login, proxied by an active channel-points multiplier (ChannelPointsContext) —
// the same signal the SUBSCRIBED watch priority uses. It probes a THROWAWAY
// streamer so the unlocked ActiveMultipliers write inside LoadChannelPointsContext
// never touches the shared discovery pool objects (which would race the broker
// loop).
func (m *Miner) probeSubscribed(login string) bool {
	s := models.NewStreamer(login, models.StreamerSettings{})
	if err := m.client.LoadChannelPointsContext(s); err != nil {
		return false
	}
	return s.ViewerHasPointsMultiplier()
}

// subscriptionProbeLoop periodically refreshes discovery's subscribed set on a
// slow, jittered cadence, kept separate from the 1-minute healthWatchdogLoop.
// RefreshSubscribedSet self-gates: it clears the set and skips all probes while
// the prefer-subscribed toggle is off, so this costs nothing by default.
func (m *Miner) subscriptionProbeLoop(ctx context.Context) {
	if m.discovery == nil {
		return
	}
	for {
		jitter := 1.0 + (rand.Float64()-0.5)*0.4 // ±20%
		delay := time.Duration(float64(subscriptionProbeInterval) * jitter)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		m.discovery.RefreshSubscribedSet(m.probeSubscribed)
	}
}

// healthWatchdogLoop periodically classifies the miner's connectivity to Twitch
// (GQL API + PubSub) and raises/clears the "connection lost" and "degraded"
// signals. A connection is only reported LOST when BOTH critical paths are
// confirmed unavailable; a normal idle API (no requests attempted) is never an
// outage. See connection_health.go for the classifier and transition rules.
func (m *Miner) healthWatchdogLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	var state connHealthState

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			now := time.Now()
			m.refreshHealthCenter(now)
			m.refreshPolicy(now)
			m.evaluateConnectionHealth(now, &state)
		}
	}
}

func (m *Miner) handleStatusChange(username string, status models.StreamerStatus) {
	if m.notifications == nil {
		return
	}

	// Only authoritative online/offline transitions notify. Unknown carries no
	// notification, so a transient check failure never fires a false "went offline".
	switch status {
	case models.StatusOnline:
		m.notifications.NotifyOnline(username)
	case models.StatusOffline:
		m.notifications.NotifyOffline(username)
	}
}

func (m *Miner) stop() {
	m.chatManager.Close()
	m.wsPool.Close()
	m.watcher.Stop()
	m.dropsTracker.Stop()
	m.discovery.Stop()
	if m.canary != nil {
		m.canary.Stop()
	}
	if m.progressWatchdog != nil {
		m.progressWatchdog.Stop()
	}

	if m.webServer != nil {
		m.webServer.Stop()
	}

	if m.debugServer != nil {
		m.debugServer.Stop()
	}

	if m.analyticsSvc != nil {
		_ = m.analyticsSvc.Close()
	}

	if m.notifications != nil {
		m.notifications.Stop()
	}

	// Close the DB only when the miner opened it itself (library use). In
	// the cmd/miner path main owns the handle and closes it after Run
	// returns — closing here would cut off writers that stop() does not
	// join (see Stage E) earlier than necessary.
	if m.db != nil && m.ownsDB {
		_ = m.db.Close()
	}

	m.streamers.PrintReport()
}

func (m *Miner) GetRuntimeSettings() settings.RuntimeSettings {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return settings.BuildRuntimeSettings(m.config)
}

func (m *Miner) GetDefaultSettings() settings.RuntimeSettings {
	m.mu.RLock()
	currentStreamers := m.config.Streamers
	m.mu.RUnlock()
	return settings.BuildDefaultSettings(currentStreamers)
}

func (m *Miner) ApplySettings(s settings.RuntimeSettings) {
	m.mu.Lock()

	oldDiscordEnabled := m.config.Discord.Enabled
	settings.ApplyToConfig(m.config, s)

	if m.watcher != nil {
		m.watcher.UpdateSettings(m.config.Priority, m.config.RateLimits)
		m.watcher.SetPreferConfiguredOverDiscovery(m.config.DiscoveryPreferTracked)
	}

	if m.dropsTracker != nil {
		m.dropsTracker.UpdateBlacklist(m.config.DropBlacklist)
		m.dropsTracker.UpdateGameFilter(m.config.DropCampaignGameIDs, m.config.DropCampaignGames)
		m.dropsTracker.UpdateSettings(m.config.RateLimits)
	}

	if m.discovery != nil {
		m.discovery.UpdateSettings(m.config.DirectoryGames, m.config.DiscoveryMode, m.config.DiscoveryPreferSubscribed, m.config.RateLimits)
	}

	added, removed, changed := m.streamers.ApplySettings(m.config.Streamers, m.config.StreamerSettings)

	discordCfg := m.config.Discord
	notifCfg := m.config.Notifications
	notifUsername := m.config.Username
	displayTZ := m.config.Logger.TimeZone
	notifMgr := m.notifications
	webServer := m.webServer
	wsPool := m.wsPool
	minuteWatcher := m.watcher
	dropsTracker := m.dropsTracker
	riskCfg := m.config.PredictionRisk

	m.mu.Unlock()

	// Push the updated GLOBAL prediction risk gates to the auto-bet path outside
	// the miner lock (SetRiskSettings takes the pool lock itself).
	if wsPool != nil {
		wsPool.SetRiskSettings(riskCfg)
	}

	// Propagate the new roster BEFORE topic reconciliation, so a frame arriving
	// on a just-subscribed topic can already resolve its streamer.
	if len(added) > 0 || len(removed) > 0 {
		allStreamers := m.streamers.All()
		if wsPool != nil {
			wsPool.UpdateStreamers(allStreamers)
		}
		if minuteWatcher != nil {
			minuteWatcher.UpdateStreamers(allStreamers)
		}
		if dropsTracker != nil {
			dropsTracker.UpdateStreamers(allStreamers)
		}
		if webServer != nil {
			webServer.AttachStreamers(allStreamers)
		}
		m.triggerStreamCheck()
	}

	// Reconcile runtime capabilities (per-channel PubSub topics + IRC presence)
	// for the WHOLE roster — added, removed, changed AND unchanged streamers —
	// with no miner lock held. The desired-state sweep is what applies an
	// existing streamer's toggles immediately and re-attempts any subscription a
	// previous apply left failed.
	m.reconcileRuntimeCapabilities(added, removed, changed)

	if notifMgr != nil {
		if err := notifMgr.UpdateDiscordConfig(&discordCfg); err != nil {
			slog.Error("Failed to update Discord config", "error", err)
		}
	} else if discordCfg.Enabled && !oldDiscordEnabled {
		newNotifMgr, err := notifications.NewManager(&discordCfg, &notifCfg, m.db, m.streamers.Names(), notifUsername)
		if err != nil {
			slog.Error("Failed to create notification manager", "error", err)
			events.Record(events.TypeModuleInitFailed, "", "notifications: "+err.Error())
		} else {
			m.mu.Lock()
			m.notifications = newNotifMgr
			m.mu.Unlock()

			newNotifMgr.InitializePointsTracking(m.streamers.PointsMap())
			newNotifMgr.SetDisplayLocation(resolveDisplayLocation(displayTZ))

			if err := newNotifMgr.Start(context.Background()); err != nil {
				slog.Error("Failed to start notification manager", "error", err)
			}

			if webServer != nil {
				webServer.SetNotificationManager(newNotifMgr)
			}
		}
	}

	if webServer != nil {
		webServer.SetDiscordEnabled(discordCfg.Enabled)
	}

	m.mu.Lock()
	if m.configPath != "" {
		if err := config.SaveConfig(m.configPath, m.config); err != nil {
			slog.Error("Failed to save config", "error", err)
		} else {
			slog.Info("Settings saved to config file")
		}
	}
	m.mu.Unlock()

	slog.Info("Runtime settings updated")
}
