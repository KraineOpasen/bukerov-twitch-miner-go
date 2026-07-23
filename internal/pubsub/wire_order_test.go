package pubsub

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// wireRecorder is the recording transport hook for LISTEN/UNLISTEN frames: it
// captures the exact write order, injects deterministic one-shot write
// failures, and can park one specific frame at a barrier so tests can pin the
// "in-flight write" instant without time-based synchronization.
type wireRecorder struct {
	mu       sync.Mutex
	writes   []string // successful writes, in wire order: "LISTEN raid.chan-1"
	attempts []string // every attempt, failed ones suffixed " !err"
	failOnce map[string]error

	barrierFor string
	entered    chan struct{}
	release    chan struct{}
}

func newWireRecorder() *wireRecorder {
	return &wireRecorder{failOnce: make(map[string]error)}
}

func wireKey(frameType string, topic Topic) string { return frameType + " " + topic.String() }

// armBarrier parks the NEXT write of the given frame at a gate: the writer
// closes entered on arrival and proceeds only after releaseBarrier.
func (r *wireRecorder) armBarrier(frameType string, topic Topic) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.barrierFor = wireKey(frameType, topic)
	r.entered = make(chan struct{})
	r.release = make(chan struct{})
}

func (r *wireRecorder) releaseBarrier() {
	r.mu.Lock()
	release := r.release
	r.mu.Unlock()
	if release != nil {
		close(release)
	}
}

func (r *wireRecorder) hook(frameType string, topic Topic) error {
	key := wireKey(frameType, topic)

	r.mu.Lock()
	if r.barrierFor == key {
		r.barrierFor = ""
		entered, release := r.entered, r.release
		r.mu.Unlock()
		close(entered)
		<-release
		r.mu.Lock()
	}
	defer r.mu.Unlock()

	if err, ok := r.failOnce[key]; ok {
		delete(r.failOnce, key)
		r.attempts = append(r.attempts, key+" !err")
		return err
	}
	r.writes = append(r.writes, key)
	r.attempts = append(r.attempts, key)
	return nil
}

// writesFor returns the successful wire commands for the topic, in order.
func (r *wireRecorder) writesFor(topic Topic) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	suffix := " " + topic.String()
	for _, w := range r.writes {
		if strings.HasSuffix(w, suffix) {
			out = append(out, strings.TrimSuffix(w, suffix))
		}
	}
	return out
}

// lastWriteFor returns the LAST successfully written wire command for the
// topic ("" when none).
func (r *wireRecorder) lastWriteFor(topic Topic) string {
	ws := r.writesFor(topic)
	if len(ws) == 0 {
		return ""
	}
	return ws[len(ws)-1]
}

func (r *wireRecorder) attemptCountFor(frameType string, topic Topic) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	key := wireKey(frameType, topic)
	for _, a := range r.attempts {
		if a == key || a == key+" !err" {
			n++
		}
	}
	return n
}

// newWireClient returns an open client whose topic frames go through the
// recorder instead of a socket.
func newWireClient(rec *wireRecorder) *WebSocketClient {
	ws := NewWebSocketClient(0, "", 60, 60, nil, nil)
	ws.isOpened = true
	ws.writeTopicFrameHook = rec.hook
	return ws
}

// newWirePool returns a pool whose connection factory produces recorder-backed
// open clients.
func newWirePool(rec *wireRecorder) *WebSocketPool {
	p := &WebSocketPool{
		predictions: make(map[string]*models.EventPrediction),
		control:     make(map[string]*roundControl),
	}
	p.newClient = func(index int) (*WebSocketClient, error) {
		ws := NewWebSocketClient(index, "", 60, 60, nil, nil)
		ws.isOpened = true
		ws.writeTopicFrameHook = rec.hook
		return ws, nil
	}
	return p
}

func (ws *WebSocketClient) unlistenRetryCount() int {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return len(ws.unlistenRetry)
}

