// Package app is the explicit application composition root.
//
// Before BKM-019, process wiring was split between cmd/miner/main.go (which
// opened the database, built the analytics service and web server, and started
// the web listener) and internal/miner (which — via SetDatabase /
// SetAnalyticsService / SetWebServer — received some of those and, in a fallback
// library path, built its own). Ownership of Close/Stop was scattered: the web
// server was stopped by BOTH miner.stop() and a main defer (a double-close that
// only worked because Stop is idempotent), the analytics service was closed by
// the miner even though main created it, and the database used an ad-hoc ownsDB
// flag to decide who closed it.
//
// App centralizes that composition. It Builds the process-level owned resources
// (database, analytics service, web server) and the miner in one deterministic
// order, Runs them, and Shuts them down in the reverse order with a single,
// idempotent, error-aggregating path. The miner remains the runtime engine that
// owns its own internal graph (pubsub, chat, watcher, drops, discovery, health,
// notifications, debug server); App owns the process-level resources main used
// to own, the miner's construction, AND — since design v6/contract §11 — the
// durable lifecycle controller that decides WHEN a miner generation runs at
// all: App.Run drives a *lifecycle.Controller instead of a single pre-built
// runner, and the controller drives zero-or-more Miner "generations" (built
// fresh, one per generation, via App's own miner factory) over the process's
// lifetime — pause/resume/restart tear down and rebuild the generation without
// App's own owned resources (db/analytics/web) ever being touched.
//
// Build performs no network I/O, starts no runtime loops, and launches no
// goroutines: authentication (a network/device-code step) and every watch /
// pubsub / IRC / HTTP-serving loop happen only under Run (inside a generation
// the controller starts). This split makes construction, startup order, and
// shutdown order independently testable.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/miner"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/updater"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/version"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

// DefaultShutdownTimeout is the deadline main puts on graceful shutdown of the
// process-level resources. The underlying Stop/Close calls are all prompt and
// non-blocking (the web/debug servers close their listeners immediately,
// analytics.Close is a no-op, and database.Close closes the single shared
// handle) and take no context, so this deadline is surfaced in the aggregated
// Shutdown error if a stop ever overran it — it reports lateness rather than
// interrupting a stall. In practice it is never reached.
const DefaultShutdownTimeout = 30 * time.Second

var (
	// ErrAlreadyRun is returned by Run when Run has already been called on this
	// App. A composition root is single-use; nothing starts twice.
	ErrAlreadyRun = errors.New("app: Run already called")

	// ErrShutDown is returned by Run when it is called after Shutdown has
	// begun. Once an App has been torn down it cannot be restarted.
	ErrShutDown = errors.New("app: Run called after Shutdown")
)

// lifecycleStep is one process-level owned resource. start is nil for resources
// that are fully live once constructed (the database and analytics service);
// stop is the resource's Close/Stop. Steps are started in slice order under Run
// and stopped in reverse order under Shutdown, giving a single deterministic
// ordering derived from construction order.
type lifecycleStep struct {
	name  string
	start func(context.Context) error
	stop  func(context.Context) error
}

// App is the explicit application composition root. Construct it with Build,
// drive it with Run, tear it down with Shutdown.
type App struct {
	cfg       *config.Config
	db        *database.DB
	analytics *analytics.Service
	web       *web.Server

	// minerFactory builds AND wires a fresh *miner.Miner exactly as Build's
	// old single pre-built runner used to (SetDashboardConfig/SetDatabase/
	// SetAnalyticsService/SetWebServer) — but is now called once PER
	// GENERATION (design v6 §14/contract §11 item 10) by the lifecycle
	// controller's Factory, instead of once per process. Set in Build; nil
	// only for an App built directly as a struct literal in a test that
	// wires its own controller (see lifecycle_test.go).
	minerFactory func() *miner.Miner

	// currentMinerMu/currentMiner track the miner instance the CURRENTLY (or
	// most recently) built generation owns, so the process-level updater's
	// notify adapter (contract §11 item 13) can resolve "the live
	// generation's notifications manager, else nil" without the updater
	// loop — which outlives any single generation — having any generation
	// concept of its own. Set by minerFactory itself, BEFORE it returns the
	// miner, under currentMinerMu; read the same way by the updater's
	// accessor closure. Never cleared on teardown: a stale-but-stopped
	// miner's NotificationManager() is still safe to call (Stop closes
	// dispatch admission), and the NEXT generation's factory call overwrites
	// it before that generation's Run ever starts.
	currentMinerMu sync.Mutex
	currentMiner   *miner.Miner

	// controller is the durable lifecycle core App.Run drives instead of a
	// single pre-built runner (contract §11 items 10-12): it owns deciding
	// WHEN a miner generation runs (persisted desired state, pause/resume/
	// restart, the process-level auto-updater's exit request) and tears
	// down/rebuilds generations via minerFactory. nil only in a test App
	// that never calls Build.
	controller *lifecycle.Controller

	// steps are the process-level owned resources in construction order:
	// database, then (optionally) analytics service, then (optionally) web
	// server. Run starts them in order; Shutdown stops them in reverse.
	steps []lifecycleStep

	started         atomic.Bool
	shutdownStarted atomic.Bool
	shutdownOnce    sync.Once
	shutdownErr     error
}

