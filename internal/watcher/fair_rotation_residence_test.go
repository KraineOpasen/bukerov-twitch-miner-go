package watcher

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// openFairRotationResidence back-dates the loop-owned residence anchor so the
// NEXT ordinary reconciliation is past fairRotationResidence.
//
// It is the deterministic stand-in for "this pair has already been resident
// long enough", used by tests whose ticks model broker evaluations spaced
// further apart than the minimum residence. It changes no pair membership, no
// deferral state and no boost state, and it can never bypass a stronger cause:
// every immediate-preemption path reaches the replacement without consulting
// the anchor at all.
func openFairRotationResidence(w *MinuteWatcher) {
	w.rotation.lastSwitch = time.Now().Add(-fairRotationResidence)
}

// newResidenceWatcher builds a rotation fixture of n online candidates with
// watch streaks off and a real WatchTimeStore, so persisted deficit is the only
// thing ranking the base pair. Tests drive reconcileLeastWatchedPair directly
// with an explicit now — the existing deterministic seam — so no assertion
// depends on wall-clock sleeps.
func newResidenceWatcher(t *testing.T, n int) (*MinuteWatcher, []*models.Streamer, []int, *WatchTimeStore) {
	t.Helper()
	store, sqlDB := openWatchTimeStore(t, filepath.Join(t.TempDir(), "watch.db"))
	t.Cleanup(func() { _ = sqlDB.Close() })
	w, streamers, online := newRotationRetirementWatcher(n, store)
	for _, streamer := range streamers {
		streamer.Settings.WatchStreak = false
	}
	return w, streamers, online, store
}

// residencePair reports the current base pair as sorted logins.
func residencePair(w *MinuteWatcher) []string {
	if !w.rotation.hasPair {
		return nil
	}
	return sortedPair([]string{
		w.streamers[w.rotation.activePair[0]].GetUsername(),
		w.streamers[w.rotation.activePair[1]].GetUsername(),
	})
}

func requireResidencePair(t *testing.T, w *MinuteWatcher, context string, want ...string) {
	t.Helper()
	got := residencePair(w)
	if len(got) != len(want) {
		t.Fatalf("%s: base pair = %v, want %v", context, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: base pair = %v, want %v", context, got, want)
		}
	}
}

func seedMinutes(t *testing.T, store *WatchTimeStore, at time.Time, minutes map[string]float64) {
	t.Helper()
	for login, m := range minutes {
		if err := store.RecordMinutes(login, m, at); err != nil {
			t.Fatalf("seed watch time for %s: %v", login, err)
		}
	}
}

// TestFairRotationResidenceAnchorInitializesWhenRotationBegins covers the 2->3
// transition: a complete direct pair that grows past the slot cap enters
// rotation, and that first complete base pair establishes the residence anchor.
func TestFairRotationResidenceAnchorInitializesWhenRotationBegins(t *testing.T) {
	w, _, online, _ := newResidenceWatcher(t, 3)

	// Two candidates: direct selection, no rotation pair and so no residence.
	runSelectionTick(w, online[:2])
	if w.rotation.hasPair {
		t.Fatalf("direct selection must not hold a rotation pair: %v", residencePair(w))
	}
	if mode := w.GetDebugState().Mode; mode != ModeDirect {
		t.Fatalf("mode with 2 candidates = %q, want %q", mode, ModeDirect)
	}

	before := time.Now()
	runSelectionTick(w, online) // 2 -> 3 candidates: rotation begins
	after := time.Now()

	if !w.rotation.hasPair {
		t.Fatal("growing past the slot cap must establish a complete base pair")
	}
	if w.rotation.lastSwitch.Before(before) || w.rotation.lastSwitch.After(after) {
		t.Fatalf("residence anchor = %v, want stamped within [%v, %v]", w.rotation.lastSwitch, before, after)
	}
	if st := w.GetDebugState(); !st.PairSince.Equal(w.rotation.lastSwitch) {
		t.Fatalf("diagnostics PairSince = %v, diverged from the residence anchor %v", st.PairSince, w.rotation.lastSwitch)
	}
}

