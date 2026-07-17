package miner

import (
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
)

// reasonTestSeq gives each analytics-backed test invocation a unique streamer
// name. The analytics DB is a process-wide singleton, so re-runs (e.g.
// -count=20) share one database; unique names keep each invocation isolated
// (exactly one row) instead of accumulating across iterations.
var reasonTestSeq atomic.Int64

func uniqueStreamer(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, reasonTestSeq.Add(1))
}

// pointsEarnedMsg builds a production-shaped community-points-user-v1
// points-earned message through the real parse path, carrying the given RAW
// reason_code (never a pre-canonicalized constant).
func pointsEarnedMsg(t *testing.T, channelID, reasonCode string, totalPoints, balance int) *pubsub.PubSubMessage {
	t.Helper()
	raw := fmt.Sprintf(`{
		"type": "points-earned",
		"data": {
			"timestamp": "2026-07-17T10:00:00.000000000Z",
			"channel_id": %q,
			"point_gain": {"user_id":"999","channel_id":%q,"total_points":%d,"reason_code":%q},
			"balance": {"user_id":"999","channel_id":%q,"balance":%d}
		}
	}`, channelID, channelID, totalPoints, reasonCode, channelID, balance)
	msg, err := pubsub.ParsePubSubMessage(&pubsub.WSData{Topic: "community-points-user-v1.999", Message: raw})
	if err != nil {
		t.Fatalf("ParsePubSubMessage: %v", err)
	}
	return msg
}

// newAnalyticsService builds a real analytics service backed by a temp SQLite
// database. The database is a process-wide singleton whose backing directory
// must outlive the test (a t.TempDir would be removed while the singleton still
// points at it), so a durable temp dir is used — mirroring the analytics
// package's own TestMain. Tests isolate via unique streamer names.
func newAnalyticsService(t *testing.T) *analytics.Service {
	t.Helper()
	dir, err := os.MkdirTemp("", "miner-reason-*")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	db, err := database.Open(dir)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	svc, err := analytics.NewService(db, dir, 0)
	if err != nil {
		t.Fatalf("new analytics service: %v", err)
	}
	return svc
}

// managerWithCache loads one streamer ("alpha") through the REAL LoadFromConfig
// path over a real StreakCache at cachePath.
func managerWithCache(t *testing.T, cachePath string) *streamer.Manager {
	t.Helper()
	mgr := streamer.NewManager(fakeStreamerAPI{}, models.DefaultStreamerSettings())
	mgr.SetStreakCache(streamer.NewStreakCache(cachePath))
	if err := mgr.LoadFromConfig([]config.StreamerConfig{{Username: "alpha"}}, nil); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	return mgr
}

// T2: the full grant -> binding -> cache composition for a production-shaped
// space-form payload, over a real Streamer Manager and a real StreakCache.
//
// The pool -> onMessage ordering itself is proven by the pubsub package
// (TestPoolSpaceFormWatchStreakBindsBeforeOnMessage). Here we reproduce that
// proven order — the pool's own UpdateHistory(canonical) FIRST, then the real
// miner onMessage callback — to exercise the cross-package cache write. The
// order is load-bearing: RecordStreakGrant reads the binding MarkStreakEarned
// set, and StreakCache.Record drops empty broadcast IDs, so a callback that ran
// before UpdateHistory would persist nothing (see mutation M4).
func TestFullGrantToCacheComposition(t *testing.T) {
	cachePath := t.TempDir() + "/streak_cache.json"
	mgr := managerWithCache(t, cachePath)
	s := mgr.Get("alpha")
	s.Stream.Update("bid-live", "title", nil, nil, 5) // identify the live broadcast
	if !s.Stream.StreakPending() {
		t.Fatal("precondition: an identified fresh broadcast should have the streak pending")
	}

	m := &Miner{streamers: mgr}
	msg := pointsEarnedMsg(t, "chan-alpha", "WATCH STREAK", 400, 231400)

	// (1) pool internal-handler effect, using the SAME shared helper the pool uses.
	_, canonical, ok := pubsub.PointReason(msg)
	if !ok {
		t.Fatal("PointReason ok = false")
	}
	s.UpdateHistory(canonical, 400)
	// (2) miner onMessage effect: the real RecordStreakGrant path.
	m.handlePubSubMessage(msg, s)

	if s.Stream.GetWatchStreakMissing() {
		t.Error("WatchStreakMissing should be cleared")
	}
	if s.Stream.StreakPending() {
		t.Error("StreakPending should be false after a grant on the live broadcast")
	}
	bid, at := s.Stream.StreakEarnedGrant()
	if bid != "bid-live" {
		t.Errorf("streakEarnedBroadcastID = %q, want bid-live", bid)
	}
	if at.IsZero() {
		t.Error("streakEarnedAt should be non-zero")
	}

	// The cache file must carry the grant bound to the live broadcast.
	grants := streamer.NewStreakCache(cachePath).Load(time.Now())
	g, ok := grants["alpha"]
	if !ok {
		t.Fatalf("no streak_cache entry for alpha: %#v", grants)
	}
	if g.BroadcastID != "bid-live" {
		t.Errorf("cached BroadcastID = %q, want bid-live", g.BroadcastID)
	}
	if g.GrantedAt.IsZero() {
		t.Error("cached GrantedAt should be non-zero")
	}
}