// TestWireOrderDisableDuringReplayIsCompensated — T1: a runtime disable that
// arrives while the reconnect replay's LISTEN write is in flight must
// serialize behind it and leave a compensating UNLISTEN as the final wire
// command; the local desired state ends absent and repeated disables stay
// no-ops only after real convergence.
func TestWireOrderDisableDuringReplayIsCompensated(t *testing.T) {
	rec := newWireRecorder()
	ws := newWireClient(rec)
	p := &WebSocketPool{
		predictions: make(map[string]*models.EventPrediction),
		control:     make(map[string]*roundControl),
	}
	p.mu.Lock()
	p.clients = []*WebSocketClient{ws}
	p.mu.Unlock()

	raid := NewTopic(TopicRaid, "chan-1")
	ws.mu.Lock()
	ws.pendingTopics = []Topic{raid}
	ws.mu.Unlock()

	rec.armBarrier("LISTEN", raid)

	replayDone := make(chan struct{})
	go func() {
		defer close(replayDone)
		ws.replayPendingTopics()
	}()
	<-rec.entered // the LISTEN write for raid is now in flight

	// Deterministic protection probe: with wire-order protection the in-flight
	// write holds writeMu, so a concurrent disable CANNOT slip in between the
	// promotion and the write. Without protection (mutation M6) the lock is
	// free — run the disable to completion first, so the stale LISTEN
	// demonstrably lands after it.
	if ws.writeMu.TryLock() {
		ws.writeMu.Unlock()
		_ = p.EnsureTopic(raid, false)
		rec.releaseBarrier()
		<-replayDone
	} else {
		disableDone := make(chan struct{})
		go func() {
			defer close(disableDone)
			_ = p.EnsureTopic(raid, false)
		}()
		rec.releaseBarrier()
		<-replayDone
		<-disableDone
	}

	if ws.HasTopic(raid) || ws.HasTopicApplied(raid) {
		t.Fatal("local desired state must end ABSENT after the disable")
	}
	writes := rec.writesFor(raid)
	if last := rec.lastWriteFor(raid); last == "LISTEN" {
		t.Fatalf("stale LISTEN is the FINAL wire command after desired=false (writes: %v)", writes)
	}
	// The LISTEN was already committed to the wire, so the compensating
	// UNLISTEN must follow it.
	if len(writes) != 2 || writes[0] != "LISTEN" || writes[1] != "UNLISTEN" {
		t.Fatalf("wire sequence = %v, want [LISTEN UNLISTEN]", writes)
	}

	// Idempotency only after true convergence: a repeated disable writes
	// nothing more.
	before := rec.attemptCountFor("UNLISTEN", raid)
	if err := p.EnsureTopic(raid, false); err != nil {
		t.Fatalf("repeated disable: %v", err)
	}
	if got := rec.attemptCountFor("UNLISTEN", raid); got != before {
		t.Fatalf("repeated disable after convergence wrote %d extra UNLISTEN frames", got-before)
	}
}

// TestWireOrderStaleUnlistenNeverBeatsReenable — T2: a re-enable racing an
// in-flight UNLISTEN write must serialize behind it, so the stale UNLISTEN is
// never the final wire command; the topic ends applied exactly once.
func TestWireOrderStaleUnlistenNeverBeatsReenable(t *testing.T) {
	rec := newWireRecorder()
	ws := newWireClient(rec)
	raid := NewTopic(TopicRaid, "chan-1")

	if err := ws.Listen(raid); err != nil {
		t.Fatalf("setup listen: %v", err)
	}

	rec.armBarrier("UNLISTEN", raid)
	unlistenDone := make(chan struct{})
	go func() {
		defer close(unlistenDone)
		_ = ws.Unlisten(raid)
	}()
	<-rec.entered // the UNLISTEN write is now in flight, holding writeMu

	listenDone := make(chan struct{})
	go func() {
		defer close(listenDone)
		_ = ws.Listen(raid)
	}()

	rec.releaseBarrier()
	<-unlistenDone
	<-listenDone

	if !ws.HasTopicApplied(raid) {
		t.Fatal("desired=true must end wire-applied")
	}
	if got := ws.TopicCount(); got != 1 {
		t.Fatalf("topic count = %d, want exactly 1 effective subscription", got)
	}
	writes := rec.writesFor(raid)
	if last := rec.lastWriteFor(raid); last != "LISTEN" {
		t.Fatalf("final wire command = %q, want LISTEN (writes: %v)", last, writes)
	}
	want := []string{"LISTEN", "UNLISTEN", "LISTEN"}
	if len(writes) != len(want) {
		t.Fatalf("wire sequence = %v, want %v", writes, want)
	}
	for i := range want {
		if writes[i] != want[i] {
			t.Fatalf("wire sequence = %v, want %v", writes, want)
		}
	}
}

