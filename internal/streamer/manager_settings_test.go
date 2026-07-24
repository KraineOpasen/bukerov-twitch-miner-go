package streamer

import (
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakeChannelAPI satisfies the manager's twitchClient slice so ApplySettings
// can resolve added streamers without HTTP.
type fakeChannelAPI struct{}

func (fakeChannelAPI) GetChannelID(username string) (string, error) { return "chan-" + username, nil }
func (fakeChannelAPI) LoadChannelPointsContext(*models.Streamer) error {
	return nil
}
func (fakeChannelAPI) CheckStreamerOnline(*models.Streamer) models.StatusTransition {
	return models.StatusTransition{}
}

func configsFor(usernames ...string) []config.StreamerConfig {
	out := make([]config.StreamerConfig, 0, len(usernames))
	for _, u := range usernames {
		out = append(out, config.StreamerConfig{Username: u})
	}
	return out
}

// seededManager returns a manager already tracking the given streamers with
// the default settings.
func seededManager(t *testing.T, defaults models.StreamerSettings, usernames ...string) *Manager {
	t.Helper()
	m := NewManager(fakeChannelAPI{}, defaults)
	added, removed, changed, _ := m.ApplySettings(configsFor(usernames...), defaults)
	if len(added) != len(usernames) || len(removed) != 0 || len(changed) != 0 {
		t.Fatalf("seeding: added=%d removed=%d changed=%d, want %d/0/0",
			len(added), len(removed), len(changed), len(usernames))
	}
	return m
}

func changedNames(changed []SettingsChange) []string {
	names := make([]string, 0, len(changed))
	for _, c := range changed {
		names = append(names, c.Streamer.Username)
	}
	return names
}

// TestApplySettingsReportsChangedExistingStreamer: an existing streamer whose
// effective settings differ appears exactly once in changed, with faithful
// old/new snapshots, and keeps its resolved ChannelID.
func TestApplySettingsReportsChangedExistingStreamer(t *testing.T) {
	defaults := models.DefaultStreamerSettings()
	m := seededManager(t, defaults, "alpha")

	override := defaults
	override.FollowRaid = false
	override.CommunityGoals = true
	_, _, changed, _ := m.ApplySettings(
		[]config.StreamerConfig{{Username: "alpha", Settings: &override}}, defaults)

	if len(changed) != 1 || changed[0].Streamer.Username != "alpha" {
		t.Fatalf("changed = %v, want exactly [alpha]", changedNames(changed))
	}
	if !changed[0].Old.FollowRaid || changed[0].New.FollowRaid {
		t.Errorf("FollowRaid snapshots: old=%v new=%v, want true->false",
			changed[0].Old.FollowRaid, changed[0].New.FollowRaid)
	}
	if changed[0].Old.CommunityGoals || !changed[0].New.CommunityGoals {
		t.Errorf("CommunityGoals snapshots: old=%v new=%v, want false->true",
			changed[0].Old.CommunityGoals, changed[0].New.CommunityGoals)
	}
	s := m.Get("alpha")
	if s.GetSettings().FollowRaid {
		t.Error("streamer settings were not actually updated")
	}
	if s.ChannelID != "chan-alpha" {
		t.Errorf("ChannelID = %q, want chan-alpha (identity must be preserved)", s.ChannelID)
	}
}

// TestApplySettingsUnchangedSettingsNotReported: a repeat apply with identical
// effective settings reports no change.
func TestApplySettingsUnchangedSettingsNotReported(t *testing.T) {
	defaults := models.DefaultStreamerSettings()
	m := seededManager(t, defaults, "alpha")

	_, _, changed, _ := m.ApplySettings(configsFor("alpha"), defaults)
	if len(changed) != 0 {
		t.Fatalf("identical apply reported changed = %v, want none", changedNames(changed))
	}

	// An explicit override VALUE-equal to the defaults is also not a change.
	same := defaults
	_, _, changed, _ = m.ApplySettings(
		[]config.StreamerConfig{{Username: "alpha", Settings: &same}}, defaults)
	if len(changed) != 0 {
		t.Fatalf("value-equal explicit override reported changed = %v, want none", changedNames(changed))
	}
}

// TestApplySettingsDefaultsToExplicitOverride: moving from inherited defaults
// to a differing explicit override is a change; ChatLogs pointer semantics
// (nil "inherit" vs explicit false) are honored.
func TestApplySettingsDefaultsToExplicitOverride(t *testing.T) {
	defaults := models.DefaultStreamerSettings()
	m := seededManager(t, defaults, "alpha")

	explicitOff := false
	override := defaults
	override.ChatLogs = &explicitOff
	_, _, changed, _ := m.ApplySettings(
		[]config.StreamerConfig{{Username: "alpha", Settings: &override}}, defaults)
	if len(changed) != 1 {
		t.Fatalf("defaults->explicit ChatLogs override: changed = %v, want [alpha]", changedNames(changed))
	}
	if got := m.Get("alpha").GetSettings().ChatLogs; got == nil || *got {
		t.Errorf("ChatLogs after override = %v, want explicit false", got)
	}
}

// TestApplySettingsExplicitOverrideBackToDefaults: dropping the per-streamer
// override reverts to defaults and is reported as a change.
func TestApplySettingsExplicitOverrideBackToDefaults(t *testing.T) {
	defaults := models.DefaultStreamerSettings()
	m := seededManager(t, defaults, "alpha")

	override := defaults
	override.MakePredictions = false
	if _, _, changed, _ := m.ApplySettings(
		[]config.StreamerConfig{{Username: "alpha", Settings: &override}}, defaults); len(changed) != 1 {
		t.Fatalf("setup override apply not reported as change")
	}

	_, _, changed, _ := m.ApplySettings(configsFor("alpha"), defaults)
	if len(changed) != 1 || changed[0].New.MakePredictions != true {
		t.Fatalf("override->defaults: changed=%v newMakePredictions=%v, want one change back to true",
			changedNames(changed), len(changed) == 1 && changed[0].New.MakePredictions)
	}
}

// TestApplySettingsAddedAndRemovedNotDuplicatedInChanged: roster membership
// changes never leak into the changed list.
func TestApplySettingsAddedAndRemovedNotDuplicatedInChanged(t *testing.T) {
	defaults := models.DefaultStreamerSettings()
	m := seededManager(t, defaults, "alpha", "bravo")

	// One apply: add charlie, remove bravo, keep alpha unchanged.
	added, removed, changed, _ := m.ApplySettings(configsFor("alpha", "charlie"), defaults)
	if len(added) != 1 || added[0].Username != "charlie" {
		t.Fatalf("added = %v, want [charlie]", added)
	}
	if len(removed) != 1 || removed[0].Username != "bravo" {
		t.Fatalf("removed = %v, want [bravo]", removed)
	}
	if len(changed) != 0 {
		t.Fatalf("changed = %v, want none (added/removed must not be duplicated)", changedNames(changed))
	}
	if m.Get("charlie").ChannelID != "chan-charlie" {
		t.Errorf("added streamer ChannelID = %q, want chan-charlie", m.Get("charlie").ChannelID)
	}
}

// TestApplySettingsConcurrentGetSetSettings: GetSettings/SetSettings and
// repeated ApplySettings race cleanly (verified under -race).
func TestApplySettingsConcurrentGetSetSettings(t *testing.T) {
	defaults := models.DefaultStreamerSettings()
	m := seededManager(t, defaults, "alpha", "bravo")

	override := defaults
	override.FollowRaid = false
	withOverride := []config.StreamerConfig{
		{Username: "alpha", Settings: &override},
		{Username: "bravo"},
	}
	plain := configsFor("alpha", "bravo")

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				if (i+j)%2 == 0 {
					m.ApplySettings(withOverride, defaults)
				} else {
					m.ApplySettings(plain, defaults)
				}
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				for _, s := range m.All() {
					_ = s.GetSettings()
				}
				if s := m.Get("alpha"); s != nil {
					next := s.GetSettings()
					next.WatchStreak = !next.WatchStreak
					s.SetSettings(next)
				}
			}
		}()
	}
	wg.Wait()

	if m.Count() != 2 {
		t.Fatalf("roster corrupted by concurrent applies: count=%d, want 2", m.Count())
	}
}