// factories are the constructor seams App composes. Production uses
// defaultFactories(); tests override the fallible constructors (openDB,
// newAnalytics) to exercise partial-build cleanup without real failures. The
// non-fallible constructors (web server, miner) are called directly in Build.
type factories struct {
	openDB       func(basePath string) (*database.DB, error)
	newAnalytics func(db *database.DB, basePath string, retentionDays int) (*analytics.Service, error)
	newWeb       func(analyticsSettings config.AnalyticsSettings, username, basePath string, svc *analytics.Service) *web.Server
	newMiner     func(cfg *config.Config, configPath string) *miner.Miner
}

func defaultFactories() factories {
	return factories{
		openDB:       database.Open,
		newAnalytics: analytics.NewService,
		newWeb:       web.NewServerEarly,
		newMiner:     miner.New,
	}
}

// Build constructs the process-level owned resources and the miner, wires the
// miner to those resources, and returns a ready-to-Run App. It validates the
// config, opens the database, runs its migrations (inside analytics/miner
// construction, as today), builds the analytics service and web server when
// analytics is enabled, and constructs the miner — but starts nothing: no
// network I/O, no serving goroutine, no watch/pubsub/IRC loop.
//
// On any failure after a resource has been opened, Build unwinds the resources
// it already opened in reverse order (so a database opened before a failing
// analytics migration is closed exactly once) and returns the wrapped error.
//
// The context is accepted for symmetry with Run/Shutdown and reserved for
// future ctx-aware construction; Build's current work is entirely local and
// non-blocking, so it completes without observing cancellation.
func Build(ctx context.Context, cfg *config.Config, rc runtimeconfig.RuntimeConfig) (*App, error) {
	return buildWith(ctx, cfg, rc, defaultFactories())
}

