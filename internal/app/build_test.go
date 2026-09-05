package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/miner"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"

	_ "modernc.org/sqlite"
)

// These tests build a real App over temporary directories and a FRESH,
// non-singleton *database.DB per Build (via the injectable factories), so many
// independent Builds can run in one test binary — the production
// database.Open singleton would otherwise return the first handle to every
// caller. Run is never exercised here (it performs device-code OAuth); Build
// and Shutdown are.

// freshDB opens an isolated on-disk SQLite handle under t.TempDir(), matching
// the production single-connection setting. It bypasses the process-wide
// database.Open singleton so each test gets its own database.
func freshDB(t *testing.T) *database.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "miner.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	return &database.DB{DB: sqlDB}
}

func testConfig() *config.Config {
	c := config.DefaultConfig()
	c.Username = "tester"
	c.Streamers = []config.StreamerConfig{{Username: "streamer1"}}
	c.Discord.Enabled = false
	c.EnableAnalytics = true
	return &c
}

func testFactories(t *testing.T, dbOut **database.DB) factories {
	return factories{
		openDB: func(string) (*database.DB, error) {
			db := freshDB(t)
			if dbOut != nil {
				*dbOut = db
			}
			return db, nil
		},
		newAnalytics: func(db *database.DB, _ string, r int) (*analytics.Service, error) {
			return analytics.NewService(db, t.TempDir(), r)
		},
		newWeb:   web.NewServerEarly,
		newMiner: miner.New,
	}
}

func stepNames(a *App) []string {
	out := make([]string, len(a.steps))
	for i, s := range a.steps {
		out[i] = s.name
	}
	return out
}

