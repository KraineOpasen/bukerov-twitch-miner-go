package watcher

import (
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/journal"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// This file exercises the fix for the late-online-no-spade delivery gap
// documented in late_online_spade_delivery_test.go: convergeIncompleteSlotSessions
// (internal/watcher/session.go) stages a bounded, deduplicated,
// event-triggered spade-fetch refresh — through the EXISTING correlated
// session-refresh path (RequestSessionRefresh / executeSessionRefreshes /
// RefreshPlaybackSession / ApplyPlaybackSessionIfCurrent) — for any currently
// slotted channel whose session is missing a spade URL. No new transport, no
// new fencing, no new periodic request cadence.

// perLoginFailRefresher wraps a real *fakeRefresher and forces a spade-fetch
// failure for a chosen set of logins (modelling a persistent per-channel
// convergence failure), while delegating every other login — and every
// non-spade refresh — to the inner fake unchanged. Used to make ONE incomplete
// streamer's convergence fail while a DIFFERENT incomplete streamer's succeeds
// through the very same shared refresher.
type perLoginFailRefresher struct {
	inner      *fakeRefresher
	failLogins map[string]bool
}

func (f *perLoginFailRefresher) RefreshPlaybackSession(s *models.Streamer, fetchSpade bool, expected models.ExpectedSession) twitch.SessionRefreshResult {
	if fetchSpade && f.failLogins[s.GetUsername()] {
		return twitch.SessionRefreshResult{
			Stage:              "spade",
			CurrentGeneration:  s.Stream.SessionGeneration(),
			CurrentBroadcastID: s.Stream.GetBroadcastID(),
		}
	}
	return f.inner.RefreshPlaybackSession(s, fetchSpade, expected)
}

// T4: an incomplete session delivers nothing (and credits nothing, and never
// goes offline) until its convergence refresh succeeds; a LATER tick then
// delivers a real beacon off the newly-published session.
func TestSessionConverge_IncompleteThenCompleteDelivers(t *testing.T) {
	b := bringOnlineCoherent("streamerb", "cid-b", "broadcast-b", "http://spade.test/b")
	c := bringOnlineViaStreamUp("streamerc", "cid-c", "broadcast-c")

	w, rt, adapter, _ := newDeliveryLifecycleWatcher(t, []*models.Streamer{b, c})
	ref := &fakeRefresher{spadeErr: errors.New("transient spade failure")}
	w.refresher = ref

	cLogin := c.GetUsername()

	// Tick 0: incomplete session, the convergence attempt fails -> no
	// delivery, no watched-minute credit, and no false offline.
	w.processWatching()

	calls := adapter.callsFor(cLogin)
	if len(calls) != 1 || calls[0].result.Delivered {
		t.Fatalf("tick 0: expected %s's send to fail (no spade yet), got %+v", cLogin, calls)
	}
	if got := c.Stream.GetMinuteWatched(); got != 0 {
		t.Fatalf("tick 0: expected no watched-minute credit while incomplete, got %v", got)
	}
	minutes, err := w.store.WindowMinutes([]string{cLogin}, time.Now())
	if err != nil {
		t.Fatalf("failed to read watch-time window: %v", err)
	}
	if got := minutes[cLogin]; got != 0 {
		t.Fatalf("tick 0: expected no store-recorded watch minutes while incomplete, got %v", got)
	}
	if !c.GetIsOnline() {
		t.Fatal("tick 0: a failed convergence attempt must never mark the streamer offline")
	}

	// Force the retry past the backoff window (mirrors forceRotate's idiom)
	// and let the refresher succeed this time.
	st := w.sessionConverge[cLogin]
	if st == nil {
		t.Fatal("expected a staged convergence attempt to be tracked after tick 0")
	}
	st.lastAttempt = time.Now().Add(-10 * time.Minute)
	ref.mu.Lock()
	ref.spadeErr = nil
	ref.mu.Unlock()

	// Tick 1 (a LATER tick): the retried convergence succeeds and publishes
	// spade within this tick, so this same tick's send delivers a real beacon.
	w.processWatching()

	calls = adapter.callsFor(cLogin)
	if len(calls) != 2 || !calls[1].result.Delivered {
		t.Fatalf("tick 1: expected %s to deliver after the retried convergence succeeded, got %+v", cLogin, calls)
	}
	urls, _ := rt.beacons()
	found := false
	for _, u := range urls {
		if u == "http://spade.test/refreshed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a real beacon POSTed to the converged spade URL, got %v", urls)
	}
}

