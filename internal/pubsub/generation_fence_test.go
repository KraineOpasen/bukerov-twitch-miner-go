package pubsub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// B1 — connection-generation fencing of ordinary MESSAGE frames.
//
// Transport invariant under test:
//
//	readerGen != currentGen  =>  zero ordinary MESSAGE downstream dispatch
//
// and its corollary: a frame drained by a superseded generation must never
// touch the 1-second EventFingerprint replay window, or a stale copy could
// suppress the same event's legitimate delivery on the live generation.
//
// Seams (pre-agreed by the task contract): the package-private
// handleMessageForGen(msg, readerGen) — the exact entry point the real read
// loop calls with the generation it captured in Connect — the real
// reconnectAfter swap for supersession, and the local wsTestServer harness for
// the end-to-end run through a real read loop. The end-to-end test additionally
// uses the process-wide slog default as a scheduling barrier on the
// "WebSocket received" MESSAGE debug record handleMessageForGen emits before
// its dispatch decision; that wait is bounded and names the record if it is
// ever reworded. The offline tests reach the fence without a network by
// establishing and advancing connGen under ws.mu exactly as Connect and the
// reconnect swap do (package precedent: lifecycle_test.go, invalid_topic_test.go),
// and one diagnostic peeks at the replay window's size next to the behavioral
// proof of the same property. The churn test's linearization witness also
// reads the window under ws.mu, in the critical section that first observes a
// round's generation retired: that instant is the earliest observation point
// separating an admission ordered before the swap from one ordered after it
// (the lock-free callback cannot tell a late-running legitimate dispatch from
// a stale one), so it is what makes a check-then-act fence — one that checks
// the generation and admits the fingerprint in separate critical sections —
// visible to the suite at all.

const genFenceTopic = "community-points-user-v1.123"

// genFenceInner returns a distinct points-earned payload per n, so every n has
// its own EventFingerprint while a repeated n is an exact replay.
func genFenceInner(n int) string {
	return fmt.Sprintf(`{"type":"points-earned","data":{"channel_id":"123","point_gain":{"reason_code":"WATCH","total_points":%d}}}`, n)
}

func genFenceFrame(n int) WSMessage {
	return WSMessage{Type: "MESSAGE", Data: &WSData{Topic: genFenceTopic, Message: genFenceInner(n)}}
}

// genFenceWire is the on-the-wire JSON of genFenceFrame(n).
func genFenceWire(t *testing.T, n int) []byte {
	t.Helper()
	b, err := json.Marshal(genFenceFrame(n))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func genFencePayloadN(m *PubSubMessage) int {
	pg, _ := m.Data["point_gain"].(map[string]interface{})
	f, _ := pg["total_points"].(float64)
	return int(f)
}

// genFenceRecorder captures every onMessage dispatch by payload n, in order.
type genFenceRecorder struct {
	mu   sync.Mutex
	seen []int
}

func (r *genFenceRecorder) handler() func(*PubSubMessage) {
	return func(m *PubSubMessage) {
		n := genFencePayloadN(m)
		r.mu.Lock()
		r.seen = append(r.seen, n)
		r.mu.Unlock()
	}
}

func (r *genFenceRecorder) dispatched() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.seen...)
}

func (r *genFenceRecorder) count(n int) int {
	c := 0
	for _, s := range r.dispatched() {
		if s == n {
			c++
		}
	}
	return c
}

func assertDispatched(t *testing.T, rec *genFenceRecorder, want ...int) {
	t.Helper()
	got := rec.dispatched()
	if len(got) != len(want) {
		t.Fatalf("dispatched = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("dispatched = %v, want %v", got, want)
		}
	}
}

// newGenFenceClient builds an offline client whose connection generation is
// already established at 1, exactly as after a successful Connect.
func newGenFenceClient(rec *genFenceRecorder) *WebSocketClient {
	ws := NewWebSocketClient(0, nil, 3600, 0, rec.handler(), nil)
	ws.mu.Lock()
	ws.connGen = 1
	ws.mu.Unlock()
	return ws
}