// TestFairRotationResidenceBlocksOrdinaryDeficitReplacement is the core
// falsifier: an ordinary persisted-deficit challenger alone cannot evict a
// complete, still-valid pair before the minimum residence, and reconciliation
// is open again at exactly the minimum residence.
func TestFairRotationResidenceBlocksOrdinaryDeficitReplacement(t *testing.T) {
	w, _, online, store := newResidenceWatcher(t, 4)
	t0 := time.Now()

	w.reconcileLeastWatchedPair(online, t0)
	requireResidencePair(t, w, "initial", "streamera", "streamerb")
	anchor := w.rotation.lastSwitch
	if !anchor.Equal(t0) {
		t.Fatalf("initial residence anchor = %v, want %v", anchor, t0)
	}

	// Persisted deficit now clearly favors the two off-pair channels.
	seedMinutes(t, store, t0, map[string]float64{"streamera": 100, "streamerb": 100})

	justBefore := t0.Add(fairRotationResidence - time.Second)
	w.selectionReasons = make(map[int]string)
	w.reconcileLeastWatchedPair(online, justBefore)
	requireResidencePair(t, w, "at 14:59", "streamera", "streamerb")
	// The hold is visible in diagnostics, and it is the residence that held it.
	for _, idx := range w.rotation.activePair {
		if reason := w.selectionReasons[idx]; !strings.Contains(reason, "residence") {
			t.Fatalf("resident seat %d reports %q, want the minimum-residence reason", idx, reason)
		}
	}
	if !w.rotation.lastSwitch.Equal(anchor) {
		t.Fatalf("a blocked evaluation moved the residence anchor: %v -> %v", anchor, w.rotation.lastSwitch)
	}
	// Blocking must not consume the one-shot streak deferral approach either.
	if !w.rotation.deferUntil.IsZero() || w.rotation.deferUsed {
		t.Fatalf("residence consumed streak-deferral state: until=%v used=%v", w.rotation.deferUntil, w.rotation.deferUsed)
	}

	open := t0.Add(fairRotationResidence)
	w.reconcileLeastWatchedPair(online, open)
	requireResidencePair(t, w, "at exactly 15:00", "streamerc", "streamerd")
	if !w.rotation.lastSwitch.Equal(open) {
		t.Fatalf("a real pair change did not stamp a fresh anchor: got %v, want %v", w.rotation.lastSwitch, open)
	}

	// A real membership change starts a fresh residence, measured from the change.
	seedMinutes(t, store, t0, map[string]float64{"streamerc": 500, "streamerd": 500})
	w.reconcileLeastWatchedPair(online, open.Add(fairRotationResidence-time.Second))
	requireResidencePair(t, w, "14:59 into the new residence", "streamerc", "streamerd")
	w.reconcileLeastWatchedPair(online, open.Add(fairRotationResidence))
	requireResidencePair(t, w, "15:00 into the new residence", "streamera", "streamerb")
}

// TestFairRotationResidenceExpiryDoesNotForceASwitch proves the rule is a
// minimum residence, not a switching cadence: reaching (and long exceeding) it
// switches nothing while the incumbents are still the two most owed, and
// re-evaluating the same pair never re-stamps the anchor.
func TestFairRotationResidenceExpiryDoesNotForceASwitch(t *testing.T) {
	w, _, online, store := newResidenceWatcher(t, 4)
	t0 := time.Now()
	seedMinutes(t, store, t0, map[string]float64{"streamerc": 500, "streamerd": 500})

	w.reconcileLeastWatchedPair(online, t0)
	requireResidencePair(t, w, "initial", "streamera", "streamerb")
	anchor := w.rotation.lastSwitch

	for _, elapsed := range []time.Duration{
		fairRotationResidence,
		fairRotationResidence + time.Second,
		2 * fairRotationResidence,
		10 * fairRotationResidence,
	} {
		w.reconcileLeastWatchedPair(online, t0.Add(elapsed))
		requireResidencePair(t, w, "elapsed "+elapsed.String(), "streamera", "streamerb")
		if !w.rotation.lastSwitch.Equal(anchor) {
			t.Fatalf("elapsed %v: re-evaluating the same pair re-stamped the residence anchor: %v -> %v",
				elapsed, anchor, w.rotation.lastSwitch)
		}
	}
}

