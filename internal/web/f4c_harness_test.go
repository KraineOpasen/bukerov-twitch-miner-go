package web

// Ф4c browser-evidence harness. Runs a real dashboard server on localhost,
// reusing the F3 fixture set (buildF3PageServer) for every non-lifecycle
// provider, plus a deterministic, harness-only fake lifecycle controller
// with dev-only control routes (devsim precedent: synchronous state changes,
// no goroutines/timers) so Playwright can drive every state the spec's
// browser tests (15/16/19) need. Env-gated: skipped unless
// MINER_F4C_HARNESS=1. Never talks to Twitch, Discord, or any network.
//
// Usage:
//   MINER_F4C_HARNESS=1 MINER_F4C_HARNESS_ADDR=127.0.0.1:8974 \
//     go test -run TestF4cEvidenceHarness -timeout 3600s ./internal/web/
//
// Dev-only control routes (registered ONLY here, never in server.go):
//   POST /api/dev/lifecycle/state?to=running|paused|stopped|failed|degraded|starting
//   POST /api/dev/lifecycle/conflict?on=1        (next command call returns 409)
//   POST /api/dev/lifecycle/persistfail?on=1     (next command call returns 500)
//   POST /api/dev/lifecycle/gen?n=<int>          (sets both the panel's and the
//                                                  status broadcaster's generation)
//   POST /api/dev/lifecycle/auth                 (generation=2 + SetAuthRequired
//                                                  fixture code, over the real SSE)
//
// The server stops when the harness receives SIGINT/SIGTERM or the timeout
// elapses.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
)

// f4cDevController is the harness's fake LifecycleController: state is set
// directly (synchronously, no goroutines) either by a command call or by the
// dev-only HTTP control routes below — the same "derive status from
// explicit, synchronous state" discipline devsim.go uses.
type f4cDevController struct {
	mu               sync.Mutex
	snap             lifecycle.Snapshot
	forceConflict    bool
	forcePersistFail bool
}

func newF4cDevController() *f4cDevController {
	return &f4cDevController{snap: lifecycle.Snapshot{
		Desired: lifecycle.DesiredRunning, Observed: lifecycle.ObservedRunning,
		Transition: lifecycle.TransitionNone,
		Capabilities: lifecycle.Capabilities{
			CanPause: true, CanRestart: true, CanStop: true,
		},
	}}
}

func (c *f4cDevController) Snapshot() lifecycle.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.snap
}

func (c *f4cDevController) command(target lifecycle.DesiredState, final lifecycle.ObservedState, caps lifecycle.Capabilities) lifecycle.SubmitResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.forceConflict {
		c.forceConflict = false
		return lifecycle.SubmitResult{Outcome: lifecycle.OutcomeRejected, Err: errors.New("process restart required")}
	}
	if c.forcePersistFail {
		c.forcePersistFail = false
		return lifecycle.SubmitResult{Outcome: lifecycle.OutcomeRejected, Err: errors.Join(ErrLifecyclePersist, errors.New("harness: simulated persist failure"))}
	}
	c.snap.Desired = target
	c.snap.Observed = final
	c.snap.Transition = lifecycle.TransitionNone
	c.snap.Generation++
	c.snap.Capabilities = caps
	c.snap.LastError = ""
	c.snap.NextRetryAt = time.Time{}
	return lifecycle.SubmitResult{Outcome: lifecycle.OutcomeAccepted, CommandID: fmt.Sprintf("dev-%d", c.snap.Generation)}
}

func (c *f4cDevController) Pause(context.Context) lifecycle.SubmitResult {
	return c.command(lifecycle.DesiredPaused, lifecycle.ObservedPaused, lifecycle.Capabilities{CanResume: true, CanRestart: true, CanStop: true})
}

func (c *f4cDevController) Resume(context.Context) lifecycle.SubmitResult {
	return c.command(lifecycle.DesiredRunning, lifecycle.ObservedRunning, lifecycle.Capabilities{CanPause: true, CanRestart: true, CanStop: true})
}

func (c *f4cDevController) Restart(context.Context) lifecycle.SubmitResult {
	return c.command(lifecycle.DesiredRunning, lifecycle.ObservedRunning, lifecycle.Capabilities{CanPause: true, CanRestart: true, CanStop: true})
}

func (c *f4cDevController) Stop(context.Context) lifecycle.SubmitResult {
	return c.command(lifecycle.DesiredStopped, lifecycle.ObservedStopped, lifecycle.Capabilities{CanResume: true, CanStop: true})
}

