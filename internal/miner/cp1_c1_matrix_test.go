package miner

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// TestApplySettings_NonRenameApply_BackfillsChannelID_C1 proves the
// no-rename coordinator path (applySettingsNoRename) still backfills
// ChannelID onto the persisted config for the common case — adding a new
// streamer, with no rename involved at all. Without this, a non-rename apply
// would never persist the stored-identity anchor (BKM-006 Corrective Pass 1,
// C1) a later cold restart depends on for its stored-ChannelID protection.
func TestApplySettings_NonRenameApply_BackfillsChannelID_C1(t *testing.T) {
	m, _, _ := newCapabilityMiner(t) // no seeded streamers

	rs := m.GetRuntimeSettings()
	rs.Streamers = append(rs.Streamers, settings.StreamerConfig{Username: "freshadd"})
	m.ApplySettings(rs)

	if len(m.config.Streamers) != 1 {
		t.Fatalf("cfg.Streamers = %d entries, want 1", len(m.config.Streamers))
	}
	if got := m.config.Streamers[0].ChannelID; got != "chan-freshadd" {
		t.Errorf("ChannelID = %q, want chan-freshadd (a non-rename apply must still backfill the resolved identity)", got)
	}
}
