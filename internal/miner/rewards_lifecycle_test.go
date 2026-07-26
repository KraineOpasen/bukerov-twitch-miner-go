package miner

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
)

// This file covers M2's SetAutoRedeem lifecycle and its interaction with the
// commit points of applySettingsWithRemovals/applySettingsWithRename
// (design §7 tests 1-10, 16-18). Every "concurrent" scenario here uses the
// applyCommitBarrier tests-only seam, invoked SYNCHRONOUSLY from within the
// apply's own call stack (before the commit m.mu.Lock for preCommit, after
// the commit m.mu.Unlock for postCommit) — no goroutines/sleeps needed,
// since the barrier callback runs on the SAME goroutine as the apply, at a
// point where m.mu is provably free.

// seedConfigFile writes m.config to a fresh temp config.json and wires
// m.configPath to it, so SetAutoRedeem/apply commit points have a real file
// to persist to and reload from.
func seedConfigFile(t *testing.T, m *Miner) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config file: %v", err)
	}
	m.configPath = configPath
	return configPath
}

// Test 1: SetAutoRedeem persistence, disable/delete and state reset.
func TestSetAutoRedeem_PersistenceAndDisableDeletesState(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "lifecyclealpha")
	configPath := seedConfigFile(t, m)

	if err := m.SetAutoRedeem("lifecyclealpha", config.AutoRedeemConfig{Enabled: true, Budget: 100, RewardIDs: []string{"r1", "r1", " r2 "}}); err != nil {
		t.Fatalf("SetAutoRedeem: %v", err)
	}
	got := m.GetAutoRedeem("lifecyclealpha")
	if !got.Enabled || got.Budget != 100 || len(got.RewardIDs) != 2 {
		t.Fatalf("in-memory config after enable = %+v, want Enabled/Budget=100/2 deduped rewards", got)
	}
	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if diskCfg, ok := onDisk.AutoRedeem["lifecyclealpha"]; !ok || diskCfg.Budget != 100 {
		t.Errorf("disk config missing the entry: %+v (ok=%v)", diskCfg, ok)
	}
	if got := m.autoRedeemGen["lifecyclealpha"]; got != 1 {
		t.Errorf("generation after first successful SetAutoRedeem = %d, want 1", got)
	}

	// Seed runtime state as if a redemption already happened this window.
	m.autoRedeemState["lifecyclealpha"] = &autoRedeemRuntime{spent: 40, redeemed: map[string]bool{"r1": true}}

	// Disable-to-zero: the config entry, disk entry, and runtime state must
	// all be deleted, and the generation must bump again.
	if err := m.SetAutoRedeem("lifecyclealpha", config.AutoRedeemConfig{}); err != nil {
		t.Fatalf("SetAutoRedeem (disable): %v", err)
	}
	if _, ok := m.config.AutoRedeem["lifecyclealpha"]; ok {
		t.Error("in-memory config entry survived disabling")
	}
	if _, ok := m.autoRedeemState["lifecyclealpha"]; ok {
		t.Error("runtime state survived disabling")
	}
	onDisk2, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config after disable: %v", err)
	}
	if _, ok := onDisk2.AutoRedeem["lifecyclealpha"]; ok {
		t.Error("disk entry survived disabling")
	}
	if got := m.autoRedeemGen["lifecyclealpha"]; got != 2 {
		t.Errorf("generation after disable = %d, want 2 (bumped again)", got)
	}
}

