package streamer

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestLoadFromConfig_StoredChannelIDMismatch_ColdRestart_BaselineC1 pins BKM-006
// Corrective Pass 1 defect C1: a persisted, non-empty config ChannelID must be an
// EXPECTED immutable broadcaster identity on a cold restart, not merely a hint.
//
// Scenario: config carries {username: "oldlogin", channelId: "123"}. The process
// restarts (a FRESH manager, empty byID). Meanwhile the login "oldlogin" now
// belongs to a DIFFERENT broadcaster and resolves to "999". The stored identity
// "123" must win: no runtime streamer for the foreign ChannelID "999" may be
// created, and the entry must not silently adopt the foreign identity.
//
// On HEAD 3bb7977 this FAILS: resolveConfigs ignores the stored ChannelID and
// reconcile matches only by the freshly-resolved id, so a streamer with
// ChannelID "999" (a foreign broadcaster) is created, hijacking the config entry.
func TestLoadFromConfig_StoredChannelIDMismatch_ColdRestart_BaselineC1(t *testing.T) {
	defaults := models.DefaultStreamerSettings()
	client := renameFakeClient{ids: map[string]string{"oldlogin": "999"}}
	m := NewManager(client, defaults)

	// Cold restart: the persisted config already carries the stable identity.
	_ = m.LoadFromConfig([]config.StreamerConfig{
		{Username: "oldlogin", ChannelID: "123"},
	}, nil)

	if s := m.GetByChannelID("999"); s != nil {
		t.Fatalf("a foreign broadcaster (ChannelID 999) was tracked for a config entry whose stored identity is 123: %q", s.GetUsername())
	}
	if s := m.Get("oldlogin"); s != nil && s.ChannelID != "123" {
		t.Fatalf("config entry adopted a foreign ChannelID %q (stored identity 123 must win)", s.ChannelID)
	}
}
