package miner

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// openRawMinerDB opens a PRIVATE, non-singleton database over its own file
// (mirroring internal/streamerlifecycle/durable_test.go's openRawDB): unlike
// database.Open, this never touches the package-wide singleton every other
// test in this binary shares, so a test that deliberately CLOSES its handle
// (to force a durable-admission failure) cannot break every other test.
func openRawMinerDB(t *testing.T, path string) *database.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	return &database.DB{DB: sqlDB}
}

// wireRawDeletionStores is wireDeletionStores' twin over a caller-supplied
// (private) db, for tests that need to close the handle afterward.
func wireRawDeletionStores(t *testing.T, m *Miner, db *database.DB) (*analytics.Service, *watcher.WatchTimeStore) {
	t.Helper()
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("analytics service: %v", err)
	}
	wt, err := watcher.NewWatchTimeStore(db)
	if err != nil {
		t.Fatalf("watch-time store: %v", err)
	}
	m.db = db
	m.analyticsSvc = svc
	m.watchTimeStore = wt
	m.buildStreamerLifecycle()
	if m.streamerLifecycle == nil {
		t.Fatal("streamer lifecycle coordinator was not built")
	}
	return svc, wt
}

// removeStreamer returns rs with victim's entry dropped — the same
// full-body-minus-one-streamer shape the Settings page posts for a deletion.
func removeStreamer(rs settings.RuntimeSettings, victim string) settings.RuntimeSettings {
	var kept []settings.StreamerConfig
	for _, sc := range rs.Streamers {
		if sc.Username != victim {
			kept = append(kept, sc)
		}
	}
	rs.Streamers = kept
	return rs
}

// TestApplySettingsAdmissionFailureClosedDBZeroMutation pins the M1 core
// invariant for a pre-commit durable-admission failure: with the database
// closed (so AdmitRemovals cannot possibly succeed — a cancelled critA is
// structurally impossible, since critA is WithoutCancel by design), the
// apply must fail AND leave absolutely nothing changed — not the runtime
// roster, not the in-memory config, not config.json on disk, not any
// persisted history, and (verified by reopening the same file) not a single
// row in either SRAP ledger.
func TestApplySettingsAdmissionFailureClosedDBZeroMutation(t *testing.T) {
	const victim, keep = "admfailvictim", "admfailkeep"
	m, _, _ := newCapabilityMiner(t, victim, keep)

	dbPath := filepath.Join(t.TempDir(), "miner.db")
	db := openRawMinerDB(t, dbPath)
	svc, wt := wireRawDeletionStores(t, m, db)

	configPath := filepath.Join(t.TempDir(), "config.json")
	m.configPath = configPath
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config file: %v", err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read seeded config file: %v", err)
	}

	if err := svc.Repository().RecordPoints(victim, 500, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	if err := wt.RecordMinutes(victim, 10, time.Now()); err != nil {
		t.Fatalf("seed watch-time: %v", err)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// Capture every log emitted during the failed apply: a pre-commit
	// admission failure must never claim a removal was durably queued or
	// committed — no durable row was ever written, so either claim would be
	// false.
	cap := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(prev)

	rs := removeStreamer(m.GetRuntimeSettings(), victim)
	err = m.applySettings(context.Background(), rs)
	if err == nil {
		t.Fatal("expected applySettings to fail when the database is closed")
	}
	if !errors.Is(err, database.ErrClosed) {
		t.Errorf("error = %v, want it to wrap database.ErrClosed", err)
	}
	if cap.hasSubstring("durably queued") {
		t.Errorf("log falsely claimed a durably-queued removal despite the admission itself failing: %v", cap.msgs)
	}
	if cap.hasSubstring("Streamer removal committed") {
		t.Errorf("log falsely claimed a committed removal despite the admission itself failing: %v", cap.msgs)
	}

	// Runtime unchanged.
	if m.streamers.Get(victim) == nil {
		t.Error("victim removed from the runtime roster despite the admission failing")
	}
	// In-memory config unchanged.
	found := false
	for _, sc := range m.config.Streamers {
		if sc.Username == victim {
			found = true
		}
	}
	if !found {
		t.Error("victim removed from the in-memory config despite the admission failing")
	}
	// Config file unchanged (SaveConfig is never even reached).
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config file after the failed apply: %v", err)
	}
	if string(before) != string(after) {
		t.Error("config.json was rewritten despite the admission failing")
	}

	// No durable row in EITHER ledger, and no persisted history lost: verify
	// by reopening a FRESH connection to the SAME file (the original handle
	// is closed) — mirrors the restart pattern in
	// internal/streamerlifecycle/durable_test.go.
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
		t.Errorf("admissions=%d pending=%d, want 0,0 (nothing durable was ever attempted before the pre-commit failure)", admissions, pending)
	}

	reopenedAn, err := analytics.NewSQLiteRepository(reopened, t.TempDir())
	if err != nil {
		t.Fatalf("reopen analytics repo: %v", err)
	}
	if data, _ := reopenedAn.GetStreamerData(victim); len(data.Series) == 0 {
		t.Error("victim's analytics history was lost despite the admission failing pre-commit")
	}
	reopenedWt, err := watcher.NewWatchTimeStore(reopened)
	if err != nil {
		t.Fatalf("reopen watch-time store: %v", err)
	}
	if got, _ := reopenedWt.WindowMinutes([]string{victim}, time.Now().Add(time.Hour)); got[victim] == 0 {
		t.Error("victim's watch-time history was lost despite the admission failing pre-commit")
	}
}

