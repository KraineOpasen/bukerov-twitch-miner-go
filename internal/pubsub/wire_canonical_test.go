package pubsub

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// newWireClientAt returns an open recorder-backed client with the given pool
// index (each client gets its OWN recorder so frames are attributable).
func newWireClientAt(rec *wireRecorder, index int) *WebSocketClient {
	ws := NewWebSocketClient(index, nil, 60, 60, nil, nil)
	ws.isOpened = true
	ws.writeTopicFrameHook = rec.hook
	return ws
}

// newTwoClientWirePool builds a pool of two recorder-backed clients; client 1
// is the pool's LAST client (the legacy fallback target for new
// subscriptions).
func newTwoClientWirePool() (*WebSocketPool, *WebSocketClient, *WebSocketClient, *wireRecorder, *wireRecorder) {
	rec0, rec1 := newWireRecorder(), newWireRecorder()
	c0, c1 := newWireClientAt(rec0, 0), newWireClientAt(rec1, 1)
	p := &WebSocketPool{
		predictions: make(map[string]*models.EventPrediction),
		control:     make(map[string]*roundControl),
	}
	p.mu.Lock()
	p.clients = []*WebSocketClient{c0, c1}
	p.mu.Unlock()
	return p, c0, c1, rec0, rec1
}

func (r *wireRecorder) totalAttempts() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.attempts)
}

// poolDesiredOwners counts clients that desire the topic (applied or pending).
func poolDesiredOwners(p *WebSocketPool, topic Topic) int {
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

// poolAppliedOwners counts clients with the topic wire-applied.
func poolAppliedOwners(p *WebSocketPool, topic Topic) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, ws := range p.clients {
		if ws.HasTopicApplied(topic) {
			n++
		}
	}
	return n
}

// poolDebts sums failed-UNLISTEN debts across all clients.
func poolDebts(p *WebSocketPool) int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	n := 0
	for _, ws := range p.clients {
		n += ws.unlistenRetryCount()
	}
	return n
}

// seedDebt puts the topic into wire-applied state on the client and then
// fails its UNLISTEN once via the pool disable path, leaving exactly one
// failed-UNLISTEN debt on that client.
func seedDebt(t *testing.T, p *WebSocketPool, ws *WebSocketClient, rec *wireRecorder, topic Topic) {
	t.Helper()
	if err := ws.Listen(topic); err != nil {
		t.Fatalf("seed listen: %v", err)
	}
	wireErr := errors.New("write failed")
	rec.mu.Lock()
	rec.failOnce[wireKey("UNLISTEN", topic)] = wireErr
	rec.mu.Unlock()
	if err := p.EnsureTopic(topic, false); !errors.Is(err, wireErr) {
		t.Fatalf("seed disable error = %v, want %v", err, wireErr)
	}
	if got := ws.unlistenRetryCount(); got != 1 {
		t.Fatalf("seed: debt = %d, want 1", got)
	}
	if ws.HasTopic(topic) || ws.HasTopicApplied(topic) {
		t.Fatal("seed: topic must be locally absent after the failed disable")
	}
}

// TestWireDebtReEnableStaysOnOwningClient — T7: a re-enable after a failed
// UNLISTEN whose owner is NOT the pool's last client must drive the LISTEN on
// the owning client (whose wire subscription may still exist), never migrate
// the topic to the last client, and settle the debt.
func TestWireDebtReEnableStaysOnOwningClient(t *testing.T) {
	p, c0, c1, rec0, rec1 := newTwoClientWirePool()
	raid := NewTopic(TopicRaid, "chan-1")

	seedDebt(t, p, c0, rec0, raid)

	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}

	if !c0.HasTopicApplied(raid) {
		t.Fatal("re-enable must land on the debt-owning client 0")
	}
	if got := rec1.attemptCountFor("LISTEN", raid); got != 0 {
		t.Fatalf("client 1 received %d LISTEN writes — topic migrated off the debt owner", got)
	}
	if c1.HasTopic(raid) || c1.HasTopicApplied(raid) {
		t.Fatal("client 1 must not own the topic")
	}
	if got := poolAppliedOwners(p, raid); got != 1 {
		t.Fatalf("applied owners = %d, want exactly 1", got)
	}
	if got := poolDesiredOwners(p, raid); got != 1 {
		t.Fatalf("desired owners = %d, want exactly 1", got)
	}
	if got := poolDebts(p); got != 0 {
		t.Fatalf("pool-wide debts = %d, want 0 (settled by the LISTEN on the same client)", got)
	}
	if last := rec0.lastWriteFor(raid); last != "LISTEN" {
		t.Fatalf("final wire command on the owner = %q, want LISTEN", last)
	}

	// Identical apply after convergence writes nothing anywhere.
	a0, a1 := rec0.totalAttempts(), rec1.totalAttempts()
	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("identical apply: %v", err)
	}
	if rec0.totalAttempts() != a0 || rec1.totalAttempts() != a1 {
		t.Fatal("identical apply after convergence wrote extra frames")
	}
}

