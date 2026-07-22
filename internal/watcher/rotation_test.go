package watcher

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"

	_ "modernc.org/sqlite"
)

func newTestWatcher(n int) (*MinuteWatcher, []int) {
	streamers := make([]*models.Streamer, n)
	online := make([]int, n)
	for i := range streamers {
		streamers[i] = models.NewStreamer("streamer"+string(rune('a'+i)), models.DefaultStreamerSettings())
		// Represent a normal, points-enabled channel: after startup the miner
		// confirms Channel Points capability for each streamer (LoadChannelPoints
		// Context), so a configured streamer is Enabled by the time watch
		// selection runs. Tests that need Unknown/Disabled override this per-case.
		streamers[i].SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
		online[i] = i
	}
	w := &MinuteWatcher{
		streamers:  streamers,
		priorities: []config.Priority{config.PriorityOrder},
		settings: config.RateLimitSettings{
			RotationIntervalMinMinutes: 1,
			RotationIntervalMaxMinutes: 1,
		},
	}
	return w, online
}

// forceRotate pushes lastSwitch far into the past so the next selectRotating
// call is guaranteed to recompute the base pair, regardless of the
// (randomized) dwell time picked last time.
func forceRotate(w *MinuteWatcher) {
	w.rotation.lastSwitch = time.Now().Add(-24 * time.Hour)
}

// openWatchTimeStore opens an independent SQLite file directly (bypassing
// database.Open's process-wide singleton, which would otherwise make every
// test in this package share one database) and wraps it in a WatchTimeStore.
// The caller owns the returned *sql.DB and must close it.
func openWatchTimeStore(t *testing.T, path string) (*WatchTimeStore, *sql.DB) {
	t.Helper()

	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}

	store, err := NewWatchTimeStore(&database.DB{DB: sqlDB})
	if err != nil {
		_ = sqlDB.Close()
		t.Fatalf("failed to create watch time store: %v", err)
	}

	return store, sqlDB
}

func TestSelectRotatingNoRotationBelowLimit(t *testing.T) {
	w, online := newTestWatcher(2)
	got := w.selectStreamersToWatch(online)
	if len(got) != 2 {
		t.Fatalf("expected both online streamers watched with count <= max, got %d", len(got))
	}
}

// TestSelectRotatingCoversEveryoneOverManyTicks checks the fairness backbone
// with no watch-time store configured (weights all equal at 0): the
// in-memory recency tie-break alone should still cycle through every online
// streamer, the same guarantee the old round-robin schedule provided.
func TestSelectRotatingCoversEveryoneOverManyTicks(t *testing.T) {
	for _, n := range []int{4, 5, 7, 8} {
		w, online := newTestWatcher(n)

		watchedCount := make(map[int]int)
		for tick := 0; tick < n*2; tick++ {
			forceRotate(w)
			pair := w.selectRotating(online)
			if len(pair) != 2 {
				t.Fatalf("n=%d tick=%d: expected 2 streamers selected, got %d", n, tick, len(pair))
			}
			for _, idx := range pair {
				watchedCount[idx]++
			}
		}

		for _, idx := range online {
			if watchedCount[idx] == 0 {
				t.Errorf("n=%d: streamer %d was never watched after %d ticks", n, idx, n*2)
			}
		}
	}
}

// TestWeightedSelectionPrefersLowerAccumulatedTime covers requirement (a):
// a channel with less accumulated watch time in the trailing window should
// be preferred over channels with more, all else being equal.
func TestWeightedSelectionPrefersLowerAccumulatedTime(t *testing.T) {
	w, online := newTestWatcher(4)
	// Isolate the accumulated-time weighting from the separate DROPS/STREAK
	// boost mechanism (covered by its own tests below): with WatchStreak
	// disabled and no active drops, every streamer is boost-ineligible, so
	// "all else equal" holds and only the weighting decides the pair.
	for _, s := range w.streamers {
		s.Settings.WatchStreak = false
	}

	store, sqlDB := openWatchTimeStore(t, filepath.Join(t.TempDir(), "watch.db"))
	t.Cleanup(func() { _ = sqlDB.Close() })
	w.store = store

	now := time.Now()
	if err := store.RecordMinutes(w.streamers[0].Username, 5, now); err != nil {
		t.Fatalf("failed to seed watch time: %v", err)
	}
	for _, idx := range online[1:] {
		if err := store.RecordMinutes(w.streamers[idx].Username, 50, now); err != nil {
			t.Fatalf("failed to seed watch time: %v", err)
		}
	}

	watchedCount := make(map[int]int)
	const ticks = 10
	for i := 0; i < ticks; i++ {
		forceRotate(w)
		pair := w.selectRotating(online)
		for _, idx := range pair {
			watchedCount[idx]++
		}
	}

	if watchedCount[0] != ticks {
		t.Errorf("streamer with the least accumulated watch time should be picked every rotation, got %d/%d", watchedCount[0], ticks)
	}
	for _, idx := range online[1:] {
		if watchedCount[idx] >= watchedCount[0] {
			t.Errorf("streamer %d (50 accumulated minutes) watched %d times, expected fewer than streamer 0's %d (5 accumulated minutes)", idx, watchedCount[idx], watchedCount[0])
		}
	}
}

