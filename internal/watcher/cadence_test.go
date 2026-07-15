package watcher

import (
	"testing"
	"time"
)

// TestSettleWait locks the watch-cadence fix: loop() must wait only the
// REMAINDER of the interval after a processWatching tick, never a second full
// interval on top of the ~interval the paced send loop already consumed.
//
// The bug it guards against: waiting randomizedDelay(interval) unconditionally
// made each channel report once per ~2*interval, so the gap between a channel's
// consecutive reports averaged 2*interval == maxContinuousGap (watcher.go's
// `2 * interval`). Jitter then pushed the gap past the threshold roughly half
// the time even for a continuously-watched channel, resetting MinuteWatched to
// 0 (stream.go) so watch streaks never accumulated. Waiting only the remainder
// restores the ~interval cadence the code's own comment already assumes.
func TestSettleWait(t *testing.T) {
	const interval = 60 * time.Second
	cases := []struct {
		name  string
		spent time.Duration
		want  time.Duration
	}{
		{"fully paced tick adds no second wait", interval, 0},
		{"paced tick slightly over adds no wait", interval + 3*time.Second, 0},
		{"idle tick with no slots waits ~full interval", 5 * time.Millisecond, interval - 5*time.Millisecond},
		{"partly paced tick waits the remainder", 40 * time.Second, 20 * time.Second},
		{"zero spent waits the full interval", 0, interval},
	}
	for _, tc := range cases {
		if got := settleWait(interval, tc.spent); got != tc.want {
			t.Errorf("%s: settleWait(%v, %v) = %v, want %v", tc.name, interval, tc.spent, got, tc.want)
		}
	}
}

// TestSettleWaitNeverExceedsInterval is the core anti-regression invariant:
// whatever a tick spent, the follow-up idle can never itself be a full second
// interval unless the tick did essentially no work — i.e. the double-wait can
// never reappear. For any non-trivial paced tick (spent grows toward interval)
// the remainder shrinks toward 0.
func TestSettleWaitNeverExceedsInterval(t *testing.T) {
	const interval = 60 * time.Second
	for spent := time.Duration(0); spent <= 2*interval; spent += 5 * time.Second {
		got := settleWait(interval, spent)
		if got < 0 {
			t.Fatalf("settleWait(%v, %v) = %v, must never be negative", interval, spent, got)
		}
		if got > interval {
			t.Fatalf("settleWait(%v, %v) = %v, must never exceed the interval", interval, spent, got)
		}
		if want := interval - spent; spent < interval && got != want {
			t.Fatalf("settleWait(%v, %v) = %v, want remainder %v", interval, spent, got, want)
		}
		if spent >= interval && got != 0 {
			t.Fatalf("settleWait(%v, %v) = %v, want 0 once the tick already spanned the interval", interval, spent, got)
		}
	}
}
