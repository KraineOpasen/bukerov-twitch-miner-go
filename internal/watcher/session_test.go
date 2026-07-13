package watcher

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakeRefresher records session-refresh calls and can fail either step.
type fakeRefresher struct {
	mu          sync.Mutex
	spadeCalls  []string
	streamCalls []string
	spadeErr    error
	streamErr   error
}

func (f *fakeRefresher) GetSpadeURL(s *models.Streamer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.spadeCalls = append(f.spadeCalls, s.Username)
	if f.spadeErr != nil {
		return f.spadeErr
	}
	s.Stream.SetSpadeURL("http://spade.test/refreshed")
	return nil
}

func (f *fakeRefresher) UpdateStream(s *models.Streamer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.streamCalls = append(f.streamCalls, s.Username)
	return f.streamErr
}

func (f *fakeRefresher) calls() (spade, stream []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.spadeCalls...), append([]string(nil), f.streamCalls...)
}

// staticAvoid is a concurrency-safe AvoidChecker fake.
type staticAvoid struct {
	mu      sync.Mutex
	avoided map[string]bool
}

func (a *staticAvoid) IsAvoided(login string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.avoided[login]
}

func (a *staticAvoid) set(login string, v bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.avoided == nil {
		a.avoided = make(map[string]bool)
	}
	a.avoided[login] = v
}

func occupantsFor(w *MinuteWatcher, idxs ...int) []slotOccupant {
	var slots []slotOccupant
	for _, i := range idxs {
		slots = append(slots, slotOccupant{streamer: w.streamers[i], origin: OriginConfigured, idx: i})
	}
	return slots
}

// TestSessionRefreshFullModeRebuildsSession: RefreshSession re-scrapes the
// spade URL, forces the stream-info gate, and refreshes the stream — and the
// published outcome reports success.
func TestSessionRefreshFullModeRebuildsSession(t *testing.T) {
	w, _ := newTestWatcher(2)
	ref := &fakeRefresher{}
	w.refresher = ref

	w.RequestSessionRefresh(w.streamers[0].Username, RefreshSession)
	w.executeSessionRefreshes(occupantsFor(w, 0, 1))

	spade, stream := ref.calls()
	if len(spade) != 1 || spade[0] != w.streamers[0].Username {
		t.Fatalf("expected one spade re-fetch for the requested channel, got %v", spade)
	}
	if len(stream) != 1 || stream[0] != w.streamers[0].Username {
		t.Fatalf("expected one stream refresh for the requested channel, got %v", stream)
	}
	out, ok := w.LastSessionRefresh(w.streamers[0].Username)
	if !ok || !out.OK || out.Mode != RefreshSession {
		t.Fatalf("expected a published OK outcome, got ok=%v %+v", ok, out)
	}
	if w.streamers[0].Stream.GetSpadeURL() != "http://spade.test/refreshed" {
		t.Error("expected the refreshed spade URL to be stored")
	}
}

// TestSessionRefreshInfoModeSkipsSpade: RefreshStreamInfo must not re-scrape
// the spade URL.
func TestSessionRefreshInfoModeSkipsSpade(t *testing.T) {
	w, _ := newTestWatcher(1)
	ref := &fakeRefresher{}
	w.refresher = ref

	w.RequestSessionRefresh(w.streamers[0].Username, RefreshStreamInfo)
	w.executeSessionRefreshes(occupantsFor(w, 0))

	spade, stream := ref.calls()
	if len(spade) != 0 {
		t.Fatalf("info mode must not touch the spade URL, got %v", spade)
	}
	if len(stream) != 1 {
		t.Fatalf("expected one stream refresh, got %v", stream)
	}
}