// TestWireWriteListenErrorStaysRetryable — T3: a failed LISTEN write surfaces
// through EnsureTopic, is never counted as applied, and the identical retry
// performs the wire write again — ending with exactly one effective LISTEN.
func TestWireWriteListenErrorStaysRetryable(t *testing.T) {
	rec := newWireRecorder()
	p := newWirePool(rec)
	raid := NewTopic(TopicRaid, "chan-1")
	wireErr := errors.New("write failed")
	rec.mu.Lock()
	rec.failOnce[wireKey("LISTEN", raid)] = wireErr
	rec.mu.Unlock()

	if err := p.EnsureTopic(raid, true); !errors.Is(err, wireErr) {
		t.Fatalf("first enable error = %v, want %v", err, wireErr)
	}
	p.mu.RLock()
	client := p.clients[0]
	p.mu.RUnlock()
	if client.HasTopicApplied(raid) {
		t.Fatal("failed LISTEN write must not be counted as wire-applied")
	}
	if !client.HasTopic(raid) {
		t.Fatal("failed LISTEN must keep the topic desired-pending (retry state)")
	}

	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	if !client.HasTopicApplied(raid) {
		t.Fatal("retry must converge to wire-applied")
	}
	if got := rec.attemptCountFor("LISTEN", raid); got != 2 {
		t.Fatalf("LISTEN write attempts = %d, want 2 (failed + retried)", got)
	}
	if writes := rec.writesFor(raid); len(writes) != 1 || writes[0] != "LISTEN" {
		t.Fatalf("successful wire commands = %v, want exactly one LISTEN", writes)
	}

	// Converged: a further identical apply writes nothing.
	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("post-convergence apply: %v", err)
	}
	if got := rec.attemptCountFor("LISTEN", raid); got != 2 {
		t.Fatalf("post-convergence apply produced an extra LISTEN (attempts=%d)", got)
	}
}

// TestWireWriteUnlistenErrorStaysRetryable — T4: a failed UNLISTEN write
// surfaces through EnsureTopic, records an explicit retry debt, and the
// identical retry re-sends the frame — converging to absent.
func TestWireWriteUnlistenErrorStaysRetryable(t *testing.T) {
	rec := newWireRecorder()
	p := newWirePool(rec)
	raid := NewTopic(TopicRaid, "chan-1")

	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("setup enable: %v", err)
	}
	p.mu.RLock()
	client := p.clients[0]
	p.mu.RUnlock()

	wireErr := errors.New("write failed")
	rec.mu.Lock()
	rec.failOnce[wireKey("UNLISTEN", raid)] = wireErr
	rec.mu.Unlock()

	if err := p.EnsureTopic(raid, false); !errors.Is(err, wireErr) {
		t.Fatalf("first disable error = %v, want %v", err, wireErr)
	}
	if client.HasTopic(raid) || client.HasTopicApplied(raid) {
		t.Fatal("failed UNLISTEN: topic must not remain desired/applied locally")
	}
	if got := client.unlistenRetryCount(); got != 1 {
		t.Fatalf("failed UNLISTEN must record a retry debt, ledger=%d", got)
	}

	if err := p.EnsureTopic(raid, false); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	if got := client.unlistenRetryCount(); got != 0 {
		t.Fatalf("retry debt not settled, ledger=%d", got)
	}
	if got := rec.attemptCountFor("UNLISTEN", raid); got != 2 {
		t.Fatalf("UNLISTEN write attempts = %d, want 2 (failed + retried)", got)
	}
	if last := rec.lastWriteFor(raid); last != "UNLISTEN" {
		t.Fatalf("final wire command = %q, want UNLISTEN", last)
	}

	// Converged absent: a further identical disable writes nothing.
	before := rec.attemptCountFor("UNLISTEN", raid)
	if err := p.EnsureTopic(raid, false); err != nil {
		t.Fatalf("post-convergence disable: %v", err)
	}
	if got := rec.attemptCountFor("UNLISTEN", raid); got != before {
		t.Fatal("post-convergence disable produced an extra UNLISTEN")
	}
}