// T5: the committed BrokerSnapshot slots, the set the adapter actually
// Send()s this tick, and the base rotation pair (the residence owners) are
// one and the same, every tick.
func TestSessionConverge_CommittedSlotsMatchActualSends(t *testing.T) {
	w, _, adapter, _, bLogin, cLogin := lateOnlineFixture(t)

	const ticks = 5
	prevLen := 0
	for tick := 0; tick < ticks; tick++ {
		forceRotate(w)
		w.processWatching()

		snap := w.BrokerSnapshot()
		committed := map[string]bool{}
		for _, s := range snap.Slots {
			committed[s.Channel] = true
		}

		all := adapter.allCalls()
		batch := all[prevLen:]
		prevLen = len(all)
		sent := map[string]bool{}
		for _, rec := range batch {
			sent[rec.login] = true
		}

		if !reflect.DeepEqual(committed, sent) {
			t.Fatalf("tick %d: committed BrokerSnapshot slots %v do not match the set actually Send() this tick %v", tick, committed, sent)
		}

		// activePair-derived residence owners: with no boost in play in this
		// fixture, the base rotation pair and the committed set are the same
		// two channels.
		base := map[string]bool{
			w.streamers[w.rotation.activePair[0]].GetUsername(): true,
			w.streamers[w.rotation.activePair[1]].GetUsername(): true,
		}
		if !reflect.DeepEqual(committed, base) {
			t.Fatalf("tick %d: committed slots %v disagree with the base rotation pair %v (residence owners)", tick, committed, base)
		}
		if len(committed) != 2 || !committed[bLogin] || !committed[cLogin] {
			t.Fatalf("tick %d: expected the committed pair {%s,%s}, got %v", tick, cLogin, bLogin, committed)
		}
	}
}

// T6: the "Rotating watch pair" base pair (rotation.activePair) and the
// committed BrokerSnapshot differ ONLY by a DROPS/STREAK boost displacement;
// the committed snapshot is the authoritative delivery truth. This is a
// pre-existing, coherent property of the broker (not a defect), pinned down
// here because the convergence fix's tests lean on "committed == delivers".
func TestSessionConverge_CommittedSnapshotReflectsBoostDisplacement(t *testing.T) {
	a := bringOnlineCoherent("streamera", "cid-a", "broadcast-a", "http://spade.test/a")
	b := bringOnlineCoherent("streamerb", "cid-b", "broadcast-b", "http://spade.test/b")
	c := bringOnlineCoherent("streamerc", "cid-c", "broadcast-c", "http://spade.test/c")
	c.Stream.SetCampaignIDs([]string{"campaign-1"}) // active (unrestricted) drop -> boost-eligible

	w, _, adapter, _ := newDeliveryLifecycleWatcher(t, []*models.Streamer{a, b, c})

	now := time.Now()
	// C is heavily "watched" already (excluded from the base ranking), so the
	// base pair is {A, B}; the boost then pulls C in on top of that ranking
	// without rewriting it.
	if err := w.store.RecordMinutes(a.GetUsername(), 5, now); err != nil {
		t.Fatalf("failed to seed watch time: %v", err)
	}
	if err := w.store.RecordMinutes(b.GetUsername(), 6, now); err != nil {
		t.Fatalf("failed to seed watch time: %v", err)
	}
	if err := w.store.RecordMinutes(c.GetUsername(), 500, now); err != nil {
		t.Fatalf("failed to seed watch time: %v", err)
	}

	forceRotate(w)
	w.processWatching()

	wantBase := map[string]bool{a.GetUsername(): true, b.GetUsername(): true}
	gotBase := map[string]bool{
		w.streamers[w.rotation.activePair[0]].GetUsername(): true,
		w.streamers[w.rotation.activePair[1]].GetUsername(): true,
	}
	if !reflect.DeepEqual(wantBase, gotBase) {
		t.Fatalf("expected the base rotation pair to stay {%s,%s} (a boost must not rewrite it), got %v", a.GetUsername(), b.GetUsername(), gotBase)
	}

	snap := w.BrokerSnapshot()
	committed := map[string]bool{}
	for _, s := range snap.Slots {
		committed[s.Channel] = true
	}
	if !committed[c.GetUsername()] {
		t.Fatalf("expected the active-drop boost target %s to hold a committed slot, got %+v", c.GetUsername(), snap.Slots)
	}
	if len(committed) != 2 {
		t.Fatalf("expected exactly 2 committed slots, got %+v", snap.Slots)
	}
	survivors := 0
	for _, base := range []string{a.GetUsername(), b.GetUsername()} {
		if committed[base] {
			survivors++
		}
	}
	if survivors != 1 {
		t.Fatalf("expected exactly one base-pair member to survive the boost displacement, got %d survivors in %+v", survivors, snap.Slots)
	}

	// All three streamers are fully coherent, so whichever pair actually got
	// committed genuinely delivers — the committed snapshot, not the base
	// pair, is what predicts delivery.
	for login := range committed {
		calls := adapter.callsFor(login)
		if len(calls) != 1 || !calls[0].result.Delivered {
			t.Errorf("expected the committed occupant %s to have delivered, got %+v", login, calls)
		}
	}
}

