package miner

import (
	"path/filepath"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// CurrentConfig is what internal/app hands from a finishing generation to the
// next one it builds, so its isolation contract is a memory-safety contract:
// two *Miner values may run concurrently only if they share no map. These
// tests pin that per map, because the failure they prevent —
// `concurrent map read and map write` — is an unrecoverable fatal throw that
// no recover catches and no assertion elsewhere would survive to report.
//
// Each case mutates the LIVE config through the miner's own public writer
// AFTER the snapshot was taken, which is exactly the shape a still-registered
// torn-down generation produces: the web providers a generation registers are
// never cleared, so the dashboard keeps routing to it until its successor
// finishes authenticating.

func TestCurrentConfigSnapshotIsolatesAutoRedeem(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha", "beta")
	m.configPath = filepath.Join(t.TempDir(), "config.json")

	if err := m.SetAutoRedeem("alpha", config.AutoRedeemConfig{
		Enabled:   true,
		RewardIDs: []string{"reward-before"},
		Budget:    100,
	}); err != nil {
		t.Fatalf("seed SetAutoRedeem: %v", err)
	}

	snap := m.CurrentConfig()
	if _, ok := snap.AutoRedeem["alpha"]; !ok {
		t.Fatalf("snapshot lost the seeded AutoRedeem entry: %v", snap.AutoRedeem)
	}

	// A write landing on the miner AFTER the handoff must not reach the
	// snapshot the next generation is running with.
	if err := m.SetAutoRedeem("beta", config.AutoRedeemConfig{
		Enabled:   true,
		RewardIDs: []string{"reward-after"},
		Budget:    200,
	}); err != nil {
		t.Fatalf("late SetAutoRedeem: %v", err)
	}

	if _, leaked := snap.AutoRedeem["beta"]; leaked {
		t.Error("AutoRedeem is shared between the live config and the handed-over snapshot; " +
			"two concurrently running generations would write and read one map under different mutexes")
	}
	if len(snap.AutoRedeem) != 1 {
		t.Errorf("snapshot AutoRedeem = %v, want only the entry present at snapshot time", snap.AutoRedeem)
	}
}

func TestCurrentConfigSnapshotIsolatesDropRules(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.configPath = filepath.Join(t.TempDir(), "config.json")

	m.SetDropRule("rule-before", config.DropRule{HighPriority: true})

	snap := m.CurrentConfig()
	if _, ok := snap.DropRules["rule-before"]; !ok {
		t.Fatalf("snapshot lost the seeded DropRule: %v", snap.DropRules)
	}

	m.SetDropRule("rule-after", config.DropRule{Skip: true})

	if _, leaked := snap.DropRules["rule-after"]; leaked {
		t.Error("DropRules is shared between the live config and the handed-over snapshot")
	}
	if len(snap.DropRules) != 1 {
		t.Errorf("snapshot DropRules = %v, want only the rule present at snapshot time", snap.DropRules)
	}
}

// Notifications.ProviderBatching has no in-place writer today — the
// notifications package deep-copies it on ingest. It is copied anyway because
// it is reached through a by-value struct field, so a shallow copy aliases it,
// and "the snapshot shares no map" is the whole safety argument for handing
// this object to a second running generation. This test keeps that argument
// true by construction, so the first writer that IS added cannot quietly
// reintroduce a cross-generation fatal map race.
func TestCurrentConfigSnapshotIsolatesProviderBatching(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")

	m.mu.Lock()
	m.config.Notifications.ProviderBatching = map[string]config.BatchingSettings{
		"discord": {Enabled: true, Interval: "30m"},
	}
	m.mu.Unlock()

	snap := m.CurrentConfig()
	if _, ok := snap.Notifications.ProviderBatching["discord"]; !ok {
		t.Fatalf("snapshot lost the seeded ProviderBatching entry: %v", snap.Notifications.ProviderBatching)
	}

	m.mu.Lock()
	m.config.Notifications.ProviderBatching["telegram"] = config.BatchingSettings{Enabled: true, Interval: "60m"}
	m.mu.Unlock()

	if _, leaked := snap.Notifications.ProviderBatching["telegram"]; leaked {
		t.Error("Notifications.ProviderBatching is shared between the live config and the handed-over snapshot")
	}
}

// A nil map must stay nil rather than becoming an empty one: the snapshot is
// persisted verbatim by whichever generation receives it, and config.SaveConfig
// omits these fields when they are nil (`omitempty`).
func TestCurrentConfigSnapshotKeepsNilMapsNil(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")

	snap := m.CurrentConfig()
	if snap.AutoRedeem != nil {
		t.Errorf("AutoRedeem = %v, want nil preserved", snap.AutoRedeem)
	}
	if snap.DropRules != nil {
		t.Errorf("DropRules = %v, want nil preserved", snap.DropRules)
	}
	if snap.Notifications.ProviderBatching != nil {
		t.Errorf("ProviderBatching = %v, want nil preserved", snap.Notifications.ProviderBatching)
	}
}
