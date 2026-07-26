package miner

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/notifications"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// wireDeletionStores attaches real analytics + watch-time stores (over the
// package-shared SQLite singleton) to a capability miner and builds the
// streamer-deletion coordinator, so a settings-apply removal runs the full
// persisted purge through finishApply -> applyStreamerDeletions.
func wireDeletionStores(t *testing.T, m *Miner) (*analytics.Service, *watcher.WatchTimeStore) {
	t.Helper()
	db, err := database.Open(t.TempDir())
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
	m.buildStreamerLifecycle()
	if m.streamerLifecycle == nil {
		t.Fatal("streamer lifecycle coordinator was not built")
	}
	return svc, wt
}

func analyticsListed(t *testing.T, svc *analytics.Service, login string) bool {
	t.Helper()
	list, err := svc.Repository().ListStreamers()
	if err != nil {
		t.Fatalf("list streamers: %v", err)
	}
	for _, info := range list {
		if info.Name == login {
			return true
		}
	}
	return false
}

// TestApplySettingsRemovalPurgesPersistedState is the end-to-end BKM-018A test:
// removing a streamer through the real settings-apply path purges its analytics
// and watch-time history, arms the resurrection fence, leaves other streamers
// intact, and a subsequent re-add starts clean.
func TestApplySettingsRemovalPurgesPersistedState(t *testing.T) {
	const victim, keep = "delintvictim", "delintkeep"
	m, _, _ := newCapabilityMiner(t, victim, keep)
	svc, wt := wireDeletionStores(t, m)

	// Seed persisted history for both.
	for _, s := range []string{victim, keep} {
		if err := svc.Repository().RecordPoints(s, 500, "WATCH"); err != nil {
			t.Fatalf("seed points %s: %v", s, err)
		}
		if err := wt.RecordMinutes(s, 10, time.Now()); err != nil {
			t.Fatalf("seed watch-time %s: %v", s, err)
		}
	}
	_ = m.ApplySettings(context.Background(), m.GetRuntimeSettings()) // seed runtime topic state

	// Remove the victim from the roster.
	rs := m.GetRuntimeSettings()
	var keepList []settings.StreamerConfig
	for _, sc := range rs.Streamers {
		if sc.Username != victim {
			keepList = append(keepList, sc)
		}
	}
	rs.Streamers = keepList
	_ = m.ApplySettings(context.Background(), rs)

	// Roster gone.
	if m.streamers.Get(victim) != nil {
		t.Fatal("removed streamer still in roster")
	}
	// Persisted analytics + watch-time purged for the victim.
	if analyticsListed(t, svc, victim) {
		t.Error("deleted streamer still present in analytics after removal (defect)")
	}
	if got, _ := wt.WindowMinutes([]string{victim}, time.Now()); got[victim] != 0 {
		t.Error("deleted streamer's watch-time survived removal")
	}
	// Fence armed: a late event cannot resurrect it.
	if err := svc.Repository().RecordPoints(victim, 1, "WATCH"); !errors.Is(err, analytics.ErrStreamerDeleted) {
		t.Errorf("fence not armed after removal: RecordPoints returned %v", err)
	}
	// Unrelated streamer fully preserved.
	if !analyticsListed(t, svc, keep) {
		t.Error("unrelated streamer lost its analytics history")
	}
	if got, _ := wt.WindowMinutes([]string{keep}, time.Now()); got[keep] == 0 {
		t.Error("unrelated streamer lost its watch-time")
	}

	// Re-add the same login: fence lifted, records fresh history.
	rs2 := m.GetRuntimeSettings()
	rs2.Streamers = append(rs2.Streamers, settings.StreamerConfig{Username: victim})
	_ = m.ApplySettings(context.Background(), rs2)
	if m.streamers.Get(victim) == nil {
		t.Fatal("re-added streamer missing from roster")
	}
	if err := svc.Repository().RecordPoints(victim, 9, "WATCH"); err != nil {
		t.Fatalf("re-added streamer cannot record (fence not lifted): %v", err)
	}
	// Only the fresh row exists — no stale history inherited.
	if data, _ := svc.Repository().GetStreamerData(victim); len(data.Series) != 1 {
		t.Errorf("re-added streamer has %d points, want 1 (clean lifecycle)", len(data.Series))
	}
}

// TestRemovalPurgesNotificationRowsWithoutLiveManager pins the fix for the gap
// where notification purge was skipped when no Discord Manager exists: point
// rules and config-list rows persist in the DB independently of the Manager, so
// removing a streamer with Discord OFF must still scrub them via the standalone
// notifications repository the coordinator falls back to.
func TestRemovalPurgesNotificationRowsWithoutLiveManager(t *testing.T) {
	const victim, keep = "nomgrvictim", "nomgrkeep"
	m, _, _ := newCapabilityMiner(t, victim, keep)

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("analytics service: %v", err)
	}
	notifRepo, err := notifications.NewRepository(db)
	if err != nil {
		t.Fatalf("notif repo: %v", err)
	}
	m.db = db
	m.analyticsSvc = svc
	m.notificationsRepo = notifRepo
	m.notifications = nil // Discord OFF: no live Manager
	m.buildStreamerLifecycle()

	// Seed point rules as a prior (Discord-enabled) run would have.
	for _, s := range []string{victim, keep} {
		if err := notifRepo.AddPointRule(&notifications.PointRule{Streamer: s, Threshold: 100}); err != nil {
			t.Fatalf("seed rule %s: %v", s, err)
		}
	}
	_ = m.ApplySettings(context.Background(), m.GetRuntimeSettings())

	// Remove the victim.
	rs := m.GetRuntimeSettings()
	var keepList []settings.StreamerConfig
	for _, sc := range rs.Streamers {
		if sc.Username != victim {
			keepList = append(keepList, sc)
		}
	}
	rs.Streamers = keepList
	_ = m.ApplySettings(context.Background(), rs)

	rules, _ := notifRepo.GetPointRules()
	victimRule, keepRule := false, false
	for _, r := range rules {
		switch r.Streamer {
		case victim:
			victimRule = true
		case keep:
			keepRule = true
		}
	}
	if victimRule {
		t.Error("notification point rule survived removal with Discord OFF (defect)")
	}
	if !keepRule {
		t.Error("unrelated streamer's point rule was removed")
	}
}

