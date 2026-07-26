package miner

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
)

// This file covers the SRAP rename+removal interaction (M1 QA follow-up F2):
// applySettingsWithRename admits any RIDING removal durably BEFORE its own
// commitRenameTransaction commit point, and compensates that admission
// (AbortAdmission) on a rename-transaction failure — the same fail-closed
// discipline applySettingsWithRemovals gives a removal-only apply, but for
// the branch where a rename rides along in the SAME apply. Neither existing
// rename tests (rename_reconcile_test.go, cp1_c2_matrix_test.go — rename
// only) nor existing removal tests (srap_test.go — no rename) drive BOTH in
// one apply.

// renameAndRemove returns rs with oldLogin's entry renamed to newLogin (in
// place, mirroring renameRuntimeStreamers) AND removeLogin's entry dropped
// entirely — the same full-body-minus-one-plus-a-rename shape a real
// Settings page POST would carry for "rename X, delete Y" done together.
func renameAndRemove(m *Miner, oldLogin, newLogin, removeLogin string) settings.RuntimeSettings {
	rs := m.GetRuntimeSettings()
	var kept []settings.StreamerConfig
	for _, sc := range rs.Streamers {
		switch sc.Username {
		case removeLogin:
			continue // dropped: this is the removal
		case oldLogin:
			sc.Username = newLogin // renamed in place
		}
		kept = append(kept, sc)
	}
	rs.Streamers = kept
	return rs
}

// TestApplySettingsRenameWithRidingRemoval_Success covers F2(a): a single
// apply that both renames one streamer and removes another must complete
// BOTH correctly — the removal's durable admission (SRAP prepare) runs
// before commitRenameTransaction's own commit point, and its completion
// (purge) runs after, exactly as a removal-only apply's admission/commit/
// complete sequence does; the rename's own config-surgery/analytics-migration
// invariants (pinned elsewhere for rename-only applies) are unaffected by the
// removal riding along.
func TestApplySettingsRenameWithRidingRemoval_Success(t *testing.T) {
	const oldLogin, newLogin, removeLogin, keepLogin = "f2renameold", "f2renamenew", "f2removeme", "f2keep"

	client := newRenameCapableAPI()
	client.set(oldLogin, "id-f2success")
	m, _, _ := newRenameTestMiner(t, client, oldLogin, removeLogin, keepLogin)
	// A PRIVATE, non-singleton DB (not wireDeletionStores' shared package
	// handle): this test's fixture logins are fixed consts, and rename
	// mutates analytics identity (old login's history moves to new login) in
	// a way that is NOT idempotent across repeated -count iterations sharing
	// one singleton — a private db keeps every iteration's state isolated.
	db := openRawMinerDB(t, filepath.Join(t.TempDir(), "miner.db"))
	svc, _ := wireRawDeletionStores(t, m, db)

	if err := svc.Repository().RecordPoints(oldLogin, 555, "WATCH"); err != nil {
		t.Fatalf("seed old login points: %v", err)
	}
	if err := svc.Repository().RecordPoints(removeLogin, 321, "WATCH"); err != nil {
		t.Fatalf("seed removed login points: %v", err)
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	m.configPath = configPath
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config file: %v", err)
	}

	client.set(newLogin, "id-f2success") // same stable ID as oldLogin -> detected as a rename

	rs := renameAndRemove(m, oldLogin, newLogin, removeLogin)
	if err := m.applySettings(context.Background(), rs); err != nil {
		t.Fatalf("rename+removal apply failed: %v", err)
	}

	// Rename completed: old login gone, new login present with migrated history.
	if m.streamers.Get(oldLogin) != nil {
		t.Error("old login still present in the runtime after a successful rename")
	}
	if m.streamers.Get(newLogin) == nil {
		t.Error("new login missing from the runtime after a successful rename")
	}
	newData, err := svc.Repository().GetStreamerData(newLogin)
	if err != nil {
		t.Fatalf("get renamed streamer data: %v", err)
	}
	if len(newData.Series) != 1 {
		t.Errorf("analytics points under %s = %d, want 1 (history must follow the rename)", newLogin, len(newData.Series))
	}

	// Removal completed: config renamed AND the OTHER streamer removed —
	// gone from the runtime, purged from persisted history, no durable
	// ledger row left owed.
	if m.streamers.Get(removeLogin) != nil {
		t.Error("removed streamer still present in the runtime")
	}
	found := false
	for _, sc := range m.config.Streamers {
		if sc.Username == removeLogin {
			found = true
		}
	}
	if found {
		t.Error("removed streamer still present in the committed config")
	}
	if data, _ := svc.Repository().GetStreamerData(removeLogin); len(data.Series) != 0 {
		t.Error("removed streamer's persisted history survived (purge did not complete)")
	}
	if has, err := m.streamerLifecycle.HasPending(context.Background(), removeLogin); err != nil || has {
		t.Errorf("HasPending for the removed streamer = (%v, %v), want (false, nil) — purge must be fully done, not merely owed", has, err)
	}

	// Unrelated streamer untouched.
	if m.streamers.Get(keepLogin) == nil {
		t.Error("unrelated streamer lost from the runtime")
	}
}