// TestApplySettingsMultiRemovalAllOrNothingAtMinerLevel proves the miner
// submits every removal of one apply as ONE admission batch, not a
// per-streamer loop: with the database closed, BOTH of two streamers this
// apply would have removed must stay in the runtime roster — a per-streamer
// loop could have admitted/removed the first before failing on the second.
func TestApplySettingsMultiRemovalAllOrNothingAtMinerLevel(t *testing.T) {
	const victim1, victim2, keep = "multivictim1", "multivictim2", "multikeep"
	m, _, _ := newCapabilityMiner(t, victim1, victim2, keep)

	dbPath := filepath.Join(t.TempDir(), "miner.db")
	db := openRawMinerDB(t, dbPath)
	wireRawDeletionStores(t, m, db)
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
	rs.Streamers = keepOnly // removes BOTH victims in one apply

	if err := m.applySettings(context.Background(), rs); err == nil {
		t.Fatal("expected the batch admission to fail")
	}

	if m.streamers.Get(victim1) == nil {
		t.Error("victim1 removed from the runtime despite the batch admission failing")
	}
	if m.streamers.Get(victim2) == nil {
		t.Error("victim2 removed from the runtime despite the batch admission failing")
	}
	if m.streamers.Get(keep) == nil {
		t.Error("unrelated streamer lost from the runtime")
	}
}

// TestApplySettingsSaveConfigFailureCompensatesAdmission pins the second
// pre-commit failure mode: durable admission SUCCEEDS, but the commit point
// itself (config.SaveConfig) fails. The apply must still be a total no-op —
// runtime/config/file unchanged — AND the prepared admission row must be
// compensated (AbortAdmission), not left to arbitration to clean up at the
// next startup (though arbitration would resolve it correctly either way).
func TestApplySettingsSaveConfigFailureCompensatesAdmission(t *testing.T) {
	const victim, keep = "savecfgfailvictim", "savecfgfailkeep"
	m, _, _ := newCapabilityMiner(t, victim, keep)
	svc, _ := wireDeletionStores(t, m)

	configPath := filepath.Join(t.TempDir(), "config.json")
	m.configPath = configPath
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config file: %v", err)
	}

	if err := svc.Repository().RecordPoints(victim, 300, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}

	// Force the NEXT SaveConfig to fail deterministically (same seam
	// cp1_c2_matrix_test.go uses for the rename path's own SaveConfig
	// failure test).
	breakConfigPathForNextSave(t, configPath)

	// Capture every log emitted during the failed apply: admission SUCCEEDED
	// but the commit point (SaveConfig) never did, so the apply must not
	// claim a committed OR durably-queued removal — it was compensated
	// (AbortAdmission), not left owed.
	cap := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(prev)

	rs := removeStreamer(m.GetRuntimeSettings(), victim)
	err := m.applySettings(context.Background(), rs)
	if err == nil {
		t.Fatal("expected SaveConfig's failure to fail the apply")
	}
	if cap.hasSubstring("Streamer removal committed") {
		t.Errorf("log falsely claimed a committed removal despite SaveConfig (the commit point) failing: %v", cap.msgs)
	}
	if cap.hasSubstring("durably queued") {
		t.Errorf("log falsely claimed a durably-queued removal despite the admission being compensated: %v", cap.msgs)
	}

	// Runtime unchanged.
	if m.streamers.Get(victim) == nil {
		t.Error("victim removed from the runtime despite SaveConfig failing")
	}
	// In-memory config unchanged.
	found := false
	for _, sc := range m.config.Streamers {
		if sc.Username == victim {
			found = true
		}
	}
	if !found {
		t.Error("victim removed from the in-memory config despite SaveConfig failing")
	}
	// The path breakConfigPathForNextSave installed (a directory) was never
	// replaced with new content.
	info, statErr := os.Stat(configPath)
	if statErr != nil || !info.IsDir() {
		t.Fatalf("configPath must still be the untouched directory: stat=%v, err=%v", info, statErr)
	}

	// The prepared admission was compensated: no durable row survives.
	if has, err := m.streamerLifecycle.HasPending(context.Background(), victim); err != nil || has {
		t.Errorf("HasPending after a compensated admission = (%v, %v), want (false, nil)", has, err)
	}

	// History intact.
	if data, _ := svc.Repository().GetStreamerData(victim); len(data.Series) == 0 {
		t.Error("victim's analytics history was purged despite SaveConfig failing")
	}
}

