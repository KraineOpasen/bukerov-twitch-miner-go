package streamer

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// renameFakeClient resolves logins to ChannelIDs from a fixed map, so a rename
// (two distinct logins mapping to the SAME stable ChannelID) can be exercised
// without HTTP. Unknown logins fall back to "chan-"+login, matching the
// existing fakeChannelAPI convention.
type renameFakeClient struct {
	ids map[string]string
}

func (f renameFakeClient) GetChannelID(username string) (string, error) {
	if id, ok := f.ids[username]; ok {
		return id, nil
	}
	return "chan-" + username, nil
}
func (renameFakeClient) LoadChannelPointsContext(*models.Streamer) error { return nil }
func (renameFakeClient) CheckStreamerOnline(*models.Streamer) models.StatusTransition {
	return models.StatusTransition{}
}

// TestApplySettings_RenameSameChannelID_BaselineGap pins BKM-006 invariant A/I1–I5:
// when a configured login changes but resolves to the SAME stable ChannelID, the
// EXISTING runtime streamer must be reconciled IN PLACE — same pointer, same
// ChannelID, settings preserved, roster count unchanged — never removed and
// re-added as a second object.
//
// On base 8cd9bec this FAILS at runtime: manager.ApplySettings matches by login,
// so it removes "oldname" (absent from the new config) and adds a fresh "newname"
// object (a different pointer), losing the original streamer's identity, slot,
// status and history.
func TestApplySettings_RenameSameChannelID_BaselineGap(t *testing.T) {
	defaults := models.DefaultStreamerSettings()
	client := renameFakeClient{ids: map[string]string{"oldname": "123", "newname": "123"}}
	m := NewManager(client, defaults)

	// Seed the roster with "oldname" carrying a non-default (custom) setting so
	// settings-preservation across the rename is observable.
	custom := defaults
	custom.FollowRaid = false
	added, _, _, _ := m.ApplySettings(
		[]config.StreamerConfig{{Username: "oldname", Settings: &custom}}, defaults)
	if len(added) != 1 {
		t.Fatalf("seed: added=%d, want 1", len(added))
	}
	orig := m.Get("oldname")
	if orig == nil || orig.ChannelID != "123" {
		t.Fatalf("seed streamer missing or wrong ChannelID: %+v", orig)
	}

	// Rename: the config now lists "newname" (same ChannelID) with the same
	// settings. This is a rename, not a remove+add.
	m.ApplySettings(
		[]config.StreamerConfig{{Username: "newname", Settings: &custom}}, defaults)

	if got := m.Count(); got != 1 {
		t.Fatalf("roster count = %d, want 1 (a rename must not create a second streamer)", got)
	}
	renamed := m.Get("newname")
	if renamed == nil {
		t.Fatal("streamer not resolvable by the new login after rename")
	}
	if renamed != orig {
		t.Error("rename replaced the streamer object; the SAME pointer must be retained (slot/status/history continuity)")
	}
	if renamed.ChannelID != "123" {
		t.Errorf("ChannelID after rename = %q, want \"123\" (stable identity)", renamed.ChannelID)
	}
	if renamed.GetSettings().FollowRaid {
		t.Error("custom settings were not preserved across the rename")
	}
	if m.Get("oldname") != nil {
		t.Error("the old login still resolves to a streamer after the rename")
	}
}
