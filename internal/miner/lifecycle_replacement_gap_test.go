package miner

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

// TestControllerDrivenReplacementGapRefusesStaleMutation is the INTEGRATED
// stale-generation regression: it is the one test that crosses every layer the
// defect actually spans, in a single scenario, driven by the real replacement
// machinery rather than by a hand-retired miner.
//
// The other fence tests retire a generation by cancelling its context directly.
// That proves the miner-side refusal, but it never asks the component that
// really performs a replacement — lifecycle.Controller — to perform one, and it
// never holds a genuine successor generation in the window where the process
// still has the OLD generation's providers registered. This test does both.
//
// Why this window is the real defect, and why the controller creates it:
// runRestart (internal/lifecycle/worker.go) cancels generation N, then
// awaitGeneration BLOCKS until N's Run has returned — N is fully retired — and
// only then calls Factory for N+1. Miner.Run in turn reaches setupComponents,
// where a generation registers itself on the web server, only AFTER
// runAuthenticate and runLoadStreamers. So between those two facts there is a
// window in which N is retired, N+1 exists but has registered nothing, and the
// process-level web.Server therefore still routes every mutation to N. In
// production that window is as long as a Twitch outage, because
// retryStartupLookup sits inside it.
//
// This test parks generation N+1 inside its authenticate stage — deterministically,
// on a channel, never a sleep — which is precisely inside that window, and then
// issues a real HTTP POST to the real policy-mode route on the real
// process-level web.Server. Nothing calls a miner method directly: the only way
// the request can reach generation N is the ownership path under test.
//
// On the unfixed base the request is answered 200 OK, the retired generation's
// in-memory CampaignPolicy changes, and config.SaveConfig rewrites the whole of
// config.json from that retired generation's stale snapshot. All three are
// asserted here, and the test finally releases N+1 and proves the SAME endpoint
// starts succeeding again once N+1 is authoritative — so the fence is shown to
// close the window rather than to break mutation permanently.
func TestControllerDrivenReplacementGapRefusesStaleMutation(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")

	// Deliberately NOT closed: database.Open hands back a process-wide
	// singleton this package's tests share (mirrors newFenceMiner).
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	// Built fresh on every call, never copied from a shared value: a
	// generation MUTATES the config it is handed, and config.Config carries
	// maps, so a struct copy would still alias them across generations.
	newConfig := func() *config.Config {
		c := config.DefaultConfig()
		c.Username = "lifecycle_gap_tester"
		c.Streamers = nil
		c.EnableAnalytics = false
		c.Discord.Enabled = false
		c.Debug.Enabled = false
		c.CampaignPolicy = string(policy.ModeGameOrder)
		c.Analytics.Host = "127.0.0.1"
		c.Analytics.Port = port
		return &c
	}
	seed := newConfig()

	if err := config.SaveConfig(configPath, seed); err != nil {
		t.Fatalf("seed config.json: %v", err)
	}

	// ONE process-level web server, built before any generation and outliving
	// all of them — the ownership App has in production, and the reason a
	// retired generation stays reachable at all.
	ws := web.NewServerEarly(seed.Analytics, seed.Username, seed.StorageKey(), nil)
	if ws == nil {
		t.Fatal("web.NewServerEarly returned nil")
	}
	if err := ws.Start(); err != nil {
		t.Fatalf("web server Start: %v", err)
	}
	t.Cleanup(ws.Stop)

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	baseURL := "http://" + addr
	waitDialable(t, addr)

	genOneCh := make(chan *Miner, 1)
	genOneReady := make(chan struct{})
	genTwoEntered := make(chan struct{})
	genTwoRelease := make(chan struct{})
	genTwoReady := make(chan struct{})

	// Generation ordinals as the Factory sees them. Any generation beyond
	// N+1 (a retry lineage) is deliberately left unparked and inert.
	const (
		genN      = 0
		genNPlus1 = 1
	)
	built := genN // Factory is called only from the worker goroutine
	//             (internal/lifecycle/worker.go:633), so this needs no lock.

	// The Factory runs on the worker goroutine, which can outlive a failing
	// test; reporting through t there risks logging after the test completed.
	// Errors are recorded and asserted on the test's own goroutine instead.
	loadErrs := make(chan error, 4)

	factory := func() lifecycle.Runner {
		// Each generation starts from the last COMMITTED config. Note this is
		// deliberately NOT how App.nextGenerationConfig sources it: App samples
		// the OUTGOING miner's in-memory CurrentConfig() (internal/app/app.go),
		// whereas this reads the file. Reading the file is the stricter choice
		// here — it means the post-gap assertions observe what actually reached
		// disk rather than a value inherited from the generation under test.
		cfg, err := config.LoadConfig(configPath)
		if err != nil {
			loadErrs <- err
			cfg = newConfig()
		}
		m := New(cfg, configPath)
		m.SetDatabase(db)
		m.SetWebServer(ws)
		stubAuthenticate(m)
		stubLoadStreamers(m)
		m.subscribeTopicsFn = func() error { return nil }

		switch built {
		case genN:
			m.startMiningFn = func(context.Context) { close(genOneReady) }
			genOneCh <- m
		case genNPlus1:
			// Park generation N+1 in authenticate — which Run calls BEFORE
			// setupComponents, so this generation has registered NOTHING on
			// the web server while it waits here. Deterministic: a channel,
			// not a sleep.
			inner := m.authenticateFn
			m.authenticateFn = func(ctx context.Context) error {
				close(genTwoEntered)
				// Honour ctx: if an assertion below fails while N+1 is parked
				// here, the cleanup's cancel() must be able to unwedge it, or
				// the worker's teardown drain never returns and this goroutine
				// leaks past the end of the test.
				select {
				case <-genTwoRelease:
				case <-ctx.Done():
					return ctx.Err()
				}
				return inner(ctx)
			}
			m.startMiningFn = func(context.Context) { close(genTwoReady) }
		default:
			m.startMiningFn = func(context.Context) {}
		}
		built++
		return m
	}

	sink := &gapStatusSink{statuses: make(chan string, 64)}
	ctrl := lifecycle.New(lifecycle.Config{Factory: factory, StatusSink: sink})

	// Wire the controller into the web server EXACTLY as App does
	// (internal/app/app.go:480). This is load-bearing, not decoration:
	// handleAPIPolicyMode consults lifecycleMutationBlocked() BEFORE it samples
	// the provider, so without this wiring the request could never even be
	// gated and the test would prove strictly less than production needs.
	ws.SetLifecycleController(ctrl)
	ctx, cancel := context.WithCancel(context.Background())
	ctrlDone := make(chan error, 1)
	go func() { ctrlDone <- ctrl.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-ctrlDone:
		case <-time.After(10 * time.Second):
			t.Error("lifecycle.Controller.Run did not return after cancellation")
		}
	})

	// Generation N is up, having run the REAL setupComponents and therefore
	// registered itself on the process-level web server.
	select {
	case <-genOneReady:
	case <-time.After(15 * time.Second):
		t.Fatal("generation N never finished starting")
	}
	genOne := <-genOneCh

	before := diskPolicy(t, configPath)
	if got, want := before, string(policy.ModeGameOrder); got != want {
		t.Fatalf("seeded on-disk CampaignPolicy = %q, want %q", got, want)
	}

	// Drain every status generation N has published BEFORE asking for the
	// restart, so the "running" awaited below is unambiguously N+1's. Without
	// this the barrier is decorative: N's own launch already published
	// "running" into the buffer, awaitStatus would consume THAT and return
	// without waiting for the restart transition to clear at all.
	sink.drain()

	// A REAL controller-driven replacement. Submit does not block on the
	// worker (its cmdCh send is non-blocking), but it is run on its own
	// goroutine anyway so the test can never wedge if that ever changes.
	restarted := make(chan lifecycle.SubmitResult, 1)
	go func() { restarted <- ctrl.Restart(context.Background()) }()

	// Generation N has now fully retired (runRestart awaited its Run before
	// building N+1) and generation N+1 is parked short of setupComponents.
	// This is the replacement gap, held open deterministically.
	select {
	case <-genTwoEntered:
	case <-time.After(15 * time.Second):
		t.Fatal("generation N+1 never reached its authenticate stage")
	}

	// Wait for the restart transition to COMPLETE while N+1 is still parked.
	// *Miner does not implement lifecycle.ReadySignaler, so the worker treats a
	// generation as ready the moment its goroutine is launched — observed flips
	// back to "running" and Snapshot().Transition returns to TransitionNone
	// while N+1 is still short of setupComponents. That is what makes this
	// window dangerous in production: the handler's lifecycleMutationBlocked()
	// gate is OPEN, so it does not answer 409, and the request goes on to
	// sample a provider that still belongs to the RETIRED generation. The
	// miner-side fence is the only thing left that can refuse it — exactly the
	// "unavoidable race between checking here and mutating there" that
	// handlers_policy.go documents.
	awaitStatus(t, sink, string(lifecycle.ObservedRunning))

	resp := postForm(t, baseURL, "/api/policy/mode", url.Values{
		"mode": {string(policy.ModeSmart)},
	})

	if resp.StatusCode == http.StatusOK {
		t.Errorf("POST /api/policy/mode during the replacement gap = %d OK; "+
			"the only generation reachable here is RETIRED and must never "+
			"acknowledge a mutation", resp.StatusCode)
	}
	if resp.StatusCode == http.StatusConflict {
		t.Errorf("status = 409; that is lifecycleMutationBlocked's transition gate, " +
			"which is UX sugar and is OPEN here by construction — this assertion " +
			"exists so the test can never pass by accidentally being refused " +
			"upstream of the fence it is meant to exercise")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d (ErrShuttingDown is this repo's fail-closed "+
			"drain refusal — see writeApplyError, internal/web/handlers_settings.go)",
			resp.StatusCode, http.StatusServiceUnavailable)
	}

	if live, _ := genOne.CurrentCampaignPolicy(); live != string(policy.ModeGameOrder) {
		t.Errorf("retired generation's in-memory CampaignPolicy = %q, want %q "+
			"(a refused mutation must not change runtime config)", live, policy.ModeGameOrder)
	}
	if after := diskPolicy(t, configPath); after != before {
		t.Errorf("config.json CampaignPolicy = %q, want %q unchanged "+
			"(a refused mutation must never reach disk, and a retired "+
			"generation's write would rewrite the WHOLE document from its "+
			"stale snapshot)", after, before)
	}

	// Release N+1 and let it become authoritative: the fence must close the
	// window, not disable mutation for good.
	close(genTwoRelease)
	select {
	case <-genTwoReady:
	case <-time.After(15 * time.Second):
		t.Fatal("generation N+1 never finished starting after release")
	}
	select {
	case res := <-restarted:
		if res.Outcome != lifecycle.OutcomeAccepted {
			t.Fatalf("Restart outcome = %q (err %v), want %q",
				res.Outcome, res.Err, lifecycle.OutcomeAccepted)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Restart never returned")
	}

	for {
		select {
		case err := <-loadErrs:
			t.Errorf("generation config load: %v", err)
			continue
		default:
		}
		break
	}

	resumed := postForm(t, baseURL, "/api/policy/mode", url.Values{
		"mode": {string(policy.ModeSmart)},
	})
	if resumed.StatusCode != http.StatusOK {
		t.Errorf("POST /api/policy/mode against the NEW authoritative generation = %d, "+
			"want %d — normal mutation must resume once N+1 owns the providers",
			resumed.StatusCode, http.StatusOK)
	}
	if got := diskPolicy(t, configPath); got != string(policy.ModeSmart) {
		t.Errorf("config.json CampaignPolicy = %q, want %q — the LIVE generation's "+
			"write must land", got, policy.ModeSmart)
	}
}

// gapStatusSink records the controller's observed-state notifications so the
// test can await a transition's COMPLETION as an event instead of racing it.
// The channel is generously buffered: SetStatus runs on the controller's
// worker goroutine (outside statusMu) and must never block on this test.
type gapStatusSink struct{ statuses chan string }

func (s *gapStatusSink) SetStatus(status, _ string) {
	select {
	case s.statuses <- status:
	default:
	}
}

func (s *gapStatusSink) SetGeneration(uint64) {}

// drain discards every status published so far, so a subsequent awaitStatus
// can only be satisfied by a status published AFTER this call.
func (s *gapStatusSink) drain() {
	for {
		select {
		case <-s.statuses:
		default:
			return
		}
	}
}

// awaitStatus blocks until the sink reports the wanted observed state.
func awaitStatus(t *testing.T, sink *gapStatusSink, want string) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case got := <-sink.statuses:
			if got == want {
				return
			}
		case <-deadline:
			t.Fatalf("controller never reported observed=%q", want)
		}
	}
}
