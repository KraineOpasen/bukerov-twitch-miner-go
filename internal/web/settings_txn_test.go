package web

import (
	"context"
	"encoding/json"
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

// --- Discord bot token secrecy: the token as a write-only secret ---
//
// These cover GET /api/settings and BOTH settings writers that can carry a
// token (the save and the reset), so they sit with the other whole-endpoint
// settings-transaction guards rather than in settings_reset_test.go — the
// reset case here is one half of a single invariant, not a reset-specific
// one, and splitting the pair would hide what makes each necessary.
// Deliberately duplicated one layer down in internal/settings/builder_test.go:
// these pin the HTTP seam a browser actually reaches, those pin the DTO rules
// that seam depends on, and a bug in either layer must not be masked by the
// other.

// discordTokenSentinel is the fake value planted as the FILE-managed Discord
// bot token. It is deliberately distinctive so a raw-body substring search can
// prove the secret never reaches the browser through ANY part of the response
// — not just through the one field the parsed assertions look at.
const discordTokenSentinel = "file-managed-sentinel-not-a-real-token"

// discordTokenProvider serves both the live runtime settings and the reset
// defaults from one config through the REAL builder, so these tests exercise
// the production GET/POST/reset pipeline (BuildRuntimeSettings /
// BuildDefaultSettings / ApplyToConfig) rather than a hand-written DTO that
// could agree with a broken builder.
//
// funcSettingsProvider is not reused: it answers GetRuntimeSettings and
// GetDefaultSettings from the SAME closure, and the whole point here is that
// the two differ — only the defaults carry the marker that clears the token.
type discordTokenProvider struct{ cfg *config.Config }

func (p *discordTokenProvider) GetRuntimeSettings() settings.RuntimeSettings {
	return settings.BuildRuntimeSettings(p.cfg)
}

func (p *discordTokenProvider) GetDefaultSettings() settings.RuntimeSettings {
	return settings.BuildDefaultSettings(p.cfg.Streamers)
}

// newDiscordTokenServer wires a Server whose apply callback runs the same
// settings.ApplyToConfig commit the miner's apply path runs, against the
// returned config. Persistence is out of scope here (BLOCK-1 owns that seam);
// what is under test is which token value survives the DTO round trip.
func newDiscordTokenServer(t *testing.T, fromEnv bool) (*Server, *config.Config) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Username = "tester"
	cfg.Streamers = []config.StreamerConfig{{Username: "alpha"}}
	cfg.Analytics.DaysAgo = 7
	cfg.Discord.Enabled = true
	cfg.Discord.BotToken = discordTokenSentinel
	cfg.Discord.GuildID = "guild-1"
	cfg.DiscordTokenFromEnv = fromEnv

	srv := &Server{settingsProvider: &discordTokenProvider{cfg: &cfg}}
	srv.onSettingsUpdate = func(_ context.Context, rt settings.RuntimeSettings) error {
		settings.ApplyToConfig(&cfg, rt)
		return nil
	}
	return srv, &cfg
}

func getSettings(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleAPISettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))
	return rec
}

func resetSettings(t *testing.T, srv *Server) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleAPISettingsReset(rec, httptest.NewRequest(http.MethodPost, "/api/settings/reset", nil))
	return rec
}

