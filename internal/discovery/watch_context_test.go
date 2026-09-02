package discovery

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/twitch"
)

// B2 — discovery's candidate re-verification belongs to the watch generation.
//
// WatchCandidates runs ON the slot broker's loop goroutine, and its candidate
// preparation re-checks a stale channel's online status over the network. That
// check therefore belongs to the watch generation: if it ignored the
// generation's context it would pin the loop goroutine — and so the
// generation's join — for the whole Twitch transport budget after cancellation.

// parkingDiscoveryClient blocks inside the online check until the context it
// was handed ends, so a test can prove which context reached it.
type parkingDiscoveryClient struct {
	entered chan struct{}
	once    sync.Once
}

func (p *parkingDiscoveryClient) CheckStreamerOnlineContext(ctx context.Context, s *models.Streamer) models.StatusTransition {
	p.once.Do(func() { close(p.entered) })
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
	}
	return models.StatusTransition{Previous: s.GetStatus(), Current: s.GetStatus()}
}

func (p *parkingDiscoveryClient) GetDirectoryStreams(string, int) ([]twitch.DirectoryStream, error) {
	return nil, nil
}

// TestCandidateRefreshObservesTheWatchGenerationContext pins the ownership: the
// context WatchCandidates is handed must reach the online check discovery runs
// during candidate preparation.
func TestCandidateRefreshObservesTheWatchGenerationContext(t *testing.T) {
	client := &parkingDiscoveryClient{entered: make(chan struct{})}
	m := &Manager{client: client}

	streamer := models.NewStreamer("candidate", models.DefaultStreamerSettings())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.runRoutineRefresh(ctx, streamer)
	}()

	select {
	case <-client.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the candidate online check never ran")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelling the watch generation did not reach discovery's candidate online check: " +
			"candidate preparation would pin the broker's loop goroutine after cancellation")
	}
}

// TestWatchCandidatesThreadsTheGenerationContext drives the REAL entry point the
// slot broker calls. runRoutineRefresh is only the last link of the chain
// WatchCandidates -> prepareCurrentWithPolicy -> runRoutineRefresh ->
// CheckStreamerOnlineContext; without this witness any hop above it could be
// rebound to context.Background() with every test still green, and candidate
// re-verification would pin the broker's loop goroutine after cancellation.
func TestWatchCandidatesThreadsTheGenerationContext(t *testing.T) {
	oldStale := staleStreamRecheck
	staleStreamRecheck = time.Nanosecond // the current channel is stale immediately
	t.Cleanup(func() { staleStreamRecheck = oldStale })

	client := &parkingDiscoveryClient{entered: make(chan struct{})}
	streamer := models.NewStreamer("candidate", models.DefaultStreamerSettings())
	streamer.SetConfirmedOnline()
	streamer.Stream.Update("b1", "t", &models.Game{ID: "g1", Name: "World of Tanks"}, nil, 1)

	m := NewManager(nil, nil, nil, config.RateLimitSettings{},
		[]string{"World of Tanks"}, config.DiscoveryModeAll, false)
	m.client = client
	m.current = &Channel{Streamer: streamer, Game: "World of Tanks", GameID: "g1"}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.WatchCandidates(ctx)
	}()

	select {
	case <-client.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchCandidates never re-verified the stale current channel")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelling the watch generation did not reach the online check inside WatchCandidates: " +
			"candidate preparation would pin the broker's loop goroutine after cancellation")
	}
}

// TestPoolScanRefreshObservesTheGenerationContext closes the last branch of the
// candidate-preparation chain. With no current channel, WatchCandidates falls
// into the pool scan (selectBestWithPolicy -> selectBestOrdered), which brings
// up to maxCandidateChecksPerTick unverified pool channels online — each a full
// GQL round trip, on the broker's loop goroutine. Those checks must carry the
// generation context too, or a teardown waits out the whole transport budget.
func TestPoolScanRefreshObservesTheGenerationContext(t *testing.T) {
	client := &parkingDiscoveryClient{entered: make(chan struct{})}

	// An unverified pool candidate: not online yet, so the scan re-checks it.
	pooled := &Channel{
		Streamer:     newEphemeralStreamer("pooled", "cid-pooled"),
		Game:         "World of Tanks",
		GameID:       "g1",
		Viewers:      10,
		DropsEnabled: true,
	}

	m := NewManager(nil, &staticCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}},
		&fakeTracked{}, config.RateLimitSettings{},
		[]string{"World of Tanks"}, config.DiscoveryModeAll, false)
	m.client = client
	m.pool = []*Channel{pooled}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		m.WatchCandidates(ctx)
	}()

	select {
	case <-client.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the pool scan never brought an unverified candidate online")
	}
	cancel()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancelling the watch generation did not reach the pool-scan online check: " +
			"candidate preparation would pin the broker's loop goroutine after cancellation")
	}
}

// staticCampaigns is a fixed CampaignsProvider for the pool-scan witness.
type staticCampaigns struct{ campaigns []*models.Campaign }

func (s *staticCampaigns) Campaigns() []*models.Campaign { return s.campaigns }
