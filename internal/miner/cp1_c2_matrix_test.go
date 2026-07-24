package miner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
)

// newRealAnalytics opens a REAL analytics.Service backed by SQLite (the
// package-wide database.Open singleton — see TestMain in
// database_singleton_test.go), matching internal/analytics's own test
// convention: tests isolate themselves with unique streamer logins rather
// than separate databases, since the underlying handle is shared for the
// whole test binary run.
func newRealAnalytics(t *testing.T) *analytics.Service {
	t.Helper()
	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatalf("new analytics service: %v", err)
	}
	return svc
}

// breakConfigPathForNextSave replaces the regular file at path with a
// directory of the same name, so the NEXT config.SaveConfig(path, ...) call
// fails deterministically at its final atomic step: WriteFileAtomic's
// os.Rename(tmpFile, path) always fails when path is an existing directory
// (POSIX rename(2) semantics — a regular file can never be renamed onto a
// directory), REGARDLESS of process privilege. This is used instead of
// os.Chmod because these tests may run as root, under which directory
// permission bits are not enforced (root bypasses them), making chmod-based
// failure injection unreliable; a rename-onto-a-directory failure is a real
// filesystem invariant, not a permission check, so it is deterministic
// either way. The temp file WriteFileAtomic created is cleaned up by its own
// failure path — path itself is left exactly as this function set it
// (a directory), so a caller can assert "no new content was ever written"
// by checking it never became a regular file again.
func breakConfigPathForNextSave(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

// TestApplySettingsWithRename_SaveConfigFailure_NothingRenamed_C2B pins
// BKM-006 Corrective Pass 1 test matrix item C2-B: config.json persistence
// failing AFTER a successful analytics migration must compensate the
// analytics commit and leave the runtime completely unrenamed — "nothing
// renamed" end to end, not just the config file. Uses a REAL config file and
// a REAL analytics.Service/SQLite (not fakes) per the corrective-pass
// requirement to verify rollback/compensation against the genuine durable
// stores, not a mock's approximation of them.
func TestApplySettingsWithRename_SaveConfigFailure_NothingRenamed_C2B(t *testing.T) {
	svc := newRealAnalytics(t)
	if err := svc.Repository().RecordPoints("c2bold", 777, "WATCH"); err != nil {
		t.Fatalf("seed analytics: %v", err)
	}

	client := newRenameCapableAPI()
	client.set("c2bold", "id-c2b")
	m, _, chatRec := newRenameTestMiner(t, client, "c2bold")
	m.analyticsSvc = svc

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config file: %v", err)
	}
	m.configPath = configPath

	// Force the NEXT SaveConfig (the one commitRenameTransaction is about to
	// attempt) to fail deterministically.
	breakConfigPathForNextSave(t, configPath)

	client.set("c2bnew", "id-c2b")
	if err := m.applySettings(renameRuntimeStreamers(m, "c2bold", "c2bnew")); err == nil {
		t.Fatal("expected the rename transaction to fail when SaveConfig fails")
	}

	// Disk: no NEW config content was ever written — the path never became a
	// regular file again (WriteFileAtomic's failure path never reaches the
	// rename that would have replaced it).
	info, err := os.Stat(configPath)
	if err != nil || !info.IsDir() {
		t.Fatalf("configPath must still be the untouched directory this test installed (no new content written): stat=%v, err=%v", info, err)
	}

	// Runtime: completely unrenamed.
	if m.streamers.Get("c2bold") == nil {
		t.Error("runtime was renamed away from c2bold even though SaveConfig failed")
	}
	if m.streamers.Get("c2bnew") != nil {
		t.Error("runtime is under the new login c2bnew despite SaveConfig failing")
	}

	// In-memory config: untouched (m.config was never swapped to the
	// candidate).
	if len(m.config.Streamers) != 1 || m.config.Streamers[0].Username != "c2bold" {
		t.Errorf("in-memory config changed despite the failed transaction: %+v", m.config.Streamers)
	}

	// Analytics: the successful migration was compensated back — old login's
	// history is restored, new login has none.
	oldData, err := svc.Repository().GetStreamerData("c2bold")
	if err != nil {
		t.Fatalf("get old streamer data: %v", err)
	}
	if len(oldData.Series) != 1 {
		t.Errorf("analytics history not restored under c2bold after compensation: %d points, want 1", len(oldData.Series))
	}
	newData, err := svc.Repository().GetStreamerData("c2bnew")
	if err != nil {
		t.Fatalf("get new streamer data: %v", err)
	}
	if len(newData.Series) != 0 {
		t.Errorf("analytics history leaked under c2bnew despite compensation: %d points, want 0", len(newData.Series))
	}

	// No IRC action for a transaction that never committed.
	if got := chatRec.leaveCount("c2bold"); got != 0 {
		t.Errorf("chat left c2bold %d times despite the failed transaction, want 0", got)
	}
}

