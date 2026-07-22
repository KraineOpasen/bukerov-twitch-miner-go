package pubsub

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/events"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// countStreamerEvents returns how many events of type t were recorded for the
// given (test-unique) streamer name. The events log is process-global but tests
// run sequentially and each test below uses a unique name, so the count isolates
// that test's transitions.
func countStreamerEvents(typ events.Type, streamer string) int {
	n := 0
	for _, e := range events.Recent(200) {
		if e.Type == typ && e.Streamer == streamer {
			n++
		}
	}
	return n
}

// newCountingPool is a thread-safe pool for the concurrent boundary tests: it
// counts online/offline StatusHandler invocations with atomics.
func newCountingPool(checker streamChecker) (*WebSocketPool, *int64, *int64) {
	var online, offline int64
	p := &WebSocketPool{checker: checker}
	p.onStatusChange = func(_ string, st models.StreamerStatus) {
		switch st {
		case models.StatusOnline:
			atomic.AddInt64(&online, 1)
		case models.StatusOffline:
			atomic.AddInt64(&offline, 1)
		}
	}
	return p, &online, &offline
}

// fakeChecker is an injectable streamChecker for the video-playback handler.
type fakeChecker struct {
	mu           sync.Mutex
	updateErr    error
	updateCalls  int
	checkResult  models.StatusTransition
	checkApplies bool // when true, actually apply checkResult to the streamer
	checkCalls   int
}

func (f *fakeChecker) UpdateStream(s *models.Streamer) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCalls++
	return f.updateErr
}

func (f *fakeChecker) CheckStreamerOnline(s *models.Streamer) models.StatusTransition {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkCalls++
	if f.checkApplies {
		return s.SetConfirmedOnline()
	}
	return f.checkResult
}

func newStatusTestPool(checker streamChecker) (*WebSocketPool, *[]statusEvent) {
	var events []statusEvent
	p := &WebSocketPool{checker: checker}
	p.onStatusChange = func(name string, st models.StreamerStatus) {
		events = append(events, statusEvent{name, st})
	}
	return p, &events
}

type statusEvent struct {
	name   string
	status models.StreamerStatus
}

// TestStreamUpConfirmsOnlineTyped verifies stream-up confirms online and notifies
// the typed StatusHandler with StatusOnline (not a bool).
func TestStreamUpConfirmsOnlineTyped(t *testing.T) {
	fc := &fakeChecker{}
	p, events := newStatusTestPool(fc)
	s := models.NewStreamer("chan", models.DefaultStreamerSettings())

	p.handleVideoPlayback(&PubSubMessage{Type: "stream-up"}, s)

	if s.GetStatus() != models.StatusOnline {
		t.Fatalf("stream-up must confirm online, got %v", s.GetStatus())
	}
	if len(*events) != 1 || (*events)[0].status != models.StatusOnline {
		t.Fatalf("expected one StatusOnline notification, got %+v", *events)
	}
	if fc.updateCalls != 1 {
		t.Errorf("stream-up should attempt one metadata refresh, got %d", fc.updateCalls)
	}
}

// TestStreamUpMetadataRefreshFailureKeepsOnline covers pubsub #2: a metadata
// refresh failure after stream-up must NOT flip the streamer offline/unknown —
// the authoritative online signal is preserved.
func TestStreamUpMetadataRefreshFailureKeepsOnline(t *testing.T) {
	fc := &fakeChecker{updateErr: errors.New("boom")}
	p, _ := newStatusTestPool(fc)
	s := models.NewStreamer("chan", models.DefaultStreamerSettings())

	p.handleVideoPlayback(&PubSubMessage{Type: "stream-up"}, s)

	if s.GetStatus() != models.StatusOnline {
		t.Fatalf("a failed metadata refresh after stream-up must keep the streamer online, got %v", s.GetStatus())
	}
}

