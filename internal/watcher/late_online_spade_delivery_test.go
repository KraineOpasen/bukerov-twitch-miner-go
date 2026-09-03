package watcher

import (
	"context"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// This file reproduces, with unmodified production code, a defect in the
// late-online delivery path: a streamer that first becomes authoritatively
// online via PubSub's "stream-up" event (internal/pubsub/pool.go's
// handleVideoPlayback, case "stream-up") is marked online by
// Streamer.SetConfirmedOnline() and then given only a best-effort metadata
// refresh (the checker's UpdateStream call) — never a spade URL. A spade URL
// is fetched ONLY by TwitchClient.ConfirmOnline (internal/api/client.go), and
// CheckStreamerOnline only ever calls ConfirmOnline when the streamer is NOT
// already StatusOnline (client.go's CheckStreamerOnline, the
// `if status != models.StatusOnline` branch); once already online it takes
// the metadata-only UpdateStream branch forever after, which never touches
// the spade URL. So a streamer that entered via stream-up converges its
// broadcast ID (via periodic metadata refreshes) but never acquires a spade
// URL — and MinuteSender.Send (internal/watcher/sender.go) refuses to send
// without one (StageSessionSnapshot / "no_spade_url"), silently and
// permanently, even while the streamer legitimately holds a watch slot.
//
// The three tests below exercise the REAL delivery seam (a genuine
// *MinuteSender over a recording HTTP transport, invoked through the real
// watch-slot broker's processWatching -> arbitrate -> selectRotating pipeline)
// so the reproduction is faithful rather than a fabricated failure.

// bringOnlineViaStreamUp models a streamer brought online exactly the way
// PubSub's "stream-up" handler does: SetConfirmedOnline() first (broadcast and
// spade unknown), then the same best-effort metadata convergence
// UpdateStream/doRefreshPlaybackSession performs (broadcast ID + beacon
// payload) — but, faithfully to that code path, no spade URL is ever
// published. This is the "late-online-no-spade" fixture at the center of the
// reproduction.
func bringOnlineViaStreamUp(login, channelID, broadcastID string) *models.Streamer {
	s := models.NewStreamer(login, models.DefaultStreamerSettings())
	s.ChannelID = channelID
	// Isolate fair rotation from the independent DROPS/STREAK boost seat so the
	// pair the test asserts on is decided purely by accumulated watch time.
	s.Settings.WatchStreak = false
	s.SetConfirmedOnline()
	s.OnlineAt = time.Now().Add(-time.Minute)
	s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
	// The metadata-only convergence a production UpdateStream call performs:
	// broadcast ID and beacon payload, deliberately WITHOUT SetSpadeURL.
	s.Stream.Update(broadcastID, "t", nil, nil, 3)
	mustSetPayload(s.Stream, channelID, broadcastID, "44322889", login, nil, nil)
	return s
}

// bringOnlineCoherent models a streamer whose watch session is fully
// converged — broadcast, spade URL, and payload all present, as ConfirmOnline
// (or an earlier settled tick) would leave it. This is the control fixture:
// it must always be able to deliver.
func bringOnlineCoherent(login, channelID, broadcastID, spadeURL string) *models.Streamer {
	s := models.NewStreamer(login, models.DefaultStreamerSettings())
	s.ChannelID = channelID
	s.Settings.WatchStreak = false
	s.SetConfirmedOnline()
	s.OnlineAt = time.Now().Add(-time.Minute)
	s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
	s.Stream.Update(broadcastID, "t", nil, nil, 3)
	s.Stream.SetSpadeURL(spadeURL)
	mustSetPayload(s.Stream, channelID, broadcastID, "44322889", login, nil, nil)
	return s
}

// sendRecord captures one adapter-observed Send invocation: the channel, the
// session identity at capture time (the real coherence gate MinuteSender.Send
// itself reads via Stream.SessionSnapshot), and the outcome the real
// MinuteSender produced.
type sendRecord struct {
	login      string
	generation uint64
	hadSpade   bool
	result     SendResult
}

// recordingSenderAdapter wraps a REAL *MinuteSender — so the genuine
// HasSpadeURL/HasPayload gate and the real beacon path are exercised, never a
// fake that fabricates success — and records every invocation for assertions
// without altering the delegated result.
type recordingSenderAdapter struct {
	mu    sync.Mutex
	real  *MinuteSender
	calls []sendRecord
}

func (a *recordingSenderAdapter) Send(ctx context.Context, streamer *models.Streamer) SendResult {
	session := streamer.Stream.SessionSnapshot()
	res := a.real.Send(ctx, streamer)
	a.mu.Lock()
	a.calls = append(a.calls, sendRecord{
		login:      streamer.GetUsername(),
		generation: session.Generation,
		hadSpade:   session.HasSpadeURL(),
		result:     res,
	})
	a.mu.Unlock()
	return res
}

func (a *recordingSenderAdapter) allCalls() []sendRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]sendRecord(nil), a.calls...)
}

