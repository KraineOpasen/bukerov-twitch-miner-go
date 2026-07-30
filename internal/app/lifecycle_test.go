package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

// These tests exercise the generic lifecycle mechanism (Run start ordering,
// reverse-order shutdown, idempotency, concurrency, context handling, and the
// single-use guards) against fake steps, plus (since contract §11 items
// 10-15) App.Run driving a REAL *lifecycle.Controller wired to a FAKE
// lifecycle.Runner factory — so generation start/stop/pause/resume/update-
// exit/dirty-teardown ordering against App's OWN owned resources (db/
// analytics/web) is verified without the miner's network/auth dependencies.
// Build's real resource+controller wiring is covered in build_test.go.

var errBoom = errors.New("boom")

// recorder captures the ordered sequence of lifecycle events across goroutines.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.events = append(r.events, s)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	copy(out, r.events)
	return out
}

// step builds a lifecycleStep that records start:<name> / stop:<name> and
// returns the supplied errors. A nil start models a resource that is live once
// constructed (database, analytics service).
func step(rec *recorder, name string, startErr, stopErr error) lifecycleStep {
	s := lifecycleStep{
		name: name,
		stop: func(context.Context) error {
			rec.add("stop:" + name)
			return stopErr
		},
	}
	if startErr != errSkipStart {
		s.start = func(context.Context) error {
			rec.add("start:" + name)
			return startErr
		}
	}
	return s
}

// errSkipStart is a sentinel meaning "this step has no start step at all"
// (distinct from a start that succeeds with nil).
var errSkipStart = errors.New("no start")

func mustContain(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event mismatch:\n got=%v\nwant=%v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d]=%q want %q (full got=%v want=%v)", i, got[i], want[i], got, want)
		}
	}
}

// --- ctrlFakeRunner/ctrlFakeFactory: fake lifecycle.Runner/Factory for the
// controller-driven App tests (contract §11 item 15) ---------------------

// ctrlFakeRunner is a controllable lifecycle.Runner: Run blocks until either
// finishCh delivers a value or (unless ignoreCtx) ctx is cancelled — on which
// it returns nil, mirroring the REAL *miner.Miner.Run's contract (a clean
// ctx-cancellation shutdown returns nil, never ctx.Err()), so App-level
// tests exercise the same "process-shutdown -> Run returns nil" path
// production actually takes.
type ctrlFakeRunner struct {
	id        int
	rec       *recorder
	startedCh chan struct{}
	finishCh  chan error
	ignoreCtx bool
}

func (r *ctrlFakeRunner) Run(ctx context.Context) error {
	if r.rec != nil {
		r.rec.add(fmt.Sprintf("run-%d", r.id))
	}
	close(r.startedCh)
	if r.ignoreCtx {
		return <-r.finishCh
	}
	select {
	case err := <-r.finishCh:
		return err
	case <-ctx.Done():
		return nil
	}
}

// ctrlFakeFactory hands out fresh ctrlFakeRunners and records them so a test
// can reach in and control (or inspect) each generation individually — the
// internal/lifecycle package's own runnerFactory test-double precedent,
// mirrored here since internal/lifecycle's _test.go helpers are not
// reachable from this (different) package.
type ctrlFakeFactory struct {
	mu        sync.Mutex
	runners   []*ctrlFakeRunner
	rec       *recorder
	ignoreCtx bool // applied to every runner this factory builds
}

func (f *ctrlFakeFactory) Factory() lifecycle.Runner {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := &ctrlFakeRunner{
		id:        len(f.runners) + 1,
		rec:       f.rec,
		startedCh: make(chan struct{}),
		finishCh:  make(chan error, 1),
		ignoreCtx: f.ignoreCtx,
	}
	f.runners = append(f.runners, r)
	return r
}

func (f *ctrlFakeFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.runners)
}

func (f *ctrlFakeFactory) at(i int) *ctrlFakeRunner {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.runners[i]
}

// waitForCond polls (bounded convergence poll, never a bare sleep-as-
// synchronization) until cond is true or timeout elapses.
func waitForCond(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !cond() {
		t.Fatalf("condition not met within %s", timeout)
	}
}

