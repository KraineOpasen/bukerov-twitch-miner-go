package miner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
)

// newHealthTestMiner builds a minimal Miner for ApplyHealthSettings tests: a
// live config seeded with the given Health settings and persisted at a real
// configPath on disk, the same shape every other config.json commit-point
// test in this package uses (see settings_persistence_test.go).
func newHealthTestMiner(t *testing.T, initial config.HealthSettings) *Miner {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := &config.Config{Username: "tester", Health: initial}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	return &Miner{config: cfg, configPath: configPath}
}

// changedHealthSettings returns a HealthSettings value that differs from cur
// in every field the Health Center forms can post, each delta staying inside
// ValidateConfig's clamp range for that field (see config.go), so a
// successful apply is never confused with a clamp. This lets a test tell
// "the candidate was published" from "it wasn't" without depending on any
// single field.
func changedHealthSettings(cur config.HealthSettings) config.HealthSettings {
	next := cur
	next.CanaryEnabled = !cur.CanaryEnabled
	next.CanaryChannel = cur.CanaryChannel + "-changed"
	next.CanaryIntervalMinutes = cur.CanaryIntervalMinutes + 60
	next.CanaryMaxStalenessHours = cur.CanaryMaxStalenessHours + 1
	next.WatchdogEnabled = !cur.WatchdogEnabled
	next.WatchdogStallDelayMinutes = cur.WatchdogStallDelayMinutes + 5
	next.WatchdogStallConfirmations = cur.WatchdogStallConfirmations + 1
	next.WatchdogRecoveryCooldownMinutes = cur.WatchdogRecoveryCooldownMinutes + 1
	next.WatchdogAvoidTTLMinutes = cur.WatchdogAvoidTTLMinutes + 10
	next.WatchdogRearmHours = cur.WatchdogRearmHours + 1
	return next
}

// TestApplyHealthSettingsPersistFailureLeavesSettingsUnchanged is the P3 core
// invariant: when config.json cannot be written, ApplyHealthSettings must
// fail out loud (a non-nil error) and leave CurrentHealthSettings() exactly
// as it was — not partially applied, not silently diverged from disk.
//
// Before the fix, ApplyHealthSettings mutated and validated the LIVE config
// before the durable write was even attempted, so a SaveConfig failure left
// the runtime holding the new (unsaved) value while the error was only
// logged. This is the M1 mutation shape (ignore/only-log the SaveConfig
// error) and also catches the config-publish half of M2 (publish runtime
// before durable save): either mutation leaves CurrentHealthSettings()
// showing the attempted value instead of the prior one.
//
// The persistence failure is induced with breakConfigPathForNextSave — the
// same deterministic seam cp1_c2_matrix_test.go and
// settings_persistence_test.go already use — which replaces a real
// config.json with a directory so SaveConfig fails at its atomic rename.
func TestApplyHealthSettingsPersistFailureLeavesSettingsUnchanged(t *testing.T) {
	before := config.DefaultHealthSettings()
	before.CanaryChannel = "seed_channel"
	m := newHealthTestMiner(t, before)
	cfgBefore := m.config
	breakConfigPathForNextSave(t, m.configPath)

	next := changedHealthSettings(before)
	err := m.ApplyHealthSettings(next)
	if err == nil {
		t.Fatal("ApplyHealthSettings returned nil after config.json persistence failed; the caller (POST /api/health/settings) would report success for a change that never reached disk")
	}

	// The live config must be the very same object, with the very same
	// contents: no publish, no in-place mutation — the same property
	// settings_persistence_test.go pins for the general settings path.
	if m.config != cfgBefore {
		t.Error("live config was republished (new object) despite the persistence failure")
	}
	if got := m.CurrentHealthSettings(); got != before {
		t.Errorf("CurrentHealthSettings mutated by a failed persist: got %+v, want %+v", got, before)
	}

	// No new content may have reached disk: breakConfigPathForNextSave
	// installed a directory at configPath, and SaveConfig's atomic rename is
	// the only step that would have replaced it with a regular file, so
	// "still a directory" is exactly "nothing was written".
	info, statErr := os.Stat(m.configPath)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("configPath must still be the untouched directory this test installed (no new config content was ever written): stat=%v, err=%v", info, statErr)
	}
}

