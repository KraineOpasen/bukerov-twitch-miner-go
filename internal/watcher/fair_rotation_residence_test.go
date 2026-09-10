package watcher

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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
	past := time.Now().Add(-fairRotationResidence)
	w.rotation.cohortSince = past
	w.rotation.lastSwitch = past
}

// reconcileAndCommitAt runs ONE ordinary base-pair evaluation at an explicit
// instant and commits the grant it produced, through the same production entry
// points processWatching uses on its final allocation.
//
// Residence belongs to committed ordinary service, so a residence test has to
// commit: an evaluation that only proposes leaves the cohort — and therefore the
// anchor these tests measure — untouched, which is a scheduler this package does
// not have. Driving an explicit instant keeps every deadline assertion exact
// without a sleep.
func reconcileAndCommitAt(w *MinuteWatcher, online []int, now time.Time) {
	w.selectionMode = ModeRotation
	w.reconcileLeastWatchedPair(online, now)
	w.commitSelectedOrdinary(w.rotation.activePair[:], now)
}

// residenceAnchor is the committed ordinary cohort's single common anchor — the
// value the minimum residence is actually measured from.
func residenceAnchor(w *MinuteWatcher) time.Time { return w.rotation.cohortSince }

// writerEpoch is a seqlock-style generation counter for a test writer call:
// even means no call is in flight, odd means one is. Entering increments to a
// unique odd value and leaving increments to the following even value, so the
// value never repeats within a run. The parity encoding assumes ONE writer
// goroutine per epoch, which is how both callers use it; two writers sharing an
// epoch would read as "no call in flight" while both were inside one.
//
// insideOneWriterCall reports whether an observation that sampled the epoch
// before and after its COMPLETE operation ran wholly inside one exact writer
// call. It requires an odd `before` and `before == after`, which together mean
// both samples are the same odd value: because every entry and every exit
// increments, an equal odd pair cannot span an exit followed by a re-entry. A
// plain "a call is in flight" boolean cannot express this — the writer may
// leave call N and enter call N+1 between the two loads, so both read true
// while the operation actually straddled the gap between them. That ABA case is
// exactly what this predicate rejects; see
// TestInsideOneWriterCallRejectsSplitObservations.
func insideOneWriterCall(before, after int64) bool {
	return before&1 == 1 && before == after
}

// enterWriterCall / leaveWriterCall move the epoch across one writer call, and
// writerCallsIn converts a final epoch back into the number of complete calls it
// brackets. Callers assert that count so a dropped enter or leave — the one
// mutation that could make the predicate accept an observation taken outside
// any call — fails instead of silently weakening the proof.
func enterWriterCall(epoch *atomic.Int64) { epoch.Add(1) }
func leaveWriterCall(epoch *atomic.Int64) { epoch.Add(1) }

func requireWriterCallsBracketed(t *testing.T, epoch *atomic.Int64, calls int) {
	t.Helper()
	if got := epoch.Load(); got != int64(2*calls) {
		t.Fatalf("writer epoch is %d after %d calls, want %d: every call must increment exactly twice (one enter, one leave)",
			got, calls, 2*calls)
	}
}

