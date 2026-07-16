package discovery

import (
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/api"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// Race-safe fakes: everything here is either immutable or mutex-guarded, so
// any race the detector reports is in the production discovery code.

type safeCampaigns struct{ campaigns []*models.Campaign }

func (f *safeCampaigns) Campaigns() []*models.Campaign { return f.campaigns }

type safeClient struct{ streams []api.DirectoryStream }

func (f *safeClient) CheckStreamerOnline(s *models.Streamer) {
	if len(s.Stream.CampaignIDs) == 0 {
		s.Stream.CampaignIDs = []string{"camp-g1"} // only the watch goroutine calls this
	}
	s.SetOnline()
}

func (f *safeClient) GetDirectoryStreams(string, int) ([]api.DirectoryStream, error) {
	return f.streams, nil
}

func newRaceManager(t *testing.T) *Manager {
	t.Helper()
	provider := &safeCampaigns{campaigns: []*models.Campaign{activeCampaign("g1", "World of Tanks")}}
	client := &safeClient{streams: []api.DirectoryStream{
		{ChannelID: "1", Login: "chan_a", Viewers: 100, GameID: "g1", DropsEnabled: true},
	}}

	m := NewManager(nil, provider, &fakeTracked{}, testRateLimits(), []string{"World of Tanks"}, config.DiscoveryModeAll, false)
	m.client = client
	// A broker whose slot status is always "watching" — State() consults it,
	// exercising that path concurrently with the sync and prepare loops.
	m.slotStatus = &fakeSlotStatus{watching: map[string]bool{"chan_a": true}}

	m.syncOnce() // build the initial pool; chan_a's *Channel is shared from here on
	if len(m.pool) != 1 {
		t.Fatalf("setup: expected 1 pool entry, got %d", len(m.pool))
	}
	return m
}

// syncOnce (sync loop goroutine) vs State (HTTP/debug goroutine).
func TestRaceSyncVsState(t *testing.T) {
	m := newRaceManager(t)

	const iters = 20000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // sync loop
		defer wg.Done()
		for i := 0; i < iters; i++ {
			m.syncOnce()
		}
	}()
	go func() { // web/debug reader
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = m.State()
		}
	}()
	wg.Wait()
}

// syncOnce (sync loop goroutine) vs prepareCurrent (broker loop goroutine).
func TestRaceSyncVsWatch(t *testing.T) {
	m := newRaceManager(t)

	const iters = 20000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // sync loop
		defer wg.Done()
		for i := 0; i < iters; i++ {
			m.syncOnce()
		}
	}()
	go func() { // broker loop (prepareCurrent)
		defer wg.Done()
		for i := 0; i < iters; i++ {
			m.prepareCurrent()
		}
	}()
	wg.Wait()
}

// TestConcurrentSyncStateWatch runs all three real access patterns at once:
// the sync loop (syncOnce), an HTTP/debug reader (State), and the broker loop
// (prepareCurrent) — exactly the goroutines Start() + the web server create in
// production. Run under -race (the repo's standard test invocation) it
// guards the mu discipline around shared *Channel entries.
func TestConcurrentSyncStateWatch(t *testing.T) {
	m := newRaceManager(t)

	const iters = 20000
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { // sync loop
		defer wg.Done()
		for i := 0; i < iters; i++ {
			m.syncOnce()
		}
	}()
	go func() { // web/debug reader
		defer wg.Done()
		for i := 0; i < iters; i++ {
			_ = m.State()
		}
	}()
	go func() { // broker loop (prepareCurrent)
		defer wg.Done()
		for i := 0; i < iters; i++ {
			m.prepareCurrent()
		}
	}()
	wg.Wait()
}
