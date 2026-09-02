package watcher

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
)

// B2 final closure — the watch generation's ONE deliberately uncancellable
// write versus the owner that closes the store underneath it.
//
// MinuteWatcher.Stop's bounded join exists so an in-flight watch_time credit
// drains before the caller closes the database (watcher.go: "the bounded join
// in Stop exists precisely so an in-flight watch_time write DRAINS before the
// caller closes the database"). When that join expires the teardown is
// classified DIRTY and shutdown proceeds anyway — by design, because the
// alternatives (waiting for a goroutine with no kill mechanism, or handing the
// close to a reaper) are exactly what the concern's contract forbids.
//
// What is NOT by design is the store silently losing the race. WatchTimeStore
// is the only store in this repository that talks to the shared handle through
// the embedded *sql.DB, bypassing the closed-barrier that database.DB provides
// and that internal/{analytics,notifications,drops,lifecycle,streamerlifecycle,
// updater} all go through. The two tests below pin the two consequences of that
// bypass, both of which are observable behaviour rather than a signature.

// openBarrierWatchTimeStore opens an independent SQLite file (bypassing
// database.Open's process-wide singleton, like openWatchTimeStore does) and
// returns the *database.DB itself, because these tests drive Close as the
// shutdown owner would. The single-connection pool mirrors database.Open: it is
// what makes a concurrent credit and a concurrent Close contend for real.
func openBarrierWatchTimeStore(t *testing.T, path string) (*WatchTimeStore, *database.DB) {
	t.Helper()

	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)

	db := &database.DB{DB: sqlDB}
	store, err := NewWatchTimeStore(db)
	if err != nil {
		_ = sqlDB.Close()
		t.Fatalf("failed to create watch time store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	return store, db
}

// waitForStoreCondition polls cond until it holds or the deadline expires. It is
// the deterministic barrier these tests use instead of sleeping: the condition
// is a database/sql pool statistic, so it flips exactly when the credit has
// reached the pool and not before.
func waitForStoreCondition(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestWatchTimeCreditRefusedAfterCloseIsRecognisable pins the diagnosis half.
//
// After a DIRTY teardown the watch loop is still live and may still attempt its
// credit while the owner has already closed the handle. That credit cannot
// land — nothing can change that — but it must fail as a RECOGNISABLE
// shutdown refusal, not as an opaque driver string. Going through the embedded
// *sql.DB yields database/sql's unexported errDBClosed ("sql: database is
// closed"), which no caller can match with errors.Is, so a shutdown-race loss
// is indistinguishable from a genuine storage fault at the call site.
func TestWatchTimeCreditRefusedAfterCloseIsRecognisable(t *testing.T) {
	store, db := openBarrierWatchTimeStore(t, filepath.Join(t.TempDir(), "wt.db"))

	if err := store.RecordMinutes("alice", 1, time.Now()); err != nil {
		t.Fatalf("the store must accept a credit while the database is open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	err := store.RecordMinutes("alice", 1, time.Now())
	if err == nil {
		t.Fatal("a credit attempted after the owner closed the database must not report success")
	}
	if !errors.Is(err, database.ErrClosed) {
		t.Fatalf("a credit refused because the shutdown owner closed the handle must be recognisable "+
			"as database.ErrClosed so the loss can be told apart from a storage fault, got %v", err)
	}
}

// TestClosingTheDatabaseDoesNotDiscardAnInFlightWatchTimeCredit pins the
// durability half, and it is the behavioural core of the CodeRabbit finding.
//
// The scenario is the dirty teardown itself: the watch loop is inside its
// credit — past every cancellation gate, in the region the bounded join exists
// to drain — when the shutdown owner closes the handle. The credit is already
// earned: the minute-watched beacon succeeded server-side, the points are
// banked at Twitch, and this row is the local fair-rotation record of it.
//
// The test makes that window deterministic instead of hoping for it: it holds
// the single pooled connection open, so the credit is provably parked inside
// the store (db.Stats().WaitCount) before Close is called. Going through the
// embedded *sql.DB, the credit holds nothing the owner must respect, so Close
// runs straight past it and database/sql wakes the parked request with
// errDBClosed — the row is gone, and the only trace in production is a
// slog.Debug line. Going through database.DB's own barrier, Close waits for
// that single statement (the same bounded wait every other store in this
// repository already imposes on it) and the row lands.
//
// This is emphatically NOT the unbounded "do not close until the watcher loop
// exits" the finding proposes: the wait is one INSERT long and completely
// independent of whether the generation ever quiesces.
func TestClosingTheDatabaseDoesNotDiscardAnInFlightWatchTimeCredit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wt.db")
	store, db := openBarrierWatchTimeStore(t, path)

	now := time.Now()

	// Occupy the only pooled connection so the credit below must wait for it.
	// Begin goes through the embedded *sql.DB deliberately: it must NOT itself
	// hold the barrier, or it — not the credit — would be what blocks Close.
	hold, err := db.Begin()
	if err != nil {
		t.Fatalf("begin the connection-holding transaction: %v", err)
	}

	credit := make(chan error, 1)
	go func() { credit <- store.RecordMinutes("alice", 7, now) }()

	waitForStoreCondition(t, "the credit to reach the connection pool", func() bool {
		return db.Stats().WaitCount > 0
	})

	closed := make(chan error, 1)
	go func() { closed <- db.Close() }()

	// Release the connection: the credit can now complete. If Close respected
	// the in-flight credit it is still waiting; if it did not, it has already
	// torn the pool down and the credit is lost.
	if err := hold.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
		t.Logf("releasing the connection-holding transaction: %v", err)
	}

	select {
	case err := <-credit:
		if err != nil {
			t.Fatalf("a credit already in flight when the shutdown owner closed the database was "+
				"discarded instead of drained: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight credit never completed")
	}

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close never returned after the in-flight credit finished — it must wait for one " +
			"statement, never for the watch generation to quiesce")
	}

	// Reopen the file: the credited minute must be on disk. The opportunistic
	// prune that follows the INSERT is deliberately NOT asserted — it is
	// housekeeping, and whether it won or lost its own race against Close must
	// never change the verdict on the credit.
	assertWatchTimeRowPersisted(t, path, "alice")
}

// TestAStuckWatchTimeCreditCannotHoldShutdownOpenForever is the counterweight
// to the test above, and it guards the cost of taking the barrier at all.
//
// Making Close wait for an in-flight credit is what saves the earned minute —
// but a wait is only safe if it has a ceiling. The credit's one genuinely
// unbounded step is acquiring the shared handle's single pooled connection.
// Before the credit took the barrier, a connection nobody released cost only the
// credit: Close ran past it regardless. Now Close queues behind it, so an
// unbounded credit would mean an unbounded shutdown — the process would never
// exit, which is precisely the "wait forever" the whole dirty-teardown design
// exists to refuse (and App.Shutdown could not rescue it: its step closer takes
// no context).
//
// So the credit carries its own ceiling, independent of the generation context:
// it gives up, and Close proceeds.
func TestAStuckWatchTimeCreditCannotHoldShutdownOpenForever(t *testing.T) {
	previous := watchTimeCreditTimeout
	watchTimeCreditTimeout = 150 * time.Millisecond
	t.Cleanup(func() { watchTimeCreditTimeout = previous })

	store, db := openBarrierWatchTimeStore(t, filepath.Join(t.TempDir(), "wt.db"))

	// Hold the only connection for the whole assertion: the credit can never
	// acquire it, so it must fall back on its own ceiling rather than the pool.
	hold, err := db.Begin()
	if err != nil {
		t.Fatalf("begin the connection-holding transaction: %v", err)
	}
	defer func() { _ = hold.Rollback() }()

	credit := make(chan error, 1)
	go func() { credit <- store.RecordMinutes("alice", 3, time.Now()) }()

	waitForStoreCondition(t, "the credit to reach the connection pool", func() bool {
		return db.Stats().WaitCount > 0
	})

	closed := make(chan error, 1)
	go func() { closed <- db.Close() }()

	select {
	case err := <-credit:
		if err == nil {
			t.Fatal("a credit that never reached the database must not report success")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the credit waited past its own ceiling for a connection it could not get")
	}

	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a credit that could not proceed held the shutdown owner's Close open: taking the " +
			"barrier turned a lost row into a shutdown that never completes")
	}
}

