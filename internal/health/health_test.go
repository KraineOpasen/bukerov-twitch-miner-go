package health

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/watcher"
)

// --- fakes ---

type fakeClient struct {
	channelID string
	idErr     error
	online    bool
	spade     string
	checks    int32

	// Gates simulate a context-unaware call that hangs at the network level: when
	// non-nil the method blocks until the gate is closed. The matching done
	// channel is closed when the (possibly abandoned) call finally returns, so a
	// test can join the detached goroutine and keep the suite race-clean.
	idGate, idDone         chan struct{}
	onlineGate, onlineDone chan struct{}
}

func (f *fakeClient) GetChannelID(string) (string, error) {
	if f.idGate != nil {
		<-f.idGate
	}
	if f.idDone != nil {
		close(f.idDone)
	}
	return f.channelID, f.idErr
}

func (f *fakeClient) CheckStreamerOnline(s *models.Streamer) {
	if f.onlineGate != nil {
		<-f.onlineGate
	}
	atomic.AddInt32(&f.checks, 1)
	if f.online {
		s.SetOnline()
		s.Stream.SpadeURL = f.spade
	} else {
		s.SetOffline()
	}
	if f.onlineDone != nil {
		close(f.onlineDone)
	}
}

type fakeProber struct {
	res     watcher.ProbeResult
	calls   int32
	block   chan struct{} // if non-nil, Probe waits on it
	entered chan struct{} // signaled once Probe is entered
	waitCtx bool          // if true, Probe waits for ctx.Done
}

func (f *fakeProber) Probe(ctx context.Context, _ *models.Streamer) watcher.ProbeResult {
	atomic.AddInt32(&f.calls, 1)
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.waitCtx {
		<-ctx.Done()
		return watcher.ProbeResult{Stage: watcher.StageBeacon, ErrorCode: "beacon_timeout"}
	}
	if f.block != nil {
		<-f.block
	}
	return f.res
}

type fakeNotifier struct {
	mu    sync.Mutex
	calls []transition
}

type transition struct {
	signal  string
	healthy bool
	detail  string
}

func (f *fakeNotifier) NotifyHealthTransition(signal string, healthy bool, detail string) {
	f.mu.Lock()
	f.calls = append(f.calls, transition{signal, healthy, detail})
	f.mu.Unlock()
}

func (f *fakeNotifier) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

type fakeSlots struct{ free int }

func (f fakeSlots) FreeSlots() int { return f.free }

func onlineClient() *fakeClient {
	return &fakeClient{channelID: "cid", online: true, spade: "http://spade.test/track"}
}

func newCanary(center *Center, client TwitchClient, prober Prober, notif Notifier, slots SlotView) *Canary {
	c := NewCanary(center, client, prober, notif, slots, CanaryConfig{
		Enabled:      true,
		Channel:      "canary_chan",
		Interval:     time.Hour,
		MaxStaleness: 48 * time.Hour,
	})
	c.ctx = context.Background()
	return c
}

// --- Center ---

func TestCenterRecordSnapshotOrderAndClientID(t *testing.T) {
	c := NewCenter()
	c.Record(Signal{Name: SignalPubSub, Status: StatusOK})
	c.Record(Signal{Name: SignalOAuth, Status: StatusOK})
	c.SetActiveClientID("TV")

	snap := c.Snapshot()
	if snap.ActiveClientID != "TV" {
		t.Errorf("expected active client TV, got %q", snap.ActiveClientID)
	}
	if len(snap.Signals) != 2 || snap.Signals[0].Name != SignalOAuth || snap.Signals[1].Name != SignalPubSub {
		t.Fatalf("expected signals in stable order [oauth, pubsub], got %+v", snap.Signals)
	}
}

func TestCenterSnapshotImmutable(t *testing.T) {
	c := NewCenter()
	c.Record(Signal{Name: SignalGQLAPI, Status: StatusOK})
	snap := c.Snapshot()
	snap.Signals[0].Status = StatusFailed // mutate the returned copy
	if again, _ := c.Snapshot().Signal(SignalGQLAPI); again.Status != StatusOK {
		t.Error("mutating a returned snapshot must not affect the center")
	}
}