// supersede performs the generation swap's linearization step — connGen
// advanced under ws.mu — which is what reconnectAfter (and then Connect) do to
// retire the reader that captured the previous generation.
func supersede(ws *WebSocketClient) uint64 {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	ws.connGen++
	return ws.connGen
}

func currentGen(ws *WebSocketClient) uint64 {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ws.connGen
}

// replayWindowSize reports how many fingerprints the client's 1-second replay
// window currently holds.
func replayWindowSize(ws *WebSocketClient) int {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return len(ws.recentMsgFingerprints)
}

// deliverLive delivers frame n attributed to whatever generation is live at
// the moment of the attempt and requires it to dispatch. If the generation
// moved underneath an attempt (a real connection can reconnect on its own
// between reading the generation and using it), that attempt was itself a
// correctly rejected stale one and is retried on the new live generation; an
// attempt on a generation that is still live and does not dispatch fails.
func deliverLive(t *testing.T, ws *WebSocketClient, rec *genFenceRecorder, n int) {
	t.Helper()
	before := rec.count(n)
	for attempt := 0; attempt < 16; attempt++ {
		g := currentGen(ws)
		ws.handleMessageForGen(genFenceFrame(n), g)
		if rec.count(n) == before+1 {
			return
		}
		if currentGen(ws) == g {
			t.Fatalf("frame %d attributed to the live generation %d was not dispatched (count %d)", n, g, rec.count(n))
		}
	}
	t.Fatalf("frame %d: the generation kept moving across 16 live delivery attempts", n)
}

// newGenFenceNetClient builds a client against the test server with the
// recorder as its onMessage sink.
func newGenFenceNetClient(t *testing.T, url string, rec *genFenceRecorder) *WebSocketClient {
	t.Helper()
	ws := NewWebSocketClient(0, nil, 3600, 0, rec.handler(), nil)
	ws.url = url
	ws.delayUnit = time.Millisecond
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

// supersedeViaReconnect retires readerGen through the REAL reconnect path:
// reconnectAfter closes the old socket, advances the generation and dials a
// fresh connection. The swap runs on its own goroutine under a bound, so a
// swap that ever waited on something (an in-flight handler, say) fails
// loudly instead of hanging the package. It returns once the redial has landed
// and the generation has demonstrably moved past readerGen.
func supersedeViaReconnect(t *testing.T, ws *WebSocketClient, ts *wsTestServer, readerGen uint64) {
	t.Helper()
	dials := ts.dialCount()
	swapDone := make(chan struct{})
	go func() {
		ws.reconnectAfter(0)
		close(swapDone)
	}()
	select {
	case <-swapDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the reconnect swap did not complete within the bound")
	}
	waitUntil(t, "redial after the reconnect swap", 5*time.Second, func() bool { return ts.dialCount() > dials })
	if live := currentGen(ws); live == readerGen {
		t.Fatalf("reconnect did not advance the generation (still %d)", live)
	}
}

// awaitParkedHandler waits, bounded, for a parked onMessage handler to report
// that it is in flight.
func awaitParkedHandler(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the generation-1 handler never entered (was frame 1 dispatched on the live generation?)")
	}
}

// TestGenerationFenceCurrentGenerationDispatches (A, F, G): the ordinary
// current-generation path is unchanged — distinct frames dispatch in order and
// an exact same-generation replay inside the window is still suppressed.
func TestGenerationFenceCurrentGenerationDispatches(t *testing.T) {
	rec := &genFenceRecorder{}
	ws := newGenFenceClient(rec)

	for n := 1; n <= 3; n++ {
		ws.handleMessageForGen(genFenceFrame(n), 1)
	}
	assertDispatched(t, rec, 1, 2, 3)

	// F: exact replay on the same generation within the 1-second window.
	ws.handleMessageForGen(genFenceFrame(2), 1)
	assertDispatched(t, rec, 1, 2, 3)

	// The current-generation convenience entry point resolves the same way.
	ws.handleMessage(genFenceFrame(4))
	assertDispatched(t, rec, 1, 2, 3, 4)
}

