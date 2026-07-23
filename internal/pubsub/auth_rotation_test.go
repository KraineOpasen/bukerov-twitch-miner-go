package pubsub

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// authRotRecorder is a thread-safe LISTEN/UNLISTEN frame recorder for the
// token-rotation tests: it counts every attempted write, records successful
// frames in order, and can be flipped into a fail-everything mode to exercise
// the sweep's write-failure path deterministically.
type authRotRecorder struct {
	mu       sync.Mutex
	frames   []string // successful writes, in order: "LISTEN community-points-user-v1.42"
	attempts int      // every attempted write, failed ones included
	fail     error    // when non-nil, every attempt fails with this error
}

func (r *authRotRecorder) hook(frameType string, topic Topic) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempts++
	if r.fail != nil {
		return r.fail
	}
	r.frames = append(r.frames, frameType+" "+topic.String())
	return nil
}

func (r *authRotRecorder) setFail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail = err
}

func (r *authRotRecorder) frameCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.frames)
}

func (r *authRotRecorder) attemptCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attempts
}

// framesSince returns a copy of the successful frames recorded after the first
// n, so a test can isolate what one operation wrote from its setup writes.
func (r *authRotRecorder) framesSince(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.frames[n:]))
	copy(out, r.frames[n:])
	return out
}

// rotatingAuth is an AuthTokenProvider backed by an atomic value, so a test
// can rotate the credential snapshot concurrently with frame writes.
type rotatingAuth struct{ v atomic.Value }

func newRotatingAuth(s AuthSnapshot) *rotatingAuth {
	a := &rotatingAuth{}
	a.v.Store(s)
	return a
}

func (a *rotatingAuth) provider() AuthSnapshot { return a.v.Load().(AuthSnapshot) }
func (a *rotatingAuth) rotate(s AuthSnapshot)  { a.v.Store(s) }

// lastAuthGeneration reads the client's last-written credential generation
// under its lock.
func (ws *WebSocketClient) lastAuthGeneration() uint64 {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return ws.lastAuthGen
}

// pendingTopicCount reads the parked-topic ledger size under the client's lock.
func (ws *WebSocketClient) pendingTopicCount() int {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	return len(ws.pendingTopics)
}

// newAuthSweepClient returns an opened, network-free client whose topic frames
// go through the recorder and whose user-topic writes read the given provider.
func newAuthSweepClient(index int, fn AuthTokenProvider, rec *authRotRecorder) *WebSocketClient {
	ws := NewWebSocketClient(index, fn, 60, 60, nil, nil)
	ws.writeTopicFrameHook = rec.hook
	ws.mu.Lock()
	ws.isOpened = true
	ws.mu.Unlock()
	return ws
}

// TestReconnectReplayUsesCurrentTokenGeneration — I1: a user-topic LISTEN
// replayed by a reconnect must read the CURRENT auth snapshot at write time,
// not a startup-time copy: after a rotation to generation 2, the replayed
// frame's recorded generation is 2.
func TestReconnectReplayUsesCurrentTokenGeneration(t *testing.T) {
	auth := newRotatingAuth(AuthSnapshot{Token: "tok-1", Generation: 1})
	rec := &authRotRecorder{}
	ws := NewWebSocketClient(0, auth.provider, 60, 60, nil, nil)
	ws.writeTopicFrameHook = rec.hook
	ws.mu.Lock()
	ws.isOpened = true
	ws.mu.Unlock()

	user := NewTopic(TopicCommunityPointsUser, "42")
	if err := ws.Listen(user); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if !ws.HasTopicApplied(user) {
		t.Fatal("setup: user topic must be wire-applied")
	}
	if got := ws.lastAuthGeneration(); got != 1 {
		t.Fatalf("setup lastAuthGen = %d, want 1 (pre-rotation write)", got)
	}

	// The token rotates while the connection is up.
	auth.rotate(AuthSnapshot{Token: "tok-2", Generation: 2})

	// Simulate reconnectAfter's generation swap exactly (same technique as the
	// wire-order reconnect tests): live set parked, conn unusable, gen bumped.
	ws.mu.Lock()
	ws.pendingTopics = mergeTopics(ws.topics, ws.pendingTopics)
	ws.topics = nil
	ws.unlistenRetry = nil
	ws.isOpened = false
	ws.connGen++
	ws.mu.Unlock()

	// The new generation comes up and replays the parked set.
	ws.mu.Lock()
	ws.isOpened = true
	ws.connGen++
	ws.mu.Unlock()
	ws.replayPendingTopics()

	if got := ws.lastAuthGeneration(); got != 2 {
		t.Fatalf("lastAuthGen after replay = %d, want 2 (rotated token, not the startup copy)", got)
	}
	want := "LISTEN " + user.String()
	if frames := rec.framesSince(0); len(frames) != 2 || frames[0] != want || frames[1] != want {
		t.Fatalf("frames = %v, want [%q %q] (initial + exactly one replay)", frames, want, want)
	}
	if !ws.HasTopicApplied(user) {
		t.Fatal("user topic must be wire-applied after the replay")
	}
	if got := ws.pendingTopicCount(); got != 0 {
		t.Fatalf("pendingTopics = %d, want 0 after replay", got)
	}
}

