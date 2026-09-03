package watcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// B2 — watch context ownership, broker half.
//
// The MinuteWatcher owns the watch generation: Start derives w.ctx from the
// miner's run context and spawns the single loop goroutine; Stop cancels it and
// joins loopDone, bounded by stopJoinTimeout. These tests assert that the
// generation is the true cancellation OWNER of the work it starts — a cancelled
// generation must not sit in a watch-transport request that nothing can
// interrupt, and a clean Stop must reach quiescence rather than time out.
//
// Everything is local: an injected http.RoundTripper on the unexported
// MinuteSender.httpClient field. No real Twitch traffic, no OAuth, no tokens.

// transportParkCap is the failure guard on a parked watch-transport request:
// never load-bearing for a pass, only a backstop so a request that genuinely
// ignores cancellation cannot wedge the suite.
const transportParkCap = 3 * time.Second

// quiesceWindow is how long a cancelled watch generation is given to reach
// quiescence. It is deliberately far below transportParkCap so a pass can only
// come from observed cancellation, never from the backstop expiring.
const quiesceWindow = time.Second

// parkingRT parks every watch-transport request until the request's OWN context
// is cancelled (the behaviour a real transport has) or the hard cap expires. It
// closes entered on the first request so a test can synchronise on "a real
// production request is in flight" without sleeping.
type parkingRT struct {
	once    sync.Once
	entered chan struct{}

	mu     sync.Mutex
	parked int
	capped int
}

func newParkingRT() *parkingRT {
	return &parkingRT{entered: make(chan struct{})}
}

func (p *parkingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	p.once.Do(func() { close(p.entered) })
	p.mu.Lock()
	p.parked++
	p.mu.Unlock()

	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	case <-time.After(transportParkCap):
		p.mu.Lock()
		p.capped++
		p.mu.Unlock()
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("#EXTM3U\n")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func (p *parkingRT) cappedRequests() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capped
}

// watchGenerationStreamer is an online, slot-eligible streamer carrying a
// coherent watch session (spade URL + payload), so the real MinuteSender gets
// past its session-snapshot gate and reaches the network stages.
func watchGenerationStreamer(login string) *models.Streamer {
	s := models.NewStreamer(login, models.DefaultStreamerSettings())
	s.ChannelID = "cid"
	s.SetConfirmedOnline()
	s.OnlineAt = time.Now().Add(-time.Minute)
	s.SetChannelPointsCapability(models.CapabilityEnabled, models.CapReasonConfirmedContext)
	s.Stream.Update("b1", "t", nil, nil, 1)
	s.Stream.SetSpadeURL("https://spade.test/track")
	mustSetPayload(s.Stream, "cid", "b1", "44322889", login, nil, nil)
	return s
}

// newWatchTransportWatcher wires the REAL MinuteSender (over rt) into a real
// MinuteWatcher, so a cancellation assertion exercises production transport
// code rather than a fake's own blocking behaviour.
func newWatchTransportWatcher(rt http.RoundTripper) *MinuteWatcher {
	streamer := watchGenerationStreamer("watched")
	return &MinuteWatcher{
		client:     &staticChecker{checked: make(chan string, 8)},
		streamers:  []*models.Streamer{streamer},
		priorities: []config.Priority{config.PriorityOrder},
		settings:   config.RateLimitSettings{MinuteWatchedInterval: 1},
		sender: &MinuteSender{
			client:     fakeToken{sig: "s", token: "t"},
			httpClient: &http.Client{Transport: rt},
		},
		pacer: func(time.Duration) bool { return true },
	}
}

// tickCtx is the generation context a test tick runs on: the watcher's own when
// the test installed one (the pre-B2 tests assign w.ctx directly to exercise the
// pacing/shutdown paths), otherwise a background context. It keeps every
// existing tick test on exactly the context it already used.
func tickCtx(w *MinuteWatcher) context.Context {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ctx != nil {
		return w.ctx
	}
	return context.Background()
}

// loopFinished reports the watcher's loop-exit channel (the generation's
// quiescence signal) under the same lock Start/Stop use.
func (w *MinuteWatcher) loopFinished() <-chan struct{} {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.loopDone
}

// TestWatchGenerationCancellationAbortsInFlightTransport is the broker-half
// falsifier for HLS MASTER/VARIANT/SEGMENT and BEACON: with a production watch
// request parked in the transport, cancelling the watch generation must abort
// it, so the loop goroutine — the generation's only worker — reaches quiescence.
//
// Before the repair the send runs on context.Background(), so nothing the
// generation cancels reaches the request and the loop stays blocked until the
// transport's own timeout.
func TestWatchGenerationCancellationAbortsInFlightTransport(t *testing.T) {
	rt := newParkingRT()
	w := newWatchTransportWatcher(rt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("the watch generation must start: %v", err)
	}

	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch transport never issued a request")
	}

	cancel()

	select {
	case <-w.loopFinished():
	case <-time.After(quiesceWindow):
		t.Fatal("cancelling the watch generation did not abort the in-flight watch-transport request: " +
			"the generation does not own the work it started")
	}

	if capped := rt.cappedRequests(); capped != 0 {
		t.Fatalf("%d watch-transport request(s) ran to the park cap instead of observing cancellation", capped)
	}
}

