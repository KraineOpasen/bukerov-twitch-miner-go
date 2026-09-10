package watcher

import (
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// This file is the recurrence matrix for the committed ordinary cohort: the
// capacity states C0/C1/C2 and every transition between them, what does and does
// not start a fresh anchor, the deadline boundaries, and the fairness bounds
// that stop "no switching" from being satisfied by pinning one channel forever.
//
// Every case drives the real processWatching pipeline and reads the published
// snapshot. Deadlines are exercised by back-dating the loop-owned anchor, the
// package's existing deterministic stand-in for "this cohort has already been
// resident this long", so nothing here progresses by sleeping.

// strongerSource is the package's existing mutableSource, named for what these
// cases use it as: the one stronger occupant that can enter, hand off, refresh
// or leave between ticks.
func strongerSource(w *MinuteWatcher) *mutableSource {
	src := &mutableSource{}
	w.AddSource(src)
	return src
}

// propose replaces the source's proposal list for the next tick.
func propose(src *mutableSource, cand ...Candidate) { src.set(cand) }

// cohortLogins is the committed ordinary cohort as sorted logins.
func cohortLogins(w *MinuteWatcher) []string {
	out := make([]string, 0, len(w.rotation.committedCohort))
	for login := range w.rotation.committedCohort {
		out = append(out, login)
	}
	sort.Strings(out)
	return out
}

func requireCohort(t *testing.T, w *MinuteWatcher, context string, wantCapacity int, want ...string) {
	t.Helper()
	got := cohortLogins(w)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Fatalf("%s: committed ordinary cohort = %v, want %v", context, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: committed ordinary cohort = %v, want %v", context, got, want)
		}
	}
	if w.rotation.cohortCapacity != wantCapacity {
		t.Fatalf("%s: residual ordinary capacity = %d, want %d", context, w.rotation.cohortCapacity, wantCapacity)
	}
	if len(want) == 0 {
		if !w.rotation.cohortSince.IsZero() {
			t.Fatalf("%s: an empty cohort must invalidate the anchor, got %v", context, w.rotation.cohortSince)
		}
		return
	}
	if w.rotation.cohortSince.IsZero() {
		t.Fatalf("%s: a non-empty cohort must carry an anchor", context)
	}
}

// backdateCohort places the committed cohort's anchor exactly `elapsed` in the
// past, so the NEXT evaluation sees precisely that much residence consumed.
func backdateCohort(w *MinuteWatcher, elapsed time.Duration) {
	w.rotation.cohortSince = time.Now().Add(-elapsed)
}

// TestCommittedOrdinaryResidenceCapacityStates covers C2, C1 and C0 and pins
// that residual ordinary capacity is counted from the seats a stronger
// admission actually took — never from how many candidates carry a strong label.
func TestCommittedOrdinaryResidenceCapacityStates(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	src := strongerSource(w)
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	// C2: no stronger admission, both seats ordinary.
	w.processWatching(tickCtx(w))
	requireCohort(t, w, "C2", 2, "streamera", "streamerb")

	// C1: one stronger discovery occupant takes one seat.
	propose(src, Candidate{Streamer: discoveryStreamer("disco1", true), Origin: OriginDiscovery})
	w.processWatching(tickCtx(w))
	if got := len(cohortLogins(w)); got != 1 {
		t.Fatalf("C1: ordinary cohort size = %d, want 1 (%v)", got, cohortLogins(w))
	}
	if w.rotation.cohortCapacity != 1 {
		t.Fatalf("C1: residual capacity = %d, want 1", w.rotation.cohortCapacity)
	}

	// C0: a second stronger discovery occupant takes the remaining seat. No
	// ordinary service is held, so residence is invalidated outright.
	propose(src,
		Candidate{Streamer: discoveryStreamer("disco1", true), Origin: OriginDiscovery},
		Candidate{Streamer: discoveryStreamer("disco2", true), Origin: OriginDiscovery},
	)
	w.processWatching(tickCtx(w))
	snap := w.BrokerSnapshot()
	if len(snap.Slots) != constants.MaxSimultaneousStreams {
		t.Fatalf("C0: committed %d slots, want %d: %v", len(snap.Slots), constants.MaxSimultaneousStreams, brokerChannels(snap))
	}
	requireCohort(t, w, "C0", 0)
}

// TestCommittedOrdinaryResidenceCapacityTransitions walks 2->1->0->1->2 and
// pins the anchor rule at each step: a real change of committed membership or
// capacity starts one fresh COMMON anchor, an empty cohort invalidates, and a
// returning capacity initializes from its own commit.
func TestCommittedOrdinaryResidenceCapacityTransitions(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	src := strongerSource(w)
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	one := Candidate{Streamer: discoveryStreamer("disco1", true), Origin: OriginDiscovery}
	two := Candidate{Streamer: discoveryStreamer("disco2", true), Origin: OriginDiscovery}

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "C2", 2, "streamera", "streamerb")
	atTwo := w.rotation.cohortSince

	// 2 -> 1 shrinks immediately and re-anchors the surviving singleton.
	propose(src, one)
	w.processWatching(tickCtx(w))
	if len(cohortLogins(w)) != 1 || w.rotation.cohortCapacity != 1 {
		t.Fatalf("2->1: cohort=%v capacity=%d, want one member at capacity 1", cohortLogins(w), w.rotation.cohortCapacity)
	}
	if !w.rotation.cohortSince.After(atTwo) {
		t.Fatalf("2->1 did not start a fresh common anchor: %v -> %v", atTwo, w.rotation.cohortSince)
	}
	survivor := cohortLogins(w)[0]

	// 1 -> 0 invalidates.
	propose(src, one, two)
	w.processWatching(tickCtx(w))
	requireCohort(t, w, "1->0", 0)

	// 0 -> 1 initializes after the commit that actually restores capacity.
	propose(src, one)
	w.processWatching(tickCtx(w))
	if len(cohortLogins(w)) != 1 || w.rotation.cohortCapacity != 1 {
		t.Fatalf("0->1: cohort=%v capacity=%d, want one member at capacity 1", cohortLogins(w), w.rotation.cohortCapacity)
	}
	if w.rotation.cohortSince.IsZero() {
		t.Fatal("0->1 did not initialize an anchor after the commit")
	}
	reentered := w.rotation.cohortSince

	// 1 -> 2 refills the freed seat immediately and starts a fresh common anchor
	// for the pair, rather than leaving the refilled seat unprotected under the
	// singleton's older one.
	propose(src)
	w.processWatching(tickCtx(w))
	if got := len(cohortLogins(w)); got != 2 || w.rotation.cohortCapacity != 2 {
		t.Fatalf("1->2: cohort=%v capacity=%d, want two members at capacity 2", cohortLogins(w), w.rotation.cohortCapacity)
	}
	if !w.rotation.cohortSince.After(reentered) {
		t.Fatalf("1->2 did not start a fresh common anchor: %v -> %v", reentered, w.rotation.cohortSince)
	}
	if !containsLogin(cohortLogins(w), survivor) {
		t.Fatalf("the refill dropped the channel that was already being served: cohort=%v survivor=%q", cohortLogins(w), survivor)
	}
}