// TestDuplicateStreamUpOneEvent covers pubsub #3: a second stream-up on an
// already-online streamer is a no-op transition — no duplicate notification.
func TestDuplicateStreamUpOneEvent(t *testing.T) {
	p, events := newStatusTestPool(&fakeChecker{})
	s := models.NewStreamer("chan", models.DefaultStreamerSettings())

	p.handleVideoPlayback(&PubSubMessage{Type: "stream-up"}, s)
	p.handleVideoPlayback(&PubSubMessage{Type: "stream-up"}, s)

	if len(*events) != 1 {
		t.Fatalf("duplicate stream-up must yield exactly one notification, got %+v", *events)
	}
}

// TestStreamDownConfirmsOfflineTyped covers pubsub #4: stream-down confirms
// offline and notifies StatusOffline; it fires from online.
func TestStreamDownConfirmsOfflineTyped(t *testing.T) {
	p, events := newStatusTestPool(&fakeChecker{})
	s := models.NewStreamer("chan", models.DefaultStreamerSettings())
	s.SetConfirmedOnline()

	p.handleVideoPlayback(&PubSubMessage{Type: "stream-down"}, s)

	if s.GetStatus() != models.StatusOffline {
		t.Fatalf("stream-down must confirm offline, got %v", s.GetStatus())
	}
	if len(*events) != 1 || (*events)[0].status != models.StatusOffline {
		t.Fatalf("expected one StatusOffline notification, got %+v", *events)
	}
}

// TestStreamDownFromUnknownReleases covers pubsub #8-ish: a stream-down during an
// unknown blip is still authoritative — it settles offline.
func TestStreamDownFromUnknownReleases(t *testing.T) {
	p, events := newStatusTestPool(&fakeChecker{})
	s := models.NewStreamer("chan", models.DefaultStreamerSettings())
	s.SetConfirmedOnline()
	s.SetUnknown(models.ReasonTransportError) // online -> unknown

	p.handleVideoPlayback(&PubSubMessage{Type: "stream-down"}, s)

	if s.GetStatus() != models.StatusOffline {
		t.Fatalf("stream-down during unknown must settle offline, got %v", s.GetStatus())
	}
	if len(*events) != 1 || (*events)[0].status != models.StatusOffline {
		t.Fatalf("expected one StatusOffline notification, got %+v", *events)
	}
}

// TestDuplicateStreamDownOneEvent covers pubsub #5: a second stream-down on an
// already-offline streamer is a no-op — no duplicate notification.
func TestDuplicateStreamDownOneEvent(t *testing.T) {
	p, events := newStatusTestPool(&fakeChecker{})
	s := models.NewStreamer("chan", models.DefaultStreamerSettings())
	s.SetConfirmedOnline()

	p.handleVideoPlayback(&PubSubMessage{Type: "stream-down"}, s)
	p.handleVideoPlayback(&PubSubMessage{Type: "stream-down"}, s)

	if len(*events) != 1 {
		t.Fatalf("duplicate stream-down must yield exactly one notification, got %+v", *events)
	}
}

// TestViewcountNeverDrivesOffline covers pubsub #7: a viewcount whose re-check
// fails (returns unknown) must never notify offline and never flip offline.
func TestViewcountNeverDrivesOffline(t *testing.T) {
	// checkResult is the zero StatusTransition (no OnlineConfirmed); simulate a
	// failed check that left the streamer unknown.
	fc := &fakeChecker{checkResult: models.StatusTransition{Current: models.StatusUnknown}}
	p, events := newStatusTestPool(fc)
	s := models.NewStreamer("chan", models.DefaultStreamerSettings())
	s.SetUnknown(models.ReasonInitial) // StreamUpElapsed true (StreamUpTime zero)

	p.handleVideoPlayback(&PubSubMessage{Type: "viewcount"}, s)

	if s.GetStatus() == models.StatusOffline {
		t.Fatal("a viewcount re-check must never drive a false offline")
	}
	for _, e := range *events {
		if e.status == models.StatusOffline {
			t.Fatalf("viewcount must never notify offline, got %+v", *events)
		}
	}
}

