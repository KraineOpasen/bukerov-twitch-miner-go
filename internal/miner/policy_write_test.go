package miner

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/drops"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/policy"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

const trackedRewardKey = "game-wot::garage slot"

func newPersistedDropRuleMiner(t *testing.T, rules map[string]config.DropRule, withCampaign bool) (*Miner, string) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.CampaignPolicy = string(policy.ModeSmart)
	if rules != nil {
		cfg.DropRules = make(map[string]config.DropRule, len(rules))
		for key, rule := range rules {
			cfg.DropRules[key] = rule
		}
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(configPath, &cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	m := &Miner{config: &cfg, configPath: configPath}
	if withCampaign {
		tracker := drops.NewDropsTracker(fakeDropsGQL{}, nil, cfg.RateLimits, nil)
		tracker.SyncNow()
		if got := len(tracker.Campaigns()); got != 1 {
			t.Fatalf("campaign fixture yielded %d campaigns, want 1", got)
		}
		m.dropsTracker = tracker
		m.watcher = &watcher.MinuteWatcher{}
		m.refreshPolicy(time.Unix(1_700_000_000, 0))
	}
	return m, configPath
}

func loadDropRuleConfig(t *testing.T, path string) *config.Config {
	t.Helper()
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	return cfg
}

func campaignDecision(t *testing.T, m *Miner) policy.Decision {
	t.Helper()
	_, decisions := m.PolicySnapshot()
	for _, decision := range decisions {
		if decision.CampaignID == "campaign-wot" {
			return decision
		}
	}
	t.Fatalf("campaign-wot decision missing: %+v", decisions)
	return policy.Decision{}
}

func TestSetDropRuleSaveFailureIsAtomic(t *testing.T) {
	oldRule := config.DropRule{HighPriority: true}
	candidate := config.DropRule{Skip: true}
	m, configPath := newPersistedDropRuleMiner(t, map[string]config.DropRule{trackedRewardKey: oldRule}, true)
	before := m.policySnap.Load()
	if before == nil {
		t.Fatal("setup: initial policy snapshot is nil")
	}

	breakConfigPathForNextSave(t, configPath)
	if err := m.SetDropRule(trackedRewardKey, candidate); err == nil {
		t.Fatal("SetDropRule succeeded despite deterministic SaveConfig failure")
	}

	_, rules := m.CurrentCampaignPolicy()
	if got := rules[trackedRewardKey]; got != oldRule {
		t.Fatalf("live rule after failed save = %+v, want previous %+v", got, oldRule)
	}
	if got := m.policySnap.Load(); got != before {
		t.Fatalf("failed candidate caused policy publication: got %p, want unchanged %p", got, before)
	}
	if decision := campaignDecision(t, m); decision.Excluded {
		t.Fatalf("failed Skip candidate changed the committed policy: %+v", decision)
	}
	info, err := os.Stat(configPath)
	if err != nil || !info.IsDir() {
		t.Fatalf("failed save unexpectedly replaced deterministic failure target: stat=%v, err=%v", info, err)
	}
}

func TestSetDropRuleResetSaveFailureIsAtomic(t *testing.T) {
	oldRule := config.DropRule{Skip: true}
	m, configPath := newPersistedDropRuleMiner(t, map[string]config.DropRule{trackedRewardKey: oldRule}, true)
	before := m.policySnap.Load()
	breakConfigPathForNextSave(t, configPath)

	if err := m.SetDropRule(trackedRewardKey, config.DropRule{}); err == nil {
		t.Fatal("reset succeeded despite deterministic SaveConfig failure")
	}
	_, rules := m.CurrentCampaignPolicy()
	if got := rules[trackedRewardKey]; got != oldRule {
		t.Fatalf("failed reset changed live rule: got %+v, want %+v", got, oldRule)
	}
	if got := m.policySnap.Load(); got != before {
		t.Fatalf("failed reset caused policy publication: got %p, want unchanged %p", got, before)
	}
	if decision := campaignDecision(t, m); !decision.Excluded {
		t.Fatalf("failed reset removed committed Skip semantics: %+v", decision)
	}
}

func TestSetDropRuleSaveFailureRestoresNilMap(t *testing.T) {
	m, configPath := newPersistedDropRuleMiner(t, nil, false)
	if m.config.DropRules != nil {
		t.Fatal("setup: DropRules must be nil")
	}
	breakConfigPathForNextSave(t, configPath)

	if err := m.SetDropRule("g::candidate", config.DropRule{Skip: true}); err == nil {
		t.Fatal("SetDropRule succeeded despite deterministic SaveConfig failure")
	}
	if m.config.DropRules != nil {
		t.Fatalf("failed first write did not restore nil map: %+v", m.config.DropRules)
	}
}

func TestSetDropRuleSuccessPublishesPersistedState(t *testing.T) {
	m, configPath := newPersistedDropRuleMiner(t, nil, true)
	before := m.policySnap.Load()
	rule := config.DropRule{Skip: true}

	if err := m.SetDropRule(trackedRewardKey, rule); err != nil {
		t.Fatalf("SetDropRule: %v", err)
	}
	_, live := m.CurrentCampaignPolicy()
	if got := live[trackedRewardKey]; got != rule {
		t.Fatalf("live rule = %+v, want %+v", got, rule)
	}
	if got := loadDropRuleConfig(t, configPath).DropRules[trackedRewardKey]; got != rule {
		t.Fatalf("durable rule = %+v, want %+v", got, rule)
	}
	if after := m.policySnap.Load(); after == nil || after == before {
		t.Fatalf("successful commit did not synchronously publish a new snapshot: before=%p after=%p", before, after)
	}
	if decision := campaignDecision(t, m); !decision.Excluded {
		t.Fatalf("published policy does not reflect committed Skip rule: %+v", decision)
	}
}

func TestSetDropRuleResetSuccessPublishesPersistedState(t *testing.T) {
	m, configPath := newPersistedDropRuleMiner(t, map[string]config.DropRule{
		trackedRewardKey: {Skip: true},
	}, true)
	before := m.policySnap.Load()

	if err := m.SetDropRule(trackedRewardKey, config.DropRule{}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	_, live := m.CurrentCampaignPolicy()
	if _, ok := live[trackedRewardKey]; ok {
		t.Fatal("reset rule remains live")
	}
	if _, ok := loadDropRuleConfig(t, configPath).DropRules[trackedRewardKey]; ok {
		t.Fatal("reset rule remains durable")
	}
	if after := m.policySnap.Load(); after == nil || after == before {
		t.Fatalf("successful reset did not publish a new snapshot: before=%p after=%p", before, after)
	}
	if decision := campaignDecision(t, m); decision.Excluded {
		t.Fatalf("published policy still reflects reset Skip rule: %+v", decision)
	}
}

func TestSetDropRuleAllFieldsRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		rule config.DropRule
	}{
		{name: "skip", rule: config.DropRule{Skip: true}},
		{name: "high priority", rule: config.DropRule{HighPriority: true}},
		{name: "always finish started", rule: config.DropRule{AlwaysFinishStarted: true}},
		{name: "next reward only", rule: config.DropRule{NextRewardOnly: true}},
		{name: "ignore subscriber only", rule: config.DropRule{IgnoreSubscriberOnly: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, configPath := newPersistedDropRuleMiner(t, nil, false)
			key := "game::" + tt.name
			if err := m.SetDropRule(key, tt.rule); err != nil {
				t.Fatalf("SetDropRule: %v", err)
			}
			_, live := m.CurrentCampaignPolicy()
			if got := live[key]; got != tt.rule {
				t.Fatalf("live round-trip = %+v, want %+v", got, tt.rule)
			}
			if got := loadDropRuleConfig(t, configPath).DropRules[key]; got != tt.rule {
				t.Fatalf("disk/restart round-trip = %+v, want %+v", got, tt.rule)
			}
		})
	}
}