func containsLogin(logins []string, want string) bool {
	for _, l := range logins {
		if l == want {
			return true
		}
	}
	return false
}

// TestCommittedOrdinaryResidenceDemandBelowEqualAndAboveCapacity pins that the
// cohort is bounded by residual capacity and never leaves permitted capacity
// idle, at ordinary demand below, equal to and above C.
func TestCommittedOrdinaryResidenceDemandBelowEqualAndAboveCapacity(t *testing.T) {
	for _, tc := range []struct {
		name    string
		offline []string
		want    int
	}{
		{name: "demand below capacity", offline: []string{"streamerb", "streamerc", "streamerd"}, want: 1},
		{name: "demand equal to capacity", offline: []string{"streamerc", "streamerd"}, want: 2},
		{name: "demand above capacity", offline: nil, want: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			w := f.w
			byLogin := streamersByLogin(w.streamers)
			for _, login := range tc.offline {
				byLogin[login].SetConfirmedOffline()
			}
			f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

			w.processWatching(tickCtx(w))
			if got := len(cohortLogins(w)); got != tc.want {
				t.Fatalf("committed ordinary cohort = %v, want %d member(s)", cohortLogins(w), tc.want)
			}
			if w.rotation.cohortCapacity != constants.MaxSimultaneousStreams {
				t.Fatalf("no stronger admission took a seat, so capacity must stay %d, got %d",
					constants.MaxSimultaneousStreams, w.rotation.cohortCapacity)
			}
			if got := len(w.BrokerSnapshot().Slots); got != tc.want {
				t.Fatalf("committed %d slots for %d eligible ordinary channels: permitted capacity must not be held back", got, tc.want)
			}
		})
	}
}

// TestCommittedOrdinaryResidenceZeroCandidatesInvalidatesTheAnchor covers the
// PRODUCTION 2->0->3 path: every candidate really goes away, so the allocation
// is empty and the anchor is invalidated rather than left stale for whoever
// arrives next.
func TestCommittedOrdinaryResidenceZeroCandidatesInvalidatesTheAnchor(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	byLogin := streamersByLogin(w.streamers)
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "initial", 2, "streamera", "streamerb")

	for _, s := range w.streamers {
		s.SetConfirmedOffline()
	}
	w.processWatching(tickCtx(w))
	if got := len(w.BrokerSnapshot().Slots); got != 0 {
		t.Fatalf("no candidate is online yet %d slots are committed: %v", got, brokerChannels(w.BrokerSnapshot()))
	}
	requireCohort(t, w, "no candidates", 0)

	// Candidates return, with persisted deficit favouring the two that were NOT
	// previously resident. The stale anchor must protect nobody.
	//
	// getOnlineStreamers holds a channel out for 30s after it comes online so the
	// stream can settle, so the fixture back-dates OnlineAt the way the loop
	// harness does — this is a returning channel, not a brand-new one.
	for _, login := range []string{"streamera", "streamerb", "streamerc", "streamerd"} {
		byLogin[login].SetConfirmedOnline()
		byLogin[login].OnlineAt = time.Now().Add(-time.Minute)
	}
	f.seedWeights(t, time.Now(), map[string]float64{"streamera": 500, "streamerb": 500})
	w.processWatching(tickCtx(w))
	requireCohort(t, w, "after candidates returned", 2, "streamerc", "streamerd")
}

// TestCommittedOrdinaryResidenceAnchorIsInertForNonChanges pins the no-restamp
// rules: a cosmetic reason/campaign relabel on a seat the cohort already holds,
// a stronger occupant handing off to a different stronger occupant, and a
// stronger occupant merely refreshing all leave the anchor exactly where it was.
func TestCommittedOrdinaryResidenceAnchorIsInertForNonChanges(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	byLogin := streamersByLogin(w.streamers)
	src := strongerSource(w)
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	first := Candidate{Streamer: discoveryStreamer("disco1", true), Origin: OriginDiscovery}
	propose(src, first)
	w.processWatching(tickCtx(w))
	if len(cohortLogins(w)) != 1 {
		t.Fatalf("precondition: want a single committed ordinary seat, got %v", cohortLogins(w))
	}
	resident := cohortLogins(w)[0]
	anchor := w.rotation.cohortSince

	// The SAME stronger occupant re-proposed: a refresh, not a change.
	w.processWatching(tickCtx(w))
	requireCohort(t, w, "stronger occupant refreshed", 1, resident)
	if !w.rotation.cohortSince.Equal(anchor) {
		t.Fatalf("a refresh re-stamped the anchor: %v -> %v", anchor, w.rotation.cohortSince)
	}

	// A DIFFERENT stronger occupant takes the same seat: the ordinary cohort and
	// the residual capacity are both unchanged, so the resident keeps its term.
	propose(src, Candidate{Streamer: discoveryStreamer("disco2", true), Origin: OriginDiscovery})
	w.processWatching(tickCtx(w))
	requireCohort(t, w, "stronger target handoff", 1, resident)
	if !w.rotation.cohortSince.Equal(anchor) {
		t.Fatalf("a stronger-target handoff re-stamped the ordinary anchor: %v -> %v", anchor, w.rotation.cohortSince)
	}

	// A cosmetic campaign relabel on the RESIDENT seat: it keeps the seat it
	// already holds by fairness, so its role has not changed.
	makeDropCandidate(byLogin[resident], false)
	w.processWatching(tickCtx(w))
	requireCohort(t, w, "cosmetic reason relabel", 1, resident)
	if !w.rotation.cohortSince.Equal(anchor) {
		t.Fatalf("a cosmetic reason change re-stamped the anchor: %v -> %v", anchor, w.rotation.cohortSince)
	}
}

// TestCommittedOrdinaryResidenceRestampsOnBroadcastReplacement pins the other
// half of that rule: the same login on a REPLACED broadcast is not the same
// committed service, so stale tenure does not carry across it.
func TestCommittedOrdinaryResidenceRestampsOnBroadcastReplacement(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	byLogin := streamersByLogin(w.streamers)
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "initial", 2, "streamera", "streamerb")
	anchor := w.rotation.cohortSince

	// Same channel, same seat, a genuinely new broadcast.
	byLogin["streamera"].Stream.Update("broadcast-streamera-2", "", nil, nil, 1)
	w.processWatching(tickCtx(w))
	requireCohort(t, w, "after broadcast replacement", 2, "streamera", "streamerb")
	if !w.rotation.cohortSince.After(anchor) {
		t.Fatalf("a broadcast replacement did not invalidate stale tenure: %v -> %v", anchor, w.rotation.cohortSince)
	}
}