func (a *recordingSenderAdapter) callsFor(login string) []sendRecord {
	var out []sendRecord
	for _, c := range a.allCalls() {
		if c.login == login {
			out = append(out, c)
		}
	}
	return out
}

// noSpadeChecker models production's CheckStreamerOnline for a streamer that
// is ALREADY StatusOnline (internal/api/client.go's CheckStreamerOnline,
// `if status != models.StatusOnline` branch is false): it takes the
// metadata-only UpdateStream/doRefreshPlaybackSession(playbackRefreshIntent{})
// path, which can refresh broadcast/title/viewer metadata but never calls
// Stream.SetSpadeURL — spade is only ever fetched by ConfirmOnline
// (fetchSpade:true), which CheckStreamerOnline reaches ONLY when NOT already
// online. Wiring this as w.client lets the watcher's real failure-recovery
// call (processWatching's `w.client.CheckStreamerOnline(streamer)` after a
// failed send) run unmodified instead of being stubbed into a no-op, so the
// "recovery never self-repairs" half of the defect is exercised faithfully
// too.
type noSpadeChecker struct {
	mu    sync.Mutex
	calls map[string]int
}

func (c *noSpadeChecker) CheckStreamerOnlineContext(_ context.Context, s *models.Streamer) models.StatusTransition {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = make(map[string]int)
	}
	c.calls[s.GetUsername()]++
	c.mu.Unlock()
	// Metadata-only refresh: re-affirms the current broadcast/title/viewers,
	// exactly mirroring UpdateStream's contract. Deliberately no SetSpadeURL.
	s.Stream.Update(s.Stream.GetBroadcastID(), s.Stream.GetTitle(), nil, nil, s.Stream.GetViewersCount())
	return models.StatusTransition{Previous: models.StatusOnline, Current: models.StatusOnline}
}