func TestSetDropRuleCanonicalBoundary(t *testing.T) {
	m, configPath := newPersistedDropRuleMiner(t, nil, false)
	rule := config.DropRule{HighPriority: true}

	if err := m.SetDropRule("G1 :: Cool Skin", rule); !errors.Is(err, models.ErrInvalidRewardKey) {
		t.Fatalf("malformed key error = %v, want ErrInvalidRewardKey", err)
	}
	_, live := m.CurrentCampaignPolicy()
	if len(live) != 0 {
		t.Fatalf("malformed key became live: %+v", live)
	}
	if disk := loadDropRuleConfig(t, configPath); len(disk.DropRules) != 0 {
		t.Fatalf("malformed key became durable: %+v", disk.DropRules)
	}

	if err := m.SetDropRule(" G1::Cool Skin ", rule); err != nil {
		t.Fatalf("compatible canonicalization failed: %v", err)
	}
	_, live = m.CurrentCampaignPolicy()
	if got := live["g1::cool skin"]; got != rule {
		t.Fatalf("canonical key not live: %+v", live)
	}
	if _, alternate := live["g1 :: cool skin"]; alternate {
		t.Fatalf("unreachable alternate key was stored: %+v", live)
	}
	missingGameKey := models.NormalizeRewardKey("", "Reward")
	if err := m.SetDropRule(missingGameKey, config.DropRule{NextRewardOnly: true}); err != nil {
		t.Fatalf("owner-produced missing-game key was rejected: %v", err)
	}
	_, live = m.CurrentCampaignPolicy()
	if live[missingGameKey] != (config.DropRule{NextRewardOnly: true}) {
		t.Fatalf("owner-produced missing-game key not stored canonically: %+v", live)
	}
}