// TestCommittedOrdinaryResidenceDeadlineBoundaries drives R-epsilon, exactly R
// and R+epsilon against the same fixture. Before R the ordinary ranking cannot
// take the seat; at and after R it may — and, when the incumbents are still the
// most owed, expiry alone still switches nothing.
func TestCommittedOrdinaryResidenceDeadlineBoundaries(t *testing.T) {
	const epsilon = 30 * time.Second

	for _, tc := range []struct {
		name    string
		elapsed time.Duration
		want    []string
	}{
		{name: "R minus epsilon", elapsed: fairRotationResidence - epsilon, want: []string{"streamera", "streamerb"}},
		{name: "exactly R", elapsed: fairRotationResidence, want: []string{"streamerc", "streamerd"}},
		{name: "R plus epsilon", elapsed: fairRotationResidence + epsilon, want: []string{"streamerc", "streamerd"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newResidenceFixture(t, 4)
			w := f.w
			f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

			w.processWatching(tickCtx(w))
			requireCohort(t, w, "initial", 2, "streamera", "streamerb")

			// Persisted deficit now clearly favours the other two.
			f.seedWeights(t, time.Now(), map[string]float64{"streamera": 500, "streamerb": 500})
			backdateCohort(w, tc.elapsed)
			w.processWatching(tickCtx(w))
			requireCohort(t, w, tc.name, 2, tc.want...)
		})
	}
}

// TestCommittedOrdinaryResidenceExpiryAloneNeverForcesASwitch pins that reaching
// and long exceeding the deadline switches nothing while the incumbents remain
// the most owed: the rule is a floor on ordinary churn, not a cadence.
func TestCommittedOrdinaryResidenceExpiryAloneNeverForcesASwitch(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	f.seedWeights(t, time.Now(), map[string]float64{"streamerc": 500, "streamerd": 500})

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "initial", 2, "streamera", "streamerb")

	for _, elapsed := range []time.Duration{
		fairRotationResidence,
		fairRotationResidence + time.Minute,
		4 * fairRotationResidence,
	} {
		backdateCohort(w, elapsed)
		w.processWatching(tickCtx(w))
		requireCohort(t, w, "elapsed "+elapsed.String(), 2, "streamera", "streamerb")
	}
}

// TestCommittedOrdinaryResidenceStrongerAdmissionIsNeverDelayed pins that a
// stronger admission, and the loss of one, both take effect at the very next
// evaluation regardless of how much residence the ordinary cohort has left.
func TestCommittedOrdinaryResidenceStrongerAdmissionIsNeverDelayed(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	src := strongerSource(w)
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "initial", 2, "streamera", "streamerb")
	if elapsed := time.Since(w.rotation.cohortSince); elapsed >= fairRotationResidence {
		t.Fatalf("precondition: the cohort is no longer resident (elapsed %v)", elapsed)
	}

	// Entry, deep inside the residence window.
	propose(src, Candidate{Streamer: discoveryStreamer("disco1", true), Origin: OriginDiscovery})
	w.processWatching(tickCtx(w))
	snap := w.BrokerSnapshot()
	if !brokerHasChannel(snap, "disco1") {
		t.Fatalf("residence delayed a stronger admission: %v", brokerChannels(snap))
	}
	if len(snap.Slots) != constants.MaxSimultaneousStreams {
		t.Fatalf("stronger admission changed the cap: %v", brokerChannels(snap))
	}

	// Departure: the freed seat refills on the very next evaluation.
	propose(src)
	w.processWatching(tickCtx(w))
	if got := len(w.BrokerSnapshot().Slots); got != constants.MaxSimultaneousStreams {
		t.Fatalf("the freed seat was not refilled at once: %v", brokerChannels(w.BrokerSnapshot()))
	}
	// Both seats are ordinary again, at full residual capacity.
	if got := cohortLogins(w); len(got) != constants.MaxSimultaneousStreams {
		t.Fatalf("the ordinary cohort did not return to both seats: %v", got)
	}
	if w.rotation.cohortCapacity != constants.MaxSimultaneousStreams {
		t.Fatalf("residual ordinary capacity = %d, want %d after the stronger occupant left",
			w.rotation.cohortCapacity, constants.MaxSimultaneousStreams)
	}
	if w.rotation.cohortSince.IsZero() {
		t.Fatal("the refilled cohort carries no anchor")
	}
}

// TestCommittedOrdinaryResidenceSurvivesPermutationAndUnselectedChanges pins
// that neither reordering the runtime roster nor a change among candidates that
// were not selected disturbs the anchor: the cohort is compared as an unordered
// set of identities.
func TestCommittedOrdinaryResidenceSurvivesPermutationAndUnselectedChanges(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	byLogin := streamersByLogin(w.streamers)
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "initial", 2, "streamera", "streamerb")
	anchor := w.rotation.cohortSince

	// Reorder the roster: same channels, different indexes.
	reordered := []*models.Streamer{
		byLogin["streamerd"], byLogin["streamerb"], byLogin["streamerc"], byLogin["streamera"],
	}
	w.applyStreamerList(reordered)
	w.processWatching(tickCtx(w))
	requireCohort(t, w, "after a roster permutation", 2, "streamera", "streamerb")
	if !w.rotation.cohortSince.Equal(anchor) {
		t.Fatalf("a roster permutation re-stamped the anchor: %v -> %v", anchor, w.rotation.cohortSince)
	}

	// A change among channels that hold no seat: streamerc replaces its
	// broadcast. It stays ordinary and stays unselected, so the committed cohort
	// is untouched and so is its term.
	byLogin["streamerc"].Stream.Update("broadcast-streamerc-2", "", nil, nil, 1)
	w.processWatching(tickCtx(w))
	if !w.rotation.cohortSince.Equal(anchor) {
		t.Fatalf("a change among unselected candidates re-stamped the anchor: %v -> %v", anchor, w.rotation.cohortSince)
	}
}

// TestCommittedOrdinaryResidenceReleasesARemovedMember pins that a channel
// removed from the runtime roster stops being protected at once, and the seat it
// held is reallocated rather than reserved.
func TestCommittedOrdinaryResidenceReleasesARemovedMember(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	byLogin := streamersByLogin(w.streamers)
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "initial", 2, "streamera", "streamerb")

	w.applyStreamerList([]*models.Streamer{
		byLogin["streamerb"], byLogin["streamerc"], byLogin["streamerd"],
	})
	if _, stale := w.rotation.committedCohort["streamera"]; stale {
		t.Fatalf("a removed channel stayed in the committed cohort: %v", cohortLogins(w))
	}

	w.processWatching(tickCtx(w))
	if got := len(w.BrokerSnapshot().Slots); got != constants.MaxSimultaneousStreams {
		t.Fatalf("the removed member's seat was not reallocated: %v", brokerChannels(w.BrokerSnapshot()))
	}
	if containsLogin(cohortLogins(w), "streamera") {
		t.Fatalf("a removed channel is still in the cohort: %v", cohortLogins(w))
	}
}