// TestGenerationFenceStaleReaderGenerationIsNotDispatched (B): once the
// generation has advanced, a frame attributed to the retired reader
// generation must produce zero downstream dispatch, while the live generation
// keeps dispatching normally.
func TestGenerationFenceStaleReaderGenerationIsNotDispatched(t *testing.T) {
	rec := &genFenceRecorder{}
	ws := newGenFenceClient(rec)

	ws.handleMessageForGen(genFenceFrame(1), 1)
	assertDispatched(t, rec, 1)

	next := supersede(ws) // reconnect swap: generation 1 is retired

	ws.handleMessageForGen(genFenceFrame(2), 1)
	assertDispatched(t, rec, 1) // stale reader generation: nothing reaches onMessage

	ws.handleMessageForGen(genFenceFrame(3), next)
	assertDispatched(t, rec, 1, 3) // live generation is unaffected
}

// TestGenerationFenceSupersededBetweenReadAndDispatchViaReconnect (C): a
// frame captured under generation 1 by a real connection, superseded by the
// REAL reconnect swap (reconnectAfter closes the old socket, retires its
// generation and dials a fresh one), must not be dispatched when the old
// reader finally releases it.
func TestGenerationFenceSupersededBetweenReadAndDispatchViaReconnect(t *testing.T) {
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		ts.serve(conn)
	})
	rec := &genFenceRecorder{}
	ws := newGenFenceNetClient(t, ts.url(), rec)
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	readerGen := currentGen(ws) // what generation-1's read loop captured
	frame := genFenceFrame(1)   // "read under generation 1", not yet dispatched

	supersedeViaReconnect(t, ws, ts, readerGen)

	ws.handleMessageForGen(frame, readerGen) // the old reader releases its frame
	assertDispatched(t, rec)                 // zero stale dispatch

	deliverLive(t, ws, rec, 2)
	assertDispatched(t, rec, 2) // the live generation dispatches

	if err := ws.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// genFenceBarrier is a slog.Handler that passes every record through but parks
// the FIRST "WebSocket received" MESSAGE record on a rendezvous. That record is
// emitted by handleMessageForGen after the read loop has read and parsed the
// frame and before any dispatch decision, so it pins a real read-loop
// goroutine at exactly the post-read, pre-dispatch point without any
// production seam.
type genFenceBarrier struct {
	inner slog.Handler
	st    *genFenceBarrierState
}

type genFenceBarrierState struct {
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

func (b *genFenceBarrier) Enabled(context.Context, slog.Level) bool { return true }

func (b *genFenceBarrier) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "WebSocket received" {
		isMessage := false
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == "type" && a.Value.String() == "MESSAGE" {
				isMessage = true
				return false
			}
			return true
		})
		if isMessage {
			first := false
			b.st.once.Do(func() { first = true })
			if first {
				b.st.entered <- struct{}{}
				<-b.st.release
			}
		}
	}
	return b.inner.Handle(ctx, r)
}

func (b *genFenceBarrier) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &genFenceBarrier{inner: b.inner.WithAttrs(attrs), st: b.st}
}

func (b *genFenceBarrier) WithGroup(name string) slog.Handler {
	return &genFenceBarrier{inner: b.inner.WithGroup(name), st: b.st}
}

// installGenFenceBarrier makes the barrier the process-wide slog default for
// the test and restores both the previous logger and the log package's
// writer/flags (slog.SetDefault redirects the latter and restoring the logger
// alone does not undo it). It returns the barrier state and an idempotent
// release that is also registered as a cleanup, so a failing assertion can
// never leave the parked read loop blocked into the client's Close.
func installGenFenceBarrier(t *testing.T) (*genFenceBarrierState, func()) {
	t.Helper()
	st := &genFenceBarrierState{entered: make(chan struct{}, 1), release: make(chan struct{})}
	prev := slog.Default()
	prevOut, prevFlags := log.Writer(), log.Flags()
	slog.SetDefault(slog.New(&genFenceBarrier{inner: slog.NewTextHandler(io.Discard, nil), st: st}))
	t.Cleanup(func() {
		slog.SetDefault(prev)
		log.SetOutput(prevOut)
		log.SetFlags(prevFlags)
	})
	var once sync.Once
	release := func() { once.Do(func() { close(st.release) }) }
	t.Cleanup(release)
	return st, release
}