func buildWith(ctx context.Context, cfg *config.Config, rc runtimeconfig.RuntimeConfig, f factories) (a *App, err error) {
	if verr := validateConfig(cfg); verr != nil {
		return nil, verr
	}

	app := &App{cfg: cfg}

	// Partial-build cleanup: if Build returns an error after opening one or
	// more resources, stop them in reverse construction order, exactly once.
	defer func() {
		if err != nil {
			_ = app.stopSteps(context.Background(), len(app.steps))
			app.steps = nil
		}
	}()

	// Storage (database/cookies/logs) is keyed by the STABLE profile key, not
	// the mutable canonical Twitch login (BKM-006 COR-2), so a renamed owner
	// keeps loading the same history. StorageKey() == Username until the first
	// rename pins ProfileKey.
	basePath := filepath.Join("database", cfg.StorageKey())

	// The database is owned regardless of EnableAnalytics (notification rules,
	// watch-time fairness, drops catalog, streamer-deletion ledger all persist
	// here), so it is opened first and closed last.
	db, err := f.openDB(basePath)
	if err != nil {
		return nil, fmt.Errorf("app: open database: %w", err)
	}
	app.db = db
	app.steps = append(app.steps, lifecycleStep{
		name: "database",
		stop: closer(db.Close),
	})

	// The lifecycle store's migration registers against db right after it
	// opens (contract §11 item 11): a failure here unwinds through the SAME
	// partial-build cleanup as every other step above, since "database" is
	// already tracked in app.steps by this point.
	store, serr := lifecycle.NewStore(db)
	if serr != nil {
		return nil, fmt.Errorf("app: build lifecycle store: %w", serr)
	}

	if cfg.EnableAnalytics {
		svc, aerr := f.newAnalytics(db, basePath, cfg.Analytics.RetentionDays)
		if aerr != nil {
			return nil, fmt.Errorf("app: build analytics service: %w", aerr)
		}
		app.analytics = svc
		app.steps = append(app.steps, lifecycleStep{
			name: "analytics",
			stop: closer(svc.Close),
		})

		// The web server is built "early" (no streamer roster yet) so its
		// loading/auth-status overlay is live during the device-code flow the
		// miner runs under Run. It is constructed here but NOT started: Start
		// (which binds the listener and spawns the serving goroutine) is a Run
		// step. The immutable dashboard exposure/auth snapshot (resolved once at
		// the cmd/miner bootstrap) is injected so the web layer reads no env.
		ws := f.newWeb(cfg.Analytics, cfg.Username, basePath, svc)
		ws.SetDashboardConfig(rc.Dashboard)
		app.web = ws
		app.steps = append(app.steps, lifecycleStep{
			name:  "web",
			start: fromError(ws.Start),
			stop:  fromVoid(ws.Stop),
		})
	}

	// The miner is the runtime engine — but, since design v6/contract §11,
	// App no longer builds a single one: it builds a FACTORY that the
	// lifecycle controller calls once per generation. Each fresh miner is
	// constructed and wired here but not run; the generation's own Run(ctx)
	// (invoked by the controller) performs authentication and starts every
	// runtime loop. The miner receives the SAME App-owned database/analytics/
	// web as external dependencies on every generation (it neither opens nor
	// closes them — App does), so ownership is single and unambiguous no
	// matter how many generations come and go.
	app.minerFactory = func() *miner.Miner {
		m := f.newMiner(cfg, rc.ConfigPath)
		// The miner's fallback web build (used only when App does not inject
		// a web server) reads the same immutable dashboard snapshot rather
		// than the env.
		m.SetDashboardConfig(rc.Dashboard)
		m.SetDatabase(db)
		if app.analytics != nil {
			m.SetAnalyticsService(app.analytics)
		}
		if app.web != nil {
			m.SetWebServer(app.web)
		}
		// Published BEFORE returning (contract §11 item 10): the process-
		// level updater's notify accessor (item 13) reads this under
		// currentMinerMu to resolve "the live generation's notifications
		// manager, else nil", and must never observe a half-wired miner.
		app.currentMinerMu.Lock()
		app.currentMiner = m
		app.currentMinerMu.Unlock()
		return m
	}

	// Wire the dirty-teardown classifier ONCE per process (contract §11 item
	// 5): internal/lifecycle stays Twitch/Miner-agnostic, so this is the one
	// integration point where both packages are visible together.
	wireDirtyTeardownClassifier()

	// The web status adapter (contract §11 item 14) is nil when there is no
	// web server at all — lifecycle.New defaults a nil StatusSink to its own
	// no-op sink, so this stays a plain, honest nil rather than a manually
	// constructed no-op wrapper.
	var sink lifecycle.StatusSink
	if app.web != nil {
		sink = lifecycleStatusAdapter{broadcaster: app.web.GetStatusBroadcaster()}
	}

	// The process-level updater loop (contract §11 item 13, design v6 §7):
	// built as a closure captured over the (not-yet-assigned) controller
	// variable below — it only ever RUNS inside controller.Run, by which
	// point controller is already assigned, so this is safe despite the
	// apparent forward reference (a Go closure captures the variable, not
	// its value at closure-creation time).
	var controller *lifecycle.Controller
	updaterRun := func(uctx context.Context) {
		currentManager := func() *notifications.Manager {
			app.currentMinerMu.Lock()
			m := app.currentMiner
			app.currentMinerMu.Unlock()
			if m == nil {
				return nil
			}
			return m.NotificationManager()
		}
		notifyAvailable, notifyFailed := miner.UpdateNotifyFuncs(currentManager)
		upd := updater.New(updater.Options{
			Repo:           version.Repo,
			CurrentVersion: version.Version,
			Enabled:        rc.AutoUpdateEnabled,
			CheckInterval:  rc.AutoUpdateInterval,
			Gate:           controller.UpdaterGate,
			OnUpdate:       controller.UpdateApplied,
			Notify:         notifyAvailable,
			NotifyFailure:  notifyFailed,
		})
		upd.Run(uctx)
	}

	controller = lifecycle.New(lifecycle.Config{
		Factory: func() lifecycle.Runner {
			return app.minerFactory()
		},
		Persistence:      store,
		StatusSink:       sink,
		ForceRunning:     rc.LifecycleForceRunning,
		NoControlSurface: app.web == nil,
		UpdaterRun:       updaterRun,
	})
	app.controller = controller

	return app, nil
}