// TestApplySettingsRequestCtxCancelledBeforeAdmissionZeroMutation proves the
// pre-admission cancellation check: a request ctx already cancelled before
// applySettings even runs must abort with zero mutation and — critically —
// WITHOUT ever attempting AdmitRemovals (asserted by the total absence of a
// durable row, not merely by a returned error, since a late abort could
// also legitimately leave no row).
func TestApplySettingsRequestCtxCancelledBeforeAdmissionZeroMutation(t *testing.T) {
	const victim, keep = "ctxcancelvictim", "ctxcancelkeep"
	m, _, _ := newCapabilityMiner(t, victim, keep)
	wireDeletionStores(t, m)

	rs := removeStreamer(m.GetRuntimeSettings(), victim)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := m.applySettings(ctx, rs)
	if err == nil {
		t.Fatal("expected a cancelled request context to abort the apply")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}

	if m.streamers.Get(victim) == nil {
		t.Error("victim removed from the runtime despite the request being cancelled before admission")
	}
	if has, err := m.streamerLifecycle.HasPending(context.Background(), victim); err != nil || has {
		t.Errorf("HasPending = (%v, %v), want (false, nil) — admission must never even be attempted", has, err)
	}
}

// TestApplySettingsRunCtxCancelledRejectsBeforeMutation pins beginApply's
// gate: once m.runCtx is cancelled (the miner is shutting down), EVERY apply
// is refused with ErrShuttingDown before touching anything — including
// PlanReconcile's own Twitch resolution, which never runs.
func TestApplySettingsRunCtxCancelledRejectsBeforeMutation(t *testing.T) {
	const victim = "runctxcancelvictim"
	m, _, _ := newCapabilityMiner(t, victim)
	runCtx, cancel := context.WithCancel(context.Background())
	cancel()
	m.runCtx = runCtx

	rs := m.GetRuntimeSettings()
	rs.Streamers = nil // even a full-roster wipe must be rejected

	err := m.applySettings(context.Background(), rs)
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("err = %v, want ErrShuttingDown", err)
	}
	if m.streamers.Get(victim) == nil {
		t.Error("streamer removed from the runtime despite beginApply rejecting the apply")
	}
}

// TestApplySettingsBeginApplyDrainingRejects mirrors the runCtx-cancelled
// case for the OTHER half of the gate: applyDraining set (Stop already in
// progress) also refuses BEFORE any mutation, independent of runCtx.
func TestApplySettingsBeginApplyDrainingRejects(t *testing.T) {
	const victim = "drainingvictim"
	m, _, _ := newCapabilityMiner(t, victim)
	m.applyMu.Lock()
	m.applyDraining = true
	m.applyMu.Unlock()

	rs := m.GetRuntimeSettings()
	rs.Streamers = nil

	err := m.applySettings(context.Background(), rs)
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("err = %v, want ErrShuttingDown", err)
	}
	if m.streamers.Get(victim) == nil {
		t.Error("streamer removed from the runtime despite applyDraining rejecting the apply")
	}
}

// failingPurger errors so a coordinator's completion (purge) step fails
// deterministically, mirroring internal/streamerlifecycle's own failPurger
// seam (unexported there, so redeclared locally for this package's tests).
type failingPurger struct{}

func (failingPurger) DeleteStreamerTx(*sql.Tx, string) (bool, error) {
	return false, errors.New("injected purge failure")
}

// captureHandler is a minimal slog.Handler that records every message
// emitted while installed, used ONLY to assert the exact truthful log text
// fires from the purge-pending branch (in addition to, never instead of,
// asserting the actual durable row state).
type captureHandler struct {
	mu   sync.Mutex
	msgs []string
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

func (h *captureHandler) has(msg string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if m == msg {
			return true
		}
	}
	return false
}