// setState is the dev route's direct state override — bypasses the command
// protocol entirely (this is a fixture control, not a simulated command).
func (c *f4cDevController) setState(to string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch to {
	case "running":
		c.snap = lifecycle.Snapshot{
			Desired: lifecycle.DesiredRunning, Observed: lifecycle.ObservedRunning, Transition: lifecycle.TransitionNone,
			Generation:   c.snap.Generation,
			Capabilities: lifecycle.Capabilities{CanPause: true, CanRestart: true, CanStop: true},
		}
	case "paused":
		c.snap = lifecycle.Snapshot{
			Desired: lifecycle.DesiredPaused, Observed: lifecycle.ObservedPaused, Transition: lifecycle.TransitionNone,
			Generation:   c.snap.Generation,
			Capabilities: lifecycle.Capabilities{CanResume: true, CanRestart: true, CanStop: true},
		}
	case "stopped":
		c.snap = lifecycle.Snapshot{
			Desired: lifecycle.DesiredStopped, Observed: lifecycle.ObservedStopped, Transition: lifecycle.TransitionNone,
			Generation:   c.snap.Generation,
			Capabilities: lifecycle.Capabilities{CanResume: true, CanStop: true},
		}
	case "starting":
		c.snap = lifecycle.Snapshot{
			Desired: lifecycle.DesiredRunning, Observed: lifecycle.ObservedStarting, Transition: lifecycle.TransitionStart,
			Generation: c.snap.Generation,
		}
	case "failed":
		c.snap = lifecycle.Snapshot{
			Desired: lifecycle.DesiredRunning, Observed: lifecycle.ObservedFailed, Transition: lifecycle.TransitionNone,
			Generation: c.snap.Generation, LastError: "connect: connection refused",
			NextRetryAt:  time.Now().Add(30 * time.Second),
			Capabilities: lifecycle.Capabilities{CanPause: true, CanResume: true, CanRestart: true, CanStop: true},
		}
	case "degraded":
		c.snap = lifecycle.Snapshot{
			Desired: lifecycle.DesiredPaused, Observed: lifecycle.ObservedDegraded, Transition: lifecycle.TransitionNone,
			Generation: c.snap.Generation, LastError: "shutdown drain incomplete (join timeout)",
		}
	default:
		return fmt.Errorf("unknown state %q", to)
	}
	return nil
}

func (c *f4cDevController) setConflict()    { c.mu.Lock(); c.forceConflict = true; c.mu.Unlock() }
func (c *f4cDevController) setPersistFail() { c.mu.Lock(); c.forcePersistFail = true; c.mu.Unlock() }
func (c *f4cDevController) setGeneration(n uint64) {
	c.mu.Lock()
	c.snap.Generation = n
	c.mu.Unlock()
}