// TestApplySettingsRenameWithRidingRemoval_RenameTxFailureCompensatesRemoval
// covers F2(b): the rename transaction's own commit point
// (commitRenameTransaction, via an unwritable configPath — the same
// deterministic seam TestApplySettingsWithRename_SaveConfigFailure_
// NothingRenamed_C2B uses) fails AFTER the riding removal's admission
// already succeeded. The whole apply must be all-or-nothing: ZERO mutation
// of runtime/config/analytics for EITHER the rename or the removal, and the
// removal's prepared admission row must be compensated (AbortAdmission) —
// not left durably admitted with no commit ever reached.
func TestApplySettingsRenameWithRidingRemoval_RenameTxFailureCompensatesRemoval(t *testing.T) {
	const oldLogin, newLogin, removeLogin, keepLogin = "f2failold", "f2failnew", "f2failremoveme", "f2failkeep"

	client := newRenameCapableAPI()
	client.set(oldLogin, "id-f2fail")
	m, _, chatRec := newRenameTestMiner(t, client, oldLogin, removeLogin, keepLogin)
	// Private, non-singleton DB — see TestApplySettingsRenameWithRidingRemoval_
	// Success's comment on why this test cannot share the package singleton.
	db := openRawMinerDB(t, filepath.Join(t.TempDir(), "miner.db"))
	svc, _ := wireRawDeletionStores(t, m, db)

	if err := svc.Repository().RecordPoints(oldLogin, 444, "WATCH"); err != nil {
		t.Fatalf("seed old login points: %v", err)
	}
	if err := svc.Repository().RecordPoints(removeLogin, 222, "WATCH"); err != nil {
		t.Fatalf("seed removed login points: %v", err)
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config file: %v", err)
	}
	m.configPath = configPath

	// Force the NEXT SaveConfig (commitRenameTransaction's own commit point)
	// to fail deterministically — the riding removal's AdmitRemovals has
	// already committed by the time this is reached.
	breakConfigPathForNextSave(t, configPath)

	client.set(newLogin, "id-f2fail")

	rs := renameAndRemove(m, oldLogin, newLogin, removeLogin)
	if err := m.applySettings(context.Background(), rs); err == nil {
		t.Fatal("expected the rename transaction to fail")
	}

	// Runtime: completely unrenamed AND the "removed" streamer still present.
	if m.streamers.Get(oldLogin) == nil {
		t.Error("runtime was renamed away from the old login despite the rename transaction failing")
	}
	if m.streamers.Get(newLogin) != nil {
		t.Error("runtime is under the new login despite the rename transaction failing")
	}
	if m.streamers.Get(removeLogin) == nil {
		t.Error("streamer removed from the runtime despite the whole apply (including the rename) failing")
	}
	if m.streamers.Get(keepLogin) == nil {
		t.Error("unrelated streamer lost from the runtime")
	}

	// In-memory config: untouched — still 3 entries, old login present,
	// removed login present.
	if len(m.config.Streamers) != 3 {
		t.Fatalf("in-memory config has %d entries, want 3 (untouched)", len(m.config.Streamers))
	}
	var haveOld, haveRemoved bool
	for _, sc := range m.config.Streamers {
		if sc.Username == oldLogin {
			haveOld = true
		}
		if sc.Username == removeLogin {
			haveRemoved = true
		}
	}
	if !haveOld || !haveRemoved {
		t.Errorf("in-memory config missing an entry that should have survived the failed apply: %+v", m.config.Streamers)
	}

	// Disk: configPath is still the untouched directory this test installed.
	info, statErr := os.Stat(configPath)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("configPath must still be the untouched directory: stat=%v, err=%v", info, statErr)
	}

	// Analytics: neither the rename nor the purge ever happened.
	oldData, err := svc.Repository().GetStreamerData(oldLogin)
	if err != nil {
		t.Fatalf("get old login data: %v", err)
	}
	if len(oldData.Series) != 1 {
		t.Errorf("old login's analytics history = %d points, want 1 (rename must not have happened)", len(oldData.Series))
	}
	newData, err := svc.Repository().GetStreamerData(newLogin)
	if err != nil {
		t.Fatalf("get new login data: %v", err)
	}
	if len(newData.Series) != 0 {
		t.Errorf("new login has %d analytics points, want 0 (rename must not have committed)", len(newData.Series))
	}
	removedData, err := svc.Repository().GetStreamerData(removeLogin)
	if err != nil {
		t.Fatalf("get removed-login data: %v", err)
	}
	if len(removedData.Series) != 1 {
		t.Errorf("removed streamer's analytics history = %d points, want 1 (purge must never have run)", len(removedData.Series))
	}

	// The riding removal's admission was compensated: ZERO rows survive in
	// the SRAP prepare-phase ledger.
	var admissions int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM streamer_deletion_admissions`).Scan(&admissions); err != nil {
		t.Fatalf("count admissions: %v", err)
	}
	if admissions != 0 {
		t.Errorf("admissions=%d, want 0 (the riding removal's admission must be compensated when the rename transaction fails)", admissions)
	}
	if has, err := m.streamerLifecycle.HasPending(context.Background(), removeLogin); err != nil || has {
		t.Errorf("HasPending for the would-be-removed streamer = (%v, %v), want (false, nil)", has, err)
	}

	// No IRC action for a transaction that never committed.
	if got := chatRec.leaveCount(oldLogin); got != 0 {
		t.Errorf("chat left %s %d times despite the failed transaction, want 0", oldLogin, got)
	}
}

// admissionRowObservingFencer wraps a real Fencer and, the moment Tombstone
// fires for the login this test cares about, synchronously records whether a
// row for it already existed in the SRAP prepare-phase ledger
// (streamer_deletion_admissions) — queried directly against the SAME db
// handle, no channel/goroutine needed, since Tombstone runs synchronously on
// the test's own goroutine inside applySettings, well before the coordinator
// ever gets a chance to move or clear that row. Tombstone is CommitRemoval's
// very first action (see lifecycle.go), called only after
// commitRenameTransaction has already committed and only BEFORE
// movePendingTx moves the row out of the admissions table — so this is
// exactly the window between "the rename transaction's commit" and
// "CommitRemoval's completion" the durability claim needs pinned.
type admissionRowObservingFencer struct {
	inner    streamerlifecycle.Fencer
	db       *database.DB
	login    string
	observed *bool
}

func (f admissionRowObservingFencer) Tombstone(login string) {
	if login == f.login {
		var n int
		if err := f.db.QueryRow(`SELECT COUNT(*) FROM streamer_deletion_admissions WHERE login = ?`, login).Scan(&n); err == nil && n > 0 {
			*f.observed = true
		}
	}
	f.inner.Tombstone(login)
}

func (f admissionRowObservingFencer) Reinstate(login string) { f.inner.Reinstate(login) }

// TestApplySettingsRenameWithRidingRemoval_AdmissionRowExistsBetweenRenameCommitAndPurge
// closes MUT-X5b: disabling applySettingsWithRename's entire AdmitRemovals
// block survives on end-state assertions alone, because movePendingTx
// UPSERTs the pending-purge row unconditionally regardless of whether an
// admissions row ever existed — TestApplySettingsRenameWithRidingRemoval_
// Success's post-apply state (removed, purged, no owed row) is IDENTICAL
// whether or not the removal was ever durably admitted before the rename's
// own commit point. This test instead observes the DURABLE LEDGER'S EXISTENCE
// synchronously, at the exact instant between commitRenameTransaction's
// commit and CommitRemoval's own completion (via admissionRowObservingFencer,
// hooked into Tombstone — CommitRemoval's first action): a streamer_deletion_
// admissions row for the removed login MUST already exist there, proving
// AdmitRemovals genuinely ran (and committed) before the rename's commit
// point, not merely that SOME row exists by the time the apply returns.
func TestApplySettingsRenameWithRidingRemoval_AdmissionRowExistsBetweenRenameCommitAndPurge(t *testing.T) {
	const oldLogin, newLogin, removeLogin, keepLogin = "f2existold", "f2existnew", "f2existremoveme", "f2existkeep"

	client := newRenameCapableAPI()
	client.set(oldLogin, "id-f2exist")
	m, _, _ := newRenameTestMiner(t, client, oldLogin, removeLogin, keepLogin)

	db := openRawMinerDB(t, filepath.Join(t.TempDir(), "miner.db"))
	an, err := analytics.NewSQLiteRepository(db, t.TempDir())
	if err != nil {
		t.Fatalf("analytics repo: %v", err)
	}

	var observedAdmissionRow bool
	coord, err := streamerlifecycle.New(db,
		[]streamerlifecycle.Purger{an},
		[]streamerlifecycle.Fencer{admissionRowObservingFencer{inner: an, db: db, login: removeLogin, observed: &observedAdmissionRow}},
		[]streamerlifecycle.Renamer{an},
	)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	m.db = db
	m.streamerLifecycle = coord

	if err := an.RecordPoints(oldLogin, 100, "WATCH"); err != nil {
		t.Fatalf("seed old login points: %v", err)
	}
	if err := an.RecordPoints(removeLogin, 50, "WATCH"); err != nil {
		t.Fatalf("seed removed login points: %v", err)
	}

	configPath := filepath.Join(t.TempDir(), "config.json")
	m.configPath = configPath
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config file: %v", err)
	}

	client.set(newLogin, "id-f2exist") // same stable ID as oldLogin -> detected as a rename

	rs := renameAndRemove(m, oldLogin, newLogin, removeLogin)
	if err := m.applySettings(context.Background(), rs); err != nil {
		t.Fatalf("rename+removal apply failed: %v", err)
	}

	if !observedAdmissionRow {
		t.Fatal("no streamer_deletion_admissions row existed for the removed login between the rename's commit and CommitRemoval's completion — the removal was never durably admitted before the rename committed")
	}

	// Sanity: the apply still reaches its normal fully-completed end state.
	if m.streamers.Get(removeLogin) != nil {
		t.Error("removed streamer still present in the runtime")
	}
	if has, err := coord.HasPending(context.Background(), removeLogin); err != nil || has {
		t.Errorf("HasPending for the removed streamer = (%v, %v), want (false, nil)", has, err)
	}
	if m.streamers.Get(newLogin) == nil {
		t.Error("new login missing from the runtime after a successful rename")
	}
	if m.streamers.Get(keepLogin) == nil {
		t.Error("unrelated streamer lost from the runtime")
	}
}
