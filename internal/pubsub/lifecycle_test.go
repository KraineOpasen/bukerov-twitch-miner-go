package pubsub

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The pure state-machine tests (T1, T2, T3, T7, T8) drive the PONG-deadline
// primitives with explicit clock values and generation numbers — a manual clock
// — so the arm/clear/expire semantics are asserted deterministically, without a
// goroutine or a real timer.

// T1 — a scheduler delay before the PING write must never manufacture a timeout:
// with no successful write (no arm), the deadline is never expired no matter how
// far the clock advances; once armed, expiry is measured from the write time.
func TestPongDeadlineNotArmedUntilPingWritten(t *testing.T) {
	ws := &WebSocketClient{}
	ws.connGen = 1
	base := time.Unix(1000, 0)

	// No PING written yet: an arbitrarily advanced clock cannot expire a
	// deadline that was never armed (P3).
	if ws.pongDeadlineExpired(1, base.Add(time.Hour)) {
		t.Fatal("no PING written yet: pongDeadlineExpired must be false regardless of the clock")
	}

	// The write happens now; the deadline is measured from AFTER the write (P2).
	writeAt := base.Add(time.Hour)
	ws.armPongDeadline(1, writeAt.Add(ws.pongTimeoutOrDefault()), 0)
	if ws.pongDeadlineExpired(1, writeAt) {
		t.Fatal("deadline must not be expired at the instant of the write")
	}
	if ws.pongDeadlineExpired(1, writeAt.Add(ws.pongTimeoutOrDefault()-time.Nanosecond)) {
		t.Fatal("deadline must not be expired before it is reached")
	}
	if !ws.pongDeadlineExpired(1, writeAt.Add(ws.pongTimeoutOrDefault())) {
		t.Fatal("deadline must be expired once the pong timeout has elapsed since the write")
	}
}

// T2 — a successful PING arms exactly one deadline; re-arming for the same
// generation moves it rather than accumulating a second.
func TestSuccessfulPingArmsExactlyOneDeadline(t *testing.T) {
	ws := &WebSocketClient{}
	ws.connGen = 1
	base := time.Unix(2000, 0)

	ws.armPongDeadline(1, base.Add(10*time.Second), 0)
	ws.mu.RLock()
	armed, gen := ws.pongDeadlineArmed, ws.pongDeadlineGen
	ws.mu.RUnlock()
	if !armed || gen != 1 {
		t.Fatalf("after arm: armed=%v gen=%d, want armed=true gen=1", armed, gen)
	}
	if ws.pongDeadlineExpired(1, base.Add(9*time.Second)) {
		t.Fatal("must not be expired before the deadline")
	}
	if !ws.pongDeadlineExpired(1, base.Add(10*time.Second)) {
		t.Fatal("must be expired at the deadline")
	}

	// Re-arm (next cycle): the single deadline moves; the old instant no longer
	// governs.
	ws.armPongDeadline(1, base.Add(30*time.Second), 0)
	if ws.pongDeadlineExpired(1, base.Add(20*time.Second)) {
		t.Fatal("re-arm must move the single deadline forward, not leave the old one")
	}
	if !ws.pongDeadlineExpired(1, base.Add(30*time.Second)) {
		t.Fatal("re-armed deadline must expire at the new instant")
	}
}

// T3 — a matching PONG clears the current deadline and refreshes liveness.
func TestTimelyPongClearsDeadline(t *testing.T) {
	ws := &WebSocketClient{}
	ws.connGen = 1
	base := time.Unix(3000, 0)

	ws.armPongDeadline(1, base.Add(10*time.Second), 0)
	ws.recordPong(1, base.Add(5*time.Second))

	if ws.pongDeadlineExpired(1, base.Add(10*time.Second)) {
		t.Fatal("a matching PONG must clear the deadline")
	}
	if got := ws.LastPong(); !got.Equal(base.Add(5 * time.Second)) {
		t.Fatalf("lastPong = %v, want the PONG time %v", got, base.Add(5*time.Second))
	}
}

