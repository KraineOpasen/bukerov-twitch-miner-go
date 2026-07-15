package pubsub

import (
	"sync"
	"testing"
	"time"
)

func TestEventWindowCountAndAgeOut(t *testing.T) {
	var w eventWindow
	now := time.Now()
	w.mark(now.Add(-1 * time.Minute))
	w.mark(now.Add(-2 * time.Minute))
	w.mark(now.Add(-3 * time.Minute))
	w.mark(now.Add(-20 * time.Minute))
	w.mark(now.Add(-30 * time.Minute))

	if got := w.count(now, 15*time.Minute); got != 3 {
		t.Fatalf("count within 15m = %d, want 3", got)
	}
	// count prunes in place, so the two old marks are gone: a wider window sees 3.
	if got := w.count(now, time.Hour); got != 3 {
		t.Fatalf("count after prune = %d, want 3", got)
	}
}

func TestEventWindowCap(t *testing.T) {
	var w eventWindow
	now := time.Now()
	for i := 0; i < eventWindowCap+50; i++ {
		w.mark(now)
	}
	if got := w.count(now, time.Minute); got != eventWindowCap {
		t.Fatalf("capped count = %d, want %d", got, eventWindowCap)
	}
}

// TestEventWindowConcurrent exercises the mutex under -race: many goroutines
// mark while others count.
func TestEventWindowConcurrent(t *testing.T) {
	var w eventWindow
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				w.mark(time.Now())
				_ = w.count(time.Now(), time.Minute)
			}
		}()
	}
	wg.Wait()
}
