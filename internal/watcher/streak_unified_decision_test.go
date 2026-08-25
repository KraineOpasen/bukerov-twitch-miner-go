package watcher

import (
	"fmt"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// The direct selector and broker protection must consume one verdict. Ten
// delivered minutes is past the stale seven-minute cutoff but below the hard
// 20-minute pursuit cap, so both paths must retain the streak.
func TestPriorityStreakAndBrokerAgreeAtTenMinutes(t *testing.T) {
	w, online := newTestWatcher(1)
	w.priorities = []config.Priority{config.PriorityStreak}
	s := w.streamers[0]
	s.Stream.Update("broadcast-1", "title", nil, nil, 10)
	s.Stream.MinuteWatched = 10

	brokerEligible := w.isBoostEligible(0)
	direct := w.selectByPriority(online)
	directEligible := len(direct) == 1 && direct[0] == 0

	if !brokerEligible || !directEligible || brokerEligible != directEligible {
		t.Fatalf("same streak state diverged: broker=%v direct=%v selected=%v", brokerEligible, directEligible, direct)
	}
}

func TestPriorityStreakAndBrokerShareEveryBoundaryVerdict(t *testing.T) {
	for _, minutes := range []float64{0, 7, 8, 15, 19, 20} {
		for _, directFirst := range []bool{false, true} {
			name := fmt.Sprintf("%.0fm/directFirst=%v", minutes, directFirst)
			t.Run(name, func(t *testing.T) {
				w, online := newTestWatcher(1)
				w.priorities = []config.Priority{config.PriorityStreak}
				s := w.streamers[0]
				s.Stream.Update("b1", "t", nil, nil, 1)
				s.Stream.MinuteWatched = minutes

				var direct, broker bool
				if directFirst {
					selected := w.selectByPriority(online)
					direct = len(selected) == 1 && selected[0] == 0
					broker = w.isBoostEligible(0)
				} else {
					broker = w.isBoostEligible(0)
					selected := w.selectByPriority(online)
					direct = len(selected) == 1 && selected[0] == 0
				}
				want := minutes < models.WatchStreakPursuitCapMinutes
				if direct != want || broker != want || direct != broker {
					t.Fatalf("direct=%v broker=%v want=%v", direct, broker, want)
				}
			})
		}
	}
}

func TestWatcherPersistsFirstTimeoutTransitionExactlyOnce(t *testing.T) {
	w, _ := newTestWatcher(1)
	s := w.streamers[0]
	s.Stream.Update("b1", "t", nil, nil, 1)
	s.Stream.MinuteWatched = 20

	var calls int
	var snapshot models.WatchStreakPersistence
	w.SetOnWatchStreakTransition(func(got *models.Streamer, state models.WatchStreakPersistence) {
		if got != s {
			t.Errorf("callback streamer=%p, want %p", got, s)
		}
		calls++
		snapshot = state
	})
	firstEligible := w.isBoostEligible(0)
	secondEligible := w.isBoostEligible(0)
	exhausted := w.streakPursuitExhausted(0)
	if firstEligible || secondEligible || !exhausted {
		t.Fatal("timed-out broadcast eligibility changed across repeated consumers")
	}
	if calls != 1 || snapshot.Revision != 1 || snapshot.Timeout == nil || snapshot.Timeout.BroadcastID != "b1" {
		t.Fatalf("callback calls=%d snapshot=%+v, want one exact b1 timeout", calls, snapshot)
	}
	if snapshot.Timeout.TimedOutAt.After(time.Now()) {
		t.Fatalf("timeout timestamp is in the future: %v", snapshot.Timeout.TimedOutAt)
	}
}