// hasSubstring reports whether ANY captured message contains sub — used to
// assert a truthful-durability claim (e.g. "durably queued", "Streamer
// removal committed") is ABSENT from a branch that never durably admitted or
// committed anything, where an exact-match check would be too narrow (the
// claim must not appear in any form, not just verbatim).
func (h *captureHandler) hasSubstring(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// TestApplySettingsPurgeFailurePostCommitStaysRemovedRowRemains pins the
// post-commit completion-failure contract (M1 §7/§8): once config.json is
// committed and the runtime roster updated, a FAILED purge is NOT an apply
// failure — the user's intent (remove the streamer) is fully durable — but
// it must leave a truthful, durable trail: the pending-purge row remains
// (retried at next startup), the persisted history survives (rolled back,
// not half-deleted), and the exact log message asserting durability fires
// ONLY because a durable row provably exists at that point.
func TestApplySettingsPurgeFailurePostCommitStaysRemovedRowRemains(t *testing.T) {
	const victim, keep = "purgefailvictim", "purgefailkeep"
	m, _, _ := newCapabilityMiner(t, victim, keep)

	db, err := database.Open(t.TempDir()) // package-wide singleton; never closed by this test
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("analytics service: %v", err)
	}
	wt, err := watcher.NewWatchTimeStore(db)
	if err != nil {
		t.Fatalf("watch-time store: %v", err)
	}
	m.db = db
	m.analyticsSvc = svc
	m.watchTimeStore = wt

	// A coordinator whose completion (purge) step is ALWAYS going to fail —
	// its admission/move steps (the DB layer) are perfectly healthy.
	coord, err := streamerlifecycle.New(db,
		[]streamerlifecycle.Purger{svc.Repository(), wt, failingPurger{}},
		[]streamerlifecycle.Fencer{svc.Repository(), wt},
		nil,
	)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	m.streamerLifecycle = coord

	if err := svc.Repository().RecordPoints(victim, 400, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	if err := m.ApplySettings(context.Background(), m.GetRuntimeSettings()); err != nil {
		t.Fatalf("baseline apply: %v", err)
	}

	cap := &captureHandler{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(prev)

	rs := removeStreamer(m.GetRuntimeSettings(), victim)
	if err := m.applySettings(context.Background(), rs); err != nil {
		t.Fatalf("a post-commit purge failure must NOT fail the apply (committed, purge pending, is a success): %v", err)
	}

	// Streamer stays removed from runtime/config.
	if m.streamers.Get(victim) != nil {
		t.Error("victim still present in the runtime despite a committed removal")
	}

	// Durable pending row remains — the truthful "durably queued" claim.
	if has, err := coord.HasPending(context.Background(), victim); err != nil || !has {
		t.Errorf("HasPending = (%v, %v), want (true, nil) — the purge failure must leave a durable row", has, err)
	}

	// Persisted history survives (purge transaction rolled back).
	if data, _ := svc.Repository().GetStreamerData(victim); len(data.Series) == 0 {
		t.Error("victim's analytics history was purged despite the injected purge failure")
	}

	// The exact truthful log line fired — only reachable from a branch where
	// a durable row provably exists (see purgeRemovedStreamer's doc comment).
	const wantMsg = "Streamer removed and removal committed, but persisted-history purge failed; durably queued to retry on the next startup"
	if !cap.has(wantMsg) {
		t.Errorf("expected the purge-pending log message %q, got: %v", wantMsg, cap.msgs)
	}
}

// TestApplySettingsLegacyPathIgnoresCancelledRequestCtx pins "zero behavior
// change" for a no-rename, no-removal apply (BKM-006's original legacy
// path): unlike the SRAP removal/rename paths, it never consults the
// request ctx at all (it always used m.runCtx, exactly as before this
// pass), so an already-cancelled REQUEST context must NOT block it.
func TestApplySettingsLegacyPathIgnoresCancelledRequestCtx(t *testing.T) {
	const alpha = "legacyctxalpha"
	m, _, _ := newCapabilityMiner(t, alpha)

	rs := m.GetRuntimeSettings()
	falseVal := false
	overrideStreamer(&rs, alpha, func(sc *settings.StreamerSettingsConfig) { sc.FollowRaid = &falseVal })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := m.applySettings(ctx, rs); err != nil {
		t.Fatalf("legacy (no-removal, no-rename) path must ignore a cancelled REQUEST ctx: %v", err)
	}
	if got := m.streamers.Get(alpha).GetSettings().FollowRaid; got {
		t.Fatal("posted setting was not applied")
	}
}
