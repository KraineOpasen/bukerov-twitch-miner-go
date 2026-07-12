package watcher

import (
	"testing"
	"time"

	"github.com/PatrickWalther/twitch-miner-go/internal/config"
	"github.com/PatrickWalther/twitch-miner-go/internal/models"
)

func newTestWatcher(n int) (*MinuteWatcher, []int) {
	streamers := make([]*models.Streamer, n)
	online := make([]int, n)
	for i := range streamers {
		streamers[i] = models.NewStreamer("streamer"+string(rune('a'+i)), models.DefaultStreamerSettings())
		online[i] = i
	}
	w := &MinuteWatcher{
		streamers:  streamers,
		priorities: []config.Priority{config.PriorityOrder},
		settings:   config.RateLimitSettings{RotationInterval: 900},
	}
	return w, online
}

func TestBuildRotationScheduleEven(t *testing.T) {
	order := []int{0, 1, 2, 3}
	schedule := buildRotationSchedule(order)

	if len(schedule) != 2 {
		t.Fatalf("expected 2 disjoint pairs for 4 streamers, got %d", len(schedule))
	}

	seen := make(map[int]int)
	for _, pair := range schedule {
		seen[pair[0]]++
		seen[pair[1]]++
	}
	for _, idx := range order {
		if seen[idx] != 1 {
			t.Errorf("streamer %d appears %d times in one cycle, want exactly 1 (even split must be disjoint)", idx, seen[idx])
		}
	}
}

func TestBuildRotationScheduleOdd(t *testing.T) {
	order := []int{0, 1, 2, 3, 4}
	schedule := buildRotationSchedule(order)

	if len(schedule) != 5 {
		t.Fatalf("expected 5 sliding-window pairs for 5 streamers, got %d", len(schedule))
	}

	seen := make(map[int]int)
	for _, pair := range schedule {
		if pair[0] == pair[1] {
			t.Fatalf("pair must not contain the same streamer twice: %v", pair)
		}
		seen[pair[0]]++
		seen[pair[1]]++
	}
	for _, idx := range order {
		if seen[idx] != 2 {
			t.Errorf("streamer %d appears %d times in one cycle, want exactly 2 (odd sliding window)", idx, seen[idx])
		}
	}
}

func TestSelectRotatingCoversEveryoneWithinOneCycle(t *testing.T) {
	for _, n := range []int{4, 5, 7, 8} {
		w, online := newTestWatcher(n)

		watchedCount := make(map[int]int)
		now := time.Now()

		// One full cycle: n ticks is always enough for both the disjoint
		// (n/2 ticks) and sliding-window (n ticks) schedules to wrap around.
		for tick := 0; tick < n; tick++ {
			pair := w.selectRotating(online)
			if len(pair) != 2 {
				t.Fatalf("n=%d tick=%d: expected 2 streamers selected, got %d", n, tick, len(pair))
			}
			for _, idx := range pair {
				watchedCount[idx]++
			}
			// Force the next tick to rotate.
			w.rotation.lastSwitch = now.Add(-2 * time.Hour)
		}

		for _, idx := range online {
			if watchedCount[idx] == 0 {
				t.Errorf("n=%d: streamer %d was never watched within one full cycle", n, idx)
			}
		}
	}
}

func TestSelectRotatingNoRotationBelowLimit(t *testing.T) {
	w, online := newTestWatcher(2)
	got := w.selectStreamersToWatch(online)
	if len(got) != 2 {
		t.Fatalf("expected both online streamers watched with count <= max, got %d", len(got))
	}
}

func TestApplyPriorityBoostSwapsInDropsStreamer(t *testing.T) {
	w, online := newTestWatcher(3)
	// streamer 2 has an active drop campaign but isn't in the base pair.
	w.streamers[2].Stream.CampaignIDs = []string{"campaign-1"}

	w.rebuildRotation(online, time.Now())
	pair := w.rotation.schedule[w.rotation.pos]

	// Mark the base pair as watched more recently than the boosted streamer,
	// so the boost logic has a clear victim to pick.
	w.rotation.lastWatched = map[int]time.Time{
		pair[0]: time.Now(),
		pair[1]: time.Now().Add(-time.Minute),
	}

	boosted := w.applyPriorityBoost(pair)
	if boosted[0] != 2 && boosted[1] != 2 {
		t.Fatalf("expected drops-eligible streamer 2 to be swapped into the pair, got %v", boosted)
	}
}

func TestNearStreakCompletionProtectsFromSwap(t *testing.T) {
	w, online := newTestWatcher(3)
	w.streamers[2].Stream.CampaignIDs = []string{"campaign-1"}

	w.rebuildRotation(online, time.Now())
	pair := w.rotation.schedule[w.rotation.pos]

	// Both current pair members are seconds away from completing their
	// watch streak; neither should be sacrificed for the boost.
	w.streamers[pair[0]].Stream.MinuteWatched = 6.5
	w.streamers[pair[1]].Stream.MinuteWatched = 6.8

	boosted := w.applyPriorityBoost(pair)
	if boosted != pair {
		t.Fatalf("expected pair unchanged when both members are near streak completion, got %v want %v", boosted, pair)
	}
}
