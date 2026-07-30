package miner

import (
	"context"
	"database/sql"
	"errors"
	"net"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

// newStartupCleanupMiner builds a Run-able miner for the S2 startup-failure
// tests: temp working directory (initialize creates cookies/logs there), the
// process-global singleton database injected as an EXTERNALLY-owned handle
// (see TestMain — closing it would poison every later test in this binary,
// and ownership-wise the cleanup must leave injected handles alone anyway),
// analytics/web/Discord/debug all disabled so the whole startup sequence runs
// offline.
func newStartupCleanupMiner(t *testing.T) (*Miner, *database.DB) {
	t.Helper()
	t.Chdir(t.TempDir())

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Username = "startup_cleanup_tester"
	cfg.Streamers = nil
	cfg.EnableAnalytics = false
	cfg.Discord.Enabled = false
	cfg.Debug.Enabled = false

	m := New(&cfg, "")
	m.SetDatabase(db)
	return m, db
}

// stubAuthenticate replaces the network authenticate stage with an offline
// stub that still leaves the miner in the state setupComponents relies on
// (m.auth and m.client populated).
func stubAuthenticate(m *Miner) {
	m.authenticateFn = func(context.Context) error {
		m.auth = auth.NewTwitchAuth(m.config.StorageKey(), m.deviceID)
		m.client = twitch.NewTwitchClient(m.auth, m.deviceID)
		return nil
	}
}

// stubLoadStreamers replaces the network streamer-resolution stage with an
// offline stub that still leaves the miner with a live (empty) roster
// manager, the state setupComponents relies on.
func stubLoadStreamers(m *Miner) {
	m.loadStreamersFn = func() error {
		m.streamers = streamer.NewManager(m.client, m.config.StreamerSettings)
		return nil
	}
}

// requireExternalDBAlive asserts the injected (externally owned) database
// handle survived the miner's cleanup — the miner must never close a handle
// it did not open (ownsDB=false).
func requireExternalDBAlive(t *testing.T, db *database.DB) {
	t.Helper()
	if err := db.WithTx(context.Background(), func(tx *sql.Tx) error { return nil }); err != nil {
		t.Fatalf("externally owned database handle is unusable after startup cleanup: %v", err)
	}
}

// requireNoComponentGoroutines polls — bounded, never a bare sleep-as-
// synchronization: the condition is "every transport/loop owner has exited",
// for which no join barrier is exposed once Run has returned — until no
// goroutine stack contains a frame from the miner-owned runtime packages
// (pubsub/chat read loops, watcher/drops/discovery/notifications loops).
// Marker-based stack filtering keeps the check immune to unrelated test or
// runtime goroutines in this shared binary.
func requireNoComponentGoroutines(t *testing.T) {
	t.Helper()
	markers := []string{
		"internal/pubsub.",
		"internal/chat.",
		"internal/watcher.",
		"internal/drops.",
		"internal/discovery.",
		"internal/notifications.",
	}
	deadline := time.Now().Add(5 * time.Second)
	var leftover string
	for {
		// runtime.Stack truncates silently at the buffer size; grow until the
		// dump fits so a large dump cannot hide a leftover marker.
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		for n == len(buf) {
			buf = make([]byte, len(buf)*2)
			n = runtime.Stack(buf, true)
		}
		stacks := string(buf[:n])
		leftover = ""
		for _, marker := range markers {
			if strings.Contains(stacks, marker) {
				leftover = marker
				break
			}
		}
		if leftover == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a goroutine owned by %q survived the startup-failure cleanup:\n%s", leftover, stacks)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStopSafeWithPartialInitialization (S2): stop() must tolerate a miner
// whose startup failed before any component was constructed — every
// component teardown is nil-guarded, nothing panics, and a no-op teardown
// reports no error.
func TestStopSafeWithPartialInitialization(t *testing.T) {
	m := &Miner{}
	done := make(chan error, 1)
	go func() {
		done <- m.stop()
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stop() on an empty miner = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stop() on an empty miner blocked")
	}
}

// TestRunCleansUpAfterAuthenticationFailure (S2, requirement 1): the first
// early return after Run's runtime initialization begins (here: the
// authentication stage, with the database already injected) must execute the
// startup cleanup exactly once, preserve the original startup error through
// errors.Is, and leave the externally owned database untouched.
func TestRunCleansUpAfterAuthenticationFailure(t *testing.T) {
	m, db := newStartupCleanupMiner(t)

	sentinel := errors.New("s2 auth boom")
	m.authenticateFn = func(context.Context) error { return sentinel }

	var cleanups atomic.Int32
	m.stopObserver = func() { cleanups.Add(1) }

	err := m.Run(context.Background())
	if err == nil {
		t.Fatal("Run with a failing authentication stage returned nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run error %v does not preserve the startup error via errors.Is", err)
	}
	if got := cleanups.Load(); got != 1 {
		t.Fatalf("startup cleanup ran %d times after an authentication failure, want exactly 1", got)
	}
	requireExternalDBAlive(t, db)
}

// TestRunSubscribeFailureCleansPartialStartup (S2 flagship, requirements
// 2/4/5/6/7/11/12): a failure at the subscribe stage — with the full
// component graph constructed and the notifications manager already started —
// must route through the single S1 teardown: the run error preserves the
// startup error, cleanup runs exactly once, the pubsub pool is closed (its
// read-loop owners joined), the externally owned database is NOT closed, and
// a second stop() does not re-run the teardown.
func TestRunSubscribeFailureCleansPartialStartup(t *testing.T) {
	m, db := newStartupCleanupMiner(t)
	stubAuthenticate(m)
	stubLoadStreamers(m)

	sentinel := errors.New("s2 subscribe boom")
	m.subscribeTopicsFn = func() error { return sentinel }

	var cleanups atomic.Int32
	m.stopObserver = func() { cleanups.Add(1) }

	err := m.Run(context.Background())
	if err == nil {
		t.Fatal("Run with a failing subscribe stage returned nil")
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Run error %v does not preserve the startup error via errors.Is", err)
	}
	if got := cleanups.Load(); got != 1 {
		t.Fatalf("startup cleanup ran %d times after a subscribe failure, want exactly 1", got)
	}

	// The pool was constructed by setupComponents; the cleanup must have
	// closed it. A closed pool refuses Submit without dialing, so this
	// assertion is offline-safe — and it only runs once the cleanup above
	// has been proven to fire.
	serr := m.wsPool.Submit(pubsub.NewTopic(pubsub.TopicCommunityPointsUser, "0"))
	if !errors.Is(serr, pubsub.ErrPoolClosed) {
		t.Fatalf("wsPool.Submit after startup cleanup = %v, want ErrPoolClosed — the pool was not closed", serr)
	}

	requireExternalDBAlive(t, db)
	requireNoComponentGoroutines(t)

	// Exactly-once: a later explicit stop() must not re-run the teardown.
	if serr := m.stop(); serr != nil {
		t.Fatalf("second stop() = %v, want the memoized nil", serr)
	}
	if got := cleanups.Load(); got != 1 {
		t.Fatalf("teardown re-ran on a second stop(): observer count %d, want 1", got)
	}
}

// TestRunNormalCancellationUsesS1DrainOnce (S2, requirement 8): a SUCCESSFUL
// startup followed by normal context cancellation must still take the S1
// drain path exactly once and return nil. The startMining stage is stubbed
// via its seam (the real one fires an immediate drops campaign sync whose
// offline GQL retry tail breaks the drain bounds — see the seam's doc
// comment); the stub still registers a REAL background loop in loopWG so the
// test proves the S1 loop join runs on this path. Requirement-8 coverage of
// startMining's real loops remains with the S1 suite (stop_join_test.go and
// the component packages).
func TestRunNormalCancellationUsesS1DrainOnce(t *testing.T) {
	m, db := newStartupCleanupMiner(t)
	stubAuthenticate(m)
	stubLoadStreamers(m)
	m.subscribeTopicsFn = func() error { return nil }

	started := make(chan struct{})
	loopJoined := make(chan struct{})
	m.startMiningFn = func(ctx context.Context) {
		m.startLoop(ctx, func(ctx context.Context) {
			<-ctx.Done()
			close(loopJoined)
		})
		close(started)
	}

	var cleanups atomic.Int32
	m.stopObserver = func() { cleanups.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- m.Run(ctx) }()

	// Cancel only after startup completed, so this covers the genuine
	// running-then-cancelled path rather than an already-cancelled start.
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("startup did not complete")
	}
	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("Run after a successful startup and normal cancellation = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancellation")
	}

	select {
	case <-loopJoined:
	default:
		t.Fatal("Run returned before its background loop observed cancellation — the S1 loop join did not run")
	}
	if got := cleanups.Load(); got != 1 {
		t.Fatalf("S1 drain ran %d times on the normal-cancellation path, want exactly 1", got)
	}
	requireExternalDBAlive(t, db)
}

// NOTE: the former TestRunAutoUpdateShutdownStaysSuccessful (S2, requirement
// 9) lived here, pinning that an applied auto-update's shutdown request
// still returns nil from Run (exit 0 semantics) and runs the S1 drain
// exactly once. Design v6 §7 moved the auto-updater out of any
// generation-owned goroutine and onto the process-level lifecycle
// controller, so that invariant no longer has a Miner-level call path to
// exercise (startAutoUpdater/requestUpdateShutdown were deleted). The
// equivalent invariant is now
// TestUpdaterLoopUpdateAppliedTeardownAndJoinBeforeRunReturns in
// internal/lifecycle/updater_loop_test.go, which drives the same
// UpdateApplied-triggered exit path against Controller.Run directly.

// TestRunEarlyExitLeavesExternalWebAndAnalyticsAlone (S2, ownership): with
// App-owned (injected) web server and analytics service — the production
// composition, where App has already STARTED the web listener before
// Miner.Run runs — a startup failure's cleanup must not stop either: the web
// listener keeps accepting connections after Run returns, and the injected
// database stays open. (analytics.Service.Close is a documented no-op, so
// the listener probe is the observable half; the same externalAnalytics flag
// gates both.)
func TestRunEarlyExitLeavesExternalWebAndAnalyticsAlone(t *testing.T) {
	m, db := newStartupCleanupMiner(t)
	stubAuthenticate(m)
	stubLoadStreamers(m)

	// Reserve a loopback port for the injected server.
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	m.config.EnableAnalytics = true
	m.config.Analytics.Host = "127.0.0.1"
	m.config.Analytics.Port = port

	svc, err := analytics.NewService(db, m.config.StorageKey(), m.config.Analytics.RetentionDays)
	if err != nil {
		t.Fatalf("analytics.NewService: %v", err)
	}
	m.SetAnalyticsService(svc)

	ws := web.NewServerEarly(m.config.Analytics, m.config.Username, m.config.StorageKey(), svc)
	if ws == nil {
		t.Fatal("web.NewServerEarly returned nil")
	}
	if err := ws.Start(); err != nil {
		t.Fatalf("web server Start: %v", err)
	}
	defer ws.Stop()
	m.SetWebServer(ws)

	// web.Server.Start binds inside its serving goroutine, so the listener
	// becomes reachable asynchronously: poll (bounded) until the first dial
	// succeeds.
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	dialDeadline := time.Now().Add(5 * time.Second)
	for {
		conn, derr := net.DialTimeout("tcp", addr, time.Second)
		if derr == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(dialDeadline) {
			t.Fatalf("injected web server not reachable before Run: %v", derr)
		}
		time.Sleep(5 * time.Millisecond)
	}

	sentinel := errors.New("s2 subscribe boom")
	m.subscribeTopicsFn = func() error { return sentinel }

	if err := m.Run(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("Run = %v, want the subscribe sentinel", err)
	}

	// The App-owned listener must still accept connections: the miner's
	// cleanup must not have closed an injected web server.
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatalf("injected web server no longer reachable after startup cleanup — the miner stopped an App-owned server: %v", err)
	}
	_ = conn.Close()

	requireExternalDBAlive(t, db)
}

