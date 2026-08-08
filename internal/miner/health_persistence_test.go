package miner

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/health"
)

// newHealthTestMiner builds a minimal Miner for ApplyHealthSettings tests: a
// live config seeded with the given Health settings and persisted at a real
// configPath on disk — the same shape settings_persistence_test.go uses for
// the general settings commit point.
func newHealthTestMiner(t *testing.T, initial config.HealthSettings) *Miner {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	cfg := &config.Config{Username: "health_persistence_tester", Health: initial}
	if err := config.SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	return &Miner{config: cfg, configPath: configPath}
}

// changedHealthSettings returns a HealthSettings value that differs from cur
// in every field the Health Center forms can post, with each delta already
// inside ValidateConfig's clamp range (config.go) so a successful apply is
// never confused with a clamp. Used by tests that only care whether the
// candidate was published, not by the clamp-specific test below.
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

// healthCanarySpy / healthWatchdogSpy record calls made through Miner's
// healthCanaryUpdate/healthWatchdogUpdate seams (miner.go) — the injectable
// observation point ApplyHealthSettings (health.go) calls through instead of
// the real canary/progress watchdog when a test installs one. It exists
// because internal/health's Canary/ProgressWatchdog keep cfg unexported with
// no synchronous exported observer, so nothing outside internal/health can
// otherwise tell "the dependent was notified" from "it wasn't".
type healthCanarySpy struct {
	calls int
	last  health.CanaryConfig
}

func (s *healthCanarySpy) record(cfg health.CanaryConfig) {
	s.calls++
	s.last = cfg
}

type healthWatchdogSpy struct {
	calls int
	last  health.WatchdogConfig
}

func (s *healthWatchdogSpy) record(cfg health.WatchdogConfig) {
	s.calls++
	s.last = cfg
}

// installHealthDependentSpies wires fresh spies into m's seams and returns
// them so a test can assert on call counts/arguments.
func installHealthDependentSpies(m *Miner) (*healthCanarySpy, *healthWatchdogSpy) {
	canarySpy := &healthCanarySpy{}
	watchdogSpy := &healthWatchdogSpy{}
	m.healthCanaryUpdate = canarySpy.record
	m.healthWatchdogUpdate = watchdogSpy.record
	return canarySpy, watchdogSpy
}

// TestApplyHealthSettingsPersistFailureLeavesSettingsUnchanged is the P3 core
// invariant: when config.json cannot be written, ApplyHealthSettings must
// fail out loud (a non-nil error) and leave CurrentHealthSettings() exactly
// as it was — not partially applied, not silently diverged from disk.
//
// The persistence failure is induced with breakConfigPathForNextSave
// (cp1_c2_matrix_test.go) — the same deterministic seam
// settings_persistence_test.go already uses — which replaces a real
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
	// contents: no publish, no in-place mutation.
	if m.config != cfgBefore {
		t.Error("live config was republished (new object) despite the persistence failure")
	}
	if got := m.CurrentHealthSettings(); got != before {
		t.Errorf("CurrentHealthSettings mutated by a failed persist: got %+v, want %+v", got, before)
	}

	// breakConfigPathForNextSave installed a directory at configPath;
	// SaveConfig's atomic rename is the only step that would have replaced it
	// with a regular file, so "still a directory" is exactly "nothing was
	// written".
	info, statErr := os.Stat(m.configPath)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("configPath must still be the untouched directory this test installed (no new config content was ever written): stat=%v, err=%v", info, statErr)
	}
}

// TestApplyHealthSettingsPersistFailureSkipsDependentUpdates is the
// seam-level counterpart of the test above: a persistence failure must not
// reach the canary/watchdog at all. Unlike the "real dependents" tests below
// (which can only prove "did not panic"), the injected spy seams make the
// call count itself observable, closing a gap this package used to have no
// way to close.
func TestApplyHealthSettingsPersistFailureSkipsDependentUpdates(t *testing.T) {
	before := config.DefaultHealthSettings()
	m := newHealthTestMiner(t, before)
	canarySpy, watchdogSpy := installHealthDependentSpies(m)
	breakConfigPathForNextSave(t, m.configPath)

	next := changedHealthSettings(before)
	if err := m.ApplyHealthSettings(next); err == nil {
		t.Fatal("ApplyHealthSettings returned nil after config.json persistence failed")
	}

	if canarySpy.calls != 0 {
		t.Errorf("canary seam called %d times on a persistence failure, want 0", canarySpy.calls)
	}
	if watchdogSpy.calls != 0 {
		t.Errorf("watchdog seam called %d times on a persistence failure, want 0", watchdogSpy.calls)
	}
}

// TestApplyHealthSettingsPersistSuccessAppliesAndPersists preserves the
// existing successful hot-apply behavior required by the P3 contract: a save
// that succeeds must still update the live config and reach disk.
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