// Test 2: a SaveConfig failure leaves memory and disk consistent, including
// the wasNil->nil restore case. Kills mutant 4 (ignore SaveConfig error) and
// mutant 5 (remove the restore-on-failure).
func TestSetAutoRedeem_SaveConfigFailureLeavesMemoryAndDiskConsistent(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "lifecyclebeta")
	configPath := seedConfigFile(t, m)

	if err := m.SetAutoRedeem("lifecyclebeta", config.AutoRedeemConfig{Enabled: true, Budget: 50, RewardIDs: []string{"r1"}}); err != nil {
		t.Fatalf("seed SetAutoRedeem: %v", err)
	}
	prev := m.GetAutoRedeem("lifecyclebeta")
	preGen := m.autoRedeemGen["lifecyclebeta"]
	preState := m.autoRedeemState["lifecyclebeta"]

	breakConfigPathForNextSave(t, configPath)

	err := m.SetAutoRedeem("lifecyclebeta", config.AutoRedeemConfig{Enabled: true, Budget: 999, RewardIDs: []string{"different"}})
	if err == nil {
		t.Fatal("expected SetAutoRedeem to fail when SaveConfig fails")
	}

	if got := m.GetAutoRedeem("lifecyclebeta"); got.Enabled != prev.Enabled || got.Budget != prev.Budget || strings.Join(got.RewardIDs, ",") != strings.Join(prev.RewardIDs, ",") {
		t.Errorf("GetAutoRedeem after a failed save = %+v, want the PREVIOUS value %+v (restore-on-failure)", got, prev)
	}
	if got := m.autoRedeemGen["lifecyclebeta"]; got != preGen {
		t.Errorf("generation changed despite the save failing: got %d, want %d", got, preGen)
	}
	if m.autoRedeemState["lifecyclebeta"] != preState {
		t.Error("runtime state pointer changed despite the save failing")
	}
	info, statErr := os.Stat(configPath)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("configPath must still be the untouched directory this test installed (disk untouched): stat=%v, err=%v", info, statErr)
	}

	// wasNil -> nil restore case: a fresh miner whose config never had an
	// AutoRedeem map at all.
	m2, _, _ := newCapabilityMiner(t, "lifecyclegamma")
	configPath2 := seedConfigFile(t, m2)
	if m2.config.AutoRedeem != nil {
		t.Fatal("setup: expected a nil AutoRedeem map")
	}
	breakConfigPathForNextSave(t, configPath2)
	if err := m2.SetAutoRedeem("lifecyclegamma", config.AutoRedeemConfig{Enabled: true, Budget: 10, RewardIDs: []string{"r"}}); err == nil {
		t.Fatal("expected SetAutoRedeem to fail when SaveConfig fails")
	}
	if m2.config.AutoRedeem != nil {
		t.Errorf("AutoRedeem map must be restored to nil after a failed save on a previously-nil map, got %+v", m2.config.AutoRedeem)
	}
}

// Test 3: SetAutoRedeem concurrent with a removal apply does not lose the
// update (D1). Kills mutant 1 (no refresh at all) among others.
func TestSetAutoRedeem_ConcurrentWithRemovalApply_NoLostUpdate(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "conckeep", "concvictim")
	configPath := seedConfigFile(t, m)

	var barrierErr error
	m.applyCommitBarrier = func(phase applyCommitPhase) {
		if phase != applyPreCommit {
			return
		}
		barrierErr = m.SetAutoRedeem("conckeep", config.AutoRedeemConfig{Enabled: true, Budget: 250, RewardIDs: []string{"r-keep"}})
	}

	rs := removeStreamer(m.GetRuntimeSettings(), "concvictim")
	if err := m.applySettings(context.Background(), rs); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if barrierErr != nil {
		t.Fatalf("SetAutoRedeem during the preCommit barrier failed: %v", barrierErr)
	}

	got := m.GetAutoRedeem("conckeep")
	if !got.Enabled || got.Budget != 250 {
		t.Fatalf("kept streamer's concurrent SetAutoRedeem was lost: %+v", got)
	}
	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if diskCfg, ok := onDisk.AutoRedeem["conckeep"]; !ok || diskCfg.Budget != 250 {
		t.Errorf("kept streamer's edit did not survive on disk: %+v (ok=%v)", diskCfg, ok)
	}
	if _, ok := onDisk.AutoRedeem["concvictim"]; ok {
		t.Error("removed streamer's AutoRedeem entry survived on disk")
	}
}