// TestFailStartupAggregatesCleanupErrors (S2, requirements 3/4/5): a cleanup
// step failing (here: the bounded background-loop join timing out on a hung
// loop) must not suppress later cleanup steps — the pubsub pool is still
// closed — and the returned error must carry BOTH the original startup error
// and the cleanup error, each discoverable via errors.Is.
func TestFailStartupAggregatesCleanupErrors(t *testing.T) {
	old := loopJoinTimeout
	loopJoinTimeout = 50 * time.Millisecond
	defer func() { loopJoinTimeout = old }()

	m := &Miner{}
	a := auth.NewTwitchAuth("s2_tester", "s2_device")
	client := twitch.NewTwitchClient(a, "s2_device")
	m.wsPool = pubsub.NewWebSocketPool(client, func() pubsub.AuthSnapshot {
		return pubsub.AuthSnapshot{}
	}, nil, config.DefaultConfig().RateLimits)

	hang := make(chan struct{})
	defer close(hang) // release the deliberately hung loop after the test
	m.startLoop(context.Background(), func(context.Context) { <-hang })

	sentinel := errors.New("s2 startup boom")
	err := m.failStartup(sentinel)
	if !errors.Is(err, sentinel) {
		t.Fatalf("failStartup error %v lost the startup error", err)
	}
	if !errors.Is(err, errLoopJoinTimeout) {
		t.Fatalf("failStartup error %v does not carry the cleanup (loop join timeout) error", err)
	}

	serr := m.wsPool.Submit(pubsub.NewTopic(pubsub.TopicCommunityPointsUser, "0"))
	if !errors.Is(serr, pubsub.ErrPoolClosed) {
		t.Fatalf("wsPool.Submit after a failed cleanup step = %v, want ErrPoolClosed — a cleanup error suppressed later steps", serr)
	}
}

