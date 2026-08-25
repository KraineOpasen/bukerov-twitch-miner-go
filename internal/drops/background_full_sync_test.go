package drops

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// backgroundFullSyncClient exposes deterministic gates at the production
// ViewerDropsDashboard boundary. Holding one request there also holds
// fullSyncMu, which lets the tests order background wake selection, lifecycle
// cancellation, and timer/resync collisions without sleep-based guesses.
type backgroundFullSyncClient struct {
	mu sync.Mutex

	gqlCalls       int
	dashboardCalls int
	nextGate       *backgroundFullSyncGate
}

type backgroundFullSyncGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func newBackgroundFullSyncGate() *backgroundFullSyncGate {
	return &backgroundFullSyncGate{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (g *backgroundFullSyncGate) open() {
	g.once.Do(func() { close(g.release) })
}

func (c *backgroundFullSyncClient) armDashboardGate(t *testing.T) *backgroundFullSyncGate {
	t.Helper()
	g := newBackgroundFullSyncGate()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.nextGate != nil {
		t.Fatal("background full-sync test client already has a pending dashboard gate")
	}
	c.nextGate = g
	return g
}

func (c *backgroundFullSyncClient) PostGQL(op constants.GQLOperation) (map[string]interface{}, error) {
	c.mu.Lock()
	c.gqlCalls++
	c.mu.Unlock()

	switch op.OperationName {
	case "ViewerDropsDashboard":
		c.mu.Lock()
		c.dashboardCalls++
		gate := c.nextGate
		c.nextGate = nil
		c.mu.Unlock()
		if gate != nil {
			close(gate.entered)
			<-gate.release
		}
		return dashboardResponse(), nil
	case "Inventory":
		return emptyInventoryResponse(), nil
	default:
		return map[string]interface{}{}, nil
	}
}

func (*backgroundFullSyncClient) GetDropCampaignDetails(string) (map[string]interface{}, error) {
	return map[string]interface{}{}, nil
}

func (*backgroundFullSyncClient) ClaimDrop(*models.Drop) (twitch.ClaimStatus, error) {
	return twitch.ClaimStatusRejected, nil
}

func (c *backgroundFullSyncClient) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dashboardCalls
}

func (c *backgroundFullSyncClient) totalGQLCalls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gqlCalls
}

type backgroundFullSyncHarness struct {
	tracker *DropsTracker
	client  *backgroundFullSyncClient
	cancel  context.CancelFunc

	ownerRelease chan struct{}
	ownerOnce    sync.Once
	stopOnce     sync.Once
}

func newBackgroundFullSyncHarness(t *testing.T, campaignInterval int) *backgroundFullSyncHarness {
	t.Helper()
	client := &backgroundFullSyncClient{}
	initialGate := client.armDashboardGate(t)
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{
		CampaignSyncInterval:     campaignInterval,
		DropProgressSyncInterval: 100_000,
	}, nil)
	tracker.intervalUnit = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	h := &backgroundFullSyncHarness{
		tracker:      tracker,
		client:       client,
		cancel:       cancel,
		ownerRelease: make(chan struct{}),
	}
	t.Cleanup(h.stop)

	tracker.Start(ctx)
	waitBackgroundFullSyncSignal(t, initialGate.entered, "startup full sync did not reach the dashboard gate")

	ownerAcquired := make(chan struct{})
	go holdBackgroundFullSyncOwner(tracker, ownerAcquired, h.ownerRelease)
	waitForBackgroundFullSyncStack(t, "holdBackgroundFullSyncOwner", "sync.Mutex.Lock")

	initialGate.open()
	waitBackgroundFullSyncSignal(t, ownerAcquired, "test owner did not acquire fullSyncMu after startup sync")
	if got := client.calls(); got != 1 {
		t.Fatalf("startup dashboard calls=%d, want exactly 1 before background-wake exercise", got)
	}
	return h
}

//go:noinline
func holdBackgroundFullSyncOwner(tracker *DropsTracker, acquired chan<- struct{}, release <-chan struct{}) {
	tracker.fullSyncMu.Lock()
	close(acquired)
	<-release
	tracker.fullSyncMu.Unlock()
}

func (h *backgroundFullSyncHarness) releaseOwner() {
	h.ownerOnce.Do(func() { close(h.ownerRelease) })
}

func (h *backgroundFullSyncHarness) stop() {
	h.stopOnce.Do(func() {
		h.cancel()
		h.releaseOwner()
		h.tracker.Stop()
	})
}

func waitBackgroundFullSyncSignal(t *testing.T, ch <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal(failure)
	}
}

// waitForBackgroundFullSyncStack waits for an observed goroutine state, not a
// guessed dwell. It is used only to prove that loop() has already accepted a
// wake and is blocked specifically on fullSyncMu before the test cancels or
// injects a colliding resync.
func waitForBackgroundFullSyncStack(t *testing.T, markers ...string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		dump := make([]byte, 1<<20)
		n := runtime.Stack(dump, true)
		text := string(dump[:n])
		for _, goroutine := range strings.Split(text, "\n\n") {
			matched := true
			for _, marker := range markers {
				if !strings.Contains(goroutine, marker) {
					matched = false
					break
				}
			}
			if matched {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not observe goroutine stack markers %q; final stacks:\n%s", markers, text)
		}
		runtime.Gosched()
	}
}