// TestInsideOneWriterCallRejectsSplitObservations is the bounded falsifier for
// the overlap predicate the two race tests rely on. It pins that an observation
// split across two adjacent writer calls — the ABA case an in-flight boolean
// would wrongly accept — cannot satisfy it.
func TestInsideOneWriterCallRejectsSplitObservations(t *testing.T) {
	tests := []struct {
		name          string
		before, after int64
		want          bool
	}{
		{"wholly inside the first call", 1, 1, true},
		{"wholly inside a later call", 7, 7, true},
		{"split across two adjacent calls (the ABA case)", 1, 3, false},
		{"split across several calls", 1, 5, false},
		{"writer left during the operation", 1, 2, false},
		{"writer entered during the operation", 2, 3, false},
		{"no call in flight at either sample", 2, 2, false},
		{"writer never entered", 0, 0, false},
		{"epoch moved backwards (must never be accepted)", 3, 1, false},
		// Unreachable in practice (the bounded loops move the epoch a few
		// thousand times, int64 wrap needs ~9e18), but it pins that the
		// parity test is sign-safe. Parity alternation survives the wrap —
		// max int64 is odd and +1 lands on an even min int64 — so an odd
		// negative epoch still means a call is in flight, and rejecting it
		// would silently stop counting overlaps. This row is what
		// distinguishes `before&1 == 1` from `before%2 == 1`, since Go's %
		// takes the sign of the dividend.
		{"in flight after an int64 wrap", -3, -3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := insideOneWriterCall(tt.before, tt.after); got != tt.want {
				t.Fatalf("insideOneWriterCall(%d, %d) = %v, want %v", tt.before, tt.after, got, tt.want)
			}
		})
	}

	// A correctly bracketed call moves the epoch by exactly two, so the values
	// the table calls "wholly inside" are the ones the harness actually
	// produces, and the ABA pair (1, 3) is what a leave followed by an enter
	// looks like from the observer's side.
	var epoch atomic.Int64
	enterWriterCall(&epoch)
	inFirst := epoch.Load()
	leaveWriterCall(&epoch)
	enterWriterCall(&epoch)
	inSecond := epoch.Load()
	leaveWriterCall(&epoch)
	requireWriterCallsBracketed(t, &epoch, 2)
	if !insideOneWriterCall(inFirst, inFirst) || !insideOneWriterCall(inSecond, inSecond) {
		t.Fatalf("an observation inside one bracketed call must be accepted (epochs %d, %d)", inFirst, inSecond)
	}
	if insideOneWriterCall(inFirst, inSecond) {
		t.Fatalf("an observation split across two adjacent bracketed calls must be rejected (epochs %d -> %d)", inFirst, inSecond)
	}
}