// Test 4: SetAutoRedeem concurrent with a rename apply does not lose the
// update, and the renamed streamer's own entry migrates correctly.
func TestSetAutoRedeem_ConcurrentWithRenameApply_NoLostUpdate(t *testing.T) {
	client := newRenameCapableAPI()
	client.set("concrenold", "id-concrename")
	m, _, _ := newRenameTestMiner(t, client, "concrenold", "concrenkeep")
	configPath := seedConfigFile(t, m)
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"concrenold": {Enabled: true, Budget: 50, RewardIDs: []string{"rr"}},
	}

	var barrierErr error
	m.applyCommitBarrier = func(phase applyCommitPhase) {
		if phase != applyPreCommit {
			return
		}
		barrierErr = m.SetAutoRedeem("concrenkeep", config.AutoRedeemConfig{Enabled: true, Budget: 400, RewardIDs: []string{"r-keep2"}})
	}

	client.set("concrennew", "id-concrename")
	if err := m.applySettings(context.Background(), renameRuntimeStreamers(m, "concrenold", "concrennew")); err != nil {
		t.Fatalf("rename apply failed: %v", err)
	}
	if barrierErr != nil {
		t.Fatalf("SetAutoRedeem during the preCommit barrier failed: %v", barrierErr)
	}

	got := m.GetAutoRedeem("concrenkeep")
	if !got.Enabled || got.Budget != 400 {
		t.Fatalf("kept streamer's concurrent edit lost across a rename apply: %+v", got)
	}
	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if diskCfg, ok := onDisk.AutoRedeem["concrenkeep"]; !ok || diskCfg.Budget != 400 {
		t.Errorf("kept streamer's edit did not survive on disk: %+v (ok=%v)", diskCfg, ok)
	}
	if diskCfg, ok := onDisk.AutoRedeem["concrennew"]; !ok || diskCfg.Budget != 50 {
		t.Errorf("renamed streamer's entry did not migrate onto disk: %+v (ok=%v)", diskCfg, ok)
	}
	if _, stillOld := onDisk.AutoRedeem["concrenold"]; stillOld {
		t.Error("renamed streamer's old-login entry survived on disk")
	}
}

// Test 5: removal wins over a concurrent SetAutoRedeem written for the
// removed login inside the SAME preCommit window. Kills mutant 2 (re-graft
// after cleanup) and mutant 7 (preserve state after deletion).
func TestSetAutoRedeem_RemovalWinsOverConcurrentEditOnRemovedLogin(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "concvictim3", "conckeep3")
	configPath := seedConfigFile(t, m)

	var barrierErr error
	m.applyCommitBarrier = func(phase applyCommitPhase) {
		if phase != applyPreCommit {
			return
		}
		// A naive "copy the live map AFTER applying removals" ordering
		// (mutant 2) would let this survive the commit.
		barrierErr = m.SetAutoRedeem("concvictim3", config.AutoRedeemConfig{Enabled: true, Budget: 999, RewardIDs: []string{"sneaky"}})
	}

	rs := removeStreamer(m.GetRuntimeSettings(), "concvictim3")
	if err := m.applySettings(context.Background(), rs); err != nil {
		t.Fatalf("apply failed: %v", err)
	}
	if barrierErr != nil {
		t.Fatalf("SetAutoRedeem during the preCommit barrier failed: %v", barrierErr)
	}

	if _, ok := m.config.AutoRedeem["concvictim3"]; ok {
		t.Error("removed streamer's AutoRedeem entry survived a concurrent write during the removal commit")
	}
	if _, ok := m.autoRedeemState["concvictim3"]; ok {
		t.Error("removed streamer's runtime state survived the removal")
	}
	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if _, ok := onDisk.AutoRedeem["concvictim3"]; ok {
		t.Error("removed streamer's AutoRedeem entry survived on disk despite the removal")
	}
}