// TestFairRotationResidenceIgnoresOrderingAndPermutation proves the anchor is
// stamped on unordered membership only: the same pair in the opposite seat
// order, under any candidate permutation, neither switches nor refreshes it.
func TestFairRotationResidenceIgnoresOrderingAndPermutation(t *testing.T) {
	w, _, online, store := newResidenceWatcher(t, 4)
	t0 := time.Now()
	seedMinutes(t, store, t0, map[string]float64{"streamerc": 50, "streamerd": 50})

	w.reconcileLeastWatchedPair(online, t0)
	requireResidencePair(t, w, "initial", "streamera", "streamerb")
	anchor := w.rotation.lastSwitch
	seat0 := w.streamers[w.rotation.activePair[0]].GetUsername()

	// Flip which incumbent ranks first: the SAME pair, computed in the opposite
	// seat order, under every candidate permutation.
	seedMinutes(t, store, t0, map[string]float64{"streamera": 1})
	permutations := [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {2, 0, 3, 1}, {1, 3, 0, 2}}
	for i, candidates := range permutations {
		w.reconcileLeastWatchedPair(candidates, t0.Add(time.Duration(i+1)*time.Minute))
		requireResidencePair(t, w, "permutation", "streamera", "streamerb")
		if !w.rotation.lastSwitch.Equal(anchor) {
			t.Fatalf("permutation %d refreshed the residence anchor: %v -> %v", i, anchor, w.rotation.lastSwitch)
		}
	}
	// The fixture really did flip the computed ranking, so the incoming pair was
	// the same set in the opposite order...
	weights := w.watchWeights(online, t0.Add(time.Minute))
	if !(weights[1] < weights[0]) {
		t.Fatalf("fixture did not flip the ranking of the two incumbents: %v", weights)
	}
	// ...and the stored seats were nonetheless left exactly as they were.
	if got := w.streamers[w.rotation.activePair[0]].GetUsername(); got != seat0 {
		t.Fatalf("a same-membership re-evaluation reshuffled the stored seats: %q -> %q", seat0, got)
	}

	// The anchor really is still the ORIGINAL one, measured from t0.
	seedMinutes(t, store, t0, map[string]float64{"streamera": 200, "streamerb": 200})
	w.reconcileLeastWatchedPair(online, t0.Add(fairRotationResidence-time.Second))
	requireResidencePair(t, w, "14:59 measured from the original anchor", "streamera", "streamerb")
	w.reconcileLeastWatchedPair(online, t0.Add(fairRotationResidence))
	requireResidencePair(t, w, "15:00 measured from the original anchor", "streamerc", "streamerd")
}

// TestFairRotationResidenceAnchorIsInertWithoutACompletePair covers 2->0: when
// the candidate set falls back to the slot cap the pair is dropped, and the
// stale anchor must not protect anything when rotation resumes.
func TestFairRotationResidenceAnchorIsInertWithoutACompletePair(t *testing.T) {
	w, _, online, store := newResidenceWatcher(t, 4)

	runSelectionTick(w, online)
	if got := sortedPair(w.GetDebugState().ActivePair); got[0] != "streamera" || got[1] != "streamerb" {
		t.Fatalf("initial fair pair = %v, want [streamera streamerb]", got)
	}
	stale := w.rotation.lastSwitch

	runSelectionTick(w, online[:2]) // 2 candidates: rotation ends, pair dropped
	if w.rotation.hasPair {
		t.Fatalf("falling back to the slot cap must drop the rotation pair: %v", residencePair(w))
	}

	// Rotation resumes far inside the OLD residence window (wall clock has barely
	// moved), with persisted deficit now favoring the other two channels.
	seedMinutes(t, store, time.Now(), map[string]float64{"streamera": 100, "streamerb": 100})
	if elapsed := time.Since(stale); elapsed >= fairRotationResidence {
		t.Fatalf("fixture is not inside the old residence window (elapsed %v)", elapsed)
	}
	runSelectionTick(w, online)
	requireResidencePair(t, w, "after rotation resumed", "streamerc", "streamerd")
	if !w.rotation.lastSwitch.After(stale) {
		t.Fatalf("refill did not stamp a fresh residence anchor: stale=%v got=%v", stale, w.rotation.lastSwitch)
	}
}