func waitForLoopFullSyncWaiter(t *testing.T) {
	t.Helper()
	waitForBackgroundFullSyncStack(t, "(*DropsTracker).loop", "sync.Mutex.Lock")
}

func TestBackgroundFullSyncWakeRechecksLifecycleAfterFullSyncLock(t *testing.T) {
	tests := []struct {
		name             string
		campaignInterval int
		wake             func(*DropsTracker)
	}{
		{
			name:             "ordinary_timer",
			campaignInterval: 1,
			wake:             func(*DropsTracker) {},
		},
		{
			name:             "campaign_resync",
			campaignInterval: 100_000,
			wake:             (*DropsTracker).triggerCampaignResync,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newBackgroundFullSyncHarness(t, tc.campaignInterval)
			tc.wake(h.tracker)
			waitForLoopFullSyncWaiter(t)

			before := h.tracker.SyncStatus()
			beforeGQL := h.client.totalGQLCalls()
			h.cancel()
			h.releaseOwner()
			h.stop()

			if got := h.client.totalGQLCalls(); got != beforeGQL {
				t.Fatalf("cancelled queued background wake started fresh GQL work: calls=%d, want unchanged %d", got, beforeGQL)
			}
			if got := h.client.calls(); got != 1 {
				t.Fatalf("cancelled queued background wake started fresh dashboard work: calls=%d, want 1 startup call", got)
			}
			after := h.tracker.SyncStatus()
			if after.Runs != before.Runs || after.Revision != before.Revision {
				t.Fatalf("cancelled queued background wake published state: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestOrdinaryTimerFullSyncCoalescesPendingCampaignResync(t *testing.T) {
	h := newBackgroundFullSyncHarness(t, 1)
	waitForLoopFullSyncWaiter(t)

	// The timer wake is already accepted and queued on fullSyncMu. Make the next
	// cadence long, then queue one resync that this covering run must consume.
	h.tracker.UpdateSettings(config.RateLimitSettings{
		CampaignSyncInterval:     100_000,
		DropProgressSyncInterval: 100_000,
	})
	h.tracker.triggerCampaignResync()
	if got := len(h.tracker.campaignResync); got != 1 {
		t.Fatalf("test setup queued campaignResync=%d, want 1", got)
	}

	coveringGate := h.client.armDashboardGate(t)
	t.Cleanup(coveringGate.open)
	h.releaseOwner()
	waitBackgroundFullSyncSignal(t, coveringGate.entered, "timer-owned covering full sync did not reach dashboard")

	// The loop cannot consume campaignResync while its covering request is gated.
	// Therefore a remaining token here proves the timer owner failed to coalesce
	// it and will run an immediate duplicate after this request returns.
	if got := len(h.tracker.campaignResync); got != 0 {
		t.Fatalf("timer-owned covering sync left %d already-covered campaignResync pending", got)
	}

	coveringGate.open()
	h.stop()
	if got := h.client.calls(); got != 2 {
		t.Fatalf("timer/resync collision dashboard calls=%d, want startup + one covering sync", got)
	}
}

func TestOrdinaryTimerFullSyncPreservesLaterCampaignResync(t *testing.T) {
	h := newBackgroundFullSyncHarness(t, 1)
	waitForLoopFullSyncWaiter(t)
	h.tracker.UpdateSettings(config.RateLimitSettings{
		CampaignSyncInterval:     100_000,
		DropProgressSyncInterval: 100_000,
	})

	coveringGate := h.client.armDashboardGate(t)
	t.Cleanup(coveringGate.open)
	h.releaseOwner()
	waitBackgroundFullSyncSignal(t, coveringGate.entered, "timer-owned covering full sync did not reach dashboard")

	// This trigger is genuinely newer: it arrives after the timer owner has
	// acquired fullSyncMu and crossed its coalescing point into network work.
	h.tracker.triggerCampaignResync()
	if got := len(h.tracker.campaignResync); got != 1 {
		t.Fatalf("later campaignResync queued=%d, want 1", got)
	}
	lateGate := h.client.armDashboardGate(t)
	t.Cleanup(lateGate.open)
	coveringGate.open()
	waitBackgroundFullSyncSignal(t, lateGate.entered, "later campaignResync was lost instead of starting its required sync")

	if got := h.client.calls(); got != 3 {
		t.Fatalf("later campaignResync dashboard calls=%d, want startup + timer + later resync", got)
	}
	h.cancel()
	lateGate.open()
	h.stop()
}

func TestDirectSyncNowStillRunsWithCancelledTrackerContext(t *testing.T) {
	client := &backgroundFullSyncClient{}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tracker.mu.Lock()
	tracker.ctx = ctx
	tracker.mu.Unlock()

	tracker.SyncNow()
	if got := client.calls(); got != 1 {
		t.Fatalf("direct SyncNow dashboard calls=%d, want 1 despite cancelled tracker context", got)
	}
}
