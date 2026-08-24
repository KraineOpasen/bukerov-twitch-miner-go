package drops

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/constants"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// TestLoopWakesForUpcomingCampaignStartAt is the behavioral regression for the
// campaign-start blind window. The initial full sync learns a future StartAt,
// while the ordinary cadence is effectively disabled. The existing full-sync
// owner must run once more when that boundary arrives; the lightweight loop is
// intentionally unable to discover the newly active campaign.
func TestLoopWakesForUpcomingCampaignStartAt(t *testing.T) {
	// Campaign timestamps are serialized as RFC3339 seconds in the production
	// fixture shape. Truncate first, then add three seconds, which guarantees at
	// least two whole seconds of headroom for startup on a loaded race build.
	startAt := time.Now().Truncate(time.Second).Add(3 * time.Second)
	summary, detail := campaignSummaryDetail(
		"startat-red", "StartAt RED", "UPCOMING", "game-red", "StartAt Game",
		startAt, startAt.Add(time.Hour),
	)
	signal := make(chan struct{}, 8)
	client := &fakeDropsClient{
		dashboard:      dashboardResponse(summary),
		inventory:      emptyInventoryResponse(),
		details:        map[string]map[string]interface{}{"startat-red": detail},
		fullSyncSignal: signal,
	}
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{
		CampaignSyncInterval:     100_000,
		DropProgressSyncInterval: 100_000,
	}, nil)
	tracker.intervalUnit = time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	tracker.Start(ctx)
	t.Cleanup(func() {
		cancel()
		tracker.Stop()
	})

	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("startup full sync did not run")
	}
	waitForUpcomingCampaign(t, tracker, "startat-red")

	if earlyWait := time.Until(startAt) / 2; earlyWait > 0 {
		select {
		case <-signal:
			t.Fatal("StartAt wake ran before the campaign start boundary")
		case <-time.After(earlyWait):
		}
	}

	select {
	case <-signal:
		if time.Now().Before(startAt) {
			t.Fatalf("StartAt wake ran early: now=%v startAt=%v", time.Now(), startAt)
		}
	case <-time.After(time.Until(startAt) + time.Second):
		t.Fatal("known future StartAt did not wake the full campaign-sync owner")
	}
}

func waitForUpcomingCampaign(t *testing.T, tracker *DropsTracker, campaignID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, campaign := range tracker.UpcomingCampaigns() {
			if campaign.ID == campaignID {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("startup sync did not publish upcoming campaign %q", campaignID)
}

// startAtFakeClock drives only the full campaign-sync scheduler. The
// lightweight progress loop deliberately keeps its production timer, which
// makes these tests a falsifier for accidental global cadence acceleration.
type startAtFakeClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*startAtFakeTimer
	created chan *startAtFakeTimer
}

type startAtFakeTimer struct {
	clock    *startAtFakeClock
	deadline time.Time
	ch       chan time.Time
	stopped  bool
	fired    bool
}

func newStartAtFakeClock(now time.Time) *startAtFakeClock {
	return &startAtFakeClock{
		now:     now,
		created: make(chan *startAtFakeTimer, 512),
	}
}

func (c *startAtFakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *startAtFakeClock) NewTimer(delay time.Duration) campaignSchedulerTimer {
	c.mu.Lock()
	fireNow := delay <= 0
	timer := &startAtFakeTimer{
		clock:    c,
		deadline: c.now.Add(delay),
		ch:       make(chan time.Time, 1),
	}
	c.timers = append(c.timers, timer)
	if fireNow {
		timer.fired = true
	}
	c.mu.Unlock()

	if fireNow {
		timer.ch <- timer.deadline
	}
	c.created <- timer
	return timer
}

func (t *startAtFakeTimer) C() <-chan time.Time { return t.ch }

func (t *startAtFakeTimer) Stop() bool {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

func (t *startAtFakeTimer) snapshot() (deadline time.Time, stopped, fired bool) {
	t.clock.mu.Lock()
	defer t.clock.mu.Unlock()
	return t.deadline, t.stopped, t.fired
}

func (c *startAtFakeClock) Advance(delta time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delta)
	now := c.now
	due := make([]*startAtFakeTimer, 0)
	for _, timer := range c.timers {
		if timer.stopped || timer.fired || timer.deadline.After(now) {
			continue
		}
		timer.fired = true
		due = append(due, timer)
	}
	c.mu.Unlock()

	for _, timer := range due {
		timer.ch <- now
	}
}

func waitStartAtTimer(t *testing.T, clock *startAtFakeClock, wantDeadline time.Time) *startAtFakeTimer {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case timer := <-clock.created:
			got, stopped, _ := timer.snapshot()
			if !stopped && got.Equal(wantDeadline) {
				return timer
			}
		case <-deadline:
			t.Fatalf("full-sync owner did not arm timer for %v", wantDeadline)
		}
	}
}