// T4: the RAW SQLite contract. A space-form payload must persist the raw
// event_type "WATCH STREAK" (the space-form), never the canonical
// "WATCH_STREAK". Verified against a real temp SQLite row, not a mock argument.
func TestRawEventTypeStaysSpaceForm(t *testing.T) {
	svc := newAnalyticsService(t)
	m := &Miner{analyticsSvc: svc} // streamers nil -> streak-grant path skipped

	name := uniqueStreamer("t4_raw")
	s := models.NewStreamer(name, models.DefaultStreamerSettings())
	s.ChannelID = "chan-t4"
	s.SetChannelPoints(231450)

	m.handlePubSubMessage(pointsEarnedMsg(t, "chan-t4", "WATCH STREAK", 450, 231450), s)

	samples, err := svc.Repository().GetPointSamples(name, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatalf("GetPointSamples: %v", err)
	}
	if len(samples) != 1 {
		t.Fatalf("want 1 points row, got %d: %#v", len(samples), samples)
	}
	if samples[0].Reason != "WATCH STREAK" {
		t.Errorf("event_type = %q, want the raw space-form %q", samples[0].Reason, "WATCH STREAK")
	}
	if samples[0].Reason == "WATCH_STREAK" {
		t.Error("event_type must NOT be the canonical underscore form in the points table")
	}
}

// T5: a space-form payload must create a canonical WATCH_STREAK annotation with
// the right text and colour, and the RAID annotation must not regress.
func TestSpaceFormCreatesCanonicalAnnotation(t *testing.T) {
	svc := newAnalyticsService(t)
	m := &Miner{analyticsSvc: svc}

	streakName := uniqueStreamer("t5_streak")
	s := models.NewStreamer(streakName, models.DefaultStreamerSettings())
	s.ChannelID = "chan-t5"
	m.handlePubSubMessage(pointsEarnedMsg(t, "chan-t5", "WATCH STREAK", 450, 231450), s)

	recs, err := svc.Repository().GetAnnotationRecords(streakName, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetAnnotationRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 annotation, got %d: %#v", len(recs), recs)
	}
	if recs[0].Type != "WATCH_STREAK" {
		t.Errorf("annotation type = %q, want WATCH_STREAK", recs[0].Type)
	}
	if recs[0].Reason != "+450 - Watch Streak" {
		t.Errorf("annotation text = %q, want %q", recs[0].Reason, "+450 - Watch Streak")
	}
	if recs[0].Color != "#45c1ff" {
		t.Errorf("annotation color = %q, want #45c1ff", recs[0].Color)
	}

	// RAID annotation regression: still recorded under its canonical type.
	raidName := uniqueStreamer("t5_raid")
	sr := models.NewStreamer(raidName, models.DefaultStreamerSettings())
	sr.ChannelID = "chan-raid"
	m.handlePubSubMessage(pointsEarnedMsg(t, "chan-raid", "RAID", 250, 500), sr)

	raidRecs, err := svc.Repository().GetAnnotationRecords(raidName, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("GetAnnotationRecords(raid): %v", err)
	}
	if len(raidRecs) != 1 || raidRecs[0].Type != "RAID" {
		t.Errorf("RAID annotation = %#v, want one row of type RAID", raidRecs)
	}
}