// TestBadAuthCarriesLastWrittenGeneration — I2: an ERR_BADAUTH RESPONSE emits
// an *AuthError that still matches ErrBadAuth via errors.Is AND carries the
// credential generation this connection last wrote — per client, so two
// clients each report their own generation.
func TestBadAuthCarriesLastWrittenGeneration(t *testing.T) {
	badAuthFor := func(t *testing.T, index int, gen uint64) *AuthError {
		t.Helper()
		var mu sync.Mutex
		var got []error
		onError := func(err error) {
			mu.Lock()
			got = append(got, err)
			mu.Unlock()
		}
		provider := func() AuthSnapshot { return AuthSnapshot{Token: "tok", Generation: gen} }
		ws := NewWebSocketClient(index, provider, 60, 60, nil, onError)
		ws.writeTopicFrameHook = func(string, Topic) error { return nil }
		ws.mu.Lock()
		ws.isOpened = true
		ws.mu.Unlock()

		// A user-topic LISTEN stamps lastAuthGen with the written generation.
		if err := ws.Listen(NewTopic(TopicCommunityPointsUser, "42")); err != nil {
			t.Fatalf("Listen: %v", err)
		}

		ws.handleMessage(WSMessage{Type: "RESPONSE", Error: "ERR_BADAUTH"})

		mu.Lock()
		defer mu.Unlock()
		if len(got) != 1 {
			t.Fatalf("onError calls = %d, want exactly 1", len(got))
		}
		err := got[0]
		if !errors.Is(err, ErrBadAuth) {
			t.Fatalf("errors.Is(err, ErrBadAuth) = false for %v", err)
		}
		var ae *AuthError
		if !errors.As(err, &ae) {
			t.Fatalf("errors.As(*AuthError) = false for %T", err)
		}
		return ae
	}

	if ae := badAuthFor(t, 0, 7); ae.Generation != 7 {
		t.Fatalf("client 0 AuthError.Generation = %d, want 7", ae.Generation)
	}
	if ae := badAuthFor(t, 1, 3); ae.Generation != 3 {
		t.Fatalf("client 1 AuthError.Generation = %d, want 3 (its OWN last-written generation)", ae.Generation)
	}
}

