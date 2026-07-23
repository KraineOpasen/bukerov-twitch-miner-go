package pubsub

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// wsTestServer is a scripted PubSub-shaped server: it counts dials, records
// LISTEN topics, answers PING with PONG, and lets each test inject
// per-connection behavior (kill the TCP stream, send a close frame, send a
// RECONNECT frame).
type wsTestServer struct {
	srv      *httptest.Server
	upgrader websocket.Upgrader

	mu      sync.Mutex
	dials   int
	listens []string

	// behavior runs on the handler goroutine for every accepted connection,
	// with its 1-based dial number, before the default serve loop.
	behavior func(ts *wsTestServer, conn *websocket.Conn, dial int)
}

func newWSTestServer(t *testing.T, behavior func(ts *wsTestServer, conn *websocket.Conn, dial int)) *wsTestServer {
	t.Helper()
	ts := &wsTestServer{behavior: behavior}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := ts.upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		ts.mu.Lock()
		ts.dials++
		dial := ts.dials
		ts.mu.Unlock()
		if ts.behavior != nil {
			ts.behavior(ts, conn, dial)
		}
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *wsTestServer) url() string {
	return "ws" + strings.TrimPrefix(ts.srv.URL, "http")
}

func (ts *wsTestServer) dialCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return ts.dials
}

func (ts *wsTestServer) listenCount() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.listens)
}

// serve is the default per-connection loop: answer PING, record LISTEN.
// Returns when the connection dies (killed by the test or by the client).
func (ts *wsTestServer) serve(conn *websocket.Conn) {
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var msg struct {
			Type string `json:"type"`
			Data struct {
				Topics []string `json:"topics"`
			} `json:"data"`
		}
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		switch msg.Type {
		case "PING":
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"PONG"}`))
		case "LISTEN":
			ts.mu.Lock()
			ts.listens = append(ts.listens, msg.Data.Topics...)
			ts.mu.Unlock()
		}
	}
}

// newTestClient builds a client pointed at the test server. pingInterval is
// huge so pingLoop stays silent; delayUnit is milliseconds so reconnect
// pacing is observable without real seconds.
func newTestClient(t *testing.T, url string, reconnectDelayMs int) *WebSocketClient {
	t.Helper()
	ws := NewWebSocketClient(0, nil, 3600, reconnectDelayMs, nil, nil)
	ws.url = url
	ws.delayUnit = time.Millisecond
	t.Cleanup(ws.Close)
	return ws
}

func waitUntil(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// lockedBuffer is a concurrency-safe io.Writer for capturing slog output
// from the client's goroutines.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestReadErrorTriggersReconnectAndResubscribe is the regression guard for
// the production deaf window (07:39:48 pattern): a socket that dies with an
// unexpected read error must reconnect within seconds — driven by the read
// error itself, not by the 5-minute PONG watchdog — and must re-LISTEN its
// topics on the new connection.
func TestReadErrorTriggersReconnectAndResubscribe(t *testing.T) {
	kill := make(chan struct{})
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		if dial == 1 {
			go func() {
				<-kill
				_ = conn.UnderlyingConn().Close() // abrupt TCP death, no close frame
			}()
		}
		ts.serve(conn)
	})

	ws := newTestClient(t, ts.url(), 0)
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	_ = ws.Listen(NewTopic(TopicVideoPlaybackByID, "123"))
	waitUntil(t, "first LISTEN to arrive", 3*time.Second, func() bool { return ts.listenCount() >= 1 })

	close(kill)

	waitUntil(t, "read-error-driven reconnect (second dial)", 5*time.Second, func() bool { return ts.dialCount() >= 2 })
	waitUntil(t, "topic re-LISTEN on the new connection", 5*time.Second, func() bool { return ts.listenCount() >= 2 })
}

// TestServerClose4100TriggersReconnect covers the 08:46:21 production case:
// Twitch closing the socket with application code 4100 ("ping pong failed")
// surfaces as a CloseError read error and must reconnect the same way.
func TestServerClose4100TriggersReconnect(t *testing.T) {
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		if dial == 1 {
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(4100, "ping pong failed"),
				time.Now().Add(time.Second))
			time.Sleep(50 * time.Millisecond)
			_ = conn.Close()
			return
		}
		ts.serve(conn)
	})

	ws := newTestClient(t, ts.url(), 0)
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	waitUntil(t, "reconnect after server close 4100", 5*time.Second, func() bool { return ts.dialCount() >= 2 })
}

// TestDeliberateCloseDoesNotReconnect: shutdown must stay silent — no
// redial after Close().
func TestDeliberateCloseDoesNotReconnect(t *testing.T) {
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		ts.serve(conn)
	})

	ws := newTestClient(t, ts.url(), 0)
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitUntil(t, "first dial", 2*time.Second, func() bool { return ts.dialCount() == 1 })

	ws.Close()
	time.Sleep(300 * time.Millisecond)

	if got := ts.dialCount(); got != 1 {
		t.Fatalf("deliberate Close must not reconnect: dials = %d, want 1", got)
	}
}

// TestDoubleTriggerSingleDial: a server RECONNECT frame immediately followed
// by the connection dying fires both the frame path and the read-error path;
// the single-flight guard must collapse them into exactly one redial.
func TestDoubleTriggerSingleDial(t *testing.T) {
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		if dial == 1 {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"RECONNECT"}`))
			_ = conn.UnderlyingConn().Close()
			return
		}
		ts.serve(conn)
	})

	// 200ms pacing keeps the winning reconnect in-flight long enough that the
	// losing trigger is guaranteed to hit the guard, making the test
	// deterministic.
	ws := newTestClient(t, ts.url(), 200)
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	waitUntil(t, "single reconnect after double trigger", 5*time.Second, func() bool { return ts.dialCount() == 2 })
	time.Sleep(500 * time.Millisecond)
	if got := ts.dialCount(); got != 2 {
		t.Fatalf("double trigger must collapse into ONE redial: dials = %d, want 2", got)
	}
}

