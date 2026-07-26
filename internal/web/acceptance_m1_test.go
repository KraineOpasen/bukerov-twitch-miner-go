package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/analytics"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/database"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/streamerlifecycle"
)

// Feature: Fail-closed streamer deletion (SRAP, M1).
//
// This file pins the scenarios best observed at the HTTP contract boundary
// (POST /api/settings and /api/settings/reset): the response status/body a
// client actually sees, and that the server's own display cache and the
// settings the next GET returns are never mutated on a failed apply. It
// deliberately does NOT re-exercise the internal miner-level admission/commit
// machinery byte-for-byte (that is pinned exhaustively in
// internal/miner/srap_test.go and internal/streamerlifecycle/srap_test.go);
// instead each test wires a REAL streamerlifecycle.Coordinator and a REAL
// config file so the failure driving the HTTP response is genuine (a closed
// SQLite handle, an unwritable config path), not a synthetic error value.
//
// The remaining scenarios (S2's parent-context propagation is pinned here at
// the HTTP layer too, since "client disconnects" is fundamentally an
// HTTP-request-context concept — see TestAcceptanceClientDisconnectsBeforeCommit)
// live in internal/miner/acceptance_m1_test.go, where the real applySettings
// state machine (admission budgets, coordinatorMu, runtime roster) exists.

// openRawWebDB opens a PRIVATE, non-singleton database over its own file
// (mirrors internal/streamerlifecycle/durable_test.go's openRawDB and
// internal/miner/srap_test.go's openRawMinerDB): unlike database.Open, this
// never touches the package-wide TestMain singleton every other web test
// shares, so a test that deliberately closes its handle cannot break every
// other test in this binary.
func openRawWebDB(t *testing.T, path string) *database.DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB.SetMaxOpenConns(1)
	return &database.DB{DB: sqlDB}
}

// acceptanceLogCapture is a minimal slog.Handler recording every message
// emitted while installed, used to assert the exact truthful "durably
// queued" claim is ABSENT from a branch that never durably admitted
// anything.
type acceptanceLogCapture struct {
	mu   sync.Mutex
	msgs []string
}

func (h *acceptanceLogCapture) Enabled(context.Context, slog.Level) bool { return true }
func (h *acceptanceLogCapture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs = append(h.msgs, r.Message)
	return nil
}
func (h *acceptanceLogCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *acceptanceLogCapture) WithGroup(string) slog.Handler      { return h }

func (h *acceptanceLogCapture) containsSubstring(sub string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, m := range h.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

// acceptanceSettingsProvider is a mutable settings.SettingsProvider: unlike
// the package's existing fakeSettingsProvider (immutable, fixed at
// construction), its onSettingsUpdate-driven callback below updates it ONLY
// on a successful apply — so a failed apply leaving it untouched is a direct,
// literal assertion of "the operator's own view of current settings did not
// change".
type acceptanceSettingsProvider struct {
	mu sync.Mutex
	rt settings.RuntimeSettings
}

func (p *acceptanceSettingsProvider) GetRuntimeSettings() settings.RuntimeSettings {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rt
}

func (p *acceptanceSettingsProvider) GetDefaultSettings() settings.RuntimeSettings {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rt
}

func (p *acceptanceSettingsProvider) set(rt settings.RuntimeSettings) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rt = rt
}

// removedLogins returns every username present in before but absent from
// after (case-sensitive on purpose: the DTO layer already canonicalizes
// before it reaches config.StreamerConfig).
func removedLogins(before, after []settings.StreamerConfig) []string {
	present := make(map[string]bool, len(after))
	for _, sc := range after {
		present[sc.Username] = true
	}
	var out []string
	for _, sc := range before {
		if !present[sc.Username] {
			out = append(out, sc.Username)
		}
	}
	return out
}

