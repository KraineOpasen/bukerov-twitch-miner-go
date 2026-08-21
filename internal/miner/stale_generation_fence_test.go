package miner

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/web"
)

// newFenceMiner builds a miner over a REAL config.json path, so a mutation
// that persists is observable on disk exactly as production would leave it.
// Unlike newStartupCleanupMiner it does not t.Chdir, because the test asserts
// on an absolute config path.
func newFenceMiner(t *testing.T) (*Miner, string) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")

	// Deliberately NOT closed here: database.Open hands back a process-wide
	// singleton that the rest of this package's tests share, so closing it
	// would break whichever test ran next (mirrors newStartupCleanupMiner).
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Username = "stale_generation_tester"
	cfg.Streamers = nil
	cfg.EnableAnalytics = false
	cfg.Discord.Enabled = false
	cfg.Debug.Enabled = false
	cfg.CampaignPolicy = string(policy.ModeGameOrder)

	if err := config.SaveConfig(configPath, &cfg); err != nil {
		t.Fatalf("seed config.json: %v", err)
	}

	m := New(&cfg, configPath)
	m.SetDatabase(db)
	return m, configPath
}

// startRetiredGeneration drives ONE miner generation through the real Run
// control flow — real setupComponents, so the miner registers itself on the
// real web.Server exactly as production does — then cancels it and waits for
// Run to return. What it hands back is a genuinely RETIRED generation still
// reachable through the process-level web server, which is precisely the
// state a lifecycle generation replacement leaves behind while the incoming
// generation is still short of its own setupComponents.
//
// Synchronization is deterministic throughout: the startMiningFn seam closes
// a channel when startup completed, and Run's return is awaited on a channel.
// No wall-clock sleep decides the outcome of any assertion.
func startRetiredGeneration(t *testing.T, m *Miner) (baseURL string) {
	t.Helper()
	baseURL, retire := startLiveGeneration(t, m)
	retire()
	return baseURL
}

// startLiveGeneration brings ONE generation up through the real Run control
// flow and leaves it LIVE and authoritative. The returned retire() cancels the
// run context and waits for Run — and therefore the full teardown — to return,
// which is what genuinely retires the generation.
func startLiveGeneration(t *testing.T, m *Miner) (baseURL string, retire func()) {
	t.Helper()

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	_ = probe.Close()

	m.config.Analytics.Host = "127.0.0.1"
	m.config.Analytics.Port = port

	ws := web.NewServerEarly(m.config.Analytics, m.config.Username, m.config.StorageKey(), nil)
	if ws == nil {
		t.Fatal("web.NewServerEarly returned nil")
	}
	if err := ws.Start(); err != nil {
		t.Fatalf("web server Start: %v", err)
	}
	t.Cleanup(ws.Stop)
	m.SetWebServer(ws)

	baseURL = "http://" + net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	waitDialable(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))

	stubAuthenticate(m)
	stubLoadStreamers(m)
	m.subscribeTopicsFn = func() error { return nil }

	// Network-free stand-in for startMining's loop-starting section, matching
	// TestProvidersSafeAfterTeardown: setupComponents itself is NEVER stubbed,
	// so the real provider registration under test has already happened by the
	// time this runs.
	started := make(chan struct{})
	m.startMiningFn = func(context.Context) { close(started) }

	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- m.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("generation startup did not complete")
	}

	var once sync.Once
	retire = func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-runErr:
				if err != nil {
					t.Errorf("Run after cancellation = %v, want nil", err)
				}
			case <-time.After(10 * time.Second):
				t.Error("Run did not return after cancellation")
			}
		})
	}
	t.Cleanup(retire)

	return baseURL, retire
}

func waitDialable(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("web server not reachable: %v", err)
		}
	}
}

