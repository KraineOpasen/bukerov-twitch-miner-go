package analytics

import (
	"errors"
	"testing"
	"time"
)

// TestRenameStreamerPreservesHistoryContinuity (BKM-006 invariant I8/L): a
// rename must UPDATE the streamers row in place — same internal id — so
// every table keyed by streamer_id (points, annotations, bets) stays
// attached to the SAME row under the new name, instead of the rename
// starting a second, empty history.
func TestRenameStreamerPreservesHistoryContinuity(t *testing.T) {
	r := newTestRepo(t)
	old := "rename-old-continuity"
	newName := "rename-new-continuity"

	oldID, err := r.getOrCreateStreamer(old)
	if err != nil {
		t.Fatalf("seed streamer: %v", err)
	}
	seedPoint(t, r, old, time.Now().Add(-time.Hour), 1000, "WATCH")
	if err := r.RecordAnnotation(old, "WIN", "Prediction WIN", "#36b535"); err != nil {
		t.Fatalf("seed annotation: %v", err)
	}
	if err := r.RecordBet(BetRecord{
		EventID: "rename-evt-1", Streamer: old, Timestamp: time.Now().UnixMilli(),
		Strategy: "SMART", ResultType: "WIN", Placed: 100, Won: 250, Gained: 150, Odds: 2.5,
	}); err != nil {
		t.Fatalf("seed bet: %v", err)
	}

	if err := r.RenameStreamer(old, newName); err != nil {
		t.Fatalf("RenameStreamer: %v", err)
	}

	// The internal id is preserved (no new row created for newName).
	var newID int64
	if err := r.db.QueryRow("SELECT id FROM streamers WHERE name = ?", newName).Scan(&newID); err != nil {
		t.Fatalf("new row missing: %v", err)
	}
	if newID != oldID {
		t.Fatalf("rename created a NEW row (id=%d) instead of updating the existing one (id=%d): history was split", newID, oldID)
	}
	var stillOld int
	_ = r.db.QueryRow("SELECT COUNT(*) FROM streamers WHERE name = ?", old).Scan(&stillOld)
	if stillOld != 0 {
		t.Fatalf("old name row still exists after rename (stillOld=%d)", stillOld)
	}

	// Every table keyed by streamer_id is now reachable under the NEW name.
	data, err := r.GetStreamerData(newName)
	if err != nil {
		t.Fatalf("get streamer data: %v", err)
	}
	if len(data.Series) != 1 {
		t.Errorf("points series under new name = %d, want 1 (history must follow the rename)", len(data.Series))
	}
	if len(data.Annotations) != 1 {
		t.Errorf("annotations under new name = %d, want 1", len(data.Annotations))
	}
	bets, err := r.GetBets(newName, "", time.Time{}, time.Time{})
	if err != nil || len(bets) != 1 {
		t.Errorf("bets under new name = %d (err=%v), want 1", len(bets), err)
	}

	// The old name no longer resolves to anything.
	oldData, err := r.GetStreamerData(old)
	if err != nil {
		t.Fatalf("get old streamer data: %v", err)
	}
	if len(oldData.Series) != 0 || len(oldData.Annotations) != 0 {
		t.Errorf("old name still carries history after rename: %+v", oldData)
	}
}

// TestRenameStreamerIdempotentWhenOldMissing (I9: repeated apply is a no-op):
// renaming a login with no recorded history is a no-op, not an error — a
// repeated identical settings apply (or a rename of a streamer that never
// recorded analytics data) must never fail.
func TestRenameStreamerIdempotentWhenOldMissing(t *testing.T) {
	r := newTestRepo(t)
	if err := r.RenameStreamer("rename-never-existed-old", "rename-never-existed-new"); err != nil {
		t.Fatalf("no-op rename must not error: %v", err)
	}
	var n int
	_ = r.db.QueryRow("SELECT COUNT(*) FROM streamers WHERE name = ?", "rename-never-existed-new").Scan(&n)
	if n != 0 {
		t.Fatalf("no-op rename must not fabricate a row, found %d", n)
	}
}