// srapRemovalCallback builds a settings.SettingsUpdateCallback that mirrors
// the production applySettingsWithRemovals sequence (admit durably BEFORE
// any commit, commit is config.SaveConfig, only then publish the provider's
// view) closely enough to drive a genuine HTTP-visible fail-closed contract
// test: PREPARE (AdmitRemovals) -> last cancellation check -> COMMIT
// (config.SaveConfig) -> publish. Any failure compensates (AbortAdmission)
// and returns an error without ever calling provider.set, so a fail-closed
// apply is a literal no-op on both the config file and the provider.
func srapRemovalCallback(coord *streamerlifecycle.Coordinator, provider *acceptanceSettingsProvider, configPath string) settings.SettingsUpdateCallback {
	return func(ctx context.Context, rt settings.RuntimeSettings) error {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("settings apply rejected; no changes were made: %w", err)
		}

		current := provider.GetRuntimeSettings()
		removed := removedLogins(current.Streamers, rt.Streamers)

		if len(removed) == 0 {
			if err := config.SaveConfig(configPath, buildConfig(rt)); err != nil {
				return fmt.Errorf("settings apply rejected; no changes were made: persist config: %w", err)
			}
			provider.set(rt)
			return nil
		}

		batch := make([]streamerlifecycle.Removal, 0, len(removed))
		for _, login := range removed {
			batch = append(batch, streamerlifecycle.Removal{ChannelID: "chan-" + login, Login: login})
		}
		if err := coord.AdmitRemovals(ctx, batch); err != nil {
			return fmt.Errorf("settings apply rejected; no changes were made: admit streamer removal(s): %w", err)
		}
		slog.Info("Prepared streamer removal(s)", "count", len(batch))

		if err := ctx.Err(); err != nil {
			_ = coord.AbortAdmission(context.Background(), removed)
			return fmt.Errorf("settings apply rejected; no changes were made: %w", err)
		}

		candidate := buildConfig(rt)
		if err := config.SaveConfig(configPath, candidate); err != nil {
			_ = coord.AbortAdmission(context.Background(), removed)
			return fmt.Errorf("settings apply rejected; no changes were made: persist config: %w", err)
		}
		provider.set(rt)
		slog.Info("Streamer removal committed", "count", len(removed))
		return nil
	}
}

// buildConfig converts a RuntimeSettings DTO into a full config.Config the
// same way the production ApplyToConfig does, over a fresh DefaultConfig()
// base (mirroring the miner's cloneConfigLocked+ApplyToConfig candidate
// construction).
func buildConfig(rt settings.RuntimeSettings) *config.Config {
	cfg := config.DefaultConfig()
	settings.ApplyToConfig(&cfg, rt)
	return &cfg
}

// seedRuntimeSettings returns a minimal, valid RuntimeSettings body carrying
// exactly the streamers given.
func seedRuntimeSettings(usernames ...string) settings.RuntimeSettings {
	rt := settings.RuntimeSettings{
		Priority:   []string{"ORDER"},
		RateLimits: settings.RateLimitSettings{MinuteWatchedInterval: 60},
		Logger:     settings.LoggerSettings{ConsoleLevel: "info", FileLevel: "warn"},
		Analytics:  settings.AnalyticsUIConfig{Refresh: 5, DaysAgo: 7},
	}
	for _, u := range usernames {
		rt.Streamers = append(rt.Streamers, settings.StreamerConfig{Username: u, ChannelID: "chan-" + u})
	}
	return rt
}

func withoutStreamer(rt settings.RuntimeSettings, victim string) settings.RuntimeSettings {
	var kept []settings.StreamerConfig
	for _, sc := range rt.Streamers {
		if sc.Username != victim {
			kept = append(kept, sc)
		}
	}
	rt.Streamers = kept
	return rt
}