func TestSetDropRuleConcurrentDifferentKeysNoLostUpdate(t *testing.T) {
	m, configPath := newPersistedDropRuleMiner(t, nil, false)
	start := make(chan struct{})
	type result struct {
		key  string
		rule config.DropRule
		err  error
	}
	results := make(chan result, 2)
	writes := []result{
		{key: "g::first", rule: config.DropRule{Skip: true}},
		{key: "g::second", rule: config.DropRule{HighPriority: true, NextRewardOnly: true}},
	}
	for _, write := range writes {
		write := write
		go func() {
			<-start
			write.err = m.SetDropRule(write.key, write.rule)
			results <- write
		}()
	}
	close(start)
	for range writes {
		if got := <-results; got.err != nil {
			t.Fatalf("SetDropRule(%q): %v", got.key, got.err)
		}
	}

	_, live := m.CurrentCampaignPolicy()
	disk := loadDropRuleConfig(t, configPath).DropRules
	for _, write := range writes {
		if live[write.key] != write.rule || disk[write.key] != write.rule {
			t.Fatalf("write %q lost: live=%+v disk=%+v want=%+v", write.key, live[write.key], disk[write.key], write.rule)
		}
	}
}

func TestSetDropRuleConcurrentSameKeyIsLinearizable(t *testing.T) {
	m, configPath := newPersistedDropRuleMiner(t, nil, false)
	key := "g::same"
	rules := []config.DropRule{
		{Skip: true, AlwaysFinishStarted: true},
		{HighPriority: true, NextRewardOnly: true, IgnoreSubscriberOnly: true},
	}
	firstSaveEntered := make(chan struct{})
	releaseFirstSave := make(chan struct{})
	saveCalls := 0
	m.saveConfigFn = func(path string, cfg *config.Config) error {
		saveCalls++ // serialized by SetDropRule's coordinator/m.mu section
		if saveCalls == 1 {
			close(firstSaveEntered)
			<-releaseFirstSave
		}
		return config.SaveConfig(path, cfg)
	}
	firstErr := make(chan error, 1)
	go func() { firstErr <- m.SetDropRule(key, rules[0]) }()
	waitTestChannel(t, firstSaveEntered, "first same-key save")
	secondErr := make(chan error, 1)
	go func() { secondErr <- m.SetDropRule(key, rules[1]) }()
	close(releaseFirstSave)
	if err := <-firstErr; err != nil {
		t.Fatalf("first SetDropRule: %v", err)
	}
	if err := <-secondErr; err != nil {
		t.Fatalf("second SetDropRule: %v", err)
	}
	_, live := m.CurrentCampaignPolicy()
	disk := loadDropRuleConfig(t, configPath).DropRules
	if live[key] != disk[key] {
		t.Fatalf("same-key final live/disk mismatch: live=%+v disk=%+v", live[key], disk[key])
	}
	if live[key] != rules[1] {
		t.Fatalf("same-key final value = %+v, want known second linearization %+v", live[key], rules[1])
	}
}