// TestWatchGenerationStopReachesQuiescence is the CLEAN STOP falsifier: when
// every owned operation cooperates with cancellation, Stop must join the
// generation rather than give up on its bounded timeout.
func TestWatchGenerationStopReachesQuiescence(t *testing.T) {
	old := stopJoinTimeout
	stopJoinTimeout = quiesceWindow
	t.Cleanup(func() { stopJoinTimeout = old })

	rt := newParkingRT()
	w := newWatchTransportWatcher(rt)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("the watch generation must start: %v", err)
	}

	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch transport never issued a request")
	}

	if err := w.Stop(); err != nil {
		t.Fatalf("a cooperating generation must stop cleanly, got %v", err)
	}

	select {
	case <-w.loopFinished():
	default:
		t.Fatal("Stop returned while generation-owned work was still running: " +
			"a clean stop must reach quiescence, not time out")
	}
}

// --- BEACON: no post-cancel beacon may be newly started ---------------------

// stageCountingRT answers every watch-transport stage successfully and counts
// beacon POSTs, so a test can prove exactly how many beacons a generation
// emitted. It parks only where the test asks it to.
type stageCountingRT struct {
	mu      sync.Mutex
	beacons int

	gate    chan struct{} // closed by the test to release a parked stage
	parkOn  string        // "GET" (master/variant), "HEAD" (segment), "POST" (beacon)
	entered chan struct{}
	once    sync.Once
}

func newStageCountingRT(parkOn string) *stageCountingRT {
	return &stageCountingRT{
		gate:    make(chan struct{}),
		parkOn:  parkOn,
		entered: make(chan struct{}),
	}
}

func (r *stageCountingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost {
		// Counted on ATTEMPT, before the cancellation check, so a test can tell
		// "the beacon was never started" apart from "the beacon was started and
		// the transport rejected it".
		r.mu.Lock()
		r.beacons++
		r.mu.Unlock()
	}
	if err := req.Context().Err(); err != nil {
		return nil, err // honour cancellation like a real transport
	}
	if req.Method == r.parkOn {
		r.once.Do(func() { close(r.entered) })
		select {
		case <-r.gate:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(transportParkCap):
		}
	}
	body := "#EXTM3U\nhttps://variant.test/low.m3u8\n"
	if strings.Contains(req.URL.Host, "variant") {
		body = "#EXTM3U\nhttps://seg.test/s.ts\n"
	}
	status := http.StatusOK
	if req.Method == http.MethodPost {
		status = http.StatusNoContent
		body = ""
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func (r *stageCountingRT) beaconCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.beacons
}

// TestCancelledGenerationStartsNoBeacon is acceptance E: a generation cancelled
// while the HLS stage is in flight must not go on to POST a minute-watched
// beacon. The HLS failure is non-fatal by design, so without the cancellation
// classification the send would sail straight into the beacon.
func TestCancelledGenerationStartsNoBeacon(t *testing.T) {
	rt := newStageCountingRT(http.MethodGet)
	sender := &MinuteSender{
		client:     fakeToken{sig: "s", token: "t"},
		httpClient: &http.Client{Transport: rt},
	}
	streamer := watchGenerationStreamer("watched")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan SendResult, 1)
	go func() { done <- sender.Send(ctx, streamer) }()

	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the HLS stage never issued a request")
	}
	cancel()

	select {
	case res := <-done:
		if !res.Cancelled {
			t.Fatalf("a send cancelled mid-transport must report Cancelled, got %+v", res)
		}
		if res.Delivered {
			t.Fatal("a cancelled send must never report a delivered minute")
		}
		if res.Failure != nil {
			t.Fatalf("a cancelled send must not be laundered into a transport failure, got stage %q", res.Failure.Stage)
		}
		if res.SimulateErr != nil {
			t.Fatalf("a cancelled send must report no simulate failure (an aborted request says nothing about Twitch), got %v", res.SimulateErr)
		}
	case <-time.After(quiesceWindow):
		t.Fatal("the cancelled send did not return")
	}

	if n := rt.beaconCount(); n != 0 {
		t.Fatalf("a cancelled generation started %d beacon POST(s); it must start none", n)
	}
}

// TestUncancelledSendStillDelivers is acceptance K: normal operation is
// unchanged — same ordering, same generation reporting, exactly one beacon.
func TestUncancelledSendStillDelivers(t *testing.T) {
	rt := newStageCountingRT("")
	sender := &MinuteSender{
		client:     fakeToken{sig: "s", token: "t"},
		httpClient: &http.Client{Transport: rt},
	}
	streamer := watchGenerationStreamer("watched")
	gen := streamer.Stream.SessionGeneration()

	res := sender.Send(context.Background(), streamer)
	if !res.Delivered || res.Cancelled || res.Failure != nil || res.Generation != gen {
		t.Fatalf("an uncancelled send must deliver against the captured generation, got %+v (gen=%d)", res, gen)
	}
	if n := rt.beaconCount(); n != 1 {
		t.Fatalf("expected exactly one beacon, got %d", n)
	}
}

// --- SESSION REFRESH: cancellable and joined --------------------------------

// blockingRefresher parks inside RefreshPlaybackSession until its context is
// cancelled, standing in for a refresh whose Twitch I/O is in flight.
type blockingRefresher struct {
	entered chan struct{}
	exited  chan struct{}
	once    sync.Once
}

func (b *blockingRefresher) RefreshPlaybackSession(ctx context.Context, _ *models.Streamer, _ bool, _ models.ExpectedSession) twitch.SessionRefreshResult {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-ctx.Done():
	case <-time.After(transportParkCap):
	}
	close(b.exited)
	return twitch.SessionRefreshResult{}
}

