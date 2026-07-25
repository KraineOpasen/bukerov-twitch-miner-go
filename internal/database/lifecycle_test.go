package database

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func createKV(t *testing.T, db *DB) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE kv (k TEXT PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
}

// TestWithTxCommits: a successful WithTx persists its writes.
func TestWithTxCommits(t *testing.T) {
	db := openRaw(t)
	createKV(t, db)

	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO kv (k, v) VALUES ('a', '1')`)
		return err
	})
	if err != nil {
		t.Fatalf("WithTx: %v", err)
	}
	var v string
	if err := db.QueryRow(`SELECT v FROM kv WHERE k = 'a'`).Scan(&v); err != nil || v != "1" {
		t.Fatalf("committed value = %q err=%v, want 1", v, err)
	}
}

// TestWithTxRollsBackOnError (R13): a WithTx whose fn returns an error leaves no
// partial writes — the transaction rolls back atomically.
func TestWithTxRollsBackOnError(t *testing.T) {
	db := openRaw(t)
	createKV(t, db)

	sentinel := errors.New("boom")
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO kv (k, v) VALUES ('b', '2')`); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("WithTx err = %v, want sentinel", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM kv WHERE k = 'b'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("rollback left %d rows, want 0 (partial write)", n)
	}
}

// TestCloseIdempotent (R6): Close may be called repeatedly without error or a
// double-close panic.
func TestCloseIdempotent(t *testing.T) {
	db := openRaw(t)
	if err := db.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second close should be a no-op, got %v", err)
	}
}

// TestWithTxAfterCloseTyped (R7): a lifecycle operation after Close returns the
// typed ErrClosed rather than a raw driver string, and never opens a connection.
func TestWithTxAfterCloseTyped(t *testing.T) {
	db := openRaw(t)
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	err := db.WithTx(context.Background(), func(*sql.Tx) error { return nil })
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("WithTx after close = %v, want ErrClosed", err)
	}
}
