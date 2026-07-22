package pubsub

import (
	"errors"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

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