// T7: a broadcast change landing WHILE a convergence refresh's I/O is in
// flight is rejected by the existing ExpectedBroadcastID fence
// (requestStale / ApplyPlaybackSessionIfCurrent) as stale — it can never
// clobber the newer residence, and no beacon is fabricated from the rejected
// result.
func TestSessionConverge_StaleOutgoingResultDoesNotClobber(t *testing.T) {
	b := bringOnlineCoherent("streamerb", "cid-b", "broadcast-b", "http://spade.test/b")
	c := bringOnlineViaStreamUp("streamerc", "cid-c", "broadcast-c")

	w, rt, adapter, _ := newDeliveryLifecycleWatcher(t, []*models.Streamer{b, c})

	raced := false
	ref := &fakeRefresher{
		beforeApply: func(s *models.Streamer) {
			if s.GetUsername() != c.GetUsername() || raced {
				return
			}
			raced = true
			// A concurrent broadcast change lands WHILE the convergence
			// refresh's I/O is in flight — exactly the race
			// ApplyPlaybackSessionIfCurrent's ExpectedBroadcastID fence exists
			// to reject.
			s.Stream.Update("broadcast-c-NEW", "t", nil, nil, 4)
		},
	}
	w.refresher = ref

	w.processWatching()

	if !raced {
		t.Fatal("expected the beforeApply hook to have fired for the incomplete streamer's convergence refresh")
	}

	out, ok := w.LastSessionRefresh(c.GetUsername())
	if !ok || out.Success || !out.Stale || out.Reason != RefreshReasonBroadcastMoved {
		t.Fatalf("expected the refresh to be rejected as stale (broadcast_changed), got ok=%v %+v", ok, out)
	}

	// No clobber: the spade URL was never published over the newer broadcast.
	if got := c.Stream.GetSpadeURL(); got != "" {
		t.Fatalf("expected the stale refresh to publish NOTHING (spade URL must stay empty), got %q", got)
	}
	// The legitimate concurrent broadcast change DID land independently of
	// the refresh's own (rejected) atomic apply.
	if got := c.Stream.GetBroadcastID(); got != "broadcast-c-NEW" {
		t.Fatalf("expected the concurrent broadcast change to have landed, got %q", got)
	}
	urls, _ := rt.beacons()
	for _, u := range urls {
		if u == "http://spade.test/refreshed" {
			t.Fatalf("the stale refresh must never post a beacon to the converged spade URL, got %v", urls)
		}
	}

	// C's send this tick still fails (no spade published) — the stale
	// rejection did not fabricate a delivery — while B is unaffected.
	cCalls := adapter.callsFor(c.GetUsername())
	if len(cCalls) != 1 || cCalls[0].result.Delivered {
		t.Fatalf("expected C's send to still fail (no spade published), got %+v", cCalls)
	}
	bCalls := adapter.callsFor(b.GetUsername())
	if len(bCalls) != 1 || !bCalls[0].result.Delivered {
		t.Fatalf("expected B to deliver independently, got %+v", bCalls)
	}
}

