package updater

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"

	_ "modernc.org/sqlite"
)

// openUpdaterTestDB opens an independent SQLite file directly (bypassing
// database.Open's process-wide singleton - the same convention
// internal/lifecycle's store_test.go uses), so each test gets its own
// isolated database.
func openUpdaterTestDB(t *testing.T) *database.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "miner.db")
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return &database.DB{DB: sqlDB}
}

// NewStore registers the miner_updater module and records exactly its own
// schema_versions row, leaving an unrelated module's row untouched (the
// per-module migration convention internal/database.RegisterModule exists
// for).
func TestStoreRegistersModuleWithoutTouchingOthers(t *testing.T) {
	db := openUpdaterTestDB(t)

	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_versions (
		module TEXT PRIMARY KEY, version INTEGER NOT NULL DEFAULT 0, updated_at INTEGER NOT NULL)`); err != nil {
		t.Fatalf("seed schema_versions: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO schema_versions (module, version, updated_at) VALUES ('other_module', 7, strftime('%s','now'))`); err != nil {
		t.Fatalf("seed other module row: %v", err)
	}

	if _, err := NewStore(db); err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	var version int
	if err := db.QueryRow(`SELECT version FROM schema_versions WHERE module = ?`, storeModuleName).Scan(&version); err != nil {
		t.Fatalf("query miner_updater version: %v", err)
	}
	if version != 1 {
		t.Fatalf("miner_updater schema version = %d, want 1", version)
	}

	var otherVersion int
	if err := db.QueryRow(`SELECT version FROM schema_versions WHERE module = 'other_module'`).Scan(&otherVersion); err != nil {
		t.Fatalf("query other_module version: %v", err)
	}
	if otherVersion != 7 {
		t.Fatalf("other_module version = %d, want unchanged 7 (NewStore must not touch another module's row)", otherVersion)
	}
}

// RecordApplying UPSERTs id=1 with phase 'applying', and a second call
// overwrites every column of a stale row (the belt-and-braces
// terminalization the spec calls for) rather than merging or duplicating.
func TestStoreRecordApplyingUpsertsAndOverwritesStaleRow(t *testing.T) {
	db := openUpdaterTestDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.RecordApplying(context.Background(), "1.0.0", "1.1.0", "https://example.test/r1"); err != nil {
		t.Fatalf("RecordApplying #1: %v", err)
	}
	if err := store.RecordApplying(context.Background(), "1.1.0", "1.2.0", "https://example.test/r2"); err != nil {
		t.Fatalf("RecordApplying #2 (stale-row overwrite): %v", err)
	}

	var from, to, phase, url string
	row := db.QueryRow(`SELECT from_version, to_version, phase, release_url FROM updater_apply_handoff WHERE id = 1`)
	if err := row.Scan(&from, &to, &phase, &url); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if from != "1.1.0" || to != "1.2.0" || phase != HandoffApplying || url != "https://example.test/r2" {
		t.Fatalf("row = (%q,%q,%q,%q), want (1.1.0,1.2.0,applying,https://example.test/r2)", from, to, phase, url)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM updater_apply_handoff`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1 (upsert on id=1, never a second row)", count)
	}
}

// RecordApplied upgrades the phase column to 'applied' while preserving the
// release_url a prior RecordApplying wrote (the ON CONFLICT clause
// deliberately omits release_url from SET so it keeps its existing value).
func TestStoreRecordAppliedUpgradesPhasePreservingReleaseURL(t *testing.T) {
	db := openUpdaterTestDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.RecordApplying(context.Background(), "1.0.0", "1.1.0", "https://example.test/r1"); err != nil {
		t.Fatalf("RecordApplying: %v", err)
	}
	if err := store.RecordApplied(context.Background(), "1.0.0", "1.1.0"); err != nil {
		t.Fatalf("RecordApplied: %v", err)
	}

	var phase, url string
	row := db.QueryRow(`SELECT phase, release_url FROM updater_apply_handoff WHERE id = 1`)
	if err := row.Scan(&phase, &url); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if phase != HandoffApplied {
		t.Fatalf("phase = %q, want %q", phase, HandoffApplied)
	}
	if url != "https://example.test/r1" {
		t.Fatalf("release_url = %q, want preserved from RecordApplying", url)
	}
}

// Clear deletes the single row, and is a no-op success (not an error) both
// against an empty (never-written) table and a missing table entirely.
func TestStoreClearDeletesAndIsNoOpWhenAbsent(t *testing.T) {
	db := openUpdaterTestDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.Clear(context.Background()); err != nil {
		t.Fatalf("Clear on empty table: %v", err)
	}

	if err := store.RecordApplying(context.Background(), "1.0.0", "1.1.0", ""); err != nil {
		t.Fatalf("RecordApplying: %v", err)
	}
	if err := store.Clear(context.Background()); err != nil {
		t.Fatalf("Clear: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM updater_apply_handoff`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("row count after Clear = %d, want 0", count)
	}

	// Second Clear on an already-empty table is still a success.
	if err := store.Clear(context.Background()); err != nil {
		t.Fatalf("second Clear: %v", err)
	}
}

func TestStoreClearToleratesMissingTable(t *testing.T) {
	db := openUpdaterTestDB(t)
	store := &Store{db: db} // deliberately skip NewStore/RegisterModule
	if err := store.Clear(context.Background()); err != nil {
		t.Fatalf("Clear on missing table: %v", err)
	}
}