func postForm(t *testing.T, baseURL, path string, form url.Values) *http.Response {
	t.Helper()
	resp, err := http.PostForm(baseURL+path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func diskPolicy(t *testing.T, configPath string) string {
	t.Helper()
	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("re-read config.json: %v", err)
	}
	return onDisk.CampaignPolicy
}

// TestRetiredGenerationRefusesPolicyModeMutationOverHTTP is the primary
// regression for the stale-generation mutation fence.
//
// It crosses the REAL ownership path the bug lives on — a real HTTP request
// to the real POST /api/policy/mode route on the real process-level
// web.Server, which samples the PolicyProvider that a now-RETIRED miner
// generation registered on it during its own setupComponents. Nothing here
// calls the miner's mutation method directly; the only way the request can
// reach the retired generation is the ownership path under test.
//
// On the unfixed base the request is served 200 with a re-rendered Drops
// list, the retired generation's in-memory CampaignPolicy is changed, and
// config.SaveConfig rewrites the WHOLE of config.json from that retired
// generation's snapshot. All three are asserted against here.
func TestRetiredGenerationRefusesPolicyModeMutationOverHTTP(t *testing.T) {
	m, configPath := newFenceMiner(t)
	baseURL := startRetiredGeneration(t, m)

	before := diskPolicy(t, configPath)
	if got, want := before, string(policy.ModeGameOrder); got != want {
		t.Fatalf("seeded on-disk CampaignPolicy = %q, want %q", got, want)
	}

	resp := postForm(t, baseURL, "/api/policy/mode", url.Values{
		"mode": {string(policy.ModeSmart)},
	})

	if resp.StatusCode == http.StatusOK {
		t.Errorf("POST /api/policy/mode against a RETIRED generation = %d OK; "+
			"a retired generation must never acknowledge a mutation", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d (ErrShuttingDown is this repo's fail-closed "+
			"drain refusal — see writeApplyError, internal/web/handlers_settings.go)",
			resp.StatusCode, http.StatusServiceUnavailable)
	}

	live, _ := m.CurrentCampaignPolicy()
	if live != string(policy.ModeGameOrder) {
		t.Errorf("retired generation's in-memory CampaignPolicy = %q, want %q "+
			"(a refused mutation must not change runtime config)", live, policy.ModeGameOrder)
	}

	if after := diskPolicy(t, configPath); after != before {
		t.Errorf("config.json CampaignPolicy = %q, want %q unchanged "+
			"(a refused mutation must never reach disk)", after, before)
	}
}

// TestRetiredGenerationRefusesDropRuleMutationOverHTTP pins the same fence on
// the per-drop-rule endpoint, the second PolicyProvider mutation. SetDropRule
// writes a MAP inside the retired generation's config in place and persists
// the whole document, so an unfenced call here both loses the operator's
// change and can revert fields a later generation already committed.
func TestRetiredGenerationRefusesDropRuleMutationOverHTTP(t *testing.T) {
	m, configPath := newFenceMiner(t)
	baseURL := startRetiredGeneration(t, m)

	const rewardKey = "stale-generation-reward"

	resp := postForm(t, baseURL, "/api/policy/drop-rule", url.Values{
		"rewardKey": {rewardKey},
		"skip":      {"on"},
	})

	if resp.StatusCode == http.StatusOK {
		t.Errorf("POST /api/policy/drop-rule against a RETIRED generation = %d OK; "+
			"a retired generation must never acknowledge a mutation", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	_, rules := m.CurrentCampaignPolicy()
	if _, present := rules[rewardKey]; present {
		t.Errorf("retired generation gained drop rule %q; a refused mutation must not "+
			"change runtime config", rewardKey)
	}

	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("re-read config.json: %v", err)
	}
	if _, ok := onDisk.DropRules[rewardKey]; ok {
		t.Errorf("config.json gained drop rule %q; a refused mutation must never reach disk", rewardKey)
	}
}

// TestRetiredGenerationRefusesHealthSettingsMutationOverHTTP pins the fence on
// HealthProvider.ApplyHealthSettings, which already returned an error but was
// never admitted through the drain interlock.
func TestRetiredGenerationRefusesHealthSettingsMutationOverHTTP(t *testing.T) {
	m, configPath := newFenceMiner(t)
	baseURL := startRetiredGeneration(t, m)

	const staleCanary = "canary-set-by-retired-generation"

	resp := postForm(t, baseURL, "/api/health/settings", url.Values{
		"section":       {"canary"},
		"canaryEnabled": {"on"},
		"canaryChannel": {staleCanary},
	})

	if resp.StatusCode == http.StatusOK {
		t.Errorf("POST /api/health/settings against a RETIRED generation = %d OK; "+
			"a retired generation must never acknowledge a mutation", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}

	if got := m.CurrentHealthSettings().CanaryChannel; got == staleCanary {
		t.Errorf("retired generation's CanaryChannel = %q; a refused mutation must not "+
			"change runtime config", got)
	}

	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("re-read config.json: %v", err)
	}
	if onDisk.Health.CanaryChannel == staleCanary {
		t.Errorf("config.json CanaryChannel = %q; a refused mutation must never reach disk", staleCanary)
	}
}

// TestRetiredGenerationRefusesAutoRedeemMutation pins the fence on
// RewardsProvider.SetAutoRedeem at the narrowest correct seam.
//
// This one is pinned on the provider method rather than over HTTP, and the
// reason is worth stating exactly rather than hand-waving. The auto-redeem
// route is reached through handleAPIStreamerRewards, which resolves a streamer
// from the URL path before dispatching; this miner tracks none, so an HTTP
// request is answered by that routing guard and never reaches SetAutoRedeem.
// Driving it over HTTP would therefore assert the routing guard, not the
// fence. The method IS the object the web server still holds, so calling it
// directly exercises the same seam the retained provider would — and the
// ordering the assertion depends on (the fence refuses BEFORE the roster
// check) is exactly what this pins. The HTTP status mapping for the other
// routes is covered in internal/web (stale_generation_status_test.go).
func TestRetiredGenerationRefusesAutoRedeemMutation(t *testing.T) {
	m, configPath := newFenceMiner(t)
	_ = startRetiredGeneration(t, m)

	const streamer = "someone"

	err := m.SetAutoRedeem(streamer, config.AutoRedeemConfig{Enabled: true, Budget: 100})
	if err == nil {
		t.Fatal("SetAutoRedeem on a RETIRED generation = nil, want a refusal error")
	}
	if !errors.Is(err, ErrShuttingDown) {
		t.Errorf("SetAutoRedeem error = %v, want ErrShuttingDown (the drain refusal), "+
			"not a downstream validation error — the fence must refuse BEFORE any other work", err)
	}

	onDisk, cerr := config.LoadConfig(configPath)
	if cerr != nil {
		t.Fatalf("re-read config.json: %v", cerr)
	}
	if _, ok := onDisk.AutoRedeem[streamer]; ok {
		t.Errorf("config.json gained auto-redeem entry for %q; a refused mutation must "+
			"never reach disk", streamer)
	}
}

// TestSetAutoRedeemRejectsUntrackedStreamerWhileLive keeps the roster guard
// genuinely covered.
//
// It exists because the fence changed what the post-teardown SetAutoRedeem
// case in provider_safety_test.go can prove: there the refusal now comes from
// the fence, so a bare "returns some error" assertion would keep passing while
// no longer exercising the roster guard at all. This pins that guard where it
// is still reachable — on a LIVE, authoritative generation, where the fence
// admits the call and the guard is what refuses.
func TestSetAutoRedeemRejectsUntrackedStreamerWhileLive(t *testing.T) {
	m, _ := newFenceMiner(t)
	_, retire := startLiveGeneration(t, m)
	defer retire()

	err := m.SetAutoRedeem("not-a-tracked-streamer", config.AutoRedeemConfig{Enabled: true})
	if err == nil {
		t.Fatal("SetAutoRedeem for an untracked streamer on a LIVE generation = nil, want an error")
	}
	if errors.Is(err, ErrShuttingDown) {
		t.Errorf("SetAutoRedeem on a LIVE generation = %v; the fence must ADMIT a live "+
			"generation and let the roster guard answer", err)
	}
}

// TestLiveGenerationStillAcceptsPolicyMutationOverHTTP is the other half of the
// fence's contract: while a generation IS the authoritative mutable one, the
// ordinary mutation path must keep working end to end, unchanged. A fence that
// refused everything would pass every refusal test in this file and still be
// broken.
func TestLiveGenerationStillAcceptsPolicyMutationOverHTTP(t *testing.T) {
	m, configPath := newFenceMiner(t)
	baseURL, retire := startLiveGeneration(t, m)
	defer retire()

	resp := postForm(t, baseURL, "/api/policy/mode", url.Values{
		"mode": {string(policy.ModeSmart)},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/policy/mode on a LIVE generation = %d, want 200", resp.StatusCode)
	}

	live, _ := m.CurrentCampaignPolicy()
	if live != string(policy.ModeSmart) {
		t.Errorf("live generation's CampaignPolicy = %q, want %q", live, policy.ModeSmart)
	}
	if got := diskPolicy(t, configPath); got != string(policy.ModeSmart) {
		t.Errorf("config.json CampaignPolicy = %q, want %q (a successful mutation must persist)",
			got, policy.ModeSmart)
	}
}

// TestMutationAdmittedBeforeRetirementSurvivesIntoTheHandoff pins the ordering
// half of the fence (acceptance criterion A10): a mutation that was ADMITTED
// and acknowledged while the generation was still authoritative must be
// present in the configuration that generation hands to its successor.
//
// The handoff is App-level, but the property it depends on is this package's:
// an admitted mutation holds applyWG, and teardown waits on applyWG BEFORE any
// other teardown step and therefore before Run returns — which is strictly
// before the lifecycle can reach the next generation's factory and sample
// CurrentConfig. Asserting on CurrentConfig taken AFTER Run returned is
// exactly the value that sample would observe.
//
// Be honest about what this does and does not catch: it is a NON-REGRESSION
// guard, not a repro. It passes on the unfixed base too, because A10 already
// held there — the fence must not BREAK it. What it would catch is a fence
// that over-refuses (rejecting a mutation on a still-authoritative
// generation) or a drain that stops waiting, either of which would silently
// drop an acknowledged operator change from the next generation's config.
func TestMutationAdmittedBeforeRetirementSurvivesIntoTheHandoff(t *testing.T) {
	m, _ := newFenceMiner(t)
	baseURL, retire := startLiveGeneration(t, m)

	resp := postForm(t, baseURL, "/api/policy/mode", url.Values{
		"mode": {string(policy.ModeSmart)},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/policy/mode on a LIVE generation = %d, want 200", resp.StatusCode)
	}

	// Retire only AFTER the mutation was acknowledged.
	retire()

	if got := m.CurrentConfig().CampaignPolicy; got != string(policy.ModeSmart) {
		t.Errorf("handoff config CampaignPolicy = %q, want %q — a mutation acknowledged "+
			"before retirement must not be lost from what the next generation starts from",
			got, policy.ModeSmart)
	}
}

// TestProviderSampledBeforeRetirementCannotMutateAfter is the direct test for
// acceptance criterion A3, and for the precise reason a web-side check could
// never have been sufficient.
//
// It captures the provider interface value while the generation is LIVE —
// exactly what internal/web does when it reads s.policyProvider under RLock
// and releases the lock — and only then retires the generation. The call is
// made afterwards, through that already-sampled value. A pre-check in the
// handler cannot help here by construction: the sample is valid at the instant
// it is taken, and the sample-to-call gap can be arbitrarily long (the rewards
// route's gap literally contains a client-paced request-body read). Only the
// callee refusing closes it.
func TestProviderSampledBeforeRetirementCannotMutateAfter(t *testing.T) {
	m, configPath := newFenceMiner(t)
	_, retire := startLiveGeneration(t, m)

	// Sample WHILE LIVE, exactly as a web handler does.
	var sampled web.PolicyProvider = m

	before := diskPolicy(t, configPath)

	// Now the generation retires, with the sample already in hand.
	retire()

	if err := sampled.ApplyCampaignPolicy(string(policy.ModeSmart)); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("ApplyCampaignPolicy through a provider sampled BEFORE retirement = %v, "+
			"want ErrShuttingDown", err)
	}
	if err := sampled.SetDropRule("sampled-before-retirement", config.DropRule{Skip: true}); !errors.Is(err, ErrShuttingDown) {
		t.Errorf("SetDropRule through a provider sampled BEFORE retirement = %v, want ErrShuttingDown", err)
	}

	if got, _ := m.CurrentCampaignPolicy(); got != string(policy.ModeGameOrder) {
		t.Errorf("runtime CampaignPolicy = %q, want %q unchanged", got, policy.ModeGameOrder)
	}
	if after := diskPolicy(t, configPath); after != before {
		t.Errorf("config.json CampaignPolicy = %q, want %q unchanged", after, before)
	}
}

// TestOnlyOneGenerationAcceptsMutationAtATime is the direct test for A12: two
// generations must never both be accepting configuration mutations.
//
// It builds a second generation over the same config path while the first is
// retired, and asserts the pair is never both-accepting: the retired one
// refuses, the live one is admitted.
func TestOnlyOneGenerationAcceptsMutationAtATime(t *testing.T) {
	genOne, configPath := newFenceMiner(t)
	_, retireOne := startLiveGeneration(t, genOne)
	retireOne()

	// The successor generation, over the SAME config file.
	cfg := *genOne.CurrentConfig()
	genTwo := New(&cfg, configPath)
	genTwo.SetDatabase(genOne.db)
	_, retireTwo := startLiveGeneration(t, genTwo)
	defer retireTwo()

	errOne := genOne.ApplyCampaignPolicy(string(policy.ModeEndingSoonest))
	errTwo := genTwo.ApplyCampaignPolicy(string(policy.ModeSmart))

	if !errors.Is(errOne, ErrShuttingDown) {
		t.Errorf("retired generation accepted a mutation (%v); at most ONE generation may accept", errOne)
	}
	if errTwo != nil {
		t.Errorf("live generation refused a mutation (%v); the authoritative generation must accept", errTwo)
	}

	if got := diskPolicy(t, configPath); got != string(policy.ModeSmart) {
		t.Errorf("config.json CampaignPolicy = %q, want %q — only the LIVE generation's write may land",
			got, policy.ModeSmart)
	}
}
