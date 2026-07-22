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
	s.SetConfirmedOnline()

	// A second, static mid-pursuit streamer so the reader goroutine reaches
	// betterBoostCandidate's both-in-progress tie-break — the MinuteWatched
	// comparison — every iteration in which the raced streamer is also
	// mid-pursuit.
	other := models.NewStreamer("static", models.DefaultStreamerSettings())
	other.SetConfirmedOnline()
	// UpdateMinuteWatched's first call only sets the baseline; the second call
	// banks the (tiny, >0) gap — enough for streakInProgress's mw > 0.
	other.Stream.Update("bid-static", "t", nil, nil, 1)
	other.Stream.UpdateMinuteWatched(time.Hour)
	other.Stream.UpdateMinuteWatched(time.Hour)

	s.Stream.Update("bid-raced", "t", nil, nil, 1)

	// A third streamer that stays STABLY mid-pursuit (no grants, no re-arms)
	// while its MinuteWatched keeps being advanced by its own writer, so the
	// reader passes strictlyHigherBoost's both-in-progress gate on every
	// iteration and hits the tie-break's field reads (the R1 site).
	accruing := models.NewStreamer("accruing", models.DefaultStreamerSettings())
	accruing.SetConfirmedOnline()
	accruing.Stream.Update("bid-accruing", "t", nil, nil, 1)
	accruing.Stream.UpdateMinuteWatched(time.Hour)
	accruing.Stream.UpdateMinuteWatched(time.Hour)

	w := &MinuteWatcher{
		streamers:  []*models.Streamer{s, other, accruing},
		priorities: []config.Priority{config.PriorityOrder},
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(4)

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
			// Both boost tie-breaks compare the streamers' MinuteWatched
			// whenever both are mid-pursuit; strictlyHigherBoost is the R1
			// site that still read the field bare.
			w.betterBoostCandidate(0, 1)
			w.strictlyHigherBoost(0, 1)
			w.strictlyHigherBoost(2, 1)
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

	// Online-transition re-arm writer, alternated with minute accrual so the
	// raced streamer keeps flipping in and out of the mid-pursuit state the
	// tie-break requires.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			s.Stream.InitWatchStreak()
			s.Stream.UpdateMinuteWatched(time.Hour) // baseline
			s.Stream.UpdateMinuteWatched(time.Hour) // banks >0 -> mid-pursuit
		}
	}()

	// Minute-accrual writer for the stably mid-pursuit streamer.
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			accruing.Stream.UpdateMinuteWatched(time.Hour)
		}
	}()

	time.Sleep(80 * time.Millisecond)
	close(stop)
	wg.Wait()
}