// TestApplySettingsWithRename_MultiRenameAnalyticsFailure_CompensatesEarlierCommit_C2C
// pins BKM-006 Corrective Pass 1 test matrix item C2-C: when a settings apply
// carries MULTIPLE renames in one batch and a LATER one's analytics commit
// fails (here: a genuine collision, both logins already have independent
// history) AFTER an EARLIER one already committed successfully, the earlier
// commit must be reversed — no committed partial state survives — and the
// runtime must stay completely unrenamed for BOTH streamers (the whole apply
// is one transaction). Verified against a REAL analytics.Service/SQLite.
func TestApplySettingsWithRename_MultiRenameAnalyticsFailure_CompensatesEarlierCommit_C2C(t *testing.T) {
	svc := newRealAnalytics(t)
	// Streamer 1's rename will succeed cleanly (no destination history yet).
	if err := svc.Repository().RecordPoints("c2c-old1", 100, "WATCH"); err != nil {
		t.Fatalf("seed old1: %v", err)
	}
	// Streamer 2's rename will collide: BOTH old2 and new2 already have
	// independent recorded history.
	if err := svc.Repository().RecordPoints("c2c-old2", 200, "WATCH"); err != nil {
		t.Fatalf("seed old2: %v", err)
	}
	if err := svc.Repository().RecordPoints("c2c-new2", 300, "WATCH"); err != nil {
		t.Fatalf("seed new2: %v", err)
	}

	client := newRenameCapableAPI()
	client.set("c2c-old1", "id-c2c-1")
	client.set("c2c-old2", "id-c2c-2")
	m, _, _ := newRenameTestMiner(t, client, "c2c-old1", "c2c-old2")
	m.analyticsSvc = svc
	// No configPath set: this test isolates the analytics-compensation step
	// (C2-B already covers the SaveConfig-failure compensation step).

	client.set("c2c-new1", "id-c2c-1")
	client.set("c2c-new2", "id-c2c-2")
	rs := m.GetRuntimeSettings()
	for i := range rs.Streamers {
		switch rs.Streamers[i].Username {
		case "c2c-old1":
			rs.Streamers[i].Username = "c2c-new1"
		case "c2c-old2":
			rs.Streamers[i].Username = "c2c-new2"
		}
	}

	if err := m.applySettings(rs); err == nil {
		t.Fatal("expected the batch to fail closed on the second rename's analytics collision")
	}

	// Streamer 1's EARLIER, already-committed analytics rename must have been
	// reversed — no partial state survives the batch's failure.
	old1, err := svc.Repository().GetStreamerData("c2c-old1")
	if err != nil {
		t.Fatalf("get old1: %v", err)
	}
	if len(old1.Series) != 1 {
		t.Errorf("streamer 1's analytics history not restored under c2c-old1: %d points, want 1", len(old1.Series))
	}
	new1, err := svc.Repository().GetStreamerData("c2c-new1")
	if err != nil {
		t.Fatalf("get new1: %v", err)
	}
	if len(new1.Series) != 0 {
		t.Errorf("streamer 1's analytics history leaked under c2c-new1 after compensation: %d points, want 0", len(new1.Series))
	}

	// Streamer 2's collision left both sides exactly as seeded (RenameStreamer
	// never mutates on a collision).
	old2, _ := svc.Repository().GetStreamerData("c2c-old2")
	if len(old2.Series) != 1 {
		t.Errorf("streamer 2 old data corrupted by the failed collision attempt: %d points, want 1", len(old2.Series))
	}
	new2, _ := svc.Repository().GetStreamerData("c2c-new2")
	if len(new2.Series) != 1 {
		t.Errorf("streamer 2 new data corrupted by the failed collision attempt: %d points, want 1", len(new2.Series))
	}

	// Runtime: BOTH streamers stay completely unrenamed — the whole apply is
	// one transaction, not a partial per-streamer commit.
	if m.streamers.Get("c2c-old1") == nil || m.streamers.Get("c2c-new1") != nil {
		t.Error("streamer 1 was renamed despite the batch failing")
	}
	if m.streamers.Get("c2c-old2") == nil || m.streamers.Get("c2c-new2") != nil {
		t.Error("streamer 2 was renamed despite the batch failing")
	}
}