// TestReconnectReleasesPreviousGeneration pins the lifecycle fix: after a
// reconnect, the previous generation's stop channel must be closed so its
// pingLoop terminates instead of leaking (and multiplying PINGs on the new
// connection). Asserted on the channel itself rather than on observed ping
// rates: randomPingInterval carries +/-2.5s jitter, so a rate-based assert
// would either take tens of seconds or flake, while the closed channel is
// the exact mechanism the leak hinged on.
func TestReconnectReleasesPreviousGeneration(t *testing.T) {
	kill := make(chan struct{})
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		if dial == 1 {
			go func() {
				<-kill
				_ = conn.UnderlyingConn().Close()
			}()
		}
		ts.serve(conn)
	})

	ws := newTestClient(t, ts.url(), 0)
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	ws.mu.RLock()
	firstGenStop := ws.stopChan
	ws.mu.RUnlock()

	close(kill)
	waitUntil(t, "reconnect (second dial)", 5*time.Second, func() bool { return ts.dialCount() >= 2 })

	select {
	case <-firstGenStop:
		// closed: the first generation's loops were released.
	default:
		t.Fatal("previous generation's stop channel is still open — its pingLoop leaks across the reconnect")
	}
}

// TestReconnectFrameArtifactIsNotAnError: on the deliberate RECONNECT-frame
// path, the read error produced by our own conn.Close() is an expected
// artifact and must not be logged at ERROR level (the old behavior polluted
// production logs with a misleading "WebSocket read error" after every
// server-requested reconnect).
func TestReconnectFrameArtifactIsNotAnError(t *testing.T) {
	buf := &lockedBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		if dial == 1 {
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"RECONNECT"}`))
		}
		ts.serve(conn)
	})

	ws := newTestClient(t, ts.url(), 0)
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	waitUntil(t, "reconnect after RECONNECT frame", 5*time.Second, func() bool { return ts.dialCount() >= 2 })
	time.Sleep(200 * time.Millisecond) // let the old reader drain its artifact

	logs := buf.String()
	if !strings.Contains(logs, "WebSocket reconnect requested") {
		t.Fatalf("expected the RECONNECT-frame log line, got:\n%s", logs)
	}
	if strings.Contains(logs, "WebSocket read error") {
		t.Fatalf("the deliberate-reconnect read artifact must not be an ERROR read-error line, got:\n%s", logs)
	}
}

// TestAntiFlapBoundsRedialRate: when every dial succeeds and instantly dies,
// the immediate-redial privilege must NOT apply (the link never stays up for
// a full reconnectDelay), so redials stay paced at reconnectDelay and a
// "dial succeeds -> dies instantly" loop cannot storm. Bound is generous to
// stay flake-free: ~1.5s window / 300ms pacing ≈ 6 dials expected, 12 allowed;
// the mutation that makes the read-error delay always 0 produces hundreds.
func TestAntiFlapBoundsRedialRate(t *testing.T) {
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		_ = conn.UnderlyingConn().Close() // accept and instantly kill, every time
	})

	ws := newTestClient(t, ts.url(), 300)
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	time.Sleep(1500 * time.Millisecond)
	ws.Close()

	got := ts.dialCount()
	if got < 2 {
		t.Fatalf("expected the flapping link to keep retrying (paced): dials = %d", got)
	}
	if got > 12 {
		t.Fatalf("anti-flap pacing failed: %d dials in 1.5s with a 300ms reconnectDelay (storm)", got)
	}
}