// TestApplyHealthSettingsPersistSuccessAppliesAndPersists preserves the
// existing successful hot-apply behavior required by the P3 contract: a save
// that succeeds must still update the live config and reach disk, exactly as
// before this pass.
func TestApplyHealthSettingsPersistSuccessAppliesAndPersists(t *testing.T) {
	before := config.DefaultHealthSettings()
	m := newHealthTestMiner(t, before)

	next := changedHealthSettings(before)
	if err := m.ApplyHealthSettings(next); err != nil {
		t.Fatalf("ApplyHealthSettings: %v", err)
	}

	if got := m.CurrentHealthSettings(); got != next {
		t.Errorf("CurrentHealthSettings not updated: got %+v, want %+v", got, next)
	}

	loaded, err := config.LoadConfig(m.configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.Health != next {
		t.Errorf("persisted config missing the change: got %+v, want %+v", loaded.Health, next)
	}
}

// TestApplyHealthSettingsWithoutConfigPathStaysSuccessful pins the existing
// configPath == "" semantics unchanged: with no config file configured there
// is nothing to persist, and the apply is a plain success — the same
// documented no-op success every other SaveConfig commit point in this
// package already treats it as (see settings_persistence_test.go).
func TestApplyHealthSettingsWithoutConfigPathStaysSuccessful(t *testing.T) {
	before := config.DefaultHealthSettings()
	m := &Miner{config: &config.Config{Username: "tester", Health: before}, configPath: ""}

	next := changedHealthSettings(before)
	if err := m.ApplyHealthSettings(next); err != nil {
		t.Fatalf("ApplyHealthSettings with no configPath returned an error: %v", err)
	}
	if got := m.CurrentHealthSettings(); got != next {
		t.Errorf("CurrentHealthSettings not updated: got %+v, want %+v", got, next)
	}
}

// TestApplyHealthSettingsPersistFailureWithRealDependentsWiredDoesNotPanic
// wires REAL (non-nil) canary and progress-watchdog instances — like a
// running miner with the Health Center enabled would have — and proves the
// same commit-point guarantee holds with them attached: the failure path
// still returns an error and leaves CurrentHealthSettings untouched.
//
// What this deliberately does NOT assert: that Canary.UpdateSettings /
// ProgressWatchdog.UpdateSettings were never invoked. internal/health (out
// of this task's allowed paths) exposes no synchronous, externally
// observable state for either — cfg/snapshotCfg are unexported, and
// Snapshot() only ever reflects the constructor's initial value or the
// async loop, neither of which this package can drive deterministically
// without starting real background goroutines (which this repo's test
// conventions steer away from — see tests.md: assert on the public
// interface, not internal state). That guarantee is structural instead:
// ApplyHealthSettings has exactly one early return on a persistence
// failure, and the canary/watchdog calls sit textually after the same
// commit-point-then-unlock section that gates the config publish this test
// DOES observe.
func TestApplyHealthSettingsPersistFailureWithRealDependentsWiredDoesNotPanic(t *testing.T) {
	before := config.DefaultHealthSettings()
	m := newHealthTestMiner(t, before)
	center := health.NewCenter()
	m.canary = health.NewCanary(center, nil, nil, nil, nil, healthCanaryConfig(before))
	m.progressWatchdog = health.NewProgressWatchdog(center, nil, nil, nil, nil, nil, nil, healthWatchdogConfig(before))
	breakConfigPathForNextSave(t, m.configPath)

	next := changedHealthSettings(before)
	err := m.ApplyHealthSettings(next)
	if err == nil {
		t.Fatal("ApplyHealthSettings returned nil after config.json persistence failed with real canary/watchdog wired")
	}
	if got := m.CurrentHealthSettings(); got != before {
		t.Errorf("CurrentHealthSettings mutated by a failed persist: got %+v, want %+v", got, before)
	}
}

// TestApplyHealthSettingsPersistSuccessWithRealDependentsWiredDoesNotPanic
// mirrors the success half with real canary/watchdog instances attached,
// pinning that wiring real dependents doesn't change the successful hot-apply
// path either — the deterministic counterpart the P3 contract requires
// alongside the failure-path coverage above.
func TestApplyHealthSettingsPersistSuccessWithRealDependentsWiredDoesNotPanic(t *testing.T) {
	before := config.DefaultHealthSettings()
	m := newHealthTestMiner(t, before)
	center := health.NewCenter()
	m.canary = health.NewCanary(center, nil, nil, nil, nil, healthCanaryConfig(before))
	m.progressWatchdog = health.NewProgressWatchdog(center, nil, nil, nil, nil, nil, nil, healthWatchdogConfig(before))

	next := changedHealthSettings(before)
	if err := m.ApplyHealthSettings(next); err != nil {
		t.Fatalf("ApplyHealthSettings: %v", err)
	}
	if got := m.CurrentHealthSettings(); got != next {
		t.Errorf("CurrentHealthSettings not updated: got %+v, want %+v", got, next)
	}
}