// TestSessionRefreshIsCancelledAndJoined is acceptance F: a staged refresh runs
// on a worker goroutine that the tick joins, so the refresh must observe the
// generation's cancellation — otherwise the join (and therefore Stop) is
// hostage to the refresh's own transport budget.
func TestSessionRefreshIsCancelledAndJoined(t *testing.T) {
	ref := &blockingRefresher{entered: make(chan struct{}), exited: make(chan struct{})}
	streamer := watchGenerationStreamer("watched")
	w := &MinuteWatcher{
		streamers: []*models.Streamer{streamer},
		settings:  config.RateLimitSettings{MinuteWatchedInterval: 1},
		refresher: ref,
	}
	w.RequestSessionRefresh(SessionRefreshRequest{
		RequestID: "r1", Login: "watched", Mode: RefreshSession, Requested: time.Now(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	joined := make(chan struct{})
	go func() {
		defer close(joined)
		w.executeSessionRefreshes(ctx, []slotOccupant{{streamer: streamer, idx: 0}})
	}()

	select {
	case <-ref.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the session refresh never started")
	}
	cancel()

	select {
	case <-joined:
	case <-time.After(quiesceWindow):
		t.Fatal("executeSessionRefreshes did not join its cancelled workers: " +
			"the refresh does not observe the watch generation's cancellation")
	}
	select {
	case <-ref.exited:
	default:
		t.Fatal("the refresh worker was not joined before executeSessionRefreshes returned")
	}
}

// --- DIRTY STOP + generation exclusion --------------------------------------

// uncooperativeSender deliberately ignores its context, standing in for a
// non-cooperative dependency that no cancellation can interrupt.
type uncooperativeSender struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (u *uncooperativeSender) Send(context.Context, *models.Streamer) SendResult {
	u.once.Do(func() { close(u.entered) })
	<-u.release
	return SendResult{}
}

// TestDirtyStopIsExplicitAndExcludesANewGeneration is acceptance J: when the
// bounded join expires, teardown is reported as dirty rather than logged and
// forgotten, and the still-live generation blocks a fresh one from starting.
func TestDirtyStopIsExplicitAndExcludesANewGeneration(t *testing.T) {
	old := stopJoinTimeout
	stopJoinTimeout = 100 * time.Millisecond
	t.Cleanup(func() { stopJoinTimeout = old })

	snd := &uncooperativeSender{entered: make(chan struct{}), release: make(chan struct{})}
	streamer := watchGenerationStreamer("watched")
	w := &MinuteWatcher{
		client:     &staticChecker{checked: make(chan string, 8)},
		streamers:  []*models.Streamer{streamer},
		priorities: []config.Priority{config.PriorityOrder},
		settings:   config.RateLimitSettings{MinuteWatchedInterval: 1},
		sender:     snd,
		pacer:      func(time.Duration) bool { return true },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("the first generation must start: %v", err)
	}

	select {
	case <-snd.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the send never started")
	}

	err := w.Stop()
	if err == nil {
		t.Fatal("a join that timed out with owned work still running must be reported as a dirty teardown, not silently")
	}
	if !errors.Is(err, ErrStopJoinTimeout) {
		t.Fatalf("a dirty teardown must be classifiable via ErrStopJoinTimeout, got %v", err)
	}

	// The old generation is still live: a fresh one must not be admitted.
	if startErr := w.Start(context.Background()); !errors.Is(startErr, ErrGenerationLive) {
		t.Fatalf("a new generation must be refused while the previous one is live, got %v", startErr)
	}

	// Release the stuck worker and confirm the generation then joins cleanly.
	close(snd.release)
	select {
	case <-w.loopFinished():
	case <-time.After(5 * time.Second):
		t.Fatal("the released generation never finished")
	}
}

// TestCleanRestartAdmitsAFreshGeneration is acceptance L: after a genuinely
// clean join, a subsequent generation starts normally and the previous one's
// cancellation does not poison it.
func TestCleanRestartAdmitsAFreshGeneration(t *testing.T) {
	rt := newParkingRT()
	w := newWatchTransportWatcher(rt)

	ctx, cancel := context.WithCancel(context.Background())
	if err := w.Start(ctx); err != nil {
		t.Fatalf("first generation must start: %v", err)
	}
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first generation never issued a watch-transport request")
	}
	cancel()
	if err := w.Stop(); err != nil {
		t.Fatalf("the first generation must stop cleanly: %v", err)
	}

	sent := make(chan string, 4)
	w.mu.Lock()
	w.sender = &countingSender{sent: sent}
	w.mu.Unlock()

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	if err := w.Start(ctx2); err != nil {
		t.Fatalf("a fresh generation must start after a clean join: %v", err)
	}
	t.Cleanup(func() { cancel2(); _ = w.Stop() })

	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the fresh generation never sent a minute-watched: the old cancellation poisoned it")
	}
}

// --- TOKEN/GQL: the send must hand the token call ITS OWN context ------------

// ctxAwareToken models the real *twitch.TwitchClient's playback-token call: it
// blocks until the context it was given ends. If Send were to hand it anything
// other than the watch generation's context, it would never return.
type ctxAwareToken struct {
	entered chan struct{}
	once    sync.Once
}

func (c *ctxAwareToken) GetPlaybackAccessToken(ctx context.Context, _ string) (string, string, error) {
	c.once.Do(func() { close(c.entered) })
	select {
	case <-ctx.Done():
		return "", "", ctx.Err()
	case <-time.After(transportParkCap):
		return "s", "t", nil
	}
}

// TestPlaybackTokenCallCarriesTheGenerationContext is the TOKEN/GQL falsifier on
// the broker side: the playback-token request is the first network step of every
// send, so a send that hands it a background context leaves the generation
// unable to interrupt its own first request.
func TestPlaybackTokenCallCarriesTheGenerationContext(t *testing.T) {
	tok := &ctxAwareToken{entered: make(chan struct{})}
	sender := &MinuteSender{
		client:     tok,
		httpClient: &http.Client{Transport: newStageCountingRT("")},
	}
	streamer := watchGenerationStreamer("watched")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan SendResult, 1)
	go func() { done <- sender.Send(ctx, streamer) }()

	select {
	case <-tok.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the playback-token step never ran")
	}
	cancel()

	select {
	case res := <-done:
		if !res.Cancelled {
			t.Fatalf("a token call cancelled with its generation must report Cancelled, got %+v", res)
		}
		if res.Failure != nil {
			t.Fatalf("a cancelled token call must not be reported as a token failure, got stage %q", res.Failure.Stage)
		}
	case <-time.After(quiesceWindow):
		t.Fatal("cancelling the generation did not reach the playback-token call: " +
			"Send does not hand it the generation's context")
	}
}

