package miner

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
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

	// Exactly-once: a later explicit stop() must not re-run the teardown.
	if serr := m.stop(); serr != nil {
		t.Fatalf("second stop() = %v, want the memoized nil", serr)
	}
	if got := cleanups.Load(); got != 1 {
		t.Fatalf("teardown re-ran on a second stop(): observer count %d, want 1", got)
	}
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
