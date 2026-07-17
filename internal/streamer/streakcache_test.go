package streamer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// fakeStreamerAPI satisfies twitchClient so manager load/add paths run
// without HTTP.
type fakeStreamerAPI struct{}

func (fakeStreamerAPI) GetChannelID(username string) (string, error)    { return "chan-" + username, nil }
func (fakeStreamerAPI) LoadChannelPointsContext(*models.Streamer) error { return nil }
func (fakeStreamerAPI) CheckStreamerOnline(*models.Streamer)            {}

func newCacheAt(t *testing.T) (*StreakCache, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "streak_cache.json")
	return NewStreakCache(path), path
}

// loadedManager builds a manager over the given cache and loads one streamer
// ("alpha") through the REAL LoadFromConfig path.
func loadedManager(t *testing.T, cache *StreakCache) *Manager {
	t.Helper()
	mgr := NewManager(fakeStreamerAPI{}, models.DefaultStreamerSettings())
	mgr.SetStreakCache(cache)
	if err := mgr.LoadFromConfig([]config.StreamerConfig{{Username: "alpha"}}, nil); err != nil {
		t.Fatalf("LoadFromConfig: %v", err)
	}
	return mgr
}

// T4: restart mid-broadcast with a persisted grant, driven through the REAL
// sequence — streamer created by LoadFromConfig (hydration), then the
// CheckStreamerOnline order: Stream.Update populates the broadcast ID BEFORE
// SetOnline. The already-granted broadcast must not be pursued.
func TestRestartHydrationBlocksSameBroadcast(t *testing.T) {
	cache, _ := newCacheAt(t)
	cache.Record("alpha", "bid-live", time.Now().Add(-30*time.Minute))

	mgr := loadedManager(t, cache)
	s := mgr.Get("alpha")

	// Mirror api.CheckStreamerOnline: UpdateStream -> Stream.Update -> SetOnline.
	s.Stream.Update("bid-live", "t", &models.Game{ID: "g"}, nil, 5)
	s.SetOnline()

	if s.Stream.StreakPending() {
		t.Fatal("restart + persisted grant on the still-live broadcast must not re-pursue")
	}

	// Control: the same sequence WITHOUT a cache pursues normally.
	bare := NewManager(fakeStreamerAPI{}, models.DefaultStreamerSettings())
	if err := bare.LoadFromConfig([]config.StreamerConfig{{Username: "alpha"}}, nil); err != nil {
		t.Fatal(err)
	}
	b := bare.Get("alpha")
	b.Stream.Update("bid-live", "t", &models.Game{ID: "g"}, nil, 5)
	b.SetOnline()
	if !b.Stream.StreakPending() {
		t.Fatal("control: without a cache the pursuit must start (historical behavior)")
	}
}

// T4b: before the first Update the broadcast is unidentified — with a fresh
// hydrated grant the pursuit is DEFERRED, then resolved by the first Update:
// blocked on the granted broadcast, released on a new one.
func TestRestartHydrationDefersUntilIdentified(t *testing.T) {
	cache, _ := newCacheAt(t)
	cache.Record("alpha", "bid-old", time.Now().Add(-time.Hour))

	mgr := loadedManager(t, cache)
	s := mgr.Get("alpha")
	s.SetOnline() // online before any stream-info fetch (id still empty)

	if s.Stream.StreakPending() {
		t.Fatal("unidentified broadcast + fresh grant: pursuit must be deferred, not started blind")
	}

	s.Stream.Update("bid-old", "t", nil, nil, 1)
	if s.Stream.StreakPending() {
		t.Fatal("identified as the granted broadcast: must stay blocked")
	}
	s.Stream.Update("bid-new", "t", nil, nil, 1)
	if !s.Stream.StreakPending() {
		t.Fatal("identified as a NEW broadcast: pursuit must start")
	}
}

// T5: no cache file at all -> exact historical behavior.
func TestRestartWithoutCachePursues(t *testing.T) {
	cache, _ := newCacheAt(t) // file never written
	mgr := loadedManager(t, cache)
	s := mgr.Get("alpha")
	s.Stream.Update("bid-live", "t", nil, nil, 1)
	s.SetOnline()
	if !s.Stream.StreakPending() {
		t.Fatal("empty cache must degrade to the historical pursue-on-restart behavior")
	}
}

// T8: TTL and corruption fail-safes.
func TestStreakCacheTTLAndCorruption(t *testing.T) {
	cache, path := newCacheAt(t)
	cache.Record("alpha", "bid-ancient", time.Now().Add(-streakCacheTTL-time.Hour))
	if got := cache.Load(time.Now()); len(got) != 0 {
		t.Fatalf("grant older than TTL must be dropped on load, got %v", got)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := cache.Load(time.Now()); len(got) != 0 {
		t.Fatalf("corrupt cache must load as empty (fail-safe), got %v", got)
	}

	// A corrupt file must also not break subsequent Records.
	cache.Record("alpha", "bid-live", time.Now())
	if got := cache.Load(time.Now()); got["alpha"].BroadcastID != "bid-live" {
		t.Fatalf("record after corruption must recover the cache, got %v", got)
	}
}

// Empty broadcast IDs are never persisted — they cannot be matched after a
// restart and would only add noise.
func TestStreakCacheSkipsEmptyBroadcast(t *testing.T) {
	cache, path := newCacheAt(t)
	cache.Record("alpha", "", time.Now())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("empty-broadcast grant must not create a cache file")
	}
}