// wireDirtyTeardownClassifierOnce guards wireDirtyTeardownClassifier so
// repeated Builds within one process (every test binary; a real process only
// ever Builds once) install the classifier exactly once.
var wireDirtyTeardownClassifierOnce sync.Once

// wireDirtyTeardownClassifier installs the real "dirty teardown" (join-
// timeout class) recognizer into internal/lifecycle's package-level seam
// (contract §11 item 5): internal/lifecycle must never import internal/miner
// directly (it stays Twitch/Miner-agnostic by design — see its package doc),
// so this integration layer is the one place both packages are visible at
// once. Recognizes BOTH lifecycle.ErrDirtyTeardown (internal/lifecycle's own
// test sentinel — keeping that package's tests working unmodified) and
// miner.IsJoinTimeoutError (the real production class this wiring exists
// for: a shutdown-drain overrun surfacing through Miner.Run's returned
// error). Assigned exactly once per process, not per Build call.
func wireDirtyTeardownClassifier() {
	wireDirtyTeardownClassifierOnce.Do(func() {
		lifecycle.IsDirtyTeardownError = func(err error) bool {
			return errors.Is(err, lifecycle.ErrDirtyTeardown) || miner.IsJoinTimeoutError(err)
		}
	})
}

// lifecycleStatusAdapter implements lifecycle.StatusSink over web.Server's
// existing status broadcaster (contract §11 item 14, design v6 §14
// hardening — THE WHITELIST RULE): it publishes ONLY existing
// web.MinerStatus values, and ONLY for the lifecycle statuses the miner
// itself could never explain on its own.
//
// Mapping table (every lifecycle status NOT listed here is dropped, logged
// at debug):
//   - "paused" / "stopped" WITH message == lifecycle.BootHonoredIntentMessage
//     -> web.StatusError, with the exact design v6 §5.4 message: a
//     boot-honored persisted lifecycle intent — otherwise an operator
//     staring at a dashboard stuck on the loading overlay would have no idea
//     the miner never started on purpose. Any OTHER paused/stopped call
//     (MINOR 13, F4b Q3 consolidated corrective) — e.g. an ordinary runtime
//     pause/stop an operator just issued, which publishTerminal routes
//     through this exact same SetStatus method — is dropped like any other
//     unmapped status: only the boot-reconciliation call is "the miner never
//     started and nothing else will ever explain why"; an ordinary runtime
//     transition is communicated through the lifecycle command surface
//     instead, not this fixed overlay message.
//   - "failed" -> web.StatusError, with the lifecycle LastError message (a
//     startup failure sitting in the retry-backoff window between attempts).
//   - "degraded" -> web.StatusError, with the lifecycle LastError message (a
//     dirty teardown while desired stayed paused/stopped).
//
// Every OTHER lifecycle status (starting/running/pausing/stopping/
// restarting/exiting) is intentionally NOT published: the miner ITSELF
// already drives this SAME broadcaster through its own startup progression
// (initializing -> auth_required/auth_waiting -> loading_streamers ->
// running) once its generation actually starts running — publishing
// "running" from here at launch would prematurely clear that in-progress
// overlay before the miner has actually finished starting. This adapter
// only ever fills the gaps the miner itself can never report.
type lifecycleStatusAdapter struct {
	broadcaster *web.StatusBroadcaster
}

// lifecycleHonoredIntentMessage is design v6 §5.4's exact operator-facing
// message for a boot-honored paused/stopped persisted intent.
const lifecycleHonoredIntentMessage = "miner paused/stopped by persisted lifecycle intent; resume: LIFECYCLE_FORCE_RUNNING=true (или дождитесь релиза с dashboard-контролами)"

func (a lifecycleStatusAdapter) SetStatus(status, message string) {
	switch status {
	case "paused", "stopped":
		if message != lifecycle.BootHonoredIntentMessage {
			// MINOR 13: an ordinary runtime paused/stopped — not the
			// boot-reconciliation call — must never surface as the fixed
			// honored-intent overlay.
			slog.Debug("app: ordinary runtime paused/stopped status dropped (not the boot-honored marker)", "status", status)
			return
		}
		a.broadcaster.SetStatus(web.StatusError, lifecycleHonoredIntentMessage)
	case "failed", "degraded":
		a.broadcaster.SetStatus(web.StatusError, message)
	default:
		slog.Debug("app: lifecycle status has no web.MinerStatus mapping, dropped", "status", status)
	}
}

// SetGeneration is a no-op in Ф4b: status.go has no generation field until
// Ф4c. Logged at debug so the call is at least visible while wiring this up.
func (a lifecycleStatusAdapter) SetGeneration(gen uint64) {
	slog.Debug("app: lifecycle SetGeneration is a no-op in Ф4b (no generation field in status.go yet)", "generation", gen)
}

