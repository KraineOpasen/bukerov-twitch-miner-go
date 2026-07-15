package api

import (
	"sync"
	"testing"
	"time"
)

func TestRecentGQLFailures(t *testing.T) {
	c := &TwitchClient{}
	now := time.Now()
	c.gqlFailures.mark(now.Add(-1 * time.Minute))
	c.gqlFailures.mark(now.Add(-2 * time.Minute))
	c.gqlFailures.mark(now.Add(-40 * time.Minute)) // outside a 15m window

	if got := c.RecentGQLFailures(15 * time.Minute); got != 2 {
		t.Fatalf("RecentGQLFailures(15m) = %d, want 2", got)
	}
	if got := c.RecentGQLFailures(time.Hour); got != 2 {
		t.Fatalf("RecentGQLFailures(1h) after prune = %d, want 2", got)
	}
}

// TestGQLFailureWindowConcurrent mirrors the real access pattern: the GQL
// request path (many goroutines) marks failures while the watchdog reads. Must
// be clean under -race.
func TestGQLFailureWindowConcurrent(t *testing.T) {
	c := &TwitchClient{}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				c.gqlFailures.mark(time.Now())
				_ = c.RecentGQLFailures(time.Minute)
			}
		}()
	}
	wg.Wait()
}