// TestViewcountConfirmsOnlineNotifies covers pubsub #6: a viewcount re-check that
// confirms online notifies StatusOnline exactly once.
func TestViewcountConfirmsOnlineNotifies(t *testing.T) {
	fc := &fakeChecker{checkApplies: true} // the check confirms online
	p, events := newStatusTestPool(fc)
	s := models.NewStreamer("chan", models.DefaultStreamerSettings())

	p.handleVideoPlayback(&PubSubMessage{Type: "viewcount"}, s)

	if s.GetStatus() != models.StatusOnline {
		t.Fatalf("viewcount confirming online must set online, got %v", s.GetStatus())
	}
	if len(*events) != 1 || (*events)[0].status != models.StatusOnline {
		t.Fatalf("expected one StatusOnline notification, got %+v", *events)
	}
}

// TestStatusHandlerIsTyped is a compile-time-plus-runtime guard that the
// StatusHandler carries a typed models.StreamerStatus (pubsub #10), not a bool.
func TestStatusHandlerIsTyped(t *testing.T) {
	got := models.StatusUnknown
	var h StatusHandler = func(_ string, st models.StreamerStatus) { got = st }
	h("x", models.StatusOnline)
	if got != models.StatusOnline {
		t.Fatalf("typed StatusHandler did not carry the status, got %v", got)
	}
}

// --- Corrective boundary tests: the offline StatusHandler callback and the
// "went offline" log must fire ONLY on a genuine online→offline transition
// (tr.OfflineConfirmed), never for an initial-unknown→offline or an
// unknown(last-confirmed-offline)→offline first/recovery confirmation. ---

// TestStreamDownInitialUnknownNoNotify covers case 1: an initial-unknown streamer
// that receives a stream-down settles offline (with OfflineAt) but was never
// confirmed online, so no StatusOffline callback and no TypeStreamerOffline event
// fire — no misleading online→offline notification.
func TestStreamDownInitialUnknownNoNotify(t *testing.T) {
	p, cb := newStatusTestPool(&fakeChecker{})
	name := "t1-initial-unknown"
	s := models.NewStreamer(name, models.DefaultStreamerSettings()) // initial unknown

	p.handleVideoPlayback(&PubSubMessage{Type: "stream-down"}, s)

	if s.GetStatus() != models.StatusOffline {
		t.Fatalf("status = %v, want offline", s.GetStatus())
	}
	if s.GetOfflineAt().IsZero() {
		t.Error("initial-unknown → offline must stamp OfflineAt")
	}
	if len(*cb) != 0 {
		t.Fatalf("initial-unknown → offline must NOT fire the StatusHandler, got %+v", *cb)
	}
	if n := countStreamerEvents(events.TypeStreamerOffline, name); n != 0 {
		t.Fatalf("initial-unknown → offline must NOT create a TypeStreamerOffline event, got %d", n)
	}
}

// TestStreamDownFromOnlineViaUnknownNotifiesOnce covers case 2: online→unknown→
// stream-down is a genuine online→offline transition — StatusHandler fires exactly
// once with StatusOffline, one TypeStreamerOffline event, and a duplicate
// stream-down produces no second callback.
func TestStreamDownFromOnlineViaUnknownNotifiesOnce(t *testing.T) {
	p, cb := newStatusTestPool(&fakeChecker{})
	name := "t2-online-unknown"
	s := models.NewStreamer(name, models.DefaultStreamerSettings())
	s.SetConfirmedOnline()                    // (no pubsub callback: set directly)
	s.SetUnknown(models.ReasonTransportError) // online → unknown

	p.handleVideoPlayback(&PubSubMessage{Type: "stream-down"}, s)
	p.handleVideoPlayback(&PubSubMessage{Type: "stream-down"}, s) // duplicate

	if s.GetStatus() != models.StatusOffline {
		t.Fatalf("status = %v, want offline", s.GetStatus())
	}
	if len(*cb) != 1 || (*cb)[0].status != models.StatusOffline {
		t.Fatalf("expected exactly one StatusOffline notification, got %+v", *cb)
	}
	if n := countStreamerEvents(events.TypeStreamerOffline, name); n != 1 {
		t.Fatalf("expected exactly one TypeStreamerOffline event, got %d", n)
	}
}