func TestSetDropRuleValidationFailureConcurrentWithSuccess(t *testing.T) {
	m, configPath := newPersistedDropRuleMiner(t, nil, false)
	start := make(chan struct{})
	invalidErr := make(chan error, 1)
	validErr := make(chan error, 1)
	go func() {
		<-start
		invalidErr <- m.SetDropRule("g :: unreachable", config.DropRule{Skip: true})
	}()
	go func() {
		<-start
		validErr <- m.SetDropRule("g::reachable", config.DropRule{HighPriority: true})
	}()
	close(start)
	if err := <-invalidErr; !errors.Is(err, models.ErrInvalidRewardKey) {
		t.Fatalf("invalid write error = %v, want ErrInvalidRewardKey", err)
	}
	if err := <-validErr; err != nil {
		t.Fatalf("valid concurrent write: %v", err)
	}
	_, live := m.CurrentCampaignPolicy()
	disk := loadDropRuleConfig(t, configPath).DropRules
	if _, exists := live["g :: unreachable"]; exists {
		t.Fatalf("invalid alternate key became live: %+v", live)
	}
	if live["g::reachable"] != (config.DropRule{HighPriority: true}) || !reflect.DeepEqual(live, disk) {
		t.Fatalf("valid concurrent commit diverged: live=%+v disk=%+v", live, disk)
	}
}

type blockingLogState struct {
	once    sync.Once
	entered chan struct{}
	release <-chan struct{}
}

type blockingLogHandler struct {
	next  slog.Handler
	state *blockingLogState
}

func (h *blockingLogHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *blockingLogHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "Failed to save config" {
		h.state.once.Do(func() { close(h.state.entered) })
		<-h.state.release
	}
	return h.next.Handle(ctx, record)
}

func (h *blockingLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &blockingLogHandler{next: h.next.WithAttrs(attrs), state: h.state}
}

func (h *blockingLogHandler) WithGroup(name string) slog.Handler {
	return &blockingLogHandler{next: h.next.WithGroup(name), state: h.state}
}

func TestSetDropRuleFailedWriteCannotRollbackLaterSuccess(t *testing.T) {
	m, configPath := newPersistedDropRuleMiner(t, map[string]config.DropRule{
		"g::old": {AlwaysFinishStarted: true},
	}, false)
	oldDisk, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read seed config: %v", err)
	}
	breakConfigPathForNextSave(t, configPath)

	release := make(chan struct{})
	state := &blockingLogState{entered: make(chan struct{}), release: release}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(&blockingLogHandler{
		next:  slog.NewTextHandler(io.Discard, nil),
		state: state,
	}))
	defer slog.SetDefault(previousLogger)

	failedErr := make(chan error, 1)
	go func() {
		failedErr <- m.SetDropRule("g::failed", config.DropRule{Skip: true})
	}()
	select {
	case <-state.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("failed write did not reach deterministic persistence barrier")
	}

	if err := os.Remove(configPath); err != nil {
		t.Fatalf("remove failure directory: %v", err)
	}
	if err := os.WriteFile(configPath, oldDisk, 0o600); err != nil {
		t.Fatalf("restore prior durable config: %v", err)
	}
	successStarted := make(chan struct{})
	successErr := make(chan error, 1)
	go func() {
		close(successStarted)
		successErr <- m.SetDropRule("g::success", config.DropRule{HighPriority: true})
	}()
	<-successStarted
	close(release)

	if err := <-failedErr; err == nil {
		t.Fatal("failed transaction returned success")
	}
	if err := <-successErr; err != nil {
		t.Fatalf("later transaction failed: %v", err)
	}
	_, live := m.CurrentCampaignPolicy()
	disk := loadDropRuleConfig(t, configPath).DropRules
	if _, exists := live["g::failed"]; exists {
		t.Fatalf("failed candidate survived in live state: %+v", live)
	}
	if live["g::success"] != (config.DropRule{HighPriority: true}) || !reflect.DeepEqual(live, disk) {
		t.Fatalf("later success was rolled back or diverged: live=%+v disk=%+v", live, disk)
	}
}

