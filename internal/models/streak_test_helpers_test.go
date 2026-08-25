package models

import (
	"testing"
	"time"
)

func acceptBoundStreakForTest(t *testing.T, stream *Stream, eventID, broadcastID string) WatchStreakGrantResult {
	t.Helper()
	result := stream.AcceptWatchStreakGrant(WatchStreakGrantEvent{
		EventID:           eventID,
		AcceptedAt:        time.Now(),
		ProvenBroadcastID: broadcastID,
	})
	if result.Admission != WatchStreakGrantNewBound {
		t.Fatalf("bound streak admission = %s, want %s", result.Admission, WatchStreakGrantNewBound)
	}
	return result
}

func applyBoundStreakForTest(t *testing.T, streamer *Streamer, eventID, broadcastID string, earned int) WatchStreakGrantResult {
	t.Helper()
	result := streamer.ApplyWatchStreakGrant(WatchStreakGrantEvent{
		EventID:           eventID,
		AcceptedAt:        time.Now(),
		ProvenBroadcastID: broadcastID,
	}, earned)
	if result.Admission != WatchStreakGrantNewBound {
		t.Fatalf("bound streak admission = %s, want %s", result.Admission, WatchStreakGrantNewBound)
	}
	return result
}