// Run starts the process-level owned resources in construction order (only the
// web server has a start step) and then runs the lifecycle controller instead
// of a single pre-built runner (contract §11 item 12): the controller performs
// startup reconciliation (persisted desired state), then drives zero or more
// Miner generations — built by App's own factory — over the rest of the
// process's life, reacting to pause/resume/restart commands and the
// process-level auto-updater's exit request. Run blocks until the controller
// returns (process-shutdown via ctx cancellation, an applied update, or a
// dirty teardown while desired stayed running).
//
// A start-step failure (e.g. the web server refusing an insecure bind) unwinds
// every resource opened so far, in reverse order, before returning the wrapped
// error — so a failed startup leaves nothing open, and the controller never
// even starts. Run is single-use: a second call returns ErrAlreadyRun, and a
// call after Shutdown returns ErrShutDown.
func (a *App) Run(ctx context.Context) error {
	if a.shutdownStarted.Load() {
		return ErrShutDown
	}
	if !a.started.CompareAndSwap(false, true) {
		return ErrAlreadyRun
	}

	for i := range a.steps {
		step := a.steps[i]
		if step.start == nil {
			continue
		}
		if serr := step.start(ctx); serr != nil {
			wrapped := fmt.Errorf("app: start %s: %w", step.name, serr)
			// Unwind everything opened/started so far, in reverse order,
			// through the single idempotent shutdown path.
			_ = a.Shutdown(context.Background())
			return wrapped
		}
	}

	if a.controller == nil {
		return nil
	}
	return a.controller.Run(ctx)
}

// Shutdown stops the process-level owned resources in reverse construction order
// (web server, then analytics service, then database — the database last, after
// every writer has stopped), aggregating errors without hiding any. It is
// idempotent and safe under concurrent callers: the teardown runs exactly once
// and every caller observes the same aggregated error. Calling Shutdown before
// Run is safe — it simply closes whatever Build opened.
//
// By the time Shutdown runs on the normal path, Run has returned, which means
// the lifecycle controller has already returned — and, on every one of its own
// exit paths, already joined the process-level updater loop and torn down
// whatever miner generation was current (each generation's own Run cancels on
// its ctx and calls stop()) — before Run itself returned; Shutdown then closes
// the resources neither the controller nor any generation owns.
func (a *App) Shutdown(ctx context.Context) error {
	a.shutdownStarted.Store(true)
	a.shutdownOnce.Do(func() {
		a.shutdownErr = a.stopSteps(ctx, len(a.steps))
	})
	return a.shutdownErr
}

// stopSteps stops the first n steps in reverse order, aggregating errors. It is
// the shared teardown used by both Shutdown and partial-build cleanup. If ctx is
// already past its deadline it is surfaced in the aggregate, but every step's
// stop is still attempted so no resource is leaked.
func (a *App) stopSteps(ctx context.Context, n int) error {
	var errs []error
	for i := n - 1; i >= 0; i-- {
		step := a.steps[i]
		if step.stop == nil {
			continue
		}
		if serr := step.stop(ctx); serr != nil {
			errs = append(errs, fmt.Errorf("app: stop %s: %w", step.name, serr))
		}
	}
	if ctx != nil && ctx.Err() != nil {
		errs = append(errs, ctx.Err())
	}
	return errors.Join(errs...)
}

// validateConfig re-checks the invariants main already guards, so Build is
// self-defending for direct (library/test) callers.
func validateConfig(cfg *config.Config) error {
	if cfg == nil {
		return errors.New("app: nil config")
	}
	if cfg.Username == "" {
		return errors.New("app: username is required in configuration")
	}
	if len(cfg.Streamers) == 0 {
		return errors.New("app: at least one streamer is required in configuration")
	}
	return nil
}

// closer adapts a func() error (e.g. db.Close, analytics.Service.Close) to a
// lifecycle stop.
func closer(fn func() error) func(context.Context) error {
	return func(context.Context) error { return fn() }
}

// fromError adapts a func() error (e.g. web.Server.Start) to a lifecycle
// start/stop.
func fromError(fn func() error) func(context.Context) error {
	return func(context.Context) error { return fn() }
}

// fromVoid adapts a func() (e.g. web.Server.Stop) to a lifecycle stop.
func fromVoid(fn func()) func(context.Context) error {
	return func(context.Context) error {
		fn()
		return nil
	}
}
