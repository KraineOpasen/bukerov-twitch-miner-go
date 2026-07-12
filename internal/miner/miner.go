package miner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/PatrickWalther/twitch-miner-go/internal/analytics"
	"github.com/PatrickWalther/twitch-miner-go/internal/api"
	"github.com/PatrickWalther/twitch-miner-go/internal/auth"
	"github.com/PatrickWalther/twitch-miner-go/internal/chat"
	"github.com/PatrickWalther/twitch-miner-go/internal/config"
	"github.com/PatrickWalther/twitch-miner-go/internal/database"
	"github.com/PatrickWalther/twitch-miner-go/internal/debug"
	"github.com/PatrickWalther/twitch-miner-go/internal/drops"
	"github.com/PatrickWalther/twitch-miner-go/internal/events"
	"github.com/PatrickWalther/twitch-miner-go/internal/logger"
	"github.com/PatrickWalther/twitch-miner-go/internal/models"
	"github.com/PatrickWalther/twitch-miner-go/internal/notifications"
	"github.com/PatrickWalther/twitch-miner-go/internal/pubsub"
	"github.com/PatrickWalther/twitch-miner-go/internal/settings"
	"github.com/PatrickWalther/twitch-miner-go/internal/streamer"
	"github.com/PatrickWalther/twitch-miner-go/internal/util"
	"github.com/PatrickWalther/twitch-miner-go/internal/watcher"
	"github.com/PatrickWalther/twitch-miner-go/internal/web"
)

type Miner struct {
	config     *config.Config
	configPath string
	auth       *auth.TwitchAuth
	client     *api.TwitchClient

	streamers *streamer.Manager

	db            *database.DB
	dbBasePath    string
	wsPool        *pubsub.WebSocketPool
	chatManager   *chat.ChatManager
	watcher       *watcher.MinuteWatcher
	dropsTracker  *drops.DropsTracker
	analyticsSvc  *analytics.Service
	webServer     *web.Server
	notifications *notifications.Manager
	debugServer   *debug.Server

	deviceID          string
	externalAnalytics bool

	nextStreamCheck    time.Time
	streamCheckTrigger chan struct{}

	// startedAt/reauthRequired/connectionLost/connectionDetail feed the debug
	// snapshot's overall status; all guarded by mu.
	startedAt        time.Time
	reauthRequired   bool
	connectionLost   bool
	connectionDetail string

	authErrOnce sync.Once

	mu sync.RWMutex
}

func New(cfg *config.Config, configPath string) *Miner {
	deviceID := util.DeviceID()

	return &Miner{
		config:             cfg,
		configPath:         configPath,
		deviceID:           deviceID,
		streamCheckTrigger: make(chan struct{}, 1),
	}
}

func (m *Miner) SetAnalyticsService(svc *analytics.Service) {
	m.analyticsSvc = svc
	m.externalAnalytics = true
}

func (m *Miner) SetWebServer(server *web.Server) {
	m.webServer = server
}

