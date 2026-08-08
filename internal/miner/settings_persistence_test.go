package miner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// TestApplySettingsNoRenamePersistFailureIsReportedAndMutatesNothing is the
// BLOCK-1 core invariant for the ORDINARY settings path (no rename, no
// removal — the shape every Settings-page save takes): when config.json
// cannot be written, the apply must FAIL OUT LOUD and leave absolutely
// nothing changed.
//
// Before this pass the no-rename path mutated the live config and committed
// the runtime roster BEFORE persisting, and finishApply only slog.Error'd a
// SaveConfig failure — so ApplySettings returned nil and POST /api/settings
// answered 200 after a failed durable write, with runtime and disk silently
// diverged. Persistence is now the commit point, exactly as it already was
// on the removal and rename paths.
//
// The persistence failure is induced with breakConfigPathForNextSave — the
// same deterministic seam the rename path's C2-B matrix test already uses —
// so a real config.json is written first and can be read back afterwards to
// prove the ON-DISK state is untouched too.
func TestApplySettingsNoRenamePersistFailureIsReportedAndMutatesNothing(t *testing.T) {
	m, topics, chat := newCapabilityMiner(t, "alpha", "beta")
	configPath := filepath.Join(t.TempDir(), "config.json")
	m.configPath = configPath
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	onDiskBefore, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}
	breakConfigPathForNextSave(t, configPath)

	cfgBefore := m.config
	consoleBefore := m.config.Logger.ConsoleLevel
	blacklistBefore := append([]string(nil), m.config.DropBlacklist...)
	followRaidBefore := m.streamers.Get("alpha").GetSettings().FollowRaid

	rs := settings.BuildRuntimeSettings(m.config)
	rs.Logger.ConsoleLevel = "debug"
	rs.DropBlacklist = []string{"persist-failure-marker"}
	overrideStreamer(&rs, "alpha", func(ss *settings.StreamerSettingsConfig) {
		ss.FollowRaid = boolPtr(!followRaidBefore)
	})

	if err := m.ApplySettings(context.Background(), rs); err == nil {
		t.Fatal("ApplySettings returned nil after config.json persistence failed; the caller (POST /api/settings) would report success for a change that never reached disk")
	}

	// The live config must be the very same object, with the very same
	// contents: no publish, no in-place mutation.
	if m.config != cfgBefore {
		t.Error("live config was republished despite the persistence failure")
	}
	if got := m.config.Logger.ConsoleLevel; got != consoleBefore {
		t.Errorf("live config mutated: Logger.ConsoleLevel = %q, want %q", got, consoleBefore)
	}
	if len(m.config.DropBlacklist) != len(blacklistBefore) {
		t.Errorf("live config mutated: DropBlacklist = %v, want %v", m.config.DropBlacklist, blacklistBefore)
	}

	// The runtime roster must not have been reconciled either.
	if got := m.streamers.Get("alpha").GetSettings().FollowRaid; got != followRaidBefore {
		t.Errorf("runtime streamer settings mutated: alpha.FollowRaid = %v, want %v", got, followRaidBefore)
	}
	if topics.touchedUserTopic() {
		t.Error("runtime capabilities were reconciled despite the persistence failure")
	}
	if n := chat.toggleCount("alpha"); n != 0 {
		t.Errorf("chat presence was reconciled despite the persistence failure: %d ToggleChat calls for alpha", n)
	}

	// And nothing may have reached disk: restoring the path back to a plain
	// file must reveal exactly the bytes that were there before the apply.
	if err := os.Remove(configPath); err != nil {
		t.Fatalf("unbreak config path: %v", err)
	}
	if err := os.WriteFile(configPath, onDiskBefore, 0o600); err != nil {
		t.Fatalf("restore config: %v", err)
	}
	reloaded, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if reloaded.Logger.ConsoleLevel != consoleBefore {
		t.Errorf("on-disk config changed despite the persistence failure: Logger.ConsoleLevel = %q, want %q", reloaded.Logger.ConsoleLevel, consoleBefore)
	}
}

