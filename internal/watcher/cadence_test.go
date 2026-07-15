package watcher

import (
	"context"
	"sort"
	"testing"
	"time"
)

// TestLoopCadenceIsOneIntervalNotTwo is the regression for issue #3: a
// continuously-watched channel must be reported about once per
// MinuteWatchedInterval, not once per 2×interval.
//
// processWatching already spreads a tick's per-slot sends across ~one interval
// via pace(interval/len(slots)); loop() must then wait only the REMAINDER of
// the interval, never a second full one. With a single slot and a pacer that
// sleeps the exact inter-slot duration the send loop requests (which is
// interval/1 == interval for one slot), consecutive reports land ~interval
// apart under the fix and ~2×interval apart under the old double-wait.
func TestLoopCadenceIsOneIntervalNotTwo(t *testing.T) {
	sender := &timestampingSender{}
	checker := &staticChecker{checked: make(chan string, 64)}
	w, _ := newLoopWatcher(1, sender, checker) // MinuteWatchedInterval = 1s

	// Real pacing: sleep exactly the duration the send loop asks for, so the
	// intra-tick spreading genuinely consumes ~one interval (as it does in
	// production via randomizedDelay). The old bug then added a second full
	// interval in loop(); the fix adds only the ~0 remainder.
	w.pacer = func(d time.Duration) bool { time.Sleep(d); return true }

	ctx, cancel := context.WithCancel(context.Background())
	w.ctx = ctx

	done := make(chan struct{})
	go func() { w.loop(); close(done) }()

	time.Sleep(4200 * time.Millisecond)
	cancel()
	<-done // loop has exited: no further sends will be recorded

	sender.mu.Lock()
	stamps := append([]time.Time(nil), sender.sends...)
	sender.mu.Unlock()

	if len(stamps) < 3 {
		t.Fatalf("expected at least 3 minute-watched reports in the ~4.2s window at a 1s interval, got %d", len(stamps))
	}

	gaps := make([]time.Duration, 0, len(stamps)-1)
	for i := 1; i < len(stamps); i++ {
		gaps = append(gaps, stamps[i].Sub(stamps[i-1]))
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i] < gaps[j] })
	median := gaps[len(gaps)/2]

	// interval is 1s. The fix keeps the median report gap ~1s (up to ~1.2s with
	// the ±20% jitter that now lives on the single remainder wait). The old
	// double-wait made it ~2s. 1.5s cleanly separates the two cases, with margin
	// on both sides so ordinary scheduling noise cannot flip the verdict.
	const interval = time.Second
	if median > 1500*time.Millisecond {
		t.Fatalf("median report gap %v exceeds 1.5×interval (%v): the watch cycle is double-waiting (issue #3 regression)", median, interval)
	}
	// Guard the opposite failure too: if pacing were skipped entirely the loop
	// would busy-spin and reports would bunch far below one interval apart.
	if median < 700*time.Millisecond {
		t.Fatalf("median report gap %v is implausibly short for a %v interval: pacing/cadence broke", median, interval)
	}
}