// TestStreamDownFromOfflineViaUnknownNoNotify covers case 3: offline→unknown→
// stream-down is a first/recovery offline confirmation — the streamer was never
// confirmed online, so no callback fires again, and OfflineAt keeps its prior
// logical-offline continuity (not re-stamped by the stream-down).
func TestStreamDownFromOfflineViaUnknownNoNotify(t *testing.T) {
	p, cb := newStatusTestPool(&fakeChecker{})
	name := "t3-offline-unknown"
	s := models.NewStreamer(name, models.DefaultStreamerSettings())
	s.SetConfirmedOffline()
	originalOfflineAt := s.GetOfflineAt()
	s.SetUnknown(models.ReasonTransportError) // offline → unknown

	p.handleVideoPlayback(&PubSubMessage{Type: "stream-down"}, s)

	if s.GetStatus() != models.StatusOffline {
		t.Fatalf("status = %v, want offline", s.GetStatus())
	}
	if len(*cb) != 0 {
		t.Fatalf("offline→unknown→stream-down must NOT fire the StatusHandler, got %+v", *cb)
	}
	if s.GetOfflineAt() != originalOfflineAt {
		t.Error("OfflineAt must preserve the prior offline continuity, not be re-stamped")
	}
	if n := countStreamerEvents(events.TypeStreamerOffline, name); n != 0 {
		t.Fatalf("recovery offline confirmation must NOT create a new TypeStreamerOffline event, got %d", n)
	}
}

// TestConcurrentDuplicateStreamDownExactlyOnce covers case 4: many concurrent
// stream-down deliveries for one logical online→offline transition yield exactly
// one callback and one event. Run under -race.
func TestConcurrentDuplicateStreamDownExactlyOnce(t *testing.T) {
	p, _, offline := newCountingPool(&fakeChecker{})
	name := "t4-concurrent-down"
	s := models.NewStreamer(name, models.DefaultStreamerSettings())
	s.SetConfirmedOnline()

	const n = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			p.handleVideoPlayback(&PubSubMessage{Type: "stream-down"}, s)
		}()
	}
	close(start)
	wg.Wait()

	if got := atomic.LoadInt64(offline); got != 1 {
		t.Fatalf("concurrent duplicate stream-down: offline callback count = %d, want 1", got)
	}
	if ev := countStreamerEvents(events.TypeStreamerOffline, name); ev != 1 {
		t.Fatalf("concurrent duplicate stream-down: TypeStreamerOffline event count = %d, want 1", ev)
	}
}

// TestConcurrentTransitionsExactlyOnce covers case 5: a controlled scenario where
// concurrency within each phase must still yield exactly-once callback/event per
// logical transition. Phase 1: N concurrent stream-up on a fresh streamer → one
// online. Phase 2: N concurrent stream-down → one offline. Deterministic, -race.
func TestConcurrentTransitionsExactlyOnce(t *testing.T) {
	p, online, offline := newCountingPool(&fakeChecker{})
	name := "t5-concurrent-transitions"
	s := models.NewStreamer(name, models.DefaultStreamerSettings())

	runConcurrent := func(msgType string) {
		const n = 32
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				p.handleVideoPlayback(&PubSubMessage{Type: msgType}, s)
			}()
		}
		close(start)
		wg.Wait()
	}

	runConcurrent("stream-up")   // fresh unknown → online, exactly once
	runConcurrent("stream-down") // online → offline, exactly once

	if got := atomic.LoadInt64(online); got != 1 {
		t.Fatalf("online callback count = %d, want exactly 1", got)
	}
	if got := atomic.LoadInt64(offline); got != 1 {
		t.Fatalf("offline callback count = %d, want exactly 1", got)
	}
	if ev := countStreamerEvents(events.TypeStreamerOnline, name); ev != 1 {
		t.Fatalf("TypeStreamerOnline event count = %d, want exactly 1", ev)
	}
	if ev := countStreamerEvents(events.TypeStreamerOffline, name); ev != 1 {
		t.Fatalf("TypeStreamerOffline event count = %d, want exactly 1", ev)
	}
}