// TestSessionRefreshSkippedWhenNotSlotted: a request for a channel that lost
// its slot completes as a skipped (not OK) outcome and performs no network
// work — and the request does not linger for later ticks.
func TestSessionRefreshSkippedWhenNotSlotted(t *testing.T) {
	w, _ := newTestWatcher(2)
	ref := &fakeRefresher{}
	w.refresher = ref

	w.RequestSessionRefresh(w.streamers[1].Username, RefreshSession)
	w.executeSessionRefreshes(occupantsFor(w, 0)) // only streamer 0 slotted

	spade, stream := ref.calls()
	if len(spade) != 0 || len(stream) != 0 {
		t.Fatalf("skipped refresh must not perform network work, got spade=%v stream=%v", spade, stream)
	}
	out, ok := w.LastSessionRefresh(w.streamers[1].Username)
	if !ok || out.OK {
		t.Fatalf("expected a published skipped outcome, got ok=%v %+v", ok, out)
	}

	// The request must have been consumed, not requeued.
	w.executeSessionRefreshes(occupantsFor(w, 0, 1))
	if _, stream := ref.calls(); len(stream) != 0 {
		t.Fatalf("a skipped request must not linger into later ticks, got %v", stream)
	}
}

// TestSessionRefreshCoalescesToStrongestMode: duplicate requests for one
// channel collapse into a single execution with the stronger mode winning,
// regardless of arrival order.
func TestSessionRefreshCoalescesToStrongestMode(t *testing.T) {
	w, _ := newTestWatcher(1)
	ref := &fakeRefresher{}
	w.refresher = ref

	login := w.streamers[0].Username
	w.RequestSessionRefresh(login, RefreshSession)
	w.RequestSessionRefresh(login, RefreshStreamInfo) // weaker: must not downgrade
	w.executeSessionRefreshes(occupantsFor(w, 0))

	spade, stream := ref.calls()
	if len(stream) != 1 {
		t.Fatalf("coalesced request must execute exactly once, got %v", stream)
	}
	if len(spade) != 1 {
		t.Fatalf("the stronger session mode must win the coalesce, got spade=%v", spade)
	}
}

