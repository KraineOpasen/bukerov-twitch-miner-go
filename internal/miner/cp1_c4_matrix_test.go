package miner

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
)

// TestApplySettings_Rename_AutoRedeemRuntimeState_DestinationCollision_C4B
// pins BKM-006 Corrective Pass 1 test matrix item C4-B: a rename whose
// DESTINATION login already has its own auto-redeem runtime state (e.g. a
// prior manual entry, or two renames converging on one login) must merge
// conservatively rather than pick a winner arbitrarily — redeemed sets union
// (an already-redeemed reward is never re-armed), spent is the MAX of the
// two (never increases the available budget = configured budget - spent),
// and the old key is removed.
func TestApplySettings_Rename_AutoRedeemRuntimeState_DestinationCollision_C4B(t *testing.T) {
	client := newRenameCapableAPI()
	client.set("oldlogin", "id-c4b")
	m, _, _ := newRenameTestMiner(t, client, "oldlogin")

	m.autoRedeemState["oldlogin"] = &autoRedeemRuntime{
		spent:    300,
		redeemed: map[string]bool{"reward-old": true},
	}
	m.autoRedeemState["newlogin"] = &autoRedeemRuntime{
		spent:    900,
		redeemed: map[string]bool{"reward-new": true},
	}

	client.set("newlogin", "id-c4b")
	m.ApplySettings(renameRuntimeStreamers(m, "oldlogin", "newlogin"))

	if _, stillOld := m.autoRedeemState["oldlogin"]; stillOld {
		t.Error("old-login auto-redeem runtime state must be removed after a collision merge")
	}
	rt := m.autoRedeemState["newlogin"]
	if rt == nil {
		t.Fatal("destination auto-redeem runtime state missing after the merge")
	}
	if rt.spent != 900 {
		t.Errorf("merged spent = %d, want 900 (max(300,900) — a merge must never INCREASE the available budget)", rt.spent)
	}
	if !rt.redeemed["reward-old"] || !rt.redeemed["reward-new"] {
		t.Errorf("merged redeemed set incomplete: %+v, want both reward-old and reward-new (union, never re-arms a redeemed reward)", rt.redeemed)
	}
	if len(rt.redeemed) != 2 {
		t.Errorf("merged redeemed set has %d entries, want exactly 2", len(rt.redeemed))
	}
}

// TestMigrateAutoRedeemRuntimeState_SameLoginIsNoop proves a rename whose
// old/new login normalize to the SAME key (case-only change) is a no-op —
// it must never delete the state it was about to "migrate" onto itself.
func TestMigrateAutoRedeemRuntimeState_SameLoginIsNoop(t *testing.T) {
	state := map[string]*autoRedeemRuntime{
		"steady": {spent: 42, redeemed: map[string]bool{"r": true}},
	}
	migrateAutoRedeemRuntimeState(state, []streamer.RenameEvent{
		{OldLogin: "steady", NewLogin: "STEADY", ChannelID: "id"},
	})
	rt := state["steady"]
	if rt == nil || rt.spent != 42 || !rt.redeemed["r"] {
		t.Errorf("same-login (case-only) migration must be a no-op: %+v", state)
	}
}

// TestMigrateAutoRedeemRuntimeState_NoOldStateIsNoop proves migrating a
// rename for a streamer with NO existing runtime state does not fabricate
// one (nothing to migrate, nothing created).
func TestMigrateAutoRedeemRuntimeState_NoOldStateIsNoop(t *testing.T) {
	state := map[string]*autoRedeemRuntime{}
	migrateAutoRedeemRuntimeState(state, []streamer.RenameEvent{
		{OldLogin: "oldlogin", NewLogin: "newlogin", ChannelID: "id"},
	})
	if len(state) != 0 {
		t.Errorf("state = %+v, want empty (nothing to migrate)", state)
	}
}
