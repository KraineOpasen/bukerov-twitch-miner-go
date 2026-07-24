package miner

import (
	"testing"
)

// TestApplySettings_Rename_MigratesAutoRedeemRuntimeState_BaselineC4 pins BKM-006
// Corrective Pass 1 defect C4: a confirmed rename must migrate the in-memory
// auto-redeem runtime state (process-lifetime spent budget + the redeemed-reward
// set), not only config.AutoRedeem. Otherwise, after a rename, the spent budget
// resets to zero and an already-redeemed reward can be redeemed again within the
// same availability window — a fail-open on automatic Channel Points spending.
//
// On HEAD 3bb7977 this FAILS: migrateAutoRedeem moves config.AutoRedeem[old] to
// [new] but m.autoRedeemState[old] is never touched, so the runtime spend/redeemed
// state orphans under the old login and the new login starts a fresh budget window.
func TestApplySettings_Rename_MigratesAutoRedeemRuntimeState_BaselineC4(t *testing.T) {
	client := newRenameCapableAPI()
	client.set("oldlogin", "id-c4")
	m, _, _ := newRenameTestMiner(t, client, "oldlogin")

	// Seed runtime auto-redeem state under the old login: 500 already spent, one
	// reward already redeemed this window.
	m.autoRedeemState["oldlogin"] = &autoRedeemRuntime{
		spent:    500,
		redeemed: map[string]bool{"reward-1": true},
	}

	client.set("newlogin", "id-c4")
	m.ApplySettings(renameRuntimeStreamers(m, "oldlogin", "newlogin"))

	if _, stillOld := m.autoRedeemState["oldlogin"]; stillOld {
		t.Error("auto-redeem runtime state is orphaned under the old login after the rename")
	}
	rt := m.autoRedeemState["newlogin"]
	if rt == nil {
		t.Fatal("auto-redeem runtime state was not migrated to the new login (spent budget resets to zero — fail-open)")
	}
	if rt.spent != 500 {
		t.Errorf("migrated spent = %d, want 500 (rename must not reset the process-lifetime budget)", rt.spent)
	}
	if !rt.redeemed["reward-1"] {
		t.Error("migrated redeemed set lost reward-1 (already-redeemed reward could be redeemed again)")
	}
}