// Test 6: the post-commit resurrection window is closed — [R2].
func TestSetAutoRedeem_PostCommitResurrectionWindowClosed(t *testing.T) {
	t.Run("removed_streamer_refused", func(t *testing.T) {
		m, _, _ := newCapabilityMiner(t, "postremoveme", "postkeep")
		configPath := seedConfigFile(t, m)
		m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
			"postremoveme": {Enabled: true, Budget: 10, RewardIDs: []string{"r"}},
		}

		var postErr error
		var postCalled bool
		m.applyCommitBarrier = func(phase applyCommitPhase) {
			if phase != applyPostCommit {
				return
			}
			postCalled = true
			// Kills mutant 11 (bypass the config-presence check): without
			// it this call would succeed, since the runtime roster still
			// lists postremoveme (CommitPlan has not run yet in this
			// window) and only the config-presence check can refuse it.
			postErr = m.SetAutoRedeem("postremoveme", config.AutoRedeemConfig{Enabled: true, Budget: 777, RewardIDs: []string{"resurrect"}})
		}

		rs := removeStreamer(m.GetRuntimeSettings(), "postremoveme")
		if err := m.applySettings(context.Background(), rs); err != nil {
			t.Fatalf("apply failed: %v", err)
		}
		if !postCalled {
			t.Fatal("postCommit barrier never fired")
		}
		if postErr == nil {
			t.Fatal("SetAutoRedeem on a just-removed streamer in the post-commit window must be refused")
		}

		if _, ok := m.config.AutoRedeem["postremoveme"]; ok {
			t.Error("resurrected AutoRedeem entry present in memory")
		}
		onDisk, err := config.LoadConfig(configPath)
		if err != nil {
			t.Fatalf("reload config: %v", err)
		}
		if _, ok := onDisk.AutoRedeem["postremoveme"]; ok {
			t.Error("resurrected AutoRedeem entry present on disk")
		}
	})

	t.Run("renamed_old_login_refused", func(t *testing.T) {
		client := newRenameCapableAPI()
		client.set("postrenold", "id-postrename")
		m, _, _ := newRenameTestMiner(t, client, "postrenold")
		seedConfigFile(t, m)

		var postErr error
		var postCalled bool
		m.applyCommitBarrier = func(phase applyCommitPhase) {
			if phase != applyPostCommit {
				return
			}
			postCalled = true
			postErr = m.SetAutoRedeem("postrenold", config.AutoRedeemConfig{Enabled: true, Budget: 555, RewardIDs: []string{"deadkey"}})
		}

		client.set("postrennew", "id-postrename")
		if err := m.applySettings(context.Background(), renameRuntimeStreamers(m, "postrenold", "postrennew")); err != nil {
			t.Fatalf("rename apply failed: %v", err)
		}
		if !postCalled {
			t.Fatal("postCommit barrier never fired")
		}
		if postErr == nil {
			t.Fatal("SetAutoRedeem on the OLD login of a just-renamed streamer must be refused")
		}
		if _, ok := m.config.AutoRedeem["postrenold"]; ok {
			t.Error("a dead old-login AutoRedeem key was written")
		}
	})

	t.Run("unrelated_kept_streamer_succeeds_and_survives", func(t *testing.T) {
		m, _, _ := newCapabilityMiner(t, "postunrelated", "postremoveme2")
		configPath := seedConfigFile(t, m)

		var postErr error
		m.applyCommitBarrier = func(phase applyCommitPhase) {
			if phase != applyPostCommit {
				return
			}
			postErr = m.SetAutoRedeem("postunrelated", config.AutoRedeemConfig{Enabled: true, Budget: 321, RewardIDs: []string{"fine"}})
		}

		rs := removeStreamer(m.GetRuntimeSettings(), "postremoveme2")
		if err := m.applySettings(context.Background(), rs); err != nil {
			t.Fatalf("apply failed: %v", err)
		}
		if postErr != nil {
			t.Fatalf("SetAutoRedeem for an unrelated kept streamer must succeed in the post-commit window: %v", postErr)
		}
		got := m.GetAutoRedeem("postunrelated")
		if !got.Enabled || got.Budget != 321 {
			t.Fatalf("edit lost: %+v", got)
		}
		onDisk, err := config.LoadConfig(configPath)
		if err != nil {
			t.Fatalf("reload config: %v", err)
		}
		if diskCfg, ok := onDisk.AutoRedeem["postunrelated"]; !ok || diskCfg.Budget != 321 {
			t.Errorf("edit did not survive on disk: %+v (ok=%v)", diskCfg, ok)
		}
	})
}

