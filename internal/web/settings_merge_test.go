package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// mergeTestServer builds a bare Server whose provider returns a fully
// populated current-settings body, capturing whatever the POST handler hands
// to the apply callback.
func mergeTestServer(applied *[]settings.RuntimeSettings) *Server {
	alphaSettings := &settings.StreamerSettingsConfig{}
	claim := true
	alphaSettings.ClaimDrops = &claim

	current := settings.RuntimeSettings{
		Streamers: []settings.StreamerConfig{
			{Username: "alpha", Settings: alphaSettings},
			{Username: "beta"},
		},
		Priority: []string{"ORDER"},
		RateLimits: settings.RateLimitSettings{
			MinuteWatchedInterval: 45,
			CampaignSyncInterval:  30,
		},
		Logger:        settings.LoggerSettings{ConsoleLevel: "info", FileLevel: "warn", Colored: true},
		Analytics:     settings.AnalyticsUIConfig{Refresh: 5, DaysAgo: 7},
		DropBlacklist: []string{"skip-me"},
	}

	return &Server{
		settingsProvider: &fakeSettingsProvider{rt: current},
		onSettingsUpdate: func(rt settings.RuntimeSettings) {
			*applied = append(*applied, rt)
		},
	}
}

func postSettings(t *testing.T, srv *Server, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/settings", strings.NewReader(body))
	srv.handleAPISettings(rec, req)
	return rec
}