// newResidenceWatcher builds a rotation fixture of n online candidates with
// watch streaks off and a real WatchTimeStore, so persisted deficit is the only
// thing ranking the base pair. Tests drive reconcileLeastWatchedPair directly
// with an explicit now — the existing deterministic seam — so no assertion
// depends on wall-clock sleeps.
func newResidenceWatcher(t *testing.T, n int) (*MinuteWatcher, []*models.Streamer, []int, *WatchTimeStore) {
	t.Helper()
	store, sqlDB := openWatchTimeStore(t, filepath.Join(t.TempDir(), "watch.db"))
	t.Cleanup(func() {
		if err := sqlDB.Close(); err != nil {
			t.Errorf("close watch-time database: %v", err)
		}
	})
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

	reconcileAndCommitAt(w, online, t0)
	requireResidencePair(t, w, "initial", "streamera", "streamerb")
	anchor := residenceAnchor(w)
	if !anchor.Equal(t0) {
		t.Fatalf("initial residence anchor = %v, want %v", anchor, t0)
	}

	// Persisted deficit now clearly favors the two off-pair channels.
	seedMinutes(t, store, t0, map[string]float64{"streamera": 100, "streamerb": 100})

	justBefore := t0.Add(fairRotationResidence - time.Second)
	w.selectionReasons = make(map[int]string)
	reconcileAndCommitAt(w, online, justBefore)
	requireResidencePair(t, w, "at 14:59", "streamera", "streamerb")
	// The hold is visible in diagnostics, and it is the residence that held it.
	for _, idx := range w.rotation.activePair {
		if reason := w.selectionReasons[idx]; !strings.Contains(reason, "residence") {
			t.Fatalf("resident seat %d reports %q, want the minimum-residence reason", idx, reason)
		}
	}
	if !residenceAnchor(w).Equal(anchor) {
		t.Fatalf("a blocked evaluation moved the residence anchor: %v -> %v", anchor, residenceAnchor(w))
	}
	// Blocking must not consume the one-shot streak deferral approach either.
	if !w.rotation.deferUntil.IsZero() || w.rotation.deferUsed {
		t.Fatalf("residence consumed streak-deferral state: until=%v used=%v", w.rotation.deferUntil, w.rotation.deferUsed)
	}

	open := t0.Add(fairRotationResidence)
	reconcileAndCommitAt(w, online, open)
	requireResidencePair(t, w, "at exactly 15:00", "streamerc", "streamerd")
	if !residenceAnchor(w).Equal(open) {
		t.Fatalf("a real cohort change did not stamp a fresh anchor: got %v, want %v", residenceAnchor(w), open)
	}

	// A real membership change starts a fresh residence, measured from the change.
	seedMinutes(t, store, t0, map[string]float64{"streamerc": 500, "streamerd": 500})
	reconcileAndCommitAt(w, online, open.Add(fairRotationResidence-time.Second))
	requireResidencePair(t, w, "14:59 into the new residence", "streamerc", "streamerd")
	reconcileAndCommitAt(w, online, open.Add(fairRotationResidence))
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

	reconcileAndCommitAt(w, online, t0)
	requireResidencePair(t, w, "initial", "streamera", "streamerb")
	anchor := residenceAnchor(w)

	for _, elapsed := range []time.Duration{
		fairRotationResidence,
		fairRotationResidence + time.Second,
		2 * fairRotationResidence,
		10 * fairRotationResidence,
	} {
		reconcileAndCommitAt(w, online, t0.Add(elapsed))
		requireResidencePair(t, w, "elapsed "+elapsed.String(), "streamera", "streamerb")
		if !residenceAnchor(w).Equal(anchor) {
			t.Fatalf("elapsed %v: re-evaluating the same cohort re-stamped the residence anchor: %v -> %v",
				elapsed, anchor, residenceAnchor(w))
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

	reconcileAndCommitAt(w, online, t0)
	requireResidencePair(t, w, "initial", "streamera", "streamerb")
	anchor := residenceAnchor(w)
	seat0 := w.streamers[w.rotation.activePair[0]].GetUsername()

	// Flip which incumbent ranks first: the SAME pair, computed in the opposite
	// seat order, under every candidate permutation.
	seedMinutes(t, store, t0, map[string]float64{"streamera": 1})
	permutations := [][]int{{0, 1, 2, 3}, {3, 2, 1, 0}, {2, 0, 3, 1}, {1, 3, 0, 2}}
	for i, candidates := range permutations {
		reconcileAndCommitAt(w, candidates, t0.Add(time.Duration(i+1)*time.Minute))
		requireResidencePair(t, w, "permutation", "streamera", "streamerb")
		if !residenceAnchor(w).Equal(anchor) {
			t.Fatalf("permutation %d refreshed the residence anchor: %v -> %v", i, anchor, residenceAnchor(w))
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
	reconcileAndCommitAt(w, online, t0.Add(fairRotationResidence-time.Second))
	requireResidencePair(t, w, "14:59 measured from the original anchor", "streamera", "streamerb")
	reconcileAndCommitAt(w, online, t0.Add(fairRotationResidence))
	requireResidencePair(t, w, "15:00 measured from the original anchor", "streamerc", "streamerd")
}

// TestCommittedOrdinaryResidenceSurvivesDroppingToTheSlotCap covers the direct
// demand 2->3 path: when the candidate set falls back TO the slot cap, the same
// two channels keep both slots, so the ordinary cohort has not changed and its
// anchor was established at demand 2 — not when a third candidate later made
// rotation begin.
//
// The stored base pair is still dropped on the way through (nothing rotates at
// or below the cap), which is exactly why the anchor cannot live on it: the pair
// is a proposal, and this cohort never stopped being served. A mutant that keys
// residence off the direct path's hasPair = false fails here.
func TestCommittedOrdinaryResidenceSurvivesDroppingToTheSlotCap(t *testing.T) {
	w, _, online, store := newResidenceWatcher(t, 4)

	runSelectionTick(w, online)
	if got := sortedPair(w.GetDebugState().ActivePair); got[0] != "streamera" || got[1] != "streamerb" {
		t.Fatalf("initial fair pair = %v, want [streamera streamerb]", got)
	}
	established := residenceAnchor(w)
	if established.IsZero() {
		t.Fatal("the initial selection did not commit an ordinary cohort")
	}

	runSelectionTick(w, online[:2]) // 2 candidates: rotation ends, the pair is dropped
	if w.rotation.hasPair {
		t.Fatalf("falling back to the slot cap must drop the rotation pair: %v", residencePair(w))
	}
	if got := residenceAnchor(w); !got.Equal(established) {
		t.Fatalf("the same two channels kept both slots, yet the anchor moved: %v -> %v", established, got)
	}

	// Rotation resumes inside that window with persisted deficit now favoring the
	// other two channels: ordinary ranking alone must not take the seats.
	seedMinutes(t, store, time.Now(), map[string]float64{"streamera": 100, "streamerb": 100})
	if elapsed := time.Since(established); elapsed >= fairRotationResidence {
		t.Fatalf("fixture is not inside the residence window (elapsed %v)", elapsed)
	}
	runSelectionTick(w, online)
	requireResidencePair(t, w, "after rotation resumed", "streamera", "streamerb")
	if got := residenceAnchor(w); !got.Equal(established) {
		t.Fatalf("rotation resuming re-stamped an anchor that was established at demand 2: %v -> %v", established, got)
	}

	// Once that residence expires the ranking is free again, and the two owed
	// channels take over: residence is a floor, not a lock.
	openFairRotationResidence(w)
	runSelectionTick(w, online)
	requireResidencePair(t, w, "after the residence expired", "streamerc", "streamerd")
}

// TestFairRotationResidenceDoesNotProtectAnIncompletePair covers 2->1 and
// 2->1->2: an incomplete pair gets no protection, the empty seat refills at
// once, and the refilled pair starts a fresh residence.
func TestFairRotationResidenceDoesNotProtectAnIncompletePair(t *testing.T) {
	w, _, online, store := newResidenceWatcher(t, 4)
	t0 := time.Now()
	// Keep a and b the most owed, so nothing ORDINARY would ever move the pair.
	seedMinutes(t, store, t0, map[string]float64{"streamerc": 500, "streamerd": 500})

	reconcileAndCommitAt(w, online, t0)
	requireResidencePair(t, w, "initial", "streamera", "streamerb")

	// One incumbent stops being a candidate a minute in: that seat is empty and
	// must refill immediately, deep inside the residence window.
	refill := t0.Add(time.Minute)
	reconcileAndCommitAt(w, []int{0, 2, 3}, refill)
	requireResidencePair(t, w, "empty-seat refill", "streamera", "streamerc")
	if !residenceAnchor(w).Equal(refill) {
		t.Fatalf("refill did not stamp a fresh residence anchor: got %v, want %v", residenceAnchor(w), refill)
	}

	// 2->1->2: the refilled pair is protected from its OWN anchor, not the
	// original one (which is already older than the minimum residence here).
	seedMinutes(t, store, t0, map[string]float64{"streamera": 1000})
	reconcileAndCommitAt(w, online, refill.Add(fairRotationResidence-time.Second))
	requireResidencePair(t, w, "14:59 into the refilled pair's residence", "streamera", "streamerc")
	// At 15:00 the ranking is open again and the two most owed take the seats.
	// c and d carry equal persisted minutes, and the tie goes to d: c has already
	// held a committed seat during this window and d has not, so the recency
	// tie-break sends the turn to the channel that was never served.
	reconcileAndCommitAt(w, online, refill.Add(fairRotationResidence))
	requireResidencePair(t, w, "15:00 into the refilled pair's residence", "streamerb", "streamerd")
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
			anchor := residenceAnchor(w)
			if anchor.IsZero() {
				t.Fatal("precondition: the selection did not commit an ordinary cohort to be resident")
			}

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
				t.Fatalf("a boost moved the base-pair anchor: %v -> %v", anchor, w.rotation.lastSwitch)
			}
			// The boost took one of the two ordinary seats, so residual ordinary
			// capacity really did fall to one — and residence, which belongs to the
			// committed cohort rather than to the pair, re-anchors on that change.
			if w.rotation.cohortCapacity != 1 {
				t.Fatalf("a boost took a seat yet residual ordinary capacity = %d, want 1", w.rotation.cohortCapacity)
			}
			if len(w.rotation.committedCohort) != 1 {
				t.Fatalf("the committed ordinary cohort should be the single surviving seat, got %v", w.rotation.committedCohort)
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

	reconcileAndCommitAt(w, online, t0)
	requireResidencePair(t, w, "initial", "streamera", "streamerb")

	// The incumbent is pursuing a watch streak, and persisted deficit now wants
	// it out — the exact situation the bounded deferral exists for.
	streamers[0].Settings.WatchStreak = true
	streamers[0].Stream.MinuteWatched = 5
	if !w.nearStreakCompletion(0) {
		t.Fatal("precondition: streamera is not pursuing a watch streak")
	}
	seedMinutes(t, store, t0, map[string]float64{"streamera": 100, "streamerb": 100})

	reconcileAndCommitAt(w, online, t0.Add(fairRotationResidence-time.Second))
	requireResidencePair(t, w, "at 14:59", "streamera", "streamerb")
	if !w.rotation.deferUntil.IsZero() || w.rotation.deferUsed {
		t.Fatalf("the one-shot streak deferral was consulted while the pair was still resident: until=%v used=%v",
			w.rotation.deferUntil, w.rotation.deferUsed)
	}

	open := t0.Add(fairRotationResidence)
	reconcileAndCommitAt(w, online, open)
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
	if !residenceAnchor(w).Equal(t0) {
		t.Fatalf("the deferral re-stamped the residence anchor: %v", residenceAnchor(w))
	}
	after := open.Add(streakDeferDelay)
	reconcileAndCommitAt(w, online, after)
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
	// No drain goroutine: countingSender.Send and
	// staticChecker.CheckStreamerOnlineContext both publish with a non-blocking
	// select/default, so nothing can block on these channels and an unjoined
	// helper goroutine would only outlive the test body for no purpose.

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
		t.Fatalf("broker arbitration moved the base-pair anchor: %v -> %v", anchor, w.rotation.lastSwitch)
	}
	// Phase B took one of the two ordinary seats, so residence — which belongs to
	// the committed cohort, not to the untouched pair — now covers a single seat.
	if w.rotation.cohortCapacity != 1 {
		t.Fatalf("Phase B took a seat yet residual ordinary capacity = %d, want 1", w.rotation.cohortCapacity)
	}
	if len(w.rotation.committedCohort) != 1 {
		t.Fatalf("the committed ordinary cohort should be the single surviving seat, got %v", w.rotation.committedCohort)
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
		weights := w.watchWeights(online, at)
		reconcileAndCommitAt(w, online, at)

		// The pair really is the two least-watched: no off-pair candidate may
		// have strictly less accumulated time than a seated one.
		seated := map[int]bool{w.rotation.activePair[0]: true, w.rotation.activePair[1]: true}
		for _, in := range w.rotation.activePair {
			for _, out := range online {
				if seated[out] {
					continue
				}
				if weights[out] < weights[in] {
					t.Fatalf("round %d: %s (%.0fm) held a seat while %s (%.0fm) was more owed",
						round, streamers[in].GetUsername(), weights[in],
						streamers[out].GetUsername(), weights[out])
				}
			}
		}

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
	// The cap is a real allocation count, not the width of the activePair array.
	if got := w.selectRotating(online, time.Now()); len(got) != constants.MaxSimultaneousStreams {
		t.Fatalf("selection allocated %d slots, want exactly %d", len(got), constants.MaxSimultaneousStreams)
	}
}

// TestFairRotationResidenceAddsNoBackgroundOwner proves structurally — by
// reading the source of the decision path itself — that residence introduces no
// goroutine, timer, scheduler, store, ledger or cache owner.
//
// A runtime goroutine count cannot carry this claim: a goroutine that starts
// and finishes inside one call is invisible to any before/after sample, so such
// a check passes whether or not an owner was added, while still being able to
// fail for reasons unrelated to residence. The AST assertions below fail on
// exactly the constructs the owner contract forbids, and the reflective scan in
// TestFairRotationResidenceIsNotAConfigurableSetting covers the rotationState
// side.
func TestFairRotationResidenceAddsNoBackgroundOwner(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "watcher.go", nil, 0)
	if err != nil {
		t.Fatalf("parse watcher.go: %v", err)
	}

	// No package-level owner may back residence: the anchor lives in the
	// loop-owned rotationState and nowhere else.
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, name := range value.Names {
				lowered := strings.ToLower(name.Name)
				if strings.Contains(lowered, "residence") || strings.Contains(lowered, "resident") {
					t.Fatalf("residence gained a package-level owner: var %s at %s", name.Name, fset.Position(name.Pos()))
				}
			}
		}
	}

	// The decision path itself must start nothing and schedule nothing.
	var decision *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "reconcileLeastWatchedPair" {
			decision = fn
			break
		}
	}
	if decision == nil {
		t.Fatal("reconcileLeastWatchedPair not found: this test no longer guards the residence decision path")
	}
	banned := map[string]bool{
		"time.NewTimer": true, "time.NewTicker": true, "time.AfterFunc": true,
		"time.Tick": true, "time.Sleep": true,
		"context.WithTimeout": true, "context.WithDeadline": true,
	}
	ast.Inspect(decision.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.GoStmt:
			t.Fatalf("residence decision path spawns a goroutine at %s", fset.Position(node.Pos()))
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if name := pkg.Name + "." + sel.Sel.Name; banned[name] {
				t.Fatalf("residence decision path schedules work via %s at %s", name, fset.Position(node.Pos()))
			}
		}
		return true
	})

	// Deliberately no runtime.NumGoroutine() companion: a global goroutine count
	// depends on unrelated scheduling, so it can fail or pass for reasons that
	// have nothing to do with residence, and it proves nothing the assertions
	// above do not already carry.
}

