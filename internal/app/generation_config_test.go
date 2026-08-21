package app

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/lifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/miner"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/runtimeconfig"
)

// These tests pin the generation-config contract: every NEW in-process miner
// generation must start from the configuration the PREVIOUS generation
// actually committed, never from the snapshot the process happened to boot
// with.
//
// Before this pass, Build's miner factory captured the boot *config.Config
// once and handed that same pointer to every later generation, while a
// successful runtime settings apply publishes a fresh candidate pointer
// (miner.go's applySettings* paths, health.go's ApplyHealthSettings). A
// setting changed at runtime therefore survived only until the next
// pause/resume or restart, and silently reverted for generation N+1 — a full
// process restart re-read config.json and hid the defect entirely.
//
// The seam under test is the real one where it matters: a real
// *lifecycle.Controller drives a real in-process generation replacement, and
// every generation is built by the REAL app.minerFactory — the function that
// actually decides which configuration a generation starts from. What each
// generation does NOT do is run: *miner.Miner.Run performs device-code OAuth
// and opens Twitch connections, which no unit test can reach and which is
// irrelevant to which configuration the generation was constructed from, so
// the harness supplies a controllable Runner in its place.
//
// Be precise about what that costs, so nobody reads more coverage into these
// tests than they have. The harness builds its own lifecycle.Controller and
// installs it on the App, rather than driving the one buildWith constructed.
// So buildWith's own Factory closure, its persistence decorator, status sink,
// ForceRunning/NoControlSurface flags and updater wiring are NOT exercised
// here — only app.minerFactory beneath them is. Two consequences worth
// knowing: a regression in that closure itself would not be caught by this
// file, and the web server stays wired to the original, never-Run controller,
// so this harness is not a faithful stand-in for dashboard-driven pause/resume
// if anyone extends it that way.

// genHarness drives real generations built by a real App's real minerFactory
// through a real lifecycle.Controller.
type genHarness struct {
	t          *testing.T
	app        *App
	controller *lifecycle.Controller
	configPath string

	mu      sync.Mutex
	miners  []*miner.Miner
	runners []*ctrlFakeRunner
}

// newGenHarness seeds config.json with boot, builds a real App over it, and
// wires a real controller whose Factory calls the App's OWN minerFactory —
// the same delegation buildWith performs at app.go's lifecycle.New call.
func newGenHarness(t *testing.T, boot *config.Config) *genHarness {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(configPath, boot); err != nil {
		t.Fatalf("seed config.json: %v", err)
	}

	rc := runtimeconfig.RuntimeConfig{ConfigPath: configPath}
	var db *database.DB
	a, err := buildWith(context.Background(), boot, rc, testFactories(t, &db))
	if err != nil {
		t.Fatalf("buildWith: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown(context.Background()) })

	h := &genHarness{t: t, app: a, configPath: configPath}

	// The controller is this harness's own, because its Factory must return a
	// controllable Runner rather than run the real miner — but it reuses the
	// database buildWith already opened rather than paying for a second one.
	store, err := lifecycle.NewStore(db)
	if err != nil {
		t.Fatalf("lifecycle.NewStore: %v", err)
	}
	h.controller = lifecycle.New(lifecycle.Config{
		Factory: func() lifecycle.Runner {
			m := a.minerFactory()
			r := &ctrlFakeRunner{startedCh: make(chan struct{}), finishCh: make(chan error, 1)}
			h.mu.Lock()
			h.miners = append(h.miners, m)
			h.runners = append(h.runners, r)
			h.mu.Unlock()
			return r
		},
		Persistence: store,
	})
	a.controller = h.controller
	return h
}

func (h *genHarness) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.miners)
}

func (h *genHarness) generation(i int) *miner.Miner {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if i >= len(h.miners) {
		h.t.Fatalf("generation %d was never built (only %d exist)", i+1, len(h.miners))
	}
	return h.miners[i]
}

