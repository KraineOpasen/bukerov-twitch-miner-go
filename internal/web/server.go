package web

import (
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/debug"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/discovery"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

//go:embed templates/*.html templates/partials/*.html
var templatesFS embed.FS

//go:embed static/*
var staticFS embed.FS

type NextStreamCheckProvider interface {
	GetNextStreamCheck() time.Time
}

// CampaignsProvider exposes the currently tracked active drop campaigns so the
// Drops page can render them, the last sync's bookkeeping for the Status Center,
// and the manual "Sync Drops now" action. It's satisfied by the drops tracker.
type CampaignsProvider interface {
	Campaigns() []*models.Campaign
	SyncStatus() drops.SyncStatus
	RequestManualSync() drops.ManualSyncResult
}

// DropCatalogProvider exposes the Drops-page catalog tabs: upcoming campaigns
// (display-only, not-yet-started) and the durable "past" catalog of expired
// campaigns. Satisfied by the miner.
type DropCatalogProvider interface {
	UpcomingCampaigns() []*models.Campaign
	// RelevantUpcomingCampaigns returns the not-yet-started campaigns filtered to
	// the operator's game filter (display-only relevance) — foreign upcoming
	// campaigns are hidden from the tab.
	RelevantUpcomingCampaigns() []*models.Campaign
	// CampaignSyncStatus reports the last full-sync bookkeeping so the Upcoming
	// tab can render honest never-synced / empty / stale states.
	CampaignSyncStatus() drops.SyncStatus
	PastCampaigns() ([]drops.CatalogRecord, error)
}

// FollowedProvider backs the Settings-page "import followed channels" picker:
// list the authenticated user's followed channels, know which are already
// tracked, and add selected ones to the tracked streamer list. Satisfied by the
// miner.
type FollowedProvider interface {
	FollowedChannels() ([]api.FollowedChannel, bool, error)
	TrackedUsernames() []string
	ImportStreamers(logins []string) (int, error)
}

// DiscoveryProvider exposes the directory-discovery subsystem's state so the
// Drops page can render the discovered-channels pool. It's satisfied by the
// discovery manager.
type DiscoveryProvider interface {
	State() discovery.State
}

// HealthProvider exposes the Health Center's aggregated signals and the canary
// controls (run-now, settings) to the dashboard and debug endpoint. Satisfied
// by the miner.
type HealthProvider interface {
	HealthSnapshot() health.Snapshot
	RunCanaryNow()
	CurrentHealthSettings() config.HealthSettings
	ApplyHealthSettings(config.HealthSettings)
}

// DropProgressProvider exposes the drop-progress watchdog's published per-drop
// state so the Drops page can render HEALTHY/RECOVERING/STALLED badges.
// Satisfied by the miner.
type DropProgressProvider interface {
	DropProgress() health.ProgressSnapshot
}

// PolicyProvider exposes the campaign-policy engine's ranked decisions and the
// mode/per-drop-rule controls to the Drops page. Satisfied by the miner.
type PolicyProvider interface {
	PolicySnapshot() (policy.Mode, []policy.Decision)
	CurrentCampaignPolicy() (string, map[string]config.DropRule)
	ApplyCampaignPolicy(mode string)
	SetDropRule(rewardKey string, rule config.DropRule)
}

// RewardsProvider exposes custom channel-points reward listing/redemption and
// per-streamer auto-redeem configuration to the dashboard. It's satisfied by
// the miner, which owns the API client and streamer state.
type RewardsProvider interface {
	ListCustomRewards(username string) ([]*models.CustomReward, error)
	RedeemCustomReward(username, rewardID, textInput string) error
	GetAutoRedeem(username string) config.AutoRedeemConfig
	SetAutoRedeem(username string, cfg config.AutoRedeemConfig) error
}

// OverviewProvider supplies the two pieces of live Overview state the web
// server can't read from the streamer objects directly: the watch-slot
// selection (from the watcher) and the tracked predictions (from the pubsub
// pool). Both are read-only in-memory snapshots - no new Twitch calls, no
// extra polling. Satisfied by the miner.
type OverviewProvider interface {
	WatchSlots() WatchSlotsView
	LivePredictions() []LivePrediction
}

// PredictionControlProvider exposes safe, server-validated manual control over
// live prediction rounds: placing a manual bet on a specific outcome, and
// toggling per-round auto-bet suppression. Both are keyed on the stable round
// (event) id and never touch global/persisted settings. Satisfied by the miner,
// which delegates to the pubsub pool that owns the round state.
type PredictionControlProvider interface {
	PlaceManualBet(eventID, outcomeID string, amount int) (string, error)
	SetAutoBetSkip(eventID string, skip bool) error
}

type Server struct {
	host           string
	port           int
	refresh        int
	daysAgo        int
	username       string
	basePath       string
	streamers      []*models.Streamer
	discordEnabled bool
	debugURL       string
	// debugSnapshot is the miner's in-process snapshot builder, wired only
	// when Debug.Enabled is true; nil keeps /api/debug/snapshot a 404.
	debugSnapshot func() debug.Snapshot

	analytics *analytics.Service
	server    *http.Server
	i18n      *i18n.Localizer
	// templates maps a page name to its per-language parsed template (page ->
	// lang -> template); partials maps a language to the standalone partial set
	// used by the htmx endpoints. Both are pre-cloned per language at start-up
	// so requests never clone or mutate a shared template.
	templates               map[string]map[string]*template.Template
	partials                map[string]*template.Template
	settingsProvider        settings.SettingsProvider
	onSettingsUpdate        settings.SettingsUpdateCallback
	notificationManager     *notifications.Manager
	nextStreamCheckProvider NextStreamCheckProvider
	campaignsProvider       CampaignsProvider
	dropCatalogProvider     DropCatalogProvider
	followedProvider        FollowedProvider
	discoveryProvider       DiscoveryProvider
	healthProvider          HealthProvider
	dropProgressProvider    DropProgressProvider
	policyProvider          PolicyProvider
	rewardsProvider         RewardsProvider
	overviewProvider        OverviewProvider
	predictionControl       PredictionControlProvider
	gameIDResolver          GameIDResolver
	status                  *StatusBroadcaster
	ready                   bool

	// displayLoc is the time zone the dashboard renders absolute times in (set
	// from config LoggerSettings.TimeZone via SetDisplayLocation). nil falls back
	// to the server's local time. Guarded by mu.
	displayLoc *time.Location

	// statsCache memoises the per-streamer analytics-derived figures
	// (points-today and points-per-hour) so the 30s Overview poll doesn't hit
	// SQLite on every request; it is refreshed at most once per statsTTL.
	statsCache map[string]streamerStats
	statsAt    time.Time

	// healthFormMu serializes the read-modify-write in handleAPIHealthSettings:
	// the canary and watchdog forms each patch their own section over the
	// current settings, and two concurrent section saves without this lock
	// could write one section's stale copy over the other's fresh save.
	healthFormMu sync.Mutex

	mu sync.RWMutex
}

// streamerStats holds the analytics-derived numbers cached per streamer.
type streamerStats struct {
	pointsToday   int
	pointsPerHour int
	hasRate       bool
}

func NewServer(analyticsSettings config.AnalyticsSettings, username string, basePath string, analyticsSvc *analytics.Service, streamers []*models.Streamer) *Server {
	loc := mustLocalizer()
	pages, partials := loadTemplates(loc)

	return &Server{
		host:      analyticsSettings.Host,
		port:      analyticsSettings.Port,
		refresh:   analyticsSettings.Refresh,
		daysAgo:   analyticsSettings.DaysAgo,
		username:  username,
		basePath:  basePath,
		streamers: streamers,
		analytics: analyticsSvc,
		i18n:      loc,
		templates: pages,
		partials:  partials,
		status:    NewStatusBroadcaster(),
		ready:     len(streamers) > 0,
	}
}

func NewServerEarly(analyticsSettings config.AnalyticsSettings, username string, basePath string, analyticsSvc *analytics.Service) *Server {
	loc := mustLocalizer()
	pages, partials := loadTemplates(loc)

	return &Server{
		host:      analyticsSettings.Host,
		port:      analyticsSettings.Port,
		refresh:   analyticsSettings.Refresh,
		daysAgo:   analyticsSettings.DaysAgo,
		username:  username,
		basePath:  basePath,
		streamers: nil,
		analytics: analyticsSvc,
		i18n:      loc,
		templates: pages,
		partials:  partials,
		status:    NewStatusBroadcaster(),
		ready:     false,
	}
}

// loadTemplates parses each page (base + page + partials) and the standalone
// partial set once, then clones a language-bound copy per supported language.
// The localization funcs (t/lang/jsMessages) are defined as placeholders at
// parse time and overridden with real, language-specific implementations on each
// clone, so every request executes an already-escaped, immutable template.
func loadTemplates(loc *i18n.Localizer) (map[string]map[string]*template.Template, map[string]*template.Template) {
	langs := i18n.SupportedLangs()
	placeholder := placeholderFuncMap()

	pageList := []string{"overview.html", "dashboard.html", "streamer.html", "settings.html", "notifications.html", "drops.html", "statistics.html", "health.html", "logs.html"}
	pages := make(map[string]map[string]*template.Template, len(pageList))
	for _, page := range pageList {
		base, err := template.New(page).Funcs(placeholder).ParseFS(templatesFS,
			"templates/base.html",
			"templates/"+page,
			"templates/partials/*.html",
		)
		if err != nil {
			slog.Error("Failed to parse template", "page", page, "error", err)
			continue
		}
		perLang := make(map[string]*template.Template, len(langs))
		for _, lang := range langs {
			clone, err := base.Clone()
			if err != nil {
				slog.Error("Failed to clone template", "page", page, "lang", lang, "error", err)
				continue
			}
			clone.Funcs(funcMapFor(loc, lang))
			perLang[lang] = clone
		}
		pages[page] = perLang
	}

	partials := make(map[string]*template.Template, len(langs))
	if base, err := template.New("partials").Funcs(placeholder).ParseFS(templatesFS, "templates/partials/*.html"); err != nil {
		slog.Error("Failed to parse partials", "error", err)
	} else {
		for _, lang := range langs {
			clone, err := base.Clone()
			if err != nil {
				slog.Error("Failed to clone partials", "lang", lang, "error", err)
				continue
			}
			clone.Funcs(funcMapFor(loc, lang))
			partials[lang] = clone
		}
	}

	return pages, partials
}

func (s *Server) AttachStreamers(streamers []*models.Streamer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamers = streamers
	s.ready = true
}

func (s *Server) GetStatusBroadcaster() *StatusBroadcaster {
	return s.status
}

func (s *Server) GetAnalyticsService() *analytics.Service {
	return s.analytics
}

func (s *Server) GetBasePath() string {
	return s.basePath
}

func (s *Server) SetSettingsProvider(provider settings.SettingsProvider) {
	s.settingsProvider = provider
}

func (s *Server) SetSettingsUpdateCallback(callback settings.SettingsUpdateCallback) {
	s.onSettingsUpdate = callback
}

func (s *Server) SetNotificationManager(mgr *notifications.Manager) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notificationManager = mgr
}

