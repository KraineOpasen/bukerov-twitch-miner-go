package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
)

// TestWithTxHoldsCloseUntilCommit pins the close barrier that read snapshots
// rely on: a Close that arrives while WithTx is running its function blocks
// until the transaction has committed, and the function keeps its connection
// for the whole run. The wait is observed structurally — a pending writer on
// the handle's lock refuses new read locks — never by timing; a WithTx that
// dropped the read lock would let Close return while the function is still
// running, which the function reports at once.
func TestWithTxHoldsCloseUntilCommit(t *testing.T) {
	db := openRawTestDB(t)
	if _, err := db.Exec(`CREATE TABLE t (id INTEGER)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	closeDone := make(chan error, 1)
	var fnReturned atomic.Bool
	var closeSawFnReturned bool
	err := db.WithTx(context.Background(), func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO t (id) VALUES (1)`); err != nil {
			return err
		}
		go func() {
			err := db.Close()
			closeSawFnReturned = fnReturned.Load()
			closeDone <- err
		}()
		// Wait until Close is pending on the write lock: from then on new
		// read locks are refused, which is exactly the barrier state. A Close
		// that got through instead is reported, not waited for.
		for db.mu.TryRLock() {
			db.mu.RUnlock()
			select {
			case err := <-closeDone:
				return fmt.Errorf("Close returned while the transaction's function was still running: %v", err)
			default:
				runtime.Gosched()
			}
		}
		// Close is blocked; the transaction still owns its connection.
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("count inside the transaction = %d, want 1", n)
		}
		fnReturned.Store(true)
		return nil
	})
	if err != nil {
		t.Fatalf("WithTx with Close pending: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close after the transaction: %v", err)
	}
	if !closeSawFnReturned {
		t.Fatal("Close returned before the transaction's function had returned")
	}
	if err := db.WithTx(context.Background(), func(*sql.Tx) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Fatalf("WithTx after Close = %v, want ErrClosed", err)
	}
}