// TestWatchTimeStorePersistsAcrossRestart covers requirement (b): recorded
// watch time must survive the process re-initializing the store from the
// same on-disk database, as happens when the container restarts and the
// miner re-opens the /database volume.
func TestWatchTimeStorePersistsAcrossRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "watch.db")
	now := time.Now()

	// "First run": record watch time, then shut down.
	firstStore, firstDB := openWatchTimeStore(t, dbPath)
	if err := firstStore.RecordMinutes("alice", 12.5, now); err != nil {
		t.Fatalf("failed to record minutes: %v", err)
	}
	if err := firstStore.RecordMinutes("alice", 7.5, now.Add(time.Minute)); err != nil {
		t.Fatalf("failed to record minutes: %v", err)
	}
	if err := firstDB.Close(); err != nil {
		t.Fatalf("failed to close db: %v", err)
	}

	// "Restart": open a brand new connection to the same file, exactly as
	// the miner does when it re-initializes against the existing volume.
	secondStore, secondDB := openWatchTimeStore(t, dbPath)
	t.Cleanup(func() { _ = secondDB.Close() })

	minutes, err := secondStore.WindowMinutes([]string{"alice"}, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("failed to read watch time after restart: %v", err)
	}

	const want = 20.0
	if got := minutes["alice"]; got != want {
		t.Errorf("watch time did not survive restart: got %v minutes, want %v", got, want)
	}
}

// TestAvoidExcludedFromSelectionWhenOthersOnline covers the "avoid" contract:
// a streamer marked PreferenceAvoid must never be picked while any other
// online streamer is available.
func TestAvoidExcludedFromSelectionWhenOthersOnline(t *testing.T) {
	w, online := newTestWatcher(4)
	w.streamers[1].Settings.Preference = models.PreferenceAvoid

	for tick := 0; tick < 10; tick++ {
		forceRotate(w)
		got := w.selectStreamersToWatch(online)
		for _, idx := range got {
			if idx == 1 {
				t.Fatalf("tick %d: avoided streamer 1 was selected while other streamers were online: %v", tick, got)
			}
		}
	}
}

// TestAvoidStillWatchedWhenOnlyOnlineChannel covers the exception to "avoid":
// if it's the only online channel at all, it must still be watched.
func TestAvoidStillWatchedWhenOnlyOnlineChannel(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.streamers[0].Settings.Preference = models.PreferenceAvoid

	got := w.selectStreamersToWatch([]int{0})
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("expected the sole online (avoided) streamer to still be watched, got %v", got)
	}
}

// TestPreferBiasesRotationTowardPreferredStreamer covers the "prefer"
// contract: all else being roughly equal, a preferred streamer should be
// picked more often than an otherwise-equivalent non-preferred one - without
// ever excluding the others outright (unlike avoid).
func TestPreferBiasesRotationTowardPreferredStreamer(t *testing.T) {
	w, online := newTestWatcher(4)
	for _, s := range w.streamers {
		s.Settings.WatchStreak = false
	}
	w.streamers[0].Settings.Preference = models.PreferencePrefer

	watchedCount := make(map[int]int)
	const ticks = 20
	for i := 0; i < ticks; i++ {
		forceRotate(w)
		pair := w.selectRotating(online)
		for _, idx := range pair {
			watchedCount[idx]++
		}
	}

	for _, idx := range online[1:] {
		if watchedCount[idx] > watchedCount[0] {
			t.Errorf("non-preferred streamer %d watched %d times, more than preferred streamer 0's %d", idx, watchedCount[idx], watchedCount[0])
		}
	}
}