// TestStopExactlyOnceMemoized (S2, requirement 7): stop() runs its teardown
// exactly once; a second call returns the first call's result without
// re-running any teardown step. The proof is channel-based, not timing-based:
// after the first stop() times out joining a hung loop, the loop is released
// — if the second stop() re-ran the join it would now succeed and return nil,
// so the memoized timeout error is only observable when the teardown did NOT
// re-run.
func TestStopExactlyOnceMemoized(t *testing.T) {
	old := loopJoinTimeout
	loopJoinTimeout = 50 * time.Millisecond
	defer func() { loopJoinTimeout = old }()

	m := &Miner{}
	var cleanups atomic.Int32
	m.stopObserver = func() { cleanups.Add(1) }

	hang := make(chan struct{})
	m.startLoop(context.Background(), func(context.Context) { <-hang })

	first := m.stop()
	if !errors.Is(first, errLoopJoinTimeout) {
		t.Fatalf("first stop() = %v, want errLoopJoinTimeout", first)
	}

	// Release the loop: a re-run of the teardown would now join cleanly.
	close(hang)

	second := m.stop()
	if !errors.Is(second, errLoopJoinTimeout) {
		t.Fatalf("second stop() = %v, want the memoized first result (errLoopJoinTimeout) — the teardown re-ran", second)
	}
	if got := cleanups.Load(); got != 1 {
		t.Fatalf("teardown observer fired %d times across two stop() calls, want exactly 1", got)
	}
}

// TestStopConcurrentCallsSafe (S2, requirement 10): concurrent stop() calls
// are safe (race detector) and the teardown still runs exactly once; every
// caller observes the same result.
func TestStopConcurrentCallsSafe(t *testing.T) {
	m := &Miner{}
	var cleanups atomic.Int32
	m.stopObserver = func() { cleanups.Add(1) }

	start := make(chan struct{})
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			results <- m.stop()
		}()
	}
	close(start)

	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Fatalf("concurrent stop() = %v, want nil", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("concurrent stop() blocked")
		}
	}
	if got := cleanups.Load(); got != 1 {
		t.Fatalf("teardown ran %d times under concurrent stop(), want exactly 1", got)
	}
}