// TestWireOrderReconnectReplayHonorsDisable — T5: a disable landing while the
// connection is mid-reconnect must not resurrect the topic — verified on the
// actual frame sequence, not just the local slices.
func TestWireOrderReconnectReplayHonorsDisable(t *testing.T) {
	rec := newWireRecorder()
	ws := newWireClient(rec)
	raid := NewTopic(TopicRaid, "chan-1")
	keep := NewTopic(TopicVideoPlaybackByID, "chan-1")

	if err := ws.Listen(raid); err != nil {
		t.Fatal(err)
	}
	if err := ws.Listen(keep); err != nil {
		t.Fatal(err)
	}

	// Simulate reconnectAfter's generation swap exactly.
	ws.mu.Lock()
	ws.pendingTopics = mergeTopics(ws.topics, ws.pendingTopics)
	ws.topics = nil
	ws.unlistenRetry = nil
	ws.isOpened = false
	ws.connGen++
	ws.mu.Unlock()

	// The disable lands mid-reconnect: no frame can or should be written.
	if err := ws.Unlisten(raid); err != nil {
		t.Fatalf("mid-reconnect disable: %v", err)
	}

	// The new generation comes up and replays.
	ws.mu.Lock()
	ws.isOpened = true
	ws.connGen++
	ws.mu.Unlock()
	ws.replayPendingTopics()

	if got := rec.writesFor(raid); len(got) != 1 || got[0] != "LISTEN" {
		t.Fatalf("raid frames = %v, want only the pre-reconnect LISTEN (no resurrection, no stray UNLISTEN)", got)
	}
	if got := rec.writesFor(keep); len(got) != 2 || got[0] != "LISTEN" || got[1] != "LISTEN" {
		t.Fatalf("keep frames = %v, want [LISTEN LISTEN] (initial + exactly one replay)", got)
	}
	if ws.HasTopic(raid) {
		t.Fatal("disabled topic resurrected in local state")
	}
	if !ws.HasTopicApplied(keep) {
		t.Fatal("kept topic must be wire-applied after the replay")
	}
}

// TestWireOrderConcurrentTogglesConverge — T6: concurrent desired toggles,
// replay activity, snapshots and PING writes race cleanly (-race), and after a
// final explicit apply the last wire command per topic matches the desired
// state with exactly one effective subscription.
func TestWireOrderConcurrentTogglesConverge(t *testing.T) {
	rec := newWireRecorder()
	p := newWirePool(rec)
	raid := NewTopic(TopicRaid, "chan-1")
	pred := NewTopic(TopicPredictionsChannel, "chan-1")

	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatal(err)
	}
	p.mu.RLock()
	client := p.clients[0]
	p.mu.RUnlock()

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_ = p.EnsureTopic(raid, (g+i)%2 == 0)
				_ = p.EnsureTopic(pred, (g+i)%3 != 0)
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			client.replayPendingTopics()
			_ = p.ConnSnapshot()
			client.ping()
		}
	}()
	wg.Wait()

	// Converge deterministically and verify wire/state agreement.
	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("final enable: %v", err)
	}
	if err := p.EnsureTopic(pred, false); err != nil {
		t.Fatalf("final disable: %v", err)
	}

	if !client.HasTopicApplied(raid) {
		t.Fatal("raid must end wire-applied")
	}
	if last := rec.lastWriteFor(raid); last != "LISTEN" {
		t.Fatalf("raid final wire command = %q, want LISTEN", last)
	}
	if client.HasTopic(pred) || client.HasTopicApplied(pred) {
		t.Fatal("pred must end absent")
	}
	if writes := rec.writesFor(pred); len(writes) > 0 && writes[len(writes)-1] != "UNLISTEN" {
		t.Fatalf("pred final wire command = %q, want UNLISTEN (writes: %v)", writes[len(writes)-1], writes)
	}
	if got := client.unlistenRetryCount(); got != 0 {
		t.Fatalf("no retry debt may remain after convergence, ledger=%d", got)
	}
	instances := 0
	p.mu.RLock()
	for _, ws := range p.clients {
		if ws.HasTopic(raid) {
			instances++
		}
	}
	p.mu.RUnlock()
	if instances != 1 {
		t.Fatalf("raid owned by %d connections, want exactly 1", instances)
	}
}