// TestCommittedOrdinaryResidenceSurvivesAFailedSend pins that a transport
// failure does not move tenure: the grant was still committed, and a failed send
// credits no service, so the anchor and the cohort are unchanged.
func TestCommittedOrdinaryResidenceSurvivesAFailedSend(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "initial", 2, "streamera", "streamerb")
	anchor := w.rotation.cohortSince
	before := f.windowMinutes(t, "streamera")

	f.sender.err = errors.New("beacon rejected")
	w.processWatching(tickCtx(w))
	requireCohort(t, w, "after a failed send", 2, "streamera", "streamerb")
	if !w.rotation.cohortSince.Equal(anchor) {
		t.Fatalf("a failed send moved the anchor: %v -> %v", anchor, w.rotation.cohortSince)
	}
	if got := f.windowMinutes(t, "streamera"); got != before {
		t.Fatalf("a failed send credited persisted watch time: %v -> %v", before, got)
	}
}

// TestCommittedOrdinaryResidenceRestartKeepsHistoryNotTenure pins that a new
// process re-reads the persisted fairness history and starts with NO tenure: the
// first commit of the new process establishes its own anchor.
func TestCommittedOrdinaryResidenceRestartKeepsHistoryNotTenure(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "before the restart", 2, "streamera", "streamerb")

	// A new watcher over the SAME persisted store: the history survives, the
	// process-local tenure does not.
	restarted := newResidenceFixture(t, 4)
	restarted.w.store = f.store
	if !restarted.w.rotation.cohortSince.IsZero() || len(restarted.w.rotation.committedCohort) != 0 {
		t.Fatalf("a fresh process started with tenure: cohort=%v since=%v",
			cohortLogins(restarted.w), restarted.w.rotation.cohortSince)
	}

	restarted.w.processWatching(tickCtx(restarted.w))
	if restarted.w.rotation.cohortSince.IsZero() {
		t.Fatal("the first commit after a restart did not establish an anchor")
	}
	// The loaded ranking is the persisted one, so the two most owed are selected.
	requireCohort(t, restarted.w, "after the restart", 2, "streamera", "streamerb")
}

// runControlledEvaluations drives `steps` broker evaluations spaced `step`
// apart on a controlled clock, and returns the committed ordinary cohort seen at
// each one.
//
// Two things are modelled, and only these two. The clock: the loop-owned anchor
// is placed exactly as far in the past as the virtual time elapsed since the
// cohort was last re-anchored, so the production deadline is evaluated at real
// offsets without a sleep. And DELIVERY: each granted ordinary channel banks the
// whole step as persisted service. That is the stated assumption these bounds are
// quoted under — "successful delivery every `step`" — not a substitute for the
// delivered-service proof, which
// TestCommittedOrdinaryResidenceDeliversPersistedWatchTime establishes through
// the real send chain. Nothing here credits a channel that held no seat.
func (f *residenceFixture) runControlledEvaluations(t *testing.T, steps int, step time.Duration) []string {
	t.Helper()

	seen := make([]string, 0, steps)
	var virtual, anchoredAt time.Duration
	previous := ""

	for i := 0; i < steps; i++ {
		if !f.w.rotation.cohortSince.IsZero() {
			f.w.rotation.cohortSince = time.Now().Add(-(virtual - anchoredAt))
		}
		f.w.processWatching(tickCtx(f.w))

		cohort := cohortLogins(f.w)
		key := joinLogins(cohort)
		if key != previous {
			anchoredAt = virtual
			previous = key
		}
		seen = append(seen, key)

		for _, login := range cohort {
			if err := f.store.RecordMinutes(login, step.Minutes(), time.Now()); err != nil {
				t.Fatalf("model delivered service for %s: %v", login, err)
			}
		}
		virtual += step
	}
	return seen
}

func joinLogins(logins []string) string {
	out := ""
	for i, l := range logins {
		if i > 0 {
			out += "+"
		}
		out += l
	}
	return out
}

// firstGrantEvaluation returns, for each login, the index of the first
// evaluation at which it held a committed ordinary seat (-1 if never).
func firstGrantEvaluation(seen []string, logins []string) map[string]int {
	first := make(map[string]int, len(logins))
	for _, l := range logins {
		first[l] = -1
	}
	for i, key := range seen {
		for _, l := range logins {
			if first[l] >= 0 {
				continue
			}
			for _, member := range splitLogins(key) {
				if member == l {
					first[l] = i
				}
			}
		}
	}
	return first
}

func splitLogins(key string) []string {
	if key == "" {
		return nil
	}
	var out []string
	current := ""
	for _, r := range key {
		if r == '+' {
			out = append(out, current)
			current = ""
			continue
		}
		current += string(r)
	}
	return append(out, current)
}

// TestCommittedOrdinaryResidenceFairnessBoundAtCapacityOne proves the audit's
// stable-fixture bound rather than merely "nothing switched": with one residual
// ordinary seat, three eligible ordinary channels of equal history, no
// preference and successful delivery every 60s, the THIRD channel receives its
// first grant by 30 minutes and every channel has had a residence turn and
// positive persisted service by 45 minutes.
//
// It is also the falsifier for a pin-one-forever repair: a change that simply
// froze the incumbent would keep the cohort stable and fail this outright.
func TestCommittedOrdinaryResidenceFairnessBoundAtCapacityOne(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	byLogin := streamersByLogin(w.streamers)

	// streamerd is the stronger occupant: enough banked history to stay off the
	// fair pair, so it can only enter through the overlay and C is exactly 1.
	makeDropCandidate(byLogin["streamerd"], true)
	f.seedWeights(t, time.Now(), map[string]float64{"streamerd": 900})

	ordinary := []string{"streamera", "streamerb", "streamerc"}
	before := map[string]float64{}
	for _, l := range ordinary {
		before[l] = f.windowMinutes(t, l)
	}

	const step = time.Minute
	seen := f.runControlledEvaluations(t, 46, step)

	for i, key := range seen {
		if len(splitLogins(key)) != 1 {
			t.Fatalf("evaluation %d committed ordinary cohort %q, want exactly one seat at capacity 1", i, key)
		}
	}

	first := firstGrantEvaluation(seen, ordinary)
	served := 0
	for _, at := range first {
		if at >= 0 {
			served++
		}
	}
	if served != len(ordinary) {
		t.Fatalf("not every eligible ordinary channel received a residence turn within 45 minutes: %v (%v)", first, seen)
	}

	latestFirst := 0
	for _, at := range first {
		if at > latestFirst {
			latestFirst = at
		}
	}
	if latestFirst > 30 {
		t.Fatalf("the third channel's first grant landed at minute %d, past the 30-minute bound: %v", latestFirst, first)
	}
	for _, l := range ordinary {
		if got := f.windowMinutes(t, l) - before[l]; got <= 0 {
			t.Fatalf("%s completed a residence turn but banked no service: %v", l, got)
		}
	}
}