// Run starts the miner and blocks until the context is cancelled.
// The caller is responsible for handling OS signals and cancelling the context.
func (m *Miner) Run(ctx context.Context) error {
	if err := m.initialize(); err != nil {
		return fmt.Errorf("initialization failed: %w", err)
	}

	if err := m.authenticate(); err != nil {
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
	if err := os.MkdirAll(m.dbBasePath, 0755); err != nil {
		return fmt.Errorf("failed to create database directory: %w", err)
	}

	db, err := database.Open(m.dbBasePath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}
	m.db = db

	return nil
}

func (m *Miner) authenticate() error {
	slog.Info("Authenticating with Twitch")

	m.auth = auth.NewTwitchAuth(m.config.Username, m.deviceID)

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

	if err := m.auth.Login(); err != nil {
		return err
	}

	m.client = api.NewTwitchClient(m.auth, m.deviceID)
	m.client.UpdateClientVersion()
	m.client.SetAuthErrorHandler(m.handleAuthError)

	userID, err := m.client.GetChannelID(m.config.Username)
	if err != nil {
		return fmt.Errorf("failed to get user ID: %w", err)
	}
	m.auth.SetUserID(userID)

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
	return m.streamers.LoadFromConfig(m.config.Streamers, progressCallback)
}

func (m *Miner) setupComponents(ctx context.Context) {
	streamers := m.streamers.All()

	m.wsPool = pubsub.NewWebSocketPool(m.client, m.auth.GetAuthToken(), streamers, m.config.RateLimits)
	m.wsPool.SetMessageHandler(m.handlePubSubMessage)
	m.wsPool.SetStatusHandler(m.handleStatusChange)
	m.wsPool.SetAuthErrorHandler(func(error) { m.handleAuthError() })

	if m.config.EnableAnalytics {
		if m.externalAnalytics && m.analyticsSvc != nil {
			if m.webServer != nil {
				m.webServer.AttachStreamers(streamers)
				m.webServer.SetSettingsProvider(m)
				m.webServer.SetSettingsUpdateCallback(m.ApplySettings)
				m.webServer.SetNextStreamCheckProvider(m)
			}
		} else {
			svc, err := analytics.NewService(m.db, m.dbBasePath)
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
			}
		}
	}

	streamerNames := m.streamers.Names()

	if m.config.Discord.Enabled {
		notifMgr, err := notifications.NewManager(&m.config.Discord, m.db, streamerNames)
		if err != nil {
			slog.Error("Failed to create notification manager", "error", err)
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
		if m.config.Debug.Enabled {
			m.webServer.SetDebugURL(fmt.Sprintf("http://localhost:%d/debug/snapshot", m.config.Debug.Port))
		}
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
	m.chatManager = chat.NewChatManager(m.config.Username, m.auth.GetAuthToken(), chatLogger, chatLogsEnabled, mentionHandler)

	var watchTimeStore *watcher.WatchTimeStore
	if m.db != nil {
		store, err := watcher.NewWatchTimeStore(m.db)
		if err != nil {
			slog.Error("Failed to create watch-time store, rotation fairness will not persist across restarts", "error", err)
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

	m.dropsTracker = drops.NewDropsTracker(
		m.client,
		streamers,
		m.config.RateLimits,
	)

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

		_ = m.wsPool.Submit(pubsub.NewTopic(pubsub.TopicVideoPlaybackByID, channelID))

		if s.Settings.FollowRaid {
			_ = m.wsPool.Submit(pubsub.NewTopic(pubsub.TopicRaid, channelID))
		}

		if s.Settings.MakePredictions {
			_ = m.wsPool.Submit(pubsub.NewTopic(pubsub.TopicPredictionsChannel, channelID))
		}

		if s.Settings.ClaimMoments {
			_ = m.wsPool.Submit(pubsub.NewTopic(pubsub.TopicCommunityMomentsChannel, channelID))
		}

		if s.Settings.CommunityGoals {
			_ = m.wsPool.Submit(pubsub.NewTopic(pubsub.TopicCommunityPointsChannel, channelID))
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
	}

	events.Record(events.TypeMinerStarted, "", "mining operations started")

	for _, s := range m.streamers.All() {
		m.client.CheckStreamerOnline(s)
		m.chatManager.ToggleChat(s)
	}

	m.watcher.Start(ctx)
	m.dropsTracker.Start(ctx)

	if m.webServer != nil {
		if !m.externalAnalytics {
			m.webServer.Start()
		}
		m.webServer.GetStatusBroadcaster().SetStatus(web.StatusRunning, "Mining active")
	}

	go m.streamCheckLoop(ctx)
	go m.healthWatchdogLoop(ctx)
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
						if m.analyticsSvc != nil {
							m.analyticsSvc.RecordPoints(s, reasonCode)

							if reasonCode == "WATCH_STREAK" {
								if earned, ok := pointGain["total_points"].(float64); ok {
									m.analyticsSvc.RecordAnnotation(s, "WATCH_STREAK", fmt.Sprintf("+%d - Watch Streak", int(earned)))
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

// handleAuthError is called the first time a Twitch API request or PubSub
// connection is rejected for an invalid/expired/revoked OAuth token. It logs
// an ERROR, notifies Discord (if enabled), and surfaces a dashboard banner
// telling the operator to reauthorize - fires once per process lifetime since
// this codebase has no mid-run token refresh, so the miner needs a restart
// and fresh login regardless of how many requests fail afterward.
func (m *Miner) handleAuthError() {
	m.authErrOnce.Do(func() {
		slog.Error("Twitch authorization expired or was revoked - reauthorization required")

		m.mu.Lock()
		m.reauthRequired = true
		m.mu.Unlock()

		if m.notifications != nil {
			m.notifications.NotifyReauthRequired("Restart the miner and complete the Twitch device login again.")
		}

		if m.webServer != nil {
			m.webServer.GetStatusBroadcaster().SetReauthRequired(true, "Twitch authorization expired or was revoked. Restart the miner and log in again.")
		}
	})
}

// healthWatchdogLoop periodically checks how long it has been since the API
// client or the PubSub pool last had confirmed successful contact with
// Twitch. If neither has responded within the configured threshold, it
// raises a "connection lost" signal (log + Discord + dashboard banner); once
// contact resumes, it logs and notifies recovery and clears the banner.
func (m *Miner) healthWatchdogLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	connectionLost := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.mu.RLock()
			threshold := time.Duration(m.config.RateLimits.ConnectionTimeoutMinutes) * time.Minute
			m.mu.RUnlock()

			now := time.Now()
			apiStale := m.client != nil && now.Sub(m.client.LastSuccessAt()) > threshold
			pubsubStale := m.wsPool != nil && !m.wsPool.LastActivity().IsZero() && now.Sub(m.wsPool.LastActivity()) > threshold

			lost := apiStale || pubsubStale

			if lost && !connectionLost {
				connectionLost = true
				detail := connectionLostDetail(apiStale, pubsubStale, threshold)
				slog.Error("Connection lost - harvesting paused", "apiStale", apiStale, "pubsubStale", pubsubStale, "thresholdMinutes", m.config.RateLimits.ConnectionTimeoutMinutes)

				m.mu.Lock()
				m.connectionLost = true
				m.connectionDetail = detail
				m.mu.Unlock()

				if m.notifications != nil {
					m.notifications.NotifyConnectionLost(detail)
				}
				if m.webServer != nil {
					m.webServer.GetStatusBroadcaster().SetConnectionLost(true, detail)
				}
			} else if !lost && connectionLost {
				connectionLost = false
				slog.Info("Connection restored - harvesting resumed")

				m.mu.Lock()
				m.connectionLost = false
				m.connectionDetail = ""
				m.mu.Unlock()

				if m.notifications != nil {
					m.notifications.NotifyConnectionRestored()
				}
				if m.webServer != nil {
					m.webServer.GetStatusBroadcaster().SetConnectionLost(false, "")
				}
			}
		}
	}
}

func connectionLostDetail(apiStale, pubsubStale bool, threshold time.Duration) string {
	minutes := int(threshold.Minutes())
	switch {
	case apiStale && pubsubStale:
		return fmt.Sprintf("No successful Twitch API or PubSub activity for over %d minutes.", minutes)
	case apiStale:
		return fmt.Sprintf("No successful Twitch API response for over %d minutes.", minutes)
	default:
		return fmt.Sprintf("No PubSub activity for over %d minutes.", minutes)
	}
}

func (m *Miner) handleStatusChange(username string, online bool) {
	if m.notifications == nil {
		return
	}

	if online {
		m.notifications.NotifyOnline(username)
	} else {
		m.notifications.NotifyOffline(username)
	}
}

func (m *Miner) stop() {
	m.chatManager.Close()
	m.wsPool.Close()
	m.watcher.Stop()
	m.dropsTracker.Stop()

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

	if m.db != nil {
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
	}

	added, removed := m.streamers.ApplySettings(m.config.Streamers, m.config.StreamerSettings)

	discordCfg := m.config.Discord
	notifMgr := m.notifications
	webServer := m.webServer
	wsPool := m.wsPool

	m.mu.Unlock()

	for _, streamer := range added {
		if wsPool != nil {
			_ = wsPool.Submit(pubsub.NewTopic(pubsub.TopicVideoPlaybackByID, streamer.ChannelID))

			if streamer.Settings.FollowRaid {
				_ = wsPool.Submit(pubsub.NewTopic(pubsub.TopicRaid, streamer.ChannelID))
			}
			if streamer.Settings.MakePredictions {
				_ = wsPool.Submit(pubsub.NewTopic(pubsub.TopicPredictionsChannel, streamer.ChannelID))
			}
			if streamer.Settings.ClaimMoments {
				_ = wsPool.Submit(pubsub.NewTopic(pubsub.TopicCommunityMomentsChannel, streamer.ChannelID))
			}
			if streamer.Settings.CommunityGoals {
				_ = wsPool.Submit(pubsub.NewTopic(pubsub.TopicCommunityPointsChannel, streamer.ChannelID))
			}
		}
	}

	for _, streamer := range removed {
		if wsPool != nil {
			wsPool.Unsubscribe(pubsub.NewTopic(pubsub.TopicVideoPlaybackByID, streamer.ChannelID))
			wsPool.Unsubscribe(pubsub.NewTopic(pubsub.TopicRaid, streamer.ChannelID))
			wsPool.Unsubscribe(pubsub.NewTopic(pubsub.TopicPredictionsChannel, streamer.ChannelID))
			wsPool.Unsubscribe(pubsub.NewTopic(pubsub.TopicCommunityMomentsChannel, streamer.ChannelID))
			wsPool.Unsubscribe(pubsub.NewTopic(pubsub.TopicCommunityPointsChannel, streamer.ChannelID))
		}
		if m.chatManager != nil {
			m.chatManager.Leave(streamer.Username)
		}
	}

	if len(added) > 0 || len(removed) > 0 {
		allStreamers := m.streamers.All()
		if wsPool != nil {
			wsPool.UpdateStreamers(allStreamers)
		}
		if webServer != nil {
			webServer.AttachStreamers(allStreamers)
		}
		m.triggerStreamCheck()
	}

	if notifMgr != nil {
		if err := notifMgr.UpdateDiscordConfig(&discordCfg); err != nil {
			slog.Error("Failed to update Discord config", "error", err)
		}
	} else if discordCfg.Enabled && !oldDiscordEnabled {
		newNotifMgr, err := notifications.NewManager(&discordCfg, m.db, m.streamers.Names())
		if err != nil {
			slog.Error("Failed to create notification manager", "error", err)
		} else {
			m.mu.Lock()
			m.notifications = newNotifMgr
			m.mu.Unlock()

			newNotifMgr.InitializePointsTracking(m.streamers.PointsMap())

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