// T8: a rapid-replacement sequence A+B -> C+B -> D+B (via seeded weights, the
// real rotation path). C is displaced before its own convergence attempt
// (deliberately failing) ever succeeds; D's independent convergence succeeds
// once it takes the seat. The final delivery owner is D, never A or C.
func TestSessionConverge_RapidReplacementFinalOwnerConverges(t *testing.T) {
	a := bringOnlineCoherent("streamera", "cid-a", "broadcast-a", "http://spade.test/a")
	b := bringOnlineCoherent("streamerb", "cid-b", "broadcast-b", "http://spade.test/b")
	c := bringOnlineViaStreamUp("streamerc", "cid-c", "broadcast-c")
	d := bringOnlineViaStreamUp("streamerd", "cid-d", "broadcast-d")

	w, _, adapter, _ := newDeliveryLifecycleWatcher(t, []*models.Streamer{a, b, c, d})
	ref := &perLoginFailRefresher{inner: &fakeRefresher{}, failLogins: map[string]bool{c.GetUsername(): true}}
	w.refresher = ref

	seed := func(login string, minutes float64) {
		t.Helper()
		if err := w.store.RecordMinutes(login, minutes, time.Now()); err != nil {
			t.Fatalf("failed to seed watch time for %s: %v", login, err)
		}
	}

	// Tick 1: A, B are least-watched -> {A, B}; both are coherent and deliver.
	seed(a.GetUsername(), 10)
	seed(b.GetUsername(), 20)
	seed(c.GetUsername(), 3000)
	seed(d.GetUsername(), 6000)
	forceRotate(w)
	w.processWatching()
	requireCommittedPair(t, w, 1, a.GetUsername(), b.GetUsername())

	// Tick 2: push A above C -> {C, B}. C's convergence attempt is staged and
	// FAILS (perLoginFailRefresher), so C never delivers this tick.
	seed(a.GetUsername(), 6000)
	forceRotate(w)
	w.processWatching()
	requireCommittedPair(t, w, 2, c.GetUsername(), b.GetUsername())

	// Tick 3: push C above D -> {D, B}. C is displaced BEFORE its convergence
	// ever succeeded; D's own (independent) convergence now stages and,
	// unlike C's, SUCCEEDS.
	seed(c.GetUsername(), 6000)
	forceRotate(w)
	w.processWatching()
	requireCommittedPair(t, w, 3, d.GetUsername(), b.GetUsername())

	for i, rec := range adapter.callsFor(c.GetUsername()) {
		if rec.result.Delivered {
			t.Errorf("call %d: expected %s to never deliver (displaced before its convergence could succeed), got %+v", i, c.GetUsername(), rec.result)
		}
	}
	dDelivered := 0
	for _, rec := range adapter.callsFor(d.GetUsername()) {
		if rec.result.Delivered {
			dDelivered++
		}
	}
	if dDelivered == 0 {
		t.Fatalf("expected the final owner %s to deliver at least one real beacon, got none: %+v", d.GetUsername(), adapter.callsFor(d.GetUsername()))
	}

	// Guard 5: C's convergence ownership was invalidated the instant it left
	// the slot — no residual bookkeeping survives for a login no longer slotted.
	if st, tracked := w.sessionConverge[c.GetUsername()]; tracked {
		t.Errorf("expected %s's convergence state to be pruned after it left the slot, but it is still tracked: %+v", c.GetUsername(), st)
	}

	bDelivered := 0
	for _, rec := range adapter.callsFor(b.GetUsername()) {
		if rec.result.Delivered {
			bDelivered++
		}
	}
	if bDelivered != 3 {
		t.Errorf("expected the retained companion %s to deliver on all 3 ticks, got %d", b.GetUsername(), bDelivered)
	}
}

// T9: the delivering send uses ONE coherent (broadcast, generation, spade,
// payload) tuple — the post-convergence snapshot, never a mix of pre- and
// post-convergence state.
func TestSessionConverge_DeliveringSendUsesCoherentPostConvergenceTuple(t *testing.T) {
	b := bringOnlineCoherent("streamerb", "cid-b", "broadcast-b", "http://spade.test/b")
	c := bringOnlineViaStreamUp("streamerc", "cid-c", "broadcast-c")

	w, rt, adapter, _ := newDeliveryLifecycleWatcher(t, []*models.Streamer{b, c})
	w.refresher = &fakeRefresher{}

	preGen := c.Stream.SessionGeneration()

	w.processWatching()

	calls := adapter.callsFor(c.GetUsername())
	if len(calls) != 1 || !calls[0].result.Delivered {
		t.Fatalf("expected %s to converge and deliver within this tick, got %+v", c.GetUsername(), calls)
	}
	rec := calls[0]
	if !rec.hadSpade {
		t.Fatal("expected the delivering send's captured snapshot to already carry a spade URL (post-convergence)")
	}
	if rec.generation <= preGen {
		t.Fatalf("expected the delivering send's session generation (%d) to strictly exceed the pre-convergence generation (%d)", rec.generation, preGen)
	}
	if rec.result.Generation != rec.generation {
		t.Fatalf("expected the delivered result's generation to equal the captured snapshot generation (one coherent tuple), got result=%d snapshot=%d", rec.result.Generation, rec.generation)
	}

	urls, _ := rt.beacons()
	n := 0
	for _, u := range urls {
		if u == "http://spade.test/refreshed" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one real beacon POSTed to the converged spade URL, got %d (all beacons: %v)", n, urls)
	}
}