// ConsumeHandoff reads-and-deletes the row in one transaction: the first
// call returns it (found=true) and the second finds nothing left to consume.
func TestStoreConsumeHandoffReturnsAndDeletesExactlyOnce(t *testing.T) {
	db := openUpdaterTestDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	if err := store.RecordApplying(context.Background(), "1.0.0", "1.1.0", "https://example.test/r"); err != nil {
		t.Fatalf("RecordApplying: %v", err)
	}

	rec, found, err := store.ConsumeHandoff(context.Background())
	if err != nil {
		t.Fatalf("ConsumeHandoff: %v", err)
	}
	if !found {
		t.Fatal("expected found=true")
	}
	if rec.FromVersion != "1.0.0" || rec.ToVersion != "1.1.0" || rec.Phase != HandoffApplying || rec.ReleaseURL != "https://example.test/r" {
		t.Fatalf("rec = %+v, unexpected", rec)
	}
	if rec.UpdatedAt.IsZero() {
		t.Fatal("UpdatedAt not populated")
	}

	rec2, found2, err := store.ConsumeHandoff(context.Background())
	if err != nil {
		t.Fatalf("second ConsumeHandoff: %v", err)
	}
	if found2 {
		t.Fatalf("second ConsumeHandoff found a row that should have already been deleted: %+v", rec2)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM updater_apply_handoff`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 0 {
		t.Fatalf("row count after ConsumeHandoff = %d, want 0", count)
	}
}

func TestStoreConsumeHandoffMissingTableReportsNotFound(t *testing.T) {
	db := openUpdaterTestDB(t)
	store := &Store{db: db} // deliberately skip NewStore/RegisterModule
	_, found, err := store.ConsumeHandoff(context.Background())
	if err != nil {
		t.Fatalf("ConsumeHandoff on missing table: %v", err)
	}
	if found {
		t.Fatal("expected found=false for a missing table")
	}
}

// A write with an already-cancelled PARENT context must still land: every
// write derives its own bounded ctx via context.WithoutCancel(ctx), since on
// every concurrent-exit path the updater's own ctx is already cancelled by
// the time the post-swap tail (RecordApplied/Clear on failure) runs, while
// the database handle itself is still open.
func TestStoreWriteSucceedsWithAlreadyCancelledParentContext(t *testing.T) {
	db := openUpdaterTestDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call

	if err := store.RecordApplying(ctx, "1.0.0", "1.1.0", "https://example.test/r"); err != nil {
		t.Fatalf("RecordApplying with an already-cancelled parent ctx: %v", err)
	}

	var count int
	if err := db.QueryRow(`SELECT count(*) FROM updater_apply_handoff`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("row count = %d, want 1 (write must land despite a cancelled parent ctx)", count)
	}
}

// After the underlying database has been closed, every write method returns
// a typed database.ErrClosed rather than hanging or panicking.
func TestStoreMethodsReturnErrClosedAfterDBClose(t *testing.T) {
	db := openUpdaterTestDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	if err := store.RecordApplying(context.Background(), "a", "b", ""); !errors.Is(err, database.ErrClosed) {
		t.Errorf("RecordApplying after Close = %v, want errors.Is(err, database.ErrClosed)", err)
	}
	if err := store.RecordApplied(context.Background(), "a", "b"); !errors.Is(err, database.ErrClosed) {
		t.Errorf("RecordApplied after Close = %v, want errors.Is(err, database.ErrClosed)", err)
	}
	if err := store.Clear(context.Background()); !errors.Is(err, database.ErrClosed) {
		t.Errorf("Clear after Close = %v, want errors.Is(err, database.ErrClosed)", err)
	}
	if _, _, err := store.ConsumeHandoff(context.Background()); !errors.Is(err, database.ErrClosed) {
		t.Errorf("ConsumeHandoff after Close = %v, want errors.Is(err, database.ErrClosed)", err)
	}
}

// ClassifyBoot: success on BOTH phases (the completed swap is ground truth,
// the phase upgrade to 'applied' is best-effort cosmetics), interrupted,
// not-effective, and anomalous.
func TestClassifyBoot(t *testing.T) {
	tests := []struct {
		name    string
		rec     HandoffRecord
		running string
		want    BootOutcome
	}{
		{
			name:    "succeeded phase applying (crashed after swap, before RecordApplied)",
			rec:     HandoffRecord{FromVersion: "1.0.0", ToVersion: "1.1.0", Phase: HandoffApplying},
			running: "1.1.0",
			want:    BootSucceeded,
		},
		{
			name:    "succeeded phase applied (ordinary happy path, v-prefix vs bare)",
			rec:     HandoffRecord{FromVersion: "1.0.0", ToVersion: "1.1.0", Phase: HandoffApplied},
			running: "v1.1.0",
			want:    BootSucceeded,
		},
		{
			name:    "interrupted mid-apply (crashed before the swap ever completed)",
			rec:     HandoffRecord{FromVersion: "1.0.0", ToVersion: "1.1.0", Phase: HandoffApplying},
			running: "1.0.0",
			want:    BootInterrupted,
		},
		{
			name:    "not effective (swap reported success but running binary is unchanged)",
			rec:     HandoffRecord{FromVersion: "1.0.0", ToVersion: "1.1.0", Phase: HandoffApplied},
			running: "1.0.0",
			want:    BootNotEffective,
		},
		{
			name:    "anomalous (running neither from nor to)",
			rec:     HandoffRecord{FromVersion: "1.0.0", ToVersion: "1.1.0", Phase: HandoffApplying},
			running: "2.0.0",
			want:    BootAnomalous,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyBoot(tt.rec, tt.running); got != tt.want {
				t.Errorf("ClassifyBoot(%+v, %q) = %q, want %q", tt.rec, tt.running, got, tt.want)
			}
		})
	}
}
