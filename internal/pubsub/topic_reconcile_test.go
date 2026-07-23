package pubsub

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// newFakeOpenClient returns a client that behaves as connected but never
// dials: send() no-ops on the nil conn, so Listen/Unlisten topic bookkeeping
// is exercised without a network.
func newFakeOpenClient(index int) *WebSocketClient {
	ws := NewWebSocketClient(index, "", 60, 60, nil, nil)
	ws.isOpened = true
	return ws
}

// newReconcilePool returns a pool whose connection factory produces fake open
// clients, so Submit/EnsureTopic run their real bounded-connection logic
// deterministically.
func newReconcilePool() *WebSocketPool {
	p := &WebSocketPool{
		predictions: make(map[string]*models.EventPrediction),
		control:     make(map[string]*roundControl),
	}
	p.newClient = func(index int) (*WebSocketClient, error) { return newFakeOpenClient(index), nil }
	return p
}

// topicInstances counts how many pool connections own the topic — the
// duplicate detector for every reconciliation test.
func topicInstances(p *WebSocketPool, topic Topic) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, ws := range p.clients {
		if ws.HasTopic(topic) {
			n++
		}
	}
	return n
}

func clientCount(p *WebSocketPool) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.clients)
}

// TestEnsureTopicSubscribeIdempotent: enabling the same topic repeatedly
// yields exactly one subscription on exactly one connection.
func TestEnsureTopicSubscribeIdempotent(t *testing.T) {
	p := newReconcilePool()
	raid := NewTopic(TopicRaid, "chan-1")

	for i := 0; i < 3; i++ {
		if err := p.EnsureTopic(raid, true); err != nil {
			t.Fatalf("EnsureTopic #%d: %v", i, err)
		}
	}

	if got := topicInstances(p, raid); got != 1 {
		t.Fatalf("topic instances = %d, want exactly 1", got)
	}
	if got := clientCount(p); got != 1 {
		t.Fatalf("clients = %d, want 1", got)
	}
	if got := p.clients[0].TopicCount(); got != 1 {
		t.Fatalf("client topic count = %d, want 1 (no duplicate LISTEN bookkeeping)", got)
	}
}

// TestEnsureTopicUnsubscribeRemovesFromAllClients: disabling removes the topic
// from EVERY connection, cleaning up a historical duplicate, without touching
// other topics; repeating the disable is a safe no-op.
func TestEnsureTopicUnsubscribeRemovesFromAllClients(t *testing.T) {
	p := newReconcilePool()
	raid := NewTopic(TopicRaid, "chan-1")
	playback := NewTopic(TopicVideoPlaybackByID, "chan-1")

	// Seed a historical duplicate: the same topic on two connections.
	c0, c1 := newFakeOpenClient(0), newFakeOpenClient(1)
	c0.Listen(raid)
	c0.Listen(playback)
	c1.Listen(raid)
	p.mu.Lock()
	p.clients = []*WebSocketClient{c0, c1}
	p.mu.Unlock()

	if got := topicInstances(p, raid); got != 2 {
		t.Fatalf("setup: duplicate instances = %d, want 2", got)
	}

	if err := p.EnsureTopic(raid, false); err != nil {
		t.Fatalf("EnsureTopic(desired=false): %v", err)
	}
	if got := topicInstances(p, raid); got != 0 {
		t.Fatalf("after disable: instances = %d, want 0 on ALL clients", got)
	}
	if got := topicInstances(p, playback); got != 1 {
		t.Fatalf("unrelated topic was touched: playback instances = %d, want 1", got)
	}

	// Repeated disable: safe no-op.
	if err := p.EnsureTopic(raid, false); err != nil {
		t.Fatalf("repeated EnsureTopic(desired=false): %v", err)
	}
	if got := topicInstances(p, raid); got != 0 {
		t.Fatalf("after repeated disable: instances = %d, want 0", got)
	}
}