func (s *Server) SetNextStreamCheckProvider(provider NextStreamCheckProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextStreamCheckProvider = provider
}

func (s *Server) SetCampaignsProvider(provider CampaignsProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.campaignsProvider = provider
}

// SetGameIDResolver wires the read-only Twitch game-ID lookup backing the
// Settings "find game ID" helper. Satisfied by *api.TwitchClient.
func (s *Server) SetGameIDResolver(resolver GameIDResolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gameIDResolver = resolver
}

func (s *Server) SetDropCatalogProvider(provider DropCatalogProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropCatalogProvider = provider
}

// SetDisplayLocation sets the time zone the dashboard renders absolute times in
// (the config's LoggerSettings.TimeZone; production uses Asia/Jerusalem). Safe
// to call at wiring time before the server serves requests.
func (s *Server) SetDisplayLocation(loc *time.Location) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.displayLoc = loc
}

// displayLocation returns the configured display time zone, falling back to the
// server's local time when none was set.
func (s *Server) displayLocation() *time.Location {
	s.mu.RLock()
	loc := s.displayLoc
	s.mu.RUnlock()
	if loc == nil {
		return time.Local
	}
	return loc
}

func (s *Server) SetFollowedProvider(provider FollowedProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.followedProvider = provider
}