// T10: a persistent, typed convergence failure for one slot never blocks the
// companion slot's delivery.
func TestSessionConverge_PersistentFailureIsolatesOnlyThatSlot(t *testing.T) {
	b := bringOnlineCoherent("streamerb", "cid-b", "broadcast-b", "http://spade.test/b")
	c := bringOnlineViaStreamUp("streamerc", "cid-c", "broadcast-c")

	w, _, adapter, _ := newDeliveryLifecycleWatcher(t, []*models.Streamer{b, c})
	w.refresher = &fakeRefresher{spadeErr: errors.New("persistent spade failure")}

	const ticks = 4
	for i := 0; i < ticks; i++ {
		w.processWatching()
	}

	cDelivered := 0
	for _, rec := range adapter.callsFor(c.GetUsername()) {
		if rec.result.Delivered {
			cDelivered++
		}
		if rec.result.Failure == nil || rec.result.Failure.Stage != StageSessionSnapshot || rec.result.Failure.ErrorCode != "no_spade_url" {
			t.Errorf("expected every %s send to fail as session_snapshot/no_spade_url, got %+v", c.GetUsername(), rec.result)
		}
	}
	if cDelivered != 0 {
		t.Fatalf("expected %s to never deliver under a persistent spade failure, got %d delivered sends", c.GetUsername(), cDelivered)
	}

	bDelivered := 0
	for _, rec := range adapter.callsFor(b.GetUsername()) {
		if rec.result.Delivered {
			bDelivered++
		}
	}
	if bDelivered != ticks {
		t.Fatalf("expected %s to deliver on all %d ticks independent of %s's persistent failure, got %d", b.GetUsername(), ticks, c.GetUsername(), bDelivered)
	}
}

// T11: exactly one Send per residence per tick — no login is ever sent twice
// in the same tick.
func TestSessionConverge_NoDuplicateOwnerPerTick(t *testing.T) {
	w, _, adapter, _, _, _ := lateOnlineFixture(t)

	const ticks = 6
	prevLen := 0
	for tick := 0; tick < ticks; tick++ {
		forceRotate(w)
		w.processWatching()
		all := adapter.allCalls()
		batch := all[prevLen:]
		prevLen = len(all)

		seen := map[string]int{}
		for _, rec := range batch {
			seen[rec.login]++
		}
		for login, n := range seen {
			if n != 1 {
				t.Errorf("tick %d: expected %s sent exactly once, got %d sends this tick", tick, login, n)
			}
		}
	}
}

// T12: never more than constants.MaxSimultaneousStreams sends/residences per
// tick, with the convergence path active.
func TestSessionConverge_NeverExceedsSlotCap(t *testing.T) {
	w, _, adapter, _, _, _ := lateOnlineFixture(t)

	const ticks = 6
	prevLen := 0
	for tick := 0; tick < ticks; tick++ {
		forceRotate(w)
		w.processWatching()
		all := adapter.allCalls()
		batch := all[prevLen:]
		prevLen = len(all)

		if len(batch) > constants.MaxSimultaneousStreams {
			t.Fatalf("tick %d: expected at most %d sends, got %d: %+v", tick, constants.MaxSimultaneousStreams, len(batch), batch)
		}
	}
}

// T13: with a slot journal injected, selection/assignment (entered),
// delivery-attempt failure (delivery_failure), and delivery success
// (delivery_success) stay distinguishable, and the convergence mechanism
// itself never emits a false delivery_success for a channel whose send is
// still failing.
func TestSessionConverge_JournalNeverFabricatesDeliverySuccess(t *testing.T) {
	b := bringOnlineCoherent("streamerb", "cid-b", "broadcast-b", "http://spade.test/b")
	c := bringOnlineViaStreamUp("streamerc", "cid-c", "broadcast-c")

	w, _, _, _ := newDeliveryLifecycleWatcher(t, []*models.Streamer{b, c})
	w.refresher = &fakeRefresher{spadeErr: errors.New("still incomplete")}
	w.SetSlotJournal(journal.New[journal.SlotEvent](64, time.Now))

	w.processWatching()

	evs := slotEvents(w)

	entered := onlyOfType(evs, journal.SlotEntered)
	enteredLogins := map[string]bool{}
	for _, e := range entered {
		enteredLogins[e.Channel] = true
	}
	if !enteredLogins[b.GetUsername()] || !enteredLogins[c.GetUsername()] {
		t.Fatalf("expected both channels to journal a SlotEntered event, got %+v", entered)
	}

	failures := onlyOfType(evs, journal.SlotDeliveryFailure)
	cFailures := 0
	for _, f := range failures {
		if f.Channel == c.GetUsername() {
			cFailures++
			if f.Stage != string(StageSessionSnapshot) || f.ErrorCode != "no_spade_url" {
				t.Errorf("expected %s's delivery failure to be session_snapshot/no_spade_url, got %+v", c.GetUsername(), f)
			}
		}
	}
	if cFailures == 0 {
		t.Fatalf("expected at least one journaled delivery failure for %s", c.GetUsername())
	}

	successes := onlyOfType(evs, journal.SlotDeliverySuccess)
	for _, s := range successes {
		if s.Channel == c.GetUsername() {
			t.Fatalf("the background convergence attempt must never be journaled as a delivery success for %s, got %+v", c.GetUsername(), s)
		}
	}
	bHasSuccess := false
	for _, s := range successes {
		if s.Channel == b.GetUsername() {
			bHasSuccess = true
		}
	}
	if !bHasSuccess {
		t.Fatalf("expected a journaled delivery success for %s, got %+v", b.GetUsername(), successes)
	}
}

