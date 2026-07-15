package api

import (
	"sync"
	"time"
)

// eventWindow is a small, self-synchronized sliding window of event timestamps.
// Producers call mark() from arbitrary goroutines (the GQL request path runs on
// watcher/drops/discovery/canary goroutines concurrently); the health watchdog
// calls count() once per minute. A plain atomic counter cannot express age-out,
// so a mutex-guarded ring is used: entries older than the query window are
// pruned on read, and the buffer is hard-capped so a window that is never read
// cannot grow without bound.
type eventWindow struct {
	mu    sync.Mutex
	times []time.Time
}

// eventWindowCap bounds retained timestamps; the cap only guards against
// unbounded growth if count() is never called.
const eventWindowCap = 64

func (w *eventWindow) mark(t time.Time) {
	w.mu.Lock()
	w.times = append(w.times, t)
	if len(w.times) > eventWindowCap {
		// Drop the oldest, keep the newest eventWindowCap. copy handles the
		// overlapping source/destination correctly (memmove semantics).
		copy(w.times, w.times[len(w.times)-eventWindowCap:])
		w.times = w.times[:eventWindowCap]
	}
	w.mu.Unlock()
}

// count returns how many marks fall within the trailing window ending at now,
// pruning anything older in place.
func (w *eventWindow) count(now time.Time, window time.Duration) int {
	cutoff := now.Add(-window)
	w.mu.Lock()
	defer w.mu.Unlock()
	kept := w.times[:0]
	for _, t := range w.times {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	w.times = kept
	return len(w.times)
}
