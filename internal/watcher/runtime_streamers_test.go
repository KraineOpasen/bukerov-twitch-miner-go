package watcher

import (
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// newOnlineStreamer builds a configured streamer that is immediately a watch
// candidate (online, settled, defaults).
func newOnlineStreamer(name string) *models.Streamer {
	s := models.NewStreamer(name, models.DefaultStreamerSettings())
	s.SetConfirmedOnline()
	// Backdate past the 30s "settle" guard so it is immediately a candidate.
	s.OnlineAt = time.Now().Add(-time.Minute)
	// Normal points-enabled channel (capability confirmed at startup in prod).
	s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
	return s
}

// TestUpdateStreamersAddsCandidateOnNextTick is the delivery guard for the
// runtime-add bug: a streamer added via UpdateStreamers must become a watch
// candidate at the very next tick boundary (applyPendingSettings), without a
// restart. Reverting the staged-list application makes this fail: the loop
// would keep selecting from the startup snapshot forever.
func TestUpdateStreamersAddsCandidateOnNextTick(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.streamers[0].SetConfirmedOnline()
	w.streamers[0].OnlineAt = time.Now().Add(-time.Minute)

	added := newOnlineStreamer("streamerz")
	newList := append(append([]*models.Streamer(nil), w.streamers...), added)
	w.UpdateStreamers(newList)

	// Staging alone must not touch the loop-owned list mid-tick.
	if len(w.streamers) != 1 {
		t.Fatalf("staged update mutated the loop-owned list before the tick boundary: len=%d", len(w.streamers))
	}

	w.applyPendingSettings()

	if len(w.streamers) != 2 {
		t.Fatalf("staged streamer list not applied at the tick boundary: len=%d, want 2", len(w.streamers))
	}
	online := w.getOnlineStreamers(nil)
	if len(online) != 2 {
		t.Fatalf("added streamer is not an online candidate: got %d candidates, want 2", len(online))
	}
	watched := w.selectStreamersToWatch(online)
	found := false
	for _, idx := range watched {
		if w.streamers[idx] == added {
			found = true
		}
	}
	if !found {
		t.Fatal("runtime-added streamer was not selected for a watch slot on the next tick")
	}
}

// TestUpdateStreamersRemovalFreesSlotOnNextTick covers the soft-release
// semantics: a removed streamer simply stops being a candidate on the next
// tick, so per-tick slot recomputation frees its slot without any external
// mutation of loop state.
func TestUpdateStreamersRemovalFreesSlotOnNextTick(t *testing.T) {
	w, online := newTestWatcher(2)
	for _, s := range w.streamers {
		s.SetConfirmedOnline()
		s.OnlineAt = time.Now().Add(-time.Minute)
	}

	watched := w.selectStreamersToWatch(online)
	if len(watched) != 2 {
		t.Fatalf("precondition: both online streamers should hold slots, got %d", len(watched))
	}

	kept, removed := w.streamers[0], w.streamers[1]
	w.UpdateStreamers([]*models.Streamer{kept})
	w.applyPendingSettings()

	if len(w.streamers) != 1 || w.streamers[0] != kept {
		t.Fatalf("removal not applied: list=%d", len(w.streamers))
	}
	online2 := w.getOnlineStreamers(nil)
	watched2 := w.selectStreamersToWatch(online2)
	for _, idx := range watched2 {
		if w.streamers[idx] == removed {
			t.Fatal("removed streamer still selected for a watch slot after the tick boundary")
		}
	}
	if len(watched2) != 1 || w.streamers[watched2[0]] != kept {
		t.Fatalf("remaining streamer should keep a slot, got %d selections", len(watched2))
	}
}

// TestUpdateStreamersRemapsIndexStateByUsername pins the remap invariant: all
// index-keyed loop state (rotation pair, fairness recency, swap-out deferrals,
// streak log bookkeeping) must follow each streamer to its NEW index when the
// list is reordered/shrunk, never stay positional. Removing the remap makes
// this fail with state attributed to the wrong streamers.
func TestUpdateStreamersRemapsIndexStateByUsername(t *testing.T) {
	w, _ := newTestWatcher(4) // indexes: 0=a 1=b 2=c 3=d
	tb := time.Now().Add(-10 * time.Minute)
	td := time.Now().Add(-5 * time.Minute)

	w.rotation.hasPair = true
	w.rotation.activePair = [2]int{1, 3} // b, d
	w.rotation.lastWatched = map[int]time.Time{1: tb, 3: td}
	w.rotation.deferredFor = map[int]bool{1: true}
	w.streakDiag = map[int]streakDiagState{3: {pursuing: true}}

	a, b, d := w.streamers[0], w.streamers[1], w.streamers[3]
	// c removed, order shuffled: new indexes 0=d 1=b 2=a.
	w.UpdateStreamers([]*models.Streamer{d, b, a})
	w.applyPendingSettings()

	if !w.rotation.hasPair {
		t.Fatal("pair should survive: both members are still on the list")
	}
	if got, want := w.rotation.activePair, [2]int{1, 0}; got != want {
		t.Fatalf("activePair not remapped by username: got %v, want %v (b->1, d->0)", got, want)
	}
	if got := w.rotation.lastWatched; len(got) != 2 || !got[1].Equal(tb) || !got[0].Equal(td) {
		t.Fatalf("lastWatched not remapped: got %v", got)
	}
	if got := w.rotation.deferredFor; len(got) != 1 || !got[1] {
		t.Fatalf("deferredFor not remapped: got %v", got)
	}
	if got := w.streakDiag; len(got) != 1 || !got[0].pursuing {
		t.Fatalf("streakDiag not remapped: got %v", got)
	}
}

// TestUpdateStreamersResetsPairWhenMemberRemoved: when a rotation-pair (or
// boost-seat) member is removed at runtime, the pair and the boost latch must
// be dropped so the next selection recomputes them from scratch instead of
// pointing at a now-different (or out-of-range) index.
func TestUpdateStreamersResetsPairWhenMemberRemoved(t *testing.T) {
	w, _ := newTestWatcher(3) // 0=a 1=b 2=c
	w.rotation.hasPair = true
	w.rotation.activePair = [2]int{0, 1}
	w.rotation.boostLatched = true
	w.rotation.boostTarget = 2
	w.rotation.boostVictim = 1

	// b (pair member and boost victim) is removed.
	w.UpdateStreamers([]*models.Streamer{w.streamers[0], w.streamers[2]})
	w.applyPendingSettings()

	if w.rotation.hasPair {
		t.Fatal("pair must be reset when a member is removed")
	}
	if w.rotation.boostLatched {
		t.Fatal("boost latch must be cleared when its victim is removed")
	}
}

// TestUpdateStreamersLastWriteWins: two stagings before one tick apply only
// the newest list, and no index-keyed state may leak through the discarded
// intermediate list.
func TestUpdateStreamersLastWriteWins(t *testing.T) {
	w, _ := newTestWatcher(3) // 0=a 1=b 2=c
	a, b, c := w.streamers[0], w.streamers[1], w.streamers[2]

	t0 := time.Now().Add(-3 * time.Minute)
	t1 := time.Now().Add(-2 * time.Minute)
	t2 := time.Now().Add(-1 * time.Minute)
	w.rotation.hasPair = true
	w.rotation.activePair = [2]int{0, 1} // a, b
	w.rotation.lastWatched = map[int]time.Time{0: t0, 1: t1, 2: t2}

	w.UpdateStreamers([]*models.Streamer{a, b}) // stale staged list
	w.UpdateStreamers([]*models.Streamer{c, a}) // newest list wins
	w.applyPendingSettings()

	if len(w.streamers) != 2 || w.streamers[0] != c || w.streamers[1] != a {
		t.Fatalf("expected the LAST staged list [c a] to be applied, got %d streamers", len(w.streamers))
	}
	// b survives only in the discarded intermediate list: the pair referenced
	// it, so the pair must reset rather than remap through the stale list.
	if w.rotation.hasPair {
		t.Fatal("pair must be reset: member b is not on the final list")
	}
	for idx := range w.rotation.lastWatched {
		if idx < 0 || idx >= len(w.streamers) {
			t.Fatalf("stale index %d leaked into lastWatched (list len %d)", idx, len(w.streamers))
		}
	}
	if got := w.rotation.lastWatched; len(got) != 2 || !got[1].Equal(t0) || !got[0].Equal(t2) {
		t.Fatalf("lastWatched must remap a->1 (t0) and c->0 (t2) only: got %v", got)
	}

	// The staged list is consumed: a second tick boundary is a no-op.
	w.applyPendingSettings()
	if len(w.streamers) != 2 || w.streamers[0] != c {
		t.Fatal("second tick must not re-apply or alter the already-applied list")
	}
}