// TestRenameStreamerSameNameIsNoOp: renaming a login to itself (can happen if
// callers stop de-duplicating upstream) must not touch the database.
func TestRenameStreamerSameNameIsNoOp(t *testing.T) {
	r := newTestRepo(t)
	name := "rename-same-name"
	if _, err := r.getOrCreateStreamer(name); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := r.RenameStreamer(name, name); err != nil {
		t.Fatalf("same-name rename must not error: %v", err)
	}
}

// TestRenameStreamerFailsClosedOnCollision (I11/L: fail-closed collision):
// when BOTH the old and new login already have independently recorded
// history, RenameStreamer must refuse to silently merge them — a typed
// conflict error, and NEITHER row is touched.
func TestRenameStreamerFailsClosedOnCollision(t *testing.T) {
	r := newTestRepo(t)
	old := "rename-collision-old"
	newName := "rename-collision-new"

	oldID, err := r.getOrCreateStreamer(old)
	if err != nil {
		t.Fatalf("seed old: %v", err)
	}
	newID, err := r.getOrCreateStreamer(newName)
	if err != nil {
		t.Fatalf("seed new: %v", err)
	}
	seedPoint(t, r, old, time.Now(), 500, "WATCH")
	seedPoint(t, r, newName, time.Now(), 700, "WATCH")

	err = r.RenameStreamer(old, newName)
	if err == nil {
		t.Fatal("collision must return an error, got nil")
	}
	var conflict *StreamerRenameConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error type = %T, want *StreamerRenameConflictError", err)
	}
	if conflict.OldName != old || conflict.NewName != newName {
		t.Errorf("conflict fields = %+v, want old=%q new=%q", conflict, old, newName)
	}

	// Fail closed: neither row was mutated, both ids and both histories
	// remain exactly as they were.
	var checkOldID, checkNewID int64
	if err := r.db.QueryRow("SELECT id FROM streamers WHERE name = ?", old).Scan(&checkOldID); err != nil {
		t.Fatalf("old row missing after failed rename: %v", err)
	}
	if err := r.db.QueryRow("SELECT id FROM streamers WHERE name = ?", newName).Scan(&checkNewID); err != nil {
		t.Fatalf("new row missing after failed rename: %v", err)
	}
	if checkOldID != oldID || checkNewID != newID {
		t.Fatalf("row ids changed by a failed rename: old %d->%d new %d->%d", oldID, checkOldID, newID, checkNewID)
	}

	oldData, _ := r.GetStreamerData(old)
	newData, _ := r.GetStreamerData(newName)
	if len(oldData.Series) != 1 || len(newData.Series) != 1 {
		t.Fatalf("failed rename must not move any data: old=%d new=%d points, want 1/1",
			len(oldData.Series), len(newData.Series))
	}
}

// TestServiceRenameStreamerLowercases proves the Service wrapper normalizes
// names the same way every other analytics write path does (streamer.Manager
// always deals in lowercase logins), so a rename request carrying whatever
// case the config/DTO happens to have still lands on the SAME row.
func TestServiceRenameStreamerLowercases(t *testing.T) {
	r := newTestRepo(t)
	svc := &Service{repo: r, now: time.Now}

	old := "rename-svc-old"
	if _, err := r.getOrCreateStreamer(old); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := svc.RenameStreamer("RENAME-SVC-OLD", "Rename-Svc-New"); err != nil {
		t.Fatalf("RenameStreamer: %v", err)
	}
	var n int
	_ = r.db.QueryRow("SELECT COUNT(*) FROM streamers WHERE name = ?", "rename-svc-new").Scan(&n)
	if n != 1 {
		t.Fatalf("lowercased destination row missing, count=%d", n)
	}
}