// TestEnsureTopicCapacityBoundaryNoDuplicate: when the connection owning the
// topic is full and newer connections have room, re-enabling the topic must
// no-op on the OLD owner instead of duplicating it onto a new connection.
func TestEnsureTopicCapacityBoundaryNoDuplicate(t *testing.T) {
	p := newReconcilePool()

	// Fill connection 0 to capacity; the raid topic is one of its members.
	raid := NewTopic(TopicRaid, "chan-0")
	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("seed raid: %v", err)
	}
	for i := 1; i < constants.MaxTopicsPerConnection; i++ {
		if err := p.EnsureTopic(NewTopic(TopicVideoPlaybackByID, fmt.Sprintf("chan-%d", i)), true); err != nil {
			t.Fatalf("fill #%d: %v", i, err)
		}
	}
	// Overflow onto a second connection.
	overflow := NewTopic(TopicVideoPlaybackByID, "chan-overflow")
	if err := p.EnsureTopic(overflow, true); err != nil {
		t.Fatalf("overflow: %v", err)
	}
	if got := clientCount(p); got != 2 {
		t.Fatalf("setup: clients = %d, want 2 (capacity boundary crossed)", got)
	}

	// The pool-wide duplicate check must see the topic on the FULL old client.
	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("re-ensure raid: %v", err)
	}
	if got := topicInstances(p, raid); got != 1 {
		t.Fatalf("raid instances = %d, want 1 (no duplicate on the newer connection)", got)
	}
	if p.clients[1].HasTopic(raid) {
		t.Fatal("raid topic leaked onto the newer connection despite existing on the full one")
	}
}

// TestEnsureTopicSubscribeFailureStaysRetryable: a failed connect surfaces the
// error, records the topic NOWHERE, and the identical retry succeeds with
// exactly one instance — the partial-failure healing contract.
func TestEnsureTopicSubscribeFailureStaysRetryable(t *testing.T) {
	p := newReconcilePool()
	dialErr := errors.New("dial failed")
	fail := true
	p.newClient = func(index int) (*WebSocketClient, error) {
		if fail {
			return nil, dialErr
		}
		return newFakeOpenClient(index), nil
	}

	raid := NewTopic(TopicRaid, "chan-1")
	if err := p.EnsureTopic(raid, true); !errors.Is(err, dialErr) {
		t.Fatalf("first attempt error = %v, want %v", err, dialErr)
	}
	if got := topicInstances(p, raid); got != 0 {
		t.Fatalf("failed attempt tracked the topic anyway: instances = %d", got)
	}
	if got := clientCount(p); got != 0 {
		t.Fatalf("failed attempt appended a client: %d", got)
	}

	fail = false
	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if got := topicInstances(p, raid); got != 1 {
		t.Fatalf("after retry: instances = %d, want exactly 1", got)
	}
}

// TestEnsureTopicLeavesUserTopicsAlone: per-channel reconciliation only ever
// names per-channel topics; a subscribed global user topic must survive any
// per-channel disable sweep for the same channel id.
func TestEnsureTopicLeavesUserTopicsAlone(t *testing.T) {
	p := newReconcilePool()
	user := NewTopic(TopicCommunityPointsUser, "user-1")
	predUser := NewTopic(TopicPredictionsUser, "user-1")
	if err := p.Submit(user); err != nil {
		t.Fatal(err)
	}
	if err := p.Submit(predUser); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []TopicType{TopicVideoPlaybackByID, TopicRaid, TopicPredictionsChannel, TopicCommunityMomentsChannel, TopicCommunityPointsChannel} {
		if err := p.EnsureTopic(NewTopic(tt, "user-1"), false); err != nil {
			t.Fatalf("disable %s: %v", tt, err)
		}
	}

	if topicInstances(p, user) != 1 || topicInstances(p, predUser) != 1 {
		t.Fatal("global user topics were disturbed by per-channel reconciliation")
	}
}

// TestUnsubscribeDuringReconnectNotResurrectedByReplay: a runtime capability
// disable landing while the owning connection is mid-reconnect (live set
// parked in pendingTopics) must stick — the post-connect replay re-checks the
// field per topic and must not resurrect the removed subscription.
func TestUnsubscribeDuringReconnectNotResurrectedByReplay(t *testing.T) {
	ws := NewWebSocketClient(0, "", 60, 60, nil, nil)
	raid := NewTopic(TopicRaid, "chan-1")
	keep := NewTopic(TopicVideoPlaybackByID, "chan-1")

	// Simulate the reconnect generation swap: live set parked, conn unusable.
	ws.mu.Lock()
	ws.pendingTopics = []Topic{raid, keep}
	ws.topics = nil
	ws.isOpened = false
	ws.mu.Unlock()

	// The disable arrives mid-reconnect.
	ws.Unlisten(raid)

	// The connection comes back and drains the parked set.
	ws.mu.Lock()
	ws.isOpened = true
	ws.mu.Unlock()
	ws.replayPendingTopics()

	if ws.HasTopic(raid) {
		t.Fatal("disabled topic was resurrected by the reconnect replay")
	}
	if !ws.HasTopic(keep) {
		t.Fatal("kept topic was lost by the reconnect replay")
	}
	if got := ws.TopicCount(); got != 1 {
		t.Fatalf("topic count = %d, want 1", got)
	}
}