// Test 7: a successful removal deletes consent and runtime state, and bumps
// the generation so a pre-removal evaluator cannot record.
func TestApplySettings_RemovalDeletesAutoRedeemConsentAndState(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "delvictim", "delkeep")
	seedConfigFile(t, m)
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"delvictim": {Enabled: true, Budget: 100, RewardIDs: []string{"r"}},
	}
	if err := config.SaveConfig(m.configPath, m.config); err != nil {
		t.Fatalf("reseed config: %v", err)
	}
	m.autoRedeemState["delvictim"] = &autoRedeemRuntime{spent: 40, redeemed: map[string]bool{"r": true}}
	preGen := m.autoRedeemGen["delvictim"]

	rs := removeStreamer(m.GetRuntimeSettings(), "delvictim")
	if err := m.applySettings(context.Background(), rs); err != nil {
		t.Fatalf("apply failed: %v", err)
	}

	if _, ok := m.config.AutoRedeem["delvictim"]; ok {
		t.Error("config entry survived the removal")
	}
	if _, ok := m.autoRedeemState["delvictim"]; ok {
		t.Error("runtime state survived the removal")
	}
	if got := m.autoRedeemGen["delvictim"]; got == preGen {
		t.Errorf("generation not bumped by the removal: got %d, want > %d", got, preGen)
	}
	if _, recorded := m.recordAutoRedeemed("delvictim", "r", 10, preGen); recorded {
		t.Error("a pre-removal (stale) generation must not be able to record spend after the removal")
	}
}

// Test 8: a failed removal admission changes nothing about AutoRedeem (I3).
func TestSetAutoRedeem_FailedRemovalAdmissionChangesNothing(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "admfailAR", "admfailARkeep")
	dbPath := filepath.Join(t.TempDir(), "miner.db")
	db := openRawMinerDB(t, dbPath)
	wireRawDeletionStores(t, m, db)

	configPath := seedConfigFile(t, m)
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"admfailAR": {Enabled: true, Budget: 50, RewardIDs: []string{"r"}},
	}
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("reseed config: %v", err)
	}
	m.autoRedeemState["admfailAR"] = &autoRedeemRuntime{spent: 5, redeemed: map[string]bool{}}
	preGen := m.autoRedeemGen["admfailAR"]

	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	rs := removeStreamer(m.GetRuntimeSettings(), "admfailAR")
	if err := m.applySettings(context.Background(), rs); err == nil {
		t.Fatal("expected the apply to fail with the database closed")
	}

	if _, ok := m.config.AutoRedeem["admfailAR"]; !ok {
		t.Error("AutoRedeem config entry was removed despite the admission failing")
	}
	if _, ok := m.autoRedeemState["admfailAR"]; !ok {
		t.Error("runtime state was removed despite the admission failing")
	}
	if got := m.autoRedeemGen["admfailAR"]; got != preGen {
		t.Errorf("generation changed despite the admission failing: got %d, want %d", got, preGen)
	}
	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if _, ok := onDisk.AutoRedeem["admfailAR"]; !ok {
		t.Error("disk AutoRedeem entry was removed despite the admission failing")
	}
}

// Test 9: batch removal cleans every committed entry atomically; the
// unrelated kept entry is fully preserved. Kills mutant 3 (skip one removal
// in the batch loop).
func TestApplySettings_BatchRemovalCleansAllCommittedAutoRedeemEntries(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "batchv1", "batchv2", "batchkeep")
	configPath := seedConfigFile(t, m)
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"batchv1":   {Enabled: true, Budget: 10, RewardIDs: []string{"r1"}},
		"batchv2":   {Enabled: true, Budget: 20, RewardIDs: []string{"r2"}},
		"batchkeep": {Enabled: true, Budget: 30, RewardIDs: []string{"r3"}},
	}
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("reseed config: %v", err)
	}
	m.autoRedeemState["batchv1"] = &autoRedeemRuntime{spent: 1, redeemed: map[string]bool{}}
	m.autoRedeemState["batchv2"] = &autoRedeemRuntime{spent: 2, redeemed: map[string]bool{}}
	m.autoRedeemState["batchkeep"] = &autoRedeemRuntime{spent: 3, redeemed: map[string]bool{}}

	rs := m.GetRuntimeSettings()
	var kept []settings.StreamerConfig
	for _, sc := range rs.Streamers {
		if sc.Username == "batchkeep" {
			kept = append(kept, sc)
		}
	}
	rs.Streamers = kept

	if err := m.applySettings(context.Background(), rs); err != nil {
		t.Fatalf("batch removal apply failed: %v", err)
	}

	for _, victim := range []string{"batchv1", "batchv2"} {
		if _, ok := m.config.AutoRedeem[victim]; ok {
			t.Errorf("%s: config entry survived batch removal", victim)
		}
		if _, ok := m.autoRedeemState[victim]; ok {
			t.Errorf("%s: runtime state survived batch removal", victim)
		}
	}
	keptCfg, ok := m.config.AutoRedeem["batchkeep"]
	if !ok || keptCfg.Budget != 30 {
		t.Errorf("unrelated streamer's AutoRedeem entry corrupted: %+v (ok=%v)", keptCfg, ok)
	}
	if rt := m.autoRedeemState["batchkeep"]; rt == nil || rt.spent != 3 {
		t.Errorf("unrelated streamer's runtime state corrupted: %+v", rt)
	}
	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if len(onDisk.AutoRedeem) != 1 {
		t.Errorf("disk AutoRedeem has %d entries, want 1: %+v", len(onDisk.AutoRedeem), onDisk.AutoRedeem)
	}
}