// startAtClient is a race-safe mutable Twitch fake for scheduler tests. Its
// dashboard gate can hold one full sync at the network boundary so collision
// and shutdown behavior are observable without touching production code.
type startAtClient struct {
	mu sync.Mutex

	dashboard    map[string]interface{}
	dashboardErr error
	inventory    map[string]interface{}
	inventoryErr error
	details      map[string]map[string]interface{}

	fullSyncCalls                  int
	dashboardInFlight              int
	maxConcurrentDashboardRequests int
	fullSyncSignal                 chan int

	gateEntered       chan struct{}
	gateRelease       chan struct{}
	detailGateEntered chan struct{}
	detailGateRelease chan struct{}
}

func newStartAtClient(summaries []map[string]interface{}, details map[string]map[string]interface{}) *startAtClient {
	return &startAtClient{
		dashboard:      dashboardResponse(summaries...),
		inventory:      emptyInventoryResponse(),
		details:        details,
		fullSyncSignal: make(chan int, 512),
	}
}

func (c *startAtClient) setSnapshot(summaries []map[string]interface{}, details map[string]map[string]interface{}) {
	c.mu.Lock()
	c.dashboard = dashboardResponse(summaries...)
	c.details = details
	c.dashboardErr = nil
	c.inventoryErr = nil
	c.mu.Unlock()
}

func (c *startAtClient) setDashboardError(err error) {
	c.mu.Lock()
	c.dashboardErr = err
	c.mu.Unlock()
}

func (c *startAtClient) setInventoryError(err error) {
	c.mu.Lock()
	c.inventoryErr = err
	c.mu.Unlock()
}

func (c *startAtClient) armDashboardGate() (<-chan struct{}, chan<- struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entered := make(chan struct{})
	release := make(chan struct{})
	c.gateEntered = entered
	c.gateRelease = release
	return entered, release
}

func (c *startAtClient) armDetailGate() (<-chan struct{}, chan<- struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entered := make(chan struct{})
	release := make(chan struct{})
	c.detailGateEntered = entered
	c.detailGateRelease = release
	return entered, release
}

func (c *startAtClient) PostGQL(op constants.GQLOperation) (map[string]interface{}, error) {
	switch op.OperationName {
	case "ViewerDropsDashboard":
		c.mu.Lock()
		c.fullSyncCalls++
		call := c.fullSyncCalls
		c.dashboardInFlight++
		if c.dashboardInFlight > c.maxConcurrentDashboardRequests {
			c.maxConcurrentDashboardRequests = c.dashboardInFlight
		}
		resp, err := c.dashboard, c.dashboardErr
		entered, release := c.gateEntered, c.gateRelease
		c.gateEntered, c.gateRelease = nil, nil
		c.mu.Unlock()

		c.fullSyncSignal <- call
		if entered != nil {
			close(entered)
			<-release
		}

		c.mu.Lock()
		c.dashboardInFlight--
		c.mu.Unlock()
		return resp, err
	case "Inventory":
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.inventory, c.inventoryErr
	default:
		return map[string]interface{}{}, nil
	}
}

func (c *startAtClient) GetDropCampaignDetails(campaignID string) (map[string]interface{}, error) {
	c.mu.Lock()
	detail := c.details[campaignID]
	entered, release := c.detailGateEntered, c.detailGateRelease
	c.detailGateEntered, c.detailGateRelease = nil, nil
	c.mu.Unlock()
	if entered != nil {
		close(entered)
		<-release
	}
	return detail, nil
}

func (*startAtClient) ClaimDrop(*models.Drop) (twitch.ClaimStatus, error) {
	return twitch.ClaimStatusRejected, nil
}

func (c *startAtClient) counts() (calls, maxDashboardRequests int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fullSyncCalls, c.maxConcurrentDashboardRequests
}

// gatedUpcomingNotifier pauses one full sync after its authoritative publish
// and filter reads but before fullSyncMu is released. It lets the running owner
// prove that a later config trigger survives a stale timer waiter.
type gatedUpcomingNotifier struct {
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (n *gatedUpcomingNotifier) arm() (<-chan struct{}, chan<- struct{}) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.armed = true
	n.entered = make(chan struct{})
	n.release = make(chan struct{})
	return n.entered, n.release
}

func (n *gatedUpcomingNotifier) NotifyUpcomingCampaign(context.Context, *models.Campaign) {
	n.mu.Lock()
	if !n.armed {
		n.mu.Unlock()
		return
	}
	n.armed = false
	entered, release := n.entered, n.release
	n.mu.Unlock()
	close(entered)
	<-release
}

func waitFullSyncCalls(t *testing.T, client *startAtClient, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		calls, _ := client.counts()
		if calls >= want {
			return
		}
		select {
		case <-client.fullSyncSignal:
		case <-deadline:
			t.Fatalf("full-sync calls=%d, want at least %d", calls, want)
		}
	}
}