// TestSessionRefreshFailureOutcomes: failures at either step publish a not-OK
// outcome whose detail carries no URL (redaction).
func TestSessionRefreshFailureOutcomes(t *testing.T) {
	w, _ := newTestWatcher(1)
	login := w.streamers[0].Username

	w.refresher = &fakeRefresher{spadeErr: errors.New("boom http://leak.example/sig=abc")}
	w.RequestSessionRefresh(login, RefreshSession)
	w.executeSessionRefreshes(occupantsFor(w, 0))
	out, _ := w.LastSessionRefresh(login)
	if out.OK {
		t.Fatal("spade failure must publish a not-OK outcome")
	}
	if containsAny(out.Detail, "http://", "sig=", "leak.example") {
		t.Fatalf("outcome detail leaked the raw error: %q", out.Detail)
	}

	w.refresher = &fakeRefresher{streamErr: errors.New("stream fail")}
	w.RequestSessionRefresh(login, RefreshStreamInfo)
	w.executeSessionRefreshes(occupantsFor(w, 0))
	out, _ = w.LastSessionRefresh(login)
	if out.OK {
		t.Fatal("stream-info failure must publish a not-OK outcome")
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if sub != "" && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}

// TestReportStatsAccountingAndPruning: successes/failures accumulate per
// slotted channel and reset once the channel leaves the allocation.
func TestReportStatsAccountingAndPruning(t *testing.T) {
	w, _ := newTestWatcher(2)
	a, b := w.streamers[0].Username, w.streamers[1].Username
	now := time.Now()

	w.noteReportOutcome(a, true, now)
	w.noteReportOutcome(a, false, now)
	w.noteReportOutcome(b, true, now)
	w.publishReportStats(occupantsFor(w, 0, 1))

	sa, ok := w.ReportStats(a)
	if !ok || sa.Successes != 1 || sa.Failures != 1 {
		t.Fatalf("expected 1/1 for %s, got ok=%v %+v", a, ok, sa)
	}

	// b leaves the allocation: its counters must be pruned, a's must survive.
	w.noteReportOutcome(a, true, now)
	w.publishReportStats(occupantsFor(w, 0))

	if sa, _ = w.ReportStats(a); sa.Successes != 2 {
		t.Fatalf("expected a's counters to accumulate across ticks, got %+v", sa)
	}
	if _, ok := w.ReportStats(b); ok {
		t.Fatal("expected b's counters to be pruned once it left the slots")
	}
}

// TestAvoidedChannelExcludedFromSelection: an avoided configured channel never
// becomes a watch candidate (mirrors DisableWatch), and candidates from
// sources are dropped too.
func TestAvoidedChannelExcludedFromSelection(t *testing.T) {
	w, _ := newTestWatcher(2)
	avoid := &staticAvoid{}
	avoid.set(w.streamers[0].Username, true)
	for _, s := range w.streamers {
		s.SetOnline()
		s.OnlineAt = time.Now().Add(-time.Minute)
	}

	online := w.getOnlineStreamers(avoid)
	if len(online) != 1 || online[0] != 1 {
		t.Fatalf("expected only the non-avoided streamer, got %v", online)
	}

	src := &staticSource{name: "discovery", cand: []Candidate{
		{Streamer: discoveryStreamer("disco", true), Origin: OriginDiscovery},
	}}
	avoid.set("disco", true)
	if got := w.gatherCandidates([]CandidateSource{src}, avoid); len(got) != 0 {
		t.Fatalf("expected the avoided discovery candidate to be dropped, got %v", got)
	}
}

// TestBrokerLoopConcurrentWithWatchdogCalls is the Stage 3 mandated race test:
// the broker loop ticks and sends on live streamers while a "watchdog"
// goroutine concurrently stages session refreshes, reads report stats and
// refresh outcomes, and flips the avoid list — the exact two-goroutine overlap
// the staged-refresh design must make safe. Run under -race.
func TestBrokerLoopConcurrentWithWatchdogCalls(t *testing.T) {
	sender := &countingSender{sent: make(chan string, 1024)}
	checker := &staticChecker{checked: make(chan string, 1024)}
	w, streamers := newLoopWatcher(2, sender, checker)
	ref := &fakeRefresher{}
	w.refresher = ref
	avoid := &staticAvoid{}
	w.SetAvoidChecker(avoid)
	// A small real inter-send pause keeps the send loop in flight while the
	// watchdog goroutine hammers the staging/reading APIs, so the two sides
	// genuinely overlap for the race detector.
	w.pacer = func(time.Duration) bool { time.Sleep(2 * time.Millisecond); return true }

	ctx, cancel := context.WithCancel(context.Background())
	w.ctx = ctx
	w.cancel = cancel

	// Pre-stage one refresh so the first tick deterministically executes it,
	// independent of how the concurrent staging below interleaves.
	w.RequestSessionRefresh(streamers[0].Username, RefreshSession)

	var loopDone sync.WaitGroup
	loopDone.Add(1)
	go func() {
		defer loopDone.Done()
		w.loop()
	}()

	login := streamers[0].Username
	var stop atomic.Bool
	var watchdogDone sync.WaitGroup
	watchdogDone.Add(1)
	go func() {
		defer watchdogDone.Done()
		for i := 0; !stop.Load(); i++ {
			w.RequestSessionRefresh(login, RefreshSession)
			w.RequestSessionRefresh(login, RefreshStreamInfo)
			_, _ = w.LastSessionRefresh(login)
			_, _ = w.ReportStats(login)
			_ = w.IsWatching(login)
			_ = w.BrokerSnapshot()
			avoid.set(streamers[1].Username, i%2 == 0)
			// The refreshed streamer's session state is read concurrently by the
			// loop's send path — exercise the locked accessors from this side too.
			_ = streamers[0].Stream.GetSpadeURL()
			_ = streamers[0].Stream.GetCampaigns()
			time.Sleep(time.Millisecond)
		}
	}()

	// Let the loop and the watchdog overlap across at least two ticks (the
	// loop's inter-tick sleep is ~1s ±20%).
	time.Sleep(1500 * time.Millisecond)
	stop.Store(true)
	watchdogDone.Wait()
	cancel()
	loopDone.Wait()

	// Sanity: the loop actually sent and at least one refresh executed.
	if len(sender.sent) == 0 {
		t.Fatal("expected the broker loop to have sent minute-watched reports during the race window")
	}
	if _, stream := ref.calls(); len(stream) == 0 {
		t.Fatal("expected at least one staged session refresh to have executed on the loop goroutine")
	}
}