// --- Canary probe ---

func TestCanaryProbeSuccess(t *testing.T) {
	center := NewCenter()
	c := newCanary(center, onlineClient(), &fakeProber{res: watcher.ProbeResult{OK: true}}, &fakeNotifier{}, nil)
	sig := c.probe(context.Background(), "canary_chan")
	if sig.Status != StatusOK || sig.Name != SignalWatchTransport {
		t.Fatalf("expected watch_transport OK, got %+v", sig)
	}
}

func TestCanaryChannelOffline(t *testing.T) {
	client := &fakeClient{channelID: "cid", online: false}
	c := newCanary(NewCenter(), client, &fakeProber{}, &fakeNotifier{}, nil)
	sig := c.probe(context.Background(), "canary_chan")
	if sig.Status != StatusFailed || sig.Stage != "stream_info" {
		t.Fatalf("expected stream_info failure for an offline channel, got %+v", sig)
	}
}

func TestCanarySpadeMissing(t *testing.T) {
	client := &fakeClient{channelID: "cid", online: true, spade: ""} // online but no spade URL
	c := newCanary(NewCenter(), client, &fakeProber{}, &fakeNotifier{}, nil)
	sig := c.probe(context.Background(), "canary_chan")
	if sig.Status != StatusFailed || sig.Stage != "spade_url" {
		t.Fatalf("expected spade_url failure, got %+v", sig)
	}
}

func TestCanaryChannelResolveError(t *testing.T) {
	client := &fakeClient{idErr: context.DeadlineExceeded}
	c := newCanary(NewCenter(), client, &fakeProber{}, &fakeNotifier{}, nil)
	sig := c.probe(context.Background(), "canary_chan")
	if sig.Status != StatusFailed || sig.Stage != "stream_info" {
		t.Fatalf("expected stream_info failure when the channel id cannot resolve, got %+v", sig)
	}
}

func TestCanaryProbeStageFailure(t *testing.T) {
	prober := &fakeProber{res: watcher.ProbeResult{Stage: watcher.StagePlaylist, Status: 403, ErrorCode: "playlist_http_403"}}
	c := newCanary(NewCenter(), onlineClient(), prober, &fakeNotifier{}, nil)
	sig := c.probe(context.Background(), "canary_chan")
	if sig.Status != StatusFailed || sig.Stage != string(watcher.StagePlaylist) || sig.ErrorCode != "playlist_http_403" {
		t.Fatalf("expected playlist failure propagated, got %+v", sig)
	}
}

func TestCanaryContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := newCanary(NewCenter(), onlineClient(), &fakeProber{res: watcher.ProbeResult{OK: true}}, &fakeNotifier{}, nil)
	sig := c.probe(ctx, "canary_chan")
	if sig.Status != StatusFailed {
		t.Fatalf("expected a cancelled probe to fail, got %+v", sig)
	}
}

func TestCanaryBeaconTimeout(t *testing.T) {
	prober := &fakeProber{waitCtx: true}
	c := newCanary(NewCenter(), onlineClient(), prober, &fakeNotifier{}, nil)
	c.timeout = 20 * time.Millisecond

	c.runOnce(true)

	sig, ok := c.center.Signal(SignalWatchTransport)
	if !ok || sig.Status != StatusFailed {
		t.Fatalf("expected a timed-out probe to record a failure, got ok=%v sig=%+v", ok, sig)
	}
}

func TestCanaryDuplicateRunSuppression(t *testing.T) {
	prober := &fakeProber{
		res:     watcher.ProbeResult{OK: true},
		block:   make(chan struct{}),
		entered: make(chan struct{}, 1),
	}
	c := newCanary(NewCenter(), onlineClient(), prober, &fakeNotifier{}, nil)

	go c.runOnce(true) // winner: enters Probe and blocks, holding running=true
	<-prober.entered

	// Every other run while the winner is in flight must be suppressed.
	for i := 0; i < 4; i++ {
		c.runOnce(true)
	}
	if n := atomic.LoadInt32(&prober.calls); n != 1 {
		t.Fatalf("expected exactly one probe while a run is in flight, got %d", n)
	}
	close(prober.block)
}