func (s *Server) SetDiscoveryProvider(provider DiscoveryProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discoveryProvider = provider
}

func (s *Server) SetHealthProvider(provider HealthProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.healthProvider = provider
}

func (s *Server) SetDropProgressProvider(provider DropProgressProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropProgressProvider = provider
}

func (s *Server) SetPolicyProvider(provider PolicyProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policyProvider = provider
}

func (s *Server) SetRewardsProvider(provider RewardsProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rewardsProvider = provider
}

func (s *Server) SetOverviewProvider(provider OverviewProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overviewProvider = provider
}

func (s *Server) SetPredictionControlProvider(provider PredictionControlProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.predictionControl = provider
}

func (s *Server) SetDiscordEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discordEnabled = enabled
}

// SetDebugURL publishes the localhost debug-snapshot URL so pages can link
// to it from the nav bar; empty (the default) hides the link.
func (s *Server) SetDebugURL(url string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.debugURL = url
}

func getAuthCredentials() (username, password string) {
	return os.Getenv("DASHBOARD_USERNAME"), os.Getenv("DASHBOARD_PASSWORD")
}

func authEnabled() bool {
	username, password := getAuthCredentials()
	return username != "" && password != ""
}

func basicAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expectedUser, expectedPass := getAuthCredentials()
		if expectedUser == "" || expectedPass == "" {
			next.ServeHTTP(w, r)
			return
		}

		user, pass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(user), []byte(expectedUser)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(pass), []byte(expectedPass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="Twitch Miner Dashboard"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Start resolves the effective bind address, enforces the fail-closed
// exposure rules (see security.go), and begins serving in the background.
// A non-loopback bind without credentials is a startup error, not a warning.
func (s *Server) Start() error {
	host, source := resolveBindHost(s.host)
	s.host = host
	if err := validateBindSecurity(host); err != nil {
		return err
	}

	handler := s.handler()

	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	slog.Info("Web server bind resolved", "host", host, "source", source, "authEnabled", authEnabled())
	if authEnabled() {
		slog.Info("Web server authentication enabled")
	}

	// No ReadTimeout/WriteTimeout: /api/miner-status/stream is a long-lived
	// SSE response that a blanket connection deadline would kill.
	// ReadHeaderTimeout and IdleTimeout still shut down slow-header and idle
	// connections (slowloris protection).
	s.server = &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	slog.Info("Web server starting", "url", "http://"+addr+"/")

	go func() {
		if err := s.server.ListenAndServe(); err != http.ErrServerClosed {
			slog.Error("Web server error", "error", err)
		}
	}()
	return nil
}

// handler builds the full route mux (and wraps it in basic-auth middleware
// when configured). Split out from Start so it can be exercised directly in
// tests and tooling.
func (s *Server) handler() http.Handler {
	mux := http.NewServeMux()

	// Static files
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		slog.Error("Failed to create static filesystem", "error", err)
	} else {
		mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

		// Browsers probe /favicon.ico at the site root regardless of the
		// <link rel="icon"> tags, so serve it there too to avoid a 404.
		mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, staticSub, "images/favicon.ico")
		})
	}

	// Dashboard routes
	mux.HandleFunc("/", s.handleDashboard)
	mux.HandleFunc("/streamer/", s.handleStreamerPage)
	mux.HandleFunc("/api/streamers", s.handleAPIStreamers)

	// Overview (redesigned dashboard) routes. /api/overview returns the live
	// content partial swapped by htmx; /api/overview/events returns the recent
	// events for one streamer (card drawer); the quick-action endpoint reuses
	// the existing settings pipeline.
	mux.HandleFunc("/api/overview", s.handleAPIOverview)
	mux.HandleFunc("/api/now-watching", s.handleAPINowWatching)
	mux.HandleFunc("/api/overview/events/", s.handleAPIOverviewEvents)
	mux.HandleFunc("/api/streamer-action/", s.handleAPIStreamerQuickAction)

	// Manual prediction control: place a manual bet, or toggle per-round
	// auto-bet suppression. Same auth/response/error/logging conventions as the
	// other dashboard JSON endpoints.
	mux.HandleFunc("/api/prediction/bet", s.handleAPIPredictionBet)
	mux.HandleFunc("/api/prediction/skip", s.handleAPIPredictionSkip)

	// Dev-only prediction simulator (fixtures + a fake Twitch placer), disabled
	// by default and only wired when MINER_DEV_PREDICTIONS is set, so simulated
	// rounds can never leak into a real run.
	if devPredictionsEnabled() {
		s.enableDevPredictions(mux)
	}

	// Custom channel-points rewards (per-streamer): list, redeem, auto-redeem
	// config. The "/api/streamer/" subtree is distinct from the exact
	// "/api/streamers" pattern above.
	mux.HandleFunc("/api/streamer/", s.handleAPIStreamerRewards)

	// Drops routes
	mux.HandleFunc("/drops", s.handleDropsPage)
	mux.HandleFunc("/api/drops", s.handleAPIDrops)
	mux.HandleFunc("/api/drops/sync", s.handleAPIDropsSync)
	mux.HandleFunc("/api/drops/upcoming", s.handleAPIDropsUpcoming)
	mux.HandleFunc("/api/drops/past", s.handleAPIDropsPast)
	mux.HandleFunc("/api/discovery", s.handleAPIDiscovery)
	mux.HandleFunc("/api/policy/mode", s.handleAPIPolicyMode)
	mux.HandleFunc("/api/policy/drop-rule", s.handleAPIPolicyDropRule)

	// Statistics routes: the dedicated points-history page, its JSON data
	// endpoint (range-filtered, downsampled for the chart), and a full-fidelity
	// export endpoint for external tools.
	mux.HandleFunc("/statistics", s.handleStatisticsPage)
	mux.HandleFunc("/api/points-history", s.handleAPIPointsHistory)
	mux.HandleFunc("/api/points-history/export", s.handleAPIPointsHistoryExport)

	// Logs: a standalone log viewer (full page + htmx-refreshed line partial).
	mux.HandleFunc("/logs", s.handleLogsPage)
	mux.HandleFunc("/api/logs", s.handleAPILogs)

	// Debug snapshot, proxied in-process from the miner's snapshot builder so
	// the Logs-page button works from remote browsers (the 127.0.0.1:5757
	// debug server stays localhost-only). 404s until the miner wires the
	// provider, which only happens when Debug.Enabled is true.
	mux.HandleFunc(DebugSnapshotPath, s.handleAPIDebugSnapshot)

	// Prediction ROI analytics: summary (filtered by streamer/strategy/period)
	// and a full-fidelity raw-bets export. Read-only; never places a bet.
	mux.HandleFunc("/api/predictions/roi", s.handleAPIPredictionsROI)
	mux.HandleFunc("/api/predictions/roi/export", s.handleAPIPredictionsROIExport)

	// Status routes
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/miner-status", s.handleAPIMinerStatus)
	mux.HandleFunc("/api/miner-status/stream", s.handleAPIMinerStatusStream)
	mux.HandleFunc("/api/next-check", s.handleAPINextCheck)

	// Settings routes
	mux.HandleFunc("/settings", s.handleSettingsPage)
	mux.HandleFunc("/api/settings", s.handleAPISettings)
	mux.HandleFunc("/api/settings/reset", s.handleAPISettingsReset)
	mux.HandleFunc("/api/settings/resolve-game-id", s.handleAPIResolveGameID)
	mux.HandleFunc("/api/followed", s.handleAPIFollowed)
	mux.HandleFunc("/api/followed/import", s.handleAPIFollowedImport)

	mux.HandleFunc("/health", s.handleHealthPage)
	mux.HandleFunc("/api/health", s.handleAPIHealth)
	mux.HandleFunc("/api/health/canary/run", s.handleAPIHealthCanaryRun)
	mux.HandleFunc("/api/health/settings", s.handleAPIHealthSettings)

	// Analytics/data routes
	mux.HandleFunc("/streamers", s.handleStreamers)
	mux.HandleFunc("/json/", s.handleJSON)
	mux.HandleFunc("/json_all", s.handleJSONAll)
	mux.HandleFunc("/api/chat/", s.handleAPIChatMessages)

	// Notifications routes
	mux.HandleFunc("/notifications", s.handleNotificationsPage)
	mux.HandleFunc("/api/notifications/config", s.handleAPINotificationsConfig)
	mux.HandleFunc("/api/notifications/channels", s.handleAPINotificationsChannels)
	mux.HandleFunc("/api/notifications/points", s.handleAPINotificationsPoints)
	mux.HandleFunc("/api/notifications/points/", s.handleAPINotificationsPointsDelete)
	mux.HandleFunc("/api/notifications/test", s.handleAPINotificationsTest)
	mux.HandleFunc("/api/test-notification", s.handleAPITestNotification)

	// Language switch: state-changing (sets the language cookie), so it is
	// POST-only and, being on this mux, inherits csrfProtectMiddleware below
	// like every other mutating endpoint.
	mux.HandleFunc("/api/lang", s.handleAPILang)

	// Middleware chain (outermost first): security headers on every
	// response, then Basic Auth when configured, then the same-origin check
	// guarding all state-changing requests.
	h := http.Handler(csrfProtectMiddleware(mux))
	if authEnabled() {
		h = basicAuthMiddleware(h)
	}
	return securityHeadersMiddleware(h)
}

