package streamerlifecycle_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// failRenamer errors so an atomic multi-store rename must roll back.
type failRenamer struct{}

func (failRenamer) RenameStreamerTx(*sql.Tx, string, string) error {
	return errors.New("injected rename failure")
}

// failingCoordinator builds a coordinator over the shared stores WITH an
// injected failing purger appended, so its Delete/Reconcile transaction always
// rolls back (simulating a disk-full / corruption purge failure) while still
// recording the durable pending marker.
func (s stores) failingCoordinator(t *testing.T) *streamerlifecycle.Coordinator {
	t.Helper()
	c, err := streamerlifecycle.New(s.db,
		[]streamerlifecycle.Purger{s.an, s.no, s.wt, failPurger{}},
		[]streamerlifecycle.Fencer{s.an, s.no, s.wt},
		nil,
	)
	if err != nil {
		t.Fatalf("failing coordinator: %v", err)
	}
	return c
}

// TestFailedPurgePersistsAndReconciles covers BLOCKER-1 tests 1,4,5,6,7,8,10: a
// purge that fails after the deletion intent is persisted records a durable
// marker (no false success, unrelated rows survive), a later reconciliation
// retries it to completion, the marker disappears only on success, and repeated
// reconciliation is idempotent.
func TestFailedPurgePersistsAndReconciles(t *testing.T) {
	ctx := context.Background()
	s := newStores(t)
	s.seedStreamer(t, "durfail", 100)
	s.seedStreamer(t, "durkeep", 200) // unrelated

	failing := s.failingCoordinator(t)

	// (1,10) Delete fails: typed error (no false success), rows remain, durable
	// marker recorded.
	if _, err := failing.Delete(ctx, "chan-durfail", "durfail"); err == nil {
		t.Fatal("Delete reported success despite the purge failing")
	}
	if !s.analyticsHas(t, "durfail") {
		t.Error("rows were deleted despite the purge transaction rolling back")
	}
	if has, _ := s.coord.HasPending(ctx, "durfail"); !has {
		t.Error("no durable pending-deletion marker after a failed purge")
	}

	// (4,5,6) A healthy reconciliation retries and completes it.
	n, err := s.coord.Reconcile(ctx)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Errorf("reconciled %d, want 1", n)
	}
	if s.analyticsHas(t, "durfail") {
		t.Error("analytics rows survived reconciliation")
	}
	if s.hasPointRule(t, "durfail") {
		t.Error("point rule survived reconciliation")
	}
	if m := s.watchMinutes(t, "durfail"); m != 0 {
		t.Errorf("watch-time survived reconciliation: %v", m)
	}
	// (6) Marker only disappears after success.
	if has, _ := s.coord.HasPending(ctx, "durfail"); has {
		t.Error("durable marker survived a successful reconciliation")
	}
	// (7) Unrelated streamer untouched throughout.
	if !s.analyticsHas(t, "durkeep") {
		t.Error("unrelated streamer was purged by reconciliation")
	}

	// (8) Idempotent: a second reconcile finds nothing.
	if n, _ := s.coord.Reconcile(ctx); n != 0 {
		t.Errorf("second reconcile processed %d, want 0 (idempotent)", n)
	}
}

// buildRawStores wires analytics + notifications + watch-time repositories over a
// non-singleton DB (its own file), matching the watcher package's test pattern,
// so a true close/reopen restart can be exercised.
func buildRawStores(t *testing.T, db *database.DB) (*analytics.SQLiteRepository, *notifications.Repository, *watcher.WatchTimeStore, *streamerlifecycle.Coordinator) {
	t.Helper()
	an, err := analytics.NewSQLiteRepository(db, t.TempDir())
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	no, err := notifications.NewRepository(db)
	if err != nil {
		t.Fatalf("notifications: %v", err)
	}
	wt, err := watcher.NewWatchTimeStore(db)
	if err != nil {
		t.Fatalf("watch-time: %v", err)
	}
	coord, err := streamerlifecycle.New(db,
		[]streamerlifecycle.Purger{an, no, wt},
		[]streamerlifecycle.Fencer{an, no, wt},
		[]streamerlifecycle.Renamer{an, no, wt},
	)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	return an, no, wt, coord
}

func openRawDB(t *testing.T, path string) *database.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	return &database.DB{DB: sqlDB}
}