func assertFullSyncCallsStable(t *testing.T, client *startAtClient, want int) {
	t.Helper()
	time.Sleep(25 * time.Millisecond)
	if calls, _ := client.counts(); calls != want {
		t.Fatalf("full-sync calls=%d after quiescence, want %d", calls, want)
	}
}

func newStartAtTracker(client twitchClient, clock *startAtFakeClock, intervalSeconds int) *DropsTracker {
	tracker := NewDropsTracker(client, nil, config.RateLimitSettings{
		CampaignSyncInterval:     intervalSeconds,
		DropProgressSyncInterval: 100_000,
	}, nil)
	tracker.intervalUnit = time.Second
	tracker.campaignClock = clock
	return tracker
}

func startTrackerForTest(t *testing.T, tracker *DropsTracker, client *startAtClient) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	tracker.Start(ctx)
	waitFullSyncCalls(t, client, 1)
	return cancel
}

func campaignSetAt(startAt time.Time, count int, gameID string) ([]map[string]interface{}, map[string]map[string]interface{}) {
	summaries := make([]map[string]interface{}, 0, count)
	details := make(map[string]map[string]interface{}, count)
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("campaign-%03d", i)
		summary, detail := campaignSummaryDetail(id, id, "UPCOMING", gameID, gameID, startAt, startAt.Add(time.Hour))
		summaries = append(summaries, summary)
		details[id] = detail
	}
	return summaries, details
}

func TestStartAtSchedulerCoalescesOneHundredCampaignsWithoutEarlySync(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	startAt := base.Add(10 * time.Second)
	summaries, details := campaignSetAt(startAt, 100, "game-coalesced")
	client := newStartAtClient(summaries, details)
	clock := newStartAtFakeClock(base)
	tracker := newStartAtTracker(client, clock, 100)
	cancel := startTrackerForTest(t, tracker, client)
	t.Cleanup(func() { cancel(); tracker.Stop() })

	wake := waitStartAtTimer(t, clock, startAt)
	if _, stopped, fired := wake.snapshot(); stopped || fired {
		t.Fatalf("new StartAt timer state stopped=%v fired=%v, want one pending timer", stopped, fired)
	}
	if got := len(tracker.UpcomingCampaigns()); got != 100 {
		t.Fatalf("startup upcoming campaigns=%d, want 100", got)
	}

	clock.Advance(9 * time.Second)
	assertFullSyncCallsStable(t, client, 1)
	clock.Advance(time.Second)
	waitFullSyncCalls(t, client, 2)
	waitStartAtTimer(t, clock, base.Add(110*time.Second))
	assertFullSyncCallsStable(t, client, 2)

	if calls, maxDashboard := client.counts(); calls != 2 || maxDashboard != 1 {
		t.Fatalf("100 identical StartAt campaigns: calls=%d maxConcurrentDashboardRequests=%d, want 2 total / 1", calls, maxDashboard)
	}
	if got := len(tracker.Campaigns()); got != 100 {
		t.Fatalf("StartAt sync published %d active campaigns, want 100", got)
	}
}

func TestNearStartAtDeadlinesDueBeforeOwnerRunCoalesce(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	firstS, firstD := campaignSummaryDetail("near-first", "Near First", "UPCOMING", "game", "Game", base.Add(10*time.Second), base.Add(time.Hour))
	secondS, secondD := campaignSummaryDetail("near-second", "Near Second", "UPCOMING", "game", "Game", base.Add(11*time.Second), base.Add(time.Hour))
	client := newStartAtClient(
		[]map[string]interface{}{firstS, secondS},
		map[string]map[string]interface{}{"near-first": firstD, "near-second": secondD},
	)
	clock := newStartAtFakeClock(base)
	tracker := newStartAtTracker(client, clock, 100)
	cancel := startTrackerForTest(t, tracker, client)
	t.Cleanup(func() { cancel(); tracker.Stop() })
	waitStartAtTimer(t, clock, base.Add(10*time.Second))

	// If the owner is busy until both nearby boundaries are due, its one
	// authoritative run covers both. No arbitrary "near" duration is invented.
	tracker.fullSyncMu.Lock()
	clock.Advance(11 * time.Second)
	tracker.fullSyncMu.Unlock()
	waitFullSyncCalls(t, client, 2)
	waitStartAtTimer(t, clock, base.Add(111*time.Second))
	assertFullSyncCallsStable(t, client, 2)
	ids := keptIDs(tracker.Campaigns())
	if !ids["near-first"] || !ids["near-second"] {
		t.Fatalf("near-deadline coalesced active set=%v, want both campaigns", ids)
	}
}

