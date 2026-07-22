package watcher

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// normalizePair returns the two watched indexes in ascending order so a watched
// set can be compared regardless of slot order.
func normalizePair(p [2]int) [2]int {
	if p[0] > p[1] {
		return [2]int{p[1], p[0]}
	}
	return p
}

// TestRotationDwellIntervalWithinDocumentedRange pins the documented behaviour:
// the fair-rotation dwell time drawn for a watch pair must land inside the
// 30-80 minute window (the config defaults), and must never collapse to the
// ~2-minute churn seen in production even when the interval settings are unset.
// A short dwell rotates channels out before the ~7 continuous minutes a watch
// streak needs, so this range is load-bearing for streak crediting.
func TestRotationDwellIntervalWithinDocumentedRange(t *testing.T) {
	w, _ := newTestWatcher(5)

	// Drive the test from the real config defaults so it tracks the documented
	// 30-80 minute window rather than a hard-coded copy of it.
	defs := config.DefaultRateLimitSettings()
	if defs.RotationIntervalMinMinutes != 30 || defs.RotationIntervalMaxMinutes != 80 {
		t.Fatalf("documented default dwell window changed: got %d-%d, expected 30-80",
			defs.RotationIntervalMinMinutes, defs.RotationIntervalMaxMinutes)
	}
	w.settings = defs

	const lo, hi = 30 * time.Minute, 80 * time.Minute
	for i := 0; i < 5000; i++ {
		d := w.randomRotationInterval()
		if d < lo || d > hi {
			t.Fatalf("dwell interval %v is outside the documented 30-80 minute range", d)
		}
	}

	// Unset interval settings must fall back to a multi-minute dwell, never the
	// ~2-minute rotation the production regression exhibited.
	w.settings.RotationIntervalMinMinutes = 0
	w.settings.RotationIntervalMaxMinutes = 0
	if d := w.randomRotationInterval(); d < 30*time.Minute {
		t.Fatalf("unset dwell settings fell back to %v; expected >= 30 minutes (never a ~2-minute churn)", d)
	}
}

// TestRotationHoldsPairForItsDwell guards the effective dwell: once a base pair
// is chosen it must be held for its whole (>=30 min) interval, not recomputed on
// the next ~1-minute tick. selectRotating is called repeatedly with no elapsed
// time; the stored pair must not change.
func TestRotationHoldsPairForItsDwell(t *testing.T) {
	w, online := newTestWatcher(5)
	w.settings = config.DefaultRateLimitSettings()

	first := w.selectRotating(online)
	firstPair := w.rotation.activePair
	if w.rotation.nextInterval < 30*time.Minute || w.rotation.nextInterval > 80*time.Minute {
		t.Fatalf("dwell interval chosen for the pair is %v, outside 30-80 minutes", w.rotation.nextInterval)
	}

	for tick := 0; tick < 20; tick++ {
		w.selectRotating(online)
		if w.rotation.activePair != firstPair {
			t.Fatalf("tick %d: base pair rotated to %v within its dwell window (was %v); the pair must be held for its 30-80 minute interval",
				tick, w.rotation.activePair, firstPair)
		}
	}
	_ = first
}

// TestBoostKeepsWatchedSetStableAcrossTicks is the core streak-regression guard.
// With more streak-eligible channels online than watch slots and a base pair
// pinned inside its dwell, the per-tick priority boost must NOT rotate the
// watched set every tick. Before the continuity latch it re-picked the
// least-recently-watched eligible channel every tick, so the watched set
// changed on every single tick: no channel was ever watched on consecutive
// ticks, MinuteWatched (which only counts CONTINUOUS viewing) was perpetually
// reset to 0, and no watch streak ever completed.
func TestBoostKeepsWatchedSetStableAcrossTicks(t *testing.T) {
	w, online := newTestWatcher(5) // 5 > MaxSimultaneousStreams (2): rotation territory
	for _, s := range w.streamers {
		s.Settings.WatchStreak = true
		s.Stream.WatchStreakMissing = true
		s.Stream.MinuteWatched = 0 // as in production while continuity keeps breaking
	}
	// Pin a stable base pair far inside its dwell window; only the boost can move
	// the watched set.
	w.rotation.hasPair = true
	w.rotation.activePair = [2]int{0, 1}
	w.rotation.nextInterval = time.Hour
	w.rotation.lastSwitch = time.Now()
	w.rotation.lastWatched = map[int]time.Time{}

	base := time.Now().Add(-time.Hour)
	var first [2]int
	for tk := 0; tk < 15; tk++ {
		pair := w.applyPriorityBoost(w.rotation.activePair, online)
		set := normalizePair(pair)
		if tk == 0 {
			first = set
		} else if set != first {
			t.Fatalf("tick %d: watched set churned to %v (started %v); the boost must hold a stable watched set so continuous viewing can accumulate",
				tk, set, first)
		}
		// Mirror selectRotating: the returned pair is what actually got watched.
		now := base.Add(time.Duration(tk) * time.Minute)
		w.rotation.lastWatched[pair[0]] = now
		w.rotation.lastWatched[pair[1]] = now
	}
}