// Test 10: re-adding a removed login does not reactivate its AutoRedeem
// consent, and evaluateAutoRedeem performs no client calls for it.
func TestApplySettings_ReAddSameLoginDoesNotReactivateAutoRedeem(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "readdvictim", "readdkeep")
	seedConfigFile(t, m)
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"readdvictim": {Enabled: true, Budget: 10, RewardIDs: []string{"r"}},
	}
	if err := config.SaveConfig(m.configPath, m.config); err != nil {
		t.Fatalf("reseed config: %v", err)
	}

	rs := removeStreamer(m.GetRuntimeSettings(), "readdvictim")
	if err := m.applySettings(context.Background(), rs); err != nil {
		t.Fatalf("removal apply failed: %v", err)
	}

	rs2 := m.GetRuntimeSettings()
	rs2.Streamers = append(rs2.Streamers, settings.StreamerConfig{Username: "readdvictim"})
	if err := m.applySettings(context.Background(), rs2); err != nil {
		t.Fatalf("re-add apply failed: %v", err)
	}

	if _, ok := m.config.AutoRedeem["readdvictim"]; ok {
		t.Error("re-added streamer's AutoRedeem consent was reactivated")
	}
	if _, ok := m.autoRedeemState["readdvictim"]; ok {
		t.Error("re-added streamer has stale runtime state")
	}

	fake := &fakeRewardsClient{}
	m.rewardsAPI = fake
	s := m.streamers.Get("readdvictim")
	if s == nil {
		t.Fatal("re-added streamer missing from the runtime")
	}
	m.evaluateAutoRedeem(s)
	if got := fake.getCallCount(); got != 0 {
		t.Errorf("evaluateAutoRedeem made %d client call(s) for a streamer with no AutoRedeem config, want 0", got)
	}
}

// Test 16: destination-wins clash semantics hold even when the clashing
// destination entry was written DURING the preCommit window, and the clash
// bumps the destination's generation.
func TestApplySettings_RenameClashDuringPreCommitWindow_DestinationWins(t *testing.T) {
	client := newRenameCapableAPI()
	client.set("clashold", "id-clash")
	m, _, _ := newRenameTestMiner(t, client, "clashold")
	seedConfigFile(t, m)
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"clashold": {Enabled: true, Budget: 10, RewardIDs: []string{"r-old"}},
	}
	if err := config.SaveConfig(m.configPath, m.config); err != nil {
		t.Fatalf("reseed config: %v", err)
	}

	m.applyCommitBarrier = func(phase applyCommitPhase) {
		if phase != applyPreCommit {
			return
		}
		// The destination login gets its OWN independently-configured entry
		// WHILE this apply is still doing durable I/O off the lock — a
		// mutation direct on the live map under m.mu, simulating a write
		// that landed via some other path. The commit-point refresh must
		// still see it (I1) and destination-wins must apply against IT, not
		// against whatever candidate.AutoRedeem happened to snapshot
		// earlier.
		m.mu.Lock()
		m.config.AutoRedeem["clashnew"] = config.AutoRedeemConfig{Enabled: true, Budget: 20, RewardIDs: []string{"r-new"}}
		m.mu.Unlock()
	}

	client.set("clashnew", "id-clash")
	if err := m.applySettings(context.Background(), renameRuntimeStreamers(m, "clashold", "clashnew")); err != nil {
		t.Fatalf("rename apply failed: %v", err)
	}

	got, ok := m.config.AutoRedeem["clashnew"]
	if !ok || got.Budget != 20 || len(got.RewardIDs) != 1 || got.RewardIDs[0] != "r-new" {
		t.Errorf("destination-wins semantics violated: %+v (ok=%v)", got, ok)
	}
	if _, stillOld := m.config.AutoRedeem["clashold"]; stillOld {
		t.Error("old-login entry survived a clashing rename")
	}
	if gen := m.autoRedeemGen["clashnew"]; gen == 0 {
		t.Errorf("the clash branch must bump clashnew's generation, got %d", gen)
	}
}

