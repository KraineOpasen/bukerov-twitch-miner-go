package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/i18n"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

func c3WebBool(value bool) *bool {
	return &value
}

func c3WebCloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	return c3WebBool(*value)
}

func c3WebAssertChatLogs(t *testing.T, got, want *bool) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("ChatLogs = %v, want nil/inherit", *got)
		}
		return
	}
	if got == nil || *got != *want {
		t.Fatalf("ChatLogs = %v, want explicit %v", got, *want)
	}
}

func c3GetSettings(t *testing.T, srv *Server) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/settings", nil)
	srv.handleAPISettings(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings status = %d, want 200", rec.Code)
	}
	return rec.Body.Bytes()
}

func c3ConfigBackedSettingsServer(cfg *config.Config) *Server {
	return &Server{
		settingsProvider: &funcSettingsProvider{get: func() settings.RuntimeSettings {
			return settings.BuildRuntimeSettings(cfg)
		}},
		onSettingsUpdate: func(_ context.Context, rt settings.RuntimeSettings) error {
			settings.ApplyToConfig(cfg, rt)
			return nil
		},
	}
}

func TestChatLogsSettingsGETPOSTGETPreservesTriState(t *testing.T) {
	tests := []struct {
		name  string
		state *bool
	}{
		{name: "inherit", state: nil},
		{name: "explicit_false", state: c3WebBool(false)},
		{name: "explicit_true", state: c3WebBool(true)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.DiscoveryMode = config.DiscoveryModeAll
			cfg.StreamerSettings.ChatLogs = c3WebCloneBool(tc.state)
			custom := models.DefaultStreamerSettings()
			custom.ChatLogs = c3WebCloneBool(tc.state)
			cfg.Streamers = []config.StreamerConfig{{Username: "alpha", Settings: &custom}}
			srv := c3ConfigBackedSettingsServer(&cfg)

			first := c3GetSettings(t, srv)
			post := postSettings(t, srv, string(first))
			if post.Code != http.StatusOK {
				t.Fatalf("POST unchanged settings status = %d, want 200: %s", post.Code, post.Body.String())
			}
			second := c3GetSettings(t, srv)

			var firstJSON, secondJSON any
			if err := json.Unmarshal(first, &firstJSON); err != nil {
				t.Fatalf("decode first GET: %v", err)
			}
			if err := json.Unmarshal(second, &secondJSON); err != nil {
				t.Fatalf("decode second GET: %v", err)
			}
			if !reflect.DeepEqual(firstJSON, secondJSON) {
				t.Fatalf("GET/POST/GET representation changed:\nfirst:  %s\nsecond: %s", first, second)
			}

			c3WebAssertChatLogs(t, cfg.StreamerSettings.ChatLogs, tc.state)
			c3WebAssertChatLogs(t, cfg.Streamers[0].Settings.ChatLogs, tc.state)
		})
	}
}

