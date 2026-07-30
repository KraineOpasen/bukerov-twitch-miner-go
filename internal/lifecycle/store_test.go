package lifecycle

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"

	_ "modernc.org/sqlite"
)

// openTestDB opens an independent SQLite file directly (bypassing
// database.Open's process-wide singleton — the same convention
// internal/watcher's rotation_test.go and internal/streamerlifecycle's
// tests use), so each test gets its own isolated database.
func openTestDB(t *testing.T) *database.DB {
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

// (16, unit) A fresh database with the module never registered (no table
// at all) reports Found=false, nil error — the caller (reconciliation)
// supplies the running back-compat default.
func TestStoreLoadMissingTableReportsNotFound(t *testing.T) {
	db := openTestDB(t)
	store := &Store{db: db} // deliberately skip NewStore/RegisterModule

	res, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load on a missing table: %v", err)
	}
	if res.Found {
		t.Fatal("expected Found=false for a missing table")
	}
}

// Load on a registered-but-empty table also reports Found=false.
func TestStoreLoadEmptyTableReportsNotFound(t *testing.T) {
	db := openTestDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	res, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load on an empty table: %v", err)
	}
	if res.Found {
		t.Fatal("expected Found=false for an empty table")
	}
}

// Save/Load round-trips desired exactly, and updated_at is stored as epoch
// seconds (design v6 §8, matching the schema_versions convention).
func TestStoreSaveLoadRoundTrip(t *testing.T) {
	db := openTestDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	before := time.Now().Unix()
	if err := store.Save(context.Background(), DesiredPaused, "user", "cmd-1"); err != nil {
		t.Fatalf("Save: %v", err)
	}
	after := time.Now().Unix()

	res, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !res.Found || res.Desired != DesiredPaused {
		t.Fatalf("Load = %+v, want Found=true Desired=paused", res)
	}

	var reason, cmdID string
	var updatedAt int64
	row := db.QueryRow(`SELECT reason, command_id, updated_at FROM miner_lifecycle_state WHERE id = 1`)
	if err := row.Scan(&reason, &cmdID, &updatedAt); err != nil {
		t.Fatalf("scan raw row: %v", err)
	}
	if reason != "user" || cmdID != "cmd-1" {
		t.Fatalf("reason/command_id = %q/%q, want user/cmd-1", reason, cmdID)
	}
	if updatedAt < before || updatedAt > after {
		t.Fatalf("updated_at = %d, want between %d and %d (epoch seconds)", updatedAt, before, after)
	}

	// A second Save upserts (overwrites) the single row, id=1 stays unique.
	if err := store.Save(context.Background(), DesiredStopped, "signal", "cmd-2"); err != nil {
		t.Fatalf("second Save: %v", err)
	}
	res2, err := store.Load(context.Background())
	if err != nil {
		t.Fatalf("Load after second Save: %v", err)
	}
	if res2.Desired != DesiredStopped {
		t.Fatalf("Load after second Save = %+v, want stopped", res2)
	}
	var rowCount int
	if err := db.QueryRow(`SELECT count(*) FROM miner_lifecycle_state`).Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("expected exactly 1 row (upsert on id=1), got %d", rowCount)
	}
}

// The DDL's CHECK constraint rejects a desired value outside {running,
// paused, stopped} on direct insert (Store.Save can never produce one; this
// proves the constraint itself is in effect).
func TestStoreDDLChecksDesiredValue(t *testing.T) {
	db := openTestDB(t)
	if _, err := NewStore(db); err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	_, err := db.Exec(`INSERT INTO miner_lifecycle_state (id, desired, reason, command_id, updated_at)
		VALUES (1, 'sleeping', '', '', strftime('%s','now'))`)
	if err == nil {
		t.Fatal("expected the CHECK constraint to reject an invalid desired value")
	}
}

// Load classifies a row that violates the CHECK constraint's intent
// (reachable only via a future schema version or manual edit — bypassed
// here with ignore_check_constraints, exactly the scenario design v6 §8
// calls out) as a *CorruptStateError carrying the raw value.
func TestStoreLoadClassifiesCorruptValue(t *testing.T) {
	db := openTestDB(t)
	if _, err := NewStore(db); err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if _, err := db.Exec(`PRAGMA ignore_check_constraints = ON`); err != nil {
		t.Fatalf("PRAGMA ignore_check_constraints: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO miner_lifecycle_state (id, desired, reason, command_id, updated_at)
		VALUES (1, 'sleeping', '', '', strftime('%s','now'))`); err != nil {
		t.Fatalf("insert bypassing CHECK: %v", err)
	}

	store := &Store{db: db}
	_, err := store.Load(context.Background())

	var corrupt *CorruptStateError
	if !errors.As(err, &corrupt) {
		t.Fatalf("Load error = %v, want a *CorruptStateError", err)
	}
	if corrupt.Raw != "sleeping" {
		t.Fatalf("corrupt.Raw = %q, want %q", corrupt.Raw, "sleeping")
	}
	if !errors.Is(err, ErrCorruptState) {
		t.Fatal("errors.Is(err, ErrCorruptState) = false")
	}
}

// (13) A persist call that cannot acquire the single SQLite connection
// (SetMaxOpenConns(1), held by a concurrent, long-running transaction)
// fails within persistTimeout instead of hanging indefinitely.
func TestStoreSaveFailsWithinPersistTimeoutWhenConnectionBusy(t *testing.T) {
	origTimeout := persistTimeout
	persistTimeout = 200 * time.Millisecond
	t.Cleanup(func() { persistTimeout = origTimeout })

	db := openTestDB(t)
	store, err := NewStore(db)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}

	holding := make(chan struct{})
	release := make(chan struct{})
	txErr := make(chan error, 1)
	go func() {
		txErr <- db.WithTx(context.Background(), func(tx *sql.Tx) error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding
	defer func() {
		close(release)
		if err := <-txErr; err != nil {
			t.Errorf("holder transaction: %v", err)
		}
	}()

	start := time.Now()
	saveErr := store.Save(context.Background(), DesiredPaused, "user", "cmd-busy")
	elapsed := time.Since(start)

	if saveErr == nil {
		t.Fatal("expected Save to fail while the single connection is held by another transaction")
	}
	// Generous slack over persistTimeout for scheduling jitter, but still
	// far below "hung indefinitely".
	if elapsed > 2*time.Second {
		t.Fatalf("Save took %s to fail, want roughly within persistTimeout (%s)", elapsed, persistTimeout)
	}
}