// TestFairRotationResidenceDoesNotProtectAnIncompletePair covers 2->1 and
// 2->1->2: an incomplete pair gets no protection, the empty seat refills at
// once, and the refilled pair starts a fresh residence.
func TestFairRotationResidenceDoesNotProtectAnIncompletePair(t *testing.T) {
	w, _, online, store := newResidenceWatcher(t, 4)
	t0 := time.Now()
	// Keep a and b the most owed, so nothing ORDINARY would ever move the pair.
	seedMinutes(t, store, t0, map[string]float64{"streamerc": 500, "streamerd": 500})

	w.reconcileLeastWatchedPair(online, t0)
	requireResidencePair(t, w, "initial", "streamera", "streamerb")

	// One incumbent stops being a candidate a minute in: that seat is empty and
	// must refill immediately, deep inside the residence window.
	refill := t0.Add(time.Minute)
	w.reconcileLeastWatchedPair([]int{0, 2, 3}, refill)
	requireResidencePair(t, w, "empty-seat refill", "streamera", "streamerc")
	if !w.rotation.lastSwitch.Equal(refill) {
		t.Fatalf("refill did not stamp a fresh residence anchor: got %v, want %v", w.rotation.lastSwitch, refill)
	}

	// 2->1->2: the refilled pair is protected from its OWN anchor, not the
	// original one (which is already older than the minimum residence here).
	seedMinutes(t, store, t0, map[string]float64{"streamera": 1000})
	w.reconcileLeastWatchedPair(online, refill.Add(fairRotationResidence-time.Second))
	requireResidencePair(t, w, "14:59 into the refilled pair's residence", "streamera", "streamerc")
	w.reconcileLeastWatchedPair(online, refill.Add(fairRotationResidence))
	requireResidencePair(t, w, "15:00 into the refilled pair's residence", "streamerb", "streamerc")
}

// TestFairRotationResidenceNeverDelaysAnInvalidIncumbent proves every
// "incumbent is no longer a valid candidate" class preempts immediately, well
// inside the residence window: offline, avoided, watching disabled, and removed
// from the runtime roster. It also covers the runtime roster/settings
// invalidation case — no obsolete anchor may protect the wrong pair.
func TestFairRotationResidenceNeverDelaysAnInvalidIncumbent(t *testing.T) {
	tests := []struct {
		name       string
		invalidate func(t *testing.T, w *MinuteWatcher)
	}{
		{
			name: "incumbent goes offline",
			invalidate: func(t *testing.T, w *MinuteWatcher) {
				w.streamers[0].SetConfirmedOffline()
			},
		},
		{
			name: "incumbent settings switch to avoid",
			invalidate: func(t *testing.T, w *MinuteWatcher) {
				settings := w.streamers[0].GetSettings()
				settings.Preference = models.PreferenceAvoid
				w.streamers[0].SetSettings(settings)
			},
		},
		{
			name: "incumbent settings disable watching",
			invalidate: func(t *testing.T, w *MinuteWatcher) {
				settings := w.streamers[0].GetSettings()
				settings.DisableWatch = true
				w.streamers[0].SetSettings(settings)
			},
		},
		{
			name: "incumbent removed from the runtime roster",
			invalidate: func(t *testing.T, w *MinuteWatcher) {
				w.applyStreamerList(append([]*models.Streamer(nil), w.streamers[1:]...))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, streamers, _, store := newResidenceWatcher(t, 4)
			for _, streamer := range streamers {
				streamer.OnlineAt = time.Now().Add(-time.Minute)
			}
			// a and b are the most owed, so ordinary fairness alone would keep
			// them for the whole residence window.
			seedMinutes(t, store, time.Now(), map[string]float64{"streamerc": 500, "streamerd": 500})

			runSelectionTick(w, w.getOnlineStreamers(nil))
			requireResidencePair(t, w, "precondition", "streamera", "streamerb")
			anchor := w.rotation.lastSwitch

			tt.invalidate(t, w)
			if elapsed := time.Since(anchor); elapsed >= fairRotationResidence {
				t.Fatalf("precondition: the pair is no longer resident (elapsed %v)", elapsed)
			}

			runSelectionTick(w, w.getOnlineStreamers(nil))
			got := residencePair(w)
			if pairContains(got, "streamera") {
				t.Fatalf("residence delayed an invalid incumbent's removal: pair still %v", got)
			}
			if len(got) != constants.MaxSimultaneousStreams {
				t.Fatalf("slot cap changed: pair = %v", got)
			}
		})
	}
}