// TestCommittedOrdinaryResidenceFairnessBoundAtCapacityTwo is the same bound at
// full ordinary capacity: with three eligible ordinary channels of equal
// history, the third is served at the FIRST evaluation past the minimum
// residence — not before it, and not later than it.
func TestCommittedOrdinaryResidenceFairnessBoundAtCapacityTwo(t *testing.T) {
	f := newResidenceFixture(t, 3)

	ordinary := []string{"streamera", "streamerb", "streamerc"}
	const step = time.Minute
	seen := f.runControlledEvaluations(t, 17, step)

	for i, key := range seen {
		if len(splitLogins(key)) != 2 {
			t.Fatalf("evaluation %d committed ordinary cohort %q, want both seats at capacity 2", i, key)
		}
	}

	first := firstGrantEvaluation(seen, ordinary)
	third := -1
	for _, at := range first {
		if at > third {
			third = at
		}
	}
	if third < 0 {
		t.Fatalf("an eligible ordinary channel was never served: %v (%v)", first, seen)
	}
	if third != int(fairRotationResidence/step) {
		t.Fatalf("the third channel was first served at evaluation %d, want the first evaluation past the %v minimum residence: %v",
			third, fairRotationResidence, seen)
	}
}

// TestCommittedOrdinaryResidenceServesOwedWaitersAfterResidence is the
// end-to-end shape of acceptance A on the incident fixture: the permitted
// ordinary cohort is retained for the whole residence, it banks real delivered
// service while it holds the seat, and the owed waiter is then served.
func TestCommittedOrdinaryResidenceServesOwedWaitersAfterResidence(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	byLogin := streamersByLogin(w.streamers)
	makeDropCandidate(byLogin["streamerd"], true)
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 900})

	// Held for the whole window, through repeated real evaluations.
	for i := 0; i < 6; i++ {
		w.processWatching(tickCtx(w))
	}
	requireCohort(t, w, "inside the residence window", 1, "streamera")

	// Real delivered service, through the production chain only.
	if got := f.windowMinutes(t, "streamera"); got <= 0 {
		t.Fatalf("the retained ordinary channel banked no delivered service: %v", got)
	}
	// The waiter has been credited nothing while waiting.
	if got := f.windowMinutes(t, "streamerb"); got != 0.5 {
		t.Fatalf("a waiting channel was credited service: %v", got)
	}

	// Past the residence the owed waiter takes the seat. streamera has now been
	// served for a whole residence, so the ranking really does owe streamerb more:
	// credit the minutes that residence delivered (the same stated 60s-delivery
	// assumption the fairness bounds are quoted under — the real chain cannot bank
	// fifteen wall-clock minutes inside a test) and re-open the ranking.
	if err := f.store.RecordMinutes("streamera", fairRotationResidence.Minutes(), time.Now()); err != nil {
		t.Fatalf("model the service the residence delivered: %v", err)
	}
	backdateCohort(w, fairRotationResidence)
	w.processWatching(tickCtx(w))
	requireCohort(t, w, "past the residence", 1, "streamerb")
}

// TestCommittedOrdinaryResidenceRejectedProposalConsumesNoTurn pins acceptance
// C's recency half: a channel Phase A proposed but the broker then displaced
// received no service, so it must not be ranked as though it had been watched.
//
// The observable consequence is its place in the queue. Four ordinary channels
// of equal history: the first evaluation proposes two, a stronger discovery
// contender displaces one of them, and only the survivor was actually served.
// At the next evaluation the displaced channel is therefore still among the most
// owed and takes a seat. If a losing proposal advanced recency, it would be
// ranked behind the two channels that were never even proposed, and would be
// passed over here.
func TestCommittedOrdinaryResidenceRejectedProposalConsumesNoTurn(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	src := strongerSource(w)
	propose(src, Candidate{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery})

	w.processWatching(tickCtx(w))
	snap := w.BrokerSnapshot()
	if !brokerHasChannel(snap, "disco") {
		t.Fatalf("precondition: the stronger contender did not take a seat: %v", brokerChannels(snap))
	}
	cohort := cohortLogins(w)
	if len(cohort) != 1 {
		t.Fatalf("precondition: want exactly one committed ordinary seat, got %v", cohort)
	}
	served := cohort[0]

	// Identify the channel Phase A proposed alongside it and the broker rejected.
	displaced := ""
	for _, c := range snap.Waiting {
		if c.Origin == OriginConfigured {
			displaced = c.Channel
		}
	}
	if displaced == "" {
		t.Fatalf("precondition: no configured channel was displaced: %+v", snap.Waiting)
	}
	if displaced == served {
		t.Fatal("precondition: the displaced channel cannot also be the served one")
	}

	// The stronger contender leaves and the residence opens: the ranking is free.
	propose(src)
	backdateCohort(w, fairRotationResidence)
	w.processWatching(tickCtx(w))

	if !containsLogin(cohortLogins(w), displaced) {
		t.Fatalf("the channel whose proposal was rejected (%q) was passed over at the next evaluation: cohort=%v served=%q: "+
			"a losing proposal must consume no turn", displaced, cohortLogins(w), served)
	}
}

// TestCommittedOrdinaryResidenceKeepsTermWhenABroadcastBecomesKnown pins the
// other side of the broadcast rule: a seat committed before its broadcast was
// identified, then observed on a real one, has not been replaced. The residence
// of a channel that never stopped being served must not restart because its
// session finally became identifiable — and a genuine replacement afterwards
// must still be measured against that now-known identity.
func TestCommittedOrdinaryResidenceKeepsTermWhenABroadcastBecomesKnown(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	// Put streamera on the roster with NO broadcast identity yet, exactly as a
	// cold start leaves it before the convergence refresh identifies the session.
	// Stream.Update deliberately never clobbers a known id with an empty one, so
	// the seat has to start unidentified rather than be cleared.
	unidentified := models.NewStreamer("streamera", models.DefaultStreamerSettings())
	unidentified.ChannelID = "ch-streamera"
	unidentified.SetConfirmedOnline()
	unidentified.OnlineAt = time.Now().Add(-time.Minute)
	unidentified.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
	roster := append([]*models.Streamer(nil), w.streamers...)
	roster[0] = unidentified
	w.applyStreamerList(roster)
	byLogin := streamersByLogin(w.streamers)
	if byLogin["streamera"].Stream.GetBroadcastID() != "" {
		t.Fatalf("fixture: streamera was expected to start without a broadcast identity, got %q",
			byLogin["streamera"].Stream.GetBroadcastID())
	}
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "committed before the broadcast was known", 2, "streamera", "streamerb")
	anchor := w.rotation.cohortSince

	byLogin["streamera"].Stream.Update("broadcast-streamera", "", nil, nil, 1)
	w.processWatching(tickCtx(w))
	requireCohort(t, w, "after the broadcast became known", 2, "streamera", "streamerb")
	if !w.rotation.cohortSince.Equal(anchor) {
		t.Fatalf("learning the broadcast identity restarted a residence that never lapsed: %v -> %v", anchor, w.rotation.cohortSince)
	}

	// A real replacement of that now-known broadcast still invalidates the term.
	byLogin["streamera"].Stream.Update("broadcast-streamera-2", "", nil, nil, 1)
	w.processWatching(tickCtx(w))
	if !w.rotation.cohortSince.After(anchor) {
		t.Fatalf("a replacement of the known broadcast did not invalidate stale tenure: %v -> %v", anchor, w.rotation.cohortSince)
	}
}