// TestAcceptanceDurableAdmissionFailsBeforeDeletionCommit is Scenario S1.
//
// Given a tracked streamer recorded in the runtime settings, on disk
// (config.json) and in a persisted store (SQLite),
// And SQLite is closed so the durable admission INSERT cannot possibly
// succeed,
// When an operator removes the streamer via the Settings API (POST
// /api/settings, driven through the real handleAPISettings handler via
// httptest),
// Then the API returns a non-2xx failure,
// And the runtime-visible settings (the next GET) are unchanged,
// And the persisted config.json file on disk is byte-for-byte unchanged,
// And no deletion fence was armed (a write for the victim's login still
// lands — it is never rejected with the fence's ErrStreamerDeleted),
// And no persisted history was removed,
// And the server log never claims the removal was "durably queued".
func TestAcceptanceDurableAdmissionFailsBeforeDeletionCommit(t *testing.T) {
	const victim, keep = "s1victim", "s1keep"
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "miner.db")
	db := openRawWebDB(t, dbPath)
	an, err := analytics.NewSQLiteRepository(db, dir)
	if err != nil {
		t.Fatalf("analytics repo: %v", err)
	}
	coord, err := streamerlifecycle.New(db, []streamerlifecycle.Purger{an}, []streamerlifecycle.Fencer{an}, nil)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}
	if err := an.RecordPoints(victim, 500, "WATCH"); err != nil {
		t.Fatalf("seed points: %v", err)
	}

	current := seedRuntimeSettings(victim, keep)
	configPath := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(configPath, buildConfig(current)); err != nil {
		t.Fatalf("seed config file: %v", err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}

	// SQLite rejects the pending-intent insertion: close the raw handle
	// AdmitRemovals will try to write through.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	provider := &acceptanceSettingsProvider{rt: current}
	srv := &Server{
		settingsProvider: provider,
		onSettingsUpdate: srapRemovalCallback(coord, provider, configPath),
	}

	cap := &acceptanceLogCapture{}
	prev := slog.Default()
	slog.SetDefault(slog.New(cap))
	defer slog.SetDefault(prev)

	body, err := json.Marshal(withoutStreamer(current, victim))
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	rec := postSettings(t, srv, string(body))

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want non-2xx (durable admission must fail closed)", rec.Code)
	}
	if strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("success body leaked despite a failed durable admission: %s", rec.Body.String())
	}

	// Runtime-visible settings unchanged.
	got := provider.GetRuntimeSettings()
	found := false
	for _, sc := range got.Streamers {
		if sc.Username == victim {
			found = true
		}
	}
	if !found {
		t.Error("victim missing from the settings the next GET would return, despite the admission failing")
	}

	// Persisted config file byte-for-byte unchanged.
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after: %v", err)
	}
	if string(before) != string(after) {
		t.Error("config.json was rewritten despite the durable admission failing")
	}

	// No fence armed: a write for the victim is rejected for some OTHER
	// reason (the DB is closed) but never with the fence's own sentinel
	// error, proving Tombstone (only ever called from CommitRemoval, i.e.
	// strictly after a commit this apply never reached) was never invoked.
	if writeErr := an.RecordPoints(victim, 1, "WATCH"); errors.Is(writeErr, analytics.ErrStreamerDeleted) {
		t.Error("victim's login was fenced despite the admission never committing")
	}

	// No persisted history removed: reopen a fresh handle to the same file
	// (the original is closed) and confirm the seeded points survive.
	reopened := openRawWebDB(t, dbPath)
	defer func() { _ = reopened.Close() }()
	reopenedAn, err := analytics.NewSQLiteRepository(reopened, dir)
	if err != nil {
		t.Fatalf("reopen analytics: %v", err)
	}
	if data, _ := reopenedAn.GetStreamerData(victim); len(data.Series) == 0 {
		t.Error("victim's analytics history was lost despite the admission failing pre-commit")
	}
	var admissions, pending int
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM streamer_deletion_admissions`).Scan(&admissions); err != nil {
		t.Fatalf("count admissions: %v", err)
	}
	if err := reopened.QueryRow(`SELECT COUNT(*) FROM pending_streamer_deletions`).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if admissions != 0 || pending != 0 {
		t.Errorf("admissions=%d pending=%d, want 0,0 (no fence/ledger row can remain when nothing was ever durably admitted)", admissions, pending)
	}

	// The log never claims durability that never happened.
	if cap.containsSubstring("durably queued") {
		t.Errorf("log claimed a durably-queued removal despite the admission itself failing: %v", cap.msgs)
	}
}

// TestAcceptanceClientDisconnectsBeforeCommit is Scenario S2.
//
// Given a tracked streamer and a settings-apply pipeline that would remove
// it,
// When the CLIENT disconnects before the request reaches the commit point —
// modeled the same way Go's own net/http models a disconnected client: the
// request's own context is already cancelled by the time the handler's
// callback runs (http.Server cancels r.Context() on client disconnect; this
// test constructs that condition directly via httptest.NewRequest(...)
// .WithContext(cancelledCtx), the standard way to simulate it without a real
// socket) —
// Then the apply aborts with zero visible or durable change: no config file
// write, no provider update, no durable admission row, and a non-2xx
// response.
func TestAcceptanceClientDisconnectsBeforeCommit(t *testing.T) {
	const victim, keep = "s2victim", "s2keep"
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "miner.db")
	db := openRawWebDB(t, dbPath)
	defer func() { _ = db.Close() }()
	an, err := analytics.NewSQLiteRepository(db, dir)
	if err != nil {
		t.Fatalf("analytics repo: %v", err)
	}
	coord, err := streamerlifecycle.New(db, []streamerlifecycle.Purger{an}, []streamerlifecycle.Fencer{an}, nil)
	if err != nil {
		t.Fatalf("coordinator: %v", err)
	}

	current := seedRuntimeSettings(victim, keep)
	configPath := filepath.Join(dir, "config.json")
	if err := config.SaveConfig(configPath, buildConfig(current)); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read seeded config: %v", err)
	}

	provider := &acceptanceSettingsProvider{rt: current}
	srv := &Server{
		settingsProvider: provider,
		onSettingsUpdate: srapRemovalCallback(coord, provider, configPath),
	}

	body, err := json.Marshal(withoutStreamer(current, victim))
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the client is already gone by the time the handler runs
	req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(string(body))).WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleAPISettings(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want non-2xx for a client that disconnected before commit", rec.Code)
	}

	got := provider.GetRuntimeSettings()
	found := false
	for _, sc := range got.Streamers {
		if sc.Username == victim {
			found = true
		}
	}
	if !found {
		t.Error("victim missing from settings despite the request being cancelled before any commit")
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config after: %v", err)
	}
	if string(before) != string(after) {
		t.Error("config.json was rewritten despite the request being cancelled before commit")
	}
	if has, err := coord.HasPending(context.Background(), victim); err != nil || has {
		t.Errorf("HasPending = (%v, %v), want (false, nil) — admission must never even be attempted for an already-cancelled request", has, err)
	}
}

// TestAcceptanceSettingsResetFailsSafely is Scenario S7.
//
// Given settings currently active (refresh=5, daysAgo=7) with a tracked
// streamer,
// When an operator triggers a settings reset (POST /api/settings/reset)
// whose apply cannot durably persist (config.SaveConfig's target path is
// unwritable — a real, not synthetic, failure of the apply's own commit
// step),
// Then the reset endpoint returns a non-2xx failure,
// And the server's cache (refresh/daysAgo) is unchanged,
// And a subsequent GET /api/settings still returns the ORIGINAL settings,
// not the defaults.
func TestAcceptanceSettingsResetFailsSafely(t *testing.T) {
	dir := t.TempDir()
	current := seedRuntimeSettings("s7keep")
	current.Analytics = settings.AnalyticsUIConfig{Refresh: 5, DaysAgo: 7}

	// configPath is a DIRECTORY, not a file: config.SaveConfig's rename step
	// can never succeed against it — a genuine, deterministic apply failure
	// (mirrors internal/miner/cp1_c2_matrix_test.go's breakConfigPathForNextSave
	// seam), standing in for "the apply generally fails" per the scenario's
	// own alternative framing (a reset's own default-settings body carries no
	// removal, so there is no admission step to fail here).
	configPath := filepath.Join(dir, "config.json")
	if err := os.Mkdir(configPath, 0o755); err != nil {
		t.Fatalf("seed unwritable config path: %v", err)
	}

	provider := &acceptanceSettingsProvider{rt: current}
	srv := &Server{
		settingsProvider: provider,
		onSettingsUpdate: func(ctx context.Context, rt settings.RuntimeSettings) error {
			if err := config.SaveConfig(configPath, buildConfig(rt)); err != nil {
				return fmt.Errorf("settings apply rejected; no changes were made: persist config: %w", err)
			}
			provider.set(rt)
			return nil
		},
		refresh: 5,
		daysAgo: 7,
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/settings/reset", nil)
	srv.handleAPISettingsReset(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want non-2xx when the reset's own apply cannot persist", rec.Code)
	}

	srv.mu.RLock()
	gotRefresh, gotDaysAgo := srv.refresh, srv.daysAgo
	srv.mu.RUnlock()
	if gotRefresh != 5 || gotDaysAgo != 7 {
		t.Errorf("cache changed on a failed reset: refresh=%d daysAgo=%d, want 5,7", gotRefresh, gotDaysAgo)
	}

	// A subsequent GET must still return the ORIGINAL settings, not defaults.
	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	srv.handleAPISettings(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET after failed reset: status = %d, want 200", getRec.Code)
	}
	var got settings.RuntimeSettings
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode GET body: %v", err)
	}
	if got.Analytics.Refresh != 5 || got.Analytics.DaysAgo != 7 {
		t.Errorf("GET after failed reset returned refresh=%d daysAgo=%d, want the original 5,7 (not defaults)", got.Analytics.Refresh, got.Analytics.DaysAgo)
	}
}