// TestRelistenUserTopicsPreservesLedger — I3: the post-rotation sweep re-sends
// a LISTEN for user topics ONLY and never touches the topic ledger: channel
// topics get no frame, both topics stay applied exactly once, nothing is
// parked, and the capacity count is unchanged.
func TestRelistenUserTopicsPreservesLedger(t *testing.T) {
	rec := &authRotRecorder{}
	ws := newAuthSweepClient(0, StaticAuthToken("t"), rec)

	user := NewTopic(TopicCommunityPointsUser, "42")
	channel := NewTopic(TopicVideoPlaybackByID, "42")
	if err := ws.Listen(user); err != nil {
		t.Fatalf("Listen user: %v", err)
	}
	if err := ws.Listen(channel); err != nil {
		t.Fatalf("Listen channel: %v", err)
	}
	base := rec.frameCount()
	countBefore := ws.TopicCount()

	ws.RelistenUserTopics()

	swept := rec.framesSince(base)
	if len(swept) != 1 {
		t.Fatalf("sweep wrote %d frames (%v), want exactly 1", len(swept), swept)
	}
	if want := "LISTEN " + user.String(); swept[0] != want {
		t.Fatalf("sweep frame = %q, want %q (user topic only, never the channel topic)", swept[0], want)
	}
	if !ws.HasTopicApplied(user) || !ws.HasTopicApplied(channel) {
		t.Fatal("both topics must remain wire-applied after the sweep")
	}
	if got := ws.pendingTopicCount(); got != 0 {
		t.Fatalf("pendingTopics = %d, want 0 (sweep must not park anything)", got)
	}
	if got := ws.TopicCount(); got != countBefore {
		t.Fatalf("TopicCount = %d, want %d (ledger untouched)", got, countBefore)
	}
}

// TestRelistenWriteFailureStopsSweep — I4: a failing frame write stops the
// sweep after the first attempt without panicking and without touching the
// ledger — the reconnect replay owns recovery on a broken socket.
func TestRelistenWriteFailureStopsSweep(t *testing.T) {
	rec := &authRotRecorder{}
	ws := newAuthSweepClient(0, StaticAuthToken("t"), rec)

	user1 := NewTopic(TopicCommunityPointsUser, "42")
	user2 := NewTopic(TopicPredictionsUser, "42")
	if err := ws.Listen(user1); err != nil {
		t.Fatalf("Listen user1: %v", err)
	}
	if err := ws.Listen(user2); err != nil {
		t.Fatalf("Listen user2: %v", err)
	}
	baseAttempts := rec.attemptCount()
	countBefore := ws.TopicCount()

	rec.setFail(errors.New("write failed"))
	ws.RelistenUserTopics() // must not panic

	if got := rec.attemptCount() - baseAttempts; got != 1 {
		t.Fatalf("sweep attempted %d writes after the failure was armed, want exactly 1 (stop on first error)", got)
	}
	if !ws.HasTopicApplied(user1) || !ws.HasTopicApplied(user2) {
		t.Fatal("ledger changed: both user topics must remain wire-applied")
	}
	if got := ws.pendingTopicCount(); got != 0 {
		t.Fatalf("pendingTopics = %d, want 0 (failed sweep must not park topics)", got)
	}
	if got := ws.TopicCount(); got != countBefore {
		t.Fatalf("TopicCount = %d, want %d (ledger untouched by the failed sweep)", got, countBefore)
	}
}

// TestPoolReauthorizeSweepsAllClientsOnce — I5: one ReauthorizeUserTopics call
// sweeps EVERY pool connection exactly once — one LISTEN per client's applied
// user topic — and a second call writes exactly one more each (bounded and
// idempotent, never a storm).
func TestPoolReauthorizeSweepsAllClientsOnce(t *testing.T) {
	p := NewWebSocketPool(nil, StaticAuthToken("t"), nil, config.RateLimitSettings{
		WebsocketPingInterval: 60,
		ReconnectDelay:        60,
	})

	rec0, rec1 := &authRotRecorder{}, &authRotRecorder{}
	c0 := newAuthSweepClient(0, StaticAuthToken("t"), rec0)
	c1 := newAuthSweepClient(1, StaticAuthToken("t"), rec1)
	user0 := NewTopic(TopicCommunityPointsUser, "42")
	user1 := NewTopic(TopicPredictionsUser, "42")
	if err := c0.Listen(user0); err != nil {
		t.Fatalf("seed c0: %v", err)
	}
	if err := c1.Listen(user1); err != nil {
		t.Fatalf("seed c1: %v", err)
	}
	p.mu.Lock()
	p.clients = []*WebSocketClient{c0, c1}
	p.mu.Unlock()

	base0, base1 := rec0.frameCount(), rec1.frameCount()

	p.ReauthorizeUserTopics()

	if got := rec0.framesSince(base0); len(got) != 1 || got[0] != "LISTEN "+user0.String() {
		t.Fatalf("client 0 sweep frames = %v, want exactly one LISTEN of %s", got, user0)
	}
	if got := rec1.framesSince(base1); len(got) != 1 || got[0] != "LISTEN "+user1.String() {
		t.Fatalf("client 1 sweep frames = %v, want exactly one LISTEN of %s", got, user1)
	}

	// A second explicit sweep is bounded: exactly one more frame per client.
	p.ReauthorizeUserTopics()
	if got := len(rec0.framesSince(base0)); got != 2 {
		t.Fatalf("client 0 frames after second sweep = %d, want 2 (one per sweep, no storm)", got)
	}
	if got := len(rec1.framesSince(base1)); got != 2 {
		t.Fatalf("client 1 frames after second sweep = %d, want 2 (one per sweep, no storm)", got)
	}
}