// TestTopicListenDuringReconnectParksUntilReplay: a Listen arriving while the
// connection is mid-reconnect must PARK the topic (no live claim, no frame to
// the dead conn); the post-connect replay promotes it exactly once, and the
// parked topic still counts toward connection capacity and dedup scans.
func TestTopicListenDuringReconnectParksUntilReplay(t *testing.T) {
	ws := NewWebSocketClient(0, "", 60, 60, nil, nil)
	topic := NewTopic(TopicRaid, "chan-1")

	// Mid-reconnect (or pre-connect): the conn is not usable.
	ws.Listen(topic)

	ws.mu.RLock()
	live, parked := len(ws.topics), len(ws.pendingTopics)
	ws.mu.RUnlock()
	if live != 0 || parked != 1 {
		t.Fatalf("live=%d parked=%d, want 0/1 (park only, no live claim before the frame can be sent)", live, parked)
	}
	if !ws.HasTopic(topic) {
		t.Fatal("parked topic invisible to the pool-wide dedup scan")
	}
	if got := ws.TopicCount(); got != 1 {
		t.Fatalf("capacity accounting ignores parked topics: count=%d, want 1", got)
	}

	// Repeated Listen while parked: no duplicate.
	ws.Listen(topic)
	if got := ws.TopicCount(); got != 1 {
		t.Fatalf("duplicate park: count=%d, want 1", got)
	}

	// Connection established: replay promotes exactly once.
	ws.mu.Lock()
	ws.isOpened = true
	ws.mu.Unlock()
	ws.replayPendingTopics()

	ws.mu.RLock()
	live, parked = len(ws.topics), len(ws.pendingTopics)
	ws.mu.RUnlock()
	if live != 1 || parked != 0 {
		t.Fatalf("after replay live=%d parked=%d, want 1/0", live, parked)
	}

	// Repeated Listen after promotion: still no duplicate.
	ws.Listen(topic)
	if got := ws.TopicCount(); got != 1 {
		t.Fatalf("duplicate after promotion: count=%d, want 1", got)
	}
}

// TestConcurrentEnsureTopicAndSnapshotsNoDuplicate: concurrent reconciles,
// pool snapshots, message handling and settings writes race cleanly (-race)
// and converge to exactly one instance per desired topic.
func TestConcurrentEnsureTopicAndSnapshotsNoDuplicate(t *testing.T) {
	p := newReconcilePool()
	s := newTestStreamer(1000)
	p.UpdateStreamers([]*models.Streamer{s})

	topics := []Topic{
		NewTopic(TopicVideoPlaybackByID, "chan-1"),
		NewTopic(TopicRaid, "chan-1"),
		NewTopic(TopicPredictionsChannel, "chan-1"),
	}

	var wg sync.WaitGroup
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				topic := topics[(g+i)%len(topics)]
				_ = p.EnsureTopic(topic, (g+i)%3 != 0)
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			_ = p.ConnSnapshot()
			_ = p.PredictionsSnapshot()
			p.handleMessage(&PubSubMessage{
				Topic:     Topic{Type: TopicVideoPlaybackByID, ChannelID: "chan-1"},
				ChannelID: "chan-1",
				Type:      "viewcount",
			})
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			next := s.GetSettings()
			next.FollowRaid = i%2 == 0
			s.SetSettings(next)
		}
	}()
	wg.Wait()

	// Converge deterministically and verify the invariant.
	for _, topic := range topics {
		if err := p.EnsureTopic(topic, true); err != nil {
			t.Fatalf("converge %v: %v", topic, err)
		}
		if got := topicInstances(p, topic); got != 1 {
			t.Fatalf("topic %v instances = %d, want exactly 1", topic, got)
		}
	}
}
