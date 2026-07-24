package pubsub

import (
	"errors"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// nonceForTopic returns the outstanding LISTEN nonce recorded for a topic (the
// correlation an ERR_BADTOPIC RESPONSE carries), or "" if none is tracked.
func nonceForTopic(ws *WebSocketClient, topic Topic) string {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	for n, a := range ws.listenNonces {
		if a.topic.String() == topic.String() {
			return n
		}
	}
	return ""
}

// newFakeClientWithInvalidHandler builds a fake open client (no network) that
// records evicted topics, exercising the RESPONSE classification path directly.
func newFakeClientWithInvalidHandler(onError func(error)) (*WebSocketClient, *[]Topic) {
	ws := NewWebSocketClient(0, StaticAuthToken("tok"), 60, 60, nil, onError)
	ws.writeTopicFrameHook = func(string, Topic) error { return nil }
	ws.isOpened = true
	evicted := &[]Topic{}
	ws.SetInvalidTopicHandler(func(tp Topic) { *evicted = append(*evicted, tp) })
	return ws, evicted
}

// T9 — an authoritative ERR_BADTOPIC evicts the exact topic from the ledger,
// fires the quarantine handler once, and leaves nothing to re-LISTEN.
func TestPermanentInvalidTopicEvicted(t *testing.T) {
	ws, evicted := newFakeClientWithInvalidHandler(nil)
	topic := NewTopic(TopicVideoPlaybackByID, "bad-channel")

	if err := ws.Listen(topic); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	if !ws.HasTopicApplied(topic) {
		t.Fatal("setup: topic must be wire-applied before the rejection")
	}
	nonce := nonceForTopic(ws, topic)
	if nonce == "" {
		t.Fatal("setup: no LISTEN nonce recorded for the topic")
	}

	ws.handleMessage(WSMessage{Type: "RESPONSE", Error: "ERR_BADTOPIC", Nonce: nonce})

	if ws.HasTopic(topic) || ws.HasTopicApplied(topic) {
		t.Fatal("ERR_BADTOPIC must evict the topic from desired state")
	}
	if len(*evicted) != 1 || (*evicted)[0].String() != topic.String() {
		t.Fatalf("quarantine handler fired = %v, want exactly [%s]", *evicted, topic.String())
	}
	// Nothing left for a reconnect replay to re-LISTEN.
	if got := mergeTopics(ws.topics, ws.pendingTopics); len(got) != 0 {
		t.Fatalf("evicted topic must not survive into the reconnect replay set: %v", got)
	}
}

// T10 — a transient rejection (ERR_SERVER) is NOT permanent: the topic stays
// desired and retryable, and nothing is quarantined.
func TestTransientRejectionKeepsTopicDesired(t *testing.T) {
	ws, evicted := newFakeClientWithInvalidHandler(nil)
	topic := NewTopic(TopicRaid, "chan")

	if err := ws.Listen(topic); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	nonce := nonceForTopic(ws, topic)

	ws.handleMessage(WSMessage{Type: "RESPONSE", Error: "ERR_SERVER", Nonce: nonce})

	if !ws.HasTopic(topic) {
		t.Fatal("a transient rejection must leave the topic desired and retryable")
	}
	if len(*evicted) != 0 {
		t.Fatalf("a transient rejection must not quarantine any topic, got %v", *evicted)
	}
}

// T11 — ERR_BADAUTH routes through onError as an *AuthError (BKM-005 recovery)
// and never evicts or quarantines a topic.
func TestBadAuthRoutesToRecoveryNotEviction(t *testing.T) {
	var mu sync.Mutex
	var authErrs []error
	ws, evicted := newFakeClientWithInvalidHandler(func(err error) {
		mu.Lock()
		authErrs = append(authErrs, err)
		mu.Unlock()
	})
	user := NewTopic(TopicCommunityPointsUser, "42")

	if err := ws.Listen(user); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	nonce := nonceForTopic(ws, user)

	ws.handleMessage(WSMessage{Type: "RESPONSE", Error: "ERR_BADAUTH", Nonce: nonce})

	mu.Lock()
	defer mu.Unlock()
	if len(authErrs) != 1 {
		t.Fatalf("onError calls = %d, want exactly 1", len(authErrs))
	}
	if !errors.Is(authErrs[0], ErrBadAuth) {
		t.Fatalf("onError = %v, want an ErrBadAuth", authErrs[0])
	}
	if len(*evicted) != 0 {
		t.Fatalf("ERR_BADAUTH must not quarantine a topic, got %v", *evicted)
	}
	if !ws.HasTopic(user) {
		t.Fatal("ERR_BADAUTH must not evict the user topic")
	}
}

// T12 — a stale or unknown rejection cannot mutate desired state: neither an
// unknown nonce nor a rejection attributed to a superseded generation evicts.
func TestStaleOrUnknownRejectionEvictsNothing(t *testing.T) {
	ws, evicted := newFakeClientWithInvalidHandler(nil)

	// Unknown nonce.
	topic := NewTopic(TopicVideoPlaybackByID, "chan-a")
	if err := ws.Listen(topic); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	ws.handleMessage(WSMessage{Type: "RESPONSE", Error: "ERR_BADTOPIC", Nonce: "no-such-nonce"})
	if !ws.HasTopic(topic) {
		t.Fatal("an unknown-nonce rejection must not evict a topic")
	}

	// Superseded generation: the LISTEN was written under generation 1, but the
	// rejection is delivered to a reader that has since been superseded.
	topic2 := NewTopic(TopicVideoPlaybackByID, "chan-b")
	if err := ws.Listen(topic2); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	nonce2 := nonceForTopic(ws, topic2)
	ws.mu.Lock()
	readerGen := ws.connGen // the generation the frame was written under
	ws.connGen++            // a reconnect advanced the connection
	ws.mu.Unlock()
	ws.handleMessageForGen(WSMessage{Type: "RESPONSE", Error: "ERR_BADTOPIC", Nonce: nonce2}, readerGen)
	if !ws.HasTopic(topic2) {
		t.Fatal("a rejection from a superseded generation must not evict a topic")
	}
	if len(*evicted) != 0 {
		t.Fatalf("stale/unknown rejections must quarantine nothing, got %v", *evicted)
	}
}

// T12b (P10) — a disable followed by a re-enable of the SAME topic identity must
// not be evicted by the old attempt's late rejection.
func TestOldRejectionDoesNotEvictReDesiredTopic(t *testing.T) {
	ws, evicted := newFakeClientWithInvalidHandler(nil)
	topic := NewTopic(TopicRaid, "chan")

	if err := ws.Listen(topic); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	oldNonce := nonceForTopic(ws, topic)

	// Disable, then re-enable: the re-LISTEN drops the topic's prior nonce.
	if err := ws.Unlisten(topic); err != nil {
		t.Fatalf("Unlisten: %v", err)
	}
	if err := ws.Listen(topic); err != nil {
		t.Fatalf("re-Listen: %v", err)
	}
	if newNonce := nonceForTopic(ws, topic); newNonce == "" || newNonce == oldNonce {
		t.Fatalf("re-LISTEN must record a fresh nonce (old=%q new=%q)", oldNonce, newNonce)
	}

	// The old attempt's rejection arrives late.
	ws.handleMessage(WSMessage{Type: "RESPONSE", Error: "ERR_BADTOPIC", Nonce: oldNonce})

	if !ws.HasTopic(topic) {
		t.Fatal("a stale rejection of a superseded attempt must not evict the re-desired topic (P10)")
	}
	if len(*evicted) != 0 {
		t.Fatalf("no eviction expected, got %v", *evicted)
	}
}

// newQuarantinePool returns a fake-client pool whose factory wires the
// invalid-topic quarantine handler exactly as production connectNewClientLocked
// does, so end-to-end quarantine can be exercised without a network.
func newQuarantinePool() *WebSocketPool {
	p := &WebSocketPool{
		predictions: make(map[string]*models.EventPrediction),
		control:     make(map[string]*roundControl),
	}
	p.newClient = func(index int) (*WebSocketClient, error) {
		ws := newFakeOpenClient(index)
		ws.writeTopicFrameHook = func(string, Topic) error { return nil }
		ws.SetInvalidTopicHandler(p.quarantineTopic)
		return ws, nil
	}
	return p
}

// T9 (pool) — once a topic is quarantined, a desired re-drive is a converged
// no-op: it is never re-subscribed.
func TestQuarantinedTopicNotReSubscribed(t *testing.T) {
	p := newQuarantinePool()
	topic := NewTopic(TopicVideoPlaybackByID, "bad")

	if err := p.EnsureTopic(topic, true); err != nil {
		t.Fatalf("EnsureTopic: %v", err)
	}
	if topicInstances(p, topic) != 1 {
		t.Fatalf("setup: topic instances = %d, want 1", topicInstances(p, topic))
	}

	// Deliver ERR_BADTOPIC to the owning client, driving the pool quarantine.
	p.mu.RLock()
	client := p.clients[0]
	p.mu.RUnlock()
	nonce := nonceForTopic(client, topic)
	if nonce == "" {
		t.Fatal("no LISTEN nonce recorded on the pool client")
	}
	client.handleMessage(WSMessage{Type: "RESPONSE", Error: "ERR_BADTOPIC", Nonce: nonce})

	if topicInstances(p, topic) != 0 {
		t.Fatalf("quarantined topic must be evicted pool-wide, instances = %d", topicInstances(p, topic))
	}
	// A desired re-drive (e.g. a settings reconcile) must not re-LISTEN it.
	if err := p.EnsureTopic(topic, true); err != nil {
		t.Fatalf("EnsureTopic re-drive: %v", err)
	}
	if topicInstances(p, topic) != 0 {
		t.Fatalf("quarantine must survive a desired re-drive, instances = %d", topicInstances(p, topic))
	}
}

// T13 — a genuinely new topic identity is attempted normally even though a prior
// identity was quarantined (quarantine is keyed on the full topic identity).
func TestNewIdentityRetriedDespitePriorInvalid(t *testing.T) {
	p := newQuarantinePool()
	old := NewTopic(TopicVideoPlaybackByID, "old-channel")
	p.quarantineTopic(old)

	if err := p.EnsureTopic(old, true); err != nil {
		t.Fatalf("EnsureTopic(old): %v", err)
	}
	if topicInstances(p, old) != 0 {
		t.Fatalf("the quarantined identity must not be subscribed, instances = %d", topicInstances(p, old))
	}

	fresh := NewTopic(TopicVideoPlaybackByID, "new-channel")
	if err := p.EnsureTopic(fresh, true); err != nil {
		t.Fatalf("EnsureTopic(fresh): %v", err)
	}
	if topicInstances(p, fresh) != 1 {
		t.Fatalf("a genuinely new identity must be subscribed, instances = %d", topicInstances(p, fresh))
	}
}

// T14 — quarantining an invalid topic on one connection must not invalidate a
// healthy, different topic living on another connection.
func TestQuarantineIsolatedToTheInvalidTopic(t *testing.T) {
	p := &WebSocketPool{
		predictions: make(map[string]*models.EventPrediction),
		control:     make(map[string]*roundControl),
	}
	ws0 := newFakeOpenClient(0)
	ws0.writeTopicFrameHook = func(string, Topic) error { return nil }
	ws1 := newFakeOpenClient(1)
	ws1.writeTopicFrameHook = func(string, Topic) error { return nil }
	p.clients = []*WebSocketClient{ws0, ws1}

	bad := NewTopic(TopicVideoPlaybackByID, "bad")
	good := NewTopic(TopicRaid, "good")
	if err := ws0.Listen(bad); err != nil {
		t.Fatalf("Listen(bad): %v", err)
	}
	if err := ws1.Listen(good); err != nil {
		t.Fatalf("Listen(good): %v", err)
	}

	p.quarantineTopic(bad)

	if ws0.HasTopic(bad) {
		t.Fatal("the invalid topic must be evicted from its connection")
	}
	if !ws1.HasTopic(good) {
		t.Fatal("a healthy topic on another connection must be untouched by the quarantine")
	}
}

// T16 (topic side) — concurrent EnsureTopic, quarantine and RESPONSE handling
// stay race-free. Run under -race.
func TestQuarantineConcurrencyRace(t *testing.T) {
	p := newQuarantinePool()
	topics := []Topic{
		NewTopic(TopicRaid, "a"),
		NewTopic(TopicVideoPlaybackByID, "b"),
		NewTopic(TopicPredictionsChannel, "c"),
	}
	// Seed subscriptions so there are clients to hammer.
	for _, tp := range topics {
		if err := p.EnsureTopic(tp, true); err != nil {
			t.Fatalf("seed EnsureTopic: %v", err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			for _, tp := range topics {
				_ = p.EnsureTopic(tp, i%2 == 0)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			p.quarantineTopic(topics[i%len(topics)])
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 300; i++ {
			p.mu.RLock()
			clients := append([]*WebSocketClient(nil), p.clients...)
			p.mu.RUnlock()
			for _, c := range clients {
				c.handleMessage(WSMessage{Type: "RESPONSE", Error: "ERR_SERVER", Nonce: "n"})
			}
		}
	}()
	wg.Wait()
}
