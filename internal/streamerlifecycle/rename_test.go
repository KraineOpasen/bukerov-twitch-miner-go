package streamerlifecycle_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
)

// TestRenameAtomicRollback covers BLOCKER-2 tests 1,2,3,8: when any store's
// rename fails, the whole multi-store rename rolls back and EVERY store stays on
// the old login (no half-renamed state), and unrelated streamers are untouched.
func TestRenameAtomicRollback(t *testing.T) {
	s := newStores(t)
	s.seedStreamer(t, "ratomicold", 100)
	if err := s.no.SaveConfig(&notifications.NotificationConfig{OnlineStreamers: []string{"ratomicold"}}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	s.seedStreamer(t, "ratomicother", 200) // unrelated

	// A coordinator whose renamers include a failing one AFTER the real stores,
	// so the real renames run in-tx and then get rolled back.
	failing, err := streamerlifecycle.New(s.db, nil, nil,
		[]streamerlifecycle.Renamer{s.an, s.no, s.wt, failRenamer{}})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	if err := failing.RenameStreamer("ratomicold", "ratomicnew"); err == nil {
		t.Fatal("expected the atomic rename to fail")
	}

	// Every store still on the OLD login.
	if !s.analyticsHas(t, "ratomicold") || s.analyticsHas(t, "ratomicnew") {
		t.Error("analytics did not roll back to the old login")
	}
	if !s.hasPointRule(t, "ratomicold") || s.hasPointRule(t, "ratomicnew") {
		t.Error("point rule did not roll back to the old login")
	}
	if m := s.watchMinutes(t, "ratomicold"); m == 0 {
		t.Error("watch-time did not roll back to the old login")
	}
	cfg, _ := s.no.GetConfig()
	if len(cfg.OnlineStreamers) != 1 || cfg.OnlineStreamers[0] != "ratomicold" {
		t.Errorf("config list did not roll back: %v", cfg.OnlineStreamers)
	}
	// Unrelated streamer untouched.
	if !s.analyticsHas(t, "ratomicother") {
		t.Error("unrelated streamer affected by the rolled-back rename")
	}
}

// TestRenameThenDeleteRemovesAllStores covers BLOCKER-2 tests 4,5,6: a successful
// atomic rename moves every store to the new login (no old-login alias survives
// in notifications), and a later delete by the CURRENT login removes everything.
func TestRenameThenDeleteRemovesAllStores(t *testing.T) {
	ctx := context.Background()
	s := newStores(t)
	s.seedStreamer(t, "rsold", 100)
	if err := s.no.SaveConfig(&notifications.NotificationConfig{OnlineStreamers: []string{"rsold", "rsother"}}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	if err := s.coord.RenameStreamer("rsold", "rsnew"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// (4) All moved to the new login; (6) nothing under the old login.
	if s.analyticsHas(t, "rsold") || !s.analyticsHas(t, "rsnew") {
		t.Error("analytics not atomically moved to the new login")
	}
	if s.hasPointRule(t, "rsold") || !s.hasPointRule(t, "rsnew") {
		t.Error("point rule not moved (old-login alias survived)")
	}
	if s.watchMinutes(t, "rsold") != 0 || s.watchMinutes(t, "rsnew") == 0 {
		t.Error("watch-time not moved to the new login")
	}
	cfg, _ := s.no.GetConfig()
	hasOld, hasNew, hasOther := false, false, false
	for _, name := range cfg.OnlineStreamers {
		switch name {
		case "rsold":
			hasOld = true
		case "rsnew":
			hasNew = true
		case "rsother":
			hasOther = true
		}
	}
	if hasOld || !hasNew || !hasOther {
		t.Errorf("config list after rename = %v, want [rsnew rsother]", cfg.OnlineStreamers)
	}

	// (5) Delete by the current login removes everything under both logins.
	if _, err := s.coord.Delete(ctx, "chan-rs", "rsnew"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, login := range []string{"rsold", "rsnew"} {
		if s.analyticsHas(t, login) || s.hasPointRule(t, login) || s.watchMinutes(t, login) != 0 {
			t.Errorf("rows survived under %q after rename+delete", login)
		}
	}
}

// TestLoginReuseByDifferentChannelPreserved covers BLOCKER-2 test 7: after a
// channel renames its login away, a DIFFERENT channel reusing the freed login is
// unaffected when the first channel is deleted — deletion never broadly purges a
// login without going through the identity-scoped rename/delete path.
func TestLoginReuseByDifferentChannelPreserved(t *testing.T) {
	ctx := context.Background()
	s := newStores(t)

	// Channel A uses "reuse", then renames to "reusenew" (A's rows move).
	s.seedStreamer(t, "reuse", 100)
	if err := s.coord.RenameStreamer("reuse", "reusenew"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	// Channel B now registers fresh under the freed login "reuse".
	if err := s.an.RecordPoints("reuse", 999, "WATCH"); err != nil {
		t.Fatalf("seed B points: %v", err)
	}
	if err := s.no.AddPointRule(&notifications.PointRule{Streamer: "reuse", Threshold: 50}); err != nil {
		t.Fatalf("seed B rule: %v", err)
	}

	// Delete channel A by its CURRENT login. Channel B's reused-login rows survive.
	if _, err := s.coord.Delete(ctx, "chan-A", "reusenew"); err != nil {
		t.Fatalf("delete A: %v", err)
	}
	if s.analyticsHas(t, "reusenew") {
		t.Error("channel A was not deleted")
	}
	if !s.analyticsHas(t, "reuse") {
		t.Error("channel B's reused-login analytics rows were wrongly deleted")
	}
	if !s.hasPointRule(t, "reuse") {
		t.Error("channel B's reused-login point rule was wrongly deleted")
	}
}

// TestRenameFailureConsistentAcrossRestart covers BLOCKER-2 test 9: a failed
// atomic rename leaves every store on the old login (rollback), which stays
// consistent across a restart, and a later successful retry moves them all.
func TestRenameFailureConsistentAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "miner.db")

	// Session 1: seed, then a rename that fails atomically.
	db1 := openRawDB(t, path)
	an1, no1, wt1, _ := buildRawStores(t, db1)
	if err := an1.RecordPoints("rnrsold", 100, "WATCH"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := no1.AddPointRule(&notifications.PointRule{Streamer: "rnrsold", Threshold: 10}); err != nil {
		t.Fatalf("seed rule: %v", err)
	}
	if err := wt1.RecordMinutes("rnrsold", 5, time.Now()); err != nil {
		t.Fatalf("seed wt: %v", err)
	}
	failing1, err := streamerlifecycle.New(db1, nil, nil,
		[]streamerlifecycle.Renamer{an1, no1, wt1, failRenamer{}})
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	if err := failing1.RenameStreamer("rnrsold", "rnrsnew"); err == nil {
		t.Fatal("expected atomic rename to fail")
	}
	if err := db1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Session 2: reopen the SAME file; everything must still be on the OLD login.
	db2 := openRawDB(t, path)
	defer func() { _ = db2.Close() }()
	an2, no2, wt2, coord2 := buildRawStores(t, db2)

	if data, _ := an2.GetStreamerData("rnrsold"); len(data.Series) == 0 {
		t.Error("analytics not on old login after failed rename + restart")
	}
	if data, _ := an2.GetStreamerData("rnrsnew"); len(data.Series) != 0 {
		t.Error("analytics moved to new login despite failed rename")
	}
	rules, _ := no2.GetPointRules()
	onOld := false
	for _, r := range rules {
		if r.Streamer == "rnrsold" {
			onOld = true
		}
		if r.Streamer == "rnrsnew" {
			t.Error("point rule moved to new login despite failed rename")
		}
	}
	if !onOld {
		t.Error("point rule not on old login after failed rename + restart")
	}

	// A successful retry now moves every store together.
	if err := coord2.RenameStreamer("rnrsold", "rnrsnew"); err != nil {
		t.Fatalf("retry rename: %v", err)
	}
	if data, _ := an2.GetStreamerData("rnrsnew"); len(data.Series) == 0 {
		t.Error("analytics not moved after successful retry")
	}
	if got, _ := wt2.WindowMinutes([]string{"rnrsnew"}, time.Now()); got["rnrsnew"] == 0 {
		t.Error("watch-time not moved after successful retry")
	}
}
