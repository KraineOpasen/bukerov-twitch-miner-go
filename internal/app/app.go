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
// notifications, debug server, auto-update); App owns only the process-level
// resources main used to own, plus the miner's construction and lifecycle.
//
// Build performs no network I/O, starts no runtime loops, and launches no
// goroutines: authentication (a network/device-code step) and every watch /
// pubsub / IRC / HTTP-serving loop happen only under Run. This split makes
// construction, startup order, and shutdown order independently testable.
package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/miner"
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

// Options carries the process-resolved settings the composition root needs but
// does not itself discover. main resolves CLI flags and environment variables
// (auto-update on/off and its interval, the config path) and passes them in, so
// App reads no environment of its own.
type Options struct {
	// ConfigPath is the on-disk config file path, forwarded to the miner so a
	// runtime owner-identity or rename reconciliation can persist back to it.
	ConfigPath string
	// AutoUpdateEnabled and AutoUpdateInterval configure the miner's background
	// release-update watcher (resolved by main from the -auto-update flag /
	// AUTO_UPDATE and AUTO_UPDATE_CHECK_INTERVAL env vars).
	AutoUpdateEnabled  bool
	AutoUpdateInterval time.Duration
}

// runner is the runtime engine App drives. *miner.Miner satisfies it. It is an
// interface so lifecycle tests can inject a fake runtime without the miner's
// network/auth dependencies.
type runner interface {
	Run(ctx context.Context) error
}

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
	runner    runner

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
func Build(ctx context.Context, cfg *config.Config, opts Options) (*App, error) {
	return buildWith(ctx, cfg, opts, defaultFactories())
}

func buildWith(ctx context.Context, cfg *config.Config, opts Options, f factories) (a *App, err error) {
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
		// step.
		ws := f.newWeb(cfg.Analytics, cfg.Username, basePath, svc)
		app.web = ws
		app.steps = append(app.steps, lifecycleStep{
			name:  "web",
			start: fromError(ws.Start),
			stop:  fromVoid(ws.Stop),
		})
	}

	// The miner is the runtime engine. It is constructed and wired here but not
	// run; Run(ctx) performs authentication and starts every runtime loop. The
	// miner receives the App-owned database/analytics/web as external
	// dependencies (it neither opens nor closes them — App does), so ownership
	// is single and unambiguous.
	m := f.newMiner(cfg, opts.ConfigPath)
	m.ConfigureAutoUpdate(opts.AutoUpdateEnabled, opts.AutoUpdateInterval)
	m.SetDatabase(db)
	if app.analytics != nil {
		m.SetAnalyticsService(app.analytics)
	}
	if app.web != nil {
		m.SetWebServer(app.web)
	}
	app.runner = m

	return app, nil
}

// Run starts the process-level owned resources in construction order (only the
// web server has a start step) and then runs the miner, which authenticates and
// drives every runtime loop until ctx is cancelled. Run blocks until the miner
// returns (on ctx cancellation or a fatal runtime error).
//
// A start-step failure (e.g. the web server refusing an insecure bind) unwinds
// every resource opened so far, in reverse order, before returning the wrapped
// error — so a failed startup leaves nothing open. Run is single-use: a second
// call returns ErrAlreadyRun, and a call after Shutdown returns ErrShutDown.
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

	if a.runner == nil {
		return nil
	}
	return a.runner.Run(ctx)
}

// Shutdown stops the process-level owned resources in reverse construction order
// (web server, then analytics service, then database — the database last, after
// every writer has stopped), aggregating errors without hiding any. It is
// idempotent and safe under concurrent callers: the teardown runs exactly once
// and every caller observes the same aggregated error. Calling Shutdown before
// Run is safe — it simply closes whatever Build opened.
//
// By the time Shutdown runs on the normal path, Run has returned, which means
// the miner has already torn down its own runtime graph (its Run cancels on ctx
// and calls stop()); Shutdown then closes the resources the miner does not own.
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
