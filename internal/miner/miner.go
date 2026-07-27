package miner

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
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
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/journal"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/logger"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/resources"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/updater"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/util"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

// ErrShuttingDown is miner's name for settings.ErrShuttingDown (defined there
// so internal/web can recognize it via errors.Is without an import cycle —
// see that package's doc comment). Kept as a miner-local alias so production
// and test code in this package can refer to it as miner.ErrShuttingDown.
var ErrShuttingDown = settings.ErrShuttingDown

// SRAP (Streamer Removal Admission Protocol, M1) budget vars — package-level
// so a test can shrink them to exercise a deterministic timeout without a
// real multi-second wait. Each bounds a WithTimeout(WithoutCancel(reqCtx), …)
// derived context (see applySettingsWithRemovals/applySettingsWithRename):
// WithoutCancel so a hard-closed HTTP connection (web.Server.Stop is
// http.Server.Close, not a graceful Shutdown) cannot abort the bounded
// commit sequence: cancellation is only ever OBSERVED at the explicit
// pre-commit gates (the entry check and the last check after admission) —
// past that final gate the sequence runs to completion even if the request
// context is cancelled while config.SaveConfig is still writing. WithTimeout
// so a wedged SQLite/coordinatorMu fails bounded rather than wedging every
// future settings apply forever.
var (
	// admissionBudget bounds AdmitRemovals/AbortAdmission (the PREPARE phase,
	// before any visible mutation).
	admissionBudget = 5 * time.Second
	// purgeBudget bounds CommitRemoval (the COMPLETE phase, after the commit
	// point — its failure is logged and durably retried, never fails the API).
	purgeBudget = 15 * time.Second
	// startupReconcileBudget bounds the one-time startup pass
	// (ArbitratePrepared then Reconcile), run on context.Background() rather
	// than the run context so a SIGTERM arriving mid-pass cannot abort it
	// halfway and leave some rows resolved and others not.
	startupReconcileBudget = 60 * time.Second
)

type Miner struct {
	config     *config.Config
	configPath string
	// dashboard is the immutable, environment-derived dashboard exposure/auth
	// snapshot (resolved once at the cmd/miner bootstrap and injected via
	// SetDashboardConfig). It is used only by the fallback web-server build in
	// start() — the library path where App did not already inject a web server;
	// in the App-driven process App injects the same snapshot into the web
	// server it builds. The zero value means "no override, no auth" (loopback).
	dashboard runtimeconfig.Dashboard
	auth      *auth.TwitchAuth
	client    *twitch.TwitchClient

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
	// notificationsRepo is a standalone notification repository over the shared
	// DB, created whenever the DB exists regardless of whether a notifications
	// Manager was constructed successfully. Since M4 a Manager is built at
	// startup for every DB (Discord and every push provider may both be off —
	// see initNotificationManager), so this repository's purge/fence is only
	// ever preferred over the Manager's own in the rare case NewManager itself
	// failed; it still ensures a removed streamer's point_rules / config-list
	// rows are purged (and renamed) even then — those rows persist in the file
	// independently of the Manager. When a Manager IS live the coordinator
	// prefers it (so the fence also covers its AddPointRule); both instances
	// share the same tables.
	notificationsRepo *notifications.Repository
	debugServer       *debug.Server
	// watchTimeStore is the persisted rotation-fairness store; kept here (not
	// only inside the watcher) so the streamer-deletion coordinator can purge a
	// removed streamer's watch-time rows in the same atomic transaction.
	watchTimeStore *watcher.WatchTimeStore
	// streamerLifecycle purges a removed streamer's PERSISTED bot-owned state
	// (analytics history, notification rules + config lists, watch-time rows) in
	// one atomic transaction and arms/clears the resurrection fence. Built once
	// the persisted stores exist; nil disables persisted purge (fields still
	// tear down runtime state).
	streamerLifecycle *streamerlifecycle.Coordinator
	// resourceSampler feeds the dashboard's resource mini-widgets with local
	// process/container CPU/Memory/Network/Disk metrics. Started in startMining
	// and stopped with the run context; nil when there is no web server.
	resourceSampler *resources.Sampler

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
	// externalWebServer is true when the web server was injected via
	// SetWebServer (the cmd/app composition root owns its Stop). When the miner
	// builds its own web server (the library-use fallback in setupComponents),
	// this stays false and stop() closes it — mirroring ownsDB/externalAnalytics
	// so an injected server is never double-stopped by both the miner and its
	// owner.
	externalWebServer bool
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

	// healthJournal is the bounded diagnostic journal of connection-health
	// transitions (BKM-014 observability). It observes the outputs of the PR-#112
	// health state machine — it never reclassifies and never affects
	// notification/dashboard decisions. Its own lock makes it safe for the debug
	// reader; recordHealthTransition appends only from the single watchdog
	// goroutine. healthJournalSuppressed counts consecutive deduped identical
	// ticks since the last recorded transition and is touched ONLY on that
	// watchdog goroutine (no lock needed).
	healthJournal           *journal.Journal[journal.HealthEvent]
	healthJournalSuppressed int

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
	// autoRedeemGen is the per-streamer (lowercase login) generation of the
	// auto-redeem runtime window (M2, I5). Guarded by mu. A generation
	// identifies one continuous budget window: it is bumped when the window
	// is genuinely invalidated (a successful SetAutoRedeem; a committed
	// removal; the clash branch of a rename migration) and MIGRATED — not
	// bumped — across an ORDINARY rename, which continues the same window
	// under the new login (C4: a rename must never reset or double the
	// budget). See migrateAutoRedeemGenLocked (rename_reconcile.go) for the
	// migration rule and bumpAutoRedeemGenLocked (rewards.go) for the bump.
	// Entries are never deleted (a re-added login must not restart at a
	// generation a stale evaluator still holds) and are monotonic per key
	// (never decreased), so a sealed generation can never match again. New()
	// initializes this map; every helper must still tolerate reading a nil
	// map (returns the zero value, 0) since struct-literal test Miners never
	// run New().
	autoRedeemGen map[string]uint64
	// rewardsAPI is the tests-only seam over the narrow rewardsClient slice
	// of the Twitch client (nil in production — see the rewards() accessor
	// in rewards.go), mirroring this repo's topicReconciler/
	// renameAnalyticsService seam precedent so reward evaluation/redemption
	// (I8) is testable without network I/O.
	rewardsAPI rewardsClient

	mu sync.RWMutex

	// coordinatorMu serializes the WHOLE fail-closed settings-apply pipeline
	// (BKM-006 Corrective Pass 1, C2): resolve+plan, durable persist
	// (analytics + config.json) for any rename, and the runtime commit. It is
	// acquired BEFORE mu (lock order: coordinatorMu -> mu -> streamer.Manager.mu
	// -> models.Streamer.mu) and held across the durable-persist I/O — but mu,
	// manager.mu, and streamer.mu are never held during that I/O — so two
	// concurrent settings applies (e.g. two dashboard tabs) can never
	// interleave their durable-persist steps and leave the runtime, config
	// file, and analytics history disagreeing about a streamer's identity.
	coordinatorMu sync.Mutex

	// applyMu/applyWG/applyDraining are the shutdown/settings-apply interlock
	// (M1): beginApply refuses a NEW apply once applyDraining is set or
	// m.runCtx is already cancelled, and registers an accepted one in
	// applyWG; endApply (deferred by every accepted apply) removes it. stop()
	// sets applyDraining and calls applyWG.Wait() BEFORE any other teardown,
	// so no apply is ever in flight when the DB is later closed (App.Shutdown
	// closes it only after Run — and therefore stop() — returns). applyMu
	// makes "check draining, then Add(1)" atomic with "set draining, then
	// Wait()" — without it, an apply could observe draining=false and call
	// Add(1) AFTER Wait() has already seen the counter reach zero and
	// returned, which is a data race on sync.WaitGroup's own contract.
	applyMu       sync.Mutex
	applyWG       sync.WaitGroup
	applyDraining bool

	// loopWG tracks the background loops startMining spawns (stream check,
	// health watchdog, bonus poll, subscription probe, hourly validation,
	// resource sampler, daily summary). stop() joins them — bounded by
	// loopJoinTimeout — after the run context is cancelled, so an in-flight
	// tick's analytics or notification-config read completes before the
	// owner closes the shared database handle (S1).
	//
	// No admission gate is needed (unlike applyWG): every Add happens on the
	// goroutine executing startMining, and Run's control flow puts
	// startMining strictly before <-ctx.Done() and therefore strictly before
	// stop()'s Wait — program order is the whole no-Add-after-Wait argument.
	// The rule that preserves it: no loop may be started outside
	// startMining; a loop started from a settings apply would need the
	// applyWG-style gate instead.
	loopWG sync.WaitGroup

	// importMu serializes the read-modify-write in ImportStreamers so two
	// concurrent imports can't both read the pre-write snapshot and lose one
	// another's additions. GetRuntimeSettings (RLock) and ApplySettings (Lock)
	// are separate acquisitions, so mu alone does not make that pair atomic.
	importMu sync.Mutex
	// importApply is the apply step ImportStreamers runs after merging; nil in
	// production (falls back to ApplySettings). It exists only as a test seam so
	// the serialization can be exercised without the network/pubsub side effects
	// of the real apply path.
	importApply func(ctx context.Context, s settings.RuntimeSettings) error

	// applyCommitBarrier is a tests-only seam (nil in production; M2 [R2]):
	// invoked by applySettingsWithRemovals/applySettingsWithRename around
	// their commit section — applyPreCommit fires after durable prepare
	// (admission/analytics) but before the commit m.mu.Lock() (the D1
	// lost-update window: the live config map is still mutable by a
	// concurrent SetAutoRedeem); applyPostCommit fires after the commit
	// m.mu.Unlock() but before CommitPlan (the D2 resurrection window: the
	// new config is published but the runtime roster has not caught up
	// yet). Lets a test deterministically interleave a SYNCHRONOUS
	// SetAutoRedeem call inside either window without goroutines/sleeps.
	applyCommitBarrier func(applyCommitPhase)
}

