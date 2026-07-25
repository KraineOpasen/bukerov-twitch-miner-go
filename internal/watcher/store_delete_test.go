package watcher

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// TestWatchTimeDeleteStreamerTx verifies a login's watch-time rows are removed
// while an unrelated streamer's rows survive.
func TestWatchTimeDeleteStreamerTx(t *testing.T) {
	store, sqlDB := openWatchTimeStore(t, filepath.Join(t.TempDir(), "wt.db"))
	defer func() { _ = sqlDB.Close() }()

	now := time.Now()
	if err := store.RecordMinutes("wt-victim", 12, now); err != nil {
		t.Fatalf("seed victim: %v", err)
	}
	if err := store.RecordMinutes("wt-keep", 8, now); err != nil {
		t.Fatalf("seed keep: %v", err)
	}

	tx, err := sqlDB.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	existed, err := store.DeleteStreamerTx(tx, "wt-victim")
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if !existed {
		t.Error("delete reported nothing removed")
	}

	got, err := store.WindowMinutes([]string{"wt-victim", "wt-keep"}, now)
	if err != nil {
		t.Fatalf("window: %v", err)
	}
	if _, ok := got["wt-victim"]; ok {
		t.Error("deleted streamer's watch-time rows survived")
	}
	if got["wt-keep"] == 0 {
		t.Error("unrelated streamer's watch-time was removed")
	}
}

// TestWatchTimeTombstoneBlocksRecord (D26 fence): a late watch tick for a
// streamer being deleted cannot recreate rotation-fairness rows.
func TestWatchTimeTombstoneBlocksRecord(t *testing.T) {
	store, sqlDB := openWatchTimeStore(t, filepath.Join(t.TempDir(), "wt.db"))
	defer func() { _ = sqlDB.Close() }()

	now := time.Now()
	store.Tombstone("WtFenced")
	if err := store.RecordMinutes("wtfenced", 5, now); !errors.Is(err, ErrStreamerDeleted) {
		t.Fatalf("RecordMinutes while tombstoned: got %v, want ErrStreamerDeleted", err)
	}
	got, _ := store.WindowMinutes([]string{"wtfenced"}, now)
	if _, ok := got["wtfenced"]; ok {
		t.Error("a tombstoned RecordMinutes created a row")
	}

	store.Reinstate("wtfenced")
	if err := store.RecordMinutes("wtfenced", 5, now); err != nil {
		t.Fatalf("RecordMinutes after reinstate: %v", err)
	}
}

// TestWatchTimeRenameStreamer (D15/D16): a rename repoints watch-time rows to the
// new login.
func TestWatchTimeRenameStreamer(t *testing.T) {
	store, sqlDB := openWatchTimeStore(t, filepath.Join(t.TempDir(), "wt.db"))
	defer func() { _ = sqlDB.Close() }()

	now := time.Now()
	if err := store.RecordMinutes("rnwtold", 10, now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.RenameStreamer("rnwtold", "rnwtnew"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	got, _ := store.WindowMinutes([]string{"rnwtold", "rnwtnew"}, now)
	if _, ok := got["rnwtold"]; ok {
		t.Error("watch-time still under old login after rename")
	}
	if got["rnwtnew"] != 10 {
		t.Errorf("watch-time under new login = %v, want 10", got["rnwtnew"])
	}
}
