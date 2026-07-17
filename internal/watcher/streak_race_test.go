package watcher

import (
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestWatchStreakFieldOwnershipRace drives the three REAL production paths
// that touch Stream.WatchStreakMissing concurrently, so `go test -race`
// proves the field has a single owner:
//
//   - the watcher loop's boost predicates (isBoostEligible/streakInProgress),
//     which historically read the field bare, without any lock;
//   - the PubSub points-earned path (UpdateHistory("WATCH_STREAK")), which
//     wrote it under the STREAMER mutex (a different lock);
//   - the online-transition re-arm (InitWatchStreak), which writes it under
//     the STREAM mutex.
//
// Two different mutexes plus a bare read is a data race even when tests
// happen to pass: this test makes -race exercise all three paths at once. It
// fails with WARNING: DATA RACE on the pre-fix code and must stay clean once
// the field is owned exclusively by Stream.mu.
func TestWatchStreakFieldOwnershipRace(t *testing.T) {
	s := models.NewStreamer("racer", models.DefaultStreamerSettings())
	s.IsOnline = true

	w := &MinuteWatcher{
		streamers:  []*models.Streamer{s},
		priorities: []config.Priority{config.PriorityOrder},
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(3)

	// Watcher-loop reader.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			w.isBoostEligible(0)
			w.streakInProgress(0)
		}
	}()

	// PubSub points-earned writer (grant clears the flag).
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.UpdateHistory("WATCH_STREAK", 300)
		}
	}()

	// Online-transition re-arm writer.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.Stream.InitWatchStreak()
		}
	}()

	time.Sleep(80 * time.Millisecond)
	close(stop)
	wg.Wait()
}
