package miner

import (
	"context"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/chat"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

type c3ChatLogsBoundary struct {
	streamer  *models.Streamer
	snapshots []c3ChatLogsSnapshot
}

type c3ChatLogsSnapshot struct {
	seen  bool
	value *bool
}

func (c *c3ChatLogsBoundary) ToggleChat(streamer *models.Streamer) {
	c.streamer = streamer
}

func (c *c3ChatLogsBoundary) Leave(string) {}

func (c *c3ChatLogsBoundary) ReconcileLogging(bool, chat.ChatLogger) {
	snapshot := c3ChatLogsSnapshot{seen: c.streamer != nil}
	if c.streamer != nil {
		snapshot.value = c3MinerCloneBool(c.streamer.GetSettings().ChatLogs)
	}
	c.snapshots = append(c.snapshots, snapshot)
	c.streamer = nil
}

func c3MinerBool(value bool) *bool {
	return &value
}

func c3MinerCloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	return c3MinerBool(*value)
}

func c3MinerAssertChatLogs(t *testing.T, got, want *bool) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("ChatLogs = %v, want nil/inherit", *got)
		}
		return
	}
	if got == nil || *got != *want {
		t.Fatalf("ChatLogs = %v, want explicit %v", got, *want)
	}
}

func c3NextBoundarySnapshot(t *testing.T, boundary *c3ChatLogsBoundary, before int) *bool {
	t.Helper()
	if len(boundary.snapshots) != before+1 {
		t.Fatalf("C2 ReconcileLogging calls = %d after apply, want %d", len(boundary.snapshots), before+1)
	}
	snapshot := boundary.snapshots[before]
	if !snapshot.seen {
		t.Fatal("C2 ReconcileLogging ran without receiving the streamer through ToggleChat")
	}
	return snapshot.value
}

func TestApplySettingsCarriesChatLogsTriStateToC2Boundary(t *testing.T) {
	tests := []struct {
		name  string
		state *bool
	}{
		{name: "inherit", state: nil},
		{name: "explicit_false", state: c3MinerBool(false)},
		{name: "explicit_true_with_global_false", state: c3MinerBool(true)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, _, _ := newCapabilityMiner(t, "alpha")
			custom := models.DefaultStreamerSettings()
			custom.ChatLogs = c3MinerCloneBool(tc.state)
			m.config.Streamers[0].Settings = &custom
			m.config.Analytics.EnableChatLogs = false

			boundary := &c3ChatLogsBoundary{}
			m.chatPresence = boundary

			runtimeSettings := m.GetRuntimeSettings()
			before := len(boundary.snapshots)
			if err := m.ApplySettings(context.Background(), runtimeSettings); err != nil {
				t.Fatalf("initial ApplySettings: %v", err)
			}
			c3MinerAssertChatLogs(t, m.streamers.Get("alpha").GetSettings().ChatLogs, tc.state)
			c3MinerAssertChatLogs(t, c3NextBoundarySnapshot(t, boundary, before), tc.state)

			unrelated := m.GetRuntimeSettings()
			if unrelated.Streamers[0].Settings == nil {
				t.Fatal("per-streamer override disappeared before unrelated edit")
			}
			followRaid := !m.streamers.Get("alpha").GetSettings().FollowRaid
			unrelated.Streamers[0].Settings.FollowRaid = &followRaid
			before = len(boundary.snapshots)
			if err := m.ApplySettings(context.Background(), unrelated); err != nil {
				t.Fatalf("unrelated ApplySettings: %v", err)
			}
			c3MinerAssertChatLogs(t, m.streamers.Get("alpha").GetSettings().ChatLogs, tc.state)
			c3MinerAssertChatLogs(t, c3NextBoundarySnapshot(t, boundary, before), tc.state)

			identical := m.GetRuntimeSettings()
			before = len(boundary.snapshots)
			if err := m.ApplySettings(context.Background(), identical); err != nil {
				t.Fatalf("identical ApplySettings: %v", err)
			}
			c3MinerAssertChatLogs(t, m.streamers.Get("alpha").GetSettings().ChatLogs, tc.state)
			c3MinerAssertChatLogs(t, c3NextBoundarySnapshot(t, boundary, before), tc.state)
		})
	}
}