// requireDialable polls (bounded) until addr accepts a TCP connection.
func requireDialable(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never became dialable: %v", addr, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- step-ordering / single-use-guard tests (App.Run/Shutdown mechanism,
// independent of the controller) -------------------------------------------

// C7 — a start-step failure unwinds every opened/started step in reverse order
// and never even reaches the controller.
func TestRunStartFailureUnwindsInReverse(t *testing.T) {
	rec := &recorder{}
	a := &App{
		steps: []lifecycleStep{
			step(rec, "database", errSkipStart, nil),
			step(rec, "first", nil, nil),
			step(rec, "second", errBoom, nil),
		},
	}
	err := a.Run(context.Background())
	if !errors.Is(err, errBoom) {
		t.Fatalf("want errBoom, got %v", err)
	}
	got := rec.snapshot()
	mustContain(t, got, []string{"start:first", "start:second", "stop:second", "stop:first", "stop:database"})
	// Explicit reverse-order assertion on the stop half.
	assertOrder(t, got, "stop:second", "stop:first")
	assertOrder(t, got, "stop:first", "stop:database")
}

// C8 — Shutdown stops steps in strict reverse construction order.
func TestShutdownReverseOrder(t *testing.T) {
	rec := &recorder{}
	a := &App{steps: []lifecycleStep{
		step(rec, "database", errSkipStart, nil),
		step(rec, "analytics", errSkipStart, nil),
		step(rec, "web", errSkipStart, nil),
	}}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	mustContain(t, rec.snapshot(), []string{"stop:web", "stop:analytics", "stop:database"})
}

// C9 — Shutdown is idempotent: a second call is a no-op and returns the same
// error.
func TestShutdownIdempotent(t *testing.T) {
	rec := &recorder{}
	a := &App{steps: []lifecycleStep{step(rec, "database", errSkipStart, nil)}}
	first := a.Shutdown(context.Background())
	second := a.Shutdown(context.Background())
	if first != nil || second != nil {
		t.Fatalf("errors: first=%v second=%v", first, second)
	}
	mustContain(t, rec.snapshot(), []string{"stop:database"})
}

// C10 — concurrent Shutdown callers each observe a single, complete teardown.
func TestShutdownConcurrent(t *testing.T) {
	var counts sync.Map
	mk := func(name string) lifecycleStep {
		return lifecycleStep{name: name, stop: func(context.Context) error {
			v, _ := counts.LoadOrStore(name, new(int))
			p := v.(*int)
			*p++
			return nil
		}}
	}
	a := &App{steps: []lifecycleStep{mk("database"), mk("analytics"), mk("web")}}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = a.Shutdown(context.Background())
		}()
	}
	wg.Wait()

	for _, name := range []string{"database", "analytics", "web"} {
		v, _ := counts.Load(name)
		if v == nil || *(v.(*int)) != 1 {
			t.Fatalf("step %s stopped %v times, want 1", name, v)
		}
	}
}

// C11 — Shutdown surfaces a context deadline while still attempting every stop.
func TestShutdownContextTimeout(t *testing.T) {
	rec := &recorder{}
	a := &App{steps: []lifecycleStep{
		{name: "slow", stop: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}},
		{name: "fast", stop: func(context.Context) error {
			rec.add("stop:fast")
			return nil
		}},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := a.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatalf("Shutdown blocked too long")
	}
	// "fast" (constructed first, stopped last) still ran.
	mustContain(t, rec.snapshot(), []string{"stop:fast"})
}

// C16 — Run is single-use: a second Run returns ErrAlreadyRun. No controller
// is needed: with a.controller == nil, Run returns nil immediately after its
// (empty) step loop.
func TestRunTwiceReturnsError(t *testing.T) {
	a := &App{}
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := a.Run(context.Background()); !errors.Is(err, ErrAlreadyRun) {
		t.Fatalf("second Run: want ErrAlreadyRun, got %v", err)
	}
}

// C17 / C18 — Shutdown before Run is safe; Run after Shutdown returns
// ErrShutDown.
func TestRunAfterShutdown(t *testing.T) {
	rec := &recorder{}
	a := &App{
		steps: []lifecycleStep{step(rec, "database", errSkipStart, nil)},
	}
	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Run: %v", err)
	}
	if err := a.Run(context.Background()); !errors.Is(err, ErrShutDown) {
		t.Fatalf("Run after Shutdown: want ErrShutDown, got %v", err)
	}
}