func TestStartAtSchedulerUsesNearestRelevantDeadlineThenRecomputes(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	foreignS, foreignD := campaignSummaryDetail("foreign", "Foreign", "UPCOMING", "game-foreign", "Foreign", base.Add(5*time.Second), base.Add(time.Hour))
	firstS, firstD := campaignSummaryDetail("first", "First", "UPCOMING", "game-relevant", "Relevant", base.Add(10*time.Second), base.Add(time.Hour))
	secondS, secondD := campaignSummaryDetail("second", "Second", "UPCOMING", "game-relevant", "Relevant", base.Add(20*time.Second), base.Add(time.Hour))
	client := newStartAtClient(
		[]map[string]interface{}{foreignS, secondS, firstS},
		map[string]map[string]interface{}{"foreign": foreignD, "first": firstD, "second": secondD},
	)
	clock := newStartAtFakeClock(base)
	tracker := newStartAtTracker(client, clock, 100)
	tracker.UpdateGameFilter([]string{"game-relevant"}, nil)
	cancel := startTrackerForTest(t, tracker, client)
	t.Cleanup(func() { cancel(); tracker.Stop() })

	// The earlier foreign campaign is displayable but does not own the relevant
	// discovery wake. Dashboard order is deliberately not deadline order.
	waitStartAtTimer(t, clock, base.Add(10*time.Second))
	clock.Advance(5 * time.Second)
	assertFullSyncCallsStable(t, client, 1)
	clock.Advance(5 * time.Second)
	waitFullSyncCalls(t, client, 2)
	waitStartAtTimer(t, clock, base.Add(20*time.Second))

	clock.Advance(10 * time.Second)
	waitFullSyncCalls(t, client, 3)
	waitStartAtTimer(t, clock, base.Add(120*time.Second))
	if calls, maxDashboard := client.counts(); calls != 3 || maxDashboard != 1 {
		t.Fatalf("nearest/recompute calls=%d maxConcurrentDashboardRequests=%d, want 3/1", calls, maxDashboard)
	}
	ids := keptIDs(tracker.Campaigns())
	if !ids["first"] || !ids["second"] || ids["foreign"] {
		t.Fatalf("relevant active set after both deadlines=%v", ids)
	}
}

func TestStartAtSchedulerIgnoresPastZeroAndUnknownWithoutStorm(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	pastS, pastD := campaignSummaryDetail("past", "Past", "EXPIRED", "game", "Game", base.Add(-20*time.Second), base.Add(-10*time.Second))
	unknownS, unknownD := campaignSummaryDetail("unknown", "Unknown", "UPCOMING", "game", "Game", base, base.Add(time.Hour))
	delete(unknownS, "startAt")
	delete(unknownD, "startAt")
	client := newStartAtClient(
		[]map[string]interface{}{pastS, unknownS},
		map[string]map[string]interface{}{"past": pastD, "unknown": unknownD},
	)
	clock := newStartAtFakeClock(base)
	tracker := newStartAtTracker(client, clock, 100)
	cancel := startTrackerForTest(t, tracker, client)
	t.Cleanup(func() { cancel(); tracker.Stop() })

	waitStartAtTimer(t, clock, base.Add(100*time.Second))
	clock.Advance(99 * time.Second)
	assertFullSyncCallsStable(t, client, 1)
	clock.Advance(time.Second)
	waitFullSyncCalls(t, client, 2)
	waitStartAtTimer(t, clock, base.Add(200*time.Second))
	assertFullSyncCallsStable(t, client, 2)
}

func TestStartAtAndOrdinaryTickerCollisionRunsOneFullSync(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	startAt := base.Add(10 * time.Second)
	summaries, details := campaignSetAt(startAt, 1, "game")
	client := newStartAtClient(summaries, details)
	clock := newStartAtFakeClock(base)
	tracker := newStartAtTracker(client, clock, 10)
	cancel := startTrackerForTest(t, tracker, client)
	t.Cleanup(func() { cancel(); tracker.Stop() })

	waitStartAtTimer(t, clock, startAt)
	clock.Advance(10 * time.Second)
	waitFullSyncCalls(t, client, 2)
	waitStartAtTimer(t, clock, base.Add(20*time.Second))
	assertFullSyncCallsStable(t, client, 2)
	if _, maxDashboard := client.counts(); maxDashboard != 1 {
		t.Fatalf("ticker + StartAt max concurrent dashboard requests=%d, want 1", maxDashboard)
	}
}