// T7 — a late PONG from a superseded generation cannot clear (or heal) the new
// generation's deadline.
func TestOldGenerationPongDoesNotClearNewDeadline(t *testing.T) {
	ws := &WebSocketClient{}
	ws.connGen = 2 // a reconnect has already advanced to generation 2
	base := time.Unix(4000, 0)

	ws.armPongDeadline(2, base.Add(10*time.Second), 0)

	// A late PONG for generation 1 (drained by the old reader) arrives.
	ws.recordPong(1, base.Add(5*time.Second))

	if !ws.pongDeadlineExpired(2, base.Add(10*time.Second)) {
		t.Fatal("a late old-generation PONG must NOT clear the new generation's deadline")
	}
	if got := ws.LastPong(); !got.IsZero() {
		t.Fatalf("lastPong must not be healed by an old-generation PONG, got %v", got)
	}

	// Arming for a superseded generation is likewise a no-op.
	ws.armPongDeadline(1, base.Add(100*time.Second), 0)
	ws.mu.RLock()
	gen := ws.pongDeadlineGen
	ws.mu.RUnlock()
	if gen != 2 {
		t.Fatalf("arming for a stale generation must not replace the current deadline (gen=%d)", gen)
	}
}

// T8 — a duplicate or unsolicited PONG must not create a deadline or otherwise
// corrupt state.
func TestUnsolicitedAndDuplicatePongAreHarmless(t *testing.T) {
	ws := &WebSocketClient{}
	ws.connGen = 1
	base := time.Unix(5000, 0)

	// Unsolicited PONG with no deadline armed: refreshes liveness, arms nothing.
	ws.recordPong(1, base.Add(time.Second))
	ws.mu.RLock()
	armed := ws.pongDeadlineArmed
	ws.mu.RUnlock()
	if armed {
		t.Fatal("an unsolicited PONG must not arm a deadline")
	}
	if ws.pongDeadlineExpired(1, base.Add(time.Hour)) {
		t.Fatal("no deadline exists, so nothing may expire")
	}

	// Duplicate PONG after a deadline was already cleared: still no re-arm.
	ws.armPongDeadline(1, base.Add(10*time.Second), ws.pongSeqSnapshot())
	ws.recordPong(1, base.Add(2*time.Second)) // clears
	ws.recordPong(1, base.Add(3*time.Second)) // duplicate
	ws.mu.RLock()
	armed = ws.pongDeadlineArmed
	ws.mu.RUnlock()
	if armed {
		t.Fatal("a duplicate PONG must not re-arm a cleared deadline")
	}
}

// T1b — a PONG processed in the window between a successful PING write and the
// deadline being armed still satisfies that ping's deadline: no false timeout,
// even when goroutine scheduling delays the arm past the PONG (the F1 window).
func TestPongInArmWindowIsNotAFalseTimeout(t *testing.T) {
	ws := &WebSocketClient{}
	ws.connGen = 1
	base := time.Unix(6500, 0)

	// The ping loop captures the PONG baseline immediately before the write.
	baseSeq := ws.pongSeqSnapshot()
	// The PONG for this ping is processed BEFORE the deadline is armed (the ping
	// goroutine was preempted between the successful write and armPongDeadline).
	ws.recordPong(1, base.Add(time.Millisecond))
	// The deadline is now armed for a ping that was already answered.
	ws.armPongDeadline(1, base.Add(10*time.Second), baseSeq)

	if ws.pongDeadlineExpired(1, base.Add(10*time.Second)) {
		t.Fatal("a PONG observed in the write->arm window must satisfy the deadline (no false timeout)")
	}
}

// pongTimeoutOrDefault returns the client's pong timeout, falling back to the
// production default for zero-valued literals used in the pure state tests.
func (ws *WebSocketClient) pongTimeoutOrDefault() time.Duration {
	if ws.pongTimeout > 0 {
		return ws.pongTimeout
	}
	return pongTimeoutDefault
}