func TestSetDropRuleSettingsCandidateCannotDetachFirstMap(t *testing.T) {
	m, configPath := newPersistedDropRuleMiner(t, nil, false)
	m.coordinatorMu.Lock()
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		result <- m.SetDropRule("g::first", config.DropRule{Skip: true})
	}()
	<-started

	// Model the Settings commit that previously could publish a candidate
	// snapshotted while DropRules was nil. SetDropRule cannot pass the shared
	// coordinator until this candidate is authoritative.
	m.mu.Lock()
	candidate := m.cloneConfigLocked()
	m.config = candidate
	m.mu.Unlock()
	m.coordinatorMu.Unlock()

	if err := <-result; err != nil {
		t.Fatalf("SetDropRule: %v", err)
	}
	_, live := m.CurrentCampaignPolicy()
	disk := loadDropRuleConfig(t, configPath).DropRules
	want := config.DropRule{Skip: true}
	if live["g::first"] != want || disk["g::first"] != want {
		t.Fatalf("first map was detached by Settings candidate: live=%+v disk=%+v", live, disk)
	}
	nextGeneration := New(m.ConfigSnapshot(), configPath)
	_, nextRules := nextGeneration.CurrentCampaignPolicy()
	if nextRules["g::first"] != want {
		t.Fatalf("next generation received stale process config: got %+v, want %+v", nextRules, want)
	}
}

func TestSetDropRuleRefusesAfterLifecycleDrain(t *testing.T) {
	m, configPath := newPersistedDropRuleMiner(t, map[string]config.DropRule{
		"g::old": {AlwaysFinishStarted: true},
	}, false)
	m.applyMu.Lock()
	m.applyDraining = true
	m.applyMu.Unlock()

	if err := m.SetDropRule("g::stale", config.DropRule{Skip: true}); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("SetDropRule after drain = %v, want ErrShuttingDown", err)
	}
	_, live := m.CurrentCampaignPolicy()
	disk := loadDropRuleConfig(t, configPath).DropRules
	if _, exists := live["g::stale"]; exists || !reflect.DeepEqual(live, disk) {
		t.Fatalf("draining generation mutated config: live=%+v disk=%+v", live, disk)
	}
}

func TestSettingsMergePreservesDropRules(t *testing.T) {
	cfg := config.DefaultConfig()
	want := map[string]config.DropRule{
		"g::all": {
			Skip:                 true,
			HighPriority:         true,
			AlwaysFinishStarted:  true,
			NextRewardOnly:       true,
			IgnoreSubscriberOnly: true,
		},
	}
	cfg.DropRules = maps.Clone(want)
	// Pin the oracle itself: if want accidentally aliases the live map again,
	// this sensitivity probe fails before it can bless a mutation.
	cfg.DropRules["g::all"] = config.DropRule{}
	if reflect.DeepEqual(cfg.DropRules, want) {
		t.Fatal("DropRules preservation oracle aliases the candidate map")
	}
	cfg.DropRules = maps.Clone(want)
	runtimeSettings := settings.BuildRuntimeSettings(&cfg)
	runtimeSettings.Analytics.Refresh++
	settings.ApplyToConfig(&cfg, runtimeSettings)
	if !reflect.DeepEqual(cfg.DropRules, want) {
		t.Fatalf("Settings merge changed DropRules: got %+v, want %+v", cfg.DropRules, want)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(path, &cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if got := loadDropRuleConfig(t, path).DropRules; !reflect.DeepEqual(got, want) {
		t.Fatalf("Settings merge round-trip changed DropRules: got %+v, want %+v", got, want)
	}
}
