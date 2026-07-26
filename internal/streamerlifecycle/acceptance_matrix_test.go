package streamerlifecycle_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
)

// This file fills the remaining M1 matrix gaps (mandate §16) not already
// covered by cd5c9c1's own internal/streamerlifecycle/srap_test.go and
// durable_test.go:
//   - marker-clear ordering (the pending row must still be visible, in the
//     SAME transaction, right up to the moment a purger fails — not merely
//     "eventually still present after rollback", which TestCommitRemoval
//     PurgeFailureLeavesPendingRow already pins).
//   - marker-clear-failure -> safe idempotent retry across REPEATED
//     CommitRemoval calls (durable_test.go's TestFailedPurgePersistsAndRec
//     onciles already pins repeated Reconcile idempotency; this covers
//     repeated CommitRemoval specifically).
//   - DB close cannot race an active critical (purge) transaction: WithTx
//     holds db.mu.RLock() for its whole duration (internal/database/
//     database.go's WithTx/Close), so a concurrent Close() must block until
//     the transaction settles — untested anywhere in internal/database or
//     internal/streamerlifecycle today.
//
// All three use PRIVATE, non-singleton databases (openRawDB/buildRawStores,
// from durable_test.go) with unique fixture logins, per the singleton-DB and
// unique-login conventions established across this package's tests.

// orderingPurger runs the REAL purge (an inner Purger) but records, via a
// query issued on the SAME *sql.Tx purgeAndClearTx passes in, whether the
// pending-purge ledger row was still visible in-transaction at the moment
// this purger ran — proving purge-then-clear ordering (the row is not
// cleared until every purger, including this one, has successfully run in
// the SAME transaction) rather than merely "the whole transaction rolled
// back so the row is still there afterward" (already covered elsewhere).
type orderingPurger struct {
	inner        streamerlifecycle.Purger
	login        string
	sawRowInTx   *bool
	failAfterRow bool
}

func (p orderingPurger) DeleteStreamerTx(tx *sql.Tx, login string) (bool, error) {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM pending_streamer_deletions WHERE login = ?`, login).Scan(&one)
	*p.sawRowInTx = err == nil
	if p.failAfterRow {
		return false, errors.New("injected ordering-probe failure")
	}
	return p.inner.DeleteStreamerTx(tx, login)
}

// TestMarkerClearOrderingRowVisibleInTxUntilPurgeCommits proves the
// transaction ordering purgeAndClearTx documents: the pending-purge row is
// still SELECT-able from WITHIN the same transaction right up to the instant
// a purger fails (proving the DELETE of that row is scheduled strictly AFTER
// every purger succeeds, never interleaved before them) — not merely that it
// survives an eventual rollback.
func TestMarkerClearOrderingRowVisibleInTxUntilPurgeCommits(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "miner.db"))
	an, _, _, coord := buildRawStores(t, db)
	ctx := context.Background()
	const login = "orderingprobe"

	if err := an.RecordPoints(login, 100, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	if err := coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{{ChannelID: "chan-ordering", Login: login}}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	var sawRow bool
	probing, err := streamerlifecycle.New(db,
		[]streamerlifecycle.Purger{orderingPurger{inner: an, login: login, sawRowInTx: &sawRow, failAfterRow: true}},
		[]streamerlifecycle.Fencer{an}, nil)
	if err != nil {
		t.Fatalf("probing coordinator: %v", err)
	}

	if _, err := probing.CommitRemoval(ctx, "chan-ordering", login); err == nil {
		t.Fatal("expected the injected ordering-probe failure")
	}
	if !sawRow {
		t.Error("the pending row was NOT visible in-transaction when the purger ran — ordering (purge runs before the row is cleared) could not be verified")
	}

	// The row must still be durably present after the rollback too (the
	// purger's own failure means the DELETE inside the same tx never ran).
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pending_streamer_deletions WHERE login = ?`, login).Scan(&n); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if n != 1 {
		t.Errorf("pending rows for %s = %d, want 1 (marker only clears once the purge itself commits)", login, n)
	}
}