// TestCommittedOrdinaryResidenceDropsTenureAcrossARename pins that a
// pointer-preserving rename leaves no unreachable residence behind. The rename
// changes the login on the existing roster entry too, so the pre-rename key
// cannot be found from the departed-streamer side; a same-process re-add of the
// old login must not inherit a term it never earned.
//
// It drives applyStreamerList directly. In production a rename reaches the
// watcher only when it arrives with a roster apply, so this pins the prune's
// correctness at the seam rather than claiming the miner performs rename-only
// applies.
func TestCommittedOrdinaryResidenceDropsTenureAcrossARename(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	byLogin := streamersByLogin(w.streamers)
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "before the rename", 2, "streamera", "streamerb")

	renamed := byLogin["streamera"]
	obs := renamed.BeginLoginObservation()
	if !renamed.RenameIfCurrent("streameraa", obs) {
		t.Fatal("rename fixture did not apply")
	}
	w.applyStreamerList(append([]*models.Streamer(nil), w.streamers...))

	if _, stale := w.rotation.committedCohort["streamera"]; stale {
		t.Fatalf("the pre-rename login kept a residence entry nothing can reach: %v", cohortLogins(w))
	}
	for login := range w.rotation.committedCohort {
		found := false
		for _, s := range w.streamers {
			if s.GetUsername() == login {
				found = true
			}
		}
		if !found {
			t.Fatalf("the cohort holds a login that is not on the roster: %q (%v)", login, cohortLogins(w))
		}
	}
}

// TestCommittedOrdinaryResidenceRotatesAfterResidenceInDirectMode closes the
// direct-mode path end to end. Recency now records committed grants there too,
// so this pins that the surviving configured channel is held for the residence
// and then genuinely yields to the one that was displaced — the cold-start
// alternation is replaced by a bounded turn, not by a permanent pin.
func TestCommittedOrdinaryResidenceRotatesAfterResidenceInDirectMode(t *testing.T) {
	f := newResidenceFixture(t, 2)
	w := f.w
	src := strongerSource(w)
	propose(src, Candidate{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery})

	w.processWatching(tickCtx(w))
	first := cohortLogins(w)
	if len(first) != 1 {
		t.Fatalf("precondition: want exactly one committed ordinary seat, got %v", first)
	}

	// Held across repeated evaluations inside the window.
	for i := 0; i < 4; i++ {
		w.processWatching(tickCtx(w))
		if got := cohortLogins(w); len(got) != 1 || got[0] != first[0] {
			t.Fatalf("the direct-mode ordinary seat changed hands inside its residence: %v -> %v", first, got)
		}
	}

	// Past the residence the other configured channel takes its turn.
	backdateCohort(w, fairRotationResidence)
	w.processWatching(tickCtx(w))
	second := cohortLogins(w)
	if len(second) != 1 || second[0] == first[0] {
		t.Fatalf("the direct-mode seat never rotated past its residence: %v -> %v", first, second)
	}
}

// TestCommittedOrdinaryResidenceDefersAStreakOnANonResidentSeat pins the
// deferral's shape under a PARTIAL cohort. The resident seat is not up for
// replacement, but the seat the cohort does not hold is re-ranked on this very
// evaluation — so a replacement there is real, and the one-shot bounded streak
// deferral is available for it exactly as it always was.
func TestCommittedOrdinaryResidenceDefersAStreakOnANonResidentSeat(t *testing.T) {
	w, streamers, online, store := newResidenceWatcher(t, 4)
	t0 := time.Now()

	reconcileAndCommitAt(w, online, t0)
	requireResidencePair(t, w, "initial", "streamera", "streamerb")

	// Only streamera keeps a committed ordinary seat: streamerb's is not held.
	w.rotation.committedCohort = map[string]string{"streamera": w.streamers[0].Stream.GetBroadcastID()}
	w.rotation.cohortCapacity = 1

	// streamerb is pursuing a streak and persisted deficit now wants it out.
	streamers[1].Settings.WatchStreak = true
	streamers[1].Stream.MinuteWatched = 5
	if !w.nearStreakCompletion(1) {
		t.Fatal("precondition: streamerb is not pursuing a watch streak")
	}
	seedMinutes(t, store, t0, map[string]float64{"streamera": 100, "streamerb": 100})

	w.selectionMode = ModeRotation
	w.reconcileLeastWatchedPair(online, t0.Add(time.Minute))

	if !w.rotation.deferUsed || w.rotation.deferStreamer != 1 {
		t.Fatalf("a real replacement on the non-resident seat was not deferred: used=%v streamer=%d",
			w.rotation.deferUsed, w.rotation.deferStreamer)
	}
	requireResidencePair(t, w, "the resident seat is untouched", "streamera", "streamerb")
}