// TestFairRotationResidenceNeverDelaysABoost proves DROPS/STREAK and stronger
// priority keep immediate preemption: the boost applies on the very next tick
// inside the residence window, and it does so through applyPriorityBoost —
// which still runs AFTER the base-pair residence decision, leaving the resident
// base pair itself untouched.
func TestFairRotationResidenceNeverDelaysABoost(t *testing.T) {
	tests := []struct {
		name  string
		boost func(t *testing.T, streamer *models.Streamer)
	}{
		{
			name: "channel-restricted drop campaign",
			boost: func(t *testing.T, streamer *models.Streamer) {
				streamer.Settings.ClaimDrops = true
				streamer.Stream.SetCampaignIDs([]string{"restricted"})
				streamer.Stream.SetCampaigns([]*models.Campaign{
					watchSlotTestCampaign("restricted", streamer.ChannelID, true),
				})
			},
		},
		{
			name: "active drop campaign",
			boost: func(t *testing.T, streamer *models.Streamer) {
				streamer.Settings.ClaimDrops = true
				streamer.Stream.SetCampaignIDs([]string{"active"})
				streamer.Stream.SetCampaigns([]*models.Campaign{
					watchSlotTestCampaign("active", streamer.ChannelID, false),
				})
			},
		},
		{
			name: "pursuit-eligible watch streak",
			boost: func(t *testing.T, streamer *models.Streamer) {
				streamer.Settings.WatchStreak = true
				streamer.Stream.MinuteWatched = 5
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, streamers, online, store := newResidenceWatcher(t, 4)
			seedMinutes(t, store, time.Now(), map[string]float64{"streamerc": 500, "streamerd": 500})

			runSelectionTick(w, online)
			requireResidencePair(t, w, "precondition", "streamera", "streamerb")
			anchor := w.rotation.lastSwitch

			tt.boost(t, streamers[2])
			if elapsed := time.Since(anchor); elapsed >= fairRotationResidence {
				t.Fatalf("precondition: the pair is no longer resident (elapsed %v)", elapsed)
			}

			runSelectionTick(w, online)
			state := w.GetDebugState()
			if !watchedLogins(state)["streamerc"] {
				t.Fatalf("residence delayed a DROPS/STREAK boost: %+v", state.Decisions)
			}
			if len(watchedLogins(state)) != constants.MaxSimultaneousStreams {
				t.Fatalf("boost changed the slot cap: %v", watchedLogins(state))
			}
			// The boost took a seat for the tick; the resident BASE pair and its
			// anchor are untouched, which is what places applyPriorityBoost after
			// the base-pair residence decision.
			requireResidencePair(t, w, "base pair after the boost", "streamera", "streamerb")
			if !w.rotation.lastSwitch.Equal(anchor) {
				t.Fatalf("a boost moved the base-pair residence anchor: %v -> %v", anchor, w.rotation.lastSwitch)
			}
		})
	}
}

