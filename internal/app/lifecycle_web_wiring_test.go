package app

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

// ---- lifecycleUpdateStateCell (task contract D5) -------------------------

// TestLifecycleUpdateStateCellRecordsAndReads is the "update-state cell
// wrapping" test D13 asks for: the tiny app-owned cell behind
// web.SetLifecycleUpdateState correctly records and reads back state/version,
// and starts at the zero value (nothing observed yet).
func TestLifecycleUpdateStateCellRecordsAndReads(t *testing.T) {
	c := &lifecycleUpdateStateCell{}

	if got := c.get(); got != (web.LifecycleUpdateState{}) {
		t.Fatalf("zero value = %+v, want empty", got)
	}

	c.set("available", "v1.2.3")
	if got := c.get(); got.State != "available" || got.Version != "v1.2.3" {
		t.Errorf("after set(available): %+v", got)
	}

	c.set("failed", "v1.2.3")
	if got := c.get(); got.State != "failed" || got.Version != "v1.2.3" {
		t.Errorf("after set(failed): %+v", got)
	}

	c.set("applied", "v1.3.0")
	if got := c.get(); got.State != "applied" || got.Version != "v1.3.0" {
		t.Errorf("after set(applied): %+v", got)
	}
}

// ---- lifecyclePersistenceDecorator (task contract D4) --------------------

type fakePersistence struct {
	saveErr error
}

func (f fakePersistence) Load(context.Context) (lifecycle.LoadResult, error) {
	return lifecycle.LoadResult{}, nil
}

func (f fakePersistence) Save(context.Context, lifecycle.DesiredState, string, string) error {
	return f.saveErr
}

// TestLifecyclePersistenceDecoratorTagsSaveErrorWithSentinel is D13's
// persist-decorator test: a Save error must come back errors.Is-matchable
// against web.ErrLifecyclePersist (500-class), while the original error is
// still reachable through the same chain (errors.Join keeps both).
func TestLifecyclePersistenceDecoratorTagsSaveErrorWithSentinel(t *testing.T) {
	original := errors.New("db is busy")
	d := lifecyclePersistenceDecorator{inner: fakePersistence{saveErr: original}}

	err := d.Save(context.Background(), lifecycle.DesiredPaused, "user", "cmd-1")
	if err == nil {
		t.Fatal("Save must return the wrapped error")
	}
	if !errors.Is(err, web.ErrLifecyclePersist) {
		t.Errorf("Save error must match web.ErrLifecyclePersist, got: %v", err)
	}
	if !errors.Is(err, original) {
		t.Errorf("Save error must still match the original error, got: %v", err)
	}
}

// TestLifecyclePersistenceDecoratorPassesThroughOnSuccess: a successful Save
// must not be tagged with anything.
func TestLifecyclePersistenceDecoratorPassesThroughOnSuccess(t *testing.T) {
	d := lifecyclePersistenceDecorator{inner: fakePersistence{}}
	if err := d.Save(context.Background(), lifecycle.DesiredRunning, "user", "cmd-1"); err != nil {
		t.Fatalf("Save = %v, want nil", err)
	}
}

// ---- Ф4c end-to-end wiring through Build (task contract D1) --------------

// TestBuildWiresLifecycleControllerAndRestartRequesterIntoWeb proves
// buildWith's additive wiring actually reaches web.Server: GET /api/lifecycle
// answers 200 (not 503) with a real controller wired, and POST
// /api/lifecycle/restart-process answers 409 (not 503) — which can only
// happen if BOTH the controller AND the process-restart requester were
// wired (a nil requester alone would answer 503 regardless of the
// controller). Run() is deliberately never called (it performs device-code
// OAuth): the pre-Run zero-value snapshot is enough to prove the wiring.
func TestBuildWiresLifecycleControllerAndRestartRequesterIntoWeb(t *testing.T) {
	cfg := testConfig()
	port := reservePort(t)
	cfg.Analytics.Host = "127.0.0.1"
	cfg.Analytics.Port = port

	app, err := buildWith(context.Background(), cfg, runtimeconfig.RuntimeConfig{}, testFactories(t, nil))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { _ = app.Shutdown(context.Background()) })

	if err := app.web.Start(); err != nil {
		t.Fatalf("web.Start: %v", err)
	}
	t.Cleanup(app.web.Stop)

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	requireDialable(t, addr)

	getResp, err := http.Get("http://" + addr + "/api/lifecycle")
	if err != nil {
		t.Fatalf("GET /api/lifecycle: %v", err)
	}
	defer func() { _ = getResp.Body.Close() }()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/lifecycle status = %d, want 200 (proves SetLifecycleController wired a real controller)", getResp.StatusCode)
	}

	restartReq, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/lifecycle/restart-process", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	restartReq.Header.Set("Accept", "application/json")
	restartResp, err := http.DefaultClient.Do(restartReq)
	if err != nil {
		t.Fatalf("POST /api/lifecycle/restart-process: %v", err)
	}
	defer func() { _ = restartResp.Body.Close() }()
	// 409 (not degraded) proves the requester WAS wired — a nil requester
	// would short-circuit to 503 before ever checking Observed.
	if restartResp.StatusCode != http.StatusConflict {
		t.Fatalf("POST /api/lifecycle/restart-process status = %d, want 409 (proves SetProcessRestartRequester wired)", restartResp.StatusCode)
	}
}

// ---- requestProcessRestart / App.runCancel (task contract D6) ------------

// TestRequestProcessRestartCancelsRunScopeOnce drives a REAL *lifecycle.Controller
// through App.Run (same fake-factory harness as lifecycle_test.go, WITHOUT
// ignoreCtx: the fake generation respects ctx.Done() exactly like the real
// Miner.Run's clean-shutdown contract) and calls App.requestProcessRestart
// directly: it must cancel the ctx App.Run handed the controller — cascading
// to the live generation's own derived ctx, which is the I31 process-exit
// path — so Run returns nil, and a second call must be a safe no-op
// (idempotent — sync.Once, no panic, no double-cancel issue).
func TestRequestProcessRestartCancelsRunScopeOnce(t *testing.T) {
	rec := &recorder{}
	factory := &ctrlFakeFactory{rec: rec}
	db, store := freshLifecycleStore(t)

	a := &App{
		db:         db,
		steps:      []lifecycleStep{{name: "database", stop: closer(db.Close)}},
		controller: lifecycle.New(lifecycle.Config{Factory: factory.Factory, Persistence: store}),
	}

	runErrCh := make(chan error, 1)
	go func() { runErrCh <- a.Run(context.Background()) }()

	waitForCond(t, 5*time.Second, func() bool { return factory.count() == 1 })
	<-factory.at(0).startedCh

	a.requestProcessRestart()

	select {
	case err := <-runErrCh:
		if err != nil {
			t.Fatalf("Run = %v, want nil (clean process-restart exit)", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after requestProcessRestart")
	}

	// Idempotent: a second call (after Run already returned) must not panic
	// or block.
	a.requestProcessRestart()
}