// TestApplyHealthSettingsPersistSuccessNotifiesDependentsWithValidatedValues
// is the TDD-required proof that a successful apply notifies BOTH dependents
// EXACTLY once each, and with the VALIDATED/CLAMPED values actually
// published — not the raw posted ones. Every field below is deliberately
// posted out of ValidateConfig's clamp range (config.go) so a bug that
// skipped clamping, or that notified from the pre-validation input, would
// still be caught even though it would slip past a test that only used
// already-valid input.
func TestApplyHealthSettingsPersistSuccessNotifiesDependentsWithValidatedValues(t *testing.T) {
	before := config.DefaultHealthSettings()
	m := newHealthTestMiner(t, before)
	canarySpy, watchdogSpy := installHealthDependentSpies(m)

	next := config.HealthSettings{
		CanaryEnabled:                   true,
		CanaryChannel:                   "overflow_channel",
		CanaryIntervalMinutes:           99999, // clamps to 1440
		CanaryMaxStalenessHours:         0,     // clamps to 1, then raised to cover the interval
		WatchdogEnabled:                 true,
		WatchdogStallDelayMinutes:       -5,   // clamps to 10
		WatchdogStallConfirmations:      0,    // clamps to 2
		WatchdogRecoveryCooldownMinutes: 999,  // clamps to 60
		WatchdogAvoidTTLMinutes:         1,    // clamps to 10
		WatchdogRearmHours:              1000, // clamps to 48
	}
	if err := m.ApplyHealthSettings(next); err != nil {
		t.Fatalf("ApplyHealthSettings: %v", err)
	}

	applied := m.CurrentHealthSettings()
	wantClamped := config.HealthSettings{
		CanaryEnabled:                   true,
		CanaryChannel:                   "overflow_channel",
		CanaryIntervalMinutes:           1440,
		CanaryMaxStalenessHours:         24,
		WatchdogEnabled:                 true,
		WatchdogStallDelayMinutes:       10,
		WatchdogStallConfirmations:      2,
		WatchdogRecoveryCooldownMinutes: 60,
		WatchdogAvoidTTLMinutes:         10,
		WatchdogRearmHours:              48,
	}
	if applied != wantClamped {
		t.Fatalf("published settings = %+v, want the clamped %+v (test's clamp-math assumption is broken, or ValidateConfig changed)", applied, wantClamped)
	}

	if canarySpy.calls != 1 {
		t.Errorf("canary seam called %d times on a successful apply, want exactly 1", canarySpy.calls)
	}
	if watchdogSpy.calls != 1 {
		t.Errorf("watchdog seam called %d times on a successful apply, want exactly 1", watchdogSpy.calls)
	}

	if want := healthCanaryConfig(wantClamped); canarySpy.last != want {
		t.Errorf("canary seam received %+v, want the validated/clamped %+v", canarySpy.last, want)
	}
	if want := healthWatchdogConfig(wantClamped); watchdogSpy.last != want {
		t.Errorf("watchdog seam received %+v, want the validated/clamped %+v", watchdogSpy.last, want)
	}
}

// TestApplyHealthSettingsWithoutConfigPathStaysSuccessful pins the existing
// configPath == "" semantics unchanged: with no config file configured there
// is nothing to persist, and the apply is a plain success.
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

// TestApplyHealthSettingsWithoutConfigPathNotifiesDependents is the
// TDD-required seam counterpart: configPath == "" has nothing to persist, but
// must still hot-apply — both dependents notified exactly once.
func TestApplyHealthSettingsWithoutConfigPathNotifiesDependents(t *testing.T) {
	before := config.DefaultHealthSettings()
	m := &Miner{config: &config.Config{Username: "tester", Health: before}, configPath: ""}
	canarySpy, watchdogSpy := installHealthDependentSpies(m)

	next := changedHealthSettings(before)
	if err := m.ApplyHealthSettings(next); err != nil {
		t.Fatalf("ApplyHealthSettings with no configPath returned an error: %v", err)
	}

	if canarySpy.calls != 1 {
		t.Errorf("canary seam called %d times with no configPath, want exactly 1 (nothing to persist is still a successful hot-apply)", canarySpy.calls)
	}
	if watchdogSpy.calls != 1 {
		t.Errorf("watchdog seam called %d times with no configPath, want exactly 1", watchdogSpy.calls)
	}
}

// TestApplyHealthSettingsPersistFailureWithRealDependentsWiredDoesNotPanic
// wires REAL (non-nil) canary and progress-watchdog instances — like a
// running miner with the Health Center enabled would have — with no spy seam
// installed, so ApplyHealthSettings takes its production nil-seam fallback
// path, and proves the commit-point guarantee holds with them attached: the
// failure path still returns an error, leaves CurrentHealthSettings
// untouched, and does not panic.
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
// mirrors the success half with real canary/watchdog instances attached (no
// spy seam), pinning that the production nil-seam fallback path works
// end-to-end and not just when a test spy is installed.
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