// TestFairRotationResidenceConcurrentDiagnosticsReader runs the real production
// access pattern under -race: the loop goroutine reconciles and publishes while
// dashboard/debug readers poll the published snapshot. Residence adds no new
// cross-goroutine reader of the loop-owned rotation state.
func TestFairRotationResidenceConcurrentDiagnosticsReader(t *testing.T) {
	w, _, online, _ := newResidenceWatcher(t, 4)

	const readers = 4
	var wg, ready sync.WaitGroup
	var writerEpoch atomic.Int64
	var overlapped atomic.Int64
	stop := make(chan struct{})

	ready.Add(readers)
	for reader := 0; reader < readers; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Read once and report ready BEFORE the writer phase starts, so the
			// concurrent window is entered deterministically instead of being
			// left to the scheduler. Without this barrier every reader could
			// first be scheduled after close(stop), take the return arm on its
			// first select, and let the test pass having exercised no
			// concurrent access at all.
			_ = w.GetDebugState()
			_ = w.BrokerSnapshot()
			ready.Done()

			reported := false
			for {
				select {
				case <-stop:
					return
				default:
					// Record the read only when the SAME odd writer epoch is
					// observed on both sides of it, which places the whole read
					// inside one exact selection tick. Sampling an in-flight
					// boolean instead would accept a read split across two
					// adjacent ticks (see insideOneWriterCall).
					before := writerEpoch.Load()
					_ = w.GetDebugState()
					_ = w.BrokerSnapshot()
					if !reported && insideOneWriterCall(before, writerEpoch.Load()) {
						reported = true
						overlapped.Add(1)
					}
				}
			}
		}()
	}

	ready.Wait()

	// The epoch is odd only for the duration of the selection tick itself, and
	// every tick gets its own value, so "same odd epoch on both sides" means
	// one exact tick rather than "the loop is somewhere in progress". Keep
	// ticking past the nominal count until every reader has recorded an
	// overlapping read, bounded so a starved run fails loudly instead of
	// hanging. Nothing blocks inside the window, so this cannot deadlock.
	const nominalTicks, maxTicks = 500, 20000
	ticks := 0
	for ; ticks < maxTicks && (ticks < nominalTicks || overlapped.Load() < readers); ticks++ {
		openFairRotationResidence(w)
		enterWriterCall(&writerEpoch)
		runSelectionTick(w, online)
		leaveWriterCall(&writerEpoch)
	}

	close(stop)
	wg.Wait()

	// Placed after the join, so a failure here cannot strand a spinning reader
	// on the fixture's closed database handle.
	requireWriterCallsBracketed(t, &writerEpoch, ticks)

	if got := overlapped.Load(); got != readers {
		t.Fatalf("only %d of %d readers took a diagnostics read wholly inside one selection tick after %d ticks",
			got, readers, ticks)
	}

	if !w.rotation.hasPair {
		t.Fatal("concurrent diagnostics reads disturbed the loop-owned rotation state")
	}
}