// TestCanonicalApplySettlesForeignDebt — T8: when one client is already
// wire-applied and ANOTHER client still owes a failed UNLISTEN for the same
// topic, desired=true must not early-return on the applied owner — the
// foreign debt must be retried and settled.
func TestCanonicalApplySettlesForeignDebt(t *testing.T) {
	p, c0, c1, rec0, _ := newTwoClientWirePool()
	raid := NewTopic(TopicRaid, "chan-1")

	seedDebt(t, p, c0, rec0, raid)
	if err := c1.Listen(raid); err != nil {
		t.Fatalf("apply on client 1: %v", err)
	}

	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("EnsureTopic(true): %v", err)
	}

	if !c1.HasTopicApplied(raid) {
		t.Fatal("client 1 must stay the canonical applied owner")
	}
	if c0.HasTopic(raid) || c0.HasTopicApplied(raid) {
		t.Fatal("client 0 must end absent")
	}
	if got := poolDebts(p); got != 0 {
		t.Fatalf("foreign debt not settled: pool-wide debts = %d, want 0", got)
	}
	if got := poolAppliedOwners(p, raid); got != 1 {
		t.Fatalf("applied owners = %d, want exactly 1 effective subscription", got)
	}
	if last := rec0.lastWriteFor(raid); last != "UNLISTEN" {
		t.Fatalf("client 0 final wire command = %q, want UNLISTEN (debt retried)", last)
	}
}

// TestCanonicalApplyCleansHistoricalAppliedDuplicate — T9: two clients
// wire-applied with the same topic; desired=true keeps one deterministic
// canonical owner and UNLISTENs the other; the repeat apply is a no-op.
func TestCanonicalApplyCleansHistoricalAppliedDuplicate(t *testing.T) {
	p, c0, c1, rec0, rec1 := newTwoClientWirePool()
	raid := NewTopic(TopicRaid, "chan-1")

	if err := c0.Listen(raid); err != nil {
		t.Fatal(err)
	}
	if err := c1.Listen(raid); err != nil {
		t.Fatal(err)
	}
	if got := poolAppliedOwners(p, raid); got != 2 {
		t.Fatalf("setup: applied owners = %d, want 2", got)
	}

	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("EnsureTopic(true): %v", err)
	}

	if !c0.HasTopicApplied(raid) {
		t.Fatal("first applied client must stay the canonical owner")
	}
	if c1.HasTopic(raid) || c1.HasTopicApplied(raid) {
		t.Fatal("duplicate owner must be cleaned")
	}
	if last := rec1.lastWriteFor(raid); last != "UNLISTEN" {
		t.Fatalf("duplicate owner final wire command = %q, want UNLISTEN", last)
	}
	if got := poolAppliedOwners(p, raid); got != 1 {
		t.Fatalf("applied owners = %d, want exactly 1", got)
	}
	if got := poolDebts(p); got != 0 {
		t.Fatalf("debts = %d, want 0", got)
	}

	a0, a1 := rec0.totalAttempts(), rec1.totalAttempts()
	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("repeat apply: %v", err)
	}
	if rec0.totalAttempts() != a0 || rec1.totalAttempts() != a1 {
		t.Fatal("repeat apply after convergence wrote extra frames")
	}
}

// TestCanonicalSweepFailureStaysRetryable — T10: the canonical owner succeeds
// but the cleanup UNLISTEN on the other client fails; EnsureTopic(true)
// surfaces the error, keeps the cleanup retryable, and the identical apply
// converges to one owner with zero debts.
func TestCanonicalSweepFailureStaysRetryable(t *testing.T) {
	p, c0, c1, _, rec1 := newTwoClientWirePool()
	raid := NewTopic(TopicRaid, "chan-1")

	if err := c0.Listen(raid); err != nil {
		t.Fatal(err)
	}
	if err := c1.Listen(raid); err != nil {
		t.Fatal(err)
	}

	wireErr := errors.New("write failed")
	rec1.mu.Lock()
	rec1.failOnce[wireKey("UNLISTEN", raid)] = wireErr
	rec1.mu.Unlock()

	if err := p.EnsureTopic(raid, true); !errors.Is(err, wireErr) {
		t.Fatalf("EnsureTopic(true) error = %v, want %v (cleanup failure must surface)", err, wireErr)
	}
	if !c0.HasTopicApplied(raid) {
		t.Fatal("canonical owner must stay applied despite the cleanup failure")
	}
	if got := c1.unlistenRetryCount(); got != 1 {
		t.Fatalf("cleanup failure must record a retryable debt on client 1, got %d", got)
	}

	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("identical apply must retry the cleanup: %v", err)
	}
	if got := poolAppliedOwners(p, raid); got != 1 {
		t.Fatalf("applied owners = %d, want exactly 1", got)
	}
	if got := poolDebts(p); got != 0 {
		t.Fatalf("debts = %d, want 0 after the retried cleanup", got)
	}
	if last := rec1.lastWriteFor(raid); last != "UNLISTEN" {
		t.Fatalf("client 1 final wire command = %q, want UNLISTEN", last)
	}
}