// blockingSlotRefresher parks a staged session refresh until the generation is
// cancelled — the realistic way a tick reaches its slot loop already cancelled.
type blockingSlotRefresher struct {
	entered chan struct{}
	once    sync.Once
}

func (b *blockingSlotRefresher) RefreshPlaybackSession(ctx context.Context, _ *models.Streamer, _ bool, _ models.ExpectedSession) twitch.SessionRefreshResult {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-ctx.Done():
	case <-time.After(transportParkCap):
	}
	return twitch.SessionRefreshResult{}
}

// recordingSender records how many sends a tick actually started.
type recordingSender struct {
	mu     sync.Mutex
	starts int
}

func (r *recordingSender) Send(context.Context, *models.Streamer) SendResult {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
	return SendResult{Delivered: true}
}

func (r *recordingSender) started() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts
}

// TestCancelledTickStartsNoSend is the other half of acceptance E: when the
// generation is cancelled earlier in the tick (here, while a staged session
// refresh is in flight), the tick must not go on to start a send for a slot it
// no longer owns.
func TestCancelledTickStartsNoSend(t *testing.T) {
	ref := &blockingSlotRefresher{entered: make(chan struct{})}
	snd := &recordingSender{}
	streamer := watchGenerationStreamer("watched")

	w := &MinuteWatcher{
		client:     &staticChecker{checked: make(chan string, 8)},
		streamers:  []*models.Streamer{streamer},
		priorities: []config.Priority{config.PriorityOrder},
		settings:   config.RateLimitSettings{MinuteWatchedInterval: 1},
		sender:     snd,
		refresher:  ref,
		pacer:      func(time.Duration) bool { return true },
	}
	w.RequestSessionRefresh(SessionRefreshRequest{
		RequestID: "r1", Login: "watched", Mode: RefreshSession, Requested: time.Now(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("the watch generation must start: %v", err)
	}

	select {
	case <-ref.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the staged session refresh never ran")
	}
	cancel()

	select {
	case <-w.loopFinished():
	case <-time.After(quiesceWindow):
		t.Fatal("the cancelled generation did not quiesce")
	}

	if n := snd.started(); n != 0 {
		t.Fatalf("a generation cancelled before its slot loop started %d send(s); it must start none", n)
	}
}

// parkingChecker parks inside the online check until its context ends and
// counts how many checks the generation actually started.
type parkingChecker struct {
	entered chan struct{}
	once    sync.Once

	mu    sync.Mutex
	calls int
}

func (p *parkingChecker) CheckStreamerOnlineContext(ctx context.Context, _ *models.Streamer) models.StatusTransition {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	p.once.Do(func() { close(p.entered) })
	select {
	case <-ctx.Done():
	case <-time.After(transportParkCap):
	}
	return models.StatusTransition{}
}

func (p *parkingChecker) started() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// staleOnlineStreamer is online and slot-eligible, paired with the watcher's
// existing per-instance routineRefreshAfter seam (set to a nanosecond below) so
// its stream info counts as stale on the very first tick.
func staleOnlineStreamer(login string) *models.Streamer {
	s := watchGenerationStreamer(login)
	s.ChannelID = "cid-" + login
	return s
}

// TestCancelledGenerationStartsNoFurtherOnlineCheck is acceptance G's "must not
// start" half: the tick re-verifies every stale online streamer in sequence, so
// a cancellation landing during the first check must stop the loop rather than
// let it work through the rest of the roster on a dead generation.
func TestCancelledGenerationStartsNoFurtherOnlineCheck(t *testing.T) {
	chk := &parkingChecker{entered: make(chan struct{})}
	w := &MinuteWatcher{
		client: chk,
		streamers: []*models.Streamer{
			staleOnlineStreamer("a"), staleOnlineStreamer("b"), staleOnlineStreamer("c"),
		},
		priorities:          []config.Priority{config.PriorityOrder},
		settings:            config.RateLimitSettings{MinuteWatchedInterval: 1},
		sender:              &recordingSender{},
		pacer:               func(time.Duration) bool { return true },
		routineRefreshAfter: time.Nanosecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("the watch generation must start: %v", err)
	}

	select {
	case <-chk.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the routine re-verification never ran")
	}
	cancel()

	select {
	case <-w.loopFinished():
	case <-time.After(quiesceWindow):
		t.Fatal("the cancelled generation did not quiesce")
	}

	if n := chk.started(); n != 1 {
		t.Fatalf("a cancelled generation started %d online check(s); only the one already in flight is allowed", n)
	}
}

// TestPaceIsInterruptibleByTheGeneration is acceptance H for the broker's own
// inter-send wait: pace spreads a tick's sends across the interval with ±20%
// jitter, and production installs no pacer override, so this is a real
// generation-owned wait that must end the moment the generation does.
func TestPaceIsInterruptibleByTheGeneration(t *testing.T) {
	w := &MinuteWatcher{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan bool, 1)
	go func() { done <- w.pace(ctx, time.Hour) }()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("a wait interrupted by cancellation must report that the send loop should stop")
		}
	case <-time.After(quiesceWindow):
		t.Fatal("pace ignored the generation's cancellation and slept on")
	}
}