// TestFairRotationResidenceIsNotAConfigurableSetting pins the shape the owner
// required: one private constant, no runtime setting, no revived
// rotation-interval surface, and no second residence owner in rotationState.
func TestFairRotationResidenceIsNotAConfigurableSetting(t *testing.T) {
	if fairRotationResidence != 15*time.Minute {
		t.Fatalf("fairRotationResidence = %v, want 15m", fairRotationResidence)
	}

	// The retired rotation-interval configuration surface stays retired: no
	// rotation/residence/dwell knob may appear on the rate-limit settings the
	// Settings page writes.
	limits := reflect.TypeOf(config.RateLimitSettings{})
	for i := 0; i < limits.NumField(); i++ {
		name := strings.ToLower(limits.Field(i).Name)
		for _, banned := range []string{"rotation", "residence", "dwell"} {
			if strings.Contains(name, banned) {
				t.Fatalf("residence became configurable: config.RateLimitSettings has field %q", limits.Field(i).Name)
			}
		}
	}

	// The residence anchor has exactly ONE owner: rotationState.cohortSince, the
	// anchor of the committed ordinary cohort. lastSwitch is no longer an anchor
	// at all — it keeps only its documented meaning, "when activePair last
	// actually changed", which is what diagnostics publish as PairSince.
	w, _, online, _ := newResidenceWatcher(t, 4)
	runSelectionTick(w, online)
	if st := w.GetDebugState(); !st.PairSince.Equal(w.rotation.lastSwitch) {
		t.Fatalf("PairSince stopped projecting base-pair membership: PairSince=%v lastSwitch=%v",
			st.PairSince, w.rotation.lastSwitch)
	}
	if w.rotation.cohortSince.IsZero() {
		t.Fatal("the committed ordinary cohort has no anchor after a selection that granted both seats")
	}

	// Exactly one field may answer "how long has this ordinary service been
	// protected". The ban is by ROLE, so it names the owner explicitly instead of
	// matching on a word an added field could simply avoid.
	const anchorOwner = "cohortsince"
	anchors := 0
	rotation := reflect.TypeOf(rotationState{})
	for i := 0; i < rotation.NumField(); i++ {
		name := strings.ToLower(rotation.Field(i).Name)
		if name == anchorOwner {
			anchors++
			continue
		}
		if name == "lastswitch" {
			continue // the base-pair membership timestamp, not a tenure anchor
		}
		if strings.Contains(name, "residence") || strings.Contains(name, "resident") ||
			strings.Contains(name, "anchor") || strings.Contains(name, "since") ||
			strings.Contains(name, "tenure") {
			t.Fatalf("a second residence owner appeared in rotationState: field %q", rotation.Field(i).Name)
		}
	}
	if anchors != 1 {
		t.Fatalf("rotationState declares %d residence anchors named %q, want exactly 1", anchors, anchorOwner)
	}

	// And no residence anchor may live outside rotationState on the watcher.
	watcher := reflect.TypeOf(MinuteWatcher{})
	for i := 0; i < watcher.NumField(); i++ {
		name := strings.ToLower(watcher.Field(i).Name)
		if strings.Contains(name, "residence") && name != "slotresidence" {
			t.Fatalf("a residence owner appeared outside rotationState: MinuteWatcher.%s", watcher.Field(i).Name)
		}
	}
}