// C1 / I1 — Build constructs every required dependency and records them as
// lifecycle steps in construction order.
func TestBuildConstructsDependencies(t *testing.T) {
	ctx := context.Background()
	app, err := buildWith(ctx, testConfig(), runtimeconfig.RuntimeConfig{}, testFactories(t, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	if app.db == nil {
		t.Error("db not built")
	}
	if app.analytics == nil {
		t.Error("analytics not built")
	}
	if app.web == nil {
		t.Error("web not built")
	}
	if app.minerFactory == nil {
		t.Error("miner factory not built")
	}
	if app.controller == nil {
		t.Error("lifecycle controller not built")
	}
	if got, want := stepNames(app), []string{"database", "analytics", "web"}; !equalStrings(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

// (contract §11 item 10) Build's miner factory produces a FRESH *miner.Miner
// on every call — never the same instance twice — and each one is tracked as
// the "current generation" miner under currentMinerMu before it's returned.
func TestBuildMinerFactoryProducesFreshMinerEveryCall(t *testing.T) {
	ctx := context.Background()
	app, err := buildWith(ctx, testConfig(), runtimeconfig.RuntimeConfig{}, testFactories(t, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	m1 := app.minerFactory()
	if m1 == nil {
		t.Fatal("minerFactory returned nil")
	}
	app.currentMinerMu.Lock()
	tracked1 := app.currentMiner
	app.currentMinerMu.Unlock()
	if tracked1 != m1 {
		t.Fatal("currentMiner was not published to the miner the factory just built")
	}
	firstRule := config.DropRule{Skip: true}
	if err := m1.SetDropRule("game::first", firstRule); err != nil {
		t.Fatalf("first generation SetDropRule: %v", err)
	}

	m2 := app.minerFactory()
	if m2 == m1 {
		t.Fatal("minerFactory returned the SAME miner twice; each generation must get a fresh one")
	}
	app.currentMinerMu.Lock()
	tracked2 := app.currentMiner
	app.currentMinerMu.Unlock()
	if tracked2 != m2 {
		t.Fatal("currentMiner was not updated to the SECOND generation's miner")
	}

	_, secondRules := m2.CurrentCampaignPolicy()
	if secondRules["game::first"] != firstRule {
		t.Fatalf("second generation did not inherit committed config: %+v", secondRules)
	}
	if err := m2.SetDropRule("game::second", config.DropRule{HighPriority: true}); err != nil {
		t.Fatalf("second generation SetDropRule: %v", err)
	}
	_, retiredRules := m1.CurrentCampaignPolicy()
	if _, shared := retiredRules["game::second"]; shared {
		t.Fatalf("fresh generation shares mutable config memory with retired provider: %+v", retiredRules)
	}

	start := make(chan struct{})
	errCh := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for range 200 {
			_, _ = m1.CurrentCampaignPolicy()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := range 200 {
			if err := m2.SetDropRule(fmt.Sprintf("game::new-%d", i), config.DropRule{HighPriority: true}); err != nil {
				select {
				case errCh <- err:
				default:
				}
				return
			}
		}
	}()
	close(start)
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("second generation concurrent write: %v", err)
	default:
	}
	_, retiredRules = m1.CurrentCampaignPolicy()
	if len(retiredRules) != 1 || retiredRules["game::first"] != firstRule {
		t.Fatalf("retired provider observed fresh-generation mutations: %+v", retiredRules)
	}
}

// BKM-021 — Build must propagate the immutable dashboard snapshot from the
// RuntimeConfig into the web server it constructs. This is the crux of the
// refactor: the web layer no longer reads the environment, so if
// ws.SetDashboardConfig(rc.Dashboard) were dropped the dashboard would silently
// serve with a zero (no-auth) snapshot — a security regression. AuthConfigured()
// observes only whether auth is on (never the credentials), so the assertion
// needs no network bind and leaks no secret.
func TestBuildPropagatesDashboardToWebServer(t *testing.T) {
	ctx := context.Background()

	// A zero snapshot leaves dashboard auth disabled.
	appNoAuth, err := buildWith(ctx, testConfig(), runtimeconfig.RuntimeConfig{}, testFactories(t, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = appNoAuth.Shutdown(context.Background()) })
	if appNoAuth.web == nil {
		t.Fatal("web server not built")
	}
	if appNoAuth.web.AuthConfigured() {
		t.Error("zero dashboard snapshot must leave dashboard auth DISABLED")
	}

	// A snapshot carrying credentials must reach the web server: if the
	// SetDashboardConfig propagation in Build were removed, this would fail.
	rc := runtimeconfig.RuntimeConfig{
		Dashboard: runtimeconfig.Dashboard{Username: "admin", Password: runtimeconfig.NewSecret("pw")},
	}
	appAuth, err := buildWith(ctx, testConfig(), rc, testFactories(t, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = appAuth.Shutdown(context.Background()) })
	if !appAuth.web.AuthConfigured() {
		t.Error("Build must propagate the credentialed dashboard snapshot into the web server")
	}
}

// C21 / I6 — with analytics disabled, Build opens only the database (no
// analytics service, no web server) and still succeeds.
func TestBuildAnalyticsDisabled(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig()
	cfg.EnableAnalytics = false

	app, err := buildWith(ctx, cfg, runtimeconfig.RuntimeConfig{}, testFactories(t, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	if app.analytics != nil {
		t.Error("analytics built despite EnableAnalytics=false")
	}
	if app.web != nil {
		t.Error("web built despite EnableAnalytics=false")
	}
	if app.minerFactory == nil {
		t.Error("miner factory not built")
	}
	if app.controller == nil {
		t.Error("lifecycle controller not built")
	}
	if got, want := stepNames(app), []string{"database"}; !equalStrings(got, want) {
		t.Errorf("steps = %v, want %v", got, want)
	}
}

// C3 / C4 — a constructor failure after the database is opened closes the
// database before returning (no leaked handle).
func TestBuildFailureClosesOpenedResources(t *testing.T) {
	ctx := context.Background()
	var opened *database.DB
	f := testFactories(t, &opened)
	f.newAnalytics = func(*database.DB, string, int) (*analytics.Service, error) {
		return nil, errBoom
	}

	app, err := buildWith(ctx, testConfig(), runtimeconfig.RuntimeConfig{}, f)
	if err == nil {
		t.Fatal("expected Build error")
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("want errBoom, got %v", err)
	}
	if app != nil {
		t.Fatal("expected nil App on failure")
	}
	if opened == nil {
		t.Fatal("database was never opened")
	}
	// The opened database must have been closed by partial-build cleanup.
	if werr := opened.WithTx(ctx, func(*sql.Tx) error { return nil }); !errors.Is(werr, database.ErrClosed) {
		t.Fatalf("database not closed after failed Build: %v", werr)
	}
}

// C2 / I13 — given an already-open database, Build starts no additional
// goroutines (no serving loop, no watch/pubsub loop).
func TestBuildStartsNoGoroutines(t *testing.T) {
	ctx := context.Background()
	db := freshDB(t)
	// Warm the handle so database/sql's own connection opener goroutine is
	// already counted in the baseline.
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}

	f := factories{
		openDB: func(string) (*database.DB, error) { return db, nil },
		newAnalytics: func(d *database.DB, _ string, r int) (*analytics.Service, error) {
			return analytics.NewService(d, t.TempDir(), r)
		},
		newWeb:   web.NewServerEarly,
		newMiner: miner.New,
	}

	runtime.GC()
	before := runtime.NumGoroutine()
	app, err := buildWith(ctx, testConfig(), runtimeconfig.RuntimeConfig{}, f)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	// Build launches no persistent goroutine, so the count settles back to the
	// pre-Build baseline; the settle window absorbs transient runtime goroutines
	// (GC assists) that would make a single-shot comparison flaky on CI.
	if !waitGoroutinesSettle(before, 2*time.Second) {
		t.Errorf("Build started goroutines: before=%d now=%d", before, runtime.NumGoroutine())
	}
}

// C30 — repeated Build/Shutdown cycles leak no goroutines.
func TestRepeatedBuildShutdownNoLeak(t *testing.T) {
	ctx := context.Background()
	runtime.GC()
	base := runtime.NumGoroutine()

	for i := 0; i < 10; i++ {
		app, err := buildWith(ctx, testConfig(), runtimeconfig.RuntimeConfig{}, testFactories(t, nil))
		if err != nil {
			t.Fatalf("Build #%d: %v", i, err)
		}
		if err := app.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown #%d: %v", i, err)
		}
	}

	// Each Build opens a fresh database (one database/sql opener goroutine),
	// which exits after Close; allow it to settle (generous window for CI).
	if !waitGoroutinesSettle(base+3, 5*time.Second) {
		t.Errorf("goroutine leak after repeated build/shutdown: base=%d now=%d", base, runtime.NumGoroutine())
	}
}

// I14 — after Shutdown the database is closed; any later use is a typed
// use-after-close error.
func TestNoDatabaseUseAfterShutdown(t *testing.T) {
	ctx := context.Background()
	var opened *database.DB
	app, err := buildWith(ctx, testConfig(), runtimeconfig.RuntimeConfig{}, testFactories(t, &opened))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if werr := opened.WithTx(ctx, func(*sql.Tx) error { return nil }); !errors.Is(werr, database.ErrClosed) {
		t.Fatalf("database still usable after Shutdown: %v", werr)
	}
}

// Build validates its inputs so it is self-defending for direct callers.
func TestBuildValidatesConfig(t *testing.T) {
	ctx := context.Background()
	f := testFactories(t, nil)

	cases := []struct {
		name string
		cfg  *config.Config
	}{
		{"nil", nil},
		{"no username", func() *config.Config { c := testConfig(); c.Username = ""; return c }()},
		{"no streamers", func() *config.Config { c := testConfig(); c.Streamers = nil; return c }()},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := buildWith(ctx, tc.cfg, runtimeconfig.RuntimeConfig{}, f); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func waitGoroutinesSettle(target int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= target {
			return true
		}
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
	}
	return runtime.NumGoroutine() <= target
}

// TestAnalyticsStepStartsBeforeWebAndStopsAfterIt pins the ordering the
// immutable Prediction observation collector depends on. The collector is
// started by the analytics step and JOINS its writer in that step's stop, so
// the order must be:
//
//	start:   database -> analytics -> web
//	stop:    web -> analytics -> database
//
// Getting this wrong in either direction is a real defect: starting analytics
// after web would let a request reach a collector that has not bootstrapped,
// and stopping it before web would leave the shared database closing while a
// collector goroutine could still reach it.
func TestAnalyticsStepStartsBeforeWebAndStopsAfterIt(t *testing.T) {
	ctx := context.Background()
	app, err := buildWith(ctx, testConfig(), runtimeconfig.RuntimeConfig{}, testFactories(t, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	names := stepNames(app)
	dbAt, analyticsAt, webAt := -1, -1, -1
	for i, n := range names {
		switch n {
		case "database":
			dbAt = i
		case "analytics":
			analyticsAt = i
		case "web":
			webAt = i
		}
	}
	if dbAt < 0 || analyticsAt < 0 || webAt < 0 {
		t.Fatalf("steps = %v, want database, analytics and web", names)
	}
	if dbAt >= analyticsAt || analyticsAt >= webAt {
		t.Fatalf("steps = %v, want database before analytics before web (Shutdown reverses this)", names)
	}

	// The analytics step must actually HAVE a start: without one the
	// collector would never bootstrap and every observation would be dropped.
	if app.steps[analyticsAt].start == nil {
		t.Fatal("the analytics step has no start; the observation collector would never bootstrap")
	}
	if app.steps[analyticsAt].stop == nil {
		t.Fatal("the analytics step has no stop; the observation writer would never be joined")
	}
	// Starting is nonblocking and never fails startup.
	if err := app.steps[analyticsAt].start(ctx); err != nil {
		t.Fatalf("analytics start must never fail runtime startup: %v", err)
	}
}

// TestAnalyticsStartNeverFailsRunFailure proves a Run that reaches the
// analytics step succeeds even when that step's own subsystem cannot
// bootstrap — an audit trail must not be able to stop the miner.
func TestAnalyticsStartIsIdempotentAndSafeAfterShutdown(t *testing.T) {
	ctx := context.Background()
	app, err := buildWith(ctx, testConfig(), runtimeconfig.RuntimeConfig{}, testFactories(t, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if app.analytics == nil {
		t.Fatal("analytics not built")
	}
	for i := 0; i < 3; i++ {
		if err := app.analytics.Start(); err != nil {
			t.Fatalf("Start #%d: %v", i, err)
		}
	}
	if err := app.Shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	// A start after shutdown is still harmless.
	if err := app.analytics.Start(); err != nil {
		t.Fatalf("Start after shutdown: %v", err)
	}
}
