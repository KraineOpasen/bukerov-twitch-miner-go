package miner

import (
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/pubsub"
	streamermanager "github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamer"
)

var watchStreakTestSequence atomic.Uint64

func TestWatchStreakReplayWritesCacheAndAnalyticsOnce(t *testing.T) {
	// The miner test package intentionally shares database.Open's process-wide
	// singleton. A unique identity keeps this assertion hermetic under
	// `go test -count=N`, where TestMain runs only once for all repetitions.
	login := fmt.Sprintf("pr05-idempotency-%d", watchStreakTestSequence.Add(1))
	m, _, _ := newCapabilityMiner(t, login)
	s := m.streamers.Get(login)
	if s == nil {
		t.Fatal("streamer not found")
	}

	db, err := database.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc, err := analytics.NewService(db, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	m.analyticsSvc = svc
	cachePath := filepath.Join(t.TempDir(), "streak_cache.json")
	cache := streamermanager.NewStreakCache(cachePath)
	m.streamers.SetStreakCache(cache)

	// The exact event identity is the same fingerprint the pool hands to the
	// streak admission below; the exact ledger keys its row by it too. The
	// frame carries Twitch's event timestamp, as every real points-earned
	// frame does (it is what makes equal grants distinct events).
	msg := &pubsub.PubSubMessage{
		Topic: pubsub.NewTopic(pubsub.TopicCommunityPointsUser, "user"),
		Type:  "points-earned",
		Data: map[string]interface{}{
			"timestamp": "2026-08-25T09:00:00.000000000Z",
			"point_gain": map[string]interface{}{
				"reason_code":  "WATCH_STREAK",
				"total_points": float64(350),
			},
		},
		EventFingerprint: "sha256:exact-replay",
	}
	s.SetChannelPoints(1234)
	event := models.WatchStreakGrantEvent{EventID: "sha256:exact-replay", AcceptedAt: time.Now()}
	first := s.ApplyWatchStreakGrant(event, 350)
	if !first.NewlyAccepted() || first.Admission != models.WatchStreakGrantNewUnbound {
		t.Fatalf("first admission=%s, want NEW_UNBOUND", first.Admission)
	}
	m.handlePubSubMessage(msg, s, pubsub.MessageOutcome{WatchStreak: first})
	firstCache, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read first cache: %v", err)
	}

	replay := s.ApplyWatchStreakGrant(event, 350)
	if replay.Admission != models.WatchStreakGrantDuplicate {
		t.Fatalf("replay admission=%s, want DUPLICATE", replay.Admission)
	}
	m.handlePubSubMessage(msg, s, pubsub.MessageOutcome{WatchStreak: replay})
	secondCache, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("read replay cache: %v", err)
	}
	if string(firstCache) != string(secondCache) {
		t.Fatalf("duplicate changed cache bytes\nfirst=%s\nsecond=%s", firstCache, secondCache)
	}

	if history := s.History["WATCH_STREAK"]; history == nil || history.Counter != 1 || history.Amount != 350 {
		t.Fatalf("history=%+v, want one +350", history)
	}
	points, err := svc.Repository().GetPointSamples(login, time.Time{}, time.Time{}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || points[0].Reason != "WATCH STREAK" || points[0].Balance != 1234 {
		t.Fatalf("analytics points=%+v, want one WATCH STREAK row", points)
	}
	annotations, err := svc.Repository().GetAnnotationRecords(login, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(annotations) != 1 || annotations[0].Type != "WATCH_STREAK" {
		t.Fatalf("analytics annotations=%+v, want one WATCH_STREAK", annotations)
	}
	// The exact ledger holds the single accepted grant at its event-local
	// amount; the replay (not newly accepted) never reached analytics.
	exact, err := svc.Repository().ExactEarningsBetween(login, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if exact.Events != 1 || len(exact.Breakdown) != 1 || exact.Breakdown[0].Reason != "WATCH_STREAK" || exact.Breakdown[0].Gained != 350 || exact.Breakdown[0].Count != 1 {
		t.Fatalf("exact ledger=%+v, want one WATCH_STREAK event of 350", exact)
	}
	loaded := cache.Load(time.Now())[login]
	if loaded.Revision != 1 || len(loaded.Grants) != 1 || loaded.Grants[0].Binding != models.WatchStreakGrantUnbound {
		t.Fatalf("persisted ledger=%+v, want one GRANTED_UNBOUND", loaded)
	}
}