// TestNoSweepWithoutRotation — I8: without an explicit ReauthorizeUserTopics
// call nothing sweeps — the pool and its clients write no frames beyond the
// setup LISTENs, guarding the wiring contract that sweeps only happen on an
// explicit rotation callback.
func TestNoSweepWithoutRotation(t *testing.T) {
	p := NewWebSocketPool(nil, StaticAuthToken("t"), nil, config.RateLimitSettings{
		WebsocketPingInterval: 60,
		ReconnectDelay:        60,
	})

	rec := &authRotRecorder{}
	c := newAuthSweepClient(0, StaticAuthToken("t"), rec)
	user := NewTopic(TopicCommunityPointsUser, "42")
	if err := c.Listen(user); err != nil {
		t.Fatalf("seed: %v", err)
	}
	p.mu.Lock()
	p.clients = []*WebSocketClient{c}
	p.mu.Unlock()

	if got := rec.attemptCount(); got != 1 {
		t.Fatalf("attempts after setup = %d, want exactly the 1 setup LISTEN — no spontaneous sweep", got)
	}
	if got := rec.frameCount(); got != 1 {
		t.Fatalf("frames after setup = %d, want 1 (no frames without an explicit ReauthorizeUserTopics)", got)
	}
}

// TestRelistenUserTopicsRaces: concurrent rotation sweeps, user/channel topic
// Listen/Unlisten churn, snapshot reads, and token rotations race cleanly
// (-race) on one opened client, and the final converged state holds each
// desired topic exactly once.
func TestRelistenUserTopicsRaces(t *testing.T) {
	auth := newRotatingAuth(AuthSnapshot{Token: "tok-1", Generation: 1})
	rec := &authRotRecorder{}
	ws := newAuthSweepClient(0, auth.provider, rec)

	topics := []Topic{
		NewTopic(TopicCommunityPointsUser, "42"),
		NewTopic(TopicPredictionsUser, "42"),
		NewTopic(TopicVideoPlaybackByID, "chan-1"),
		NewTopic(TopicRaid, "chan-1"),
	}

	var wg sync.WaitGroup
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				ws.RelistenUserTopics()
			}
		}()
	}
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				topic := topics[(g+i)%len(topics)]
				if (g+i)%2 == 0 {
					_ = ws.Listen(topic)
				} else {
					_ = ws.Unlisten(topic)
				}
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = ws.TopicCount()
			_ = ws.HasTopicApplied(topics[0])
			_ = ws.lastAuthGeneration()
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			auth.rotate(AuthSnapshot{Token: fmt.Sprintf("tok-%d", i), Generation: uint64(i)})
		}
	}()
	wg.Wait()

	// Converge deterministically: every topic desired, each present exactly once.
	for _, topic := range topics {
		if err := ws.Listen(topic); err != nil {
			t.Fatalf("converge %v: %v", topic, err)
		}
		if !ws.HasTopicApplied(topic) {
			t.Fatalf("topic %v must be wire-applied after convergence", topic)
		}
	}
	if got := ws.TopicCount(); got != len(topics) {
		t.Fatalf("TopicCount = %d, want %d (each desired topic exactly once, no duplicates)", got, len(topics))
	}
	if got := ws.pendingTopicCount(); got != 0 {
		t.Fatalf("pendingTopics = %d, want 0 after convergence", got)
	}
}