func TestCampaignResyncAndStartAtCollisionCoalesce(t *testing.T) {
	triggers := []struct {
		name string
		run  func(*DropsTracker)
	}{
		{name: "manual", run: func(tracker *DropsTracker) {
			if result := tracker.RequestManualSync(); !result.Triggered {
				t.Fatal("manual resync unexpectedly rejected")
			}
		}},
		{name: "config", run: func(tracker *DropsTracker) {
			tracker.UpdateGameFilter(nil, nil)
		}},
	}

	for _, tc := range triggers {
		t.Run(tc.name, func(t *testing.T) {
			base := time.Now().UTC().Truncate(time.Second)
			startAt := base.Add(10 * time.Second)
			summaries, details := campaignSetAt(startAt, 1, "game")
			client := newStartAtClient(summaries, details)
			clock := newStartAtFakeClock(base)
			tracker := newStartAtTracker(client, clock, 100)
			cancel := startTrackerForTest(t, tracker, client)
			t.Cleanup(func() { cancel(); tracker.Stop() })
			waitStartAtTimer(t, clock, startAt)

			// Hold the one owner lock while making both wake sources ready. No
			// select ordering can turn this collision into two covering runs.
			tracker.fullSyncMu.Lock()
			tc.run(tracker)
			clock.Advance(10 * time.Second)
			tracker.fullSyncMu.Unlock()

			waitFullSyncCalls(t, client, 2)
			waitStartAtTimer(t, clock, base.Add(110*time.Second))
			assertFullSyncCallsStable(t, client, 2)
			if _, maxDashboard := client.counts(); maxDashboard != 1 {
				t.Fatalf("campaignResync + StartAt max concurrent dashboard requests=%d, want 1", maxDashboard)
			}
		})
	}
}

func TestStaleTimedWakePreservesNewerConfigTrigger(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	startAt := base.Add(50 * time.Second)
	summaries, details := campaignSetAt(startAt, 1, "old-game")
	client := newStartAtClient(summaries, details)
	clock := newStartAtFakeClock(base)
	notifier := &gatedUpcomingNotifier{}
	tracker := newStartAtTracker(client, clock, 10)
	tracker.SetUpcomingNotifier(notifier)
	cancel := startTrackerForTest(t, tracker, client)
	t.Cleanup(func() { cancel(); tracker.Stop() })
	waitStartAtTimer(t, clock, base.Add(10*time.Second))
	clock.Advance(9 * time.Second)

	// Hold a direct SyncNow in details until the old ordinary timer becomes due.
	// After the direct sync publishes generation G+1, hold it again in the
	// notifier while a newer config trigger is queued. The old timer waiter must
	// reject its stale generation without consuming that explicit trigger.
	detailEntered, detailRelease := client.armDetailGate()
	notifyEntered, notifyRelease := notifier.arm()
	directDone := make(chan struct{})
	go func() {
		tracker.SyncNow()
		close(directDone)
	}()
	select {
	case <-detailEntered:
	case <-time.After(2 * time.Second):
		close(detailRelease)
		t.Fatal("direct SyncNow did not reach the details gate")
	}
	clock.Advance(time.Second)
	for i := 0; i < 100; i++ {
		runtime.Gosched()
	}
	close(detailRelease)
	select {
	case <-notifyEntered:
	case <-time.After(2 * time.Second):
		close(notifyRelease)
		t.Fatal("direct SyncNow did not reach the post-publish notifier gate")
	}
	tracker.UpdateGameFilter([]string{"new-game"}, nil)
	close(notifyRelease)
	select {
	case <-directDone:
	case <-time.After(2 * time.Second):
		t.Fatal("direct SyncNow did not finish after notifier release")
	}

	waitFullSyncCalls(t, client, 3)
	waitStartAtTimer(t, clock, base.Add(20*time.Second))
	assertFullSyncCallsStable(t, client, 3)
	if relevant := tracker.RelevantUpcomingCampaigns(); len(relevant) != 0 {
		t.Fatalf("preserved config resync did not apply new filter: relevant=%d", len(relevant))
	}
	if calls, maxDashboard := client.counts(); calls != 3 || maxDashboard != 1 {
		t.Fatalf("stale-timer/config calls=%d maxConcurrentDashboardRequests=%d, want 3/1", calls, maxDashboard)
	}
}

func TestSuccessfulSyncInvalidatesStaleStartAtDeadline(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	oldStartAt := base.Add(10 * time.Second)
	summaries, details := campaignSetAt(oldStartAt, 1, "game")
	client := newStartAtClient(summaries, details)
	clock := newStartAtFakeClock(base)
	tracker := newStartAtTracker(client, clock, 100)
	cancel := startTrackerForTest(t, tracker, client)
	t.Cleanup(func() { cancel(); tracker.Stop() })

	oldTimer := waitStartAtTimer(t, clock, oldStartAt)
	client.setSnapshot(nil, map[string]map[string]interface{}{})
	tracker.SyncNow()
	waitFullSyncCalls(t, client, 2)
	waitStartAtTimer(t, clock, base.Add(100*time.Second))
	if _, stopped, _ := oldTimer.snapshot(); !stopped {
		t.Fatal("snapshot replacement did not stop the stale StartAt timer")
	}

	clock.Advance(10 * time.Second)
	assertFullSyncCallsStable(t, client, 2)
	if len(tracker.UpcomingCampaigns()) != 0 {
		t.Fatal("successful replacement did not publish the new empty upcoming snapshot")
	}
}