// T14: the published BrokerSnapshot and the published debug snapshot always
// agree on which channels are watched — neither ever reports a
// pair/residence combination the other did not also commit to.
func TestSessionConverge_BrokerAndDebugSnapshotsAgree(t *testing.T) {
	w, _, _, _, bLogin, cLogin := lateOnlineFixture(t)

	const ticks = 6
	for tick := 0; tick < ticks; tick++ {
		forceRotate(w)
		w.processWatching()

		snap := w.BrokerSnapshot()
		committed := map[string]bool{}
		for _, s := range snap.Slots {
			committed[s.Channel] = true
		}

		dbg := w.GetDebugState()
		watching := map[string]bool{}
		for _, d := range dbg.Decisions {
			if d.Watching {
				watching[d.Username] = true
			}
		}

		if !reflect.DeepEqual(committed, watching) {
			t.Fatalf("tick %d: BrokerSnapshot committed set %v disagrees with GetDebugState's watching set %v", tick, committed, watching)
		}
		if len(committed) != 2 || !committed[bLogin] || !committed[cLogin] {
			t.Fatalf("tick %d: expected the committed pair {%s,%s}, got %v", tick, cLogin, bLogin, committed)
		}
	}
}

// T15: concurrent session publication + rotation-weight perturbation +
// delivery + external RequestSessionRefresh staging + snapshot reads, run
// under -race. The convergence path (loop-goroutine-only) and the broker's
// public read/stage surface (any goroutine) must stay race-free together.
func TestSessionConverge_RaceSafety(t *testing.T) {
	a := bringOnlineCoherent("streamera", "cid-a", "broadcast-a", "http://spade.test/a")
	b := bringOnlineCoherent("streamerb", "cid-b", "broadcast-b", "http://spade.test/b")
	c := bringOnlineViaStreamUp("streamerc", "cid-c", "broadcast-c")

	w, _, adapter, _ := newDeliveryLifecycleWatcher(t, []*models.Streamer{a, b, c})
	w.refresher = &fakeRefresher{}

	now := time.Now()
	if err := w.store.RecordMinutes(a.GetUsername(), 10, now); err != nil {
		t.Fatalf("failed to seed watch time: %v", err)
	}
	if err := w.store.RecordMinutes(b.GetUsername(), 20, now); err != nil {
		t.Fatalf("failed to seed watch time: %v", err)
	}

	var stop atomic.Bool
	var wdDone sync.WaitGroup
	wdDone.Add(1)
	go func() {
		defer wdDone.Done()
		for !stop.Load() {
			_ = w.BrokerSnapshot()
			_ = w.GetDebugState()
			_, _ = w.ReportStats(c.GetUsername())
			_ = w.IsWatching(b.GetUsername())
			_ = c.Stream.GetSpadeURL()
			_ = c.Stream.GetBroadcastID()
			// An external caller staging the SAME channel's refresh
			// concurrently with the loop goroutine's own convergence staging —
			// the exact new two-caller overlap the fix introduces.
			w.RequestSessionRefresh(SessionRefreshRequest{Login: c.GetUsername(), Mode: RefreshStreamInfo})
			// Perturb rotation ranking concurrently with the loop's ticks.
			_ = w.store.RecordMinutes(a.GetUsername(), 1, time.Now())
			time.Sleep(time.Millisecond)
		}
	}()

	for tick := 0; tick < 6; tick++ {
		forceRotate(w)
		w.processWatching()
	}

	stop.Store(true)
	wdDone.Wait()

	if len(adapter.allCalls()) == 0 {
		t.Fatal("expected at least one send during the race window")
	}
}

