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
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
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

	// runCancel cancels the ctx App.Run hands to controller.Run (task
	// contract D6/design v6 I31: cancelling the controller's parent ctx is
	// the documented process-shutdown exit path). Set once, right before
	// controller.Run is invoked; read by the degraded "Restart process"
	// requester wired to web in buildWith. nil until Run actually reaches
	// that point (there is no controller, or Run hasn't been called yet).
	runCancelMu sync.Mutex
	runCancel   context.CancelFunc
	// restartProcessOnce makes repeated "Restart process" presses (or a slow
	// double click before the panel disables itself) a safe no-op — only
	// the first call ever cancels runCancel.
	restartProcessOnce sync.Once

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

	// The durable updater-handoff store (design Ф5a1) registers its own
	// migration against the SAME db, right alongside the lifecycle store -
	// same partial-build cleanup, same "database" step already tracked.
	updStore, uerr := updater.NewStore(db)
	if uerr != nil {
		return nil, fmt.Errorf("app: build updater store: %w", uerr)
	}

	// Boot-time handoff consumption (design Ф5a1): reads-and-deletes any
	// durable apply-handoff row left by the PREVIOUS process generation,
	// strictly BEFORE the lifecycle controller (and therefore any new
	// updater cycle that could write a fresh row) is constructed below - see
	// Store.ConsumeHandoff's own doc comment for why that ordering makes
	// this race-free. A read/consume failure is best-effort: it is logged
	// and Build proceeds exactly as if no row had been found, since a
	// degraded boot-time classification must never be the reason the whole
	// process fails to start.
	//
	// seedUpdaterApplied is nil unless classification is BootSucceeded, in
	// which case it captures the (from, to) pair to seed into the updater's
	// Snapshot once that updater is constructed further down this function -
	// upd does not exist yet at this point.
	var seedUpdaterApplied func(upd *updater.Updater)
	if rec, found, cerr := updStore.ConsumeHandoff(ctx); cerr != nil {
		slog.Warn("app: failed to consume updater handoff at boot (best-effort)", "error", cerr)
	} else if found {
		switch updater.ClassifyBoot(rec, version.Version) {
		case updater.BootSucceeded:
			events.Record(events.TypeUpdateSucceeded, "", rec.FromVersion+" -> "+rec.ToVersion)
			slog.Info("app: self-update applied successfully across restart",
				"from", rec.FromVersion, "to", rec.ToVersion, "url", rec.ReleaseURL)
			seedUpdaterApplied = func(upd *updater.Updater) {
				// rec.UpdatedAt (the durable row's own timestamp) IS the apply
				// time — using time.Now() here instead would misreport it by
				// the restart gap (however long the process took to come back
				// up), which can be seconds under a supervisor or much longer
				// under a manual restart.
				upd.SeedApplied(rec.FromVersion, rec.ToVersion, rec.UpdatedAt)
			}
		case updater.BootInterrupted:
			events.Record(events.TypeUpdateFailed, "",
				fmt.Sprintf("%s -> %s: update interrupted mid-apply (process restarted before the swap completed)",
					rec.FromVersion, rec.ToVersion))
			slog.Warn("app: durable updater handoff shows an interrupted apply",
				"from", rec.FromVersion, "to", rec.ToVersion, "phase", rec.Phase)
		case updater.BootNotEffective:
			events.Record(events.TypeUpdateFailed, "",
				fmt.Sprintf("%s -> %s: update reported success but the swap did not take effect",
					rec.FromVersion, rec.ToVersion))
			slog.Warn("app: durable updater handoff shows a swap that did not take effect",
				"from", rec.FromVersion, "to", rec.ToVersion, "phase", rec.Phase)
		case updater.BootAnomalous:
			// Neither FromVersion nor ToVersion matches the running binary -
			// this package's own boot classification cannot explain it, so no
			// ring event is recorded (it would mislead more than help);
			// manual investigation is the only sound next step.
			slog.Warn("app: anomalous durable updater handoff at boot; manual investigation may be required",
				"from", rec.FromVersion, "to", rec.ToVersion, "phase", rec.Phase, "running", version.Version)
		}
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
	// updateState is Ф4c's best-effort lifecycle.updateState source (task
	// contract D5): internal/updater keeps no exported state of its own, so
	// this additively wraps the SAME Notify/NotifyFailure/OnUpdate closures
	// below to record into an app-owned cell, exposed to web read-only via
	// SetLifecycleUpdateState below. Deliberately tiny: "" (nothing observed
	// yet) | "available" | "failed" | "applied".
	var controller *lifecycle.Controller
	updateState := &lifecycleUpdateStateCell{}

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

	// upd is now constructed HERE, at buildWith level (design Ф5a1) rather
	// than lazily inside updaterRun as before: seedUpdaterApplied (set above,
	// during boot-handoff consumption) needs a live *updater.Updater to seed
	// BEFORE the lifecycle controller ever starts the updater goroutine, and
	// controller.Run — the only place that goroutine gets started — happens
	// later, under App.Run, long after Build (and this whole function) has
	// returned. Gate MUST be a call-time closure, `func() bool { return
	// controller.UpdaterGate() }`, NOT the hoisted method value
	// `controller.UpdaterGate`: a method value binds its receiver at the
	// moment the expression is evaluated, and at this point in the function
	// `controller` is still nil (declared above, assigned only below) — a
	// bound-to-nil method value would panic the first time Gate() is ever
	// called. The closure instead captures the `controller` VARIABLE, which
	// Go closures always do by reference, so by the time Gate() is actually
	// invoked (inside a running updater cycle, always after controller has
	// been assigned) the read observes the real controller.
	upd := updater.New(updater.Options{
		Repo:           version.Repo,
		CurrentVersion: version.Version,
		ReleaseChannel: version.Channel,
		StableCacheDir: updater.DefaultStableCacheDir,
		Enabled:        rc.AutoUpdateEnabled,
		CheckInterval:  rc.AutoUpdateInterval,
		Handoff:        updStore,
		Gate:           func() bool { return controller.UpdaterGate() },
		OnUpdate: func() {
			updateState.set("applied", version.Version)
			controller.UpdateApplied()
		},
		Notify: func(cur, latest, releaseURL string) {
			updateState.set("available", latest)
			notifyAvailable(cur, latest, releaseURL)
		},
		NotifyFailure: func(cur, latest, reason string) {
			updateState.set("failed", latest)
			notifyFailed(cur, latest, reason)
		},
	})
	if seedUpdaterApplied != nil {
		seedUpdaterApplied(upd)
	}
	updaterRun := func(uctx context.Context) { upd.Run(uctx) }

	controller = lifecycle.New(lifecycle.Config{
		Factory: func() lifecycle.Runner {
			return app.minerFactory()
		},
		// Ф4c (task contract D4): the persistence decorator additively
		// tags a Save failure with web.ErrLifecyclePersist so the web
		// handler can discriminate 500-class persist failures from
		// 409-class table/slot rejections via errors.Is, without
		// string-matching lifecycle's rejection prose.
		Persistence:      lifecyclePersistenceDecorator{inner: store},
		StatusSink:       sink,
		ForceRunning:     rc.LifecycleForceRunning,
		NoControlSurface: app.web == nil,
		UpdaterRun:       updaterRun,
	})
	app.controller = controller

	// Wire the lifecycle command/snapshot seam, the best-effort updater
	// state, and the degraded "Restart process" action into web (task
	// contract D1/D5/D6) — additive, right after the controller exists;
	// nil app.web (EnableAnalytics=false) simply means none of this is
	// wired, exactly like every other web-only provider above.
	if app.web != nil {
		app.web.SetLifecycleController(controller)
		app.web.SetLifecycleUpdateState(updateState.get)
		app.web.SetProcessRestartRequester(app.requestProcessRestart)
	}

	return app, nil
}