// TestFairRotationResidenceIsEvaluatedBeforeTheStreakDeferral proves the
// ordering the owner specified: while the pair is still resident there is no
// replacement to defer, so the one-shot bounded streak deferral is neither
// armed nor consumed; once residence opens, it becomes available exactly as
// before.
func TestFairRotationResidenceIsEvaluatedBeforeTheStreakDeferral(t *testing.T) {
	w, streamers, online, store := newResidenceWatcher(t, 4)
	t0 := time.Now()

	w.reconcileLeastWatchedPair(online, t0)
	requireResidencePair(t, w, "initial", "streamera", "streamerb")

	// The incumbent is pursuing a watch streak, and persisted deficit now wants
	// it out — the exact situation the bounded deferral exists for.
	streamers[0].Settings.WatchStreak = true
	streamers[0].Stream.MinuteWatched = 5
	if !w.nearStreakCompletion(0) {
		t.Fatal("precondition: streamera is not pursuing a watch streak")
	}
	seedMinutes(t, store, t0, map[string]float64{"streamera": 100, "streamerb": 100})

	w.reconcileLeastWatchedPair(online, t0.Add(fairRotationResidence-time.Second))
	requireResidencePair(t, w, "at 14:59", "streamera", "streamerb")
	if !w.rotation.deferUntil.IsZero() || w.rotation.deferUsed {
		t.Fatalf("the one-shot streak deferral was consulted while the pair was still resident: until=%v used=%v",
			w.rotation.deferUntil, w.rotation.deferUsed)
	}

	open := t0.Add(fairRotationResidence)
	w.reconcileLeastWatchedPair(online, open)
	requireResidencePair(t, w, "at exactly 15:00", "streamera", "streamerb")
	if !w.rotation.deferUsed {
		t.Fatal("the bounded streak deferral was not available once residence expired")
	}
	if want := open.Add(streakDeferDelay); !w.rotation.deferUntil.Equal(want) {
		t.Fatalf("deferral deadline = %v, want the bounded %v", w.rotation.deferUntil, want)
	}
	if w.rotation.deferStreamer != 0 {
		t.Fatalf("deferral protected streamer %d, want streamera (0)", w.rotation.deferStreamer)
	}
	// The deferral holds the pair without re-stamping residence, and once it
	// expires ordinary fairness reconciles.
	if !w.rotation.lastSwitch.Equal(t0) {
		t.Fatalf("the deferral re-stamped the residence anchor: %v", w.rotation.lastSwitch)
	}
	after := open.Add(streakDeferDelay)
	w.reconcileLeastWatchedPair(online, after)
	requireResidencePair(t, w, "after the bounded deferral expired", "streamerc", "streamerd")
}

// TestFairRotationResidenceKeepsBrokerArbitration proves Phase A's residence
// does not guard the final BrokerSnapshot: the cap stays exactly 2, and a
// stronger Phase-B contender (a channel-restricted discovery drop) still
// arbitrates a slot while the configured base pair is resident.
func TestFairRotationResidenceKeepsBrokerArbitration(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 64)}
	checker := &staticChecker{checked: make(chan string, 64)}
	w, _ := newLoopWatcher(4, sender, checker)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.ctx = ctx
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-sender.sent:
			case <-checker.checked:
			}
		}
	}()

	w.processWatching(tickCtx(w))
	first := w.BrokerSnapshot()
	if len(first.Slots) != constants.MaxSimultaneousStreams {
		t.Fatalf("broker allocated %d slots, want exactly %d: %v", len(first.Slots), constants.MaxSimultaneousStreams, brokerChannels(first))
	}
	if !w.rotation.hasPair {
		t.Fatal("precondition: no configured base pair to be resident")
	}
	anchor := w.rotation.lastSwitch
	resident := residencePair(w)

	// Phase B proposes a strictly stronger channel-restricted drop while the
	// Phase-A pair is still resident.
	w.AddSource(&staticSource{name: "discovery", cand: []Candidate{
		{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery},
	}})
	if elapsed := time.Since(anchor); elapsed >= fairRotationResidence {
		t.Fatalf("precondition: the pair is no longer resident (elapsed %v)", elapsed)
	}

	w.processWatching(tickCtx(w))
	second := w.BrokerSnapshot()
	if len(second.Slots) != constants.MaxSimultaneousStreams {
		t.Fatalf("broker allocated %d slots, want exactly %d: %v", len(second.Slots), constants.MaxSimultaneousStreams, brokerChannels(second))
	}
	if !brokerHasChannel(second, "disco") {
		t.Fatalf("residence blocked Phase-B arbitration after Phase A: %v", brokerChannels(second))
	}
	// Phase A's own resident pair is untouched: the residence rule guards the
	// base pair, never the final broker snapshot.
	requireResidencePair(t, w, "base pair after broker arbitration", resident...)
	if !w.rotation.lastSwitch.Equal(anchor) {
		t.Fatalf("broker arbitration moved the base-pair residence anchor: %v -> %v", anchor, w.rotation.lastSwitch)
	}
}

