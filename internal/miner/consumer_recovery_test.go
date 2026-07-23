package miner

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/auth"
)

// Review regression: a fast consumer rejection loop (IRC redials every
// second) must not spawn one recovery observer per event — at most ONE
// observer exists at a time and new observations start at most once per
// cooldown window, so neither goroutines nor OAuth traffic can grow with the
// event rate.
func TestConsumerRecoveryObserverBoundedAndThrottled(t *testing.T) {
	m := &Miner{auth: auth.NewTwitchAuth("tester", "device-xyz")}

	var calls atomic.Int64
	release := make(chan struct{})
	m.authRecoverFn = func(ctx context.Context, gen uint64) error {
		calls.Add(1)
		<-release
		return nil
	}

	var wg sync.WaitGroup
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.recoverFromRejectedGeneration(0, "test")
		}()
	}
	wg.Wait()

	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("recovery observations = %d, want exactly 1 for 100 rapid rejections", got)
	}

	close(release)
	// Wait for the observer slot to free, then hit the cooldown gate: a new
	// rejection immediately after a completed observation must NOT start
	// another one within the cooldown window.
	for m.authRecoveryObserver.Load() && time.Now().Before(deadline) {
	}
	m.recoverFromRejectedGeneration(0, "test")
	if got := calls.Load(); got != 1 {
		t.Fatalf("cooldown did not throttle a fresh observation: %d calls", got)
	}

	// A stale rejection (generation already rotated past) never observes.
	m.authRecoveryLastStart.Store(0) // cooldown elapsed
	m.auth.SetToken("rotated")       // bumps the generation past 0
	m.recoverFromRejectedGeneration(0, "test")
	if got := calls.Load(); got != 1 {
		t.Fatalf("stale rejection started an observation: %d calls", got)
	}
}