// lifecyclePersistenceDecorator wraps the lifecycle.Store this package
// already builds for lifecycle.Config.Persistence (task contract D4): its
// Save additively tags any error with web.ErrLifecyclePersist via
// errors.Join (preserving the original chain), so internal/web can tell a
// 500-class persistence failure apart from a 409-class table/slot rejection
// with errors.Is(res.Err, web.ErrLifecyclePersist) — lifecycle.Submit wraps
// Store errors with %w, so the sentinel survives that wrap unchanged. Load
// passes straight through; only Save (the write path §5.2 discriminates on)
// is decorated.
type lifecyclePersistenceDecorator struct {
	inner lifecycle.Persistence
}

func (d lifecyclePersistenceDecorator) Load(ctx context.Context) (lifecycle.LoadResult, error) {
	return d.inner.Load(ctx)
}

func (d lifecyclePersistenceDecorator) Save(ctx context.Context, desired lifecycle.DesiredState, reason, commandID string) error {
	if err := d.inner.Save(ctx, desired, reason, commandID); err != nil {
		return errors.Join(web.ErrLifecyclePersist, err)
	}
	return nil
}

// lifecycleUpdateStateCell is the app-owned, mutex-guarded cell behind
// web.SetLifecycleUpdateState (task contract D5) — see the Options.OnUpdate/
// Notify/NotifyFailure closures in buildWith above for where it's written;
// read through get, wired directly as the closure web calls on every GET
// /api/lifecycle and panel poll.
type lifecycleUpdateStateCell struct {
	mu    sync.Mutex
	state web.LifecycleUpdateState
}

func (c *lifecycleUpdateStateCell) set(state, ver string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = web.LifecycleUpdateState{State: state, Version: ver}
}

