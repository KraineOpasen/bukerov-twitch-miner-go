package web

import (
	"context"
	"net/http"
	"net/http/httptest"
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

// TestQuickActionAndSettingsPostConcurrentKeepBothChanges is the CROSS-WRITER
// half of the BLOCK-5 guard. The Overview card quick action is a third
// settings writer with the same read-modify-write shape as the Settings-page
// save — GetRuntimeSettings, change one per-streamer field, hand the merged
// whole to the apply callback — so serializing only the two /api/settings
// entry points leaves the hazard fully open across the two endpoints: a card
// toggle and a Settings-page POST that overlap both merge onto the same stale
// snapshot, and whichever applies last reverts the other. That is a lost
// update between DISJOINT keys (one per-streamer field vs. one non-streamer
// analytics field), which is why both must survive.
//
// Same barrier discipline as the test above — channel rendezvous only, no
// sleep and no wall-clock wait, and neither branch can stall:
//
//   - The QUICK ACTION is started first and parks INSIDE its apply callback,
//     in the window between "it has read the snapshot and mutated" and "it has
//     published", so it is guaranteed to apply LAST and thus be the writer
//     whose stale snapshot would do the reverting.
//   - Only then is the settings POST started. With the quick action inside the
//     transaction the POST cannot reach its snapshot read at all and reports
//     from the lock's contention seam (bContended); without it the POST sails
//     through, reads the stale snapshot and applies (bApplied). Either signal
//     releases the quick action, so the test converges in both worlds and
//     asserts the same thing in both.
func TestQuickActionAndSettingsPostConcurrentKeepBothChanges(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Username = "tester"
	cfg.Streamers = []config.StreamerConfig{{Username: "alpha"}}
	cfg.Analytics.DaysAgo = 7

	// cfgMu guards cfg itself, exactly as in the test above: the two requests
	// genuinely run concurrently, and it is deliberately NOT the thing under
	// test — held only around each individual read or apply, it leaves the
	// read-modify-write window between them wide open.
	var cfgMu sync.Mutex
	readSettings := func() settings.RuntimeSettings {
		cfgMu.Lock()
		defer cfgMu.Unlock()
		return settings.BuildRuntimeSettings(&cfg)
	}

	quickInApply := make(chan struct{})
	postContended := make(chan struct{})
	postApplied := make(chan struct{})
	var applies int

	srv := &Server{settingsProvider: &funcSettingsProvider{get: readSettings}}
	srv.settingsTxnContended = func() { close(postContended) }
	srv.onSettingsUpdate = func(ctx context.Context, rt settings.RuntimeSettings) error {
		cfgMu.Lock()
		first := applies == 0
		applies++
		cfgMu.Unlock()

		if first {
			// This is the quick action: it has read and mutated, but has
			// published nothing yet. Open the window, then let the POST decide
			// how it ends.
			close(quickInApply)
			select {
			case <-postContended:
			case <-postApplied:
			}
		}

		cfgMu.Lock()
		settings.ApplyToConfig(&cfg, rt)
		cfgMu.Unlock()

		if !first {
			close(postApplied)
		}
		return nil
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/streamer-action/alpha", strings.NewReader(`{"action":"toggle-watch"}`))
		srv.handleAPIStreamerQuickAction(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("quick action status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
	}()

	<-quickInApply

	wg.Add(1)
	go func() {
		defer wg.Done()
		if rec := postSettings(t, srv, `{"analytics":{"daysAgo":3}}`); rec.Code != http.StatusOK {
			t.Errorf("settings POST status = %d, want 200", rec.Code)
		}
	}()

	wg.Wait()

	cfgMu.Lock()
	gotDaysAgo := cfg.Analytics.DaysAgo
	var gotDisableWatch bool
	if len(cfg.Streamers) == 1 && cfg.Streamers[0].Settings != nil {
		gotDisableWatch = cfg.Streamers[0].Settings.DisableWatch
	}
	cfgMu.Unlock()

	if !gotDisableWatch {
		t.Errorf("the quick action's change was lost: alpha DisableWatch = %v, want true", gotDisableWatch)
	}
	if gotDaysAgo != 3 {
		t.Errorf("the settings POST's change was lost: Analytics.DaysAgo = %d, want 3", gotDaysAgo)
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
		// Shallow copy, unlike the miner's cloneConfigLocked: reference fields
		// (AutoRedeem, DropRules) stay aliased to cfg, so ApplyToConfig's
		// ValidateConfig can reach them through the candidate. The assertions
		// below only read Analytics, which are plain value fields — anything
		// asserted on a map field would need a real deep copy first.
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