// awaitParkedReader waits, bounded, for the real read loop to reach the
// barrier; a timeout names the seam so a reworded log line fails loudly
// instead of hanging the package.
func awaitParkedReader(t *testing.T, st *genFenceBarrierState) {
	t.Helper()
	select {
	case <-st.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("generation-1 reader never reached the post-read barrier (is the \"WebSocket received\" MESSAGE debug record still emitted at the top of handleMessageForGen?)")
	}
}

// TestGenerationFenceOldReaderParkedAcrossReconnectDoesNotDispatch (C,
// end to end): the REAL generation-1 read loop reads a frame from a real
// socket and is parked between that read and its dispatch decision; the real
// reconnect swap then retires generation 1 and establishes a live one. When
// the old reader resumes, its frame must not reach onMessage. Close joins
// every loop of every generation, so once it returns the old reader has
// finished and the negative assertion is deterministic — no sleep is
// load-bearing. What it pins is that the frame a real reader took off the
// socket goes through the fence; it does not pin that readLoop hands the
// fence its Connect-captured generation rather than the live one read at
// dispatch entry, because the reader parks after that choice is made and no
// seam exists between ReadMessage and the call.
func TestGenerationFenceOldReaderParkedAcrossReconnectDoesNotDispatch(t *testing.T) {
	// Wire bytes are built on the test goroutine: the server behavior runs on
	// the HTTP handler goroutine, where t.Fatal is not allowed.
	wire1, wire2 := genFenceWire(t, 1), genFenceWire(t, 2)
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		switch dial {
		case 1:
			_ = conn.WriteMessage(websocket.TextMessage, wire1)
		case 2:
			_ = conn.WriteMessage(websocket.TextMessage, wire2)
		}
		ts.serve(conn)
	})
	rec := &genFenceRecorder{}
	ws := newGenFenceNetClient(t, ts.url(), rec)
	// Registered after the client so it runs before the client's Close.
	st, release := installGenFenceBarrier(t)
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	readerGen := currentGen(ws)

	awaitParkedReader(t, st) // the generation-1 reader holds frame 1, read but not dispatched

	supersedeViaReconnect(t, ws, ts, readerGen) // real supersession while the old reader is parked
	// Positive control: the live generation's reader dispatches its own frame.
	waitUntil(t, "live-generation dispatch of frame 2", 5*time.Second, func() bool { return rec.count(2) == 1 })

	release() // the old reader resumes with its generation-1 frame

	if err := ws.Close(); err != nil { // joins the old reader too
		t.Fatalf("Close: %v", err)
	}
	if got := rec.count(1); got != 0 {
		t.Fatalf("frame read by the superseded generation-1 reader was dispatched %d time(s); want 0 (dispatched=%v)", got, rec.dispatched())
	}
	assertDispatched(t, rec, 2)
}

// TestGenerationFenceStaleFrameDoesNotPoisonReplayWindow (E): a stale
// generation-1 frame carrying fingerprint F is rejected AND leaves the replay
// window untouched, so the legitimate live-generation delivery of the same F
// is still admitted. Without the fence the stale copy is dispatched and its
// fingerprint then suppresses the legitimate delivery.
func TestGenerationFenceStaleFrameDoesNotPoisonReplayWindow(t *testing.T) {
	t.Run("swap step", func(t *testing.T) {
		rec := &genFenceRecorder{}
		ws := newGenFenceClient(rec)
		live := supersede(ws)

		ws.handleMessageForGen(genFenceFrame(7), 1) // stale: rejected
		assertDispatched(t, rec)
		// Diagnostic, secondary to the behavioral proof below: the stale
		// frame left no trace in the window.
		if got := replayWindowSize(ws); got != 0 {
			t.Fatalf("stale frame entered the replay window (%d entries); want 0", got)
		}
		ws.handleMessageForGen(genFenceFrame(7), live) // legitimate: admitted
		assertDispatched(t, rec, 7)
	})

	t.Run("real reconnect", func(t *testing.T) {
		ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
			ts.serve(conn)
		})
		rec := &genFenceRecorder{}
		ws := newGenFenceNetClient(t, ts.url(), rec)
		if err := ws.Connect(); err != nil {
			t.Fatalf("Connect: %v", err)
		}
		readerGen := currentGen(ws)
		supersedeViaReconnect(t, ws, ts, readerGen)

		ws.handleMessageForGen(genFenceFrame(7), readerGen)
		assertDispatched(t, rec)
		deliverLive(t, ws, rec, 7)
		assertDispatched(t, rec, 7)
		if err := ws.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
}

