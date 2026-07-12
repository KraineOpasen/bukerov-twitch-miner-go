package events

import (
	"fmt"
	"testing"
)

func TestRecentReturnsNewestFirst(t *testing.T) {
	l := NewLog(10)
	l.Record(TypeStreamerOnline, "alice", "")
	l.Record(TypeBonusClaimed, "alice", "+50")
	l.Record(TypeStreamerOffline, "bob", "")

	got := l.Recent(2)
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %d", len(got))
	}
	if got[0].Type != TypeStreamerOffline || got[1].Type != TypeBonusClaimed {
		t.Fatalf("expected newest-first order, got %v then %v", got[0].Type, got[1].Type)
	}
}

func TestRingBufferOverwritesOldest(t *testing.T) {
	l := NewLog(3)
	for i := 0; i < 5; i++ {
		l.Record(TypePointsEarned, "alice", fmt.Sprintf("event-%d", i))
	}

	got := l.Recent(10)
	if len(got) != 3 {
		t.Fatalf("expected capacity-bound 3 events, got %d", len(got))
	}
	if got[0].Detail != "event-4" || got[2].Detail != "event-2" {
		t.Fatalf("expected events 4..2 newest-first, got %q..%q", got[0].Detail, got[2].Detail)
	}
}

func TestRecentOnEmptyLog(t *testing.T) {
	l := NewLog(3)
	if got := l.Recent(5); len(got) != 0 {
		t.Fatalf("expected no events from empty log, got %d", len(got))
	}
}