// TestWireDebtCapacityBoundaryNoMigration — T11: the debt owner sits on a
// FULL old client while the newer last client has room; the re-enable must
// still stay on the debt owner and never create a duplicate via the capacity
// fallback.
func TestWireDebtCapacityBoundaryNoMigration(t *testing.T) {
	p, c0, c1, rec0, rec1 := newTwoClientWirePool()
	raid := NewTopic(TopicRaid, "chan-0")

	// Fill client 0 to capacity, raid included.
	if err := c0.Listen(raid); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < constants.MaxTopicsPerConnection; i++ {
		if err := c0.Listen(NewTopic(TopicVideoPlaybackByID, fmt.Sprintf("chan-%d", i))); err != nil {
			t.Fatalf("fill #%d: %v", i, err)
		}
	}
	if got := c0.TopicCount(); got != constants.MaxTopicsPerConnection {
		t.Fatalf("setup: client 0 count = %d, want full %d", got, constants.MaxTopicsPerConnection)
	}

	// Fail the raid UNLISTEN once: client 0 keeps the debt, still full.
	wireErr := errors.New("write failed")
	rec0.mu.Lock()
	rec0.failOnce[wireKey("UNLISTEN", raid)] = wireErr
	rec0.mu.Unlock()
	if err := p.EnsureTopic(raid, false); !errors.Is(err, wireErr) {
		t.Fatalf("disable error = %v, want %v", err, wireErr)
	}
	if got := c0.unlistenRetryCount(); got != 1 {
		t.Fatalf("debt = %d, want 1", got)
	}

	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("re-enable: %v", err)
	}

	if !c0.HasTopicApplied(raid) {
		t.Fatal("re-enable must stay on the full debt-owning client")
	}
	if got := rec1.attemptCountFor("LISTEN", raid); got != 0 {
		t.Fatalf("capacity fallback migrated the topic: client 1 got %d LISTEN writes", got)
	}
	if c1.HasTopic(raid) {
		t.Fatal("client 1 must not own the topic")
	}
	if got := poolAppliedOwners(p, raid); got != 1 {
		t.Fatalf("applied owners = %d, want exactly 1 (no capacity duplicate)", got)
	}
	if got := c0.TopicCount(); got != constants.MaxTopicsPerConnection {
		t.Fatalf("client 0 count = %d, want %d (debt slot converted back, no leak)", got, constants.MaxTopicsPerConnection)
	}
	if got := poolDebts(p); got != 0 {
		t.Fatalf("debts = %d, want 0", got)
	}
}

// TestWireDebtConcurrentTogglesConverge — T12: with two clients and a seeded
// failed-UNLISTEN debt, concurrent toggles, replay and snapshots race cleanly
// (-race); the final explicit desired=true converges to exactly one applied
// owner with no pending/debt duplicates anywhere.
func TestWireDebtConcurrentTogglesConverge(t *testing.T) {
	p, c0, c1, rec0, _ := newTwoClientWirePool()
	raid := NewTopic(TopicRaid, "chan-1")

	seedDebt(t, p, c0, rec0, raid)

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				_ = p.EnsureTopic(raid, (g+i)%2 == 0)
			}
		}(g)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			c0.replayPendingTopics()
			c1.replayPendingTopics()
			_ = p.ConnSnapshot()
		}
	}()
	wg.Wait()

	if err := p.EnsureTopic(raid, true); err != nil {
		t.Fatalf("final enable: %v", err)
	}

	if got := poolAppliedOwners(p, raid); got != 1 {
		t.Fatalf("applied owners = %d, want exactly 1", got)
	}
	if got := poolDesiredOwners(p, raid); got != 1 {
		t.Fatalf("desired owners = %d, want exactly 1 (no pending duplicates)", got)
	}
	if got := poolDebts(p); got != 0 {
		t.Fatalf("pool-wide debts = %d, want 0", got)
	}
}