// TestPaceStillWaitsWhenTheGenerationIsAlive guards the other direction: the
// jittered spacing is a functional requirement, not incidental noise, so an
// uncancelled wait must actually wait.
func TestPaceStillWaitsWhenTheGenerationIsAlive(t *testing.T) {
	w := &MinuteWatcher{}
	start := time.Now()
	if !w.pace(context.Background(), 50*time.Millisecond) {
		t.Fatal("an uncancelled wait must report that the send loop may continue")
	}
	// ±20% jitter around 50ms, so the floor is 40ms.
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("the jittered inter-send spacing was skipped: waited only %v", elapsed)
	}
}

// failingSender reports a genuine (non-cancellation) transport failure, which is
// what drives the broker's post-failure online re-check.
type failingSender struct{}

func (failingSender) Send(context.Context, *models.Streamer) SendResult {
	return SendResult{Failure: &WatchFailure{Stage: StageBeacon, Status: 500, ErrorCode: "beacon_http_500"}}
}

// TestPostFailureOnlineRecheckIsGenerationOwned covers the second watcher-owned
// online check: after a failed send the broker re-checks the channel's status,
// and that check runs inline on the loop goroutine. It must belong to the watch
// generation like every other operation the tick starts.
func TestPostFailureOnlineRecheckIsGenerationOwned(t *testing.T) {
	chk := &parkingChecker{entered: make(chan struct{})}
	w := &MinuteWatcher{
		client:     chk,
		streamers:  []*models.Streamer{watchGenerationStreamer("watched")},
		priorities: []config.Priority{config.PriorityOrder},
		settings:   config.RateLimitSettings{MinuteWatchedInterval: 1},
		sender:     failingSender{},
		pacer:      func(time.Duration) bool { return true },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("the watch generation must start: %v", err)
	}

	select {
	case <-chk.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the post-failure online re-check never ran")
	}
	cancel()

	select {
	case <-w.loopFinished():
	case <-time.After(quiesceWindow):
		t.Fatal("cancelling the generation did not reach the post-failure online re-check: " +
			"it would pin the loop goroutine for the whole transport budget")
	}
}

// --- BEACON in flight, HLS variant and HLS segment ---------------------------

// sendCancelledMidStage parks the production send at the named transport stage,
// cancels the generation while that request is in flight, and returns the
// SendResult. It exercises the real MinuteSender end to end.
func sendCancelledMidStage(t *testing.T, parkOn string) (SendResult, *stageCountingRT) {
	t.Helper()
	rt := newStageCountingRT(parkOn)
	sender := &MinuteSender{
		client:     fakeToken{sig: "s", token: "t"},
		httpClient: &http.Client{Transport: rt},
	}
	streamer := watchGenerationStreamer("watched")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan SendResult, 1)
	go func() { done <- sender.Send(ctx, streamer) }()

	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("the send never reached the %s stage", parkOn)
	}
	cancel()

	select {
	case res := <-done:
		return res, rt
	case <-time.After(quiesceWindow):
		t.Fatalf("cancelling the generation did not abort the in-flight %s request", parkOn)
	}
	return SendResult{}, rt
}

// TestBeaconInFlightIsCancelledWithTheGeneration is acceptance E's other half:
// once the beacon POST is already on the wire, the generation must still be able
// to abort it rather than wait out the sender's 20s client timeout.
func TestBeaconInFlightIsCancelledWithTheGeneration(t *testing.T) {
	res, rt := sendCancelledMidStage(t, http.MethodPost)
	if !res.Cancelled {
		t.Fatalf("a beacon cancelled in flight must report Cancelled, got %+v", res)
	}
	if res.Delivered {
		t.Fatal("a beacon aborted by cancellation must never count as a delivered minute")
	}
	if res.Failure != nil {
		t.Fatalf("a cancelled beacon must not be reported as a transport failure, got stage %q", res.Failure.Stage)
	}
	if n := rt.beaconCount(); n != 1 {
		t.Fatalf("expected exactly one attempted beacon (the parked one), got %d", n)
	}
}

// TestHLSSegmentRequestIsCancelledWithTheGeneration is acceptance D: the segment
// HEAD is the last HLS step and has its own request construction, so it needs
// its own witness rather than inheriting the master playlist's.
func TestHLSSegmentRequestIsCancelledWithTheGeneration(t *testing.T) {
	res, rt := sendCancelledMidStage(t, http.MethodHead)
	if !res.Cancelled {
		t.Fatalf("a generation cancelled during the segment request must report Cancelled, got %+v", res)
	}
	if n := rt.beaconCount(); n != 0 {
		t.Fatalf("a generation cancelled during the segment request started %d beacon(s); it must start none", n)
	}
}

// TestHLSVariantRequestIsCancelledWithTheGeneration is acceptance C: the variant
// playlist is a second GET issued against the URL parsed out of the master
// playlist, so it is a distinct request that must carry the same context.
func TestHLSVariantRequestIsCancelledWithTheGeneration(t *testing.T) {
	rt := newVariantParkingRT()
	sender := &MinuteSender{
		client:     fakeToken{sig: "s", token: "t"},
		httpClient: &http.Client{Transport: rt},
	}
	streamer := watchGenerationStreamer("watched")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan SendResult, 1)
	go func() { done <- sender.Send(ctx, streamer) }()

	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the send never reached the variant playlist request")
	}
	cancel()

	select {
	case res := <-done:
		if !res.Cancelled {
			t.Fatalf("a generation cancelled during the variant request must report Cancelled, got %+v", res)
		}
	case <-time.After(quiesceWindow):
		t.Fatal("cancelling the generation did not abort the in-flight variant playlist request")
	}
}