// T16: given identical eligible state, the convergence fix leaves selection
// byte-identical — it neither reads nor writes the watch-time store, the
// rotation state, or the selection output.
func TestSessionConverge_DoesNotAlterSelection(t *testing.T) {
	w, _, _, aLogin, bLogin, cLogin := lateOnlineFixture(t)
	online := []int{0, 1, 2} // a, b, c in lateOnlineFixture's construction order

	forceRotate(w)
	pairBefore := append([]int(nil), w.selectStreamersToWatch(online)...)

	usernames := []string{aLogin, bLogin, cLogin}
	now := time.Now()
	weightsBefore, err := w.store.WindowMinutes(usernames, now)
	if err != nil {
		t.Fatalf("failed to read watch-time window: %v", err)
	}

	slots, _ := w.arbitrate(pairBefore, nil, now)
	w.convergeIncompleteSlotSessions(slots, now)

	weightsAfter, err := w.store.WindowMinutes(usernames, time.Now())
	if err != nil {
		t.Fatalf("failed to read watch-time window: %v", err)
	}
	if !reflect.DeepEqual(weightsBefore, weightsAfter) {
		t.Fatalf("convergence must never touch the watch-time store, got before=%v after=%v", weightsBefore, weightsAfter)
	}

	forceRotate(w)
	pairAfter := append([]int(nil), w.selectStreamersToWatch(online)...)

	if !reflect.DeepEqual(pairBefore, pairAfter) {
		t.Fatalf("expected the fix to leave selection byte-identical for the same eligible state, got before=%v after=%v", pairBefore, pairAfter)
	}
}

// --- Guard 9: deterministic request-delta proofs ---

// Guard 9(a): repeated minute ticks inside the backoff window create NO
// additional refresh requests — a persistently failing spade fetch is staged
// exactly once across many ticks with no aging.
func TestSessionConverge_Guard9DedupWithinBackoffWindow(t *testing.T) {
	b := bringOnlineCoherent("streamerb", "cid-b", "broadcast-b", "http://spade.test/b")
	c := bringOnlineViaStreamUp("streamerc", "cid-c", "broadcast-c")

	w, _, _, _ := newDeliveryLifecycleWatcher(t, []*models.Streamer{b, c})
	ref := &fakeRefresher{spadeErr: errors.New("spade unavailable")}
	w.refresher = ref

	const ticks = 10
	for i := 0; i < ticks; i++ {
		w.processWatching() // no aging of lastAttempt between ticks
	}

	spadeCalls, _ := ref.calls()
	if len(spadeCalls) != 1 {
		t.Fatalf("expected exactly 1 spade-fetch call across %d ticks inside the backoff window, got %d: %v", ticks, len(spadeCalls), spadeCalls)
	}
}

// Guard 9(b): a bounded retry fires a 2nd and 3rd attempt once the backoff is
// aged past, then NO 4th even with further aging (the per-broadcast cap) —
// until the broadcast identity genuinely changes, which resets the budget.
func TestSessionConverge_Guard9BoundedRetryThenCapThenResetOnNewBroadcast(t *testing.T) {
	b := bringOnlineCoherent("streamerb", "cid-b", "broadcast-b", "http://spade.test/b")
	c := bringOnlineViaStreamUp("streamerc", "cid-c", "broadcast-c")

	w, _, _, _ := newDeliveryLifecycleWatcher(t, []*models.Streamer{b, c})
	ref := &fakeRefresher{spadeErr: errors.New("spade unavailable")}
	w.refresher = ref

	cLogin := c.GetUsername()
	age := func() {
		t.Helper()
		st := w.sessionConverge[cLogin]
		if st == nil {
			t.Fatalf("expected convergence state for %s to exist before aging it", cLogin)
		}
		st.lastAttempt = time.Now().Add(-10 * time.Minute)
	}

	// Attempt 1.
	w.processWatching()
	if spade, _ := ref.calls(); len(spade) != 1 {
		t.Fatalf("expected exactly 1 spade-fetch call after the first tick, got %d", len(spade))
	}

	// Attempt 2 (forced past the backoff window).
	age()
	w.processWatching()
	if spade, _ := ref.calls(); len(spade) != 2 {
		t.Fatalf("expected exactly 2 spade-fetch calls after the second attempt, got %d", len(spade))
	}

	// Attempt 3 (forced past the backoff window again) — hits the cap.
	age()
	w.processWatching()
	if spade, _ := ref.calls(); len(spade) != 3 {
		t.Fatalf("expected exactly 3 spade-fetch calls after the third attempt, got %d", len(spade))
	}
	if got := w.sessionConverge[cLogin].attempts; got != sessionConvergeMaxAttempts {
		t.Fatalf("expected attempts to have reached the cap (%d), got %d", sessionConvergeMaxAttempts, got)
	}

	// A 4th tick, even with the backoff aged out of the way, must NOT stage a
	// 4th attempt: the cap is per-broadcast-identity, not time-bounded.
	age()
	w.processWatching()
	if spade, _ := ref.calls(); len(spade) != 3 {
		t.Fatalf("expected NO 4th spade-fetch call once the attempt cap is reached, got %d", len(spade))
	}

	// A genuinely new broadcast resets the budget from scratch.
	c.Stream.Update("broadcast-c-2", "t", nil, nil, 1)
	w.processWatching()
	if spade, _ := ref.calls(); len(spade) != 4 {
		t.Fatalf("expected a 4th spade-fetch call (attempt 1 of the NEW broadcast identity) after the broadcast changed, got %d", len(spade))
	}
	if got := w.sessionConverge[cLogin].attempts; got != 1 {
		t.Fatalf("expected the new broadcast identity's attempt count to start at 1, got %d", got)
	}
}

