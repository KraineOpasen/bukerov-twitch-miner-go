package watcher

import (
	"testing"
	"time"
)

func TestLostMiningAccrualAndDrain(t *testing.T) {
	w, _ := newTestWatcher(1)
	min := time.Minute

	// 2 fillable, 1 watched → 1 lost slot-minute (cap=min(2,MaxSlots)=2).
	w.accrueLostMining(2, 1, min)
	// 1 fillable, 1 watched → 0 lost (only one thing was watchable).
	w.accrueLostMining(1, 1, min)
	// 3 fillable, 0 watched → capped at MaxSlots(2) lost.
	w.accrueLostMining(3, 0, min)

	got := w.LostMiningMinutes()
	if got != 3 {
		t.Fatalf("accumulated lost = %v, want 3", got)
	}
	// Drain semantics: a second read is zero.
	if again := w.LostMiningMinutes(); again != 0 {
		t.Fatalf("after drain expected 0, got %v", again)
	}
}

func TestLostMiningNothingOnlineIsNotLost(t *testing.T) {
	w, _ := newTestWatcher(1)
	// 0 fillable (nothing online), 0 watched → nothing lost.
	w.accrueLostMining(0, 0, time.Minute)
	if got := w.LostMiningMinutes(); got != 0 {
		t.Fatalf("empty-with-no-candidates must not count as lost, got %v", got)
	}
}

func TestLostMiningScalesWithInterval(t *testing.T) {
	w, _ := newTestWatcher(1)
	// 1 lost slot for a 30s interval = 0.5 minutes.
	w.accrueLostMining(2, 1, 30*time.Second)
	if got := w.LostMiningMinutes(); got != 0.5 {
		t.Fatalf("lost = %v, want 0.5", got)
	}
}