// newPingTestClient builds a client wired to a test server with a fast,
// millisecond-scaled ping cadence and a short PONG deadline, so the deadline
// lifecycle is observable in tens of milliseconds instead of tens of seconds.
func newPingTestClient(t *testing.T, url string, reconnectDelayMs int) *WebSocketClient {
	t.Helper()
	ws := NewWebSocketClient(0, nil, 3, reconnectDelayMs, nil, nil)
	ws.url = url
	ws.delayUnit = time.Millisecond
	ws.pingUnit = time.Millisecond
	// Generous vs a healthy localhost PONG round-trip (sub-millisecond) yet tiny
	// vs the production default, so silent-server tests still time out quickly
	// while healthy-server tests do not flake when goroutine scheduling is
	// starved under the full parallel suite.
	ws.pongTimeout = 300 * time.Millisecond
	t.Cleanup(ws.Close)
	return ws
}

// silentThenNormal serves dial 1 by reading and DISCARDING every frame (so our
// PINGs succeed on the wire but no PONG ever comes back, and the read loop
// blocks), then serves every later dial normally (answering PING with PONG).
func silentThenNormal(ts *wsTestServer, conn *websocket.Conn, dial int) {
	if dial == 1 {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}
	ts.serve(conn)
}

// T4 — a missed PONG closes the deaf socket and hands control to the reconnect
// owner exactly once, surfacing a typed pong-timeout reason; the healthy second
// connection then stabilises.
func TestMissedPongReconnectsOnceWithTypedReason(t *testing.T) {
	ts := newWSTestServer(t, silentThenNormal)

	var mu sync.Mutex
	var reasons []error
	ws := newPingTestClient(t, ts.url(), 0)
	ws.onError = func(err error) {
		mu.Lock()
		reasons = append(reasons, err)
		mu.Unlock()
	}
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	waitUntil(t, "reconnect after missed PONG", 5*time.Second, func() bool { return ts.dialCount() >= 2 })
	waitUntil(t, "typed pong-timeout reason surfaced to the owner", 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, r := range reasons {
			if errors.Is(r, errPongTimeout) {
				return true
			}
		}
		return false
	})

	// The second (healthy) connection answers PONG, so the deadline is cleared
	// every cycle and the reconnect path stabilises instead of storming.
	stable := ts.dialCount()
	time.Sleep(600 * time.Millisecond)
	if got := ts.dialCount(); got != stable {
		t.Fatalf("missed PONG must stop reconnecting once the link is healthy again: dials grew %d -> %d", stable, got)
	}
}

// T5 — a PING write failure never arms a deadline; it converges to the reconnect
// owner with a typed ping-write reason instead of being silently discarded.
func TestPingWriteFailureConvergesToReconnect(t *testing.T) {
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		// Keep every connection open and silent so the READ loop blocks and
		// cannot be the trigger — only the injected ping-write failure can drive
		// recovery (a half-open socket: writes fail, reads block).
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})

	var mu sync.Mutex
	var reasons []error
	ws := newPingTestClient(t, ts.url(), 30) // paced redials
	ws.writePingHook = func() error { return errors.New("simulated write failure") }
	ws.onError = func(err error) {
		mu.Lock()
		reasons = append(reasons, err)
		mu.Unlock()
	}
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	waitUntil(t, "reconnect driven by the ping-write failure", 5*time.Second, func() bool { return ts.dialCount() >= 2 })
	waitUntil(t, "typed ping-write reason surfaced to the owner", 2*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, r := range reasons {
			if errors.Is(r, errPingWrite) {
				return true
			}
		}
		return false
	})
	ws.Close() // stop the (correctly) persistent retry once observed
}