// TestPersistedDeficitFairnessResumesAfterResidence proves the fairness
// guarantee is intact once residence opens: over evaluations spaced one
// residence apart, every candidate is watched and the pair is always the two
// least-watched.
func TestPersistedDeficitFairnessResumesAfterResidence(t *testing.T) {
	const candidates = 5
	w, streamers, online, store := newResidenceWatcher(t, candidates)
	t0 := time.Now()

	seen := make(map[string]int)
	for round := 0; round < candidates*3; round++ {
		at := t0.Add(time.Duration(round) * fairRotationResidence)
		w.reconcileLeastWatchedPair(online, at)
		for _, idx := range w.rotation.activePair {
			login := streamers[idx].GetUsername()
			seen[login]++
			// The watched pair accumulates minutes and becomes less owed, exactly
			// as a real tick would record them.
			seedMinutes(t, store, at, map[string]float64{login: 15})
		}
	}

	for _, streamer := range streamers {
		if seen[streamer.GetUsername()] == 0 {
			t.Fatalf("persisted-deficit fairness starved %s once residence expired: %v", streamer.GetUsername(), seen)
		}
	}
	if len(w.rotation.activePair) != constants.MaxSimultaneousStreams {
		t.Fatalf("slot cap changed: %v", w.rotation.activePair)
	}
}

// TestFairRotationResidenceAddsNoBackgroundOwner proves the rule is derived on
// the broker tick the loop already runs: no goroutine, timer, store, scheduler,
// ledger or cache owner is created by evaluating residence.
func TestFairRotationResidenceAddsNoBackgroundOwner(t *testing.T) {
	// No store, no context, no loop: the reconciliation path and nothing else.
	w, _, online := newRotationRetirementWatcher(4, nil)
	for _, streamer := range w.streamers {
		streamer.Settings.WatchStreak = false
	}
	t0 := time.Now()

	w.reconcileLeastWatchedPair(online, t0)
	runtime.GC()
	before := runtime.NumGoroutine()

	for i := 1; i <= 500; i++ {
		w.reconcileLeastWatchedPair(online, t0.Add(time.Duration(i)*time.Minute))
	}

	runtime.GC()
	if after := runtime.NumGoroutine(); after > before {
		t.Fatalf("residence evaluation started a background owner: goroutines %d -> %d", before, after)
	}
}

// TestFairRotationResidenceConcurrentDiagnosticsReader runs the real production
// access pattern under -race: the loop goroutine reconciles and publishes while
// dashboard/debug readers poll the published snapshot. Residence adds no new
// cross-goroutine reader of the loop-owned rotation state.
func TestFairRotationResidenceConcurrentDiagnosticsReader(t *testing.T) {
	w, _, online, _ := newResidenceWatcher(t, 4)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = w.GetDebugState()
					_ = w.BrokerSnapshot()
				}
			}
		}()
	}

	for tick := 0; tick < 500; tick++ {
		openFairRotationResidence(w)
		runSelectionTick(w, online)
	}
	close(stop)
	wg.Wait()

	if !w.rotation.hasPair {
		t.Fatal("concurrent diagnostics reads disturbed the loop-owned rotation state")
	}
}

// TestFairRotationResidenceIsNotAConfigurableSetting pins the shape the owner
// required: one private constant, no runtime setting, no rotation-interval
// surface, and no second residence owner in rotationState.
func TestFairRotationResidenceIsNotAConfigurableSetting(t *testing.T) {
	if fairRotationResidence != 15*time.Minute {
		t.Fatalf("fairRotationResidence = %v, want 15m", fairRotationResidence)
	}
	// The retired rotation-interval configuration surface stays retired: nothing
	// about residence is configurable.
	var limits config.RateLimitSettings
	if got := limits.MinuteWatchedInterval; got != 0 {
		t.Fatalf("unexpected rate-limit default: %v", got)
	}
	w, _, online, _ := newResidenceWatcher(t, 4)
	runSelectionTick(w, online)
	if st := w.GetDebugState(); !st.PairSince.Equal(w.rotation.lastSwitch) {
		t.Fatalf("residence is owned by something other than rotationState.lastSwitch: PairSince=%v anchor=%v",
			st.PairSince, w.rotation.lastSwitch)
	}
}