// applyCommitPhase identifies which side of an apply's commit section
// applyCommitBarrier was invoked from — see that field's doc comment.
type applyCommitPhase int

const (
	// applyPreCommit fires after durable prepare, before the commit m.mu.Lock.
	applyPreCommit applyCommitPhase = iota
	// applyPostCommit fires after the commit m.mu.Unlock, before CommitPlan.
	applyPostCommit
)

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
		autoRedeemGen:      make(map[string]uint64),
		healthJournal:      journal.New[journal.HealthEvent](healthJournalCapacity, nil),
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

// SetDashboardConfig injects the resolved, immutable dashboard exposure/auth
// snapshot. App calls it during composition; it is consumed only by the
// fallback web-server build in start() (the library path where App did not
// inject a web server). Called before Run.
func (m *Miner) SetDashboardConfig(d runtimeconfig.Dashboard) {
	m.dashboard = d
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

// SetWebServer injects an externally-owned web server (the composition root
// builds it, starts it, and stops it). When set, the miner uses the server and
// wires itself in as the runtime data provider but neither starts nor stops it;
// without it the library-use fallback in setupComponents builds and owns its own
// — exactly one owner either way.
func (m *Miner) SetWebServer(server *web.Server) {
	m.webServer = server
	m.externalWebServer = true
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

	// A drain timeout is surfaced as an explicit shutdown error (S1): the
	// teardown itself still completed (stop attempts every step), but the
	// caller learns that an asynchronous writer had to be abandoned at its
	// bound instead of the failure hiding in a log line.
	if derr := m.stop(); derr != nil {
		return fmt.Errorf("shutdown drain incomplete: %w", derr)
	}

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

	m.dbBasePath = filepath.Join("database", m.config.StorageKey())

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

	// Auth dials cookies/credentials under the STABLE storage key (COR-2), not
	// the mutable canonical login — so a renamed owner keeps loading the same
	// credential file. StorageKey() == config.Username until the first rename.
	m.auth = auth.NewTwitchAuth(m.config.StorageKey(), m.deviceID)
	// Recovery-owner work (refresh, device-flow polling) is bounded by the run
	// context, not by whichever rejected request happened to trigger it.
	m.auth.SetLifecycleContext(ctx)
	// BKM-006 Corrective Pass 1, C3: pin the trusted owner identity BEFORE
	// Login, so a renamed owner (same Twitch account, new login) is tolerated
	// on this and every future restart with no fresh Device Flow — config.Username
	// stays the stable profile/cookie/db/log storage key regardless. Empty
	// when no pin is configured yet, which preserves the exact legacy
	// login-anchor behavior (BKM-005).
	m.auth.SetExpectedUserID(m.config.OwnerUserID)

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

	// BKM-006 Corrective Pass 1 (C3 + COR-2): after a confirmed Login, reconcile
	// the persisted owner identity — in one save.
	//
	// (a) COR-2 canonical-login adoption: the authoritative validate reports the
	//     account's CURRENT Twitch login (GetCanonicalLogin, only ever a login
	//     that already passed the identity check — never foreign). If it differs
	//     from config.Username, the owner was renamed: pin ProfileKey to the
	//     pre-rename login FIRST (so cookies/db/logs stay under the key they were
	//     created with), then adopt the new canonical login into config.Username
	//     so every Twitch-/user-facing use (owner channel-ID resolution, IRC
	//     NICK, notifications, dashboard) tracks the new name. Storage never
	//     follows the mutable login.
	// (b) C3 owner-pin backfill: userIDAuthoritative resets to false on every
	//     fresh process start, so IsUserIDConfirmed() here means THIS Login
	//     authoritatively confirmed the identity; with no pin configured,
	//     applyValidation's legacy anchor required the login to have matched (a
	//     mismatch would have failed closed to a fresh Device Flow), so the
	//     confirmation is trustworthy. Fires once, then persists.
	//
	// A SaveConfig failure is logged, not fatal: both are re-attempted on the
	// next restart until they persist (and the pin then makes every future
	// restart rename-tolerant).
	oldLogin := m.config.Username
	res := reconcileOwnerIdentity(m.config, m.auth.GetCanonicalLogin(), m.auth.GetUserID(), m.auth.IsUserIDConfirmed())
	if res.renamed {
		slog.Info("Owner Twitch login changed; adopting the new canonical login (storage key unchanged)",
			"oldLogin", oldLogin, "newLogin", m.config.Username)
	}
	if res.pinned {
		slog.Info("Pinned the Twitch owner identity for rename-tolerant startups")
	}
	if res.changed() && m.configPath != "" {
		if err := config.SaveConfig(m.configPath, m.config); err != nil {
			slog.Warn("Failed to persist owner-identity reconciliation; will retry on the next restart", "error", err)
		}
	}

	m.client = twitch.NewTwitchClient(m.auth, m.deviceID)
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
	lookupID, lookupErr := retryStartupLookup(ctx, func() (string, error) {
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
	if retried && retryBroadcaster != nil {
		// Clear the retry banner now that Twitch answered (or fail-fast'd);
		// without this the overlay would keep showing the last error until
		// loadStreamers runs.
		retryBroadcaster.SetStatus(web.StatusLoadingStreamers, "Loading streamers...")
	}
	// Explicit identity-binding guard. The token session's user ID (validated
	// via /oauth2/validate at login/promotion, or loaded from the stored
	// record on a degraded startup) is the identity source; GetChannelID's
	// resolution of the configured username is only a CROSS-CHECK, never a
	// substitute for it — an empty session ID after a successful Login is an
	// invariant violation that fails startup rather than fabricating
	// authority from the lookup. A RENAMED owner login (Twitch reports the
	// configured username as not-found) does not abort startup, though:
	// resolveStartupIdentity falls back to the already-validated session
	// identity (BKM-006 P) instead of treating a stale config username as a
	// fatal mismatch. Any other mismatch still fails closed — nothing is
	// deleted, the operator decides.
	userID, staleLogin, err := resolveStartupIdentity(m.auth.GetUserID(), lookupID, lookupErr)
	if err != nil {
		if lookupErr != nil && !errors.Is(lookupErr, twitch.ErrStreamerDoesNotExist) {
			return fmt.Errorf("failed to get user ID: %w", err)
		}
		// The credential file is keyed by the stable StorageKey() (COR-2), which
		// can differ from the (mutable) canonical login shown for context.
		return fmt.Errorf("%w: session/profile identity binding failed for %q; remove cookies/%s.json or fix the configured username",
			err, m.config.Username, m.config.StorageKey())
	}
	if staleLogin {
		slog.Warn("Configured username appears to have been renamed on Twitch; proceeding with the validated session identity",
			"username", m.config.Username)
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
				m.webServer.SetDashboardConfig(m.dashboard)
				m.webServer.SetSettingsProvider(m)
				m.webServer.SetSettingsUpdateCallback(m.ApplySettings)
				m.webServer.SetNextStreamCheckProvider(m)
				m.webServer.SetRewardsProvider(m)
				m.webServer.SetOverviewProvider(m)
				m.webServer.SetPredictionControlProvider(m)
			}
		}
	}

	m.initNotificationManager(ctx)

	// Resolve the dashboard/notification display time zone once (from the logger
	// config; production sets Asia/Jerusalem) and share it, so absolute times on
	// the Drops "Upcoming" tab and in the upcoming-campaign alert match. Standard
	// time.Location handles DST; an empty/invalid zone falls back to local time.
	// initNotificationManager already resolved and pushed the same location into
	// the notifications manager itself, before publishing it (I8) — only the web
	// side still needs it here.
	if m.webServer != nil {
		m.webServer.SetDisplayLocation(resolveDisplayLocation(m.config.Logger.TimeZone))
	}

	var mentionHandler chat.MentionHandler
	if notifMgr := m.notificationManager(); notifMgr != nil {
		mentionHandler = notifMgr.NotifyMention
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
	// Gate every join against the LIVE roster (M3): rejects a stale
	// *models.Streamer surviving a removal (checkAllStreamers/
	// checkUncheckedStreamers iterating an older All() snapshot, or a
	// delayed startMining initial sweep) before it can create a ghost IRC
	// client for a channel Leave already tore down.
	m.chatManager.SetRosterMembership(m.chatRosterMembership)

	var watchTimeStore *watcher.WatchTimeStore
	if m.db != nil {
		store, err := watcher.NewWatchTimeStore(m.db)
		if err != nil {
			slog.Error("Failed to create watch-time store, rotation fairness will not persist across restarts", "error", err)
			events.Record(events.TypeModuleInitFailed, "", "watch_time: "+err.Error())
		} else {
			watchTimeStore = store
			m.watchTimeStore = store
		}
	}

	// A standalone notifications repository over the shared DB, independent of
	// whether Discord/notifications are enabled, so a streamer's point_rules and
	// config-list membership are purged (and rename-synced) even with Discord off
	// — those rows persist in the file regardless of a live Manager.
	if m.db != nil && m.notificationsRepo == nil {
		if repo, err := notifications.NewRepository(m.db); err != nil {
			slog.Error("Failed to create notifications repository for streamer-deletion purge", "error", err)
		} else {
			m.notificationsRepo = repo
		}
	}

	// Build the streamer-deletion coordinator over every login-keyed persisted
	// store that exists, so removing a streamer purges its analytics history,
	// notification rules/config-lists, and watch-time rows in ONE atomic
	// transaction (and arms the resurrection fence). Each store implements both
	// the purge and the fence; nil subsystems are simply not covered.
	m.buildStreamerLifecycle()

	// Retry any streamer deletion whose persisted purge did not finish before the
	// last exit (durable pending-deletion rows). Runs here, before the watch /
	// pubsub / event loops start, so reinstating a cleaned login cannot race a
	// live event.
	m.reconcilePendingStreamerDeletions()

	m.watcher = watcher.NewMinuteWatcher(
		m.client,
		streamers,
		m.config.Priority,
		m.config.RateLimits,
		watchTimeStore,
	)
	// Inject the bounded slot-lifecycle diagnostic journal (BKM-013). Diagnostic
	// only: it observes slot transitions and never affects selection or sending.
	m.watcher.SetSlotJournal(journal.New[journal.SlotEvent](slotJournalCapacity, nil))
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
	// upcoming campaign. The adapter reads the write-once notification manager
	// via the shared accessor at call time, and the manager owns the opt-in
	// gate + durable dedupe, so no alert is ever sent unless the operator
	// enabled the event.
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
		minerHealthNotifier{m}, // reads the write-once notification manager via the accessor at call time
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
		minerDropNotifier{m}, // reads the write-once notification manager via the accessor at call time
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
		// Wired unconditionally (unlike SetDebugSnapshotProvider below, which
		// is gated on Debug.Enabled): the redacted support bundle is an
		// always-available diagnostic download, not a debug-mode feature. The
		// same in-process snapshot builder feeds both; the web layer's own
		// typed allowlist (internal/supportbundle) decides what actually
		// leaves the process.
		m.webServer.SetSupportBundleSource(m.BuildDebugSnapshot)
	}

	if m.config.ClaimDropsOnStartup {
		slog.Info("Claiming all drops from inventory on startup")
	}
}

// notificationManager returns the write-once notifications Manager published
// by initNotificationManager (nil when the miner runs without a database, or
// when construction itself failed at startup). This is the ONLY way any
// other code in this package may observe m.notifications: it takes a brief
// RLock, copies the pointer, and releases the lock before returning, so a
// caller never holds m.mu while calling into the Manager (UpdateDiscordConfig
// and several Notify* methods perform Discord network I/O or a SQLite read)
// and never races the one-time publish. An AST boundary test
// (m4_boundary_test.go) enforces at build-test time that no other production
// code in this package reads the field directly.
func (m *Miner) notificationManager() *notifications.Manager {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.notifications
}

// initNotificationManager builds and publishes the miner's notifications
// Manager (M4). It is the ONLY assignment to m.notifications anywhere in
// production code — every other reader goes through the notificationManager()
// accessor above. Called once from setupComponents, strictly before
// startMining launches any goroutine that could read m.notifications, so
// every later reader observes either nil (no database, or a failed
// construction — see the log/events branch below) or a fully constructed,
// already-started Manager — never a partially built one and never a torn
// write. (This replaces the fix's confirmed data race: the prior version
// created the Manager lazily, at RUNTIME, from inside finishApply when
// Discord first flipped on, so a concurrent reader elsewhere could observe a
// manager that was still mid-construction.)
//
// A Manager is constructed whenever a database exists, regardless of whether
// Discord or any push provider is actually enabled (NewManager tolerates both
// being off) — so a LATER runtime enable (finishApply's UpdateDiscordConfig
// call) never needs to create or replace the pointer, only reconfigure the
// one that already exists; there is no more create-or-rebuild branch at
// runtime at all. Points tracking and the display time zone are both set
// BEFORE publication, so a reader that observes a non-nil manager never sees
// one with stale/zero-value tracking state. Web wiring order matters too:
// SetNotificationManager always runs before SetDiscordEnabled, and
// SetDiscordEnabled is only ever true when a manager actually exists — so the
// dashboard can never believe Discord is live while there is no manager
// behind it (this holds both here and in finishApply's runtime reconcile).
func (m *Miner) initNotificationManager(ctx context.Context) {
	var notifMgr *notifications.Manager
	if m.db != nil {
		mgr, err := notifications.NewManager(&m.config.Discord, &m.config.Notifications, m.db, m.streamers.Names(), m.config.Username)
		if err != nil {
			// Unlike before this pass, there is no off->on retry path left: the
			// runtime creation branch that used to give a second chance (when
			// Discord was later enabled) is gone, since the Manager is now
			// always constructed here, once. A failed construction therefore
			// disables notifications for the rest of this process's life.
			slog.Error("Failed to create notification manager; notifications stay DISABLED until the miner is restarted", "error", err)
			events.Record(events.TypeModuleInitFailed, "", "notifications: "+err.Error())
		} else {
			notifMgr = mgr
		}
	}

	if notifMgr != nil {
		notifMgr.InitializePointsTracking(m.streamers.PointsMap())
		notifMgr.SetDisplayLocation(resolveDisplayLocation(m.config.Logger.TimeZone))

		m.mu.Lock()
		m.notifications = notifMgr
		m.mu.Unlock()
	}

	if m.webServer != nil {
		if notifMgr != nil {
			m.webServer.SetNotificationManager(notifMgr)
		}
		m.webServer.SetDiscordEnabled(m.config.Discord.Enabled && notifMgr != nil)
	}

	if notifMgr != nil {
		if err := notifMgr.Start(ctx); err != nil {
			slog.Error("Failed to start notification manager", "error", err)
		}
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
			logPath = logger.LogFilePath(m.config.StorageKey())
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
		// Local resource sampler for the dashboard mini-widgets. Reads only the
		// process/container's own /proc and cgroup counters; no external calls,
		// no state mutation. Runs on the run context and stops with it.
		m.resourceSampler = resources.New()
		m.webServer.SetResourceSnapshotProvider(m.resourceSampler.Latest)
		m.startLoop(ctx, m.resourceSampler.Run)

		// Start the web server only when the miner built it itself (library
		// use). An injected server is started AND stopped by its owner (the
		// composition root), so both halves of its lifecycle key off the same
		// externalWebServer flag — never analytics ownership.
		if !m.externalWebServer {
			// Fail-closed: on a non-loopback bind without credentials the
			// dashboard stays down (mining continues); the primary
			// cmd/miner path aborts the whole process for the same error.
			if err := m.webServer.Start(); err != nil {
				slog.Error("Web server NOT started", "error", err)
			}
		}
		m.webServer.GetStatusBroadcaster().SetStatus(web.StatusRunning, "Mining active")
	}

	m.startLoop(ctx, m.streamCheckLoop)
	m.startLoop(ctx, m.healthWatchdogLoop)
	m.startLoop(ctx, m.bonusPollLoop)
	m.startLoop(ctx, m.subscriptionProbeLoop)
	// Hourly token validation is a Twitch requirement (validate on startup —
	// done inside Login — and hourly thereafter). One validator per session;
	// it joins the shared single-flight recovery on an authoritative 401.
	m.startLoop(ctx, m.auth.RunHourlyValidation)
	if m.config.DailySummary.Enabled && m.analyticsSvc != nil {
		m.startLoop(ctx, m.dailySummaryLoop)
	}
	m.startAutoUpdater(ctx)
}

// startLoop spawns one of startMining's background loops registered in
// loopWG, so stop() can join it (bounded) before the shared database handle
// is closed. Must only be called on the startMining call path (startMining
// itself and startAutoUpdater, which startMining invokes on the same
// goroutine) — see loopWG's ordering contract.
func (m *Miner) startLoop(ctx context.Context, fn func(context.Context)) {
	m.loopWG.Add(1)
	go func() {
		defer m.loopWG.Done()
		fn(ctx)
	}()
}

// loopJoinTimeout bounds how long stop() waits for startMining's background
// loops to drain their in-flight tick. The run context is already cancelled
// by the time stop() runs, so the expected real wait is the tail of at most
// one tick body. Package variable so tests can shrink it (the watcher/drops
// precedent). 3s keeps the aggregate shutdown drain budget documented in
// stop() under the process-level shutdown deadline.
var loopJoinTimeout = 3 * time.Second

// errLoopJoinTimeout is the explicit shutdown error joinLoops returns when
// the background loops did not finish inside loopJoinTimeout.
var errLoopJoinTimeout = errors.New("miner: background loop join timed out")

// joinLoops waits — bounded by loopJoinTimeout — for the loops startMining
// spawned, returning the explicit errLoopJoinTimeout on timeout (it never
// hangs). A miner whose startMining never ran has an empty loopWG and
// returns nil immediately.
func (m *Miner) joinLoops() error {
	done := make(chan struct{})
	go func() {
		m.loopWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-time.After(loopJoinTimeout):
		slog.Warn("Miner background loops did not finish within the stop timeout; proceeding with shutdown",
			"timeout", loopJoinTimeout)
		return fmt.Errorf("%w after %s", errLoopJoinTimeout, loopJoinTimeout)
	}
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

	// Tracked like the other startMining loops (S1): the updater's notify
	// callbacks read the notifications config from SQLite on this goroutine,
	// so it must be joined before the handle closes. startAutoUpdater runs on
	// startMining's goroutine, preserving loopWG's program-order argument.
	m.startLoop(ctx, upd.Run)
}

// notifyUpdateAvailable logs and, when Discord is enabled, dispatches an
// update-available notification. Reads the write-once notifications manager
// via the shared accessor so it works even if Discord was toggled on after
// startup.
func (m *Miner) notifyUpdateAvailable(current, latest, releaseURL string) {
	events.Record(events.TypeUpdateAvailable, "", fmt.Sprintf("%s -> %s", current, latest))

	if notifMgr := m.notificationManager(); notifMgr != nil {
		notifMgr.NotifyUpdateAvailable(current, latest, releaseURL)
	}
}

// notifyUpdateFailed logs and, when Discord is enabled, dispatches an
// update-failed notification (fail-closed checksum refusal, download error,
// or a failed binary swap). Mirrors notifyUpdateAvailable.
func (m *Miner) notifyUpdateFailed(current, latest, reason string) {
	events.Record(events.TypeUpdateFailed, "", fmt.Sprintf("%s -> %s: %s", current, latest, reason))

	if notifMgr := m.notificationManager(); notifMgr != nil {
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
			slog.Debug("Skipping bonus poll: not eligible", "streamer", s.GetUsername(), "reason", err.Error())
			m.evaluateAutoRedeem(s)
			continue
		}

		claimed, err := m.client.ClaimAvailableBonus(s)
		if err != nil {
			slog.Debug("Bonus poll failed", "streamer", s.GetUsername(), "error", err)
		} else if claimed {
			slog.Info("Claimed channel points bonus via GQL fallback poll (PubSub missed the claim-available event)",
				"streamer", s.GetUsername())
			events.Record(events.TypeBonusClaimed, s.GetUsername(), "bonus claimed (GQL fallback)")
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

// chatRosterMembership is the ChatManager.SetRosterMembership predicate
// (M3): it checks POINTER IDENTITY of s against the live roster's CURRENT
// object for s's login, not login equality. This rejects (a) a stale pointer
// after removal (the login no longer maps to s at all, or maps to nothing),
// (b) a mis-keyed/foreign pointer whose login now resolves to a different
// tracked object (never true roster membership even though the login
// matches), and (c) the OLD pointer after a same-login re-add (the roster
// tracks a NEW object under that login now) — while accepting the NEW
// pointer after a same-login re-add and a renamed-in-place member (same
// object, whatever its current login is, since Get resolves by CURRENT
// login and byLogin/byID are repointed together on a rename).
//
// Benign TOCTOU: a rename can commit between s.GetUsername() (read by
// joinChat, under ChatManager.mu, before this predicate runs) and this
// method's own m.streamers.Get call. That race makes the join fail closed for
// one cycle — never a false accept — and the next periodic sweep
// (checkAllStreamers/checkUncheckedStreamers) or capability reconcile heals
// it on the very next ToggleChat. Do NOT "fix" this by moving the predicate
// outside ChatManager.mu; that would reopen the stale-pointer race this
// predicate exists to close.
//
// The nil-guard mirrors the existing m.streamers nil-guard pattern elsewhere
// in this package (e.g. rewards.go) for struct-literal Miners built directly
// in tests, which never run New() and so never populate m.streamers.
func (m *Miner) chatRosterMembership(s *models.Streamer) bool {
	return m.streamers != nil && m.streamers.Get(s.GetUsername()) == s
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
							m.streamers.RecordStreakGrant(s.GetUsername())
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

			if notifMgr := m.notificationManager(); notifMgr != nil {
				notifMgr.NotifyPointsReached(s.GetUsername(), s.GetChannelPoints())
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

// resolveStartupIdentity decides which Twitch user ID the miner binds its own
// identity to at startup, reconciling BKM-005's session-is-authoritative
// stance with BKM-006's config-driven rename reconciliation: GetChannelID
// resolves the CONFIGURED username, which may have been renamed on Twitch,
// while the OAuth session's own validated user ID never depends on that
// login at all. It is a pure decision function — the caller (authenticate)
// owns all I/O and logging — so every branch is directly unit-testable.
//
//   - lookupErr == nil: the lookup succeeded, so it is cross-checked against
//     the session identity exactly as before (verifyIdentityBinding); on
//     success userID is resolvedUserID (== sessionUserID) and staleLogin is
//     false.
//   - errors.Is(lookupErr, twitch.ErrStreamerDoesNotExist) with a non-empty
//     sessionUserID: the configured login no longer resolves (most likely
//     renamed away), but the session already carries a validated identity —
//     proceed with it instead of aborting startup. staleLogin is true so the
//     caller can log ONE privacy-safe warning.
//   - an empty sessionUserID: fails closed regardless of lookupErr — a
//     successful Login always leaves a session identity, so its absence is
//     an invariant violation, never a reason to fabricate one from the
//     lookup.
//   - anything else (token rejection, transport exhaustion that outlived the
//     retry loop, a cancelled context, ...): passed through verbatim; the
//     caller decides how to report it.
func resolveStartupIdentity(sessionUserID, resolvedUserID string, lookupErr error) (userID string, staleLogin bool, err error) {
	if lookupErr == nil {
		if err := verifyIdentityBinding(sessionUserID, resolvedUserID); err != nil {
			return "", false, err
		}
		return resolvedUserID, false, nil
	}
	if errors.Is(lookupErr, twitch.ErrStreamerDoesNotExist) && sessionUserID != "" {
		return sessionUserID, true, nil
	}
	if sessionUserID == "" {
		return "", false, auth.ErrIdentityMismatch
	}
	return "", false, lookupErr
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

	if notifMgr := m.notificationManager(); notifMgr != nil {
		notifMgr.NotifyReauthRequired("Open the dashboard to complete the Twitch device login (or restart the miner).")
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
	// notifMgr is nil only when the miner runs without a database (or startup
	// construction itself failed) — never a race with startup, since the
	// write-once manager is published before this handler's goroutine can
	// even be reached (see initNotificationManager).
	notifMgr := m.notificationManager()
	if notifMgr == nil {
		return
	}

	// Only authoritative online/offline transitions notify. Unknown carries no
	// notification, so a transient check failure never fires a false "went offline".
	switch status {
	case models.StatusOnline:
		notifMgr.NotifyOnline(username)
	case models.StatusOffline:
		notifMgr.NotifyOffline(username)
	}
}

// stop tears the runtime graph down and drains every asynchronous SQLite
// writer it spawned. It returns the aggregate of the explicit drain-timeout
// errors (nil on a fully clean drain); every teardown step is attempted
// regardless of earlier failures, and stop never hangs.
//
// Aggregate drain budget (S1): the bounded waits below are serial in the
// worst case — miner loops 3s, chat 3s (per-client joins run concurrently),
// pubsub 3s (likewise), watcher 5s, drops 5s, notifications dispatch 5s plus
// its pre-existing 15s batcher flush — ~39s if literally everything wedges
// at once, but each bound only spends time when its component is actually
// stuck; the expected real drain is milliseconds (every loop is already
// unblocked by the cancelled run context and closed sockets, and in-flight
// sends are cancelled before the dispatch wait). main's
// app.DefaultShutdownTimeout (30s) applies to the App steps AFTER Run
// returns and is unaffected by these bounds.
func (m *Miner) stop() error {
	// Drain in-flight settings applies FIRST, before anything else tears
	// down: App.Shutdown closes the DB only after Run (and therefore this
	// function) returns, so once applyWG.Wait() returns here no apply can
	// still be mid-flight when that close happens (M1: closes the same race
	// class as the cancelled-runCtx hazard, but for the DB handle itself).
	m.applyMu.Lock()
	m.applyDraining = true
	m.applyMu.Unlock()
	m.applyWG.Wait()

	var drainErrs []error

	// Join the miner's own background loops BEFORE closing the transports
	// (S1): streamCheckLoop creates IRC clients via ToggleChat and pubsub
	// topics via the check path, so draining the producer first means the
	// transports' Close below is not racing a fresh spawn (their own closed
	// flags are the belt to this braces). The loops exit on the already-
	// cancelled run context; the join is bounded by loopJoinTimeout.
	drainErrs = append(drainErrs, m.joinLoops())

	// Both transports JOIN their read loops (bounded): after these two calls
	// no goroutine can enter analytics or notifications from a network
	// message, and any handler that was mid-write has finished.
	drainErrs = append(drainErrs, m.chatManager.Close())
	drainErrs = append(drainErrs, m.wsPool.Close())
	m.watcher.Stop()
	m.dropsTracker.Stop()
	m.discovery.Stop()
	if m.canary != nil {
		m.canary.Stop()
	}
	if m.progressWatchdog != nil {
		m.progressWatchdog.Stop()
	}

	// Stop the web server and analytics service only when the miner built them
	// itself (library use). When they were injected (SetWebServer /
	// SetAnalyticsService), the composition root owns their teardown and closes
	// them after Run returns — closing here would be a second close by a second
	// owner. The debug server is always miner-built, so it is always stopped
	// here.
	if m.webServer != nil && !m.externalWebServer {
		m.webServer.Stop()
	}

	if m.debugServer != nil {
		m.debugServer.Stop()
	}

	// Notifications stop after every Notify* caller above has quiesced (so
	// legitimate shutdown-time alerts are not refused early) and BEFORE the
	// analytics close below, keeping the invariant ordering: drain admitted
	// writers, then close repositories, then the database. The Manager's
	// Stop performs the S1 dispatch drain — admission closes, in-flight
	// sends are cancelled, dispatch goroutines (including the
	// point_rule.triggered persistence that runs after a network send) are
	// joined, bounded — so no notification writer is in flight once stop()
	// returns and the owner closes the database.
	if notifMgr := m.notificationManager(); notifMgr != nil {
		drainErrs = append(drainErrs, notifMgr.Stop())
	}

	if m.analyticsSvc != nil && !m.externalAnalytics {
		_ = m.analyticsSvc.Close()
	}

	// Close the DB only when the miner opened it itself (library use). In
	// the cmd/miner path main owns the handle and closes it after Run
	// returns. With the S1 drains above (settings applies, miner loops incl.
	// the auto-updater, chat/pubsub read loops and reconnect drivers,
	// watcher, drops, notification dispatch) every asynchronous SQLite
	// WRITER this runtime spawns has been joined or refused by the time this
	// line runs. Known read-only residual: the bounded (2-min) auth-recovery
	// observer can, in a rare recovery race, still perform a notifications
	// config READ after Run returns — logged, no data loss (see
	// recoverFromRejectedGeneration).
	if m.db != nil && m.ownsDB {
		_ = m.db.Close()
	}

	m.streamers.PrintReport()
	return errors.Join(drainErrs...)
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

// beginApply registers the caller as an in-flight settings apply, refusing
// (false) once the miner has started draining (Stop, in progress or done) or
// its run context is already cancelled — the latter closes the same window
// the cancelled-runCtx hazard exploited: a POST arriving after runCtx.Done()
// fires but before the web listener is actually closed (web.Server.Stop is a
// hard Close, not a graceful Shutdown; nothing else joins an in-flight apply —
// see the M1 design manifest). Every accepted apply MUST defer endApply.
func (m *Miner) beginApply() bool {
	m.applyMu.Lock()
	defer m.applyMu.Unlock()
	if m.applyDraining {
		return false
	}
	if m.runCtx != nil && m.runCtx.Err() != nil {
		return false
	}
	m.applyWG.Add(1)
	return true
}

// endApply releases the registration beginApply made. Always deferred
// immediately after a successful beginApply.
func (m *Miner) endApply() {
	m.applyWG.Done()
}

// ApplySettings applies posted runtime settings. It satisfies
// settings.SettingsUpdateCallback: ctx bounds/cancels the request exactly
// like an http.Handler's r.Context(), and the returned error is
// applySettings' own, verbatim — a failed apply is ALSO logged here (so
// every caller gets a diagnostic even if it discards the error, e.g.
// ImportStreamers' fire-and-forget legacy callers), never silently
// swallowed, and — BKM-006 Corrective Pass 1, C2 — never followed by a
// misleading "Runtime settings updated" success log, since that log lives
// only on applySettings' success path.
func (m *Miner) ApplySettings(ctx context.Context, s settings.RuntimeSettings) error {
	if err := m.applySettings(ctx, s); err != nil {
		slog.Error("Settings apply failed; no runtime, config, analytics, or capability state was changed for the affected streamer(s)",
			"error", err)
		return err
	}
	return nil
}

// applySettings is the fail-closed settings-apply coordinator (BKM-006
// Corrective Pass 1, C2; extended by M1's Streamer Removal Admission
// Protocol, SRAP, for any apply that removes a streamer). It gates on
// beginApply (refusing with ErrShuttingDown before touching anything if the
// miner is draining or already shut down), captures the streamer-deletion
// coordinator ONCE for the whole apply (m.streamerLifecycle is built once at
// startup and — since M4 removed finishApply's runtime create-or-rebuild
// branch for the notifications manager — is never rebuilt mid-run in
// production; the single capture here still gives the apply a stable,
// self-consistent view even if a caller, e.g. a test, swaps the coordinator
// concurrently), then resolves the intended streamer roster ONCE
// (streamer.Manager.PlanReconcile: Twitch resolution unlocked, then a
// read-locked, non-mutating decision pass) and reuses that SAME plan for
// whichever path below applies, so Twitch is never queried twice for one
// apply:
//
//   - No rename AND no removal planned: every other settings field applies
//     directly to the live config — today's behavior, unchanged (a SaveConfig
//     failure here is logged and non-fatal, exactly as before this pass,
//     since no non-rename, non-removal change can split runtime/config/
//     analytics identity across two owners, or leave an unaccounted-for
//     purge).
//   - A removal planned, no rename: SRAP's own two-phase protocol — see
//     applySettingsWithRemovals.
//   - At least one rename planned (with or without a removal riding along):
//     the whole apply becomes a single transaction — see
//     applySettingsWithRename. A CANDIDATE config is built and the live
//     config is NEVER mutated until durable persistence succeeds.
//
// The whole sequence is serialized by coordinatorMu (lock order: coordinatorMu
// -> m.mu -> streamer.Manager.mu -> models.Streamer.mu). No Twitch/network
// I/O and no SQLite transaction ever runs under m.mu, manager.mu, or
// streamer.mu. Config-file writes are the one deliberate exception (M1):
// config.SaveConfig runs UNDER m.mu — in SRAP's commit step and in the other
// config writers alike — so durable config persistence and the publication
// of m.config form one serialized commit sequence with no lost-update window
// between them.
//
// [M2/R6] This lost-update guarantee is tightened further for AutoRedeem
// specifically: applySettingsWithRemovals and applySettingsWithRename both
// rebuild candidate.AutoRedeem from the LIVE map at their commit point
// (refreshCandidateAutoRedeemLocked, rewards.go), in the SAME m.mu section as
// config.SaveConfig and the m.config publish, so a SetAutoRedeem that commits
// during THIS apply's earlier unlocked I/O (durable admission, analytics
// rename) is never silently overwritten (D1), and a removed or renamed
// login's consent can never resurrect after the commit (D2). This guarantee
// is scoped to AutoRedeem: ApplyHealthSettings (health.go:424-434) and
// ApplyCampaignPolicy (policy.go:281-287) still mutate live VALUE fields that
// cloneConfigLocked's shallow copy already snapshotted by the time this
// function took m.mu above, so an edit to either landing inside an apply's
// own unlocked window is still silently overwritten by that apply's publish
// — a known residual of the same defect class, NOT fixed by this pass.
func (m *Miner) applySettings(ctx context.Context, s settings.RuntimeSettings) error {
	if !m.beginApply() {
		return ErrShuttingDown
	}
	defer m.endApply()

	m.coordinatorMu.Lock()
	defer m.coordinatorMu.Unlock()

	m.mu.RLock()
	coord := m.streamerLifecycle
	m.mu.RUnlock()

	streamersCfg := settings.StreamersFromDTO(s.Streamers)
	defaultSettings := settings.StreamerSettingsFromDTO(s.DefaultSettings)
	plan := m.streamers.PlanReconcile(streamersCfg, defaultSettings, nil)

	plannedRenames := plan.PlannedRenames()
	plannedRemovals := plan.PlannedRemovals(m.streamers)

	switch {
	case len(plannedRenames) > 0:
		return m.applySettingsWithRename(ctx, s, plan, plannedRenames, plannedRemovals, coord)
	case len(plannedRemovals) > 0:
		return m.applySettingsWithRemovals(ctx, s, plan, plannedRemovals, coord)
	default:
		return m.applySettingsNoRename(s, plan, coord)
	}
}

// applySettingsNoRename is the non-identity-mutating, non-removing path
// (PlannedRenames and PlannedRemovals are BOTH empty — the caller already
// checked): every posted setting (including the resolved streamer roster)
// applies directly to the live config, exactly as ApplySettings always did
// before the M1 pass. Zero behavior change: applyStreamerDeletions still runs
// (for any RE-ADD's owed-purge reconciliation) on m.runCtx exactly as before,
// since there is no removal in this apply to admit or commit.
func (m *Miner) applySettingsNoRename(s settings.RuntimeSettings, plan *streamer.ReconcilePlan, coord *streamerlifecycle.Coordinator) error {
	m.mu.Lock()
	settings.ApplyToConfig(m.config, s)
	cfg := m.config
	m.mu.Unlock()

	added, removed, changed, renamed, conflicts := m.streamers.CommitPlan(plan)
	logReconcileConflicts(conflicts)

	ctx := m.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	m.finishApply(ctx, coord, cfg, added, removed, changed, renamed, false)
	return nil
}

// applySettingsWithRemovals is SRAP's no-rename removal path (M1): a
// CANDIDATE config is built and the live config is never touched until the
// durable admission AND the config.json commit both succeed, so any failure
// leaves the runtime, in-memory config, on-disk config, and every persisted
// store completely untouched — "settings apply rejected; no changes were
// made" is then literally true, not just a log message.
//
// Sequence (see the M1 design manifest §5/§7 for the full state machine):
//  1. Build the candidate; ctx already cancelled -> abort, nothing touched.
//  2. AdmitRemovals on critA (WithTimeout(WithoutCancel(ctx), admissionBudget))
//     durably prepares every removal in ONE transaction. coord == nil means no
//     persisted store exists at all (nothing to admit) — the rest of this
//     function's config/runtime discipline still applies unchanged.
//  3. A LAST pre-commit cancellation check on the ORIGINAL ctx (critA never
//     observes cancellation by design): cancelled -> AbortAdmission + return,
//     zero mutation. This check is the CANCELLATION LINEARIZATION POINT:
//     cancellation observed at or before it aborts with zero visible
//     mutation; cancellation arriving after it — including while SaveConfig
//     is running in step 4 — is never observed again, and the bounded
//     sequence runs to completion. (The atomic rename in step 4 is the
//     durability commit point for FAILURE semantics, not a cancellation
//     boundary.)
//  4. THE COMMIT POINT: under m.mu, refreshCandidateAutoRedeemLocked first
//     rebuilds candidate.AutoRedeem from the CURRENT live map and applies
//     this apply's own removal deletions (I2 — a SetAutoRedeem committed
//     during step 2's unlocked admission I/O is never lost, D1); then
//     config.SaveConfig(candidate) runs; on success publish m.config =
//     candidate AND, in THE SAME critical section, delete
//     m.autoRedeemState[login] and bump its generation for every committed
//     removal (I4 — atomic with the publish, so a removed streamer's
//     consent can never resurrect, D2) — closing the lost-update window
//     against the other four config-writers (which all persist under m.mu
//     too). On failure: AbortAdmission, return — zero mutation, including
//     to AutoRedeem/state/generation. configPath == "" is a documented
//     no-op success (as every other non-fatal SaveConfig path already
//     treats it).
//  5. Past the commit point there is NO abort: CommitPlan + finishApply
//     (persisted=true) commit the runtime and complete each removal
//     (CommitRemoval) on critB — a completion failure is logged truthfully
//     and durably retried, never fails this function (a committed removal
//     with a purge still owed IS a success).
func (m *Miner) applySettingsWithRemovals(ctx context.Context, s settings.RuntimeSettings, plan *streamer.ReconcilePlan, removals []*models.Streamer, coord *streamerlifecycle.Coordinator) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("settings apply rejected; no changes were made: %w", err)
	}

	m.mu.Lock()
	candidate := m.cloneConfigLocked()
	configPath := m.configPath
	m.mu.Unlock()

	settings.ApplyToConfig(candidate, s)

	// removedLogins is built ONCE per apply, lowercased at construction,
	// regardless of whether coord is nil — the commit-point AutoRedeem/
	// runtime-state cleanup below (I2/I4) must happen even when there is no
	// persisted store to admit removals against.
	removedLogins := make([]string, 0, len(removals))
	for _, r := range removals {
		removedLogins = append(removedLogins, strings.ToLower(r.GetUsername()))
	}

	var admittedLogins []string
	if coord != nil {
		batch := make([]streamerlifecycle.Removal, 0, len(removals))
		channelIDs := make([]string, 0, len(removals))
		for _, r := range removals {
			login := r.GetUsername()
			batch = append(batch, streamerlifecycle.Removal{ChannelID: r.ChannelID, Login: login})
			admittedLogins = append(admittedLogins, login)
			channelIDs = append(channelIDs, r.ChannelID)
		}

		critA, cancelA := context.WithTimeout(context.WithoutCancel(ctx), admissionBudget)
		admitErr := coord.AdmitRemovals(critA, batch)
		cancelA()
		if admitErr != nil {
			return fmt.Errorf("settings apply rejected; no changes were made: admit streamer removal(s): %w", admitErr)
		}
		slog.Info("Prepared streamer removal(s)", "count", len(batch), "channelIDs", channelIDs)

		if err := ctx.Err(); err != nil {
			m.abortAdmittedRemovals(coord, admittedLogins, "the request was cancelled before config.json could be committed")
			return fmt.Errorf("settings apply rejected; no changes were made: %w", err)
		}
	}

	if m.applyCommitBarrier != nil {
		m.applyCommitBarrier(applyPreCommit)
	}
	m.mu.Lock()
	m.refreshCandidateAutoRedeemLocked(candidate, nil, removedLogins)
	if configPath != "" {
		if err := config.SaveConfig(configPath, candidate); err != nil {
			m.mu.Unlock()
			m.abortAdmittedRemovals(coord, admittedLogins, "config.json persistence failed")
			return fmt.Errorf("settings apply rejected; no changes were made: persist config: %w", err)
		}
	}
	m.config = candidate
	for _, login := range removedLogins { // I4: atomic with the publish
		delete(m.autoRedeemState, login)
		m.bumpAutoRedeemGenLocked(login)
	}
	m.mu.Unlock()
	if m.applyCommitBarrier != nil {
		m.applyCommitBarrier(applyPostCommit)
	}
	if len(admittedLogins) > 0 {
		slog.Info("Streamer removal committed", "count", len(admittedLogins))
	}

	added, removed, changed, renamed, conflicts := m.streamers.CommitPlan(plan)
	logReconcileConflicts(conflicts)

	critB, cancelB := context.WithTimeout(context.WithoutCancel(ctx), purgeBudget)
	defer cancelB()
	m.finishApply(critB, coord, candidate, added, removed, changed, renamed, true)
	return nil
}

// abortAdmittedRemovals compensates AdmitRemovals for an apply that never
// reached its commit point, on a fresh WithoutCancel+WithTimeout context
// (the original request ctx may already be the reason this is being called).
// A no-op when logins is empty (coord == nil, or nothing was admitted this
// apply) or coord is nil. A failed compensation is logged but never returned
// — the rows it leaves behind are deterministically resolved by
// ArbitratePrepared at the next startup (see that function's doc comment).
func (m *Miner) abortAdmittedRemovals(coord *streamerlifecycle.Coordinator, logins []string, reason string) {
	if coord == nil || len(logins) == 0 {
		return
	}
	abortCtx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), admissionBudget)
	defer cancel()
	if err := coord.AbortAdmission(abortCtx, logins); err != nil {
		slog.Error("Failed to compensate a prepared streamer removal after "+reason+"; startup arbitration will resolve it on the next restart",
			"streamers", logins, "error", err)
	}
}

// applySettingsWithRename is the fail-closed transaction path (BKM-006 C2,
// extended by M1 for any removal riding along with a rename in the same
// apply, and by M2/I7 for the AutoRedeem commit): builds a candidate config
// independent of the live one, admits any planned removals durably (SRAP
// prepare phase) BEFORE the rename's own commit, durably persists analytics
// off-lock (commitAnalyticsRenames), and THE COMMIT POINT below persists
// config.json + publishes the candidate + migrates AutoRedeem runtime
// state/generation all under one m.mu section (split out of the former
// commitRenameTransaction — see that section's own comment for why). Only on
// success does it commit the plan to the runtime. A rename-transaction
// failure compensates the removal admission too (AbortAdmission), so the
// whole apply stays all-or-nothing: ZERO mutation of runtime, in-memory
// config, config file, analytics, AutoRedeem, IRC, or PubSub on any failure.
func (m *Miner) applySettingsWithRename(ctx context.Context, s settings.RuntimeSettings, plan *streamer.ReconcilePlan, plannedRenames []streamer.RenameEvent, removals []*models.Streamer, coord *streamerlifecycle.Coordinator) error {
	m.mu.Lock()
	candidate := m.cloneConfigLocked()
	configPath := m.configPath
	analyticsSvc := m.analyticsSvc
	m.mu.Unlock()

	settings.ApplyToConfig(candidate, s)
	// Config surgery for each planned rename: update the entry's Username in
	// place (settings pointer untouched) and stamp ChannelID, then backfill
	// ChannelID onto every OTHER entry from this SAME plan's resolution — all
	// on the candidate, still before any durable or runtime commit. This
	// early pass also touches candidate.AutoRedeem, but that result is
	// discarded: the commit-point refreshCandidateAutoRedeemLocked below
	// replaces candidate.AutoRedeem wholesale from the LIVE map — see
	// applyConfigRenames' doc comment (rename_reconcile.go).
	applyConfigRenames(candidate, plannedRenames)
	backfillChannelIDs(candidate, plan.ResolvedChannelIDs())

	// removedLogins is built ONCE per apply, lowercased at construction,
	// regardless of whether coord is nil — the commit-point AutoRedeem/
	// runtime-state cleanup below (I2/I4) must happen even when there is no
	// persisted store to admit removals against.
	removedLogins := make([]string, 0, len(removals))
	for _, r := range removals {
		removedLogins = append(removedLogins, strings.ToLower(r.GetUsername()))
	}

	var admittedLogins []string
	if coord != nil && len(removals) > 0 {
		batch := make([]streamerlifecycle.Removal, 0, len(removals))
		channelIDs := make([]string, 0, len(removals))
		for _, r := range removals {
			login := r.GetUsername()
			batch = append(batch, streamerlifecycle.Removal{ChannelID: r.ChannelID, Login: login})
			admittedLogins = append(admittedLogins, login)
			channelIDs = append(channelIDs, r.ChannelID)
		}
		critA, cancelA := context.WithTimeout(context.WithoutCancel(ctx), admissionBudget)
		admitErr := coord.AdmitRemovals(critA, batch)
		cancelA()
		if admitErr != nil {
			return fmt.Errorf("settings apply rejected; no changes were made: admit streamer removal(s): %w", admitErr)
		}
		slog.Info("Prepared streamer removal(s)", "count", len(batch), "channelIDs", channelIDs)
	}

	// Prefer the coordinator as the renamer: it renames analytics + notifications
	// + watch-time in ONE atomic transaction (all stores move together or none
	// do), so a successful rename leaves no old-login orphan in ANY store and a
	// failure leaves every store on the old login. Fall back to analytics-only
	// when the coordinator is not built (e.g. a unit test with no shared DB). Each
	// is assigned only when non-nil so the interface never wraps a nil pointer.
	var svc renameAnalyticsService
	switch {
	case coord != nil:
		svc = coord
	case analyticsSvc != nil:
		svc = analyticsSvc
	}
	rollback, err := commitAnalyticsRenames(plannedRenames, svc)
	if err != nil {
		m.abortAdmittedRemovals(coord, admittedLogins, "the rename transaction failed")
		return fmt.Errorf("rename transaction aborted: %w", err)
	}

	// THE COMMIT POINT (I7, fixes D7): config.SaveConfig, the m.config
	// publish, and the AutoRedeem runtime-state/generation migration all run
	// in ONE m.mu critical section — closing both the D1 lost-update window
	// (a SetAutoRedeem committing during the unlocked admission/analytics I/O
	// above must never be silently overwritten) and the concurrent-map panic
	// a rename's SaveConfig running off m.mu used to risk: candidate.DropRules
	// aliases the LIVE map (cloneConfigLocked deep-copies only AutoRedeem),
	// and SetDropRule (policy.go) writes that live map under m.mu while an
	// unlocked json.MarshalIndent of an old candidate iterates it —
	// "concurrent map iteration and map write". This is a hard constraint: a
	// candidate's config.SaveConfig must never move back off m.mu on this
	// path.
	if m.applyCommitBarrier != nil {
		m.applyCommitBarrier(applyPreCommit)
	}
	m.mu.Lock()
	clashes := m.refreshCandidateAutoRedeemLocked(candidate, plannedRenames, removedLogins)
	if configPath != "" {
		if err := config.SaveConfig(configPath, candidate); err != nil {
			m.mu.Unlock()
			rollback() // reverse the committed analytics renames (off-lock)
			m.abortAdmittedRemovals(coord, admittedLogins, "config.json persistence failed")
			return fmt.Errorf("rename transaction aborted: persisting config: %w", err)
		}
	}
	m.config = candidate
	stateClashes := migrateAutoRedeemRuntimeState(m.autoRedeemState, plannedRenames) // C4: same section as the publish
	for k := range stateClashes {
		clashes[k] = true
	}
	m.migrateAutoRedeemGenLocked(plannedRenames, clashes)
	for _, login := range removedLogins { // I4: atomic with the publish
		delete(m.autoRedeemState, login)
		m.bumpAutoRedeemGenLocked(login)
	}
	m.mu.Unlock()
	if m.applyCommitBarrier != nil {
		m.applyCommitBarrier(applyPostCommit)
	}
	if len(admittedLogins) > 0 {
		slog.Info("Streamer removal committed", "count", len(admittedLogins))
	}

	// Durable stores now agree with `candidate`. Commit the runtime and
	// publish it as the live config.
	added, removed, changed, renamed, conflicts := m.streamers.CommitPlan(plan)
	logReconcileConflicts(conflicts)

	critB, cancelB := context.WithTimeout(context.WithoutCancel(ctx), purgeBudget)
	defer cancelB()
	m.finishApply(critB, coord, candidate, added, removed, changed, renamed, true)
	return nil
}

// logReconcileConflicts emits the same summary warning
// streamer.Manager.ApplySettings always logged for each reconciliation
// conflict CommitPlan reports (duplicate settings, a login collision, or a
// C1 stored-ChannelID mismatch) — the miner's coordinator calls
// PlanReconcile/CommitPlan directly rather than through that wrapper, so it
// reproduces the summary log itself. The detailed, privacy-safe warning for
// each conflict is already emitted at detection time inside PlanReconcile;
// this is purely an additional summary line for parity.
func logReconcileConflicts(conflicts []streamer.ReconcileConflict) {
	for _, c := range conflicts {
		slog.Warn("Streamer settings not applied due to reconciliation conflict", "detail", c.Error())
	}
}

// cloneConfigLocked returns a copy of m.config safe to mutate independently
// of the live one. config.Config's only reference-typed field any pre-commit
// candidate mutation touches IN PLACE is AutoRedeem (applyConfigRenames'
// early, discarded migrateAutoRedeem pass — see that function's doc
// comment), so that map is deep-copied explicitly here; it is later REBUILT
// WHOLESALE and authoritatively from the LIVE map by
// refreshCandidateAutoRedeemLocked at the actual commit point (I1/I2,
// rewards.go), so this copy only needs to survive mutation up to that point,
// not to publish. Streamers (and every other slice ApplyToConfig touches) is
// reassigned wholesale rather than mutated in place, so a shallow struct copy
// is already independent for it.
//
// DropRules is deliberately NOT deep-copied: no candidate-config code path
// mutates it, only aliases it to the live map. [R7] This is load-bearing, not
// an oversight — SetDropRule (policy.go) writes that live map under m.mu, so
// any candidate carrying the aliased map must have its own config.SaveConfig
// run under m.mu too, or an unlocked json.MarshalIndent iterating it can race
// a concurrent write ("concurrent map iteration and map write" — a real
// panic the M2/D7 fix eliminated by moving the rename path's SaveConfig under
// m.mu; see applySettingsWithRename's commit-point comment). Never move a
// candidate's config.SaveConfig back off m.mu. Every other field is a plain
// value struct with no aliasable reference. Caller holds m.mu.
func (m *Miner) cloneConfigLocked() *config.Config {
	clone := *m.config
	if m.config.AutoRedeem != nil {
		clone.AutoRedeem = make(map[string]config.AutoRedeemConfig, len(m.config.AutoRedeem))
		for k, v := range m.config.AutoRedeem {
			clone.AutoRedeem[k] = v
		}
	}
	return &clone
}

// finishApply performs everything that must happen once the roster
// reconciliation has been committed to the runtime AND — for a rename-
// carrying apply — durable persistence has already succeeded: publish the new
// config, wire the updated settings into every dependent component,
// reconcile runtime capabilities (IRC/PubSub), reconcile the write-once
// notifications manager's Discord config (M4 — see notificationManager()/
// initNotificationManager; there is no longer a runtime create-or-rebuild
// branch here, only an UpdateDiscordConfig call against whatever the
// accessor returns), and — only for a non-rename apply (persisted=false) —
// persist config.json (non-fatal on failure, exactly as ApplySettings always
// did before this pass; a rename-carrying apply already persisted newConfig
// durably at its own commit point in applySettingsWithRename before this
// ran, so persisted=true skips a redundant save).
//
// [M2] The AutoRedeem runtime-STATE migration for a rename (BKM-006
// Corrective Pass 1, C4) now happens PRIMARILY at the rename's own commit
// point (applySettingsWithRename), in the SAME m.mu section as the config
// publish and the generation migration — so no auto-redeem poll can ever
// observe an orphaned old-login state or a fresh budget window. The
// migrateAutoRedeemRuntimeState(m.autoRedeemState, renamed) call retained
// below is an IDEMPOTENT BACKSTOP, not a second real migration: by the time
// finishApply runs, the old-login key is already gone from
// m.autoRedeemState, so this re-run is a no-op (hadOld is false for every
// rename) except for a non-rename-carrying apply, where renamed is simply
// empty. finishApply performs NO generation migration of its own [R1] — a
// second gen-migration pass here would bump PAST the already-migrated
// generation and sever the budget-window continuity
// migrateAutoRedeemGenLocked exists to preserve.
//
// [R8] This split leaves one bounded, fail-closed window: between the rename
// commit (config/state/gen already migrated to the new login) and CommitPlan
// (called by the caller just before this function) the runtime roster still
// resolves the streamer under its OLD login — so an evaluateAutoRedeem cycle
// racing that window looks up cfg.AutoRedeem under the old login (its helper
// key, s.GetUsername(), hasn't been renamed in the runtime yet) and finds
// nothing (already migrated away), so auto-redeem simply no-ops for that
// streamer until CommitPlan repoints the roster a few lines later. [RR7] This
// relies on the standing assumption that CommitPlan actually confirms the
// very rename the commit section just migrated for: a RenameIfCurrent CAS
// discard inside CommitPlan (production-unreachable, since coordinatorMu
// serializes applies end to end) would leave the migrated AutoRedeem key with
// no matching roster entry — auto-redeem silently stays off for that
// streamer — a pre-existing shape of this design, not a regression it
// introduces.
//
// ctx and coord are the SAME values the caller (applySettingsNoRename /
// applySettingsWithRemovals / applySettingsWithRename) already resolved for
// this one apply — passed through to applyStreamerDeletions rather than
// re-read from m.runCtx/m.streamerLifecycle here, so this function never
// second-guesses the caller's own resolution of either value.
func (m *Miner) finishApply(ctx context.Context, coord *streamerlifecycle.Coordinator, newConfig *config.Config, added, removed []*models.Streamer, changed []streamer.SettingsChange, renamed []streamer.RenameEvent, persisted bool) {
	m.mu.Lock()
	m.config = newConfig
	migrateAutoRedeemRuntimeState(m.autoRedeemState, renamed)
	// Best-effort backfill of ChannelID onto every entry the JUST-COMMITTED
	// roster resolved (BKM-006 C1) — e.g. a brand-new streamer added by this
	// very apply, which a rename-carrying apply's pre-commit backfill
	// (applySettingsWithRename, from the plan's own resolution) already
	// covers and this call is then a no-op for (never overwrites a non-empty
	// ChannelID). Without this, a non-rename apply (the common case: adding a
	// streamer, toggling a setting) would never persist the stored-identity
	// anchor a cold restart depends on.
	backfillChannelIDs(m.config, channelIDsByLogin(m.streamers.All()))

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

	discordCfg := m.config.Discord
	webServer := m.webServer
	wsPool := m.wsPool
	minuteWatcher := m.watcher
	dropsTracker := m.dropsTracker
	riskCfg := m.config.PredictionRisk

	m.mu.Unlock()

	// Snapshot the write-once notifications manager AFTER releasing m.mu
	// (never convert this into an in-lock read: the accessor itself only
	// ever takes a brief RLock, but UpdateDiscordConfig below performs
	// Discord Connect/Disconnect network I/O, which must never run while
	// m.mu is held, or every other m.mu reader in the miner would be
	// blocked on a Discord round trip).
	notifMgr := m.notificationManager()

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
	// previous apply left failed. renamed carries each old login so the stale
	// IRC join is explicitly left exactly once (PubSub needs no equivalent
	// action: topics are keyed by ChannelID, which a rename never changes).
	m.reconcileRuntimeCapabilities(added, removed, changed, renamed)

	// Purge every removed streamer's PERSISTED bot-owned state (analytics
	// history, notification rules/config-lists, watch-time rows) in one atomic
	// transaction, clear its streak grant, and lift the fence for any re-added
	// login — the persisted half of BKM-018A, mirroring the runtime teardown
	// above. Runs off the miner lock, on THIS apply's own captured coord/ctx.
	m.applyStreamerDeletions(ctx, coord, added, removed)

	// The durable analytics migration already ran (and, for a rename-carrying
	// apply, succeeded) before this — see commitAnalyticsRenames and
	// applySettingsWithRename's commit point. This emits the one
	// privacy-safe rename log per event (old login, new login, ChannelID
	// only — no tokens/URLs/headers/payloads).
	for _, r := range renamed {
		slog.Info("Reconciled streamer rename by stable Twitch channel ID",
			"oldLogin", r.OldLogin, "newLogin", r.NewLogin, "channelID", r.ChannelID)
	}

	// Reconcile the write-once manager's Discord settings (M4): the manager
	// itself decides whether a reconnect/disconnect is actually needed
	// (UpdateDiscordConfig's own idempotent CASE A-H reconciliation). A nil
	// notifMgr means the miner runs without a database (or the one-time
	// startup construction failed — there is no retry path for that short of
	// restarting the process), so Discord simply cannot be turned on for this
	// run and the web dashboard must never claim otherwise.
	if notifMgr != nil {
		if err := notifMgr.UpdateDiscordConfig(&discordCfg); err != nil {
			slog.Error("Failed to update Discord config", "error", err)
		}
	}

	if webServer != nil {
		webServer.SetDiscordEnabled(discordCfg.Enabled && notifMgr != nil)
	}

	if !persisted {
		m.mu.Lock()
		if m.configPath != "" {
			if err := config.SaveConfig(m.configPath, m.config); err != nil {
				slog.Error("Failed to save config", "error", err)
			} else {
				slog.Info("Settings saved to config file")
			}
		}
		m.mu.Unlock()
	}

	slog.Info("Runtime settings updated")
}