// TestBoostHandsOffWhenTargetNoLongerEligible ensures the continuity latch is
// not a permanent lock: once the boosted channel earns its streak (stops being
// eligible), the seat hands off to the next eligible channel instead of staying
// pinned to the finished one.
func TestBoostHandsOffWhenTargetNoLongerEligible(t *testing.T) {
	w, online := newTestWatcher(4)
	for _, s := range w.streamers {
		s.Settings.WatchStreak = true
		s.Stream.WatchStreakMissing = true
	}
	w.rotation.hasPair = true
	w.rotation.activePair = [2]int{0, 1}
	now := time.Now()
	w.rotation.lastWatched = map[int]time.Time{
		0: now, 1: now,
		2: now.Add(-2 * time.Hour), // least recently watched -> first boost target
		3: now.Add(-time.Hour),
	}

	w.applyPriorityBoost(w.rotation.activePair, online)
	held := w.rotation.boostTarget
	if held != 2 {
		t.Fatalf("expected streamer 2 (least-recently-watched eligible) to take the boost seat, got %d", held)
	}

	// The boosted channel earns its streak and is no longer boost-eligible.
	w.streamers[held].Stream.WatchStreakMissing = false

	pair := w.applyPriorityBoost(w.rotation.activePair, online)
	if pair[0] == held || pair[1] == held {
		t.Fatalf("boost stayed latched to streamer %d after it earned its streak; must hand off, got %v", held, pair)
	}
	if w.rotation.boostTarget != 3 {
		t.Fatalf("expected hand-off to the next eligible streamer 3, got boostTarget=%d", w.rotation.boostTarget)
	}
}

// TestBoostLatchYieldsToRestrictedDrop keeps drop priority intact through the
// continuity latch: a channel-restricted drop that appears mid-dwell must
// immediately preempt a latched (equal-or-lower-priority) streak target, since
// its progress can only ever be earned on that exact channel.
func TestBoostLatchYieldsToRestrictedDrop(t *testing.T) {
	w, online := newTestWatcher(5)
	for _, s := range w.streamers {
		s.Settings.WatchStreak = true
		s.Stream.WatchStreakMissing = true
	}
	w.rotation.hasPair = true
	w.rotation.activePair = [2]int{0, 1}
	now := time.Now()
	w.rotation.lastWatched = map[int]time.Time{
		0: now, 1: now,
		2: now.Add(-3 * time.Hour), // least recently watched -> latched first
		3: now.Add(-time.Hour),
		4: now.Add(-time.Minute),
	}

	w.applyPriorityBoost(w.rotation.activePair, online)
	if w.rotation.boostTarget != 2 {
		t.Fatalf("expected streak target 2 to be latched first, got %d", w.rotation.boostTarget)
	}

	// Streamer 4 now has a channel-restricted campaign.
	w.streamers[4].Stream.SetCampaignIDs([]string{"restricted"})
	w.streamers[4].Stream.Campaigns = []*models.Campaign{
		{ID: "restricted", Channels: []string{w.streamers[4].ChannelID}},
	}

	pair := w.applyPriorityBoost(w.rotation.activePair, online)
	if pair[0] != 4 && pair[1] != 4 {
		t.Fatalf("channel-restricted drop on streamer 4 must preempt the latched streak target, got %v", pair)
	}
}

// TestRotationSlotClassifiedAsFairRotation covers the reason-code labelling: a
// plain configured slot held under the fair watch-pair rotation must report
// reasonCode=fair_rotation (not the misleading "priority" it reported before),
// while a direct-mode configured slot still reports priority.
func TestRotationSlotClassifiedAsFairRotation(t *testing.T) {
	w, _ := newTestWatcher(5)
	s := w.streamers[0] // no drops, streak not in progress (MinuteWatched == 0)

	w.selectionMode = ModeRotation
	if rc, _, _ := w.classify(s, OriginConfigured, 0); rc != ReasonFairRotation {
		t.Fatalf("rotation-mode configured slot classified as %q, want %q", rc, ReasonFairRotation)
	}

	w.selectionMode = ModeDirect
	if rc, _, _ := w.classify(s, OriginConfigured, 0); rc != ReasonPriority {
		t.Fatalf("direct-mode configured slot classified as %q, want %q", rc, ReasonPriority)
	}
}