func TestFullSyncCrossingStartAtKeepsOneCatchUpWake(t *testing.T) {
	paths := []string{"campaign_resync", "direct_sync_now"}
	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			base := time.Now().UTC().Truncate(time.Second)
			startAt := base.Add(10 * time.Second)
			summaries, details := campaignSetAt(startAt, 1, "game")
			client := newStartAtClient(summaries, details)
			clock := newStartAtFakeClock(base)
			tracker := newStartAtTracker(client, clock, 100)
			cancel := startTrackerForTest(t, tracker, client)
			t.Cleanup(func() { cancel(); tracker.Stop() })
			waitStartAtTimer(t, clock, startAt)
			clock.Advance(9 * time.Second)

			entered, release := client.armDetailGate()
			var directDone chan struct{}
			switch path {
			case "campaign_resync":
				tracker.triggerCampaignResync()
			case "direct_sync_now":
				directDone = make(chan struct{})
				go func() {
					tracker.SyncNow()
					close(directDone)
				}()
			}
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				close(release)
				t.Fatal("covering full sync did not reach the gated campaign-details request")
			}

			// The sync observed StartAt as future, then remained in flight until
			// the boundary. It must retain exactly one immediate catch-up wake;
			// otherwise it publishes an upcoming campaign with no deadline.
			clock.Advance(time.Second)
			close(release)
			if directDone != nil {
				select {
				case <-directDone:
				case <-time.After(2 * time.Second):
					t.Fatal("direct SyncNow did not finish after gate release")
				}
			}
			waitFullSyncCalls(t, client, 3)
			waitStartAtTimer(t, clock, base.Add(110*time.Second))
			assertFullSyncCallsStable(t, client, 3)
			if ids := keptIDs(tracker.Campaigns()); !ids["campaign-000"] {
				t.Fatalf("catch-up sync did not activate campaign: %v", ids)
			}
		})
	}
}

func TestFailedStartAtSyncPreservesLKGAndDoesNotStorm(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	activeS, activeD := campaignSummaryDetail("active", "Active", "ACTIVE", "game", "Game", base.Add(-time.Hour), base.Add(time.Hour))
	futureS, futureD := campaignSummaryDetail("future", "Future", "UPCOMING", "game", "Game", base.Add(10*time.Second), base.Add(time.Hour))
	client := newStartAtClient(
		[]map[string]interface{}{activeS, futureS},
		map[string]map[string]interface{}{"active": activeD, "future": futureD},
	)
	clock := newStartAtFakeClock(base)
	tracker := newStartAtTracker(client, clock, 100)
	cancel := startTrackerForTest(t, tracker, client)
	t.Cleanup(func() { cancel(); tracker.Stop() })

	waitStartAtTimer(t, clock, base.Add(10*time.Second))
	beforeRevision := tracker.Revision()
	beforeActive := keptIDs(tracker.Campaigns())
	beforeUpcoming := keptIDs(tracker.UpcomingCampaigns())
	client.setDashboardError(errors.New("dashboard unavailable"))
	clock.Advance(10 * time.Second)
	waitFullSyncCalls(t, client, 2)
	waitStartAtTimer(t, clock, base.Add(110*time.Second))

	if got := tracker.Revision(); got != beforeRevision {
		t.Fatalf("failed StartAt sync revision=%d, want LKG revision %d", got, beforeRevision)
	}
	if got := keptIDs(tracker.Campaigns()); !sameIDSet(got, beforeActive) {
		t.Fatalf("failed StartAt sync active=%v, want LKG %v", got, beforeActive)
	}
	if got := keptIDs(tracker.UpcomingCampaigns()); !sameIDSet(got, beforeUpcoming) {
		t.Fatalf("failed StartAt sync upcoming=%v, want LKG %v", got, beforeUpcoming)
	}
	if status := tracker.SyncStatus(); status.LastError == "" {
		t.Fatal("failed StartAt attempt was not recorded")
	}
	clock.Advance(time.Second)
	assertFullSyncCallsStable(t, client, 2)

	// Recovery remains available through the existing manual owner path, and
	// the now-due campaign becomes active without any retry constant.
	client.setSnapshot(
		[]map[string]interface{}{activeS, futureS},
		map[string]map[string]interface{}{"active": activeD, "future": futureD},
	)
	if result := tracker.RequestManualSync(); !result.Triggered {
		t.Fatal("manual recovery resync unexpectedly rejected")
	}
	waitFullSyncCalls(t, client, 3)
	waitStartAtTimer(t, clock, base.Add(111*time.Second))
	if ids := keptIDs(tracker.Campaigns()); !ids["active"] || !ids["future"] {
		t.Fatalf("manual recovery active campaigns=%v, want active+future", ids)
	}
}

