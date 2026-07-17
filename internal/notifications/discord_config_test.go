package notifications

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// TestUpdateDiscordConfigNoReconnectWhenUnchanged is the regression guard for the
// bug where any settings save (e.g. a streamer-priority change) tore down and
// reopened the live Discord session: ApplySettings calls UpdateDiscordConfig on
// every POST with the same effective Discord config, and it reconnected
// unconditionally. With a provider already live and the config unchanged it must
// now be a no-op.
//
// No network is needed: the provider is constructed (not connected) with empty
// credentials, so IsConfigured() is false and Connect() would fail immediately.
// A wrongly-taken reconnect path therefore surfaces as a returned error, while
// the correct no-op returns nil and keeps the same provider.
func TestUpdateDiscordConfigNoReconnectWhenUnchanged(t *testing.T) {
	cfg := &config.DiscordSettings{Enabled: true, BotToken: "", GuildID: ""}
	prov := NewDiscordProvider(cfg.BotToken, cfg.GuildID)
	m := &Manager{discordConfig: cfg, discord: prov}

	same := &config.DiscordSettings{Enabled: true, BotToken: "", GuildID: ""}
	if err := m.UpdateDiscordConfig(same); err != nil {
		t.Fatalf("unchanged Discord config must be a no-op, got reconnect error: %v", err)
	}
	if m.discord != prov {
		t.Fatal("unchanged Discord config replaced the live Discord provider")
	}
}

// TestUpdateDiscordConfigReconnectsWhenChanged proves the no-op guard is specific
// to an unchanged config: a real Discord change (here GuildID) still takes the
// reconnect path. With incomplete credentials that path fails fast (no network),
// which is exactly what confirms the branch was taken rather than skipped.
func TestUpdateDiscordConfigReconnectsWhenChanged(t *testing.T) {
	cfg := &config.DiscordSettings{Enabled: true, BotToken: "", GuildID: "g1"}
	m := &Manager{discordConfig: cfg, discord: NewDiscordProvider(cfg.BotToken, cfg.GuildID)}

	changed := &config.DiscordSettings{Enabled: true, BotToken: "", GuildID: "g2"}
	if err := m.UpdateDiscordConfig(changed); err == nil {
		t.Fatal("a changed Discord config must take the reconnect path (expected a fast connect error with incomplete credentials)")
	}
}