// decodeDiscordBlock pulls the discord block out of a settings response so an
// assertion can name the field, while the caller separately searches the RAW
// body for the sentinel.
func decodeDiscordBlock(t *testing.T, rec *httptest.ResponseRecorder) settings.DiscordUIConfig {
	t.Helper()
	var got struct {
		Discord settings.DiscordUIConfig `json:"discord"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decoding the settings response failed: %v", err)
	}
	return got.Discord
}

// TestSettingsGetNeverExposesDiscordBotToken is the headline secrecy guard:
// the Discord bot token is a WRITE-ONLY secret, so GET /api/settings must
// never serialize the real value — under EITHER ownership.
//
// Env-managed was already hidden (uiBotToken). File-managed was not: the
// dashboard handed the live bot token to every browser that loaded the
// Settings page, where it sat in the DOM, in the browser's memory, and in any
// proxy/devtools/HAR capture of that response.
//
// The assertion is made on the RAW body, not only on the decoded field, so it
// covers the whole response rather than the one key the fix touches.
func TestSettingsGetNeverExposesDiscordBotToken(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fromEnv bool
	}{
		{"file-managed", false},
		{"env-managed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newDiscordTokenServer(t, tc.fromEnv)

			rec := getSettings(t, srv)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if strings.Contains(rec.Body.String(), discordTokenSentinel) {
				t.Errorf("GET /api/settings leaked the Discord bot token in its body")
			}
			discord := decodeDiscordBlock(t, rec)
			if discord.BotToken != "" {
				t.Errorf("discord.botToken = %q, want the secret withheld", discord.BotToken)
			}
			// Non-secret Discord state must still round-trip: withholding the
			// token must not blank the block the Settings page renders.
			if !discord.Enabled {
				t.Errorf("discord.enabled = false, want the non-secret state preserved")
			}
			if discord.GuildID != "guild-1" {
				t.Errorf("discord.guildId = %q, want %q", discord.GuildID, "guild-1")
			}
		})
	}
}

// TestSettingsPostWithoutTokenPreservesFileManagedToken is the other half of
// the fix, and the reason GET redaction cannot be done on its own.
//
// Every settings mutation is a read-modify-write over the DTO, and the
// Settings page posts the whole DTO back — including discord.botToken, which
// is now always empty because GET withholds it. Redacting GET without adding
// preservation semantics would therefore make the FIRST unrelated save (or
// any card quick action, or a followed-channel import) silently erase the
// configured token. An omitted or empty token means "no new token", not
// "clear it".
func TestSettingsPostWithoutTokenPreservesFileManagedToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{
			// Exactly what the Settings page sends after GET withheld the
			// secret: the discord block is present, its token empty.
			name: "explicit empty token",
			body: `{"analytics":{"refresh":5,"daysAgo":3},"discord":{"enabled":true,"botToken":"","guildId":"guild-1"}}`,
		},
		{
			// A partial body that never mentions discord at all.
			name: "discord block omitted",
			body: `{"analytics":{"refresh":5,"daysAgo":3}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cfg := newDiscordTokenServer(t, false)

			rec := postSettings(t, srv, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if cfg.Discord.BotToken != discordTokenSentinel {
				t.Errorf("the file-managed Discord token was erased by an unrelated save: %q", cfg.Discord.BotToken)
			}
			// The rest of the save must still have landed.
			if cfg.Analytics.DaysAgo != 3 {
				t.Errorf("analytics.daysAgo = %d, want 3", cfg.Analytics.DaysAgo)
			}
			if !cfg.Discord.Enabled || cfg.Discord.GuildID != "guild-1" {
				t.Errorf("non-secret Discord state changed: %+v", cfg.Discord)
			}
		})
	}
}

// TestSettingsPostWithExplicitTokenReplacesFileManagedToken pins the write
// half of the write-only secret: typing a new token into the Settings page
// still replaces the stored one. Preservation must apply to the ABSENCE of a
// value, never to a value the user actually supplied.
func TestSettingsPostWithExplicitTokenReplacesFileManagedToken(t *testing.T) {
	srv, cfg := newDiscordTokenServer(t, false)

	rec := postSettings(t, srv, `{"discord":{"enabled":true,"botToken":"replacement-token","guildId":"guild-2"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if cfg.Discord.BotToken != "replacement-token" {
		t.Errorf("discord token = %q, want the explicitly posted replacement", cfg.Discord.BotToken)
	}
	if cfg.Discord.GuildID != "guild-2" {
		t.Errorf("discord.guildId = %q, want %q", cfg.Discord.GuildID, "guild-2")
	}
}

// TestSettingsPostCannotOverrideEnvManagedToken pins the env ownership rule
// end to end: while DISCORD_BOT_TOKEN is set the environment is the sole
// source of truth, so no posted value — empty or not — may reach the config.
func TestSettingsPostCannotOverrideEnvManagedToken(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"explicit token", `{"discord":{"enabled":true,"botToken":"attacker-supplied","guildId":"guild-1"}}`},
		{"empty token", `{"discord":{"enabled":true,"botToken":"","guildId":"guild-1"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, cfg := newDiscordTokenServer(t, true)

			if rec := postSettings(t, srv, tc.body); rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if cfg.Discord.BotToken != discordTokenSentinel {
				t.Errorf("a POST overrode the env-managed token: %q", cfg.Discord.BotToken)
			}
		})
	}
}