// C5 (mechanism) — stopSteps tears down every step exactly once, in reverse.
func TestStopStepsReverseExactlyOnce(t *testing.T) {
	rec := &recorder{}
	counts := map[string]int{}
	mk := func(name string) lifecycleStep {
		return lifecycleStep{name: name, stop: func(context.Context) error {
			rec.add(name)
			counts[name]++
			return nil
		}}
	}
	a := &App{steps: []lifecycleStep{mk("database"), mk("analytics"), mk("web")}}
	if err := a.stopSteps(context.Background(), len(a.steps)); err != nil {
		t.Fatalf("stopSteps: %v", err)
	}
	mustContain(t, rec.snapshot(), []string{"web", "analytics", "database"})
	for name, n := range counts {
		if n != 1 {
			t.Fatalf("step %s stopped %d times, want 1", name, n)
		}
	}
}

// stopSteps aggregates errors from every step without hiding any.
func TestStopStepsAggregatesErrors(t *testing.T) {
	rec := &recorder{}
	a := &App{steps: []lifecycleStep{
		{name: "database", stop: func(context.Context) error { rec.add("database"); return errBoom }},
		{name: "web", stop: func(context.Context) error { rec.add("web"); return errors.New("web-fail") }},
	}}
	err := a.stopSteps(context.Background(), len(a.steps))
	if err == nil || !errors.Is(err, errBoom) {
		t.Fatalf("want aggregated error wrapping errBoom, got %v", err)
	}
	if !strings.Contains(err.Error(), "web-fail") {
		t.Fatalf("aggregate missing web error: %v", err)
	}
	// Both stops still ran despite the first error.
	mustContain(t, rec.snapshot(), []string{"web", "database"})
}

// assertOrder asserts that a appears before b in the event slice.
func assertOrder(t *testing.T, events []string, a, b string) {
	t.Helper()
	ai, bi := -1, -1
	for i, e := range events {
		if e == a && ai == -1 {
			ai = i
		}
		if e == b {
			bi = i
		}
	}
	if ai == -1 {
		t.Fatalf("event %q not found in %v", a, events)
	}
	if bi == -1 {
		t.Fatalf("event %q not found in %v", b, events)
	}
	if ai >= bi {
		t.Fatalf("expected %q before %q, got order %v", a, b, events)
	}
}

// --- controller-driven App tests (contract §11 item 15) -------------------
//
// NOTE: the former TestRunStartsStepsInOrder (C6) and
// TestRunThenCancelThenShutdown (I2/I10) — "Run starts steps in order, then
// runs the runner until cancelled, then Shutdown tears the step down" — are
// superseded by TestControllerDefaultPathStartsOneGeneration below, which
// covers the identical ordering guarantee against a REAL *lifecycle.Controller
// instead of a bare runner. TestRunPropagatesRunnerError (C19) — "a runtime
// fatal error from the runner propagates out of Run unchanged" — no longer
// holds AT ALL under the controller: an ordinary (non-dirty-teardown-class)
// generation error while desired=running is now a startup failure that
// SCHEDULES A RETRY instead of exiting the process (design v6 §5.3 — this is
// the entire point of the durable lifecycle core). Its replacement is
// TestControllerDirtyTeardownDesiredRunningExitsSupervisorPath below, which
// pins the ACTUAL new contract: only a RECOGNIZED dirty-teardown-class error
// exits; anything else retries.

// freshLifecycleStore builds a *lifecycle.Store over a fresh, isolated sqlite
// handle (mirrors build_test.go's freshDB), so each controller test gets its
// own durable desired-state row.
func freshLifecycleStore(t *testing.T) (*database.DB, *lifecycle.Store) {
	t.Helper()
	db := freshDB(t)
	store, err := lifecycle.NewStore(db)
	if err != nil {
		t.Fatalf("lifecycle.NewStore: %v", err)
	}
	return db, store
}

// reservePort returns a free loopback TCP port (reserved then released
// immediately), for building a real, unstarted web.Server.
func reservePort(t *testing.T) int {
	t.Helper()
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()
	return port
}

