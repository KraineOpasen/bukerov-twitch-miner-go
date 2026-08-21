package web

import (
	"context"
	"crypto/subtle"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/debug"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/discovery"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/resources"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

//go:embed templates/*.html templates/partials/*.html templates/components/*.html
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
	FollowedChannels() ([]twitch.FollowedChannel, bool, error)
	TrackedUsernames() []string
	ImportStreamers(ctx context.Context, logins []string) (int, error)
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
	// ApplyHealthSettings validates, durably persists, and only then applies
	// new canary/watchdog settings. A non-nil error means persistence failed
	// and nothing was changed — the caller must not treat this as success.
	ApplyHealthSettings(config.HealthSettings) error
}

// DropProgressProvider exposes the drop-progress watchdog's published per-drop
// state so the Drops page can render HEALTHY/RECOVERING/STALLED badges.
// Satisfied by the miner.
type DropProgressProvider interface {
	DropProgress() health.ProgressSnapshot
}

// PolicyProvider exposes the campaign-policy engine's ranked decisions and the
// mode/per-drop-rule controls to the Drops page. Satisfied by the miner.
// A non-nil error from either mutator means nothing was changed — matching
// HealthProvider.ApplyHealthSettings and RewardsProvider.SetAutoRedeem below.
// In particular the miner returns settings.ErrShuttingDown once the generation
// backing this provider has been retired: the Server keeps a retired
// generation's providers until the next one re-registers, so a mutation can
// arrive here after its target stopped being the authoritative one.
type PolicyProvider interface {
	PolicySnapshot() (policy.Mode, []policy.Decision)
	CurrentCampaignPolicy() (string, map[string]config.DropRule)
	ApplyCampaignPolicy(mode string) error
	SetDropRule(rewardKey string, rule config.DropRule) error
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

	// dashboard is the immutable, environment-derived dashboard exposure/auth
	// configuration (DASHBOARD_* / MINER_DEV_PREDICTIONS), resolved once at the
	// cmd/miner bootstrap and injected via SetDashboardConfig before Start. The
	// web layer never reads the process environment itself: every auth/CSRF/dev
	// decision reads this snapshot (captured by value into the middleware at
	// handler build time). The zero value is "no override, no auth, loopback
	// bind" — exactly the behavior of an unset environment.
	dashboard runtimeconfig.Dashboard
	// debugSnapshot is the miner's in-process snapshot builder, wired only
	// when Debug.Enabled is true; nil keeps /api/debug/snapshot a 404.
	debugSnapshot func() debug.Snapshot
	// supportBundleSource is the same in-process snapshot builder, but wired
	// UNCONDITIONALLY (see internal/miner) since the redacted support bundle
	// is an always-available diagnostic tool, not a debug-mode feature. A
	// nil source is handled gracefully (empty operational sections), never
	// as a 404 - see handlers_support_bundle.go.
	supportBundleSource func() debug.Snapshot
	// supportBundleClock is a test seam overriding the clock
	// supportbundle.Build uses for generatedAt/the filename; nil in
	// production (Build then defaults to time.Now().UTC()).
	supportBundleClock func() time.Time
	// resourceSnapshot returns the resource sampler's latest in-memory snapshot
	// (CPU/Memory/Network/Disk) for the dashboard mini-widgets. Wired by the
	// miner once the sampler is running; nil keeps /api/resources a graceful
	// all-unavailable 200, never a 404.
	resourceSnapshot func() resources.Snapshot

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

	// lifecycleController is the narrow lifecycle-command seam (Ф4c): nil
	// (the default, e.g. every pre-Ф4c test/build) means no lifecycle
	// controller is wired at all — GET/POST /api/lifecycle then answer 503
	// and the dashboard panel renders nothing, exactly like every other
	// nil-provider path in this file. See handlers_lifecycle.go.
	lifecycleController LifecycleController
	// lifecycleUpdateState reports the best-effort auto-updater state (Ф4c
	// design D5) for the panel's "update available/failed/applied" line and
	// the GET JSON's updateState field. nil means "no updater information
	// wired" (updateState omitted).
	lifecycleUpdateState func() LifecycleUpdateState
	// processRestartRequester is wired by internal/app (Ф4c design D6) to
	// cancel the app's own run scope — the documented process-shutdown exit
	// path — when the degraded "Restart process" action is accepted. nil
	// means the action is unavailable (503 for API, hidden/disabled in the
	// panel).
	processRestartRequester func()

	// displayLoc is the time zone the dashboard renders absolute times in (set
	// from config LoggerSettings.TimeZone via SetDisplayLocation). nil falls back
	// to the server's local time. Guarded by mu.
	displayLoc *time.Location

	// statsCache memoises the per-streamer analytics-derived figures
	// (points-today and points-per-hour) so the 30s Overview poll doesn't hit
	// SQLite on every request; it is refreshed at most once per statsTTL.
	statsCache map[string]streamerStats
	statsAt    time.Time

	// settingsTxnMu serializes the whole settings mutation transaction —
	// snapshot read, partial-body merge, apply callback, success bookkeeping
	// — for POST /api/settings, POST /api/settings/reset and the Overview
	// card quick action (POST /api/streamer-action/{name}), which is the same
	// read-modify-write against the same pipeline. See
	// beginSettingsTxn (handlers_settings.go) for why the snapshot read has
	// to be inside it, and for the lock order. GET /api/settings deliberately
	// does NOT take it: a reader wants the current published state, and
	// serializing reads behind an in-flight apply would only make the
	// dashboard block on Discord reconnects for no added consistency.
	settingsTxnMu sync.Mutex
	// settingsTxnContended is a tests-only seam (nil in production),
	// mirroring internal/miner's applyCommitBarrier: beginSettingsTxn invokes
	// it SYNCHRONOUSLY at the moment a request finds the settings transaction
	// already held by another request — i.e. exactly when the serialization
	// below is doing its job. It exists so a concurrency test can observe
	// that a second POST was held OUT of the transaction window without any
	// wall-clock waiting. Set before serving; never written afterwards.
	settingsTxnContended func()

	// recentEvents is a tests-only seam over the process-wide event ring
	// (task S5-7): nil in production, where the /events journal reads
	// events.Recent directly — the journal has no other data source, no
	// persistence and no new provider. Set before serving in tests (like
	// settingsTxnContended); never written afterwards.
	recentEvents func(n int) []events.Event

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
	// sidebarSlot is a pure data conversion (no localization), so it's set
	// once here rather than in funcMapFor: now_watching.html uses it to route
	// each occupied WatchSlotView through the shared c12.slot component (Q3
	// MAJOR-2) without changing WatchSlotView's own shape.
	placeholder["sidebarSlot"] = sidebarSlotData

	pageList := []string{"overview.html", "streamer.html", "settings.html", "notifications.html", "drops.html", "drops_upcoming_page.html", "drops_claims.html", "drops_past_page.html", "statistics.html", "analytics_points.html", "analytics_roi.html", "health.html", "logs.html", "help.html", "help_glossary.html", "help_troubleshooting.html", "help_notifications_audio.html", "help_diagnostics_support.html", "events.html", "events_browser.html", "events_sound.html", "events_discord.html", "queue.html", "system_status.html", "system_diagnostics.html", "settings_streamers.html", "settings_rotation.html", "settings_drops.html", "settings_predictions.html", "settings_chat_raids.html", "settings_transport.html", "settings_analytics_logging.html", "settings_events_notifications.html", "settings_discord.html", "settings_system.html"}
	pages := make(map[string]map[string]*template.Template, len(pageList))
	for _, page := range pageList {
		base, err := template.New(page).Funcs(placeholder).ParseFS(templatesFS,
			"templates/base.html",
			"templates/"+page,
			"templates/partials/*.html",
			"templates/components/*.html",
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
	if base, err := template.New("partials").Funcs(placeholder).ParseFS(templatesFS, "templates/partials/*.html", "templates/components/*.html"); err != nil {
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

// SetSettingsProvider and SetSettingsUpdateCallback are guarded by s.mu (like
// every other setter on Server) so a request landing between the two calls
// during startup wiring can never observe settingsProvider != nil with
// onSettingsUpdate still nil — the handlers below read both together under
// one RLock and refuse (503) unless BOTH are set.
func (s *Server) SetSettingsProvider(provider settings.SettingsProvider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settingsProvider = provider
}

func (s *Server) SetSettingsUpdateCallback(callback settings.SettingsUpdateCallback) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
// Settings "find game ID" helper. Satisfied by *twitch.TwitchClient.
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

// SetDashboardConfig injects the resolved, immutable dashboard exposure/auth
// snapshot. It is called once during composition (by internal/app, or by the
// miner's fallback web build) before Start; the web layer reads no process
// environment of its own. It MUST be called before Start: Start's call to
// s.handler() captures this snapshot into the middleware chain
// (basicAuthMiddleware/csrfProtectMiddleware are built once, closing over
// the dashboard value passed to them at that moment), so reconfiguring the
// dashboard after Start has already built and started serving that handler
// is not supported and will not take effect on the running listener.
func (s *Server) SetDashboardConfig(d runtimeconfig.Dashboard) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dashboard = d
}

// dashboardConfig returns the injected dashboard snapshot (a value copy, safe
// to read without further synchronization).
func (s *Server) dashboardConfig() runtimeconfig.Dashboard {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dashboard
}

// AuthConfigured reports whether the dashboard requires Basic Auth (both
// credentials present in the injected snapshot). It lets the composition root
// and diagnostics observe whether auth is on WITHOUT exposing the credentials
// themselves. Do not call it while already holding s.mu (it takes the read
// lock; sync.RWMutex is not reentrant).
func (s *Server) AuthConfigured() bool {
	return s.dashboardConfig().AuthEnabled()
}

func basicAuthMiddleware(cfg runtimeconfig.Dashboard, next http.Handler) http.Handler {
	expectedUser, expectedPass := cfg.Username, cfg.Password.Reveal()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// logTrustedLANCIDRsAudit is Ф4d's corrective-pass startup audit trail: when
// the trusted-LAN allowlist is actually active (InsecureNoAuth AND at least
// one configured prefix), it is logged once at Info so an operator can see
// exactly what was accepted from the process's own logs — these are config
// values, not secrets, so there's nothing to redact. Any configured prefix
// broader than a private/loopback/link-local/ULA range (see
// nonPrivateTrustedLANPrefixes in security.go) additionally gets its own
// Warn, naming that prefix, since an accidentally-public allowlist entry is
// exactly the kind of mistake worth surfacing loudly rather than leaving to
// be discovered later. Called from Start after validateBindSecurity has
// already accepted the configuration (never for a value that fails
// startup); a no-op when the allowlist isn't active at all.
func logTrustedLANCIDRsAudit(dash runtimeconfig.Dashboard) {
	if !dash.InsecureNoAuth || len(dash.TrustedLANCIDRs) == 0 {
		return
	}
	prefixStrs := make([]string, len(dash.TrustedLANCIDRs))
	for i, p := range dash.TrustedLANCIDRs {
		prefixStrs[i] = p.String()
	}
	slog.Info("dashboard_trusted_lan_cidrs", "prefixes", strings.Join(prefixStrs, ","))
	for _, p := range nonPrivateTrustedLANPrefixes(dash) {
		slog.Warn("dashboard_trusted_lan_cidrs_broad",
			"prefix", p.String(),
			"hint", "this entry is broader than a private/loopback/link-local/ULA range")
	}
}

// Start resolves the effective bind address, enforces the fail-closed
// exposure rules (see security.go), and begins serving in the background.
// A non-loopback bind without credentials is a startup error, not a warning.
func (s *Server) Start() error {
	dash := s.dashboardConfig()
	host, source := resolveBindHost(dash, s.host)
	s.host = host
	if err := validateBindSecurity(dash, host); err != nil {
		return err
	}
	logTrustedLANCIDRsAudit(dash)

	handler := s.handler()

	addr := fmt.Sprintf("%s:%d", s.host, s.port)
	slog.Info("Web server bind resolved", "host", host, "source", source, "authEnabled", dash.AuthEnabled())
	if dash.AuthEnabled() {
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
	dash := s.dashboardConfig()
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
	if dash.DevPredictions {
		s.enableDevPredictions(mux)
	}

	// Custom channel-points rewards (per-streamer): list, redeem, auto-redeem
	// config. The "/api/streamer/" subtree is distinct from the exact
	// "/api/streamers" pattern above.
	mux.HandleFunc("/api/streamer/", s.handleAPIStreamerRewards)

	// Drops routes. /drops/current, /drops/upcoming, /drops/claims and
	// /drops/past are direct-render routes (task S5-4); /drops remains a
	// compatibility alias for Current through the exact same handler. Every
	// existing /api/drops*, discovery and policy endpoint below is unchanged.
	mux.HandleFunc("/drops", s.handleDropsPage)
	mux.HandleFunc("/drops/current", s.handleDropsPage)
	mux.HandleFunc("/drops/upcoming", s.handleDropsUpcomingPage)
	mux.HandleFunc("/drops/claims", s.handleDropsClaimsPage)
	mux.HandleFunc("/drops/past", s.handleDropsPastPage)
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

	// Redacted support bundle: a downloadable ZIP built entirely from an
	// in-memory, typed allowlist (internal/supportbundle). Unlike the debug
	// snapshot above, this route is wired unconditionally and additionally
	// enforces its own real-auth check server-side (see
	// requireRealDashboardAuth in handlers_support_bundle.go) - it must never
	// be reachable under DASHBOARD_INSECURE_NO_AUTH=true, even though that
	// mode leaves every other route unauthenticated.
	mux.HandleFunc(SupportBundlePath, s.handleSupportBundle)

	// Prediction ROI analytics: summary (filtered by streamer/strategy/period)
	// and a full-fidelity raw-bets export. Read-only; never places a bet.
	mux.HandleFunc("/api/predictions/roi", s.handleAPIPredictionsROI)
	mux.HandleFunc("/api/predictions/roi/export", s.handleAPIPredictionsROIExport)

	// Local resource metrics for the dashboard mini-widgets. Read-only; serves
	// only the sampler's last in-memory snapshot (no /proc read in the handler,
	// no external call, no state mutation). Inherits the shared middleware chain.
	mux.HandleFunc(ResourcesPath, s.handleAPIResources)

	// Status routes
	mux.HandleFunc("/api/status", s.handleAPIStatus)
	mux.HandleFunc("/api/miner-status", s.handleAPIMinerStatus)
	mux.HandleFunc("/api/miner-status/stream", s.handleAPIMinerStatusStream)
	mux.HandleFunc("/api/next-check", s.handleAPINextCheck)

	// Lifecycle routes (Ф4c): "/api/lifecycle" (exact) is GET-only (current
	// snapshot); "/api/lifecycle/" (subtree) is POST-only, action taken from
	// the trailing path segment (pause|resume|restart|stop|restart-process) —
	// same exact-vs-subtree split as "/api/streamers" vs "/api/streamer/"
	// above. See handlers_lifecycle.go for the response contract.
	mux.HandleFunc("/api/lifecycle", s.handleAPILifecycle)
	mux.HandleFunc("/api/lifecycle/", s.handleAPILifecycleAction)

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

	// S5-2 seven-section chrome: additive compatibility routes (handlers_chrome.go).
	// Direct-render routes reuse/extend the existing rendering pipelines;
	// every legacy route above keeps rendering directly and is never
	// redirected. By task S5-9 every route in the design's 30-route page
	// matrix is registered somewhere in this function — nothing falls
	// through to the "/" catch-all as an honest 404 anymore.
	mux.HandleFunc("/overview", s.handleOverviewPage)
	// S5-3: /overview/queue (handlers_queue.go) — the Overview section's
	// second page, no longer deferred. Direct-render, GET/HEAD, no new API
	// endpoint or transport.
	mux.HandleFunc("/overview/queue", s.handleOverviewQueuePage)
	mux.HandleFunc("/events", s.handleEventsPage)
	mux.HandleFunc("/help/getting-started", s.handleHelpGettingStarted)

	// S5-9 Help pages (routes 27-30, handlers_help.go): direct-render routes
	// completing the Help group — /help/getting-started above was the last
	// of the five to stay a 404-free direct route already; these four were
	// the last deferred routes in the entire 30-route matrix. Static,
	// backend-free reading-density pages: no htmx, no polling, no new API
	// endpoint. /help/glossary is wired to the same canonical dictionaries
	// (reasonCodeKeys, rosterStatusKeys, eventTypeKeys, eventJournalGroups)
	// used elsewhere in this package — see handlers_help.go.
	mux.HandleFunc("/help/glossary", s.handleHelpGlossaryPage)
	mux.HandleFunc("/help/troubleshooting", s.handleHelpTroubleshootingPage)
	mux.HandleFunc("/help/notifications-audio", s.handleHelpNotificationsAudioPage)
	mux.HandleFunc("/help/diagnostics-support", s.handleHelpDiagnosticsSupportPage)

	// S5-7 Events pages (routes 9-12, handlers_events.go): /events is the
	// real session-scoped journal (superseding the S5-2 minimal landing
	// through the same route registration above), and the three former
	// deferred 404s become direct-render status pages. Registered before
	// the redirect loop below, the same ordering S5-4/S5-5/S5-6 used; no
	// entry is ever added to (or removed from) compatibilityRedirects.
	mux.HandleFunc("/events/browser", s.handleEventsBrowserPage)
	mux.HandleFunc("/events/sound", s.handleEventsSoundPage)
	mux.HandleFunc("/events/discord", s.handleEventsDiscordPage)

	// S5-5 System pages: direct-render routes (handlers_system.go) replacing
	// the three former /system/* compatibility redirects to /health and
	// /logs. Registered before the redirect loop below so these three paths
	// are never shadowed by (and no longer appear in) compatibilityRedirects.
	// Legacy /health and /logs keep rendering directly, unchanged.
	mux.HandleFunc("/system/status", s.handleSystemStatusPage)
	mux.HandleFunc("/system/diagnostics", s.handleSystemDiagnosticsPage)
	mux.HandleFunc("/system/logs", s.handleSystemLogsPage)

	// S5-6 Settings category routes (13-22): direct-render routes
	// (handlers_settings_categories.go) replacing the ten former
	// /settings/* compatibility redirects — registered before the redirect
	// loop below (same ordering S5-4/S5-5 used) so these ten paths are never
	// shadowed by, and no longer appear in, compatibilityRedirects. Legacy
	// /settings (the mega-form) keeps rendering directly, unchanged.
	mux.HandleFunc("/settings/streamers", s.handleSettingsStreamersPage)
	mux.HandleFunc("/settings/rotation", s.handleSettingsRotationPage)
	mux.HandleFunc("/settings/drops", s.handleSettingsDropsPage)
	mux.HandleFunc("/settings/predictions", s.handleSettingsPredictionsPage)
	mux.HandleFunc("/settings/chat-raids", s.handleSettingsChatRaidsPage)
	mux.HandleFunc("/settings/transport", s.handleSettingsTransportPage)
	mux.HandleFunc("/settings/analytics-logging", s.handleSettingsAnalyticsLoggingPage)
	mux.HandleFunc("/settings/events-notifications", s.handleSettingsEventsNotificationsPage)
	mux.HandleFunc("/settings/discord", s.handleSettingsDiscordPage)
	mux.HandleFunc("/settings/system", s.handleSettingsSystemPage)

	// S5-8 Analytics pages (routes 7-8): direct-render routes
	// (handlers_analytics_pages.go) replacing the two former /analytics/*
	// compatibility redirects to /statistics — registered before the redirect
	// loop below (the same ordering S5-4/S5-5/S5-6 used) so these two paths
	// are never shadowed by, and no longer appear in, compatibilityRedirects.
	// Legacy /statistics keeps rendering directly, unchanged. /analytics
	// itself is deliberately not registered: it is not a page, so it keeps
	// falling through to the "/" catch-all and 404s honestly.
	mux.HandleFunc("/analytics/points", s.handleAnalyticsPointsPage)
	mux.HandleFunc("/analytics/roi", s.handleAnalyticsROIPage)

	for route, target := range compatibilityRedirects {
		mux.HandleFunc(route, redirectCompat(target))
	}

	// Middleware chain (outermost first): security headers on every
	// response, then Basic Auth when configured, then the same-origin check
	// guarding all state-changing requests.
	h := http.Handler(csrfProtectMiddleware(dash, mux))
	if dash.AuthEnabled() {
		h = basicAuthMiddleware(dash, h)
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