func (h *genHarness) runner(i int) *ctrlFakeRunner {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if i >= len(h.runners) {
		h.t.Fatalf("generation %d was never built (only %d exist)", i+1, len(h.runners))
	}
	return h.runners[i]
}

// start runs the App and waits for generation 1 to be running.
func (h *genHarness) start() (stop func()) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runErrCh := make(chan error, 1)
	go func() { runErrCh <- h.app.Run(ctx) }()

	// ONE stop, idempotent, registered as a cleanup BEFORE any t.Fatal below
	// can fire. A fatal between the goroutine spawn and this function's return
	// would otherwise leave ctx uncancelled: App.Run would keep driving
	// generations while newGenHarness's own cleanup closed the database
	// underneath it, and the resulting failure would surface in whichever test
	// ran next. sync.Once is what lets the caller's own `defer stop()` and this
	// cleanup both fire — the second call must not wait again on a channel the
	// first already drained.
	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-runErrCh:
				if err != nil {
					h.t.Errorf("App.Run = %v, want nil (clean shutdown)", err)
				}
			case <-time.After(5 * time.Second):
				h.t.Error("App.Run did not return after cancel")
			}
		})
	}
	h.t.Cleanup(stop)

	waitForCond(h.t, 5*time.Second, func() bool { return h.count() == 1 })
	select {
	case <-h.runner(0).startedCh:
	case <-time.After(5 * time.Second):
		h.t.Fatal("generation 1 never started")
	}

	return stop
}

// replaceGeneration performs a REAL in-process generation replacement
// (pause -> teardown -> resume) and waits for generation want to be running.
func (h *genHarness) replaceGeneration(want int) {
	h.t.Helper()
	prev := want - 1

	if res := h.controller.Pause(context.Background()); res.Outcome != lifecycle.OutcomeAccepted {
		h.t.Fatalf("pause: %v (%v)", res.Outcome, res.Err)
	}
	h.runner(prev - 1).finishCh <- nil
	waitForCond(h.t, 5*time.Second, func() bool {
		return h.controller.Snapshot().Observed == lifecycle.ObservedPaused
	})

	if res := h.controller.Resume(context.Background()); res.Outcome != lifecycle.OutcomeAccepted {
		h.t.Fatalf("resume: %v (%v)", res.Outcome, res.Err)
	}
	waitForCond(h.t, 5*time.Second, func() bool { return h.count() == want })
	select {
	case <-h.runner(want - 1).startedCh:
	case <-time.After(5 * time.Second):
		h.t.Fatalf("generation %d never started", want)
	}
}

// bootConfigWithCanary builds a valid boot config carrying a recognizable
// marker in a field a REAL runtime settings commit path can change.
func bootConfigWithCanary(canary string) *config.Config {
	c := testConfig()
	c.Health.CanaryChannel = canary
	return c
}

// commitCanary drives the REAL runtime settings commit path
// (*miner.Miner.ApplyHealthSettings — the exact entry point
// internal/web's POST /api/health/settings handler calls): it clones the live
// config, persists the candidate to config.json, and only then publishes it.
func commitCanary(t *testing.T, m *miner.Miner, canary string) error {
	t.Helper()
	hs := m.CurrentHealthSettings()
	hs.CanaryChannel = canary
	return m.ApplyHealthSettings(hs)
}