// TestApplySettingsNoRenamePersistSuccessAppliesAndPersists is the other half
// of the BLOCK-1 contract: where persistence SUCCEEDS, the ordinary settings
// path behaves exactly as it always has — the change reaches the live config,
// the runtime roster, and config.json.
func TestApplySettingsNoRenamePersistSuccessAppliesAndPersists(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha", "beta")
	configPath := filepath.Join(t.TempDir(), "config.json")
	m.configPath = configPath

	followRaidBefore := m.streamers.Get("alpha").GetSettings().FollowRaid

	rs := settings.BuildRuntimeSettings(m.config)
	rs.Logger.ConsoleLevel = "debug"
	rs.DropBlacklist = []string{"persist-success-marker"}
	overrideStreamer(&rs, "alpha", func(ss *settings.StreamerSettingsConfig) {
		ss.FollowRaid = boolPtr(!followRaidBefore)
	})

	if err := m.ApplySettings(context.Background(), rs); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	if got := m.config.Logger.ConsoleLevel; got != "debug" {
		t.Errorf("live config not updated: Logger.ConsoleLevel = %q, want \"debug\"", got)
	}
	if got := m.streamers.Get("alpha").GetSettings().FollowRaid; got == followRaidBefore {
		t.Errorf("runtime streamer settings not updated: alpha.FollowRaid = %v, want %v", got, !followRaidBefore)
	}

	loaded, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if loaded.Logger.ConsoleLevel != "debug" {
		t.Errorf("persisted config missing the change: Logger.ConsoleLevel = %q, want \"debug\"", loaded.Logger.ConsoleLevel)
	}
	if len(loaded.DropBlacklist) != 1 || loaded.DropBlacklist[0] != "persist-success-marker" {
		t.Errorf("persisted config missing the change: DropBlacklist = %v", loaded.DropBlacklist)
	}
}

// TestApplySettingsNoRenameWithoutConfigPathStaysSuccessful pins the
// documented configPath == "" semantics unchanged: with no config file
// configured there is nothing to persist, and the apply is a plain success —
// the same no-op the removal and rename commit points already treat it as.
// Making persistence the commit point must not turn "no config file" into a
// failure.
func TestApplySettingsNoRenameWithoutConfigPathStaysSuccessful(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.configPath = ""

	rs := settings.BuildRuntimeSettings(m.config)
	rs.Logger.ConsoleLevel = "debug"

	if err := m.ApplySettings(context.Background(), rs); err != nil {
		t.Fatalf("ApplySettings with no config path must succeed, got: %v", err)
	}
	if got := m.config.Logger.ConsoleLevel; got != "debug" {
		t.Errorf("live config not updated: Logger.ConsoleLevel = %q, want \"debug\"", got)
	}
}

// TestApplySettingsNoRenamePersistsResolvedChannelIDs pins the stored-identity
// anchor a cold restart depends on: a newly added streamer's ChannelID, freshly
// resolved by THIS apply, must appear in the config.json this apply writes.
// Moving persistence ahead of CommitPlan means the candidate has to be stamped
// from the plan's own resolution rather than from the post-commit roster — this
// test is what keeps that stamping in place.
func TestApplySettingsNoRenamePersistsResolvedChannelIDs(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	configPath := filepath.Join(t.TempDir(), "config.json")
	m.configPath = configPath

	rs := settings.BuildRuntimeSettings(m.config)
	rs.Streamers = append(rs.Streamers, settings.StreamerConfig{Username: "gamma"})

	if err := m.ApplySettings(context.Background(), rs); err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}

	loaded, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	for _, want := range []string{"alpha", "gamma"} {
		var found bool
		for _, sc := range loaded.Streamers {
			if sc.Username != want {
				continue
			}
			found = true
			if sc.ChannelID != "chan-"+want {
				t.Errorf("persisted config missing the resolved ChannelID for %q: got %q, want %q", want, sc.ChannelID, "chan-"+want)
			}
		}
		if !found {
			t.Errorf("persisted config missing streamer %q: %+v", want, loaded.Streamers)
		}
	}
}