// TestCommittedOrdinaryResidenceFollowsTheAcceptedProvisionalOutcome pins that
// the cohort follows the FINAL allocation across a provisional overlay's whole
// lifecycle. A proof that promotes an existing configured seat, and a final
// proof reconciliation that rejects it and restores the exact fallback, both
// change the seat's reason LABEL and neither changes who is being served — so
// the accepted cohort is recorded and the term is not restarted either way.
//
// It drives the same seam processWatching drives, in the same order: arbitrate,
// then reconcileProvisionalSlots, then commit against whatever came out.
func TestCommittedOrdinaryResidenceFollowsTheAcceptedProvisionalOutcome(t *testing.T) {
	w, _ := newTestWatcher(1)
	w.SetProvisionalMonitoringEnabled(true)
	w.selectionMode = ModeDirect
	w.selectionReasons = map[int]string{0: "original configured selection"}
	owner := w.streamers[0]
	candidate := configuredProvisionalFixture(t, owner)

	commit := func(slots []slotOccupant) {
		w.commitOrdinaryResidence(slots, time.Now())
	}

	initial, _ := w.arbitrate([]int{0}, []Candidate{{
		Streamer: owner, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
	}}, time.Now())
	admitted, _ := w.reconcileProvisionalSlots(initial, nil, time.Now())
	if len(admitted) != 1 {
		t.Fatal("precondition: the configured overlay was not admitted")
	}
	commit(admitted)
	requireCohort(t, w, "unproved overlay on a configured seat", 2, owner.GetUsername())
	anchor := w.rotation.cohortSince

	lease, _ := w.ProvisionalLease()
	baselineAt := lease.ReservedAt.Add(time.Second)
	if !w.ArmProvisionalLease(lease.LeaseID, 1, baselineAt, 1) ||
		!w.ObserveProvisionalProgress(lease.LeaseID, 2, baselineAt.Add(time.Second), 2) {
		t.Fatal("precondition: the overlay was not proved")
	}

	promoted, _ := w.arbitrate([]int{0}, []Candidate{{
		Streamer: owner, Origin: OriginDiscovery, ProvisionalDrop: &candidate,
	}}, time.Now())
	if len(promoted) != 1 || !promoted[0].provisionalProven {
		t.Fatalf("precondition: the proof did not promote the seat: %+v", promoted)
	}
	proven, _ := w.reconcileProvisionalSlots(promoted, nil, time.Now())
	commit(proven)
	requireCohort(t, w, "after the proof promoted the seat's label", 2, owner.GetUsername())
	if !w.rotation.cohortSince.Equal(anchor) {
		t.Fatalf("a proof promotion on a seat the cohort already held restarted its term: %v -> %v",
			anchor, w.rotation.cohortSince)
	}

	// Final proof reconciliation now REJECTS the overlay and restores the exact
	// configured fallback. The accepted cohort is what gets recorded.
	owner.Stream.SetSpadeURL("https://spade.invalid/proof-invalidated")
	restored, _ := w.reconcileProvisionalSlots(promoted, nil, time.Now())
	if len(restored) != 1 || restored[0].origin != OriginConfigured || restored[0].provisionalDrop != nil {
		t.Fatalf("precondition: the rejected proof did not restore the configured fallback: %+v", restored)
	}
	commit(restored)
	requireCohort(t, w, "after the final proof was rejected", 2, owner.GetUsername())
	if !w.rotation.cohortSince.Equal(anchor) {
		t.Fatalf("a rejected proof restarted the term of a seat that never stopped being served: %v -> %v",
			anchor, w.rotation.cohortSince)
	}
}

// TestCommittedOrdinaryResidenceCountsCapacityAfterFinalProofReconciliation
// pins WHERE the cohort is settled, not just what it contains. Arbitration puts
// an unproved provisional observation into the idle slot, and final proof
// reconciliation then removes it. Only the allocation that survives that
// reconciliation is the committed one, so residual ordinary capacity must be
// counted from it — a cohort settled on the pre-reconciliation slice would
// record a seat that was never actually granted.
func TestCommittedOrdinaryResidenceCountsCapacityAfterFinalProofReconciliation(t *testing.T) {
	f := newResidenceFixture(t, 1)
	w := f.w

	external, candidate := provisionalWatcherFixture(t, "ext", "ext-channel", "game-9")
	src := strongerSource(w)
	propose(src, Candidate{Streamer: external, Origin: OriginDiscovery, ProvisionalDrop: &candidate})

	w.processWatching(tickCtx(w))

	snap := w.BrokerSnapshot()
	if len(snap.Slots) != 1 || snap.Slots[0].Channel != "streamera" {
		t.Fatalf("final proof reconciliation did not own the committed set: %v", brokerChannels(snap))
	}
	// One configured seat, no stronger admission survived: full ordinary capacity.
	requireCohort(t, w, "after final proof reconciliation", constants.MaxSimultaneousStreams, "streamera")
}

// TestCommittedOrdinaryResidenceHoldsThroughAnUnknownBlip pins the interaction
// between residence and the existing UNKNOWN continuity retention. A channel
// that goes online->unknown while holding a committed seat is retained through
// the blip, and its residence is retained with it — the two must agree, or the
// seat would be protected by one rule and released by the other. Once the
// uncertainty outlives its bounded grace the channel stops being a candidate at
// all, and residence does not delay that.
func TestCommittedOrdinaryResidenceHoldsThroughAnUnknownBlip(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	byLogin := streamersByLogin(w.streamers)
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "initial", 2, "streamera", "streamerb")
	anchor := w.rotation.cohortSince

	// A transient check failure: online -> unknown while holding the seat.
	byLogin["streamera"].SetUnknown(models.ReasonTransportError)
	if !w.retainsSlotWhileUnknown(byLogin["streamera"]) {
		t.Fatal("precondition: the blip is not inside the retention grace")
	}
	w.processWatching(tickCtx(w))
	requireCohort(t, w, "during the unknown blip", 2, "streamera", "streamerb")
	if !w.rotation.cohortSince.Equal(anchor) {
		t.Fatalf("a retained unknown blip restarted the term: %v -> %v", anchor, w.rotation.cohortSince)
	}

	// The uncertainty outlives its bounded grace: the channel is no longer a
	// candidate, so it is replaced at once rather than held by its residence.
	byLogin["streamera"].UnknownSince = time.Now().Add(-2 * unknownSlotRetentionGrace)
	w.processWatching(tickCtx(w))
	if containsLogin(cohortLogins(w), "streamera") {
		t.Fatalf("residence delayed the release of a channel whose unknown outlived its grace: %v", cohortLogins(w))
	}
}

// TestCommittedOrdinaryResidenceRestampsOnARoleChangingReason is the other half
// of the cosmetic-relabel rule. A reason change that actually moves a channel
// from the ordinary role into a stronger admission changes the role partition
// and the residual capacity, so it forces reconciliation and a fresh common
// anchor — where the same relabel on a seat the cohort keeps by fairness
// (TestCommittedOrdinaryResidenceAnchorIsInertForNonChanges) changes nothing.
func TestCommittedOrdinaryResidenceRestampsOnARoleChangingReason(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	byLogin := streamersByLogin(w.streamers)
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "initial", 2, "streamera", "streamerb")
	anchor := w.rotation.cohortSince

	// streamera acquires a drop AND enough banked history to fall out of the fair
	// pair: it can now only hold a seat through the overlay, which is a stronger
	// admission and no longer an ordinary one.
	makeDropCandidate(byLogin["streamera"], true)
	f.seedWeights(t, time.Now(), map[string]float64{"streamera": 500})
	backdateCohort(w, fairRotationResidence)

	w.processWatching(tickCtx(w))

	snap := w.BrokerSnapshot()
	if !brokerHasChannel(snap, "streamera") {
		t.Fatalf("the newly stronger channel was not admitted: %v", brokerChannels(snap))
	}
	if containsLogin(cohortLogins(w), "streamera") {
		t.Fatalf("a channel seated by the overlay is still counted as ordinary service: %v", cohortLogins(w))
	}
	if w.rotation.cohortCapacity != 1 {
		t.Fatalf("residual ordinary capacity = %d, want 1 once a stronger admission took a seat", w.rotation.cohortCapacity)
	}
	if !w.rotation.cohortSince.After(anchor) {
		t.Fatalf("a role-changing reason did not force reconciliation: %v -> %v", anchor, w.rotation.cohortSince)
	}
}