// TestCanaryNotifiesOnlyOnTransition drives the transition detector through a
// healthy -> healthy -> failed -> failed -> recovered sequence.
func TestCanaryNotifiesOnlyOnTransition(t *testing.T) {
	notif := &fakeNotifier{}
	c := newCanary(NewCenter(), onlineClient(), &fakeProber{}, notif, nil)

	c.handleTransition(Signal{Status: StatusOK})     // baseline healthy -> healthy: no notify
	c.handleTransition(Signal{Status: StatusFailed}) // -> failed: notify degraded
	c.handleTransition(Signal{Status: StatusFailed}) // repeated failure: no notify
	c.handleTransition(Signal{Status: StatusOK})     // -> recovered: notify

	if notif.count() != 2 {
		t.Fatalf("expected exactly 2 transition notifications, got %d (%+v)", notif.count(), notif.calls)
	}
	if notif.calls[0].healthy || !notif.calls[1].healthy {
		t.Errorf("expected [degraded, recovered], got %+v", notif.calls)
	}
}

// TestCanaryRecordedSignalRedacted guards that a recorded failure carries no
// token, signed URL, or sig/token query params.
func TestCanaryRecordedSignalRedacted(t *testing.T) {
	prober := &fakeProber{res: watcher.ProbeResult{Stage: watcher.StagePlaylist, Status: 403, ErrorCode: "playlist_http_403"}}
	center := NewCenter()
	c := newCanary(center, onlineClient(), prober, &fakeNotifier{}, nil)
	c.runOnce(true)

	sig, _ := center.Signal(SignalWatchTransport)
	blob := sig.Detail + " " + sig.ErrorCode + " " + sig.Stage
	for _, secret := range []string{"http://", "https://", "sig=", "token=", "spade.test"} {
		if strings.Contains(blob, secret) {
			t.Fatalf("recorded signal leaked %q: %q", secret, blob)
		}
	}
}

// probeWithin runs c.probe under a ctxTimeout deadline on a separate goroutine
// and fails if probe does not return within hardLimit — i.e. it hung waiting on
// a context-unaware client call instead of honoring the deadline.
func probeWithin(t *testing.T, c *Canary, ctxTimeout, hardLimit time.Duration) Signal {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), ctxTimeout)
	defer cancel()
	res := make(chan Signal, 1)
	go func() { res <- c.probe(ctx, "canary_chan") }()
	select {
	case sig := <-res:
		return sig
	case <-time.After(hardLimit):
		t.Fatalf("probe did not return within %v — it hung on a blocking client call", hardLimit)
		return Signal{}
	}
}

// TestCanaryProbeTimesOutWhenClientBlocks proves the watchdog: even though
// GetChannelID / CheckStreamerOnline are context-unaware and hang indefinitely,
// probe() still returns on its deadline (abandoning the detached goroutine), and
// a timed-out CheckStreamerOnline invalidates the cached streamer so the leaked
// writer never shares it with a later probe. Each subtest joins the detached
// goroutine (close the gate, wait on done) to keep the run race-clean.
func TestCanaryProbeTimesOutWhenClientBlocks(t *testing.T) {
	t.Run("GetChannelID hangs", func(t *testing.T) {
		gate, done := make(chan struct{}), make(chan struct{})
		client := &fakeClient{channelID: "cid", online: true, spade: "http://spade.test/x", idGate: gate, idDone: done}
		c := newCanary(NewCenter(), client, &fakeProber{res: watcher.ProbeResult{OK: true}}, &fakeNotifier{}, nil)
		t.Cleanup(func() { close(gate); <-done })

		sig := probeWithin(t, c, 20*time.Millisecond, 2*time.Second)
		if sig.Status != StatusFailed || sig.Stage != "stream_info" || sig.ErrorCode != "timeout" {
			t.Fatalf("expected a stream_info timeout, got %+v", sig)
		}
	})

	t.Run("CheckStreamerOnline hangs", func(t *testing.T) {
		gate, done := make(chan struct{}), make(chan struct{})
		client := &fakeClient{channelID: "cid", online: true, spade: "http://spade.test/x", onlineGate: gate, onlineDone: done}
		c := newCanary(NewCenter(), client, &fakeProber{res: watcher.ProbeResult{OK: true}}, &fakeNotifier{}, nil)
		t.Cleanup(func() { close(gate); <-done })

		sig := probeWithin(t, c, 20*time.Millisecond, 2*time.Second)
		if sig.Status != StatusFailed || sig.Stage != "stream_info" || sig.ErrorCode != "timeout" {
			t.Fatalf("expected a stream_info timeout, got %+v", sig)
		}
		c.mu.Lock()
		cached := c.streamer
		c.mu.Unlock()
		if cached != nil {
			t.Error("expected the cached streamer to be dropped after a CheckStreamerOnline timeout")
		}
	})
}