// mutexCallOn reports whether the node is a call to w.<field>.<method>() — e.g.
// w.mu.Lock().
func mutexCallOn(n ast.Node, field, method string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	outer, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || outer.Sel.Name != method {
		return false
	}
	inner, ok := outer.X.(*ast.SelectorExpr)
	if !ok || inner.Sel.Name != field {
		return false
	}
	recv, ok := inner.X.(*ast.Ident)
	return ok && recv.Name == "w"
}

// receiverFieldCall reports whether the node is a call to w.<field>() — e.g.
// w.cancel().
func receiverFieldCall(n ast.Node, field string) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != field {
		return false
	}
	recv, ok := sel.X.(*ast.Ident)
	return ok && recv.Name == "w"
}

// TestStopCancelsInsideTheCommitLock pins the invariant the committed-allocation
// commit rests on: Stop must cancel the generation while HOLDING w.mu, the same
// lock commitFinalAllocation takes around its generation check and its two
// mutations.
//
// If Stop instead reads the cancel func under the lock and calls it after
// releasing, the commit lock buys nothing: the cancellation can then land while
// a tick holds w.mu, between its check and its mutation, and that tick credits
// committed service to a generation already ended. No dynamic test can separate
// the two orderings — both block Stop for as long as the lock is held, and the
// difference is an interleaving inside two critical sections — so the ordering
// is pinned where it is actually decided, in the source, the same way this file
// already pins that residence has no background owner.
func TestStopCancelsInsideTheCommitLock(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "watcher.go", nil, 0)
	if err != nil {
		t.Fatalf("parse watcher.go: %v", err)
	}

	var stop *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "Stop" && fn.Recv != nil {
			stop = fn
			break
		}
	}
	if stop == nil {
		t.Fatal("Stop not found: this test no longer guards the cancellation ordering")
	}

	type event struct {
		pos  token.Pos
		kind string
	}
	var events []event
	ast.Inspect(stop.Body, func(n ast.Node) bool {
		switch {
		case mutexCallOn(n, "mu", "Lock"):
			events = append(events, event{n.Pos(), "lock"})
		case mutexCallOn(n, "mu", "Unlock"):
			events = append(events, event{n.Pos(), "unlock"})
		case receiverFieldCall(n, "cancel"):
			events = append(events, event{n.Pos(), "cancel"})
		}
		return true
	})
	// A deferred unlock would keep the lock to the end of the function, which the
	// depth walk below would misread. Stop does not use one; fail loudly rather
	// than silently mis-measuring if that changes.
	for _, stmt := range stop.Body.List {
		if def, ok := stmt.(*ast.DeferStmt); ok && mutexCallOn(def.Call, "mu", "Unlock") {
			t.Fatalf("Stop now defers w.mu.Unlock at %s: update this test before trusting it",
				fset.Position(def.Pos()))
		}
	}
	sort.Slice(events, func(i, j int) bool { return events[i].pos < events[j].pos })

	depth, sawCancel := 0, false
	for _, e := range events {
		switch e.kind {
		case "lock":
			depth++
		case "unlock":
			depth--
		case "cancel":
			sawCancel = true
			if depth <= 0 {
				t.Fatalf("Stop calls w.cancel() at %s without holding w.mu: "+
					"the generation could then end while a tick holds the commit lock, between its "+
					"generation check and its commit, and that tick would credit committed service "+
					"to a generation already gone", fset.Position(e.pos))
			}
		}
	}
	if !sawCancel {
		t.Fatal("Stop no longer calls w.cancel(): this test no longer guards the cancellation ordering")
	}
}