func TestChatLogsSettingsUnrelatedPartialPOSTPreservesTriState(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.StreamerSettings.ChatLogs = c3WebBool(true)
	custom := models.DefaultStreamerSettings()
	custom.ChatLogs = c3WebBool(false)
	cfg.Streamers = []config.StreamerConfig{{Username: "alpha", Settings: &custom}}
	srv := c3ConfigBackedSettingsServer(&cfg)

	rec := postSettings(t, srv, `{"logger":{"consoleLevel":"debug"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST unrelated edit status = %d, want 200", rec.Code)
	}
	c3WebAssertChatLogs(t, cfg.StreamerSettings.ChatLogs, c3WebBool(true))
	c3WebAssertChatLogs(t, cfg.Streamers[0].Settings.ChatLogs, c3WebBool(false))
}

func TestChatLogsSettingsNullClearsSeededOverride(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.StreamerSettings.ChatLogs = c3WebBool(true)
	custom := models.DefaultStreamerSettings()
	custom.ChatLogs = c3WebBool(false)
	cfg.Streamers = []config.StreamerConfig{{Username: "alpha", Settings: &custom}}
	srv := c3ConfigBackedSettingsServer(&cfg)

	rec := postSettings(t, srv, `{"defaultSettings":{"chatLogs":null}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST default null status = %d, want 200", rec.Code)
	}
	c3WebAssertChatLogs(t, cfg.StreamerSettings.ChatLogs, nil)
	c3WebAssertChatLogs(t, cfg.Streamers[0].Settings.ChatLogs, c3WebBool(false))

	cfg.StreamerSettings.ChatLogs = c3WebBool(true)
	rec = postSettings(t, srv, `{"streamers":[{"username":"alpha","settings":{"chatLogs":null}}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST streamer null status = %d, want 200", rec.Code)
	}
	c3WebAssertChatLogs(t, cfg.StreamerSettings.ChatLogs, c3WebBool(true))
	c3WebAssertChatLogs(t, cfg.Streamers[0].Settings.ChatLogs, nil)
}

func TestChatLogsQuickActionMaterializationPreservesEffectiveTriState(t *testing.T) {
	tests := []struct {
		name  string
		state *bool
	}{
		{name: "inherit", state: nil},
		{name: "explicit_false", state: c3WebBool(false)},
		{name: "explicit_true", state: c3WebBool(true)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defaults := models.DefaultStreamerSettings()
			defaults.ChatLogs = c3WebCloneBool(tc.state)
			runtimeSettings := settings.RuntimeSettings{
				DefaultSettings: settings.StreamerSettingsToDTO(defaults),
				Streamers:       []settings.StreamerConfig{{Username: "alpha"}},
			}
			var applied settings.RuntimeSettings
			srv := &Server{
				settingsProvider: &funcSettingsProvider{get: func() settings.RuntimeSettings {
					return runtimeSettings
				}},
				onSettingsUpdate: func(_ context.Context, rt settings.RuntimeSettings) error {
					applied = rt
					return nil
				},
			}

			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/streamer-action/alpha", strings.NewReader(`{"action":"toggle-watch"}`))
			srv.handleAPIStreamerQuickAction(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("quick action status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if len(applied.Streamers) != 1 || applied.Streamers[0].Settings == nil {
				t.Fatalf("quick action did not materialize streamer settings: %+v", applied.Streamers)
			}
			c3WebAssertChatLogs(t, applied.Streamers[0].Settings.ChatLogs, tc.state)
			if applied.Streamers[0].Settings.DisableWatch == nil || !*applied.Streamers[0].Settings.DisableWatch {
				t.Fatal("quick action did not apply the requested DisableWatch change")
			}
		})
	}
}

func TestChatLogsSettingsUIHasDeterministicTriStateMapping(t *testing.T) {
	sourceBytes, err := templatesFS.ReadFile("templates/settings.html")
	if err != nil {
		t.Fatalf("read settings.html: %v", err)
	}
	source := string(sourceBytes)
	for _, want := range []string{
		`const chatLogsOptions = [`,
		`{ value: 'inherit', label: t('js.set.opt.chat_logs.inherit') }`,
		`{ value: 'enabled', label: t('js.set.opt.chat_logs.enabled') }`,
		`{ value: 'disabled', label: t('js.set.opt.chat_logs.disabled') }`,
		`data-field="chatLogs"`,
		`value === true ? 'enabled' : (value === false ? 'disabled' : 'inherit')`,
		`settings[field] = null`,
		`settings[field] = true`,
		`settings[field] = false`,
	} {
		if !strings.Contains(source, want) {
			t.Errorf("settings UI missing ChatLogs tri-state contract %q", want)
		}
	}
	if strings.Contains(source, `type="checkbox" data-field="chatLogs"`) {
		t.Fatal("ChatLogs uses a checkbox, which collapses inherit and explicit false")
	}

	loc, err := i18n.New()
	if err != nil {
		t.Fatalf("i18n.New: %v", err)
	}
	for _, lang := range []string{i18n.LangRU, i18n.LangEN} {
		for _, key := range []string{
			"js.set.form.chat_logs",
			"js.set.form.chat_logs.desc",
			"js.set.opt.chat_logs.inherit",
			"js.set.opt.chat_logs.enabled",
			"js.set.opt.chat_logs.disabled",
		} {
			if got := loc.T(lang, key); got == key || strings.TrimSpace(got) == "" {
				t.Errorf("lang %s missing localized %s, got %q", lang, key, got)
			}
		}
	}
}
