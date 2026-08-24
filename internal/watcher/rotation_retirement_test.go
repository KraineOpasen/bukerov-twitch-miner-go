package watcher

import (
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func newRotationRetirementWatcher(n int, store *WatchTimeStore) (*MinuteWatcher, []*models.Streamer, []int) {
	streamers := make([]*models.Streamer, n)
	online := make([]int, n)
	for i := range streamers {
		streamers[i] = models.NewStreamer("streamer"+string(rune('a'+i)), models.DefaultStreamerSettings())
		streamers[i].SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
		streamers[i].SetConfirmedOnline()
		online[i] = i
	}
	return NewMinuteWatcher(
		nil,
		streamers,
		[]config.Priority{config.PriorityOrder},
		config.DefaultConfig().RateLimits,
		store,
	), streamers, online
}

func watchedLogins(st DebugState) map[string]bool {
	result := make(map[string]bool, len(st.Decisions))
	for _, decision := range st.Decisions {
		if decision.Watching {
			result[decision.Username] = true
		}
	}
	return result
}

func sortedPair(pair []string) []string {
	result := append([]string(nil), pair...)
	sort.Strings(result)
	return result
}

func pairContains(pair []string, login string) bool {
	for _, member := range pair {
		if member == login {
			return true
		}
	}
	return false
}

func TestOrdinaryBrokerEvaluationReconcilesPersistedFairness(t *testing.T) {
	store, sqlDB := openWatchTimeStore(t, filepath.Join(t.TempDir(), "watch.db"))
	t.Cleanup(func() { _ = sqlDB.Close() })
	w, streamers, online := newRotationRetirementWatcher(4, store)
	for _, streamer := range streamers {
		streamer.Settings.WatchStreak = false
	}

	runSelectionTick(w, online)
	first := watchedLogins(w.GetDebugState())
	if len(first) != 2 {
		t.Fatalf("first broker evaluation watched %d channels, want 2: %v", len(first), first)
	}
	for login := range first {
		if err := store.RecordMinutes(login, 100, time.Now()); err != nil {
			t.Fatalf("seed incumbent watch time for %s: %v", login, err)
		}
	}

	runSelectionTick(w, online)
	secondState := w.GetDebugState()
	second := watchedLogins(secondState)
	for login := range first {
		if second[login] {
			t.Fatalf("100-minute incumbent %s remained ahead of zero-minute contenders: first=%v second=%v", login, first, second)
		}
	}
	for _, decision := range secondState.Decisions {
		if decision.Watching && !strings.Contains(decision.Reason, "persisted") {
			t.Errorf("watched decision does not describe persisted fairness: %+v", decision)
		}
	}
}

func TestFairPairIsDeterministicAcrossCandidatePermutations(t *testing.T) {
	permutations := [][]int{
		{0, 1, 2, 3, 4},
		{4, 3, 2, 1, 0},
		{2, 4, 1, 3, 0},
		{1, 3, 0, 4, 2},
	}
	for run, candidates := range permutations {
		w, streamers, _ := newRotationRetirementWatcher(5, nil)
		for _, streamer := range streamers {
			streamer.Settings.WatchStreak = false
		}
		runSelectionTick(w, candidates)
		got := sortedPair(w.GetDebugState().ActivePair)
		if len(got) != 2 || got[0] != "streamera" || got[1] != "streamerb" {
			t.Fatalf("permutation %d selected %v, want login-deterministic [streamera streamerb]", run, got)
		}
	}
}

func TestDebugStateHasNoFutureRotationProjection(t *testing.T) {
	w, _, online := newRotationRetirementWatcher(4, nil)
	runSelectionTick(w, online)

	body, err := json.Marshal(w.GetDebugState())
	if err != nil {
		t.Fatalf("marshal debug state: %v", err)
	}
	if strings.Contains(string(body), "NextRotationAt") {
		t.Fatalf("debug state exposes obsolete future-rotation projection: %s", body)
	}
	for _, decision := range w.GetDebugState().Decisions {
		if strings.Contains(strings.ToLower(decision.Reason), "around") {
			t.Fatalf("decision projects an obsolete future rotation: %q", decision.Reason)
		}
	}
}

func TestBoostContinuitySurvivesBasePairReconciliation(t *testing.T) {
	w, streamers, online := newRotationRetirementWatcher(5, nil)
	for _, streamer := range streamers {
		streamer.Settings.WatchStreak = true
		streamer.Stream.WatchStreakMissing = true
	}

	basePairs := make(map[string]bool)
	for tick := 0; tick < 6; tick++ {
		runSelectionTick(w, online)
		state := w.GetDebugState()
		basePairs[strings.Join(sortedPair(state.ActivePair), ",")] = true
		if !watchedLogins(state)["streamerc"] {
			t.Fatalf("tick %d: base reconciliation churned the continuity target out: %+v", tick, state.Decisions)
		}
	}
	if len(basePairs) < 2 {
		t.Fatalf("test did not exercise a base-pair reconciliation: %v", basePairs)
	}
}

func TestEqualActiveDropDoesNotChurnHeldBoostAcrossBaseReconciliation(t *testing.T) {
	store, sqlDB := openWatchTimeStore(t, filepath.Join(t.TempDir(), "watch.db"))
	t.Cleanup(func() { _ = sqlDB.Close() })
	w, streamers, online := newRotationRetirementWatcher(5, store)
	for _, streamer := range streamers {
		streamer.Settings.WatchStreak = false
	}
	for idx, minutes := range map[int]float64{2: 10, 3: 20, 4: 30} {
		streamers[idx].Settings.ClaimDrops = true
		if idx == 2 || idx == 3 {
			streamers[idx].Stream.SetCampaignIDs([]string{"equal-drop"})
		}
		if err := store.RecordMinutes(streamers[idx].GetUsername(), minutes, time.Now()); err != nil {
			t.Fatalf("seed initial fairness for %s: %v", streamers[idx].GetUsername(), err)
		}
	}

	runSelectionTick(w, online)
	first := w.GetDebugState()
	if !watchedLogins(first)["streamerc"] {
		t.Fatalf("precondition: active-drop continuity target was not admitted: %+v", first.Decisions)
	}
	if got := sortedPair(first.ActivePair); len(got) != 2 || got[0] != "streamera" || got[1] != "streamerb" {
		t.Fatalf("precondition: initial fair base pair = %v, want [streamera streamerb]", got)
	}

	for _, idx := range []int{0, 1, 2} {
		if err := store.RecordMinutes(streamers[idx].GetUsername(), 100, time.Now()); err != nil {
			t.Fatalf("advance fairness for %s: %v", streamers[idx].GetUsername(), err)
		}
	}
	runSelectionTick(w, online)
	second := w.GetDebugState()
	if got := sortedPair(second.ActivePair); len(got) != 2 || got[0] != "streamerd" || got[1] != "streamere" {
		t.Fatalf("test did not reconcile onto the equal-drop base contender: got %v", got)
	}
	if !watchedLogins(second)["streamerc"] {
		t.Fatalf("equal active-drop contender churned a still-eligible held target: %+v", second.Decisions)
	}
}

func TestBoostHandsOffWhenTargetNoLongerEligible(t *testing.T) {
	w, streamers, online := newRotationRetirementWatcher(4, nil)
	for _, streamer := range streamers {
		streamer.Settings.WatchStreak = true
		streamer.Stream.WatchStreakMissing = true
	}

	runSelectionTick(w, online)
	if !watchedLogins(w.GetDebugState())["streamerc"] {
		t.Fatalf("precondition: expected streamerc to hold the continuity seat: %+v", w.GetDebugState().Decisions)
	}
	streamers[2].Stream.WatchStreakMissing = false

	runSelectionTick(w, online)
	state := w.GetDebugState()
	if watchedLogins(state)["streamerc"] {
		t.Fatalf("ineligible continuity target still held a watch slot: %+v", state.Decisions)
	}
	if len(watchedLogins(state)) != 2 {
		t.Fatalf("handoff did not preserve the global two-slot allocation: %+v", state.Decisions)
	}
}

func TestStrictlyStrongerRestrictedDropPreemptsProtectedStreak(t *testing.T) {
	w, streamers, online := newRotationRetirementWatcher(3, nil)
	for _, idx := range []int{0, 1} {
		streamers[idx].Settings.WatchStreak = true
		streamers[idx].Stream.WatchStreakMissing = true
		streamers[idx].Stream.MinuteWatched = 5
	}
	streamers[2].Settings.WatchStreak = false
	streamers[2].Settings.ClaimDrops = true
	streamers[2].Stream.SetCampaignIDs([]string{"restricted"})
	streamers[2].Stream.SetCampaigns([]*models.Campaign{{
		ID:       "restricted",
		Channels: []string{"streamerc"},
	}})

	runSelectionTick(w, online)
	state := w.GetDebugState()
	if !watchedLogins(state)["streamerc"] {
		t.Fatalf("strictly stronger restricted drop did not preempt a weaker protected streak: %+v", state.Decisions)
	}
}

func TestStreakDeferralHasOneNonExtendingDeadline(t *testing.T) {
	store, sqlDB := openWatchTimeStore(t, filepath.Join(t.TempDir(), "watch.db"))
	t.Cleanup(func() { _ = sqlDB.Close() })
	w, streamers, online := newRotationRetirementWatcher(4, store)
	for _, streamer := range streamers {
		streamer.Settings.WatchStreak = false
	}
	runSelectionTick(w, online)
	initial := w.GetDebugState()

	streamers[0].Settings.WatchStreak = true
	streamers[0].Stream.WatchStreakMissing = true
	streamers[0].Stream.MinuteWatched = 5
	for _, login := range initial.ActivePair {
		if err := store.RecordMinutes(login, 100, time.Now()); err != nil {
			t.Fatalf("seed incumbent watch time for %s: %v", login, err)
		}
	}

	before := time.Now()
	runSelectionTick(w, online)
	first := w.GetDebugState()
	after := time.Now()
	if got := sortedPair(first.ActivePair); strings.Join(got, ",") != strings.Join(sortedPair(initial.ActivePair), ",") {
		t.Fatalf("bounded deferral did not preserve the approached pair: initial=%v got=%v", initial.ActivePair, first.ActivePair)
	}
	if !first.PairSince.Equal(initial.PairSince) {
		t.Fatalf("deferral falsified PairSince: initial=%v got=%v", initial.PairSince, first.PairSince)
	}
	if len(first.PostponedSwapOuts) != 1 || first.PostponedSwapOuts[0].Username != "streamera" {
		t.Fatalf("diagnostics do not expose the active explicit deferral: %+v", first.PostponedSwapOuts)
	}
	deadline := first.PostponedSwapOuts[0].Until
	if deadline.Before(before.Add(2*time.Minute)) || deadline.After(after.Add(2*time.Minute)) {
		t.Fatalf("deferral deadline is not the bounded two-minute deadline: before=%v after=%v got=%v", before, after, deadline)
	}

	runSelectionTick(w, online)
	second := w.GetDebugState()
	if len(second.PostponedSwapOuts) != 1 || !second.PostponedSwapOuts[0].Until.Equal(deadline) {
		t.Fatalf("ordinary re-evaluation extended the one-shot deadline: first=%v second=%+v", deadline, second.PostponedSwapOuts)
	}
	if !second.PairSince.Equal(initial.PairSince) {
		t.Fatalf("re-evaluation changed PairSince without a pair change: initial=%v got=%v", initial.PairSince, second.PairSince)
	}
}

func TestDeferredStreamerLeavesImmediatelyWhenOfflineOrIneligible(t *testing.T) {
	tests := []struct {
		name   string
		leave  func(*models.Streamer)
		online []int
	}{
		{
			name: "offline",
			leave: func(streamer *models.Streamer) {
				streamer.SetConfirmedOffline()
			},
			online: []int{1, 2, 3},
		},
		{
			name: "streak no longer pending",
			leave: func(streamer *models.Streamer) {
				streamer.Stream.WatchStreakMissing = false
			},
			online: []int{0, 1, 2, 3},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, sqlDB := openWatchTimeStore(t, filepath.Join(t.TempDir(), "watch.db"))
			t.Cleanup(func() { _ = sqlDB.Close() })
			w, streamers, online := newRotationRetirementWatcher(4, store)
			for _, streamer := range streamers {
				streamer.Settings.WatchStreak = false
			}
			runSelectionTick(w, online)
			initial := w.GetDebugState()

			streamers[0].Settings.WatchStreak = true
			streamers[0].Stream.WatchStreakMissing = true
			streamers[0].Stream.MinuteWatched = 5
			for _, login := range initial.ActivePair {
				if err := store.RecordMinutes(login, 100, time.Now()); err != nil {
					t.Fatalf("seed incumbent watch time for %s: %v", login, err)
				}
			}
			runSelectionTick(w, online)
			if len(w.GetDebugState().PostponedSwapOuts) != 1 {
				t.Fatalf("precondition: explicit deferral was not armed: %+v", w.GetDebugState())
			}

			tt.leave(streamers[0])
			runSelectionTick(w, tt.online)
			state := w.GetDebugState()
			if pairContains(state.ActivePair, "streamera") {
				t.Fatalf("offline/ineligible deferred streamer remained in the fair pair: %+v", state)
			}
			if len(state.PostponedSwapOuts) != 0 {
				t.Fatalf("stale deferral remained after immediate removal: %+v", state.PostponedSwapOuts)
			}
			if !state.PairSince.After(initial.PairSince) {
				t.Fatalf("actual pair change did not advance PairSince: initial=%v got=%v", initial.PairSince, state.PairSince)
			}
		})
	}
}