// (15a) Default path: no persisted lifecycle row -> exactly one generation
// started; App.Run starts steps in order, blocks in the controller until ctx
// is cancelled, and returns the generation's (nil, clean-shutdown) result —
// behavior equivalent to the pre-b3 single-runner model for the common case.
func TestControllerDefaultPathStartsOneGeneration(t *testing.T) {
	rec := &recorder{}
	factory := &ctrlFakeFactory{rec: rec}
	_, store := freshLifecycleStore(t)

	a := &App{
		steps:      []lifecycleStep{step(rec, "web", nil, nil)},
		controller: lifecycle.New(lifecycle.Config{Factory: factory.Factory, Persistence: store}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- a.Run(ctx) }()

	waitForCond(t, 5*time.Second, func() bool { return factory.count() == 1 })
	r0 := factory.at(0)
	select {
	case <-r0.startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("generation never started")
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("Run = %v, want nil (clean shutdown)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}

	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	got := rec.snapshot()
	assertOrder(t, got, "start:web", "run-1")
	assertOrder(t, got, "run-1", "stop:web")
	if factory.count() != 1 {
		t.Fatalf("factory called %d times, want exactly 1", factory.count())
	}
}

// (15b) App-owned DB/analytics/web survive a generation stop: Pause tears
// down the first generation, the App-owned resources stay alive and usable
// throughout, and Resume builds a SECOND, fresh miner (factory call count ==
// 2), never reusing the first.
func TestControllerPauseResumeSurvivesAppResources(t *testing.T) {
	rec := &recorder{}
	factory := &ctrlFakeFactory{rec: rec}
	db, store := freshLifecycleStore(t)

	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("analytics.NewService: %v", err)
	}
	port := reservePort(t)
	ws := web.NewServerEarly(config.AnalyticsSettings{Host: "127.0.0.1", Port: port}, "tester", t.TempDir(), svc)

	a := &App{
		db: db, analytics: svc, web: ws,
		steps: []lifecycleStep{
			{name: "database", stop: closer(db.Close)},
			{name: "analytics", stop: closer(svc.Close)},
			{name: "web", start: fromError(ws.Start), stop: fromVoid(ws.Stop)},
		},
		controller: lifecycle.New(lifecycle.Config{Factory: factory.Factory, Persistence: store}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- a.Run(ctx) }()

	waitForCond(t, 5*time.Second, func() bool { return factory.count() == 1 })
	r0 := factory.at(0)
	<-r0.startedCh

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	requireDialable(t, addr)

	res := a.controller.Pause(context.Background())
	if res.Outcome != lifecycle.OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}
	r0.finishCh <- nil
	waitForCond(t, 5*time.Second, func() bool { return a.controller.Snapshot().Observed == lifecycle.ObservedPaused })

	// App-owned resources must survive the generation's teardown.
	requireDialable(t, addr)
	if werr := db.WithTx(context.Background(), func(*sql.Tx) error { return nil }); werr != nil {
		t.Fatalf("db not usable after pause: %v", werr)
	}

	res = a.controller.Resume(context.Background())
	if res.Outcome != lifecycle.OutcomeAccepted {
		t.Fatalf("resume: %v (%v)", res.Outcome, res.Err)
	}
	waitForCond(t, 5*time.Second, func() bool { return factory.count() == 2 })
	r1 := factory.at(1)
	select {
	case <-r1.startedCh:
	case <-time.After(5 * time.Second):
		t.Fatal("second generation never started")
	}
	if r1 == r0 {
		t.Fatal("resume must build a FRESH miner (generation), never reuse the old one")
	}

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after final cancel")
	}
}

// (15c) SIGTERM-equivalent with a hung generation: App.Shutdown's database
// close must NOT begin before the generation's own Run has actually
// returned (M1 ordering) — a hung generation blocks the WHOLE shutdown
// drain, it never gets silently skipped.
func TestControllerShutdownWaitsForHungGenerationBeforeDBClose(t *testing.T) {
	rec := &recorder{}
	factory := &ctrlFakeFactory{rec: rec, ignoreCtx: true} // hung: ignores ctx cancellation
	db, store := freshLifecycleStore(t)

	dbClosed := make(chan struct{})
	a := &App{
		db: db,
		steps: []lifecycleStep{{name: "database", stop: func(context.Context) error {
			close(dbClosed)
			return db.Close()
		}}},
		controller: lifecycle.New(lifecycle.Config{Factory: factory.Factory, Persistence: store}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- a.Run(ctx) }()

	waitForCond(t, 5*time.Second, func() bool { return factory.count() == 1 })
	r0 := factory.at(0)
	<-r0.startedCh

	cancel() // SIGTERM-equivalent

	select {
	case <-dbClosed:
		t.Fatal("db close observer fired before the hung generation's Run returned")
	case <-time.After(150 * time.Millisecond):
		// expected: still blocked, waiting on the hung generation
	}

	r0.finishCh <- nil // release the hung generation
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the hung generation finally returned")
	}

	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-dbClosed:
	default:
		t.Fatal("Shutdown did not close the database")
	}
}