// TestCommittedOrdinaryResidenceDoesNotPinAChannelUnderStrongerChurn is the
// anti-pin falsifier for the one common anchor.
//
// A real change of committed membership or capacity starts a fresh common
// anchor, which is the rule. A stronger occupant that arrives and leaves on
// alternating evaluations therefore re-anchors the cohort on EVERY evaluation,
// and the residence deadline is never reached — so residence can no longer
// separate the two seats, and whatever decides the victim then decides it for
// the whole uptime. Persisted deficit has to be what decides it: the channel
// that has been served the most is the one that yields. If that tie-break falls
// through to recency or a fixed login order instead, one channel holds a seat
// indefinitely against the very ranking the residence exists to serve.
func TestCommittedOrdinaryResidenceDoesNotPinAChannelUnderStrongerChurn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		channels int
		ordinary []string
		churn    func(t *testing.T, f *residenceFixture, on bool)
	}{
		{
			// Cross-source path: a discovery restricted drop that comes and goes.
			name:     "external discovery contender",
			channels: 4,
			ordinary: []string{"streamera", "streamerb", "streamerc", "streamerd"},
			churn: func(t *testing.T, f *residenceFixture, on bool) {
				src := f.churnSource(t)
				if on {
					propose(src, Candidate{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery})
					return
				}
				propose(src)
			},
		},
		{
			// Configured boost path: an off-pair channel whose drop campaign is
			// present on alternating evaluations.
			name:     "configured off-pair boost",
			channels: 5,
			ordinary: []string{"streamera", "streamerb", "streamerc", "streamerd"},
			churn: func(t *testing.T, f *residenceFixture, on bool) {
				booster := streamersByLogin(f.w.streamers)["streamere"]
				if on {
					makeDropCandidate(booster, true)
					return
				}
				booster.Stream.SetCampaigns(nil)
				booster.Stream.SetCampaignIDs(nil)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runChurnPinCase(t, tc.channels, tc.ordinary, tc.churn)
		})
	}
}

// churnSource lazily attaches the one mutable source a churn case uses.
func (f *residenceFixture) churnSource(t *testing.T) *mutableSource {
	t.Helper()
	if f.src == nil {
		f.src = strongerSource(f.w)
	}
	return f.src
}

func runChurnPinCase(t *testing.T, channels int, ordinary []string, churn func(*testing.T, *residenceFixture, bool)) {
	t.Helper()
	f := newResidenceFixture(t, channels)
	w := f.w
	// Any channel outside the ordinary set is the churning stronger candidate:
	// bank enough history to keep it off the fair pair so it can only ever enter
	// through a stronger admission.
	byLogin := streamersByLogin(w.streamers)
	for login := range byLogin {
		if !containsLogin(ordinary, login) {
			f.seedWeights(t, time.Now(), map[string]float64{login: 900})
		}
	}

	const evaluations = 40
	grants := map[string]int{}
	for i := 0; i < evaluations; i++ {
		churn(t, f, i%2 == 0)
		w.processWatching(tickCtx(w))

		cohort := cohortLogins(w)
		for _, login := range cohort {
			grants[login]++
			// Stated delivery assumption, as in the fairness bounds: a granted
			// ordinary seat banks its evaluation as persisted service.
			if err := f.store.RecordMinutes(login, 1, time.Now()); err != nil {
				t.Fatalf("model delivered service for %s: %v", login, err)
			}
		}
	}

	most, least := 0, evaluations+1
	for _, login := range ordinary {
		if grants[login] > most {
			most = grants[login]
		}
		if grants[login] < least {
			least = grants[login]
		}
	}
	if least == 0 {
		t.Fatalf("a channel was never granted an ordinary seat across %d evaluations: %v", evaluations, grants)
	}
	// Under equal history and equal delivery no channel may take more than
	// double another's share. A pinned channel takes an order of magnitude more.
	if most > 2*least {
		t.Fatalf("stronger-occupant churn pinned a channel: grants=%v (most=%d, least=%d) — "+
			"a cohort re-anchored on every evaluation must still yield the seat to the most owed channel",
			grants, most, least)
	}
}

// TestCommittedOrdinaryResidenceRestampsOnCapacityAloneWhenMembershipIsUnchanged
// isolates the capacity term of the re-anchor rule. A stronger candidate fills
// the IDLE second slot: the committed ordinary cohort is the same single login on
// the same broadcast, so membership and identity are both unchanged, and only the
// residual ordinary capacity moved (2 -> 1). That alone is a real change and must
// start a fresh common anchor — otherwise the surviving seat would serve a
// capacity it was never committed against.
func TestCommittedOrdinaryResidenceRestampsOnCapacityAloneWhenMembershipIsUnchanged(t *testing.T) {
	f := newResidenceFixture(t, 1)
	w := f.w
	src := strongerSource(w)

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "one configured channel, both seats ordinary capacity", constants.MaxSimultaneousStreams, "streamera")
	anchor := w.rotation.cohortSince
	broadcast := w.rotation.committedCohort["streamera"]

	// A stronger candidate takes the idle seat. Nothing about streamera changes.
	propose(src, Candidate{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery})
	w.processWatching(tickCtx(w))

	if got := w.rotation.committedCohort["streamera"]; got != broadcast {
		t.Fatalf("the surviving seat's identity changed: %q -> %q", broadcast, got)
	}
	requireCohort(t, w, "after the idle seat was taken", 1, "streamera")
	if !w.rotation.cohortSince.After(anchor) {
		t.Fatalf("a capacity change alone did not start a fresh common anchor: %v -> %v", anchor, w.rotation.cohortSince)
	}
}

// TestCommittedOrdinaryResidenceNeverProtectsAnExternalOccupant pins that
// residence is keyed to CONFIGURED seats only. An external occupant carries no
// streamer index, so it can never join the ordinary cohort — and it must not be
// treated as resident even when its login collides with a cohort member's, which
// is the one case a login-keyed lookup alone would get wrong.
func TestCommittedOrdinaryResidenceNeverProtectsAnExternalOccupant(t *testing.T) {
	f := newResidenceFixture(t, 4)
	w := f.w
	f.seedWeights(t, time.Now(), map[string]float64{"streamerb": 0.5, "streamerc": 30, "streamerd": 90})

	w.processWatching(tickCtx(w))
	requireCohort(t, w, "initial", 2, "streamera", "streamerb")

	// An external occupant whose login collides with a committed cohort member.
	colliding := slotOccupant{
		streamer: discoveryStreamer("streamera", true),
		origin:   OriginDiscovery,
		idx:      -1,
	}
	if w.residentOrdinarySlot(colliding, time.Now()) {
		t.Fatal("an external occupant was treated as holding committed ordinary residence")
	}
	// The configured seat of the same login is resident, so the collision really
	// is the only thing the guard is separating.
	configured := slotOccupant{streamer: w.streamers[0], origin: OriginConfigured, idx: 0}
	if !w.residentOrdinarySlot(configured, time.Now()) {
		t.Fatal("the configured seat that holds the committed ordinary service is not resident")
	}
}