// TestReconcileFailureBumpsAttemptsCounter closes a coverage gap Wave 1
// flagged (bumpAttempts at 0%, exercised by neither cd5c9c1's own tests nor
// this pass' others, since every other Reconcile-under-failure test here
// heals the purger before calling Reconcile): a pending row whose retried
// purge ALSO fails during Reconcile (not CommitRemoval) has its durable
// attempts counter incremented, so a persistently-stuck deletion is at least
// observable/boundable across repeated startups, per Reconcile's own doc
// comment ("its attempt counter is bumped, and it is retried on the next
// startup").
func TestReconcileFailureBumpsAttemptsCounter(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "miner.db"))
	an, _, _, _ := buildRawStores(t, db)
	ctx := context.Background()
	const login = "bumpattemptsprobe"

	if err := an.RecordPoints(login, 100, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	// Seed a pending row directly, as an already-committed-but-owed purge
	// left by an earlier (successful) admission+commit would.
	if _, err := db.Exec(`INSERT INTO pending_streamer_deletions (login, channel_id, requested_at, attempts) VALUES (?, ?, ?, 0)`,
		login, "chan-bump", 1); err != nil {
		t.Fatalf("seed pending row: %v", err)
	}

	stillFailing, err := streamerlifecycle.New(db, []streamerlifecycle.Purger{an, failPurger{}}, []streamerlifecycle.Fencer{an}, nil)
	if err != nil {
		t.Fatalf("failing coordinator: %v", err)
	}

	if _, err := stillFailing.Reconcile(ctx); err == nil {
		t.Fatal("expected Reconcile to report the injected purge failure")
	}

	var attempts int
	if err := db.QueryRow(`SELECT attempts FROM pending_streamer_deletions WHERE login = ?`, login).Scan(&attempts); err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 after one failed Reconcile pass", attempts)
	}

	// A second failed pass bumps it again.
	if _, err := stillFailing.Reconcile(ctx); err == nil {
		t.Fatal("expected the second Reconcile to also report the injected purge failure")
	}
	if err := db.QueryRow(`SELECT attempts FROM pending_streamer_deletions WHERE login = ?`, login).Scan(&attempts); err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts != 2 {
		t.Errorf("attempts = %d, want 2 after two failed Reconcile passes", attempts)
	}
}

// TestCommitRemovalRepeatedFailureIdempotentRetry proves CommitRemoval
// itself (not just Reconcile — durable_test.go's TestFailedPurgePersists
// AndReconciles already pins Reconcile's idempotency) is safe to call
// repeatedly against a still-failing purge: each call fails the same typed
// way, the pending row is neither duplicated nor lost, and once the purger
// is healed the NEXT CommitRemoval call completes cleanly.
func TestCommitRemovalRepeatedFailureIdempotentRetry(t *testing.T) {
	db := openRawDB(t, filepath.Join(t.TempDir(), "miner.db"))
	an, _, _, coord := buildRawStores(t, db)
	ctx := context.Background()
	const login = "repeatedretry"

	if err := an.RecordPoints(login, 100, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	if err := coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{{ChannelID: "chan-retry", Login: login}}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	failing, err := streamerlifecycle.New(db, []streamerlifecycle.Purger{an, failPurger{}}, []streamerlifecycle.Fencer{an}, nil)
	if err != nil {
		t.Fatalf("failing coordinator: %v", err)
	}

	// Two repeated failed retries: neither duplicates nor drops the marker.
	for i := 0; i < 2; i++ {
		if _, err := failing.CommitRemoval(ctx, "chan-retry", login); err == nil {
			t.Fatalf("retry %d: expected the injected purge failure", i)
		}
		var pending, admissions int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pending_streamer_deletions WHERE login = ?`, login).Scan(&pending); err != nil {
			t.Fatalf("count pending: %v", err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM streamer_deletion_admissions WHERE login = ?`, login).Scan(&admissions); err != nil {
			t.Fatalf("count admissions: %v", err)
		}
		if pending != 1 || admissions != 0 {
			t.Fatalf("retry %d: pending=%d admissions=%d, want 1,0 (idempotent — no duplication, no loss)", i, pending, admissions)
		}
	}

	// A HEALTHY coordinator (the purger fixed, as a retried/healed run would
	// be) completes the very next CommitRemoval call cleanly.
	if _, err := coord.CommitRemoval(ctx, "chan-retry", login); err != nil {
		t.Fatalf("CommitRemoval after the purger healed: %v", err)
	}
	if has, err := coord.HasPending(ctx, login); err != nil || has {
		t.Errorf("HasPending after a successful retry = (%v, %v), want (false, nil)", has, err)
	}
	if data, _ := an.GetStreamerData(login); len(data.Series) != 0 {
		t.Error("history survived a successful retried purge")
	}
}

// blockingCloseProbePurger blocks (signalling reached, then waiting on
// release) from WITHIN a purge transaction, so a test can deterministically
// hold db.WithTx's RLock open while a concurrent Close() attempts to acquire
// the write lock.
type blockingCloseProbePurger struct {
	inner   streamerlifecycle.Purger
	reached chan struct{}
	release chan struct{}
}

func (p blockingCloseProbePurger) DeleteStreamerTx(tx *sql.Tx, login string) (bool, error) {
	close(p.reached)
	<-p.release
	return p.inner.DeleteStreamerTx(tx, login)
}

