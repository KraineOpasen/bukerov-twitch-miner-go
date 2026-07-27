package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// openRawTestDB opens a PRIVATE, non-singleton handle over its own file (the
// openRawNotifDB/openRawMinerDB precedent), so a single test can exercise
// Close without breaking the process-wide singleton other tests share.
func openRawTestDB(t *testing.T) *DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "close-once.db"))
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	return &DB{DB: sqlDB}
}

// TestCloseHappensExactlyOnce (S1): the shutdown path may reach Close from
// more than one owner sequence (App.Shutdown's step, partial-build unwind, a
// library-mode miner). The shared handle must close exactly once — a repeat
// is a defined nil no-op, never a second driver Close — and lifecycle-aware
// work after the close is refused with the typed ErrClosed instead of a raw
// driver error.
func TestCloseHappensExactlyOnce(t *testing.T) {
	db := openRawTestDB(t)

	if _, err := db.Exec(`CREATE TABLE t (id INTEGER)`); err != nil {
		t.Fatalf("exec before close: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close must be an idempotent nil no-op, got: %v", err)
	}

	if err := db.WithTx(context.Background(), func(*sql.Tx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("WithTx after Close = %v, want ErrClosed", err)
	}
}
