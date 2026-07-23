package pubsub

import (
	"errors"
	"sync/atomic"
	"testing"
)

// C9.1: a failed post-rotation user-topic re-LISTEN produces exactly ONE
// explicit reconnect request — not just a log line.
func TestRelistenWriteFailureRequestsOneReconnect(t *testing.T) {
	provider := newRotatingAuth(AuthSnapshot{Token: "tok-1", Generation: 1})
	rec := &authRotRecorder{}
	ws := newAuthSweepClient(0, provider.provider, rec)

	var reconnects atomic.Int64
	ws.reconnectRequestHook = func() { reconnects.Add(1) }

	for _, id := range []string{"1", "2"} {
		if err := ws.Listen(NewTopic(TopicCommunityPointsUser, id)); err != nil {
			t.Fatalf("listen: %v", err)
		}
	}
	rec.setFail(errors.New("injected write failure"))

	ws.RelistenUserTopics()

	if got := reconnects.Load(); got != 1 {
		t.Fatalf("reconnect requests = %d, want exactly 1 (failed sweep must have an explicit recovery path)", got)
	}
	// C9.4/C9.6: the desired-topic ledger is untouched by the failed sweep.
	for _, id := range []string{"1", "2"} {
		if !ws.HasTopicApplied(NewTopic(TopicCommunityPointsUser, id)) {
			t.Fatalf("failed sweep lost a desired user topic from the ledger")
		}
	}
}

// C9.2/C9.9: a reconnect already in flight collapses the request, and
// shutdown suppresses it entirely.
func TestReconnectRequestCollapsedAndShutdownSuppressed(t *testing.T) {
	provider := newRotatingAuth(AuthSnapshot{Token: "tok-1", Generation: 1})
	rec := &authRotRecorder{}
	ws := newAuthSweepClient(0, provider.provider, rec)
	var reconnects atomic.Int64
	ws.reconnectRequestHook = func() { reconnects.Add(1) }

	ws.mu.Lock()
	ws.isReconnecting = true
	ws.mu.Unlock()
	ws.requestAuthReconnect()
	if reconnects.Load() != 0 {
		t.Fatalf("request not collapsed while a reconnect is already in flight")
	}

	ws.mu.Lock()
	ws.isReconnecting = false
	ws.forcedClose = true
	ws.mu.Unlock()
	ws.requestAuthReconnect()
	if reconnects.Load() != 0 {
		t.Fatalf("request not suppressed after shutdown")
	}

	ws.mu.Lock()
	ws.forcedClose = false
	ws.mu.Unlock()
	ws.requestAuthReconnect()
	if reconnects.Load() != 1 {
		t.Fatalf("healthy request should pass through exactly once, got %d", reconnects.Load())
	}
}

// C9.3: the reconnect replay path writes the parked topics with the CURRENT
// (rotated) credential snapshot.
func TestReconnectReplayAfterFailureUsesRotatedGeneration(t *testing.T) {
	provider := newRotatingAuth(AuthSnapshot{Token: "tok-1", Generation: 1})
	rec := &authRotRecorder{}
	ws := newAuthSweepClient(0, provider.provider, rec)
	ws.reconnectRequestHook = func() {}

	user := NewTopic(TopicCommunityPointsUser, "42")
	if err := ws.Listen(user); err != nil {
		t.Fatalf("listen: %v", err)
	}

	// Rotation happens, then the sweep's write fails.
	provider.rotate(AuthSnapshot{Token: "tok-2", Generation: 2})
	rec.setFail(errors.New("injected write failure"))
	ws.RelistenUserTopics()

	// Simulate the requested reconnect's generation swap + replay, the same
	// way the reconnect tests do: applied topics park, the swap bumps the
	// generation, the replay re-LISTENs with the current provider snapshot.
	rec.setFail(nil)
	ws.mu.Lock()
	ws.pendingTopics = append([]Topic{}, ws.topics...)
	ws.topics = nil
	ws.connGen++
	ws.mu.Unlock()
	ws.replayPendingTopics()

	if got := ws.lastAuthGeneration(); got != 2 {
		t.Fatalf("replay after failed sweep presented generation %d, want the rotated 2", got)
	}
	if !ws.HasTopicApplied(user) {
		t.Fatalf("replay did not restore the desired user topic")
	}
}

// C9.7: a fully successful sweep requests no reconnect.
func TestSuccessfulRelistenDoesNotReconnect(t *testing.T) {
	provider := newRotatingAuth(AuthSnapshot{Token: "tok-1", Generation: 1})
	rec := &authRotRecorder{}
	ws := newAuthSweepClient(0, provider.provider, rec)
	var reconnects atomic.Int64
	ws.reconnectRequestHook = func() { reconnects.Add(1) }

	if err := ws.Listen(NewTopic(TopicCommunityPointsUser, "42")); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ws.RelistenUserTopics()
	if reconnects.Load() != 0 {
		t.Fatalf("healthy sweep requested a reconnect")
	}
}

// C9.8/C9.5: one failing client does not deprive the other pool clients of
// their re-authorization, and channel topics stay untouched throughout.
func TestOneFailingClientDoesNotBlockPoolSweep(t *testing.T) {
	provider := AuthTokenProvider(func() AuthSnapshot { return AuthSnapshot{Token: "tok", Generation: 1} })

	recA, recB := &authRotRecorder{}, &authRotRecorder{}
	wsA := newAuthSweepClient(0, provider, recA)
	wsB := newAuthSweepClient(1, provider, recB)
	var reconnectsA atomic.Int64
	wsA.reconnectRequestHook = func() { reconnectsA.Add(1) }
	wsB.reconnectRequestHook = func() {}

	if err := wsA.Listen(NewTopic(TopicCommunityPointsUser, "1")); err != nil {
		t.Fatalf("listen A: %v", err)
	}
	if err := wsB.Listen(NewTopic(TopicPredictionsUser, "1")); err != nil {
		t.Fatalf("listen B: %v", err)
	}
	channel := NewTopic(TopicVideoPlaybackByID, "chan")
	if err := wsB.Listen(channel); err != nil {
		t.Fatalf("listen channel: %v", err)
	}
	baselineB := recB.frameCount()

	p := &WebSocketPool{clients: []*WebSocketClient{wsA, wsB}}
	recA.setFail(errors.New("injected write failure"))
	p.ReauthorizeUserTopics()

	if reconnectsA.Load() != 1 {
		t.Fatalf("failing client A did not request its reconnect")
	}
	if got := recB.frameCount() - baselineB; got != 1 {
		t.Fatalf("client B re-authorizations = %d, want 1 (sweep must continue past A's failure)", got)
	}
	// Channel topics carry no auth_token and are never re-LISTENed by the sweep.
	frames := recB.framesSince(baselineB)
	for _, fr := range frames {
		if fr == "LISTEN "+channel.String() {
			t.Fatalf("sweep touched a channel topic")
		}
	}
}