func (s *Server) Stop() {
	if s.server != nil {
		_ = s.server.Close()
	}
}

func (s *Server) renderPage(w http.ResponseWriter, r *http.Request, page string, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	perLang, ok := s.templates[page]
	if !ok {
		slog.Error("Template not found", "page", page)
		writeInternalError(w, "Template not found")
		return
	}

	lang := s.langFromRequest(r)
	tmpl := perLang[lang]
	if tmpl == nil {
		tmpl = perLang[i18n.DefaultLang]
	}
	if tmpl == nil {
		slog.Error("Template language variant not found", "page", page, "lang", lang)
		writeInternalError(w, "Template not found")
		return
	}

	if err := tmpl.ExecuteTemplate(w, "base.html", data); err != nil {
		slog.Error("Failed to render page", "page", page, "error", err)
		writeInternalError(w, "Failed to render page")
	}
}

// renderPartial executes a named partial in the request's language, for the htmx
// endpoints that swap fragments without a full page render.
func (s *Server) renderPartial(w http.ResponseWriter, r *http.Request, name string, data interface{}) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	lang := s.langFromRequest(r)
	tmpl := s.partials[lang]
	if tmpl == nil {
		tmpl = s.partials[i18n.DefaultLang]
	}
	if tmpl == nil {
		slog.Error("Partials unavailable", "partial", name)
		writeInternalError(w, "Failed to render partial")
		return
	}

	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("Failed to render partial", "partial", name, "error", err)
		writeInternalError(w, "Failed to render partial")
	}
}