// variantParkingRT answers the master playlist immediately and parks the SECOND
// GET — the selected variant — so a test can cancel with exactly that request in
// flight.
type variantParkingRT struct {
	entered chan struct{}
	once    sync.Once

	mu   sync.Mutex
	gets int
}

func newVariantParkingRT() *variantParkingRT {
	return &variantParkingRT{entered: make(chan struct{})}
}

func (v *variantParkingRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	if req.Method == http.MethodGet {
		v.mu.Lock()
		v.gets++
		n := v.gets
		v.mu.Unlock()
		if n >= 2 {
			v.once.Do(func() { close(v.entered) })
			select {
			case <-req.Context().Done():
				return nil, req.Context().Err()
			case <-time.After(transportParkCap):
			}
		}
	}
	body := "#EXTM3U\nhttps://variant.test/low.m3u8\n"
	if strings.Contains(req.URL.Host, "variant") {
		body = "#EXTM3U\nhttps://seg.test/s.ts\n"
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// --- the broker's Cancelled arm: no fabricated credit -----------------------

// cancellingSender reports a cancelled send, exactly as the real MinuteSender
// does once its generation ends mid-transport.
type cancellingSender struct{}

func (cancellingSender) Send(context.Context, *models.Streamer) SendResult {
	return SendResult{Cancelled: true}
}

// TestCancelledSendCreditsNothing pins the broker's Cancelled arm: a teardown
// must not be credited as a watched minute. Without the arm a cancelled send
// (not Stale, no Failure) falls into the delivered branch and books a minute,
// a persisted watch-time row, a success statistic and streak progress that the
// channel never earned.
func TestCancelledSendCreditsNothing(t *testing.T) {
	store, db := openWatchTimeStore(t, filepath.Join(t.TempDir(), "cancelled.db"))
	t.Cleanup(func() { _ = db.Close() })

	chk := &parkingChecker{entered: make(chan struct{})}
	streamer := watchGenerationStreamer("watched")
	w := &MinuteWatcher{
		client:     chk,
		streamers:  []*models.Streamer{streamer},
		priorities: []config.Priority{config.PriorityOrder},
		settings:   config.RateLimitSettings{MinuteWatchedInterval: 1},
		sender:     cancellingSender{},
		store:      store,
		pacer:      func(time.Duration) bool { return true },
	}

	before := streamer.Stream.GetMinuteWatched()
	w.processWatching(context.Background())

	if after := streamer.Stream.GetMinuteWatched(); after != before {
		t.Fatalf("a cancelled send credited a watched minute: %v -> %v", before, after)
	}
	if mins, err := store.WindowMinutes([]string{"watched"}, time.Now()); err != nil {
		t.Fatalf("reading the watch-time store failed: %v", err)
	} else if mins["watched"] != 0 {
		t.Fatalf("a cancelled send persisted %v watch minutes; it must persist none", mins["watched"])
	}
	if stats, ok := w.ReportStats("watched"); ok && (stats.Successes != 0 || stats.Failures != 0) {
		t.Fatalf("a cancelled send moved the delivery accounting: %+v", stats)
	}
	if n := chk.started(); n != 0 {
		t.Fatalf("a cancelled send triggered %d online re-check(s); a teardown must trigger none", n)
	}
}

// --- CandidateSource: the generation context must reach the source -----------

// parkingSource parks inside WatchCandidates until the context it was HANDED is
// done — so it can only be released by the generation's own cancellation.
type parkingSource struct {
	entered chan struct{}
	once    sync.Once

	mu     sync.Mutex
	capped bool
}

func (p *parkingSource) SourceName() string { return "parking" }

func (p *parkingSource) WatchCandidates(ctx context.Context) []Candidate {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-ctx.Done():
	case <-time.After(transportParkCap):
		p.mu.Lock()
		p.capped = true
		p.mu.Unlock()
	}
	return nil
}

func (p *parkingSource) hitCap() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.capped
}

// TestCandidateSourceReceivesTheGenerationContext pins the broker end of the
// CandidateSource contract: sources prepare candidates ON this goroutine and do
// their own re-verification I/O there, so the context they are handed must be
// the live generation's, not a background one.
func TestCandidateSourceReceivesTheGenerationContext(t *testing.T) {
	src := &parkingSource{entered: make(chan struct{})}
	w := &MinuteWatcher{
		client:     &staticChecker{checked: make(chan string, 8)},
		streamers:  []*models.Streamer{watchGenerationStreamer("watched")},
		priorities: []config.Priority{config.PriorityOrder},
		settings:   config.RateLimitSettings{MinuteWatchedInterval: 1},
		sender:     &recordingSender{},
		pacer:      func(time.Duration) bool { return true },
	}
	w.AddSource(src)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := w.Start(ctx); err != nil {
		t.Fatalf("the watch generation must start: %v", err)
	}

	select {
	case <-src.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the source's candidate preparation never ran")
	}
	cancel()

	select {
	case <-w.loopFinished():
	case <-time.After(quiesceWindow):
		t.Fatal("cancelling the generation did not reach the candidate source: " +
			"source preparation would pin the broker's loop goroutine after cancellation")
	}
	if src.hitCap() {
		t.Fatal("the source ran to its park cap instead of observing the generation's cancellation")
	}
}