// TestGenerationFenceSupersessionInsideDispatchWindow (D): the swap lands at
// the narrowest reachable point after the admission decision — inside the
// dispatch itself. The frame admitted before the swap completes exactly once
// (its admission was linearized ahead of the swap); every later frame
// attributed to the retired generation is rejected; and the retired
// generation's rejected frames leave the live generation free to deliver
// them, while a legitimately admitted frame keeps its replay-window entry.
func TestGenerationFenceSupersessionInsideDispatchWindow(t *testing.T) {
	rec := &genFenceRecorder{}
	ws := newGenFenceClient(rec)
	var swapOnce sync.Once
	ws.onMessage = func(m *PubSubMessage) {
		rec.handler()(m)
		swapOnce.Do(func() { supersede(ws) }) // supersede while dispatching
	}

	// The in-dispatch swap takes ws.mu, so a dispatch that ran under that
	// mutex would deadlock here; bound it so such a regression fails loudly.
	dispatched := make(chan struct{})
	go func() {
		ws.handleMessageForGen(genFenceFrame(1), 1)
		close(dispatched)
	}()
	select {
	case <-dispatched:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatch did not return: onMessage appears to run under ws.mu")
	}
	assertDispatched(t, rec, 1)
	live := currentGen(ws)
	if live != 2 {
		t.Fatalf("connGen = %d after the in-dispatch swap, want 2", live)
	}

	ws.handleMessageForGen(genFenceFrame(2), 1) // retired generation: rejected
	assertDispatched(t, rec, 1)

	// Frame 1 was legitimately admitted before the swap: its replay-window
	// entry stands (unchanged 1-second transport replay defense), so a
	// redelivery on the live generation inside the window is still a replay.
	ws.handleMessageForGen(genFenceFrame(1), live)
	assertDispatched(t, rec, 1)

	// Frame 2 was rejected as stale and never entered the window: the live
	// generation delivers it.
	ws.handleMessageForGen(genFenceFrame(2), live)
	assertDispatched(t, rec, 1, 2)
}

// newParkedHandlerClient builds a client against the test server whose
// onMessage records the frame and then parks until released, so "a handler is
// in flight" is a channel-synchronized state (stop_join_test.go precedent).
func newParkedHandlerClient(t *testing.T, url string, rec *genFenceRecorder, entered chan struct{}, release chan struct{}) *WebSocketClient {
	t.Helper()
	record := rec.handler()
	ws := NewWebSocketClient(0, nil, 3600, 0, func(m *PubSubMessage) {
		record(m)
		entered <- struct{}{}
		<-release
	}, nil)
	ws.url = url
	ws.delayUnit = time.Millisecond
	t.Cleanup(func() { _ = ws.Close() })
	return ws
}