// --- scheduling (opportunistic vs forced) ---

func TestCanaryMaybeRunScheduling(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	newAt := func(lastCheck time.Time, free int) (*Canary, *fakeProber) {
		center := NewCenter()
		if !lastCheck.IsZero() {
			center.Record(Signal{Name: SignalWatchTransport, Status: StatusOK, CheckedAt: lastCheck})
		}
		prober := &fakeProber{res: watcher.ProbeResult{OK: true}}
		c := newCanary(center, onlineClient(), prober, &fakeNotifier{}, fakeSlots{free: free})
		c.now = func() time.Time { return base }
		return c, prober
	}

	// Due (2h ago >= 1h) AND a slot is free -> runs opportunistically.
	c, p := newAt(base.Add(-2*time.Hour), 1)
	c.maybeRun()
	if atomic.LoadInt32(&p.calls) != 1 {
		t.Errorf("due + free slot: expected a probe, got %d", p.calls)
	}

	// Due but no free slot and not yet stale -> skipped.
	c, p = newAt(base.Add(-2*time.Hour), 0)
	c.maybeRun()
	if atomic.LoadInt32(&p.calls) != 0 {
		t.Errorf("due + busy slots + not stale: expected no probe, got %d", p.calls)
	}

	// Past max staleness (50h > 48h) with no free slot -> forced.
	c, p = newAt(base.Add(-50*time.Hour), 0)
	c.maybeRun()
	if atomic.LoadInt32(&p.calls) != 1 {
		t.Errorf("stale + busy slots: expected a forced probe, got %d", p.calls)
	}

	// Not due yet (30m < 1h) with a free slot -> skipped.
	c, p = newAt(base.Add(-30*time.Minute), 1)
	c.maybeRun()
	if atomic.LoadInt32(&p.calls) != 0 {
		t.Errorf("not due: expected no probe, got %d", p.calls)
	}
}

// TestCanaryManualRunIgnoresDisabled confirms a manual run probes even when the
// scheduled canary is disabled, as long as a channel is set.
func TestCanaryManualRunIgnoresDisabled(t *testing.T) {
	prober := &fakeProber{res: watcher.ProbeResult{OK: true}}
	c := newCanary(NewCenter(), onlineClient(), prober, &fakeNotifier{}, nil)
	c.UpdateSettings(CanaryConfig{Enabled: false, Channel: "canary_chan", Interval: time.Hour, MaxStaleness: 48 * time.Hour})

	c.runOnce(true) // manual
	if atomic.LoadInt32(&prober.calls) != 1 {
		t.Errorf("manual run must probe even when disabled, got %d calls", prober.calls)
	}

	// A scheduled run while disabled must NOT probe.
	prober2 := &fakeProber{res: watcher.ProbeResult{OK: true}}
	c2 := newCanary(NewCenter(), onlineClient(), prober2, &fakeNotifier{}, nil)
	c2.UpdateSettings(CanaryConfig{Enabled: false, Channel: "canary_chan", Interval: time.Hour, MaxStaleness: 48 * time.Hour})
	c2.runOnce(false)
	if atomic.LoadInt32(&prober2.calls) != 0 {
		t.Errorf("scheduled run while disabled must not probe, got %d", prober2.calls)
	}
}