// TestApplySettingsWithRename_Success_EndToEnd_C2D pins BKM-006 Corrective
// Pass 1 test matrix item C2-D: a successful rename transaction, verified
// end to end against a REAL config file and a REAL analytics.Service/SQLite —
// config durable, analytics under the new login carries the SAME history
// (continuity, not a fresh row), the SAME *models.Streamer pointer survives
// (byID unchanged, byLogin repointed), IRC leaves the old channel exactly
// once, PubSub has zero churn, and a repeated identical apply is a no-op.
func TestApplySettingsWithRename_Success_EndToEnd_C2D(t *testing.T) {
	svc := newRealAnalytics(t)
	if err := svc.Repository().RecordPoints("c2dold", 500, "WATCH"); err != nil {
		t.Fatalf("seed analytics: %v", err)
	}

	client := newRenameCapableAPI()
	client.set("c2dold", "id-c2d")
	m, topics, chatRec := newRenameTestMiner(t, client, "c2dold")
	m.analyticsSvc = svc

	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := config.SaveConfig(configPath, m.config); err != nil {
		t.Fatalf("seed config file: %v", err)
	}
	m.configPath = configPath

	// Establish the initial topic set (mirrors TestApplySettings_Rename_PubSubZeroChurn).
	m.ApplySettings(m.GetRuntimeSettings())
	channelTopics := []pubsub.TopicType{
		pubsub.TopicVideoPlaybackByID, pubsub.TopicRaid, pubsub.TopicPredictionsChannel, pubsub.TopicCommunityMomentsChannel,
	}
	before := make(map[pubsub.TopicType]int, len(channelTopics))
	for _, tt := range channelTopics {
		before[tt] = topics.listenCount(idTopic(tt, "id-c2d"))
	}

	beforeByID := m.streamers.GetByChannelID("id-c2d")
	beforeByLogin := m.streamers.Get("c2dold")
	if beforeByID == nil || beforeByID != beforeByLogin {
		t.Fatalf("setup: byID/byLogin inconsistent before the rename")
	}

	client.set("c2dnew", "id-c2d")
	if err := m.applySettings(renameRuntimeStreamers(m, "c2dold", "c2dnew")); err != nil {
		t.Fatalf("rename transaction failed: %v", err)
	}

	// Runtime identity: SAME pointer, byID unchanged, byLogin repointed.
	after := m.streamers.Get("c2dnew")
	if after == nil || after != beforeByID {
		t.Fatalf("rename did not preserve the SAME *models.Streamer pointer: before=%p after=%p", beforeByID, after)
	}
	if m.streamers.GetByChannelID("id-c2d") != beforeByID {
		t.Error("byID index changed identity across the rename")
	}
	if m.streamers.Get("c2dold") != nil {
		t.Error("old login still resolves to a streamer after a successful rename")
	}

	// Config durable on disk.
	onDisk, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config from disk: %v", err)
	}
	if len(onDisk.Streamers) != 1 || onDisk.Streamers[0].Username != "c2dnew" {
		t.Errorf("persisted config wrong: %+v", onDisk.Streamers)
	}
	if onDisk.Streamers[0].ChannelID != "id-c2d" {
		t.Errorf("persisted ChannelID = %q, want id-c2d", onDisk.Streamers[0].ChannelID)
	}

	// Analytics: SAME history followed the rename (continuity, not a fresh
	// independent row) — record one more point post-rename and confirm BOTH
	// are visible together under the new login.
	svc.RecordPoints(after, "WATCH")
	newData, err := svc.Repository().GetStreamerData("c2dnew")
	if err != nil {
		t.Fatalf("get new streamer data: %v", err)
	}
	if len(newData.Series) != 2 {
		t.Fatalf("analytics points under c2dnew = %d, want 2 (pre-rename history + post-rename write, same identity)", len(newData.Series))
	}
	oldData, _ := svc.Repository().GetStreamerData("c2dold")
	if len(oldData.Series) != 0 {
		t.Errorf("analytics points still under c2dold: %d, want 0", len(oldData.Series))
	}

	// IRC: left the old channel exactly once.
	if got := chatRec.leaveCount("c2dold"); got != 1 {
		t.Errorf("chat left c2dold %d times, want exactly 1", got)
	}
	if chatRec.toggleCount("c2dnew") == 0 {
		t.Error("chat presence for the new login was never reconciled")
	}

	// PubSub: zero churn (topics keyed by ChannelID, never touched by a rename).
	for _, tt := range channelTopics {
		if got := topics.listenCount(idTopic(tt, "id-c2d")); got != before[tt] {
			t.Errorf("topic %s LISTEN count changed by the rename: %d -> %d, want zero churn", tt, before[tt], got)
		}
	}

	// Repeat = no-op: applying the SAME (already-renamed) settings again must
	// not error, must not touch analytics again, and must not add a second
	// IRC leave.
	if err := m.applySettings(renameRuntimeStreamers(m, "c2dold", "c2dnew")); err != nil {
		t.Fatalf("repeated identical apply failed: %v", err)
	}
	if got := chatRec.leaveCount("c2dold"); got != 1 {
		t.Errorf("repeated apply added another chat leave for c2dold: %d, want still 1", got)
	}
	onDiskAgain, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("reload config after repeat: %v", err)
	}
	if len(onDiskAgain.Streamers) != 1 || onDiskAgain.Streamers[0].Username != "c2dnew" {
		t.Errorf("repeated apply changed persisted config: %+v", onDiskAgain.Streamers)
	}
}
