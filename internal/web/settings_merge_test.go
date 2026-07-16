package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

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