// (15d) A persisted paused intent is honored at boot: the miner factory is
// NEVER called, App.Run stays blocked (idle) rather than returning, the web
// server stays alive, and the controller's snapshot reports observed=paused.
// (The NoControlSurface ring-event-once behavior this scenario can also
// trigger is already covered at the lifecycle-package level —
// internal/lifecycle/boot_status_test.go — since it needs no App-level
// resource to observe.)
func TestControllerPersistedPausedHonoredFactoryNeverCalled(t *testing.T) {
	rec := &recorder{}
	factory := &ctrlFakeFactory{rec: rec}
	_, store := freshLifecycleStore(t)
	if err := store.Save(context.Background(), lifecycle.DesiredPaused, "test-seed", ""); err != nil {
		t.Fatalf("seed persisted paused: %v", err)
	}

	port := reservePort(t)
	svc, err := analytics.NewService(freshDB(t), t.TempDir(), 0)
	if err != nil {
		t.Fatalf("analytics.NewService: %v", err)
	}
	ws := web.NewServerEarly(config.AnalyticsSettings{Host: "127.0.0.1", Port: port}, "tester", t.TempDir(), svc)

	a := &App{
		web:   ws,
		steps: []lifecycleStep{{name: "web", start: fromError(ws.Start), stop: fromVoid(ws.Stop)}},
		controller: lifecycle.New(lifecycle.Config{
			Factory: factory.Factory, Persistence: store, NoControlSurface: false,
		}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- a.Run(ctx) }()

	waitForCond(t, 5*time.Second, func() bool { return a.controller.Snapshot().Observed == lifecycle.ObservedPaused })
	time.Sleep(20 * time.Millisecond)
	if factory.count() != 0 {
		t.Fatalf("factory called %d times for a persisted-paused boot, want 0", factory.count())
	}

	select {
	case err := <-runErrCh:
		t.Fatalf("Run returned (%v) despite no shutdown signal — a persisted-paused boot must stay idle", err)
	case <-time.After(150 * time.Millisecond):
		// expected: still blocked
	}

	requireDialable(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))

	cancel()
	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("Run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// (15e/15f) Update-exit through App: the process-level updater loop calling
// UpdateApplied makes App.Run return nil (exit-0 semantics) WITHOUT the
// parent ctx ever being cancelled, and the updater loop is joined (its own
// goroutine observably returned) strictly BEFORE Run returns — and, since
// Shutdown is a separate, later call in the real cmd/miner sequence, the
// database close does not happen until that separate Shutdown call, at
// which point it proceeds cleanly.
func TestControllerUpdateExitFullExitZeroAndUpdaterJoinedBeforeDBClose(t *testing.T) {
	rec := &recorder{}
	factory := &ctrlFakeFactory{rec: rec}
	db, store := freshLifecycleStore(t)

	loopStarted := make(chan struct{})
	loopReturned := make(chan struct{})
	dbClosed := make(chan struct{})

	updaterRun := func(ctx context.Context) {
		close(loopStarted)
		<-ctx.Done()
		close(loopReturned)
	}

	controller := lifecycle.New(lifecycle.Config{
		Factory: factory.Factory, Persistence: store, UpdaterRun: updaterRun,
	})
	a := &App{
		db: db,
		steps: []lifecycleStep{{name: "database", stop: func(context.Context) error {
			close(dbClosed)
			return db.Close()
		}}},
		controller: controller,
	}

	// The parent ctx is deliberately NEVER cancelled here: an update-exit is
	// independent of process-shutdown (design v6 §7) — App.Run must return
	// on its own.
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- a.Run(context.Background()) }()

	waitForCond(t, 5*time.Second, func() bool { return factory.count() == 1 })
	<-factory.at(0).startedCh
	select {
	case <-loopStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("updater loop never started")
	}

	controller.UpdateApplied()

	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("Run = %v, want nil (update-exit, exit-0 semantics)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after UpdateApplied")
	}

	// The updater loop must already have returned (joined) by the time Run
	// returned — a non-blocking check is exactly right here.
	select {
	case <-loopReturned:
	default:
		t.Fatal("updater loop was not joined before Run returned")
	}

	// Shutdown has not been called yet: the database must still be open.
	select {
	case <-dbClosed:
		t.Fatal("db closed before Shutdown was even called")
	default:
	}

	if err := a.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	select {
	case <-dbClosed:
	default:
		t.Fatal("Shutdown did not close the database")
	}
}