// TestFailedPurgeSurvivesRestart covers BLOCKER-1 tests 2,3,4,5,6: a failed purge
// leaves a durable marker; after the DB is CLOSED and the process "restarts"
// (a fresh handle over the SAME file), startup reconciliation discovers and
// completes the deletion, and the marker is gone only after success.
func TestFailedPurgeSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "miner.db")

	// Session 1: seed, fail the purge, then CLOSE the repository.
	db1 := openRawDB(t, path)
	an1, no1, wt1, _ := buildRawStores(t, db1)
	if err := an1.RecordPoints("restartfail", 100, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	if err := no1.AddPointRule(&notifications.PointRule{Streamer: "restartfail", Threshold: 10}); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	if err := wt1.RecordMinutes("restartfail", 5, time.Now()); err != nil {
		t.Fatalf("seed watch-time: %v", err)
	}
	failing1, err := streamerlifecycle.New(db1,
		[]streamerlifecycle.Purger{an1, no1, wt1, failPurger{}},
		[]streamerlifecycle.Fencer{an1, no1, wt1}, nil)
	if err != nil {
		t.Fatalf("failing coordinator: %v", err)
	}
	if _, err := failing1.Delete(ctx, "chan-restartfail", "restartfail"); err == nil {
		t.Fatal("Delete reported success despite the injected purge failure")
	}
	if has, _ := failing1.HasPending(ctx, "restartfail"); !has {
		t.Fatal("no durable marker before restart")
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close db1: %v", err)
	}

	// Session 2: reopen the SAME file (a fresh process would) and reconcile.
	db2 := openRawDB(t, path)
	defer func() { _ = db2.Close() }()
	an2, no2, wt2, coord2 := buildRawStores(t, db2)

	n, err := coord2.Reconcile(ctx)
	if err != nil {
		t.Fatalf("startup reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("startup reconciled %d unfinished deletions, want 1 (durable retry lost across restart)", n)
	}
	if data, _ := an2.GetStreamerData("restartfail"); len(data.Series) != 0 {
		t.Error("analytics rows survived restart+reconcile")
	}
	rules, _ := no2.GetPointRules()
	for _, r := range rules {
		if r.Streamer == "restartfail" {
			t.Error("point rule survived restart+reconcile")
		}
	}
	if got, _ := wt2.WindowMinutes([]string{"restartfail"}, time.Now()); got["restartfail"] != 0 {
		t.Error("watch-time survived restart+reconcile")
	}
	if has, _ := coord2.HasPending(ctx, "restartfail"); has {
		t.Error("durable marker survived a successful restart reconciliation")
	}
}

// TestReAddBeforeReconcileCannotInheritStale covers BLOCKER-1 test 9: a streamer
// re-added while a deletion is still pending has its stale rows purged FIRST, so
// it starts clean rather than inheriting old history.
func TestReAddBeforeReconcileCannotInheritStale(t *testing.T) {
	ctx := context.Background()
	s := newStores(t)
	s.seedStreamer(t, "readdstale", 100)

	failing := s.failingCoordinator(t)
	if _, err := failing.Delete(ctx, "chan-readdstale", "readdstale"); err == nil {
		t.Fatal("expected failed purge")
	}
	// Old rows remain (purge rolled back), deletion still pending.
	if !s.analyticsHas(t, "readdstale") {
		t.Fatal("setup: stale rows should remain after a failed purge")
	}

	// Re-add: reconcile the pending deletion FIRST (purges stale rows), then lift
	// the fence.
	hadPending, err := s.coord.ReconcileLogin(ctx, "readdstale")
	if err != nil {
		t.Fatalf("reconcile on re-add: %v", err)
	}
	if !hadPending {
		t.Error("expected a pending deletion to reconcile on re-add")
	}
	if s.analyticsHas(t, "readdstale") {
		t.Error("re-added streamer inherited stale analytics rows")
	}
	if s.hasPointRule(t, "readdstale") {
		t.Error("re-added streamer inherited a stale point rule")
	}
	s.coord.Reinstate("readdstale")

	// Fresh writes now start clean.
	if err := s.an.RecordPoints("readdstale", 7, "WATCH"); err != nil {
		t.Fatalf("record after clean re-add: %v", err)
	}
	if data, _ := s.an.GetStreamerData("readdstale"); len(data.Series) != 1 {
		t.Errorf("re-added streamer has %d points, want 1 (clean start)", len(data.Series))
	}
}