// TestGenerationFenceSwapNeverWaitsForInFlightHandler pins the lifecycle
// contract the fence comment asserts: the reconnect swap never waits for a
// downstream handler. With the generation-1 handler parked mid-dispatch, the
// real reconnectAfter must complete (swap + redial) within a bound, the frame
// admitted before the swap is delivered exactly once, and the live generation
// dispatches normally.
func TestGenerationFenceSwapNeverWaitsForInFlightHandler(t *testing.T) {
	wire1, wire2 := genFenceWire(t, 1), genFenceWire(t, 2)
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		switch dial {
		case 1:
			_ = conn.WriteMessage(websocket.TextMessage, wire1)
		case 2:
			_ = conn.WriteMessage(websocket.TextMessage, wire2)
		}
		ts.serve(conn)
	})
	rec := &genFenceRecorder{}
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	ws := newParkedHandlerClient(t, ts.url(), rec, entered, release)
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	readerGen := currentGen(ws)
	awaitParkedHandler(t, entered) // the generation-1 handler is in flight for frame 1

	swapDone := make(chan struct{})
	go func() {
		ws.reconnectAfter(0)
		close(swapDone)
	}()
	select {
	case <-swapDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the reconnect swap waited on an in-flight handler")
	}
	waitUntil(t, "redial while the handler is parked", 5*time.Second, func() bool { return ts.dialCount() >= 2 })
	if live := currentGen(ws); live == readerGen {
		t.Fatalf("reconnect did not advance the generation (still %d)", live)
	}
	if ws.state().Reconnecting {
		t.Fatal("connection still reports Reconnecting after the swap completed")
	}
	// The live generation dispatches while the old handler is still parked.
	waitUntil(t, "live-generation dispatch of frame 2", 5*time.Second, func() bool { return rec.count(2) == 1 })

	releaseOnce.Do(func() { close(release) })
	if err := ws.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := rec.count(1); got != 1 {
		t.Fatalf("frame admitted before the swap dispatched %d time(s), want exactly 1", got)
	}
}

// TestGenerationFenceCloseStaysBoundedAcrossSwapWithParkedHandler: a wedged
// handler on a retired generation plus a completed reconnect swap must not
// change Close's contract — it still returns within stopJoinTimeout with the
// explicit errLoopJoinTimeout, so the reconnect driver holds nothing Close
// needs and no generation's loop can hang shutdown.
func TestGenerationFenceCloseStaysBoundedAcrossSwapWithParkedHandler(t *testing.T) {
	old := stopJoinTimeout
	stopJoinTimeout = 100 * time.Millisecond
	defer func() { stopJoinTimeout = old }()

	wire1 := genFenceWire(t, 1)
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		if dial == 1 {
			_ = conn.WriteMessage(websocket.TextMessage, wire1)
		}
		ts.serve(conn)
	})
	rec := &genFenceRecorder{}
	entered := make(chan struct{}, 1)
	release := make(chan struct{}) // wedged: released only at cleanup
	ws := newParkedHandlerClient(t, ts.url(), rec, entered, release)
	t.Cleanup(func() { close(release) })
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	awaitParkedHandler(t, entered)
	readerGen := currentGen(ws)
	supersedeViaReconnect(t, ws, ts, readerGen)

	// Close runs on its own goroutine under the test's own bound
	// (stop_join_test.go precedent): a join that lost its timeout fails here,
	// at this line, instead of hanging the package until go test's alarm.
	type closeResult struct {
		err     error
		elapsed time.Duration
	}
	closed := make(chan closeResult, 1)
	start := time.Now()
	go func() {
		err := ws.Close()
		closed <- closeResult{err: err, elapsed: time.Since(start)}
	}()
	var res closeResult
	select {
	case res = <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Close blocked far beyond the join timeout — did the join lose its bound?")
	}
	if !errors.Is(res.err, errLoopJoinTimeout) {
		t.Fatalf("Close with a wedged retired-generation handler = %v, want errLoopJoinTimeout", res.err)
	}
	if res.elapsed < stopJoinTimeout {
		t.Fatalf("Close returned before the join timeout (%v)", res.elapsed)
	}
}