// Guard 9(c): once the spade-bearing session is published, further staging
// stops automatically — exactly 1 spade-fetch call total, then 0 further
// across many more ticks, and the tracked state is cleared.
func TestSessionConverge_Guard9StopsStagingOnceSpadeConverges(t *testing.T) {
	b := bringOnlineCoherent("streamerb", "cid-b", "broadcast-b", "http://spade.test/b")
	c := bringOnlineViaStreamUp("streamerc", "cid-c", "broadcast-c")

	w, _, adapter, _ := newDeliveryLifecycleWatcher(t, []*models.Streamer{b, c})
	ref := &fakeRefresher{} // succeeds immediately
	w.refresher = ref

	w.processWatching() // converges and delivers within this very tick
	if spade, _ := ref.calls(); len(spade) != 1 {
		t.Fatalf("expected exactly 1 spade-fetch call on convergence, got %d", len(spade))
	}
	if calls := adapter.callsFor(c.GetUsername()); len(calls) != 1 || !calls[0].result.Delivered {
		t.Fatalf("expected %s to deliver on the converging tick, got %+v", c.GetUsername(), calls)
	}

	const moreTicks = 9
	for i := 0; i < moreTicks; i++ {
		w.processWatching()
	}
	if spade, _ := ref.calls(); len(spade) != 1 {
		t.Fatalf("expected NO further spade-fetch calls once spade is present, got %d total across %d further ticks", len(spade), moreTicks)
	}
	if st, tracked := w.sessionConverge[c.GetUsername()]; tracked {
		t.Errorf("expected %s's convergence state to be cleared once its session is complete, but it is still tracked: %+v", c.GetUsername(), st)
	}

	delivered := 0
	for _, rec := range adapter.callsFor(c.GetUsername()) {
		if rec.result.Delivered {
			delivered++
		}
	}
	if want := 1 + moreTicks; delivered != want {
		t.Fatalf("expected %s to keep delivering every subsequent tick off its converged session, got %d/%d delivered", c.GetUsername(), delivered, want)
	}
}

// Guard 9(d): a slot release (rotation) invalidates the tracked convergence
// state immediately, and when the same login later returns to a slot, its
// bookkeeping starts fresh — no leaked attempt count or backoff timer from
// the evicted residence.
func TestSessionConverge_Guard9ReleaseInvalidatesTrackedState(t *testing.T) {
	w, _, _, aLogin, bLogin, cLogin := lateOnlineFixture(t)
	ref := &fakeRefresher{spadeErr: errors.New("spade unavailable")}
	w.refresher = ref

	forceRotate(w)
	w.processWatching()
	requireCommittedPair(t, w, 0, cLogin, bLogin)
	if st := w.sessionConverge[cLogin]; st == nil || st.attempts != 1 {
		t.Fatalf("expected %s to have one staged (failed) convergence attempt while slotted, got %+v", cLogin, st)
	}

	// Evict C: push its accumulated weight far above A's so A re-enters.
	if err := w.store.RecordMinutes(cLogin, 100000, time.Now()); err != nil {
		t.Fatalf("failed to reseed watch time: %v", err)
	}
	forceRotate(w)
	w.processWatching()
	requireCommittedPair(t, w, 1, aLogin, bLogin)

	if st, tracked := w.sessionConverge[cLogin]; tracked {
		t.Fatalf("expected %s's convergence state to be pruned the instant it left the slot, but it is still tracked: %+v", cLogin, st)
	}

	// C returns to a slot later (a fresh residence). Its convergence
	// bookkeeping must start from scratch.
	if err := w.store.RecordMinutes(aLogin, 100000, time.Now()); err != nil {
		t.Fatalf("failed to reseed watch time: %v", err)
	}
	forceRotate(w)
	w.processWatching()
	requireCommittedPair(t, w, 2, cLogin, bLogin)

	st := w.sessionConverge[cLogin]
	if st == nil || st.attempts != 1 {
		t.Fatalf("expected %s's new residence to start its attempt budget at 1 (no leakage from the evicted residence), got %+v", cLogin, st)
	}
}