// TestStartupReconcilesDurablePendingDeletion verifies the miner wires durable
// reconciliation: a pending-deletion marker left in the DB (a prior run's failed
// purge) is discovered and completed by reconcilePendingStreamerDeletions at
// startup, so the failed purge is not lost across a restart.
func TestStartupReconcilesDurablePendingDeletion(t *testing.T) {
	m, _, _ := newCapabilityMiner(t, "keepwire")
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	notifRepo, err := notifications.NewRepository(db)
	if err != nil {
		t.Fatalf("notif: %v", err)
	}
	wt, err := watcher.NewWatchTimeStore(db)
	if err != nil {
		t.Fatalf("watch-time: %v", err)
	}
	m.db = db
	m.analyticsSvc = svc
	m.notificationsRepo = notifRepo
	m.watchTimeStore = wt
	m.buildStreamerLifecycle()
	if m.streamerLifecycle == nil {
		t.Fatal("coordinator not built")
	}

	// A streamer with rows + a durable pending-deletion marker (a prior failed purge).
	if err := svc.Repository().RecordPoints("reconcilewire", 100, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO pending_streamer_deletions (login, channel_id, requested_at, attempts)
		VALUES (?, ?, ?, 0)`, "reconcilewire", "chan-reconcilewire", 1); err != nil {
		t.Fatalf("seed pending marker: %v", err)
	}

	m.reconcilePendingStreamerDeletions()

	if analyticsListed(t, svc, "reconcilewire") {
		t.Error("startup reconciliation did not purge the pending deletion")
	}
	var cnt int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pending_streamer_deletions WHERE login = 'reconcilewire'`).Scan(&cnt); err != nil {
		t.Fatalf("count markers: %v", err)
	}
	if cnt != 0 {
		t.Error("durable marker not cleared after successful startup reconciliation")
	}
}

// TestArbitrationKeepFuncCanonicalizesMixedCaseUsername (M1 QA follow-up F4)
// proves arbitrationKeepFunc's config-side lookup canonicalizes the SAME way
// AdmitRemovals/listAdmissions canonicalize a prepared row's login (ToLower +
// TrimSpace): a config entry with a MIXED-CASE Username must still be found
// for a prepared admissions row recorded under its (already-canonical)
// lowercase login — losing that canonicalization would make arbitration
// treat a still-configured, mixed-case-named streamer as absent and wrongly
// PROMOTE (and then purge) it instead of ABORTing the stale prepared row.
func TestArbitrationKeepFuncCanonicalizesMixedCaseUsername(t *testing.T) {
	const mixedCaseUsername = "MixedCase_M1F4"
	const canonicalLogin = "mixedcase_m1f4" // strings.ToLower(mixedCaseUsername)
	const channelID = "chan-f4mixedcase"

	dbPath := filepath.Join(t.TempDir(), "miner.db")
	db := openRawMinerDB(t, dbPath)
	an, err := analytics.NewSQLiteRepository(db, t.TempDir())
	if err != nil {
		t.Fatalf("analytics: %v", err)
	}
	coord, err := streamerlifecycle.New(db, []streamerlifecycle.Purger{an}, []streamerlifecycle.Fencer{an}, nil)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	if err := an.RecordPoints(canonicalLogin, 100, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}
	// AdmitRemovals itself canonicalizes the login it stores — seed the
	// admissions row directly under the ALREADY-canonical form, matching
	// what a real admission would have written.
	if err := coord.AdmitRemovals(context.Background(), []streamerlifecycle.Removal{
		{ChannelID: channelID, Login: mixedCaseUsername}, // AdmitRemovals lowercases this itself
	}); err != nil {
		t.Fatalf("admit: %v", err)
	}

	m := &Miner{
		config: &config.Config{Streamers: []config.StreamerConfig{
			{Username: mixedCaseUsername, ChannelID: channelID}, // MIXED CASE, as a real config.json entry would be
		}},
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
		t.Errorf("admissions=%d, want 0 (the still-configured mixed-case streamer must be ABORTED, not left prepared)", admissions)
	}
	if pending != 0 {
		t.Errorf("pending=%d, want 0 (a still-configured streamer must never be PROMOTED to purge, even under a mixed-case Username)", pending)
	}
	if data, _ := an.GetStreamerData(canonicalLogin); len(data.Series) == 0 {
		t.Error("still-configured mixed-case streamer's history was purged — arbitrationKeepFunc failed to canonicalize the config Username, wrongly treating it as absent")
	}
}