// (15g) Dirty teardown, desired=running: a generation dying spontaneously
// with an error the classifier recognizes as "dirty teardown" (join-timeout
// class) exits the whole process (App.Run returns non-nil, the supervisor-
// restart path) rather than retrying.
func TestControllerDirtyTeardownDesiredRunningExitsSupervisorPath(t *testing.T) {
	withDirtyTeardownTestClassifier(t)

	rec := &recorder{}
	factory := &ctrlFakeFactory{rec: rec}
	db, store := freshLifecycleStore(t)

	a := &App{
		db:         db,
		steps:      []lifecycleStep{{name: "database", stop: closer(db.Close)}},
		controller: lifecycle.New(lifecycle.Config{Factory: factory.Factory, Persistence: store}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- a.Run(ctx) }()

	waitForCond(t, 5*time.Second, func() bool { return factory.count() == 1 })
	r0 := factory.at(0)
	<-r0.startedCh

	// Spontaneous death, dirty-teardown class, desired stays running (never
	// commanded to pause/stop).
	r0.finishCh <- fmt.Errorf("wrapped: %w", lifecycle.ErrDirtyTeardown)

	select {
	case err := <-runErrCh:
		if err == nil {
			t.Fatal("Run returned nil, want a non-nil error (dirty teardown while desired=running)")
		}
		if !errors.Is(err, lifecycle.ErrDirtyTeardown) {
			t.Errorf("Run error = %v, want it to wrap lifecycle.ErrDirtyTeardown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after a dirty teardown with desired=running")
	}
}

// (15g, continued) Dirty teardown, desired=paused: the SAME class of error,
// but during a commanded pause (desired already paused), enters degraded
// instead of exiting — App.Run stays blocked, and no new generation is
// built until the process itself restarts.
func TestControllerDirtyTeardownDesiredPausedEntersDegradedWithoutExit(t *testing.T) {
	withDirtyTeardownTestClassifier(t)

	rec := &recorder{}
	factory := &ctrlFakeFactory{rec: rec, ignoreCtx: true} // deterministic: only finishCh resolves Run
	db, store := freshLifecycleStore(t)

	a := &App{
		db:         db,
		steps:      []lifecycleStep{{name: "database", stop: closer(db.Close)}},
		controller: lifecycle.New(lifecycle.Config{Factory: factory.Factory, Persistence: store}),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- a.Run(ctx) }()

	waitForCond(t, 5*time.Second, func() bool { return factory.count() == 1 })
	r0 := factory.at(0)
	<-r0.startedCh

	res := a.controller.Pause(context.Background())
	if res.Outcome != lifecycle.OutcomeAccepted {
		t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}
	r0.finishCh <- fmt.Errorf("wrapped: %w", lifecycle.ErrDirtyTeardown)

	waitForCond(t, 5*time.Second, func() bool { return a.controller.Snapshot().Observed == lifecycle.ObservedDegraded })

	select {
	case err := <-runErrCh:
		t.Fatalf("Run returned (%v) on a degraded (desired=paused) dirty teardown, want it to stay blocked", err)
	case <-time.After(150 * time.Millisecond):
		// expected: still blocked
	}

	if factory.count() != 1 {
		t.Fatalf("factory called %d times after entering degraded, want exactly 1 (no new generation)", factory.count())
	}

	cancel()
	select {
	case <-runErrCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the final cancel")
	}
}

// withDirtyTeardownTestClassifier temporarily overrides
// lifecycle.IsDirtyTeardownError to recognize ONLY lifecycle.ErrDirtyTeardown
// (the package's own test sentinel), restoring the previous classifier on
// cleanup — deterministic regardless of whether some OTHER test in this
// binary already called wireDirtyTeardownClassifier (that one recognizes
// miner.IsJoinTimeoutError too, which is irrelevant and harmless here, but a
// test should not rely on another test's side effect for correctness).
func withDirtyTeardownTestClassifier(t *testing.T) {
	t.Helper()
	orig := lifecycle.IsDirtyTeardownError
	lifecycle.IsDirtyTeardownError = func(err error) bool {
		return errors.Is(err, lifecycle.ErrDirtyTeardown)
	}
	t.Cleanup(func() { lifecycle.IsDirtyTeardownError = orig })
}
