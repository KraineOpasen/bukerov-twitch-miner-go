package miner

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
)

// Feature: Fail-closed streamer deletion (SRAP, M1).
//
// This file pins Scenarios S3-S6 (the ones that need the real applySettings
// state machine: coordinatorMu, the admission/purge budgets, the runtime
// roster) plus the remaining M1 matrix items (mandate §16) not already
// covered by cd5c9c1's own internal/miner/srap_test.go: concurrent applies,
// apply-vs-shutdown draining, and apply-then-re-add. Scenarios S1/S2/S7 —
// the ones best pinned at the HTTP contract boundary — live in
// internal/web/acceptance_m1_test.go. All fixture logins are unique to this
// file (the process-wide database.Open singleton hazard, see
// internal/miner/database_singleton_test.go's TestMain) and every raw-DB
// test opens its OWN private handle (openRawMinerDB, from srap_test.go) so
// closing it cannot break any other test in this binary.

// TestAcceptanceClientDisconnectsAfterCommitPoint is Scenario S3.
//
// Given a tracked streamer whose removal is about to be durably admitted and
// committed,
// When the client's request context is cancelled at EXACTLY the post-commit
// purge boundary — deterministically, via a Fencer whose Tombstone (the
// first thing SRAP's COMPLETE phase does, called strictly after config.json
// has already been persisted and the runtime roster already committed)
// cancels the SAME context the caller originally passed to applySettings —
// Then the bounded critical sequence (critB, derived via
// context.WithoutCancel before this cancellation ever happens) still runs to
// completion: the apply returns success, the streamer stays removed, and its
// persisted history is fully purged — the deletion is not lost, and no
// partial/untracked state remains.
func TestAcceptanceClientDisconnectsAfterCommitPoint(t *testing.T) {
	const victim, keep = "s3victim", "s3keep"
	m, _, _ := newCapabilityMiner(t, victim, keep)

	db := openRawMinerDB(t, filepath.Join(t.TempDir(), "miner.db"))
	an, err := analytics.NewSQLiteRepository(db, t.TempDir())
	if err != nil {
		t.Fatalf("analytics repo: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	fired := false
	fencer := cancelOnTombstoneFencer{inner: an, cancel: cancel, fired: &fired}
	coord, err := streamerlifecycle.New(db, []streamerlifecycle.Purger{an}, []streamerlifecycle.Fencer{fencer}, nil)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	m.db = db
	m.streamerLifecycle = coord

	configPath := filepath.Join(t.TempDir(), "config.json")
	m.configPath = configPath
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := an.RecordPoints(victim, 300, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}

	rs := removeStreamer(m.GetRuntimeSettings(), victim)
	if err := m.applySettings(ctx, rs); err != nil {
		t.Fatalf("apply must complete despite a client disconnect strictly AFTER the commit point: %v", err)
	}
	if !fired {
		t.Fatal("setup: the cancellation hook never fired; this test proves nothing")
	}
	if ctx.Err() == nil {
		t.Fatal("setup: the request ctx must actually be cancelled by the time applySettings returns")
	}

	if m.streamers.Get(victim) != nil {
		t.Error("victim still present in the runtime despite a completed removal")
	}
	if has, err := coord.HasPending(context.Background(), victim); err != nil || has {
		t.Errorf("HasPending = (%v, %v), want (false, nil) — the purge completed, nothing should remain owed", has, err)
	}
	if data, _ := an.GetStreamerData(victim); len(data.Series) != 0 {
		t.Error("victim's history survived despite a successful post-commit purge")
	}
}

// cancelOnTombstoneFencer wraps a real Fencer and cancels a caller-supplied
// context the first time Tombstone is invoked — the deterministic seam used
// to model "the client disconnected at exactly the post-commit purge
// boundary" without a sleep: Tombstone is the very first action of SRAP's
// COMPLETE phase (CommitRemoval), called synchronously and strictly after
// config.json/the runtime roster have already committed.
type cancelOnTombstoneFencer struct {
	inner  streamerlifecycle.Fencer
	cancel context.CancelFunc
	fired  *bool
}

func (f cancelOnTombstoneFencer) Tombstone(login string) {
	if !*f.fired {
		*f.fired = true
		f.cancel()
	}
	f.inner.Tombstone(login)
}

func (f cancelOnTombstoneFencer) Reinstate(login string) { f.inner.Reinstate(login) }

// TestAcceptancePurgeFailsAfterDurableAdmission is Scenario S4.
//
// Given a streamer whose removal has been durably admitted AND committed
// (config.json persisted, runtime roster updated),
// When the purge step itself fails (an injected purger failure),
// Then the streamer stays removed (the user's intent is fully durable), the
// pending-purge row remains, the API/log do not claim a full purge — and,
// crucially, restart reconciliation (simulated by closing and reopening the
// same SQLite file, then running the equivalent of
// reconcilePendingStreamerDeletions with a HEALTHY coordinator) retries and
// completes the owed purge.
func TestAcceptancePurgeFailsAfterDurableAdmission(t *testing.T) {
	const victim, keep = "s4victim", "s4keep"
	m, _, _ := newCapabilityMiner(t, victim, keep)

	dbPath := filepath.Join(t.TempDir(), "miner.db")
	db := openRawMinerDB(t, dbPath)
	an, err := analytics.NewSQLiteRepository(db, t.TempDir())
	if err != nil {
		t.Fatalf("analytics repo: %v", err)
	}
	coord, err := streamerlifecycle.New(db,
		[]streamerlifecycle.Purger{an, failingPurger{}},
		[]streamerlifecycle.Fencer{an},
		nil,
	)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	m.db = db
	m.streamerLifecycle = coord

	configPath := filepath.Join(t.TempDir(), "config.json")
	m.configPath = configPath
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := an.RecordPoints(victim, 400, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}

	rs := removeStreamer(m.GetRuntimeSettings(), victim)
	if err := m.applySettings(context.Background(), rs); err != nil {
		t.Fatalf("a post-commit purge failure must NOT fail the apply: %v", err)
	}

	// Then: removed from runtime/config; pending row remains; history intact
	// (purge tx rolled back).
	if m.streamers.Get(victim) != nil {
		t.Error("victim still present in the runtime despite a committed removal")
	}
	if has, err := coord.HasPending(context.Background(), victim); err != nil || !has {
		t.Errorf("HasPending = (%v, %v), want (true, nil) — the failed purge must leave a durable row", has, err)
	}
	if data, _ := an.GetStreamerData(victim); len(data.Series) == 0 {
		t.Error("victim's history was purged despite the injected purge failure")
	}

	// Restart: close the process' handle, reopen a fresh one over the SAME
	// file, and build a HEALTHY coordinator (no injected failure) exactly as
	// a real restart's buildStreamerLifecycle would.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	db2 := openRawMinerDB(t, dbPath)
	defer func() { _ = db2.Close() }()
	an2, err := analytics.NewSQLiteRepository(db2, t.TempDir())
	if err != nil {
		t.Fatalf("reopen analytics: %v", err)
	}
	healthyCoord, err := streamerlifecycle.New(db2, []streamerlifecycle.Purger{an2}, []streamerlifecycle.Fencer{an2}, nil)
	if err != nil {
		t.Fatalf("healthy coordinator: %v", err)
	}

	m2 := &Miner{config: m.config, streamerLifecycle: healthyCoord}
	m2.reconcilePendingStreamerDeletions()

	if has, err := healthyCoord.HasPending(context.Background(), victim); err != nil || has {
		t.Errorf("HasPending after restart reconciliation = (%v, %v), want (false, nil) — the owed purge must complete", has, err)
	}
	if data, _ := an2.GetStreamerData(victim); len(data.Series) != 0 {
		t.Error("victim's history survived restart reconciliation")
	}
}

// TestAcceptanceMultipleRemovalsAreAtomic is Scenario S5.
//
// Given three tracked streamers,
// When they are all removed in ONE settings update, and durable admission
// fails for the operation (SQLite closed before the admission INSERT),
// Then none of the three removals commit — the runtime roster is untouched
// for all three — and no partial admission/pending row or resurrection
// fence remains for ANY of them.
func TestAcceptanceMultipleRemovalsAreAtomic(t *testing.T) {
	const victim1, victim2, victim3, keep = "s5victim1", "s5victim2", "s5victim3", "s5keep"
	m, _, _ := newCapabilityMiner(t, victim1, victim2, victim3, keep)

	dbPath := filepath.Join(t.TempDir(), "miner.db")
	db := openRawMinerDB(t, dbPath)
	an, err := analytics.NewSQLiteRepository(db, t.TempDir())
	if err != nil {
		t.Fatalf("analytics repo: %v", err)
	}
	for _, v := range []string{victim1, victim2, victim3} {
		if err := an.RecordPoints(v, 100, "WATCH"); err != nil {
			t.Fatalf("seed points %s: %v", v, err)
		}
	}
	coord, err := streamerlifecycle.New(db, []streamerlifecycle.Purger{an}, []streamerlifecycle.Fencer{an}, nil)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	m.db = db
	m.streamerLifecycle = coord

	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	rs := m.GetRuntimeSettings()
	var keepOnly []settings.StreamerConfig
	for _, sc := range rs.Streamers {
		if sc.Username == keep {
			keepOnly = append(keepOnly, sc)
		}
	}
	rs.Streamers = keepOnly // removes all three victims in one apply

	if err := m.applySettings(context.Background(), rs); err == nil {
		t.Fatal("expected the batch admission to fail")
	}

	for _, v := range []string{victim1, victim2, victim3} {
		if m.streamers.Get(v) == nil {
			t.Errorf("%s removed from the runtime despite the batch admission failing", v)
		}
	}
	if m.streamers.Get(keep) == nil {
		t.Error("unrelated streamer lost from the runtime")
	}

	// No partial admission/pending row or fence for ANY of the three,
	// verified against a freshly reopened handle to the same file.
	reopened := openRawMinerDB(t, dbPath)
	defer func() { _ = reopened.Close() }()
	var admissions, pending int
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM streamer_deletion_admissions`).Scan(&admissions); err != nil {
		t.Fatalf("count admissions: %v", err)
	}
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM pending_streamer_deletions`).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if admissions != 0 || pending != 0 {
		t.Errorf("admissions=%d pending=%d, want 0,0 for a wholly-failed batch admission", admissions, pending)
	}
	reopenedAn, err := analytics.NewSQLiteRepository(reopened, t.TempDir())
	if err != nil {
		t.Fatalf("reopen analytics: %v", err)
	}
	for _, v := range []string{victim1, victim2, victim3} {
		if writeErr := reopenedAn.RecordPoints(v, 1, "WATCH"); errors.Is(writeErr, analytics.ErrStreamerDeleted) {
			t.Errorf("%s was fenced despite the batch admission never committing", v)
		}
	}
}

// TestAcceptanceRestartAfterEveryTransactionBoundary is Scenario S6: the
// process stops at each of three defined crash points (M1 design manifest
// §7's crash matrix), and startup arbitration/reconciliation must resolve
// each one correctly — a committed deletion completes, an uncommitted one
// rolls back — with no streamer ever resurrected or accidentally deleted.
// Each subtest constructs its crash point directly with raw SQL (bypassing
// AdmitRemovals/CommitRemoval, which would just reproduce the already-tested
// happy path) plus an in-memory config, mirroring exactly what a real
// restart's reconcilePendingStreamerDeletions sees.
func TestAcceptanceRestartAfterEveryTransactionBoundary(t *testing.T) {
	t.Run("C3_admission_committed_pre_config_rollback", func(t *testing.T) {
		// Given: an admissions row exists (AdmitRemovals committed) but
		// config.json was NEVER updated (the crash happened before
		// SaveConfig) — the streamer is STILL configured under the SAME
		// channel ID the row names.
		const victim = "c3victim"
		db := openRawMinerDB(t, filepath.Join(t.TempDir(), "miner.db"))
		an, err := analytics.NewSQLiteRepository(db, t.TempDir())
		if err != nil {
			t.Fatalf("analytics: %v", err)
		}
		coord, err := streamerlifecycle.New(db, []streamerlifecycle.Purger{an}, []streamerlifecycle.Fencer{an}, nil)
		if err != nil {
			t.Fatalf("coordinator: %v", err)
		}
		if err := an.RecordPoints(victim, 100, "WATCH"); err != nil {
			t.Fatalf("seed points: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO streamer_deletion_admissions (login, channel_id, requested_at) VALUES (?, ?, ?)`,
			victim, "chan-c3", 1); err != nil {
			t.Fatalf("seed admission row: %v", err)
		}

		m := &Miner{
			config:            &config.Config{Streamers: []config.StreamerConfig{{Username: victim, ChannelID: "chan-c3"}}},
			streamerLifecycle: coord,
		}
		m.reconcilePendingStreamerDeletions()

		var admissions, pending int
		if err := db.QueryRow(`SELECT COUNT(*) FROM streamer_deletion_admissions`).Scan(&admissions); err != nil {
			t.Fatalf("count admissions: %v", err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM pending_streamer_deletions`).Scan(&pending); err != nil {
			t.Fatalf("count pending: %v", err)
		}
		if admissions != 0 {
			t.Errorf("admissions=%d, want 0 (an uncommitted removal must be aborted at restart)", admissions)
		}
		if pending != 0 {
			t.Errorf("pending=%d, want 0 (a still-configured login must never be promoted to purge)", pending)
		}
		if data, _ := an.GetStreamerData(victim); len(data.Series) == 0 {
			t.Error("still-configured streamer's history was purged — resurrection-safety violated in the wrong direction (accidental delete)")
		}
	})

	t.Run("C5_config_committed_pre_runtime_roll_forward", func(t *testing.T) {
		// Given: an admissions row exists AND config.json is ALREADY
		// post-removal (SaveConfig committed before the crash) — the
		// streamer is no longer configured at all.
		const victim = "c5victim"
		db := openRawMinerDB(t, filepath.Join(t.TempDir(), "miner.db"))
		an, err := analytics.NewSQLiteRepository(db, t.TempDir())
		if err != nil {
			t.Fatalf("analytics: %v", err)
		}
		coord, err := streamerlifecycle.New(db, []streamerlifecycle.Purger{an}, []streamerlifecycle.Fencer{an}, nil)
		if err != nil {
			t.Fatalf("coordinator: %v", err)
		}
		if err := an.RecordPoints(victim, 100, "WATCH"); err != nil {
			t.Fatalf("seed points: %v", err)
		}
		if _, err := db.Exec(`INSERT INTO streamer_deletion_admissions (login, channel_id, requested_at) VALUES (?, ?, ?)`,
			victim, "chan-c5", 1); err != nil {
			t.Fatalf("seed admission row: %v", err)
		}

		m := &Miner{
			config:            &config.Config{}, // victim absent: the commit already happened
			streamerLifecycle: coord,
		}
		m.reconcilePendingStreamerDeletions()

		var admissions, pending int
		if err := db.QueryRow(`SELECT COUNT(*) FROM streamer_deletion_admissions`).Scan(&admissions); err != nil {
			t.Fatalf("count admissions: %v", err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM pending_streamer_deletions`).Scan(&pending); err != nil {
			t.Fatalf("count pending: %v", err)
		}
		if admissions != 0 || pending != 0 {
			t.Errorf("admissions=%d pending=%d, want 0,0 (promoted AND reconciled in the same startup pass)", admissions, pending)
		}
		if data, _ := an.GetStreamerData(victim); len(data.Series) != 0 {
			t.Error("a committed-but-not-yet-purged deletion was not completed at restart")
		}
	})

	t.Run("C8_purge_failed_retried_on_restart", func(t *testing.T) {
		// Given: a genuine committed removal (real AdmitRemovals+CommitRemoval
		// through a failing purger, reaching the pending table exactly as a
		// crash mid purge-tx would leave it), config.json already
		// post-removal.
		const victim = "c8victim"
		dbPath := filepath.Join(t.TempDir(), "miner.db")
		db := openRawMinerDB(t, dbPath)
		an, err := analytics.NewSQLiteRepository(db, t.TempDir())
		if err != nil {
			t.Fatalf("analytics: %v", err)
		}
		failing, err := streamerlifecycle.New(db, []streamerlifecycle.Purger{an, failingPurger{}}, []streamerlifecycle.Fencer{an}, nil)
		if err != nil {
			t.Fatalf("failing coordinator: %v", err)
		}
		if err := an.RecordPoints(victim, 100, "WATCH"); err != nil {
			t.Fatalf("seed points: %v", err)
		}
		if err := failing.AdmitRemovals(context.Background(), []streamerlifecycle.Removal{{ChannelID: "chan-c8", Login: victim}}); err != nil {
			t.Fatalf("admit: %v", err)
		}
		if _, err := failing.CommitRemoval(context.Background(), "chan-c8", victim); err == nil {
			t.Fatal("expected the injected purge failure")
		}
		if err := db.Close(); err != nil {
			t.Fatalf("close db (simulated crash): %v", err)
		}

		db2 := openRawMinerDB(t, dbPath)
		defer func() { _ = db2.Close() }()
		an2, err := analytics.NewSQLiteRepository(db2, t.TempDir())
		if err != nil {
			t.Fatalf("reopen analytics: %v", err)
		}
		healthyCoord, err := streamerlifecycle.New(db2, []streamerlifecycle.Purger{an2}, []streamerlifecycle.Fencer{an2}, nil)
		if err != nil {
			t.Fatalf("healthy coordinator: %v", err)
		}

		m := &Miner{
			config:            &config.Config{}, // victim absent: already committed
			streamerLifecycle: healthyCoord,
		}
		m.reconcilePendingStreamerDeletions()

		if has, err := healthyCoord.HasPending(context.Background(), victim); err != nil || has {
			t.Errorf("HasPending after restart = (%v, %v), want (false, nil) — the retried purge must complete", has, err)
		}
		if data, _ := an2.GetStreamerData(victim); len(data.Series) != 0 {
			t.Error("victim's history survived a restart retry of the owed purge")
		}
	})
}

// TestConcurrentSettingsApplySerializesConsistentFinalState (M1 matrix §16:
// two concurrent settings POSTs) proves coordinatorMu genuinely SERIALIZES
// two concurrent applySettings calls into one consistent final state — never
// a torn/interleaved commit (a partial config write racing a partial runtime
// commit, or a leaked durable ledger row).
//
// It deliberately does NOT assert that both removals survive: the settings
// API is a full-roster REPLACE (like any stale-read REST PUT), a property
// that predates M1 and is orthogonal to it — two concurrent requests each
// built from a snapshot that does not yet reflect the other's removal will
// have the later one's snapshot silently "resurrect" (re-add) whatever the
// earlier one removed, exactly as CommitPlan's own added/removed diff is
// designed to do for ANY stale full-roster snapshot. What coordinatorMu
// guarantees — and what this test pins — is that this resolves to ONE clean,
// fully-committed state (config and runtime always agree, no leaked
// admission/pending row, no error from either call), never a corrupted
// halfway state from the two applies interleaving.
func TestConcurrentSettingsApplySerializesConsistentFinalState(t *testing.T) {
	const victimA, victimB, keep = "concA", "concB", "conckeep"
	m, _, _ := newCapabilityMiner(t, victimA, victimB, keep)
	wireDeletionStores(t, m)

	configPath := filepath.Join(t.TempDir(), "config.json")
	m.configPath = configPath
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		rs := removeStreamer(m.GetRuntimeSettings(), victimA)
		errs[0] = m.applySettings(context.Background(), rs)
	}()
	go func() {
		defer wg.Done()
		<-start
		rs := removeStreamer(m.GetRuntimeSettings(), victimB)
		errs[1] = m.applySettings(context.Background(), rs)
	}()
	close(start)
	wg.Wait()

	if errs[0] != nil {
		t.Errorf("removal A failed: %v", errs[0])
	}
	if errs[1] != nil {
		t.Errorf("removal B failed: %v", errs[1])
	}
	if m.streamers.Get(keep) == nil {
		t.Error("unrelated streamer lost from the runtime")
	}

	// The final on-disk config must agree EXACTLY with the final runtime
	// roster — proof that coordinatorMu serialized the two SaveConfig+
	// CommitPlan pairs rather than letting them interleave into a torn state.
	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("load final config: %v", err)
	}
	// Compared case-insensitively: the runtime roster canonicalizes logins to
	// lowercase (streamer.Manager) while config.json preserves the posted
	// case verbatim — an unrelated, pre-existing normalization difference,
	// not a coordinatorMu consistency issue.
	diskUsernames := make(map[string]bool, len(onDisk.Streamers))
	for _, sc := range onDisk.Streamers {
		diskUsernames[strings.ToLower(sc.Username)] = true
	}
	runtimeUsernames := make(map[string]bool)
	for _, s := range m.streamers.All() {
		runtimeUsernames[strings.ToLower(s.GetUsername())] = true
	}
	if !reflect.DeepEqual(diskUsernames, runtimeUsernames) {
		t.Errorf("on-disk config %v and runtime roster %v diverged after two concurrent applies", diskUsernames, runtimeUsernames)
	}

	// No durable ledger row leaks regardless of which removal "won".
	var admissions, pending int
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM streamer_deletion_admissions WHERE login IN (?, ?)`, victimA, victimB).Scan(&admissions); err != nil {
		t.Fatalf("count admissions: %v", err)
	}
	if err := m.db.QueryRow(`SELECT COUNT(*) FROM pending_streamer_deletions WHERE login IN (?, ?)`, victimA, victimB).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if admissions != 0 || pending != 0 {
		t.Errorf("admissions=%d pending=%d, want 0,0 — no durable row may outlive two successfully serialized applies", admissions, pending)
	}
}

// TestSettingsApplyVsShutdownDrainWaitsForInFlightApply (M1 matrix §16:
// settings apply vs shutdown) pins beginApply/applyDraining/applyWG — the
// exact interlock stop() uses (see miner.go's stop(), which this test does
// NOT call directly: newCapabilityMiner's fixture has no chatManager/
// wsPool/watcher/etc, and stop() would nil-panic tearing them down; the
// interlock itself is the thing under test, not stop()'s full teardown
// sequence). A Fencer hook blocks an in-flight apply exactly at its
// post-commit purge step; while blocked, this test proves (a) draining
// (applyWG.Wait()) cannot return until that apply finishes, and (b) a NEW
// apply started after draining begins is refused immediately, even though
// the first apply is still running. Synchronized purely with channels.
func TestSettingsApplyVsShutdownDrainWaitsForInFlightApply(t *testing.T) {
	const victim, keep = "shutdownvictim", "shutdownkeep"
	m, _, _ := newCapabilityMiner(t, victim, keep)

	db := openRawMinerDB(t, filepath.Join(t.TempDir(), "miner.db"))
	an, err := analytics.NewSQLiteRepository(db, t.TempDir())
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	reached := make(chan struct{})
	release := make(chan struct{})
	coord, err := streamerlifecycle.New(db, []streamerlifecycle.Purger{an}, []streamerlifecycle.Fencer{blockingFencer{inner: an, reached: reached, release: release}}, nil)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	m.db = db
	m.streamerLifecycle = coord

	configPath := filepath.Join(t.TempDir(), "config.json")
	m.configPath = configPath
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	if err := an.RecordPoints(victim, 200, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}

	applyDone := make(chan error, 1)
	go func() {
		rs := removeStreamer(m.GetRuntimeSettings(), victim)
		applyDone <- m.applySettings(context.Background(), rs)
	}()

	<-reached // the in-flight apply is now past its commit point, blocked in Tombstone

	drainStarted := make(chan struct{})
	drainDone := make(chan struct{})
	go func() {
		m.applyMu.Lock()
		m.applyDraining = true
		m.applyMu.Unlock()
		close(drainStarted) // applyDraining is now visibly set — safe to probe beginApply()
		m.applyWG.Wait()
		close(drainDone)
	}()
	<-drainStarted

	// drainDone CANNOT have fired: the in-flight apply is still registered in
	// applyWG (it has not returned — it is blocked on <-release), so
	// applyWG.Wait() cannot return regardless of scheduling. Not a timing
	// race: a nonzero WaitGroup counter makes this deterministic.
	select {
	case <-drainDone:
		t.Fatal("draining (applyWG.Wait) returned before the in-flight apply finished")
	default:
	}

	// A NEW apply started once applyDraining is set must be refused
	// immediately by beginApply() itself — BEFORE ever touching
	// coordinatorMu (which the first, still-in-flight apply holds), or this
	// call would deadlock instead of returning ErrShuttingDown.
	if err := m.applySettings(context.Background(), m.GetRuntimeSettings()); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("new apply during draining = %v, want ErrShuttingDown", err)
	}

	close(release) // let the blocked apply finish
	if err := <-applyDone; err != nil {
		t.Fatalf("in-flight apply failed: %v", err)
	}
	<-drainDone // must now complete promptly
}

// blockingFencer wraps a real Fencer, closes reached the first time
// Tombstone is called (signalling "the apply is now past its commit point,
// deep inside the purge step"), then blocks until release is closed.
type blockingFencer struct {
	inner   streamerlifecycle.Fencer
	reached chan struct{}
	release chan struct{}
}

func (f blockingFencer) Tombstone(login string) {
	close(f.reached)
	<-f.release
	f.inner.Tombstone(login)
}

func (f blockingFencer) Reinstate(login string) { f.inner.Reinstate(login) }

// TestSettingsApplyThenReAddReconcilesOwedPurge (M1 matrix §16: settings
// apply vs re-add) covers the sequential case: a removal whose purge fails
// leaves an owed purge; a LATER apply that re-adds the same login must
// reconcile (purge the stale rows) BEFORE the re-added streamer can record
// anything fresh, via applyStreamerDeletions' ReconcileLogin call for added
// streamers.
func TestSettingsApplyThenReAddReconcilesOwedPurge(t *testing.T) {
	const victim, keep = "readdapply", "readdapplykeep"
	m, _, _ := newCapabilityMiner(t, victim, keep)

	db, err := database.Open(t.TempDir()) // package-wide singleton; unique login avoids collision
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("analytics service: %v", err)
	}
	m.db = db
	m.analyticsSvc = svc

	coord, err := streamerlifecycle.New(db, []streamerlifecycle.Purger{svc.Repository(), failingPurger{}}, []streamerlifecycle.Fencer{svc.Repository()}, nil)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	m.streamerLifecycle = coord

	if err := svc.Repository().RecordPoints(victim, 111, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	if err := m.ApplySettings(context.Background(), m.GetRuntimeSettings()); err != nil {
		t.Fatalf("baseline apply: %v", err)
	}

	// Step 1: remove victim. The purge fails (injected), leaving an owed
	// purge, but the apply itself succeeds (committed, purge pending).
	rs := removeStreamer(m.GetRuntimeSettings(), victim)
	if err := m.applySettings(context.Background(), rs); err != nil {
		t.Fatalf("removal apply (purge-pending) must not fail: %v", err)
	}
	if has, err := coord.HasPending(context.Background(), victim); err != nil || !has {
		t.Fatalf("HasPending after the failed purge = (%v, %v), want (true, nil)", has, err)
	}

	// Step 2: re-add the SAME login in a LATER apply. ReconcileLogin must
	// purge the stale rows first (this apply's coordinator has no injected
	// failure on its own call path for the re-add's ReconcileLogin, since
	// failingPurger is still wired — so the re-add purge itself will ALSO
	// fail here on purpose, keeping the streamer fenced; verifying it stays
	// inert rather than silently inheriting the stale history is exactly
	// the property under test).
	rs2 := m.GetRuntimeSettings()
	rs2.Streamers = append(rs2.Streamers, settings.StreamerConfig{Username: victim})
	if err := m.applySettings(context.Background(), rs2); err != nil {
		t.Fatalf("re-add apply: %v", err)
	}

	// The re-added streamer must still be fenced (purge retried and failed
	// again) rather than able to record fresh points over stale history.
	if writeErr := svc.Repository().RecordPoints(victim, 5, "WATCH"); !errors.Is(writeErr, analytics.ErrStreamerDeleted) {
		t.Errorf("re-added streamer with an unresolved owed purge accepted a write (err=%v), want ErrStreamerDeleted (stays fenced)", writeErr)
	}

	// Once the purge finally succeeds (drop the failing purger, as a
	// restart's healthy coordinator would), the SAME retry path a real
	// restart uses (reconcilePendingStreamerDeletions — the re-add apply
	// above already tried once via ReconcileLogin and failed again, exactly
	// as production leaves it: fenced, durably queued for the next
	// startup) clears the owed purge and the streamer starts clean.
	m.streamerLifecycle = nil // detach the always-failing coordinator
	healthyCoord, err := streamerlifecycle.New(db, []streamerlifecycle.Purger{svc.Repository()}, []streamerlifecycle.Fencer{svc.Repository()}, nil)
	if err != nil {
		t.Fatalf("healthy coordinator: %v", err)
	}
	m.streamerLifecycle = healthyCoord
	m.reconcilePendingStreamerDeletions()
	if has, err := healthyCoord.HasPending(context.Background(), victim); err != nil || has {
		t.Errorf("HasPending after a healthy re-add reconcile = (%v, %v), want (false, nil)", has, err)
	}
	if writeErr := svc.Repository().RecordPoints(victim, 9, "WATCH"); writeErr != nil {
		t.Errorf("re-added streamer still fenced after a successful reconcile: %v", writeErr)
	}
	if data, _ := svc.Repository().GetStreamerData(victim); len(data.Series) != 1 {
		t.Errorf("re-added streamer has %d points, want exactly 1 (clean start, stale history purged)", len(data.Series))
	}
}
