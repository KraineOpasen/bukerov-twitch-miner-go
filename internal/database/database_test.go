package database

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// openRaw builds a private, non-singleton DB over its own file so migration
// tests are isolated from the process-wide instance other packages' tests
// share (same construction rotation_test.go uses in the watcher package).
func openRaw(t *testing.T) *DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "miner.db"))
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return &DB{DB: sqlDB}
}

type testModule struct {
	name       string
	migrations []Migration
}

func (m testModule) Name() string            { return m.name }
func (m testModule) Migrations() []Migration { return m.migrations }

func (db *DB) versionOf(t *testing.T, module string) int {
	t.Helper()
	var v int
	err := db.QueryRow("SELECT version FROM schema_versions WHERE module = ?", module).Scan(&v)
	if err == sql.ErrNoRows {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (db *DB) tableExists(t *testing.T, table string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestRegisterModuleAppliesAndBumpsVersion(t *testing.T) {
	db := openRaw(t)
	mod := testModule{name: "m", migrations: []Migration{
		{Version: 1, Description: "t1", SQL: `CREATE TABLE t1 (id INTEGER);`},
		{Version: 2, Description: "t2", SQL: `CREATE TABLE t2 (id INTEGER);`},
	}}
	if err := db.RegisterModule(mod); err != nil {
		t.Fatal(err)
	}
	if !db.tableExists(t, "t1") || !db.tableExists(t, "t2") {
		t.Fatal("migrations not applied")
	}
	if v := db.versionOf(t, "m"); v != 2 {
		t.Fatalf("version = %d, want 2", v)
	}
}

// A failing migration must roll back BOTH its schema changes and any earlier
// statements of the same migration, and must not bump the version — the
// crash/failure window between "SQL applied" and "version bumped" is closed.
func TestRegisterModuleFailedMigrationRollsBackAtomically(t *testing.T) {
	db := openRaw(t)
	mod := testModule{name: "m", migrations: []Migration{
		{Version: 1, Description: "ok", SQL: `CREATE TABLE ok_table (id INTEGER);`},
		{Version: 2, Description: "boom", SQL: `
			CREATE TABLE half_table (id INTEGER);
			THIS IS NOT SQL;
		`},
	}}
	if err := db.RegisterModule(mod); err == nil {
		t.Fatal("expected migration v2 to fail")
	}

	if !db.tableExists(t, "ok_table") {
		t.Fatal("v1 must stay applied")
	}
	// The transactional guarantee: v2's first statement must NOT survive.
	if db.tableExists(t, "half_table") {
		t.Fatal("half of failed migration v2 survived — migration not atomic")
	}
	if v := db.versionOf(t, "m"); v != 1 {
		t.Fatalf("version = %d, want 1 (v2 must not be recorded)", v)
	}

	// A later run with the migration fixed applies cleanly from v2.
	fixed := testModule{name: "m", migrations: []Migration{
		mod.migrations[0],
		{Version: 2, Description: "fixed", SQL: `CREATE TABLE half_table (id INTEGER);`},
	}}
	if err := db.RegisterModule(fixed); err != nil {
		t.Fatalf("fixed migration should apply: %v", err)
	}
	if v := db.versionOf(t, "m"); v != 2 {
		t.Fatalf("version after fix = %d, want 2", v)
	}
}

// Already-applied migrations are skipped by the version check — repeated
// registration of a fully-applied module executes nothing and stays at the
// same version (the property that makes the transactional rewrite
// behavior-neutral for existing healthy databases).
func TestRegisterModuleSkipsAppliedMigrations(t *testing.T) {
	db := openRaw(t)
	applied := 0
	mod := testModule{name: "m", migrations: []Migration{
		{Version: 1, Description: "counted", Run: func(tx *sql.Tx) error {
			applied++
			_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS c (id INTEGER)`)
			return err
		}},
	}}
	for i := 0; i < 3; i++ {
		if err := db.RegisterModule(mod); err != nil {
			t.Fatal(err)
		}
	}
	if applied != 1 {
		t.Fatalf("migration executed %d times, want exactly 1", applied)
	}
	if v := db.versionOf(t, "m"); v != 1 {
		t.Fatalf("version = %d, want 1", v)
	}
}

// AddColumnIfMissing heals a half-applied ALTER TABLE migration with
// PER-COLUMN granularity: a table where exactly one of two columns already
// exists gets only the missing one added, and the migration then succeeds.
func TestAddColumnIfMissingHealsPartiallyAppliedMigration(t *testing.T) {
	db := openRaw(t)

	// Simulate the pre-transactional crash window of a two-ALTER migration:
	// base table + FIRST column already present, version still at 1.
	if _, err := db.Exec(`CREATE TABLE cfg (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`ALTER TABLE cfg ADD COLUMN col_a TEXT DEFAULT ''`); err != nil {
		t.Fatal(err)
	}
	seed := testModule{name: "m", migrations: []Migration{
		{Version: 1, Description: "base", SQL: `CREATE TABLE IF NOT EXISTS cfg_unused (id INTEGER);`},
	}}
	if err := db.RegisterModule(seed); err != nil {
		t.Fatal(err)
	}

	mod := testModule{name: "m", migrations: []Migration{
		seed.migrations[0],
		{Version: 2, Description: "two guarded columns", Run: func(tx *sql.Tx) error {
			if err := AddColumnIfMissing(tx, "cfg", "col_a", "TEXT DEFAULT ''"); err != nil {
				return err
			}
			return AddColumnIfMissing(tx, "cfg", "col_b", "INTEGER DEFAULT 1")
		}},
	}}
	if err := db.RegisterModule(mod); err != nil {
		t.Fatalf("self-heal migration must succeed on a partially-applied schema: %v", err)
	}

	// Both columns present, existing one untouched, version bumped.
	for _, col := range []string{"col_a", "col_b"} {
		var n int
		q := fmt.Sprintf("SELECT COUNT(*) FROM pragma_table_info('cfg') WHERE name = '%s'", col)
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("column %s count = %d, want 1", col, n)
		}
	}
	if v := db.versionOf(t, "m"); v != 2 {
		t.Fatalf("version = %d, want 2", v)
	}

	// Fully-applied variant: running again is a no-op (idempotent now).
	if err := db.RegisterModule(mod); err != nil {
		t.Fatalf("re-registration must be a no-op: %v", err)
	}
}

// The singleton must not cache a failed initialization: a failed first Open
// returns an error (not (nil, nil)) and a later Open with a good path works.
func TestOpenRetryableAfterFailedInit(t *testing.T) {
	instanceMu.Lock()
	if instance != nil {
		// Another package's test (or an earlier test in this binary) already
		// initialized the process-wide singleton; the failed-first-init path
		// can no longer be exercised in this process.
		instanceMu.Unlock()
		t.Skip("process-wide DB singleton already initialized")
	}
	instanceMu.Unlock()

	// A path whose parent is a FILE makes MkdirAll fail deterministically.
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("file, not dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	badPath := filepath.Join(blocker, "sub")

	db, err := Open(badPath)
	if err == nil {
		t.Fatal("expected first Open with a bad path to fail")
	}
	if db != nil {
		t.Fatal("failed Open must return a nil DB with a non-nil error")
	}

	// Second attempt must NOT return (nil, nil): with a good path it succeeds.
	good := filepath.Join(dir, "goodbase")
	db2, err := Open(good)
	if err != nil {
		t.Fatalf("Open must be retryable after a failed init: %v", err)
	}
	if db2 == nil {
		t.Fatal("retried Open returned nil DB")
	}
}