// TestRandomizedDelayKeepsItsJitter guards the anti-fingerprinting jitter the
// repair rewrote around: the inter-send spacing must stay spread, not collapse
// to a fixed interval.
func TestRandomizedDelayKeepsItsJitter(t *testing.T) {
	w := &MinuteWatcher{}
	const base = time.Second
	seen := make(map[time.Duration]bool)
	for i := 0; i < 200; i++ {
		d := w.randomizedDelay(base)
		if d < 800*time.Millisecond || d >= 1200*time.Millisecond {
			t.Fatalf("jittered delay %v left the documented ±20%% band around %v", d, base)
		}
		seen[d] = true
	}
	if len(seen) < 2 {
		t.Fatalf("the inter-send spacing lost its jitter: %d distinct value(s) over 200 draws", len(seen))
	}
}

// --- redaction: the informational simulate outcome must carry no secrets -----

// failingPlaylistRT fails the master playlist GET so Send produces a
// SimulateErr, without touching any other stage.
type failingPlaylistRT struct{}

func (failingPlaylistRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodGet {
		return nil, fmt.Errorf("dial tcp: connection refused")
	}
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// TestSimulateErrIsRedacted guards the one informational field that is written
// to the log: the raw transport error wraps the signed usher URL, whose query
// carries the playback sig and token. sendMinuteWatched logs SimulateErr at
// debug level, so an unredacted value puts a signed playback URL into logs/.
func TestSimulateErrIsRedacted(t *testing.T) {
	sender := &MinuteSender{
		client:     fakeToken{sig: "SECRETSIG", token: "SECRETTOKEN"},
		httpClient: &http.Client{Transport: failingPlaylistRT{}},
	}

	res := sender.Send(context.Background(), watchGenerationStreamer("watched"))
	if res.SimulateErr == nil {
		t.Fatal("a failed playlist fetch must still be reported as an informational simulate error")
	}
	msg := res.SimulateErr.Error()
	for _, secret := range []string{"SECRETSIG", "SECRETTOKEN", "sig=", "token=", "https://", "usher"} {
		if strings.Contains(msg, secret) {
			t.Fatalf("the logged simulate error leaked %q: %q", secret, msg)
		}
	}
	if !strings.Contains(msg, string(StagePlaylist)) {
		t.Fatalf("the redacted simulate error must still name the stage it failed at, got %q", msg)
	}
	// The beacon is unaffected: a simulation failure stays informational.
	if !res.Delivered {
		t.Fatalf("a playlist failure must remain non-fatal for the send, got %+v", res)
	}
}

// --- a cancelled refresh is a SKIP, not a fabricated Twitch failure ---------

// TestCancelledSessionRefreshIsSkippedNotFailed pins the classification: the
// drop-progress watchdog rolls a SKIPPED stage back and re-runs it once farming
// is re-confirmed, but treats any other non-success as a completed failure and
// burns a recovery stage on it. A teardown must not consume one.
func TestCancelledSessionRefreshIsSkippedNotFailed(t *testing.T) {
	ref := &blockingRefresher{entered: make(chan struct{}), exited: make(chan struct{})}
	streamer := watchGenerationStreamer("watched")
	w := &MinuteWatcher{
		streamers: []*models.Streamer{streamer},
		settings:  config.RateLimitSettings{MinuteWatchedInterval: 1},
		refresher: ref,
	}
	w.RequestSessionRefresh(SessionRefreshRequest{
		RequestID: "r1", Login: "watched", Mode: RefreshSession, Requested: time.Now(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.executeSessionRefreshes(ctx, []slotOccupant{{streamer: streamer, idx: 0}})
	}()

	select {
	case <-ref.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the session refresh never started")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(quiesceWindow):
		t.Fatal("executeSessionRefreshes did not join its cancelled worker")
	}

	out, ok := w.LastSessionRefresh("watched")
	if !ok {
		t.Fatal("the refresh outcome was not published")
	}
	if out.Success {
		t.Fatalf("a cancelled refresh must not be reported as a success: %+v", out)
	}
	if !out.Skipped {
		t.Fatalf("a cancelled refresh must be reported as SKIPPED (roll the stage back), not as a Twitch failure: %+v", out)
	}
	if out.Reason != RefreshReasonCancelled {
		t.Fatalf("a cancelled refresh must carry the cancellation reason, got %q", out.Reason)
	}
}

// TestCancelledTickDrainsNoStagedRefresh: a generation cancelled before the tick
// reaches the refresh stage must leave staged requests intact — consuming them
// would discard recovery episodes it can no longer execute.
func TestCancelledTickDrainsNoStagedRefresh(t *testing.T) {
	streamer := watchGenerationStreamer("watched")
	w := &MinuteWatcher{
		streamers: []*models.Streamer{streamer},
		settings:  config.RateLimitSettings{MinuteWatchedInterval: 1},
		refresher: &blockingRefresher{entered: make(chan struct{}), exited: make(chan struct{})},
	}
	w.RequestSessionRefresh(SessionRefreshRequest{
		RequestID: "r1", Login: "watched", Mode: RefreshSession, Requested: time.Now(),
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w.executeSessionRefreshes(ctx, []slotOccupant{{streamer: streamer, idx: 0}})

	if _, ok := w.LastSessionRefresh("watched"); ok {
		t.Fatal("a cancelled generation published a refresh outcome for work it never ran")
	}
	if pending := w.drainPendingRefreshes(); len(pending) != 1 {
		t.Fatalf("a cancelled generation consumed the staged refresh request; %d left, want 1", len(pending))
	}
}

// TestCancelledSendWithAnIncompleteSessionIsNotAFailure completes the send-path
// classification: a slot whose session is incomplete (no spade URL yet) fails
// its snapshot gate before any I/O. If that gate ran before the cancellation
// check, a teardown landing on such a slot would surface as a StageSessionSnapshot
// transport failure — fabricating a failure statistic and sending the broker off
// to re-check a dying generation's channel.
func TestCancelledSendWithAnIncompleteSessionIsNotAFailure(t *testing.T) {
	sender := &MinuteSender{
		client:     fakeToken{sig: "s", token: "t"},
		httpClient: &http.Client{Transport: newStageCountingRT("")},
	}
	// Online, but never brought fully online: no spade URL, no payload.
	streamer := models.NewStreamer("watched", models.DefaultStreamerSettings())
	streamer.SetConfirmedOnline()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := sender.Send(ctx, streamer)
	if !res.Cancelled {
		t.Fatalf("a send on a cancelled generation must report Cancelled, got %+v", res)
	}
	if res.Failure != nil {
		t.Fatalf("a teardown must not be reported as a %s transport failure", res.Failure.Stage)
	}
}

// --- the candidate-preparation gate itself ----------------------------------

// recordingSource reports whether the broker entered it.
type recordingSource struct {
	mu      sync.Mutex
	entered bool
}

func (r *recordingSource) SourceName() string { return "recording" }

func (r *recordingSource) WatchCandidates(context.Context) []Candidate {
	r.mu.Lock()
	r.entered = true
	r.mu.Unlock()
	return []Candidate{{Streamer: watchGenerationStreamer("disco"), Origin: OriginDiscovery}}
}

func (r *recordingSource) wasEntered() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.entered
}

// TestCancelledGenerationEntersNoCandidateSource pins the candidate-preparation
// gate: sources do their own re-verification I/O on the broker's loop goroutine,
// so a cancelled generation must not enter one at all.
func TestCancelledGenerationEntersNoCandidateSource(t *testing.T) {
	src := &recordingSource{}
	w := &MinuteWatcher{
		streamers: []*models.Streamer{watchGenerationStreamer("watched")},
		settings:  config.RateLimitSettings{MinuteWatchedInterval: 1},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := w.gatherCandidates(ctx, []CandidateSource{src}, nil); got != nil {
		t.Fatalf("a cancelled generation must gather no candidates, got %d", len(got))
	}
	if src.wasEntered() {
		t.Fatal("a cancelled generation entered a candidate source: preparation would run on a dead generation")
	}
}

// cancellingSource ends the generation from inside candidate preparation — the
// realistic way a teardown lands after the tick's earlier gates have passed.
type cancellingSource struct{ cancel context.CancelFunc }

func (c *cancellingSource) SourceName() string { return "cancelling" }

func (c *cancellingSource) WatchCandidates(context.Context) []Candidate {
	c.cancel()
	return []Candidate{{Streamer: watchGenerationStreamer("disco"), Origin: OriginDiscovery}}
}

// TestCancelledTickPublishesNoSnapshot pins the tick-level half of the same
// gate: a generation cancelled DURING candidate preparation must end the tick
// rather than arbitrate on a candidate set the sources never really produced and
// publish slot releases and a broker snapshot on behalf of a generation that no
// longer exists.
//
// The context is live when the tick starts on purpose: cancelling up front would
// make the earlier routine-refresh gate return first and this test would never
// reach the gate it names.
func TestCancelledTickPublishesNoSnapshot(t *testing.T) {
	snd := &recordingSender{}
	w := &MinuteWatcher{
		streamers: []*models.Streamer{watchGenerationStreamer("watched")},
		settings:  config.RateLimitSettings{MinuteWatchedInterval: 1},
		sender:    snd,
		pacer:     func(time.Duration) bool { return true },
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w.AddSource(&cancellingSource{cancel: cancel})

	w.processWatching(ctx)

	if snap := w.BrokerSnapshot(); !snap.EvaluatedAt.IsZero() || len(snap.Slots) != 0 {
		t.Fatalf("a tick cancelled during candidate preparation published a broker snapshot: %+v", snap)
	}
	if n := snd.started(); n != 0 {
		t.Fatalf("a tick cancelled during candidate preparation started %d send(s); it must start none", n)
	}
}

// staleRefresher reports a superseded apply — a real, correlated Twitch-side
// outcome that must keep its Stale classification even during a teardown.
type staleRefresher struct{}

func (staleRefresher) RefreshPlaybackSession(context.Context, *models.Streamer, bool, models.ExpectedSession) twitch.SessionRefreshResult {
	return twitch.SessionRefreshResult{Stale: true, Reason: models.SessionStaleBroadcast}
}

// TestSupersededRefreshKeepsItsStaleClassificationDuringTeardown guards the
// ordering of runSessionRefresh's cancellation arm. A stale apply always has
// Applied=false, so a cancellation arm placed ahead of it would swallow the
// supersession — and the drop-progress watchdog rebaselines an episode on Stale
// but only rolls a stage back on Skipped, so the two are not interchangeable.
func TestSupersededRefreshKeepsItsStaleClassificationDuringTeardown(t *testing.T) {
	streamer := watchGenerationStreamer("watched")
	w := &MinuteWatcher{
		streamers: []*models.Streamer{streamer},
		settings:  config.RateLimitSettings{MinuteWatchedInterval: 1},
		refresher: staleRefresher{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := &SessionRefreshOutcome{Login: "watched", Mode: RefreshSession}
	w.runSessionRefresh(ctx, out, streamer, SessionRefreshRequest{RequestID: "r1", Login: "watched", Mode: RefreshSession})

	if !out.Stale {
		t.Fatalf("a superseded apply must keep its Stale classification during a teardown, got %+v", out)
	}
	if out.Skipped {
		t.Fatalf("a superseded apply must not be relabelled as a skipped teardown: %+v", out)
	}
	if out.Reason != RefreshReasonBroadcastMoved {
		t.Fatalf("a superseded apply must keep its own reason, got %q", out.Reason)
	}
}