// TestSettingsPostPartialBodyKeepsOmittedFields is the regression guard for
// the partial-settings wipe: a body carrying only one (partial!) block must
// change exactly the posted fields — every omitted key, including fields
// omitted INSIDE a posted block, keeps its current value instead of being
// zeroed on the way into ApplyToConfig's wholesale replace.
func TestSettingsPostPartialBodyKeepsOmittedFields(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := mergeTestServer(&applied)

	rec := postSettings(t, srv, `{"logger":{"consoleLevel":"debug"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(applied) != 1 {
		t.Fatalf("apply callback invoked %d times, want 1", len(applied))
	}
	got := applied[0]

	if got.Logger.ConsoleLevel != "debug" {
		t.Fatalf("posted field not applied: consoleLevel = %q", got.Logger.ConsoleLevel)
	}
	if got.Logger.FileLevel != "warn" || !got.Logger.Colored {
		t.Fatalf("fields omitted inside the posted block were zeroed: %+v", got.Logger)
	}
	if got.RateLimits.MinuteWatchedInterval != 45 || got.RateLimits.CampaignSyncInterval != 30 {
		t.Fatalf("omitted rateLimits block was zeroed: %+v", got.RateLimits)
	}
	if len(got.Streamers) != 2 || got.Streamers[0].Username != "alpha" || got.Streamers[1].Username != "beta" {
		t.Fatalf("omitted streamers block was altered: %+v", got.Streamers)
	}
	if !reflect.DeepEqual(got.DropBlacklist, []string{"skip-me"}) || !reflect.DeepEqual(got.Priority, []string{"ORDER"}) {
		t.Fatalf("omitted list fields were zeroed: blacklist=%v priority=%v", got.DropBlacklist, got.Priority)
	}
	if got.Analytics.Refresh != 5 || got.Analytics.DaysAgo != 7 {
		t.Fatalf("omitted analytics block was zeroed: %+v", got.Analytics)
	}
}

// TestSettingsPostWithoutStreamersKeepsRoster pins the worst case of the bug:
// a body without a "streamers" key must NOT wipe the streamer list.
func TestSettingsPostWithoutStreamersKeepsRoster(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := mergeTestServer(&applied)

	rec := postSettings(t, srv, `{"dropBlacklist":["other"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := applied[0]
	if len(got.Streamers) != 2 {
		t.Fatalf("absent streamers key wiped the roster: %+v", got.Streamers)
	}
	if got.Streamers[0].Settings == nil || got.Streamers[0].Settings.ClaimDrops == nil || !*got.Streamers[0].Settings.ClaimDrops {
		t.Fatalf("per-streamer settings lost: %+v", got.Streamers[0].Settings)
	}
	if !reflect.DeepEqual(got.DropBlacklist, []string{"other"}) {
		t.Fatalf("posted list not applied: %v", got.DropBlacklist)
	}
}

// TestSettingsPostEmptyStreamersListClears: an EXPLICIT empty list is an
// intentional clear and must still empty the roster — absent and empty are
// different operations.
func TestSettingsPostEmptyStreamersListClears(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := mergeTestServer(&applied)

	rec := postSettings(t, srv, `{"streamers":[]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := applied[0].Streamers; len(got) != 0 {
		t.Fatalf("explicit empty streamers list must clear the roster, got %+v", got)
	}
}

// TestSettingsPostStreamersReplaceWithoutElementBleedThrough guards the
// encoding/json slice-reuse pitfall: decoding a posted streamers array over
// the seeded slice would append into the retained backing array, so a posted
// element could inherit leftover fields — including the previous occupant's
// per-streamer Settings pointer — from its index. A posted element must carry
// exactly what was posted.
func TestSettingsPostStreamersReplaceWithoutElementBleedThrough(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := mergeTestServer(&applied)

	rec := postSettings(t, srv, `{"streamers":[{"username":"gamma"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := applied[0].Streamers
	if len(got) != 1 || got[0].Username != "gamma" {
		t.Fatalf("posted streamers list not applied verbatim: %+v", got)
	}
	if got[0].Settings != nil {
		t.Fatalf("posted element inherited the previous occupant's Settings pointer: %+v", got[0].Settings)
	}
}

// TestSettingsPostExplicitFalseAppliesOverTrue: merge semantics must never be
// implemented as "skip zero values" — an EXPLICIT false posted over a current
// true is applied, while the untouched sibling fields of the same block keep
// their current values.
func TestSettingsPostExplicitFalseAppliesOverTrue(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := mergeTestServer(&applied) // current Logger.Colored = true

	rec := postSettings(t, srv, `{"logger":{"colored":false}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := applied[0].Logger
	if got.Colored {
		t.Fatal("explicit false was swallowed by the merge: logger.colored stayed true")
	}
	if got.ConsoleLevel != "info" || got.FileLevel != "warn" {
		t.Fatalf("sibling fields of the posted block must keep current values: %+v", got)
	}
}

// TestSettingsPostExplicitZeroAppliesOverNonZero: an EXPLICIT 0 posted over a
// non-zero current value is applied. The field is deliberately
// analytics.daysAgo — zero is a legal value there and config.ValidateConfig
// does not clamp it (unlike every rateLimits interval, where a zero would be
// silently rewritten to the clamp minimum and the assertion would lie).
func TestSettingsPostExplicitZeroAppliesOverNonZero(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := mergeTestServer(&applied) // current Analytics.DaysAgo = 7

	rec := postSettings(t, srv, `{"analytics":{"daysAgo":0}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := applied[0].Analytics
	if got.DaysAgo != 0 {
		t.Fatalf("explicit zero was swallowed by the merge: daysAgo = %d, want 0", got.DaysAgo)
	}
	if got.Refresh != 5 {
		t.Fatalf("sibling field of the posted block must keep its current value: refresh = %d, want 5", got.Refresh)
	}
}

// funcSettingsProvider adapts a closure so the provider can serve LIVE
// settings built from a mutating config (unlike fakeSettingsProvider's
// static snapshot).
type funcSettingsProvider struct {
	get func() settings.RuntimeSettings
}

func (f *funcSettingsProvider) GetRuntimeSettings() settings.RuntimeSettings { return f.get() }
func (f *funcSettingsProvider) GetDefaultSettings() settings.RuntimeSettings { return f.get() }

// TestSettingsPostPartialRoundTripThroughConfigFile drives the full
// production pipeline for a partial body: handler merge -> ApplyToConfig
// (which ends in config.ValidateConfig's silent clamps) -> SaveConfig ->
// LoadConfig (which re-runs ValidateConfig) -> BuildRuntimeSettings. The
// effective settings right after the apply and after a reload from disk must
// be identical — this is what pins the merge x clamp interaction: the posted
// out-of-range interval must be clamped ONCE at apply time and then survive
// the disk round-trip unchanged, and the explicit unclamped zero must
// survive it verbatim.
func TestSettingsPostPartialRoundTripThroughConfigFile(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")

	cfg := config.DefaultConfig()
	cfg.Username = "tester"
	cfg.Streamers = []config.StreamerConfig{{Username: "alpha"}}
	cfg.Analytics.DaysAgo = 7

	var afterApply settings.RuntimeSettings
	srv := &Server{
		settingsProvider: &funcSettingsProvider{get: func() settings.RuntimeSettings {
			return settings.BuildRuntimeSettings(&cfg)
		}},
		onSettingsUpdate: func(rt settings.RuntimeSettings) {
			settings.ApplyToConfig(&cfg, rt)
			if err := config.SaveConfig(cfgPath, &cfg); err != nil {
				t.Fatalf("SaveConfig: %v", err)
			}
			afterApply = settings.BuildRuntimeSettings(&cfg)
		},
	}

	// minuteWatchedInterval:5 is below ValidateConfig's floor of 30 (an
	// intentionally clamped input); daysAgo:0 is a legal unclamped zero.
	rec := postSettings(t, srv, `{"rateLimits":{"minuteWatchedInterval":5},"analytics":{"daysAgo":0}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	afterLoad := settings.BuildRuntimeSettings(loaded)

	if !reflect.DeepEqual(afterApply, afterLoad) {
		t.Fatalf("effective settings changed across the disk round-trip:\nafter apply: %+v\nafter load:  %+v", afterApply, afterLoad)
	}
	if afterLoad.RateLimits.MinuteWatchedInterval != 30 {
		t.Fatalf("out-of-range posted interval must be clamped exactly once to 30, got %d", afterLoad.RateLimits.MinuteWatchedInterval)
	}
	if afterLoad.Analytics.DaysAgo != 0 {
		t.Fatalf("legal explicit zero must survive the round-trip, got %d", afterLoad.Analytics.DaysAgo)
	}
	if len(afterLoad.Streamers) != 1 || afterLoad.Streamers[0].Username != "alpha" {
		t.Fatalf("roster must survive a partial-body round-trip untouched: %+v", afterLoad.Streamers)
	}
	if afterLoad.RateLimits.CampaignSyncInterval != afterApply.RateLimits.CampaignSyncInterval {
		t.Fatal("untouched clamped fields must be stable across the round-trip")
	}
}

// TestSettingsPostFullBodyUnchangedBehavior: the Settings page's full-body
// POST keeps its exact semantics — everything posted is applied verbatim.
func TestSettingsPostFullBodyUnchangedBehavior(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := mergeTestServer(&applied)

	full := srv.settingsProvider.GetRuntimeSettings()
	full.Logger.ConsoleLevel = "debug"
	full.RateLimits.MinuteWatchedInterval = 60
	full.Streamers = []settings.StreamerConfig{{Username: "delta"}}
	body, err := json.Marshal(full)
	if err != nil {
		t.Fatal(err)
	}

	rec := postSettings(t, srv, string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !reflect.DeepEqual(applied[0], full) {
		t.Fatalf("full body must be applied verbatim:\nwant %+v\ngot  %+v", full, applied[0])
	}
}

// TestSettingsPostInvalidJSONRejected: malformed bodies are rejected before
// the apply callback runs.
func TestSettingsPostInvalidJSONRejected(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := mergeTestServer(&applied)

	rec := postSettings(t, srv, `{"logger":`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(applied) != 0 {
		t.Fatal("apply callback must not run on invalid JSON")
	}
}

// TestSettingsPostWithoutProviderUnavailable: with no settings provider there
// is no current state to merge onto, so the POST is refused instead of
// applying a zero-seeded (i.e. wiping) body.
func TestSettingsPostWithoutProviderUnavailable(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := &Server{onSettingsUpdate: func(rt settings.RuntimeSettings) {
		applied = append(applied, rt)
	}}

	rec := postSettings(t, srv, `{}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if len(applied) != 0 {
		t.Fatal("apply callback must not run without a provider")
	}
}