// Test 17: rename-into-a-removed-login defence in depth. The ApplySettings
// route is structurally unreachable (the planner's login-collision rule
// refuses such a rename), so this drives refreshCandidateAutoRedeemLocked
// directly, as the design mandates.
func TestRefreshCandidateAutoRedeemLocked_RenameIntoRemovedLogin_DefenceInDepth(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "alpha")
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"alpha": {Enabled: true, Budget: 10, RewardIDs: []string{"r"}},
	}
	candidate := &config.Config{}

	m.mu.Lock()
	clashes := m.refreshCandidateAutoRedeemLocked(candidate, []streamer.RenameEvent{
		{OldLogin: "alpha", NewLogin: "beta", ChannelID: "id"},
	}, []string{"beta"})
	m.mu.Unlock()

	if clashes == nil {
		t.Error("refreshCandidateAutoRedeemLocked must always return a non-nil map [RR1]")
	}
	if _, ok := candidate.AutoRedeem["beta"]; ok {
		t.Error("a rename INTO a removed login must not leave the migrated entry behind (copy -> migrate -> delete ordering)")
	}
	if _, ok := candidate.AutoRedeem["alpha"]; ok {
		t.Error("the old-login entry must be gone after the migration")
	}
}

// Test 18: a rename SaveConfig failure leaves AutoRedeem/state/gen fully
// untouched, extending the C2B pattern. Kills mutant 8 (publish before
// SaveConfig).
func TestApplySettingsWithRename_SaveConfigFailure_AutoRedeemStateGenUntouched(t *testing.T) {
	svc := newRealAnalytics(t)
	client := newRenameCapableAPI()
	client.set("c18old", "id-c18")
	m, _, _ := newRenameTestMiner(t, client, "c18old")
	m.analyticsSvc = svc

	configPath := seedConfigFile(t, m)
	m.config.AutoRedeem = map[string]config.AutoRedeemConfig{
		"c18old": {Enabled: true, Budget: 40, RewardIDs: []string{"r18"}},
	}
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("reseed config: %v", err)
	}
	m.autoRedeemState["c18old"] = &autoRedeemRuntime{spent: 15, redeemed: map[string]bool{}}
	preGen := m.autoRedeemGen["c18old"]

	breakConfigPathForNextSave(t, configPath)

	client.set("c18new", "id-c18")
	if err := m.applySettings(context.Background(), renameRuntimeStreamers(m, "c18old", "c18new")); err == nil {
		t.Fatal("expected the rename transaction to fail when SaveConfig fails")
	}

	if _, ok := m.config.AutoRedeem["c18new"]; ok {
		t.Error("AutoRedeem migrated to the new login despite the failed transaction")
	}
	got, ok := m.config.AutoRedeem["c18old"]
	if !ok || got.Budget != 40 {
		t.Errorf("old-login AutoRedeem entry changed despite the failed transaction: %+v (ok=%v)", got, ok)
	}
	rt := m.autoRedeemState["c18old"]
	if rt == nil || rt.spent != 15 {
		t.Errorf("runtime state changed despite the failed transaction: %+v", rt)
	}
	if _, ok := m.autoRedeemState["c18new"]; ok {
		t.Error("runtime state created under the new login despite the failed transaction")
	}
	if got := m.autoRedeemGen["c18old"]; got != preGen {
		t.Errorf("generation changed despite the failed transaction: got %d, want %d", got, preGen)
	}
}