// T6 — half-open regression: a deaf socket whose read loop would block forever
// must not leave the ping loop as the only living goroutine. When the PONG
// deadline fires, the previous generation's stop channel is closed — the exact
// mechanism that tears the read loop down — and a redial follows.
func TestHalfOpenDeadlineReleasesReadLoop(t *testing.T) {
	ts := newWSTestServer(t, silentThenNormal)

	ws := newPingTestClient(t, ts.url(), 0)
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	ws.mu.RLock()
	firstGenStop := ws.stopChan
	ws.mu.RUnlock()

	waitUntil(t, "reconnect after the deaf socket's deadline", 5*time.Second, func() bool { return ts.dialCount() >= 2 })

	select {
	case <-firstGenStop:
		// closed: the deaf generation's read AND ping loops were released.
	default:
		t.Fatal("the deaf generation's stop channel is still open — its read loop is owned forever (half-open zombie)")
	}
}

// T15 — after a deliberate shutdown there must be no reconnect, no false
// timeout, and a second Close must be safe (no double-close panic).
func TestShutdownDuringActivePingLoopIsClean(t *testing.T) {
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		ts.serve(conn) // healthy: answers PONG
	})

	ws := newPingTestClient(t, ts.url(), 0)
	// A very generous PONG deadline vs a healthy localhost round-trip: the ping
	// loop is genuinely active (blocked in its pong-wait), but no spurious
	// timeout can fire, so any reconnect here would be a real shutdown bug.
	ws.pongTimeout = time.Second
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	waitUntil(t, "first dial", 2*time.Second, func() bool { return ts.dialCount() == 1 })
	time.Sleep(50 * time.Millisecond) // the ping loop is now active (in its pong-wait)

	ws.Close()
	ws.Close() // idempotent: must not panic on a double close
	time.Sleep(300 * time.Millisecond)

	if got := ts.dialCount(); got != 1 {
		t.Fatalf("deliberate shutdown must not reconnect: dials = %d, want 1", got)
	}
}

// T16 — the deadline/generation state stays consistent under concurrent PONGs,
// deadline arming, expiry checks, RESPONSE handling and generation swaps. Run
// under -race this is the data-race guard for the new lifecycle state.
func TestPingPongLifecycleStateRace(t *testing.T) {
	ws := &WebSocketClient{}
	ws.connGen = 1
	ws.pongTimeout = time.Millisecond

	const iters = 500
	var wg sync.WaitGroup
	var gen uint64 = 1

	start := time.Unix(6000, 0)
	wg.Add(6)

	go func() { // arm deadlines
		defer wg.Done()
		for i := 0; i < iters; i++ {
			ws.armPongDeadline(atomic.LoadUint64(&gen), start.Add(time.Duration(i)*time.Millisecond), 0)
		}
	}()
	go func() { // matching PONGs
		defer wg.Done()
		for i := 0; i < iters; i++ {
			ws.recordPong(atomic.LoadUint64(&gen), start.Add(time.Duration(i)*time.Millisecond))
		}
	}()
	go func() { // stale-generation PONGs
		defer wg.Done()
		for i := 0; i < iters; i++ {
			ws.recordPong(0, start.Add(time.Duration(i)*time.Millisecond))
		}
	}()
	go func() { // expiry checks
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = ws.pongDeadlineExpired(atomic.LoadUint64(&gen), start.Add(time.Duration(i)*time.Millisecond))
		}
	}()
	go func() { // RESPONSE + PONG frames through the public handler
		defer wg.Done()
		for i := 0; i < iters; i++ {
			ws.handleMessage(WSMessage{Type: "PONG"})
			ws.handleMessage(WSMessage{Type: "RESPONSE", Nonce: "n", Error: "ERR_SERVER"})
		}
	}()
	go func() { // generation swaps (reconnect), under the same lock reconnect uses
		defer wg.Done()
		for i := 0; i < iters; i++ {
			ws.mu.Lock()
			ws.connGen++
			ws.mu.Unlock()
			atomic.StoreUint64(&gen, func() uint64 { ws.mu.RLock(); defer ws.mu.RUnlock(); return ws.connGen }())
		}
	}()

	wg.Wait()
}