func (c *lifecycleUpdateStateCell) get() web.LifecycleUpdateState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
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
// hardening — THE WHITELIST RULE, extended in Ф4c per §14's "Ф4c расширяет
// ТОЛЬКО адаптер и web-слой"): it publishes ONLY existing web.MinerStatus
// values, and ONLY for the lifecycle statuses the miner itself could never
// explain on its own.
//
// Mapping table (every lifecycle status NOT listed here is dropped, logged
// at debug):
//   - "paused" -> web.StatusPaused, "stopped" -> web.StatusStopped,
//     "restarting" -> web.StatusRestarting: message is passed through
//     verbatim, WHETHER this is the once-per-boot boot-honored-intent call
//     (lifecycle.BootHonoredIntentMessage) or an ordinary runtime
//     pause/stop/restart an operator just issued — Ф4b's special-cased
//     overlay text for the boot-honored case is gone: the lifecycle panel
//     (Ф4c, driven off GET /api/lifecycle) is now the single honest surface
//     for every paused/stopped/restarting state, boot-honored or not, so
//     there is no longer a reason to distinguish them here.
//   - "failed" -> web.StatusFailed, "degraded" -> web.StatusDegraded: message
//     is the lifecycle LastError (a startup failure sitting in the
//     retry-backoff window, or a dirty teardown while desired stayed
//     paused/stopped).
//
// Every OTHER lifecycle status (starting/running/pausing/stopping/exiting)
// is intentionally NOT published: the miner ITSELF already drives this SAME
// broadcaster through its own startup progression (initializing ->
// auth_required/auth_waiting -> loading_streamers -> running) once its
// generation actually starts running — publishing "running" from here at
// launch would prematurely clear that in-progress overlay before the miner
// has actually finished starting, and publishing the other transitional
// states would only flicker the (Ф4c-forbidden, design v6 §10 rule 3)
// full-screen overlay for a lifecycle transition; the lifecycle panel covers
// those via its own 2s poll instead. This adapter only ever fills the gaps
// the miner itself can never report.
type lifecycleStatusAdapter struct {
	broadcaster *web.StatusBroadcaster
}

func (a lifecycleStatusAdapter) SetStatus(status, message string) {
	switch status {
	case "paused":
		a.broadcaster.SetStatus(web.StatusPaused, message)
	case "stopped":
		a.broadcaster.SetStatus(web.StatusStopped, message)
	case "restarting":
		a.broadcaster.SetStatus(web.StatusRestarting, message)
	case "failed":
		a.broadcaster.SetStatus(web.StatusFailed, message)
	case "degraded":
		a.broadcaster.SetStatus(web.StatusDegraded, message)
	default:
		slog.Debug("app: lifecycle status has no web.MinerStatus mapping, dropped", "status", status)
	}
}

// SetGeneration forwards the lifecycle generation token to the broadcaster
// (design v6 §10: the client-visible discriminator that tells the overlay/
// reload logic whether a startup-phase status belongs to the first boot).
func (a lifecycleStatusAdapter) SetGeneration(gen uint64) {
	a.broadcaster.SetGeneration(gen)
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

	// Ф4c (task contract D6): derive a cancelable ctx for the controller's
	// own run scope so the degraded "Restart process" action (web,
	// SetProcessRestartRequester) has a documented, I31-compliant exit path
	// — cancelling THIS ctx, not os.Exit — available to it. An ordinary
	// SIGINT/SIGTERM still works identically: cancelling the ctx passed to
	// Run cancels runCtx too (context.WithCancel derives from it), so no
	// existing shutdown behavior changes.
	runCtx, cancel := context.WithCancel(ctx)
	a.runCancelMu.Lock()
	a.runCancel = cancel
	a.runCancelMu.Unlock()
	defer cancel()

	return a.controller.Run(runCtx)
}

// requestProcessRestart is wired to web.Server.SetProcessRestartRequester
// (task contract D6): cancelling a.runCancel is the documented
// process-shutdown path (design v6 I31) — teardown -> App.Run returns ->
// main performs the full Shutdown -> process exits, and (under a restart
// policy) the supervisor brings the container back. One-shot: only the
// first call has any effect. A nil runCancel (Run hasn't reached that point
// yet, or there is no controller at all) is a safe no-op — web only wires
// this action visibly once a real degraded snapshot exists, which cannot
// happen before Run has started the controller.
func (a *App) requestProcessRestart() {
	a.restartProcessOnce.Do(func() {
		events.Record(events.TypeLifecycleRestartProcessRequested, "", "operator requested a process restart while degraded")
		slog.Warn("app: process restart requested (degraded) — cancelling the lifecycle run scope for a clean exit")
		a.runCancelMu.Lock()
		cancel := a.runCancel
		a.runCancelMu.Unlock()
		if cancel != nil {
			cancel()
		}
	})
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
