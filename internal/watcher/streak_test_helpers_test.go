package watcher

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func acceptBoundStreakForWatcherTest(t *testing.T, stream *models.Stream, eventID, broadcastID string) models.WatchStreakGrantResult {
	t.Helper()
	result := stream.AcceptWatchStreakGrant(models.WatchStreakGrantEvent{
		EventID:           eventID,
		AcceptedAt:        time.Now(),
		ProvenBroadcastID: broadcastID,
	})
	if result.Admission != models.WatchStreakGrantNewBound {
		t.Fatalf("bound streak admission = %s, want %s", result.Admission, models.WatchStreakGrantNewBound)
	}
	return result
}

func hydrateBoundStreakForWatcherTest(stream *models.Stream, eventID, broadcastID string) {
	stream.HydrateWatchStreak(models.WatchStreakPersistence{
		Revision: 1,
		Grants: []models.WatchStreakGrantFact{{
			EventID: eventID, Binding: models.WatchStreakGrantBound,
			BroadcastID: broadcastID, AcceptedAt: time.Now(),
		}},
	})
}
