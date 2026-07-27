package pubsub

import (
	"errors"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// pointsEarnedFrame is a minimal wire MESSAGE the read loop parses into a
// PubSubMessage and hands to onMessage — the same dispatch path the miner's
// analytics/notification writers hang off in production.
const pointsEarnedFrame = `{"type":"MESSAGE","data":{"topic":"community-points-user-v1.123","message":"{\"type\":\"points-earned\",\"data\":{}}"}}`

// newHandlerClient builds a client against the test server whose onMessage
// blocks until the test releases it, so "a message handler is in flight" is a
// deterministic, channel-synchronized state.
func newHandlerClient(t *testing.T, url string, entered chan struct{}, release chan struct{}, handlerDone chan struct{}) *WebSocketClient {
	t.Helper()
	ws := NewWebSocketClient(0, nil, 3600, 0, func(*PubSubMessage) {
		entered <- struct{}{}
		<-release
		close(handlerDone)
	}, nil)
	ws.url = url
	ws.delayUnit = time.Millisecond
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

// TestCloseJoinsInFlightMessageHandler (S1): Close must not return while the
// read loop is still inside a message handler — the handler is what performs
// analytics writes and schedules notification dispatches, so Close returning
// early is what let those writes race the database close. onMessage runs on
// the read-loop goroutine, so handlerDone being closed the moment Close
// returns proves the join, deterministically.
func TestCloseJoinsInFlightMessageHandler(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	handlerDone := make(chan struct{})

	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(pointsEarnedFrame))
		ts.serve(conn)
	})

	ws := newHandlerClient(t, ts.url(), entered, release, handlerDone)
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	<-entered // the read loop is inside the handler

	closeDone := make(chan struct{})
	go func() {
		_ = ws.Close()
		close(closeDone)
	}()

	// Negative assertion (stop_join_test precedent): a joining Close cannot
	// return while the handler is still blocked.
	select {
	case <-closeDone:
		t.Fatal("Close returned while a message handler was still in flight")
	case <-time.After(300 * time.Millisecond):
	}

	close(release)

	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the handler finished")
	}
	select {
	case <-handlerDone:
	default:
		t.Fatal("Close returned before the in-flight message handler completed")
	}
}

// TestCloseReturnsDespiteWedgedHandler (S1): the join is bounded — a handler
// that never returns cannot hang shutdown past stopJoinTimeout.
func TestCloseReturnsDespiteWedgedHandler(t *testing.T) {
	old := stopJoinTimeout
	stopJoinTimeout = 100 * time.Millisecond
	defer func() { stopJoinTimeout = old }()

	entered := make(chan struct{}, 1)
	release := make(chan struct{}) // never closed until cleanup: wedged handler
	handlerDone := make(chan struct{})

	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(pointsEarnedFrame))
		ts.serve(conn)
	})

	ws := newHandlerClient(t, ts.url(), entered, release, handlerDone)
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	<-entered
	defer close(release) // let the wedged goroutine exit after the test

	start := time.Now()
	done := make(chan struct{})
	go func() {
		_ = ws.Close()
		close(done)
	}()

	select {
	case <-done:
		if elapsed := time.Since(start); elapsed < stopJoinTimeout {
			t.Fatalf("Close returned before the join timeout (%v) — did it wait at all?", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked far beyond the join timeout — wedged-handler protection missing")
	}
}

// TestConnectAfterCloseIsRefused (S1): a closed client must never spawn new
// read/ping loops — a reconnect generation born after Close would outlive the
// shutdown drain. Connect must refuse with ErrClientClosed (which
// reconnectAfter treats as terminal).
func TestConnectAfterCloseIsRefused(t *testing.T) {
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		ts.serve(conn)
	})

	ws := newTestClient(t, ts.url(), 0)
	_ = ws.Close()

	if err := ws.Connect(); !errors.Is(err, ErrClientClosed) {
		t.Fatalf("Connect after Close = %v, want ErrClientClosed", err)
	}
}

// TestPoolCloseJoinsHandlerWithoutDeadlock (S1): Pool.Close must release the
// pool lock before joining each connection's loops. A message handler
// routinely takes the pool lock (findStreamer, prediction bookkeeping);
// joining it while holding p.mu would deadlock until the join timeout. The
// handler here re-enters the pool lock after release, so the test hangs (and
// fails on its bound) if Close ever joins under p.mu again.
func TestPoolCloseJoinsHandlerWithoutDeadlock(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	handlerDone := make(chan struct{})

	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		_ = conn.WriteMessage(websocket.TextMessage, []byte(pointsEarnedFrame))
		ts.serve(conn)
	})

	p := &WebSocketPool{}
	ws := NewWebSocketClient(0, nil, 3600, 0, func(*PubSubMessage) {
		entered <- struct{}{}
		<-release
		// The pool lock is what the old Close held across the join.
		p.UpdateStreamers(nil)
		close(handlerDone)
	}, nil)
	ws.url = ts.url()
	ws.delayUnit = time.Millisecond
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	p.clients = []*WebSocketClient{ws}

	<-entered

	closeDone := make(chan struct{})
	go func() {
		_ = p.Close()
		close(closeDone)
	}()
	close(release)

	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Pool.Close did not return — join is deadlocking against a pool-lock-taking handler")
	}
	select {
	case <-handlerDone:
	default:
		t.Fatal("Pool.Close returned before the in-flight handler completed")
	}

	// Repeated close stays a defined no-op.
	_ = p.Close()

	// A closed pool must not dial fresh connections.
	if err := p.Submit(NewTopic(TopicVideoPlaybackByID, "123")); !errors.Is(err, ErrPoolClosed) {
		t.Fatalf("Submit after Close = %v, want ErrPoolClosed", err)
	}
}