// TestGenerationFenceReconnectChurnStaysLinearizableAndJoinable (D under
// -race, H, I): concurrent dispatch attempts race real reconnect swaps over
// several generations. After every swap, frames attributed to the retired
// generation produce zero dispatch, frames rejected during the race are still
// deliverable on the live generation, every distinct frame is dispatched
// exactly once overall, and Close still joins every generation's loops within
// its bound. A linearization witness reads the replay window under ws.mu in
// the critical section that first observes the round's generation retired: a
// racer frame the window holds only after that instant was admitted after its
// generation was retired, which a fence that checks the generation and admits
// the fingerprint in separate critical sections lets through and the single
// critical section rules out. The witness passes by construction on the
// single critical section; against a split fence it fires whenever its
// observation is ordered before the late admission, so a run is a
// probabilistic falsifier of the split fence — most runs at GOMAXPROCS >= 2,
// fewer at GOMAXPROCS=1 — not a per-round guarantee, since no seam exists
// between a check and an admission. The test tolerates additional
// generations appearing on their own (a superseded
// reader that classifies its own read error late can request one extra
// reconnect — a pre-existing read-error-path behavior outside this fence),
// because B1's invariant holds for every generation however it came to be
// retired.
func TestGenerationFenceReconnectChurnStaysLinearizableAndJoinable(t *testing.T) {
	ts := newWSTestServer(t, func(ts *wsTestServer, conn *websocket.Conn, dial int) {
		ts.serve(conn)
	})
	rec := &genFenceRecorder{}
	ws := newGenFenceNetClient(t, ts.url(), rec)
	// Every admitted reconnect swap reports itself once, so the dial count
	// can be bounded by swaps actually admitted rather than by rounds: a
	// round whose swap collapsed into a self-initiated reconnect already in
	// flight contributes no dial of its own.
	var swaps atomic.Int64
	ws.SetReconnectHandler(func() { swaps.Add(1) })
	if err := ws.Connect(); err != nil {
		t.Fatalf("Connect: %v", err)
	}

	const rounds, racers, postSwap = 6, 12, 4
	for round := 1; round <= rounds; round++ {
		churnRound(t, ws, rec, round, racers, postSwap)
	}

	if err := ws.Close(); err != nil {
		t.Fatalf("Close after %d reconnects: %v", rounds, err)
	}
	// One dial for Connect plus at most one per admitted swap: no generation
	// ever dialed without an admitted swap, and at least one swap landed.
	if got, admitted := ts.dialCount(), int(swaps.Load()); got < 2 || got > 1+admitted {
		t.Fatalf("dials = %d with %d admitted swaps; want between 2 and %d", got, admitted, 1+admitted)
	}
}

// churnRound races racers concurrent dispatch attempts attributed to the live
// generation against one real reconnect swap, then checks the round's
// invariants: zero dispatch for the retired generation after the swap, no
// admission ordered after the swap (the linearization witness), and every
// frame delivered exactly once overall.
func churnRound(t *testing.T, ws *WebSocketClient, rec *genFenceRecorder, round, racers, postSwap int) {
	gen := currentGen(ws)
	base := round * 100
	fps := racerFingerprints(t, base, base+racers)

	witnessStop := make(chan struct{})
	defer close(witnessStop) // on every exit, so the witness never outlives the round
	witnessed := make(chan map[int]struct{}, 1)
	witnessReady := make(chan struct{})
	go func() { witnessed <- witnessRetirement(ws, gen, fps, witnessReady, witnessStop) }()
	// The racers start only once the witness has taken its first read of the
	// generation, so a witness that is not yet scheduled cannot miss a swap.
	select {
	case <-witnessReady:
	case <-time.After(5 * time.Second):
		t.Fatalf("round %d: the witness never took its first read of the generation", round)
	}

	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			ws.handleMessageForGen(genFenceFrame(n), gen)
		}(base + i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		ws.reconnectAfter(0)
	}()
	raced := make(chan struct{})
	go func() {
		wg.Wait()
		close(raced)
	}()
	select {
	case <-raced:
	case <-time.After(5 * time.Second):
		t.Fatalf("round %d: racers or the reconnect swap did not complete within the bound", round)
	}
	// gen must be retired (connGen is monotonic, so every frame attributed
	// to gen is void from here on) AND a live generation re-established —
	// by this round's swap or, when that collapsed into a self-initiated
	// reconnect already in flight, by that one. Closed is cleared only by
	// a successful Connect, so it is the re-establishment signal even
	// across a failed dial and its self-retry.
	waitUntil(t, fmt.Sprintf("round %d generation retired and re-established", round), 5*time.Second, func() bool {
		st := ws.state()
		return currentGen(ws) != gen && !st.Reconnecting && !st.Closed
	})
	var atRetirement map[int]struct{}
	select {
	case atRetirement = <-witnessed:
	case <-time.After(5 * time.Second):
		t.Fatalf("round %d: the witness never observed generation %d retired", round, gen)
	}

	// Sequential, after the swap: the retired generation is void.
	for j := 0; j < postSwap; j++ {
		n := base + racers + j
		ws.handleMessageForGen(genFenceFrame(n), gen)
		if got := rec.count(n); got != 0 {
			t.Fatalf("round %d: frame %d attributed to retired generation %d was dispatched %d time(s)", round, n, gen, got)
		}
	}
	// Linearization witness: every racer frame the window holds now was
	// already in it when gen was first observed retired, i.e. no admission
	// was ordered after the swap. Read before deliverLive below, which
	// admits the rejected frames on the live generation.
	ws.mu.RLock()
	afterRace := windowedRacersLocked(ws, fps)
	ws.mu.RUnlock()
	for n := range afterRace {
		if _, ok := atRetirement[n]; !ok {
			t.Fatalf("round %d: frame %d attributed to generation %d entered the replay window after that generation was retired (dispatched %d time(s)); the admission was not ordered against the swap", round, n, gen, rec.count(n))
		}
	}
	// Anything the race rejected must still be deliverable live (no
	// poisoned window); anything admitted is already counted once.
	for n := base; n < base+racers+postSwap; n++ {
		if rec.count(n) == 0 {
			deliverLive(t, ws, rec, n)
		}
		if got := rec.count(n); got != 1 {
			t.Fatalf("round %d: frame %d dispatched %d time(s) overall, want exactly 1", round, n, got)
		}
	}
}