// newDeliveryLifecycleWatcher builds a MinuteWatcher wired with the real
// delivery seam (a genuine *MinuteSender over a recording HTTP transport,
// reached only through recordingSenderAdapter's instrumentation) and a real
// temporary WatchTimeStore, so fair rotation and the sender's coherence gate
// both run unmodified production code — no hand-placed slots, no fabricated
// beacon.
func newDeliveryLifecycleWatcher(t *testing.T, streamers []*models.Streamer) (w *MinuteWatcher, rt *recordingRT, adapter *recordingSenderAdapter, checker *noSpadeChecker) {
	t.Helper()

	rt = &recordingRT{}
	real := &MinuteSender{client: fakeToken{sig: "s", token: "t"}, httpClient: &http.Client{Transport: rt}}
	adapter = &recordingSenderAdapter{real: real}
	checker = &noSpadeChecker{}

	store, sqlDB := openWatchTimeStore(t, filepath.Join(t.TempDir(), "watch.db"))
	t.Cleanup(func() { _ = sqlDB.Close() })

	w = &MinuteWatcher{
		client:     checker,
		streamers:  streamers,
		priorities: []config.Priority{config.PriorityOrder},
		settings: config.RateLimitSettings{
			MinuteWatchedInterval: 1,
		},
		store:  store,
		sender: adapter,
		// Present so the harness matches a fix's expected wiring; unmodified
		// production never reaches the refresher for this failure (the failed
		// send's recovery call is CheckStreamerOnline, not a staged refresh).
		refresher: &fakeRefresher{},
		pacer:     func(time.Duration) bool { return true },
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.ctx, w.cancel = ctx, cancel
	t.Cleanup(cancel)

	return w, rt, adapter, checker
}

// requireCommittedPair asserts the broker's published BrokerSnapshot holds
// exactly the given logins this tick — i.e. that selectRotating's chosen pair,
// arbitrate's committed slots, and the published snapshot (the residence
// owners discovery/the dashboard/this test all read) are one and the same.
func requireCommittedPair(t *testing.T, w *MinuteWatcher, tick int, want ...string) {
	t.Helper()
	snap := w.BrokerSnapshot()
	got := make(map[string]bool, len(snap.Slots))
	for _, s := range snap.Slots {
		got[s.Channel] = true
	}
	for _, login := range want {
		if !got[login] {
			t.Fatalf("tick %d: expected the committed watch-slot set to include %q (selected pair == published BrokerSnapshot == residence owner), got %+v",
				tick, login, snap.Slots)
		}
	}
	if len(snap.Slots) != len(want) {
		t.Fatalf("tick %d: expected exactly the committed pair %v, got %+v", tick, want, snap.Slots)
	}
}

// T1 (control): two fully coherent, freshly-online streamers must both
// deliver a real beacon on the very first tick. This must pass on unmodified
// main — it pins down that the harness itself (real sender, real transport,
// real broker) reproduces ordinary successful delivery before T2 shows the
// late-online-no-spade gap.
func TestMinuteWatchedDelivery_CoherentPairBothDeliver(t *testing.T) {
	a := bringOnlineCoherent("streamera", "cid-a", "broadcast-a", "http://spade.test/a")
	b := bringOnlineCoherent("streamerb", "cid-b", "broadcast-b", "http://spade.test/b")

	w, rt, adapter, _ := newDeliveryLifecycleWatcher(t, []*models.Streamer{a, b})

	w.processWatching(tickCtx(w))

	requireCommittedPair(t, w, 0, a.GetUsername(), b.GetUsername())

	urls, _ := rt.beacons()
	countURL := func(u string) int {
		n := 0
		for _, got := range urls {
			if got == u {
				n++
			}
		}
		return n
	}
	if n := countURL("http://spade.test/a"); n != 1 {
		t.Errorf("expected exactly one real beacon POSTed to streamer a's spade URL, got %d (all beacons: %v)", n, urls)
	}
	if n := countURL("http://spade.test/b"); n != 1 {
		t.Errorf("expected exactly one real beacon POSTed to streamer b's spade URL, got %d (all beacons: %v)", n, urls)
	}

	for _, login := range []string{a.GetUsername(), b.GetUsername()} {
		calls := adapter.callsFor(login)
		if len(calls) != 1 || !calls[0].result.Delivered {
			t.Errorf("expected the adapter to record exactly one Delivered send for %s, got %+v", login, calls)
		}
	}
}

// lateOnlineFixture builds the shared three-streamer scenario for both the
// baseline reproduction (T2) and the retained-companion-independence check
// (T3): A and B are fully coherent; C is brought online through the SAME
// sequence production's PubSub stream-up handler uses (bringOnlineViaStreamUp)
// — broadcast and payload converge, spade URL never does. Watch time is
// seeded so fair rotation's real ranking (ascending accumulated minutes)
// deterministically selects {C, B} — least-watched first — evicting A, so the
// pair is reached through the genuine selectRotating/fairness reconciliation
// path rather than hand-placed.
func lateOnlineFixture(t *testing.T) (w *MinuteWatcher, rt *recordingRT, adapter *recordingSenderAdapter, aLogin, bLogin, cLogin string) {
	t.Helper()

	a := bringOnlineCoherent("streamera", "cid-a", "broadcast-a", "http://spade.test/a")
	b := bringOnlineCoherent("streamerb", "cid-b", "broadcast-b", "http://spade.test/b")
	c := bringOnlineViaStreamUp("streamerc", "cid-c", "broadcast-c")

	w, rt, adapter, _ = newDeliveryLifecycleWatcher(t, []*models.Streamer{a, b, c})

	now := time.Now()
	// A is heavily watched already (excluded from the pair); B lightly watched;
	// C has no recorded minutes at all (least watched of the three), so ranking
	// ascending by accumulated time yields exactly {C, B}.
	if err := w.store.RecordMinutes(a.GetUsername(), 500, now); err != nil {
		t.Fatalf("failed to seed watch time for %s: %v", a.GetUsername(), err)
	}
	if err := w.store.RecordMinutes(b.GetUsername(), 5, now); err != nil {
		t.Fatalf("failed to seed watch time for %s: %v", b.GetUsername(), err)
	}

	return w, rt, adapter, a.GetUsername(), b.GetUsername(), c.GetUsername()
}

// T2 (baseline reproduction — MUST FAIL on unmodified main): C enters a
// genuine fair-rotation watch slot alongside the retained companion B (with A
// evicted), exactly as production's rotation would place a late-online
// streamer once it out-ranks a heavily-watched configured channel. Every tick
// the real MinuteSender is asked to deliver a minute-watched beacon for C. On
// unmodified main this never succeeds: C's session snapshot never carries a
// spade URL (see the file doc comment), so every send is rejected before any
// network I/O with Stage=session_snapshot/ErrorCode=no_spade_url, and the
// watcher's own failure-recovery call (noSpadeChecker, modeling
// CheckStreamerOnline on an already-online streamer) cannot repair it either.
func TestMinuteWatchedDelivery_LateOnlineNoSpadeNeverDelivers(t *testing.T) {
	const ticks = 5

	w, rt, adapter, _, bLogin, cLogin := lateOnlineFixture(t)

	for tick := 0; tick < ticks; tick++ {
		w.processWatching(tickCtx(w))
		requireCommittedPair(t, w, tick, cLogin, bLogin)

		if calls := adapter.callsFor(cLogin); len(calls) == 0 {
			t.Fatalf("tick %d: expected a send attempt recorded for %s (it holds a rotation slot)", tick, cLogin)
		}
		if calls := adapter.callsFor(bLogin); len(calls) == 0 {
			t.Fatalf("tick %d: expected a send attempt recorded for the retained companion %s", tick, bLogin)
		}
	}

	// Harness sanity: every real transport POST must correspond 1:1 with an
	// adapter-recorded Delivered outcome (proves rt and adapter are wired to
	// the same production code path, not two independently-fabricated views).
	deliveredCount := 0
	for _, rec := range adapter.allCalls() {
		if rec.result.Delivered {
			deliveredCount++
		}
	}
	urls, _ := rt.beacons()
	if len(urls) != deliveredCount {
		t.Fatalf("transport-recorded beacon count (%d) must equal adapter-recorded Delivered count (%d); harness wiring is not faithfully instrumented",
			len(urls), deliveredCount)
	}

	// Diagnostic: every one of C's sends that did not deliver failed exactly at
	// the stage/code the root cause predicts, and never observed a spade URL.
	for i, rec := range adapter.callsFor(cLogin) {
		if rec.result.Delivered {
			continue
		}
		if rec.hadSpade {
			t.Errorf("call %d: adapter observed a spade URL for %s at generation %d yet the send did not deliver (unexpected failure mode): %+v",
				i, cLogin, rec.generation, rec.result)
		}
		if rec.result.Failure == nil || rec.result.Failure.Stage != StageSessionSnapshot || rec.result.Failure.ErrorCode != "no_spade_url" {
			t.Errorf("call %d: expected %s's failed send to be Stage=%s ErrorCode=no_spade_url (spade never converged), got %+v",
				i, cLogin, StageSessionSnapshot, rec.result)
		}
	}

	// Retained-companion independence (diagnostic in this test — the focused,
	// must-pass check lives in T3): B must have kept delivering every tick
	// regardless of C's failures.
	bDelivered := 0
	for _, rec := range adapter.callsFor(bLogin) {
		if rec.result.Delivered {
			bDelivered++
		}
	}
	if bDelivered != ticks {
		t.Errorf("expected the retained companion %s to deliver on all %d ticks independent of %s's failures, got %d delivered sends",
			bLogin, ticks, cLogin, bDelivered)
	}

	// The headline assertion: C must eventually deliver a real beacon while
	// holding a genuine rotation slot. This is expected to FAIL on unmodified
	// main — that failure IS the reproduction.
	cDelivered := 0
	for _, rec := range adapter.callsFor(cLogin) {
		if rec.result.Delivered {
			cDelivered++
		}
	}
	if cDelivered == 0 {
		t.Fatalf("BUG REPRODUCTION: %s never delivered a real minute-watched beacon across %d ticks while holding a genuine fair-rotation slot "+
			"(committed pair {%s,%s} verified via BrokerSnapshot every tick); every send instead failed as Stage=%s ErrorCode=no_spade_url. "+
			"The spade URL never converges because CheckStreamerOnline only fetches it via ConfirmOnline when the streamer is NOT already "+
			"StatusOnline (internal/api/client.go) — a streamer that first went online via PubSub stream-up (SetConfirmedOnline, then a "+
			"metadata-only UpdateStream) is already StatusOnline by the time it reaches a watch slot, so its failure-recovery re-check "+
			"(w.client.CheckStreamerOnline in processWatching) takes the same metadata-only branch forever and can never self-repair.",
			cLogin, ticks, cLogin, bLogin, StageSessionSnapshot)
	}
}

// T3 (retained-slot independence — MUST PASS on unmodified main): a focused
// re-check that, across every tick where C's send is failing, the retained
// companion B's per-channel delivery is completely unaffected — it delivers a
// real beacon on every single tick. This isolates the claim that C's failure
// is self-contained (a per-slot send outcome) and never blocks or degrades
// its companion slot.
func TestMinuteWatchedDelivery_RetainedCompanionDeliversIndependently(t *testing.T) {
	const ticks = 5

	w, _, adapter, _, bLogin, cLogin := lateOnlineFixture(t)

	for tick := 0; tick < ticks; tick++ {
		w.processWatching(tickCtx(w))
		requireCommittedPair(t, w, tick, cLogin, bLogin)

		calls := adapter.callsFor(bLogin)
		if len(calls) == 0 {
			t.Fatalf("tick %d: expected a send attempt recorded for the retained companion %s", tick, bLogin)
		}
		last := calls[len(calls)-1]
		if !last.result.Delivered {
			t.Fatalf("tick %d: expected the retained companion %s to deliver independent of %s's failure, got %+v",
				tick, bLogin, cLogin, last.result)
		}
	}

	delivered := 0
	for _, rec := range adapter.callsFor(bLogin) {
		if rec.result.Delivered {
			delivered++
		}
	}
	if delivered != ticks {
		t.Fatalf("expected %s to deliver on all %d ticks, got %d delivered sends", bLogin, ticks, delivered)
	}
}
