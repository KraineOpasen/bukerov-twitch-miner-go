package miner

import (
	"fmt"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

func TestMergeStreamerLoginsDedups(t *testing.T) {
	existing := []settings.StreamerConfig{{Username: "alpha"}, {Username: "beta"}}
	// "Beta" already tracked (case-insensitive), "gamma" repeated in input, "  "
	// blank — only gamma and delta are genuinely new.
	merged, added := mergeStreamerLogins(existing, []string{"Beta", "gamma", "GAMMA", "delta", "  "})

	if added != 2 {
		t.Fatalf("added = %d, want 2", added)
	}
	counts := map[string]int{}
	for _, sc := range merged {
		counts[sc.Username]++
	}
	for _, want := range []string{"alpha", "beta", "gamma", "delta"} {
		if counts[want] != 1 {
			t.Errorf("%q appears %d times, want exactly 1", want, counts[want])
		}
	}
	if len(merged) != 4 {
		t.Errorf("merged has %d entries, want 4: %+v", len(merged), merged)
	}
}

func TestMergeStreamerLoginsDoesNotMutateInput(t *testing.T) {
	existing := []settings.StreamerConfig{{Username: "alpha"}}
	merged, _ := mergeStreamerLogins(existing, []string{"beta"})
	if len(existing) != 1 {
		t.Errorf("input slice was mutated: len = %d, want 1", len(existing))
	}
	if len(merged) != 2 {
		t.Errorf("merged len = %d, want 2", len(merged))
	}
}

// TestImportStreamersConcurrentExactlyOnce is the race guard for the
// read-modify-write in ImportStreamers. Each goroutine imports the same shared
// login plus one distinct login; with importMu serializing the snapshot→apply
// sequence, the final list must contain every login EXACTLY once — the shared
// one is never duplicated, and no goroutine's distinct login is lost to a
// concurrent wholesale-replace. Run under -race.
func TestImportStreamersConcurrentExactlyOnce(t *testing.T) {
	m := &Miner{config: &config.Config{}}
	// Stand in for ApplySettings: persist the streamer list back into m.config
	// under mu, exactly as ApplyToConfig's wholesale replace would, but without
	// the network/pubsub side effects of the real apply path.
	m.importApply = func(s settings.RuntimeSettings) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.config.Streamers = make([]config.StreamerConfig, len(s.Streamers))
		for i, sc := range s.Streamers {
			m.config.Streamers[i] = config.StreamerConfig{Username: sc.Username}
		}
	}

	const goroutines = 12
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func(i int) {
			defer wg.Done()
			// "common" is imported by every goroutine; "chan_i" is unique.
			if _, err := m.ImportStreamers([]string{"common", fmt.Sprintf("chan_%d", i)}); err != nil {
				t.Errorf("ImportStreamers: %v", err)
			}
		}(i)
	}
	wg.Wait()

	counts := map[string]int{}
	for _, sc := range m.config.Streamers {
		counts[sc.Username]++
	}
	if counts["common"] != 1 {
		t.Errorf("shared login duplicated: appears %d times, want exactly 1", counts["common"])
	}
	for i := 0; i < goroutines; i++ {
		name := fmt.Sprintf("chan_%d", i)
		if counts[name] != 1 {
			t.Errorf("distinct login %q appears %d times, want exactly 1 (lost update?)", name, counts[name])
		}
	}
	// common + one per goroutine, each exactly once.
	if len(m.config.Streamers) != goroutines+1 {
		t.Errorf("final list has %d entries, want %d", len(m.config.Streamers), goroutines+1)
	}
}