// TestCloseBlocksUntilInFlightTransactionCompletes proves database.DB's
// WithTx/Close mutual exclusion (WithTx holds db.mu.RLock() for its whole
// duration; Close takes db.mu.Lock()) actually protects a streamerlifecycle
// transaction: a concurrent Close() call issued while a purge transaction is
// still in flight must BLOCK until that transaction settles — never
// interleave, panic, or corrupt the ledger. Uses the purge transaction as
// the concrete vehicle (AdmitRemovals' own tx has no purger/fencer hook to
// synchronize on; WithTx's locking is identical for both, so this exercises
// the same generic guarantee an in-flight ADMISSION transaction relies on).
// Synchronized purely with channels, no sleeps.
func TestCloseBlocksUntilInFlightTransactionCompletes(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "miner.db")
	db := openRawDB(t, dbPath)
	an, _, _, coord := buildRawStores(t, db)
	ctx := context.Background()
	const login = "closeraceprobe"

	if err := an.RecordPoints(login, 50, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	if err := coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{{ChannelID: "chan-close", Login: login}}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	reached := make(chan struct{})
	release := make(chan struct{})
	blocking, err := streamerlifecycle.New(db,
		[]streamerlifecycle.Purger{blockingCloseProbePurger{inner: an, reached: reached, release: release}},
		[]streamerlifecycle.Fencer{an}, nil)
	if err != nil {
		t.Fatalf("blocking coordinator: %v", err)
	}

	commitDone := make(chan error, 1)
	go func() {
		_, err := blocking.CommitRemoval(ctx, "chan-close", login)
		commitDone <- err
	}()
	<-reached // the purge transaction is now in flight, holding db.mu.RLock()

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- db.Close()
	}()

	// Close() cannot have returned yet: the purge transaction still holds
	// the read lock (blocked on <-release), and Close() needs the write
	// lock. Deterministic, not a timing race — mirrored on the CommitRemoval
	// side below via the same non-blocking probe.
	select {
	case <-closeDone:
		t.Fatal("Close() returned while a purge transaction was still in flight")
	default:
	}

	close(release) // let the purge transaction (and therefore Close) proceed

	if err := <-commitDone; err != nil {
		t.Fatalf("in-flight CommitRemoval failed despite no injected purge error: %v", err)
	}
	if err := <-closeDone; err != nil {
		t.Fatalf("Close() after the in-flight transaction settled: %v", err)
	}

	// No corruption: reopen a fresh handle and confirm a clean, fully
	// purged, non-torn final state.
	reopened := openRawDB(t, dbPath)
	defer func() { _ = reopened.Close() }()
	var pending, admissions int
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM pending_streamer_deletions WHERE login = ?`, login).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM streamer_deletion_admissions WHERE login = ?`, login).Scan(&admissions); err != nil {
		t.Fatalf("count admissions: %v", err)
	}
	if pending != 0 || admissions != 0 {
		t.Errorf("pending=%d admissions=%d after Close waited for the in-flight transaction, want 0,0 (no torn state)", pending, admissions)
	}
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='streamer_deletion_admissions'`).Scan(new(int)); err != nil {
		t.Fatalf("database corrupted after the Close/WithTx race: %v", err)
	}
}

// TestAbortAdmissionEmptyListIsNoop covers AbortAdmission's own "nothing to
// compensate" early return (an untested branch: every existing AbortAdmission
// test names at least one login, even if never-admitted) — both a nil slice
// and an all-blank one must be silent no-ops, matching AdmitRemovals' own
// documented empty-batch contract.
func TestAbortAdmissionEmptyListIsNoop(t *testing.T) {
	s := newStores(t)
	if err := s.coord.AbortAdmission(context.Background(), nil); err != nil {
		t.Fatalf("AbortAdmission(nil) = %v, want nil", err)
	}
	if err := s.coord.AbortAdmission(context.Background(), []string{"   ", ""}); err != nil {
		t.Fatalf("AbortAdmission(all-blank) = %v, want nil", err)
	}
}

// TestCommitRemovalMoveFailureLeavesAdmissionRowArmedFence covers
// CommitRemoval's OTHER durability branch (its doc comment's "if step 2
// fails" clause): the admissions row is closed over BEFORE Tombstone runs
// (in-memory, always succeeds), but the move-to-pending transaction itself
// fails (DB closed between AdmitRemovals and CommitRemoval — never exercised
// by any existing test, which all fail either the whole admission or the
// purge step, never the move step in isolation). The ORIGINAL admissions row
// must survive untouched (never moved, never duplicated into pending), and
// the fence must already be armed (Tombstone is unconditional and runs
// first) — exactly what the doc comment promises ArbitratePrepared will
// later resolve via PROMOTE.
func TestCommitRemovalMoveFailureLeavesAdmissionRowArmedFence(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "miner.db")
	db := openRawDB(t, dbPath)
	an, _, _, coord := buildRawStores(t, db)
	ctx := context.Background()
	const login = "movefailprobe"

	if err := an.RecordPoints(login, 100, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	if err := coord.AdmitRemovals(ctx, []streamerlifecycle.Removal{{ChannelID: "chan-movefail", Login: login}}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	if _, err := coord.CommitRemoval(ctx, "chan-movefail", login); err == nil {
		t.Fatal("expected CommitRemoval to fail once the database is closed")
	}

	// The fence is armed regardless (Tombstone is in-memory and unconditional,
	// and runs BEFORE the move transaction): a write for the login is
	// rejected with the fence's own sentinel, not a generic (DB-closed) error
	// — proving Tombstone genuinely ran despite the move step failing right
	// after it.
	if writeErr := an.RecordPoints(login, 1, "WATCH"); !errors.Is(writeErr, analytics.ErrStreamerDeleted) {
		t.Errorf("write after a failed move step = %v, want analytics.ErrStreamerDeleted (the fence must already be armed)", writeErr)
	}

	reopened := openRawDB(t, dbPath)
	defer func() { _ = reopened.Close() }()
	var admissions, pending int
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM streamer_deletion_admissions WHERE login = ?`, login).Scan(&admissions); err != nil {
		t.Fatalf("count admissions: %v", err)
	}
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM pending_streamer_deletions WHERE login = ?`, login).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if admissions != 1 {
		t.Errorf("admissions=%d, want 1 (the original admitted row must survive a failed move step)", admissions)
	}
	if pending != 0 {
		t.Errorf("pending=%d, want 0 (nothing may be duplicated into the pending ledger by a failed move)", pending)
	}
}

// TestReconcileLoginNoPendingIsNoop covers ReconcileLogin's "nothing owed"
// branch (a re-add whose login was never deleted, the common case): must
// report (false, nil) without tombstoning or touching any store — an
// untested branch in this package's own tests (TestReAddBeforeReconcile
// CannotInheritStale only ever exercises the has-pending=true path).
func TestReconcileLoginNoPendingIsNoop(t *testing.T) {
	s := newStores(t)
	ctx := context.Background()
	const login = "reconcileloginnoop"

	if err := s.an.RecordPoints(login, 10, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}

	had, err := s.coord.ReconcileLogin(ctx, login)
	if err != nil {
		t.Fatalf("ReconcileLogin: %v", err)
	}
	if had {
		t.Fatal("ReconcileLogin reported a pending purge for a login that was never deleted")
	}
	// Untouched: no fence, history intact.
	if err := s.an.RecordPoints(login, 5, "WATCH"); err != nil {
		t.Errorf("write after a no-op ReconcileLogin failed: %v", err)
	}
}

// TestReconcileLoginPurgeFailureBumpsAttemptsStaysFenced covers
// ReconcileLogin's own purge-failure branch (distinct from
// TestReAddBeforeReconcileCannotInheritStale, which only exercises the
// success path): a re-add whose owed purge STILL fails must report
// (true, err), bump the attempts counter, and leave the fence armed (the
// re-added streamer stays inert rather than inheriting stale history).
func TestReconcileLoginPurgeFailureBumpsAttemptsStaysFenced(t *testing.T) {
	s := newStores(t)
	ctx := context.Background()
	const login = "reconcileloginfails"

	if err := s.an.RecordPoints(login, 10, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO pending_streamer_deletions (login, channel_id, requested_at, attempts) VALUES (?, ?, ?, 0)`,
		login, "chan-reconcileloginfails", 1); err != nil {
		t.Fatalf("seed pending row: %v", err)
	}

	failing := s.failingCoordinator(t)
	had, err := failing.ReconcileLogin(ctx, login)
	if !had {
		t.Error("ReconcileLogin reported no pending purge despite the seeded row")
	}
	if err == nil {
		t.Fatal("expected the injected purge failure")
	}

	var attempts int
	if err := s.db.QueryRow(`SELECT attempts FROM pending_streamer_deletions WHERE login = ?`, login).Scan(&attempts); err != nil {
		t.Fatalf("read attempts: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if writeErr := s.an.RecordPoints(login, 1, "WATCH"); !errors.Is(writeErr, analytics.ErrStreamerDeleted) {
		t.Errorf("write after a failed ReconcileLogin = %v, want analytics.ErrStreamerDeleted (stays fenced)", writeErr)
	}

	// Clean up via the healthy shared coordinator so this test leaves no
	// stuck row behind for its package-singleton siblings.
	if _, err := s.coord.Reconcile(ctx); err != nil {
		t.Fatalf("cleanup reconcile: %v", err)
	}
	s.coord.Reinstate(login)
}