// TestFailedPruneIsNotReportedAsALostWatchTimeCredit pins the third
// consequence of the same shutdown race, the one that turns a SUCCESS into a
// reported failure.
//
// RecordMinutes credits a minute and then opportunistically prunes rows that
// have aged out. Those are two different statements with two different stakes:
// the credit is earned data, the prune is housekeeping. Reporting the prune's
// error as RecordMinutes' verdict conflates them — and during a dirty teardown
// that is exactly what happens, because the credit can commit just before the
// owner closes the handle and the prune then fails on the closed one. The watch
// loop would log "Failed to record watch time" for a minute that is on disk,
// pointing any later diagnosis at the wrong thing.
//
// The failure is forced here by construction rather than by racing a close, so
// the assertion is deterministic: a BEFORE DELETE trigger makes every prune
// abort while leaving inserts untouched.
func TestFailedPruneIsNotReportedAsALostWatchTimeCredit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wt.db")
	store, db := openBarrierWatchTimeStore(t, path)

	now := time.Now()

	// A row old enough for the prune to actually target, so the trigger fires.
	aged := now.Add(-3 * watchTimeWindow)
	if _, err := db.Exec(
		`INSERT INTO watch_time_events (streamer, timestamp, minutes) VALUES (?, ?, ?)`,
		"stale", aged.Unix(), 4.0,
	); err != nil {
		t.Fatalf("seed an aged row: %v", err)
	}
	if _, err := db.Exec(`CREATE TRIGGER refuse_prune BEFORE DELETE ON watch_time_events
		BEGIN SELECT RAISE(ABORT, 'prune refused'); END;`); err != nil {
		t.Fatalf("install the prune-refusing trigger: %v", err)
	}

	if err := store.RecordMinutes("alice", 5, now); err != nil {
		t.Fatalf("a credit that committed must not be reported as failed because the opportunistic "+
			"prune that follows it failed: %v", err)
	}

	// Prove the prune really did fail, so the assertion above was not vacuous.
	var stale float64
	if err := db.QueryRow(
		`SELECT COALESCE(SUM(minutes), 0) FROM watch_time_events WHERE streamer = ?`, "stale",
	).Scan(&stale); err != nil {
		t.Fatalf("read back the aged row: %v", err)
	}
	if stale == 0 {
		t.Fatal("the aged row was pruned, so this test never exercised a failing prune")
	}

	got, err := store.WindowMinutes([]string{"alice"}, now)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if got["alice"] != 5 {
		t.Fatalf("the credited minutes are missing: got %v, want 5", got["alice"])
	}
}

// assertWatchTimeRowPersisted reopens the database file with a fresh handle and
// fails unless login has at least one credited row.
func assertWatchTimeRowPersisted(t *testing.T, path, login string) {
	t.Helper()

	reopened, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	var minutes float64
	err = reopened.QueryRow(
		`SELECT COALESCE(SUM(minutes), 0) FROM watch_time_events WHERE streamer = ?`, login,
	).Scan(&minutes)
	if err != nil {
		t.Fatalf("read back the credited watch time: %v", err)
	}
	if minutes <= 0 {
		t.Fatalf("the credit for %q is not on disk: the shutdown owner closed the database out from "+
			"under a write the bounded join exists to drain, and the earned minute was lost", login)
	}
}