func TestFailedStartAtInventoryMergeDoesNotPublishReplacementUpcomingOrDeadline(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	activeS, activeD := campaignSummaryDetail("active-lkg", "Active LKG", "ACTIVE", "game", "Game", base.Add(-time.Hour), base.Add(time.Hour))
	oldS, oldD := campaignSummaryDetail("old-upcoming", "Old Upcoming", "UPCOMING", "game", "Game", base.Add(10*time.Second), base.Add(time.Hour))
	client := newStartAtClient(
		[]map[string]interface{}{activeS, oldS},
		map[string]map[string]interface{}{"active-lkg": activeD, "old-upcoming": oldD},
	)
	clock := newStartAtFakeClock(base)
	tracker := newStartAtTracker(client, clock, 100)
	cancel := startTrackerForTest(t, tracker, client)
	t.Cleanup(func() { cancel(); tracker.Stop() })
	waitStartAtTimer(t, clock, base.Add(10*time.Second))

	beforeRevision := tracker.Revision()
	beforeActive := keptIDs(tracker.Campaigns())
	beforeUpcoming := keptIDs(tracker.UpcomingCampaigns())
	replacementS, replacementD := campaignSummaryDetail("replacement", "Replacement", "UPCOMING", "game", "Game", base.Add(20*time.Second), base.Add(time.Hour))
	client.setSnapshot(
		[]map[string]interface{}{replacementS},
		map[string]map[string]interface{}{"replacement": replacementD},
	)
	client.setInventoryError(errors.New("inventory merge unavailable"))

	clock.Advance(10 * time.Second)
	waitFullSyncCalls(t, client, 2)
	waitStartAtTimer(t, clock, base.Add(110*time.Second))
	if got := tracker.Revision(); got != beforeRevision {
		t.Fatalf("failed inventory merge revision=%d, want LKG %d", got, beforeRevision)
	}
	if got := keptIDs(tracker.Campaigns()); !sameIDSet(got, beforeActive) {
		t.Fatalf("failed inventory merge active=%v, want LKG %v", got, beforeActive)
	}
	if got := keptIDs(tracker.UpcomingCampaigns()); !sameIDSet(got, beforeUpcoming) {
		t.Fatalf("failed inventory merge upcoming=%v, want LKG %v", got, beforeUpcoming)
	}
	if keptIDs(tracker.UpcomingCampaigns())["replacement"] {
		t.Fatal("failed inventory merge published replacement upcoming campaign")
	}
	assertFullSyncCallsStable(t, client, 2)
}

func sameIDSet(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for id := range a {
		if !b[id] {
			return false
		}
	}
	return true
}

func TestStartAtScheduleRebuildsOnRestartAndCancelsCleanly(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	startAt := base.Add(10 * time.Second)
	summaries, details := campaignSetAt(startAt, 1, "game")
	client := newStartAtClient(summaries, details)
	clock := newStartAtFakeClock(base)

	first := newStartAtTracker(client, clock, 100)
	firstCancel := startTrackerForTest(t, first, client)
	firstTimer := waitStartAtTimer(t, clock, startAt)
	clock.Advance(4 * time.Second)
	firstCancel()
	first.Stop()
	if _, stopped, _ := firstTimer.snapshot(); !stopped {
		t.Fatal("context cancellation did not stop the pending StartAt timer")
	}
	clock.Advance(time.Second)
	assertFullSyncCallsStable(t, client, 1)

	// A fresh tracker has no timer state to load. Its startup authoritative sync
	// naturally reconstructs the same absolute boundary with five seconds left.
	second := newStartAtTracker(client, clock, 100)
	secondCancel := startTrackerForTest(t, second, client)
	t.Cleanup(func() { secondCancel(); second.Stop() })
	waitStartAtTimer(t, clock, startAt)
	clock.Advance(5 * time.Second)
	waitFullSyncCalls(t, client, 3) // first startup + second startup + StartAt
	waitStartAtTimer(t, clock, base.Add(110*time.Second))
	if calls, maxDashboard := client.counts(); calls != 3 || maxDashboard != 1 {
		t.Fatalf("restart calls=%d maxConcurrentDashboardRequests=%d, want 3/1", calls, maxDashboard)
	}
}