// TestSettingsResetCannotClearEnvManagedDiscordToken completes the env
// ownership rule at this seam: the reset is the ONE writer whose DTO carries
// an authoritative token, so it is the one that could clear a secret the
// environment still owns. It must not — the token would come straight back on
// the next load, while SaveConfig has already dropped the on-disk copy,
// leaving config and runtime disagreeing about the secret.
//
// Pinned here and not only in internal/settings/builder_test.go because the
// reset reaches ApplyToConfig through a different handler than the save, and
// a regression on that path would otherwise pass the whole web suite.
func TestSettingsResetCannotClearEnvManagedDiscordToken(t *testing.T) {
	srv, cfg := newDiscordTokenServer(t, true)

	rec := resetSettings(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if cfg.Discord.BotToken != discordTokenSentinel {
		t.Errorf("reset cleared the env-managed token: %q", cfg.Discord.BotToken)
	}
	if strings.Contains(rec.Body.String(), discordTokenSentinel) {
		t.Errorf("POST /api/settings/reset leaked the env-managed token in its body")
	}
}

// TestSettingsResetClearsFileManagedDiscordToken PINS the pre-existing reset
// semantics, unchanged by the write-only rule: "Reset to defaults" rebuilds
// the DTO from config defaults, whose Discord block is entirely empty, and
// that reset genuinely clears a file-managed token.
//
// This is the constraint that rules out the obvious one-line fix. Preservation
// cannot simply be "an empty token means keep the current one", because the
// reset posts an empty token too and must keep clearing it. The two intents
// have to be distinguishable, and this test is what stops the secrecy work
// from silently turning "Reset to defaults" into a reset that leaves the
// secret behind.
func TestSettingsResetClearsFileManagedDiscordToken(t *testing.T) {
	srv, cfg := newDiscordTokenServer(t, false)

	rec := resetSettings(t, srv)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if cfg.Discord.BotToken != "" {
		t.Errorf("reset to defaults left the Discord token behind: %q", cfg.Discord.BotToken)
	}
	if cfg.Discord.Enabled || cfg.Discord.GuildID != "" {
		t.Errorf("reset left non-default Discord state: %+v", cfg.Discord)
	}
	// The reset's own response echoes the applied defaults; it must not carry
	// the secret it just cleared either.
	if strings.Contains(rec.Body.String(), discordTokenSentinel) {
		t.Errorf("POST /api/settings/reset leaked the Discord bot token in its body")
	}
}

// TestSettingsResetIsNotTriggeredByAnEquivalentPost guards the mechanism that
// makes the two intents above distinguishable: "clear the token" is carried
// by the reset DTO itself, NOT inferred from its contents. A hand-crafted POST
// whose body happens to look like the defaults is still an ordinary save, so
// it preserves the token rather than clearing it.
func TestSettingsResetIsNotTriggeredByAnEquivalentPost(t *testing.T) {
	srv, cfg := newDiscordTokenServer(t, false)

	rec := postSettings(t, srv, `{"discord":{"enabled":false,"botToken":"","guildId":""}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if cfg.Discord.BotToken != discordTokenSentinel {
		t.Errorf("a defaults-shaped POST cleared the token like a reset: %q", cfg.Discord.BotToken)
	}
}