// registerF4cDevLifecycleRoutes wires the dev-only control routes (harness
// ONLY — never server.go). srv is used to also drive the real status
// broadcaster (SetGeneration/SetAuthRequired) so the base.html overlay/
// banner gating is exercisable end to end, not just the panel.
func registerF4cDevLifecycleRoutes(mux *http.ServeMux, srv *Server, dev *f4cDevController) {
	mux.HandleFunc("/api/dev/lifecycle/state", func(w http.ResponseWriter, r *http.Request) {
		if err := dev.setState(r.URL.Query().Get("to")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/dev/lifecycle/conflict", func(w http.ResponseWriter, r *http.Request) {
		dev.setConflict()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/dev/lifecycle/persistfail", func(w http.ResponseWriter, r *http.Request) {
		dev.setPersistFail()
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/dev/lifecycle/gen", func(w http.ResponseWriter, r *http.Request) {
		n, err := strconv.ParseUint(r.URL.Query().Get("n"), 10, 64)
		if err != nil {
			http.Error(w, "bad n", http.StatusBadRequest)
			return
		}
		dev.setGeneration(n)
		srv.GetStatusBroadcaster().SetGeneration(n)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/dev/lifecycle/auth", func(w http.ResponseWriter, r *http.Request) {
		// Test 19 fixture: a resume that needs a fresh device-code auth at
		// generation>1 — SetGeneration BEFORE SetAuthRequired, exactly the
		// ordering design v6 §10 requires of the real controller/miner.
		srv.GetStatusBroadcaster().SetGeneration(2)
		srv.GetStatusBroadcaster().SetAuthRequired("https://www.twitch.tv/activate", "DEV1-CODE", 900)
		w.WriteHeader(http.StatusOK)
	})
	// /api/dev/status/auth-no-gen deliberately does NOT touch generation —
	// evidence for the omitempty rule ((status.generation || 1) <= 1):
	// auth_required with no generation ever published this boot must still
	// show the blocking full-screen overlay (first-boot semantics).
	mux.HandleFunc("/api/dev/status/auth-no-gen", func(w http.ResponseWriter, r *http.Request) {
		srv.GetStatusBroadcaster().SetAuthRequired("https://www.twitch.tv/activate", "FIRSTBOOT-CODE", 900)
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/dev/status/running", func(w http.ResponseWriter, r *http.Request) {
		srv.GetStatusBroadcaster().SetStatus(StatusRunning, "")
		w.WriteHeader(http.StatusOK)
	})
	// Ф4d evidence fixture: POST /api/dev/lifecycle/lan?mode=allowed|denied|unset
	// reconfigures the dashboard so Playwright can exercise all three
	// trusted-LAN panel states end to end. Every mode always sets
	// InsecureNoAuth true (the trust gate only ever fires under it);
	// "allowed"/"denied" additionally set a DASHBOARD_TRUSTED_LAN_CIDRS
	// allowlist that does/doesn't contain 127.0.0.1 (loopback — what a
	// browser driving this harness connects from). RFC1918/loopback
	// placeholders only, per governance — never a real owner address.
	mux.HandleFunc("/api/dev/lifecycle/lan", func(w http.ResponseWriter, r *http.Request) {
		switch mode := r.URL.Query().Get("mode"); mode {
		case "allowed":
			srv.SetDashboardConfig(runtimeconfig.Dashboard{
				InsecureNoAuth:  true,
				TrustedLANCIDRs: mustParseHarnessLANCIDRs("127.0.0.0/8,::1/128"),
			})
		case "denied":
			srv.SetDashboardConfig(runtimeconfig.Dashboard{
				InsecureNoAuth:  true,
				TrustedLANCIDRs: mustParseHarnessLANCIDRs("192.168.0.0/16"),
			})
		case "unset":
			srv.SetDashboardConfig(runtimeconfig.Dashboard{InsecureNoAuth: true})
		default:
			http.Error(w, "bad mode", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
}

// mustParseHarnessLANCIDRs parses a fixed, known-good CIDR literal for the
// dev harness routes above. It cannot be the shared mustLANCIDRs test
// helper (render_helpers_test.go): these calls happen inside an
// http.HandlerFunc closure serving live requests, not inside a *testing.T
// test body, so there is no *testing.T to call t.Fatalf on — it panics
// instead (never reachable in practice, since the two literals above are
// fixed and known-good; a typo here would be a harness bug, not behavior
// under test).
func mustParseHarnessLANCIDRs(raw string) []netip.Prefix {
	p, err := runtimeconfig.ParseTrustedLANCIDRs(raw)
	if err != nil {
		panic(fmt.Sprintf("f4c harness: bad fixture CIDR %q: %v", raw, err))
	}
	return p
}

// TestF4cEvidenceHarness serves the dashboard, with the full F3 fixture set
// plus the Ф4c dev lifecycle controller, for Playwright evidence capture.
func TestF4cEvidenceHarness(t *testing.T) {
	if os.Getenv("MINER_F4C_HARNESS") != "1" {
		t.Skip("harness disabled (set MINER_F4C_HARNESS=1)")
	}
	addr := os.Getenv("MINER_F4C_HARNESS_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8974"
	}

	srv := buildF3PageServer(t)

	dev := newF4cDevController()
	srv.SetLifecycleController(dev)
	srv.SetLifecycleUpdateState(func() LifecycleUpdateState {
		return LifecycleUpdateState{}
	})
	srv.SetProcessRestartRequester(func() {
		slog.Warn("F4c harness: restart-process requested (no-op — resetting to running)")
		_ = dev.setState("running")
	})

	// Report "running" so the status overlay never covers pages by default;
	// dev routes flip specific states/generation on demand.
	srv.GetStatusBroadcaster().SetStatus(StatusRunning, "")

	mux := http.NewServeMux()
	registerF4cDevLifecycleRoutes(mux, srv, dev)
	mux.Handle("/", srv.handler())

	httpSrv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()
	t.Logf("F4c harness serving on http://%s", addr)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-sig:
	case err := <-errCh:
		t.Fatalf("harness server failed to start: %v", err)
	case <-time.After(55 * time.Minute):
	}
	_ = httpSrv.Shutdown(context.Background())
}