// TestApplyHealthSettingsConcurrentApplyKeepsDependentsInSyncWithCommit is
// the TDD/mutation-tested proof for PR #160's Major review finding: without
// healthApplyMu (miner.go) serializing the ENTIRE apply — commit through
// both dependent notifications — two concurrent ApplyHealthSettings calls
// can leave the canary/watchdog holding an earlier call's settings even
// though a LATER call's settings are what actually got persisted and
// published.
//
// It forces exactly that interleaving deterministically, with no sleep or
// polling: goroutine A commits sA, then blocks — via the healthCanaryUpdate
// seam, the first thing ApplyHealthSettings calls after publishing — before
// notifying either dependent. This test then drives B's ApplyHealthSettings
// (sB) to full completion — commit, canary notify, AND watchdog notify —
// before ever releasing A to make its own, by-then-stale, notifications.
//
// Which of the two branches below runs is decided once, deterministically,
// by a single TryLock on m.healthApplyMu taken immediately after A signals
// it has committed. By that point A has already called healthApplyMu.Lock()
// as ApplyHealthSettings' very first statement, if that call exists at all
// — so the probe can never race it: held means the fix is present and B is
// guaranteed to block on entry until A fully returns (release A
// immediately; B simply follows once unblocked); free means nothing can
// block B, so this drives B to completion synchronously, on this goroutine,
// before A is ever released — reproducing the bad interleaving the Major
// finding describes.
//
// Either branch converges on the same assertion: both seams' final observed
// values must equal the final CurrentHealthSettings() — B's, in every case
// this test drives — never A's superseded ones.
func TestApplyHealthSettingsConcurrentApplyKeepsDependentsInSyncWithCommit(t *testing.T) {
	before := config.DefaultHealthSettings()
	m := newHealthTestMiner(t, before)

	sA := changedHealthSettings(before)
	sB := changedHealthSettings(sA)

	var mu sync.Mutex
	canaryCalls, watchdogCalls := 0, 0
	var canaryLast health.CanaryConfig
	var watchdogLast health.WatchdogConfig
	aPaused := false

	aAtCanary := make(chan struct{})
	aResume := make(chan struct{})

	m.healthCanaryUpdate = func(cfg health.CanaryConfig) {
		mu.Lock()
		first := !aPaused
		aPaused = true
		mu.Unlock()
		if first {
			close(aAtCanary)
			<-aResume
		}
		mu.Lock()
		canaryCalls++
		canaryLast = cfg
		mu.Unlock()
	}
	m.healthWatchdogUpdate = func(cfg health.WatchdogConfig) {
		mu.Lock()
		watchdogCalls++
		watchdogLast = cfg
		mu.Unlock()
	}

	aErrCh := make(chan error, 1)
	go func() { aErrCh <- m.ApplyHealthSettings(sA) }()
	<-aAtCanary // A has committed sA and is paused before notifying either dependent.

	bErrCh := make(chan error, 1)
	if !m.healthApplyMu.TryLock() {
		// Held by A: ApplyHealthSettings serializes on it, so B cannot make
		// any progress — not even its own m.mu.Lock() — until A fully
		// returns. Release A now; B simply follows once unblocked, with no
		// further coordination needed.
		go func() { bErrCh <- m.ApplyHealthSettings(sB) }()
		close(aResume)
	} else {
		m.healthApplyMu.Unlock() // undo the probe; nothing else uses this lock.
		// Free: nothing stops B from running to completion right now, on
		// this goroutine, while A sits paused — commit, both notifications,
		// return — before A is ever released. This is the exact
		// interleaving the Major finding describes.
		bErrCh <- m.ApplyHealthSettings(sB)
		close(aResume)
	}

	if err := <-aErrCh; err != nil {
		t.Fatalf("A: ApplyHealthSettings: %v", err)
	}
	if err := <-bErrCh; err != nil {
		t.Fatalf("B: ApplyHealthSettings: %v", err)
	}

	final := m.CurrentHealthSettings()
	if final != sB {
		t.Fatalf("final CurrentHealthSettings = %+v, want B's %+v", final, sB)
	}

	mu.Lock()
	defer mu.Unlock()
	if canaryCalls != 2 {
		t.Fatalf("canary seam called %d times, want exactly 2 (one per concurrent apply)", canaryCalls)
	}
	if want := healthCanaryConfig(sB); canaryLast != want {
		t.Errorf("canary seam's last-observed config = %+v, want the final committed %+v (a stale, superseded notification survived)", canaryLast, want)
	}
	if watchdogCalls != 2 {
		t.Fatalf("watchdog seam called %d times, want exactly 2", watchdogCalls)
	}
	if want := healthWatchdogConfig(sB); watchdogLast != want {
		t.Errorf("watchdog seam's last-observed config = %+v, want the final committed %+v (a stale, superseded notification survived)", watchdogLast, want)
	}
}
