package web

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// TestSettingsPostConcurrentPartialBodiesKeepBothChanges is the BLOCK-5
// regression guard. A partial POST is a read-modify-write: the handler reads
// the CURRENT settings, decodes the partial body onto that snapshot, then
// applies the merged result wholesale. Two concurrent partial POSTs that both
// read before either applies therefore both merge onto the same stale
// snapshot, and whichever applies last silently reverts the other's change —
// even though the two bodies touch completely disjoint keys.
//
// The interleaving is forced with channel barriers only; there is no sleep
// and no wall-clock wait anywhere, and neither branch of the rendezvous can
// stall:
//
//   - Request A is started first and parks INSIDE its apply callback — the
//     exact window between "A has read the snapshot" and "A has published its
//     result" that the defect lives in. Only once A is parked there (aInApply)
//     is request B started, so A is guaranteed to be the request that applies
//     last, and thus the one whose stale snapshot would do the reverting.
//   - A then waits for whichever of two mutually exclusive things happens
//     next. With the transaction lock in place, B cannot reach its snapshot
//     read at all: it finds the transaction held and signals bContended from
//     the lock's own contention seam, releasing A. Without the lock, B sails
//     through, reads the stale snapshot and applies — signalling bApplied,
//     which releases A just the same. So the test converges in both worlds and
//     asserts the same thing in both: whatever the interleaving, BOTH posted
//     changes must survive.
func TestSettingsPostConcurrentPartialBodiesKeepBothChanges(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Username = "tester"
	cfg.Streamers = []config.StreamerConfig{{Username: "alpha"}}
	cfg.Logger.ConsoleLevel = "info"
	cfg.Analytics.DaysAgo = 7

	// cfgMu guards cfg itself: the two requests genuinely run concurrently, so
	// the shared config they read and apply through needs its own lock
	// regardless of whether the handler serializes them. It is deliberately
	// NOT the thing under test — holding it only around each individual read
	// or apply leaves the read-modify-write window between them wide open,
	// which is precisely the window BLOCK-5 is about.
	var cfgMu sync.Mutex
	readSettings := func() settings.RuntimeSettings {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		return settings.BuildRuntimeSettings(&cfg)
	}

	aInApply := make(chan struct{})
	bContended := make(chan struct{})
	bApplied := make(chan struct{})
	var applies int

	srv := &Server{settingsProvider: &funcSettingsProvider{get: readSettings}}
	srv.settingsTxnContended = func() { close(bContended) }
	srv.onSettingsUpdate = func(ctx context.Context, rt settings.RuntimeSettings) error {
		cfgMu.Lock()
		first := applies == 0
		applies++
		cfgMu.Unlock()

		if first {
			// This is A: it has already read and merged, but has published
			// nothing yet. Open the window, then let B decide how it ends.
			close(aInApply)
			select {
			case <-bContended:
			case <-bApplied:
			}
		}

		cfgMu.Lock()
		settings.ApplyToConfig(&cfg, rt)
		cfgMu.Unlock()

		if !first {
			close(bApplied)
		}
		return nil
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if rec := postSettings(t, srv, `{"logger":{"consoleLevel":"debug"}}`); rec.Code != http.StatusOK {
			t.Errorf("request A status = %d, want 200", rec.Code)
		}
	}()

	<-aInApply

	wg.Add(1)
	go func() {
		defer wg.Done()
		if rec := postSettings(t, srv, `{"analytics":{"daysAgo":3}}`); rec.Code != http.StatusOK {
			t.Errorf("request B status = %d, want 200", rec.Code)
		}
	}()

	wg.Wait()

	cfgMu.Lock()
	gotConsole, gotDaysAgo := cfg.Logger.ConsoleLevel, cfg.Analytics.DaysAgo
	cfgMu.Unlock()

	if gotConsole != "debug" {
		t.Errorf("request A's change was lost: Logger.ConsoleLevel = %q, want \"debug\"", gotConsole)
	}
	if gotDaysAgo != 3 {
		t.Errorf("request B's change was lost: Analytics.DaysAgo = %d, want 3", gotDaysAgo)
	}
}

// TestSettingsPostPersistenceFailureNo200 is the HTTP half of BLOCK-1: when
// the apply's durable write genuinely fails, POST /api/settings must not
// answer 200 or emit a success body, and must leave the server's own
// refresh/daysAgo display cache untouched.
//
// The callback here performs the SAME two steps the miner's apply commit
// point does — settings.ApplyToConfig onto a candidate, then
// config.SaveConfig — against a directory that does not exist, so the error
// under test is a real config.SaveConfig failure rather than a hand-written
// stand-in. internal/web cannot import internal/miner (the miner imports the
// web server), so the miner-side half of this invariant — that such a failure
// is returned at all, and mutates nothing — is pinned next to that code in
// internal/miner/settings_persistence_test.go.
func TestSettingsPostPersistenceFailureNo200(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Username = "tester"
	cfg.Analytics.Refresh = 5
	cfg.Analytics.DaysAgo = 7
	missingDir := filepath.Join(t.TempDir(), "no-such-dir", "config.json")

	srv := &Server{
		settingsProvider: &funcSettingsProvider{get: func() settings.RuntimeSettings {
			return settings.BuildRuntimeSettings(&cfg)
		}},
		refresh: 5,
		daysAgo: 7,
	}
	srv.onSettingsUpdate = func(ctx context.Context, rt settings.RuntimeSettings) error {
		candidate := cfg
		settings.ApplyToConfig(&candidate, rt)
		if err := config.SaveConfig(missingDir, &candidate); err != nil {
			return err
		}
		cfg = candidate
		return nil
	}

	rec := postSettings(t, srv, `{"analytics":{"refresh":99,"daysAgo":99}}`)

	if rec.Code == http.StatusOK {
		t.Errorf("status = %d, want non-2xx when the durable write failed", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("success body returned for a failed durable write: %s", rec.Body.String())
	}
	if cfg.Analytics.Refresh != 5 || cfg.Analytics.DaysAgo != 7 {
		t.Errorf("config mutated despite the failed durable write: %+v", cfg.Analytics)
	}
	srv.mu.RLock()
	gotRefresh, gotDaysAgo := srv.refresh, srv.daysAgo
	srv.mu.RUnlock()
	if gotRefresh != 5 || gotDaysAgo != 7 {
		t.Errorf("display cache changed after a failed durable write: refresh=%d daysAgo=%d, want 5,7", gotRefresh, gotDaysAgo)
	}
}
