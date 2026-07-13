package watcher

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// mutableSource is a candidate source whose proposal list can be swapped
// concurrently, standing in for directory discovery refreshing its pick from
// its own goroutine while the broker reads it from the loop goroutine.
type mutableSource struct {
	cand atomic.Pointer[[]Candidate]
}

func (s *mutableSource) SourceName() string { return "discovery" }
func (s *mutableSource) WatchCandidates() []Candidate {
	if c := s.cand.Load(); c != nil {
		return *c
	}
	return nil
}
func (s *mutableSource) set(c []Candidate) { s.cand.Store(&c) }

// TestBrokerConcurrentRefreshSettingsSnapshot runs the real production access
// patterns concurrently under -race: the loop goroutine (processWatching),
// runtime settings updates (UpdateSettings), a source refreshing its proposals
// (discovery), and dashboard/debug readers (BrokerSnapshot / GetDebugState /
// IsWatching). It guards the broker's mu/atomic discipline.
func TestBrokerConcurrentRefreshSettingsSnapshot(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 64)}
	checker := &staticChecker{checked: make(chan string, 64)}
	w, _ := newLoopWatcher(4, sender, checker)

	src := &mutableSource{}
	src.set([]Candidate{{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery}})
	w.AddSource(src)

	ctx, cancel := context.WithCancel(context.Background())
	w.ctx = ctx

	// Drain the buffered fake channels so nothing blocks over many iterations.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sender.sent:
			case <-checker.checked:
			}
		}
	}()

	const iters = 3000
	var wg sync.WaitGroup
	wg.Add(4)

	go func() { // broker loop
		defer wg.Done()
		for i := 0; i < iters; i++ {
			w.processWatching()
		}
	}()
	go func() { // runtime settings updates
		defer wg.Done()
		for i := 0; i < iters; i++ {
			w.UpdateSettings([]config.Priority{config.PriorityDrops}, config.RateLimitSettings{
				MinuteWatchedInterval:      1,
				RotationIntervalMinMinutes: 1,
				RotationIntervalMaxMinutes: 1,
			})
		}
	}()
	go func() { // source refreshing its proposal
		defer wg.Done()
		alt := discoveryStreamer("disco2", false)
		for i := 0; i < iters; i++ {
			if i%2 == 0 {
				src.set([]Candidate{{Streamer: alt, Origin: OriginDiscovery}})
			} else {
				src.set(nil)
			}
		}
	}()
	go func() { // dashboard/debug readers
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = w.BrokerSnapshot()
			_ = w.GetDebugState()
			_ = w.IsWatching("disco")
		}
	}()

	wg.Wait()
	cancel()
}

// TestConcurrentSettingsUpdateNoLostState hammers UpdateSettings from many
// goroutines while the loop applies them, asserting the final applied state is
// one of the written values (never torn).
func TestConcurrentSettingsUpdateNoLostState(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 16)}
	checker := &staticChecker{checked: make(chan string, 16)}
	w, _ := newLoopWatcher(1, sender, checker)

	go func() {
		for range sender.sent {
		}
	}()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(interval int) {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				w.UpdateSettings([]config.Priority{config.PriorityOrder}, config.RateLimitSettings{
					MinuteWatchedInterval:      interval,
					RotationIntervalMinMinutes: 1,
					RotationIntervalMaxMinutes: 1,
				})
			}
		}(30 + g)
	}

	ctx, cancel := context.WithCancel(context.Background())
	w.ctx = ctx
	for i := 0; i < 500; i++ {
		w.processWatching()
		time.Sleep(time.Microsecond)
	}
	wg.Wait()
	cancel()
	close(sender.sent)

	// The applied interval must be a fully-written value (30..37), never torn.
	if w.settings.MinuteWatchedInterval < 30 || w.settings.MinuteWatchedInterval > 37 {
		t.Fatalf("applied interval %d is not one of the written values", w.settings.MinuteWatchedInterval)
	}
}