func TestStopJoinsInFlightStartAtSyncAfterCancellation(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	startAt := base.Add(10 * time.Second)
	summaries, details := campaignSetAt(startAt, 1, "game")
	client := newStartAtClient(summaries, details)
	clock := newStartAtFakeClock(base)
	tracker := newStartAtTracker(client, clock, 100)
	cancel := startTrackerForTest(t, tracker, client)
	waitStartAtTimer(t, clock, startAt)

	entered, release := client.armDashboardGate()
	clock.Advance(10 * time.Second)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		cancel()
		tracker.Stop()
		t.Fatal("StartAt-triggered full sync did not enter the gated request")
	}

	stopped := make(chan struct{})
	go func() {
		cancel()
		tracker.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned before the in-flight StartAt sync was released")
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not join the released StartAt-triggered sync")
	}
	if _, maxDashboard := client.counts(); maxDashboard != 1 {
		t.Fatalf("in-flight shutdown max concurrent dashboard requests=%d, want 1", maxDashboard)
	}
}

func TestCanceledOwnerDoesNotStartQueuedStartAtSync(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	startAt := base.Add(10 * time.Second)
	summaries, details := campaignSetAt(startAt, 1, "game")
	client := newStartAtClient(summaries, details)
	clock := newStartAtFakeClock(base)
	tracker := newStartAtTracker(client, clock, 100)
	ctx, cancel := context.WithCancel(context.Background())
	tracker.Start(ctx)
	waitFullSyncCalls(t, client, 1)
	waitStartAtTimer(t, clock, startAt)

	// A failed direct sync keeps the schedule generation unchanged while holding
	// fullSyncMu. Make the StartAt wake due behind it, then cancel before the
	// direct caller releases the lock. The queued owner wake must exit without a
	// third dashboard request.
	client.setDashboardError(errors.New("direct sync failed during shutdown"))
	entered, release := client.armDashboardGate()
	directDone := make(chan struct{})
	go func() {
		tracker.SyncNow()
		close(directDone)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		cancel()
		close(release)
		tracker.Stop()
		t.Fatal("direct SyncNow did not reach the dashboard gate")
	}
	clock.Advance(10 * time.Second)
	for i := 0; i < 100; i++ {
		runtime.Gosched()
	}
	time.Sleep(25 * time.Millisecond)
	cancel()
	close(release)
	select {
	case <-directDone:
	case <-time.After(2 * time.Second):
		tracker.Stop()
		t.Fatal("failed direct SyncNow did not return after gate release")
	}
	tracker.Stop()
	assertFullSyncCallsStable(t, client, 2)
}

func TestStartAtWakePreservesGlobalCampaignCadenceAndProgressNonDiscovery(t *testing.T) {
	base := time.Now().UTC().Truncate(time.Second)
	startAt := base.Add(10 * time.Second)
	summaries, details := campaignSetAt(startAt, 1, "game")
	client := newStartAtClient(summaries, details)
	clock := newStartAtFakeClock(base)
	tracker := newStartAtTracker(client, clock, 60)
	cancel := startTrackerForTest(t, tracker, client)
	t.Cleanup(func() { cancel(); tracker.Stop() })
	waitStartAtTimer(t, clock, startAt)

	// Make the StartAt timer ready while holding the sole owner lock. The
	// lightweight inventory-only path still cannot discover the campaign.
	tracker.fullSyncMu.Lock()
	clock.Advance(10 * time.Second)
	tracker.syncProgress()
	if got := len(tracker.Campaigns()); got != 0 {
		tracker.fullSyncMu.Unlock()
		t.Fatalf("lightweight progress sync discovered %d campaign(s), want 0", got)
	}
	if calls, _ := client.counts(); calls != 1 {
		tracker.fullSyncMu.Unlock()
		t.Fatalf("lightweight progress sync reached dashboard: calls=%d, want 1", calls)
	}
	tracker.fullSyncMu.Unlock()

	waitFullSyncCalls(t, client, 2)
	// The StartAt wake is one additional full sync, then the unmodified 60-unit
	// ordinary cadence restarts from its completion — no global acceleration.
	waitStartAtTimer(t, clock, base.Add(70*time.Second))
	clock.Advance(59 * time.Second)
	assertFullSyncCallsStable(t, client, 2)
	clock.Advance(time.Second)
	waitFullSyncCalls(t, client, 3)
	waitStartAtTimer(t, clock, base.Add(130*time.Second))
	if calls, maxDashboard := client.counts(); calls != 3 || maxDashboard != 1 {
		t.Fatalf("cadence proof calls=%d maxConcurrentDashboardRequests=%d, want 3/1", calls, maxDashboard)
	}
}

func TestCampaignStartBoundaryIsInclusive(t *testing.T) {
	startAt := time.Now().UTC().Truncate(time.Second)
	summary, detail := campaignSummaryDetail("equal", "Equal", "UPCOMING", "game", "Game", startAt, startAt.Add(time.Hour))
	campaign, _, skip := buildTrackedCampaignAt(summary, detail, startAt)
	if skip != skipNone || !campaign.DateMatch {
		t.Fatalf("campaign at exact StartAt: skip=%v DateMatch=%v, want active", skip, campaign.DateMatch)
	}
}