// racerFingerprints maps each racer frame's EventFingerprint to its n, so the
// replay window can be read back in terms of frames.
func racerFingerprints(t *testing.T, from, to int) map[string]int {
	t.Helper()
	fps := make(map[string]int, to-from)
	for n := from; n < to; n++ {
		msg, err := ParsePubSubMessage(genFenceFrame(n).Data)
		if err != nil {
			t.Fatal(err)
		}
		fps[msg.EventFingerprint] = n
	}
	return fps
}

// windowedRacersLocked reports which racer frames the replay window holds.
// ws.mu must be held (read or write).
func windowedRacersLocked(ws *WebSocketClient, fps map[string]int) map[int]struct{} {
	held := make(map[int]struct{})
	for fp, n := range fps {
		if _, ok := ws.recentMsgFingerprints[fp]; ok {
			held[n] = struct{}{}
		}
	}
	return held
}

// witnessRetirement spins under ws.mu until gen is no longer the live
// generation and reports which racer frames the replay window holds in that
// same critical section. The swap retires gen under ws.mu — the mutex the
// fence check and the fingerprint admission share — so with the check and the
// admission in one critical section every racer frame attributed to gen is
// admitted either before this observation (and is in the set) or never. A
// racer frame that enters the window only later was admitted after its
// generation was retired. ready is closed once the witness has taken its
// first read of the generation; it returns nil if stopped before the
// retirement.
//
// The loop deliberately does not yield between reads: the witness catches a
// late admission only when its read section is ordered before that
// admission's write section, which it is whenever it is already waiting on
// the lock when the swap releases it (sync.RWMutex hands the lock to waiting
// readers before the next writer). Yielding between iterations measurably
// weakens that. The spin lasts only until the swap and is stopped on every
// exit of the round.
func witnessRetirement(ws *WebSocketClient, gen uint64, fps map[string]int, ready chan<- struct{}, stop <-chan struct{}) map[int]struct{} {
	var readyOnce sync.Once
	signalReady := func() { readyOnce.Do(func() { close(ready) }) }
	defer signalReady()
	for {
		ws.mu.RLock()
		if ws.connGen != gen {
			held := windowedRacersLocked(ws, fps)
			ws.mu.RUnlock()
			return held
		}
		ws.mu.RUnlock()
		signalReady()
		select {
		case <-stop:
			return nil
		default:
		}
	}
}
