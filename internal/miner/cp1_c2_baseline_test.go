package miner

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// TestApplySettings_Rename_FailClosedOnAnalyticsConflict_BaselineC2 pins BKM-006
// Corrective Pass 1 defect C2: a rename must be a fail-closed transaction. If the
// durable analytics history migration cannot complete (here: a name collision,
// because both the old and the new login already have independent recorded
// history), the runtime must NOT be left renamed — otherwise runtime + in-memory
// config are under the new login while analytics history stays under the old one,
// a permanently split identity that only produced a logged warning.
//
// On HEAD 3bb7977 this FAILS: manager.ApplySettings mutates the runtime (login +
// byLogin) BEFORE the analytics migration runs, and the analytics conflict is
// swallowed as a non-fatal warning — so the runtime is renamed even though the
// durable history migration failed.
func TestApplySettings_Rename_FailClosedOnAnalyticsConflict_BaselineC2(t *testing.T) {
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("new analytics service: %v", err)
	}
	// Seed independent history under BOTH logins → RenameStreamer(old,new) will
	// fail closed with a collision (it must never silently merge two histories).
	if err := svc.Repository().RecordPoints("c2old", 111, "WATCH"); err != nil {
		t.Fatalf("seed old: %v", err)
	}
	if err := svc.Repository().RecordPoints("c2new", 222, "WATCH"); err != nil {
		t.Fatalf("seed new: %v", err)
	}

	client := newRenameCapableAPI()
	client.set("c2old", "id-c2")
	m, _, _ := newRenameTestMiner(t, client, "c2old")
	m.analyticsSvc = svc

	client.set("c2new", "id-c2")
	m.ApplySettings(renameRuntimeStreamers(m, "c2old", "c2new"))

	if m.streamers.Get("c2old") == nil {
		t.Error("runtime was renamed away from c2old even though the durable analytics migration failed (not fail-closed)")
	}
	if m.streamers.Get("c2new") != nil {
		t.Error("runtime is under the new login c2new despite the analytics migration conflict (partial committed state)")
	}
	if len(m.config.Streamers) == 1 && m.config.Streamers[0].Username == "c2new" {
		t.Error("in-memory config was moved to c2new despite the durable migration failing")
	}
}