func TestApplyPriorityBoostSwapsInDropsStreamer(t *testing.T) {
	w, online := newTestWatcher(3)
	// streamer 2 has an active drop campaign but isn't in the base pair.
	w.streamers[2].Stream.CampaignIDs = []string{"campaign-1"}

	pair := [2]int{0, 1}
	w.rotation.lastWatched = map[int]time.Time{
		0: time.Now(),
		1: time.Now().Add(-time.Minute),
	}

	boosted := w.applyPriorityBoost(pair, online)
	if boosted[0] != 2 && boosted[1] != 2 {
		t.Fatalf("expected drops-eligible streamer 2 to be swapped into the pair, got %v", boosted)
	}
}

// TestApplyPriorityBoostPrefersChannelRestrictedDrop covers the case where
// two off-pair streamers are both boost-eligible via DropsCondition, but
// only one holds a channel-restricted campaign (progress only countable on
// that exact channel). That one must win the boost seat even if it was
// watched more recently than the unrestricted-campaign candidate.
func TestApplyPriorityBoostPrefersChannelRestrictedDrop(t *testing.T) {
	w, online := newTestWatcher(4)
	w.streamers[2].Stream.CampaignIDs = []string{"campaign-unrestricted"}
	w.streamers[3].Stream.CampaignIDs = []string{"campaign-restricted"}
	w.streamers[3].Stream.Campaigns = []*models.Campaign{
		{ID: "campaign-restricted", Channels: []string{w.streamers[3].ChannelID}},
	}

	pair := [2]int{0, 1}
	w.rotation.lastWatched = map[int]time.Time{
		0: time.Now(),
		1: time.Now().Add(-time.Minute),
		2: time.Now().Add(-time.Hour), // watched longer ago than 3
		3: time.Now().Add(-time.Minute / 2),
	}

	boosted := w.applyPriorityBoost(pair, online)
	if boosted[0] != 3 && boosted[1] != 3 {
		t.Fatalf("expected channel-restricted-campaign streamer 3 to win the boost seat over streamer 2, got %v", boosted)
	}
}

func TestNearStreakCompletionProtectsFromSwap(t *testing.T) {
	w, online := newTestWatcher(3)
	w.streamers[2].Stream.CampaignIDs = []string{"campaign-1"}

	pair := [2]int{0, 1}

	// Both current pair members are seconds away from completing their
	// watch streak; neither should be sacrificed for the boost.
	w.streamers[0].Stream.MinuteWatched = 6.5
	w.streamers[1].Stream.MinuteWatched = 6.8

	boosted := w.applyPriorityBoost(pair, online)
	if boosted != pair {
		t.Fatalf("expected pair unchanged when both members are near streak completion, got %v want %v", boosted, pair)
	}
}

// TestRotateDropsOfflineStreamerEvenIfNearStreakCompletion guards against a
// regression where a pair member that went offline could still linger in
// activePair because the near-streak-completion deferral didn't check
// whether the member was still online.
func TestRotateDropsOfflineStreamerEvenIfNearStreakCompletion(t *testing.T) {
	w, _ := newTestWatcher(4)
	w.rotation.hasPair = true
	w.rotation.activePair = [2]int{0, 1}
	w.rotation.deferredFor = make(map[int]bool)
	// Make streamer 1 look recently watched so it ranks worse than 2 and 3
	// (whose lastWatched is still zero) and gets excluded from the newly
	// computed pair - i.e. it becomes a "leaving" candidate alongside the
	// now-offline streamer 0.
	w.rotation.lastWatched = map[int]time.Time{1: time.Now()}

	// Streamer 0 has gone offline (it's no longer in the online set below).
	// Streamer 1 is still online and seconds away from completing its watch
	// streak - on its own this would justify deferring the swap-out, but it
	// must not do so here because its outgoing partner (0) is already gone.
	w.streamers[1].Stream.MinuteWatched = 6.5

	stillOnline := []int{1, 2, 3}
	w.rotateToLeastWatchedPair(stillOnline, time.Now())

	if w.rotation.activePair[0] == 0 || w.rotation.activePair[1] == 0 {
		t.Fatalf("offline streamer 0 should never remain in activePair, got %v", w.rotation.activePair)
	}
}