// TestNewGenerationStartsFromLastCommittedConfig is the core regression: a
// runtime settings change that was successfully committed by generation 1
// must still be in force in generation 2, WITHOUT a process restart.
func TestNewGenerationStartsFromLastCommittedConfig(t *testing.T) {
	const (
		bootCanary      = "boot-config-A"
		committedCanary = "committed-config-B"
	)

	h := newGenHarness(t, bootConfigWithCanary(bootCanary))
	stop := h.start()
	defer stop()

	// B — generation 1 starts from the boot configuration.
	gen1 := h.generation(0)
	if got := gen1.CurrentHealthSettings().CanaryChannel; got != bootCanary {
		t.Fatalf("generation 1 CanaryChannel = %q, want the boot value %q", got, bootCanary)
	}

	// C — a real settings commit changes A -> B and persists it.
	if err := commitCanary(t, gen1, committedCanary); err != nil {
		t.Fatalf("ApplyHealthSettings: %v", err)
	}
	onDisk, err := config.LoadConfig(h.configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if onDisk.Health.CanaryChannel != committedCanary {
		t.Fatalf("commit did not reach config.json: CanaryChannel = %q, want %q",
			onDisk.Health.CanaryChannel, committedCanary)
	}

	// D — the running generation observes the committed value.
	if got := gen1.CurrentHealthSettings().CanaryChannel; got != committedCanary {
		t.Fatalf("generation 1 CanaryChannel = %q after its own commit, want %q", got, committedCanary)
	}

	// E/F — a REAL in-process generation replacement builds generation 2.
	h.replaceGeneration(2)

	// G/H — generation 2 must start from the COMMITTED configuration.
	if got := h.generation(1).CurrentHealthSettings().CanaryChannel; got != committedCanary {
		t.Errorf("generation 2 CanaryChannel = %q, want the committed value %q; "+
			"a new in-process generation was built from the stale process-boot configuration, "+
			"so the runtime settings change silently reverted without a process restart",
			got, committedCanary)
	}
}

// breakConfigPath replaces config.json with a directory so the next
// config.SaveConfig fails at its atomic rename — the same deterministic
// persistence-failure seam internal/miner's own commit-point tests use
// (cp1_c2_matrix_test.go's breakConfigPathForNextSave).
func breakConfigPath(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// TestRejectedSettingsCandidateNeverReachesNextGeneration pins the fail-closed
// half of the contract for the CANDIDATE-PUBLISHING paths. For those,
// persistence is the commit point: when config.json cannot be written the
// apply is rejected, nothing is published, and the rejected candidate stays
// invisible — including to a generation built after the failed attempt. What
// the next generation gets is the last successfully committed configuration,
// never the rejected one and never the process-boot one.
//
// This is deliberately a claim about the candidate-publishing paths;
// TestRejectedInPlaceWriteNeverReachesNextGeneration below pins the SAME
// fail-closed contract for the in-place writers.
func TestRejectedSettingsCandidateNeverReachesNextGeneration(t *testing.T) {
	const (
		bootCanary      = "boot-config-A"
		committedCanary = "committed-config-B"
		rejectedCanary  = "rejected-config-C"
	)

	h := newGenHarness(t, bootConfigWithCanary(bootCanary))
	stop := h.start()
	defer stop()

	gen1 := h.generation(0)
	if err := commitCanary(t, gen1, committedCanary); err != nil {
		t.Fatalf("ApplyHealthSettings (committed): %v", err)
	}

	// A candidate that cannot be persisted must be rejected outright.
	breakConfigPath(t, h.configPath)
	if err := commitCanary(t, gen1, rejectedCanary); err == nil {
		t.Fatal("ApplyHealthSettings returned nil after config.json persistence failed; " +
			"a rejected candidate must never be reported as applied")
	}
	if got := gen1.CurrentHealthSettings().CanaryChannel; got != committedCanary {
		t.Fatalf("generation 1 CanaryChannel = %q after a REJECTED apply, want the last committed %q",
			got, committedCanary)
	}

	h.replaceGeneration(2)

	switch got := h.generation(1).CurrentHealthSettings().CanaryChannel; got {
	case committedCanary:
		// correct: the last successfully committed configuration
	case rejectedCanary:
		t.Errorf("generation 2 CanaryChannel = %q — a candidate whose persistence FAILED leaked into a later generation", got)
	default:
		t.Errorf("generation 2 CanaryChannel = %q, want the last committed value %q", got, committedCanary)
	}
}

// TestGenerationConfigHandoffChainsAcrossGenerations pins that the handoff is
// a chain, not a one-shot: each generation commits, and the generation after
// it starts from THAT commit. A design that merely remembered the first
// generation's result would pass the two-generation case and fail here.
func TestGenerationConfigHandoffChainsAcrossGenerations(t *testing.T) {
	h := newGenHarness(t, bootConfigWithCanary("boot-config-A"))
	stop := h.start()
	defer stop()

	for _, step := range []struct {
		commit string
		next   int
	}{
		{commit: "committed-by-gen-1", next: 2},
		{commit: "committed-by-gen-2", next: 3},
		{commit: "committed-by-gen-3", next: 4},
	} {
		current := h.generation(step.next - 2)
		if err := commitCanary(t, current, step.commit); err != nil {
			t.Fatalf("generation %d ApplyHealthSettings: %v", step.next-1, err)
		}
		h.replaceGeneration(step.next)
		if got := h.generation(step.next - 1).CurrentHealthSettings().CanaryChannel; got != step.commit {
			t.Fatalf("generation %d CanaryChannel = %q, want %q committed by generation %d",
				step.next, got, step.commit, step.next-1)
		}
	}
}

// TestGenerationConfigWithoutConfigPathStillHandsOffCommit pins the
// documented configPath == "" semantics across the handoff. It exercises the
// factory directly rather than through a controller: with no config file there
// is nothing for a lifecycle replacement to add to the question, and building
// an App with no ConfigPath is the whole point of the case.
// With no config file configured, a settings apply persists nothing and is
// still a plain success (internal/miner's
// TestApplySettingsNoRenameWithoutConfigPathStaysSuccessful pins that for the
// apply itself) — so the value it publishes is a real commit, and the next
// generation must start from it. Nothing about this path may depend on there
// being a file on disk to read back.
func TestGenerationConfigWithoutConfigPathStillHandsOffCommit(t *testing.T) {
	const committedCanary = "committed-without-config-file"

	boot := bootConfigWithCanary("boot-config-A")
	a, err := buildWith(context.Background(), boot, runtimeconfig.RuntimeConfig{}, testFactories(t, nil))
	if err != nil {
		t.Fatalf("buildWith: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown(context.Background()) })

	gen1 := a.minerFactory()
	if err := commitCanary(t, gen1, committedCanary); err != nil {
		t.Fatalf("ApplyHealthSettings with no config path must succeed, got: %v", err)
	}

	gen2 := a.minerFactory()
	if got := gen2.CurrentHealthSettings().CanaryChannel; got != committedCanary {
		t.Errorf("generation 2 CanaryChannel = %q, want %q; with configPath == \"\" the apply is a "+
			"documented no-op persist but still a real commit, so the next generation must observe it",
			got, committedCanary)
	}
}

// TestGenerationConfigCarriesInPlaceRuntimeMutations covers the OTHER shape a
// runtime settings change takes in internal/miner: ApplyCampaignPolicy (a
// plain string field) and SetDropRule (a map entry) mutate the live config in
// place under m.mu and persist it, rather than publishing a fresh candidate
// the way the Settings and Health paths do. Both shapes are commits, and both must survive a generation
// replacement — a fix that only followed republished pointers would leave
// this half silently reverting once any candidate-publishing apply had run.
func TestGenerationConfigCarriesInPlaceRuntimeMutations(t *testing.T) {
	h := newGenHarness(t, bootConfigWithCanary("boot-config-A"))
	stop := h.start()
	defer stop()

	gen1 := h.generation(0)

	// First a candidate-publishing commit, so the live config is no longer
	// the boot object — this is precisely the state in which an in-place
	// mutation used to be lost.
	if err := commitCanary(t, gen1, "committed-config-B"); err != nil {
		t.Fatalf("ApplyHealthSettings: %v", err)
	}

	before, _ := gen1.CurrentCampaignPolicy()
	var want string
	for _, candidate := range []string{
		string(policy.ModeEndingSoonest),
		string(policy.ModeClosestToReward),
		string(policy.ModeSmart),
	} {
		if candidate != before {
			want = candidate
			break
		}
	}
	// A live (non-retired) generation must be ADMITTED by the mutation fence;
	// a refusal here would mean the fence is over-refusing, not that the
	// handoff broke.
	if err := gen1.ApplyCampaignPolicy(want); err != nil {
		t.Fatalf("ApplyCampaignPolicy on a live generation = %v, want nil", err)
	}
	if got, _ := gen1.CurrentCampaignPolicy(); got != want {
		t.Fatalf("generation 1 CampaignPolicy = %q after ApplyCampaignPolicy(%q), want %q", got, want, want)
	}

	h.replaceGeneration(2)

	if got, _ := h.generation(1).CurrentCampaignPolicy(); got != want {
		t.Errorf("generation 2 CampaignPolicy = %q, want %q committed in place by generation 1", got, want)
	}
	if got := h.generation(1).CurrentHealthSettings().CanaryChannel; got != "committed-config-B" {
		t.Errorf("generation 2 CanaryChannel = %q, want %q", got, "committed-config-B")
	}
}

// TestGenerationConfigHandsOverAnIsolatedSnapshot pins the isolation half of
// the handoff, and it is a memory-safety contract rather than a stylistic one.
//
// A generation stays reachable after its Run returns: the web providers a
// generation registers are never cleared, so the dashboard keeps routing to a
// torn-down generation until the next one finishes authenticating. Two of
// those still-reachable entry points — SetDropRule here and SetAutoRedeem in
// rewards.go — write MAPS inside the live config in place, under their OWN
// miner's lock. (Per-map isolation is pinned directly in internal/miner's
// current_config_test.go; this test pins it at the generation boundary.) Handing the live *config.Config to the
// next generation would therefore put one map behind two different mutexes,
// which Go answers with a `concurrent map read and map write` fatal throw that
// no recover can catch.
//
// So the observable contract is: a write landing on the OUTGOING generation
// after the handoff must not be visible in the generation that was handed the
// configuration. That is a lost update — which the doc on
// App.nextGenerationConfig records honestly — and it is strictly what we want
// over a shared map.
func TestGenerationConfigHandsOverAnIsolatedSnapshot(t *testing.T) {
	const lateRule = "rule-written-after-the-handoff"

	h := newGenHarness(t, bootConfigWithCanary("boot-config-A"))
	stop := h.start()
	defer stop()

	gen1 := h.generation(0)
	// Seed one rule BEFORE the handoff: it must carry over, proving the
	// snapshot copies content rather than dropping the map.
	if err := gen1.SetDropRule("rule-committed-before-the-handoff", config.DropRule{HighPriority: true}); err != nil {
		t.Fatalf("SetDropRule on a live generation = %v, want nil", err)
	}

	h.replaceGeneration(2)
	gen2 := h.generation(1)

	if _, rules := gen2.CurrentCampaignPolicy(); len(rules) != 1 {
		t.Fatalf("generation 2 DropRules = %v, want the single rule generation 1 committed before the handoff", rules)
	}

	// Now write through the OUTGOING generation, exactly as a dashboard
	// request routed to the stale provider would.
	// gen1's Run was never started by this harness (it supplies a controllable
	// Runner in the miner's place), so gen1 is not RETIRED in the miner's own
	// terms and the fence admits this write. That is what makes this test
	// still about the config handoff rather than about the fence.
	if err := gen1.SetDropRule(lateRule, config.DropRule{Skip: true}); err != nil {
		t.Fatalf("SetDropRule on a never-run generation = %v, want nil", err)
	}

	if _, rules := gen1.CurrentCampaignPolicy(); !rules[lateRule].Skip {
		t.Fatalf("generation 1 did not record its own late write: %v", rules)
	}
	if _, rules := gen2.CurrentCampaignPolicy(); len(rules) != 1 {
		t.Errorf("generation 2 DropRules = %v — the running generation shares a live map with the "+
			"torn-down one, so an in-place write on the old generation reaches the new one's config "+
			"under a different mutex", rules)
	}
}

// TestRejectedInPlaceWriteNeverReachesNextGeneration pins the fail-closed
// half of the contract for the IN-PLACE writers, completing what
// TestRejectedSettingsCandidateNeverReachesNextGeneration pins for the
// candidate-publishing paths.
//
// DELIBERATE CHARACTERIZATION UPDATE. This test replaces
// TestInPlaceRuntimeWriteSurvivesAFailedPersist, which characterized the
// OPPOSITE behavior — SetDropRule/ApplyCampaignPolicy mutating the live
// config, persistLocked only LOGGING a SaveConfig failure, the change
// staying live, and the handoff carrying the never-committed value into the
// next generation — and whose own comment said that if those writers were
// ever made fail-closed, "this test SHOULD fail, and updating it is then the
// deliberate act of recording that decision." That is exactly what happened:
// the in-place writers now make persistence the commit point (exact rollback
// under m.mu, non-nil error, no refresh from the rejected value —
// internal/miner/policy.go), so this assertion intentionally inverts to
// record the improved contract.
//
// The observable chain pinned here: a persist-failed in-place write returns
// an error, leaves the outgoing generation at its prior committed state, and
// therefore can never be inherited by generation N+1 through the
// CurrentConfig handoff. The success half — an ACKNOWLEDGED in-place write
// does reach generation N+1 — stays pinned by
// TestGenerationConfigCarriesInPlaceRuntimeMutations above, unchanged.
func TestRejectedInPlaceWriteNeverReachesNextGeneration(t *testing.T) {
	const rejectedRule = "rule-whose-save-failed"

	h := newGenHarness(t, bootConfigWithCanary("boot-config-A"))
	stop := h.start()
	defer stop()

	gen1 := h.generation(0)

	// A committed rule first, so the rejection below is shown to preserve —
	// not merely empty out — the prior committed state.
	if err := gen1.SetDropRule("committed-rule", config.DropRule{Skip: true}); err != nil {
		t.Fatalf("SetDropRule (committed) = %v, want nil", err)
	}

	// Make the next persist fail, then attempt the in-place runtime write.
	breakConfigPath(t, h.configPath)
	if err := gen1.SetDropRule(rejectedRule, config.DropRule{HighPriority: true}); err == nil {
		t.Fatal("SetDropRule = nil after config.json persistence failed; " +
			"an in-place policy write must fail closed, not acknowledge a change that never became durable")
	}

	// The rejected value must not be live on the outgoing generation.
	if _, rules := gen1.CurrentCampaignPolicy(); rules[rejectedRule].HighPriority {
		t.Fatalf("generation 1 kept the REJECTED in-place write: %v", rules)
	}
	// And nothing reached disk: the directory breakConfigPath installed is
	// still a directory, exactly as internal/miner's own commit-point tests
	// assert.
	if info, err := os.Stat(h.configPath); err != nil || !info.IsDir() {
		t.Fatalf("configPath must still be the untouched directory (nothing was written): stat=%v err=%v", info, err)
	}

	h.replaceGeneration(2)

	_, rules := h.generation(1).CurrentCampaignPolicy()
	if rules[rejectedRule].HighPriority {
		t.Errorf("generation 2 DropRules = %v — a rejected in-place write leaked into a later "+
			"generation through the CurrentConfig handoff", rules)
	}
	if !rules["committed-rule"].Skip {
		t.Errorf("generation 2 DropRules = %v — the COMMITTED rule must survive the handoff", rules)
	}
}
