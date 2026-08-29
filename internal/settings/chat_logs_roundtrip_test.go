package settings

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

func c3Bool(value bool) *bool {
	return &value
}

func c3CloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	return c3Bool(*value)
}

func c3StateName(value *bool) string {
	if value == nil {
		return "inherit"
	}
	if *value {
		return "explicit_true"
	}
	return "explicit_false"
}

func c3AssertChatLogs(t *testing.T, got, want *bool) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("ChatLogs = %v, want nil/inherit", *got)
		}
		return
	}
	if got == nil {
		t.Fatalf("ChatLogs = nil/inherit, want explicit %v", *want)
	}
	if *got != *want {
		t.Fatalf("ChatLogs = %v, want %v", *got, *want)
	}
}

func c3AssertWireChatLogs(t *testing.T, object map[string]any, want *bool) {
	t.Helper()
	got, present := object["chatLogs"]
	if want == nil {
		if present {
			t.Fatalf("chatLogs wire field = %#v, want omission for nil/inherit", got)
		}
		return
	}
	if !present {
		t.Fatalf("chatLogs wire field omitted, want explicit %v", *want)
	}
	boolValue, ok := got.(bool)
	if !ok || boolValue != *want {
		t.Fatalf("chatLogs wire field = %#v, want boolean %v", got, *want)
	}
}

func TestChatLogsTriStateRoundTripThroughRuntimeSettingsJSON(t *testing.T) {
	tests := []struct {
		name  string
		state *bool
	}{
		{name: "inherit", state: nil},
		{name: "explicit_false", state: c3Bool(false)},
		{name: "explicit_true", state: c3Bool(true)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.StreamerSettings.ChatLogs = c3CloneBool(tc.state)
			custom := models.DefaultStreamerSettings()
			custom.ChatLogs = c3CloneBool(tc.state)
			cfg.Streamers = []config.StreamerConfig{{Username: "alpha", Settings: &custom}}

			body, err := json.Marshal(BuildRuntimeSettings(&cfg))
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}

			var wire struct {
				DefaultSettings map[string]any `json:"defaultSettings"`
				Streamers       []struct {
					Settings map[string]any `json:"settings"`
				} `json:"streamers"`
			}
			if err := json.Unmarshal(body, &wire); err != nil {
				t.Fatalf("decode wire shape: %v", err)
			}
			if len(wire.Streamers) != 1 {
				t.Fatalf("wire streamers = %d, want 1", len(wire.Streamers))
			}
			c3AssertWireChatLogs(t, wire.DefaultSettings, tc.state)
			c3AssertWireChatLogs(t, wire.Streamers[0].Settings, tc.state)

			var decoded RuntimeSettings
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("json.Unmarshal RuntimeSettings: %v", err)
			}
			out := config.DefaultConfig()
			ApplyToConfig(&out, decoded)
			c3AssertChatLogs(t, out.StreamerSettings.ChatLogs, tc.state)
			if len(out.Streamers) != 1 || out.Streamers[0].Settings == nil {
				t.Fatalf("round-trip streamer override missing: %+v", out.Streamers)
			}
			c3AssertChatLogs(t, out.Streamers[0].Settings.ChatLogs, tc.state)
		})
	}
}

func TestChatLogsDefaultsAndPerStreamerRemainDistinct(t *testing.T) {
	states := []*bool{nil, c3Bool(false), c3Bool(true)}
	for _, defaultState := range states {
		for _, streamerState := range states {
			name := "default_" + c3StateName(defaultState) + "/streamer_" + c3StateName(streamerState)
			t.Run(name, func(t *testing.T) {
				cfg := config.DefaultConfig()
				cfg.StreamerSettings.ChatLogs = c3CloneBool(defaultState)
				custom := models.DefaultStreamerSettings()
				custom.ChatLogs = c3CloneBool(streamerState)
				cfg.Streamers = []config.StreamerConfig{
					{Username: "whole-default"},
					{Username: "custom", Settings: &custom},
				}

				body, err := json.Marshal(BuildRuntimeSettings(&cfg))
				if err != nil {
					t.Fatalf("json.Marshal: %v", err)
				}
				var decoded RuntimeSettings
				if err := json.Unmarshal(body, &decoded); err != nil {
					t.Fatalf("json.Unmarshal: %v", err)
				}
				out := config.DefaultConfig()
				ApplyToConfig(&out, decoded)

				c3AssertChatLogs(t, out.StreamerSettings.ChatLogs, defaultState)
				if len(out.Streamers) != 2 {
					t.Fatalf("streamers = %d, want 2", len(out.Streamers))
				}
				if out.Streamers[0].Settings != nil {
					t.Fatalf("whole-default streamer gained an override: %+v", out.Streamers[0].Settings)
				}
				if out.Streamers[1].Settings == nil {
					t.Fatal("custom streamer lost its settings object")
				}
				c3AssertChatLogs(t, out.Streamers[1].Settings.ChatLogs, streamerState)
			})
		}
	}
}

func TestChatLogsUnrelatedEditPreservesTriState(t *testing.T) {
	for _, state := range []*bool{nil, c3Bool(false), c3Bool(true)} {
		name := "inherit"
		if state != nil {
			if *state {
				name = "explicit_true"
			} else {
				name = "explicit_false"
			}
		}
		t.Run(name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			custom := models.DefaultStreamerSettings()
			custom.ChatLogs = c3CloneBool(state)
			cfg.Streamers = []config.StreamerConfig{{Username: "alpha", Settings: &custom}}

			runtimeSettings := BuildRuntimeSettings(&cfg)
			followRaid := false
			runtimeSettings.Streamers[0].Settings.FollowRaid = &followRaid

			out := config.DefaultConfig()
			ApplyToConfig(&out, runtimeSettings)
			c3AssertChatLogs(t, out.Streamers[0].Settings.ChatLogs, state)
			if out.Streamers[0].Settings.FollowRaid {
				t.Fatal("unrelated FollowRaid edit was not applied")
			}
		})
	}
}

func TestChatLogsGlobalIndependence(t *testing.T) {
	tests := []struct {
		name   string
		global bool
		state  *bool
	}{
		{name: "global_true_inherit", global: true, state: nil},
		{name: "global_true_explicit_false", global: true, state: c3Bool(false)},
		{name: "global_true_explicit_true", global: true, state: c3Bool(true)},
		{name: "global_false_inherit", global: false, state: nil},
		{name: "global_false_explicit_false", global: false, state: c3Bool(false)},
		{name: "global_false_explicit_true", global: false, state: c3Bool(true)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Analytics.EnableChatLogs = tc.global
			custom := models.DefaultStreamerSettings()
			custom.ChatLogs = c3CloneBool(tc.state)
			cfg.Streamers = []config.StreamerConfig{{Username: "alpha", Settings: &custom}}

			body, err := json.Marshal(BuildRuntimeSettings(&cfg))
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var decoded RuntimeSettings
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			out := config.DefaultConfig()
			ApplyToConfig(&out, decoded)
			if out.Analytics.EnableChatLogs != tc.global {
				t.Fatalf("global EnableChatLogs = %v, want %v", out.Analytics.EnableChatLogs, tc.global)
			}
			c3AssertChatLogs(t, out.Streamers[0].Settings.ChatLogs, tc.state)
		})
	}
}

func TestChatLogsGlobalTransitionDoesNotSynthesizeOverride(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Analytics.EnableChatLogs = true
	cfg.StreamerSettings.ChatLogs = nil
	custom := models.DefaultStreamerSettings()
	custom.ChatLogs = nil
	cfg.Streamers = []config.StreamerConfig{{Username: "alpha", Settings: &custom}}

	for _, global := range []bool{false, true} {
		runtimeSettings := BuildRuntimeSettings(&cfg)
		runtimeSettings.Analytics.EnableChatLogs = global
		ApplyToConfig(&cfg, runtimeSettings)

		if cfg.Analytics.EnableChatLogs != global {
			t.Fatalf("global EnableChatLogs = %v, want %v", cfg.Analytics.EnableChatLogs, global)
		}
		c3AssertChatLogs(t, cfg.StreamerSettings.ChatLogs, nil)
		if len(cfg.Streamers) != 1 || cfg.Streamers[0].Settings == nil {
			t.Fatalf("streamer override missing after global transition: %+v", cfg.Streamers)
		}
		c3AssertChatLogs(t, cfg.Streamers[0].Settings.ChatLogs, nil)
	}
}

func TestChatLogsPersistsAcrossConfigFileRoundTrip(t *testing.T) {
	states := []*bool{nil, c3Bool(false), c3Bool(true)}
	for _, state := range states {
		t.Run(c3StateName(state), func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.Username = "tester"
			cfg.StreamerSettings.ChatLogs = c3CloneBool(state)
			custom := models.DefaultStreamerSettings()
			custom.ChatLogs = c3CloneBool(state)
			cfg.Streamers = []config.StreamerConfig{{Username: "alpha", Settings: &custom}}

			body, err := json.Marshal(BuildRuntimeSettings(&cfg))
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			var decoded RuntimeSettings
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			applied := config.DefaultConfig()
			applied.Username = cfg.Username
			ApplyToConfig(&applied, decoded)

			path := filepath.Join(t.TempDir(), "config.json")
			if err := config.SaveConfig(path, &applied); err != nil {
				t.Fatalf("SaveConfig: %v", err)
			}
			loaded, err := config.LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			c3AssertChatLogs(t, loaded.StreamerSettings.ChatLogs, state)
			if len(loaded.Streamers) != 1 || loaded.Streamers[0].Settings == nil {
				t.Fatalf("persisted streamer override missing: %+v", loaded.Streamers)
			}
			c3AssertChatLogs(t, loaded.Streamers[0].Settings.ChatLogs, state)
		})
	}
}

func TestChatLogsJSONNullMeansInherit(t *testing.T) {
	tests := []struct {
		name string
		body string
		want *bool
	}{
		{name: "null", body: `{"chatLogs":null}`, want: nil},
		{name: "false", body: `{"chatLogs":false}`, want: c3Bool(false)},
		{name: "true", body: `{"chatLogs":true}`, want: c3Bool(true)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var dto StreamerSettingsConfig
			if err := json.Unmarshal([]byte(tc.body), &dto); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}
			c3AssertChatLogs(t, StreamerSettingsFromDTO(dto).ChatLogs, tc.want)
		})
	}
}

func TestChatLogsConversionDoesNotAliasPointers(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.StreamerSettings.ChatLogs = c3Bool(true)
	runtimeSettings := BuildRuntimeSettings(&cfg)

	if err := json.Unmarshal([]byte(`{"defaultSettings":{"chatLogs":false}}`), &runtimeSettings); err != nil {
		t.Fatalf("decode false over DTO: %v", err)
	}
	c3AssertChatLogs(t, cfg.StreamerSettings.ChatLogs, c3Bool(true))

	out := config.DefaultConfig()
	ApplyToConfig(&out, runtimeSettings)
	c3AssertChatLogs(t, out.StreamerSettings.ChatLogs, c3Bool(false))

	if err := json.Unmarshal([]byte(`{"defaultSettings":{"chatLogs":true}}`), &runtimeSettings); err != nil {
		t.Fatalf("decode true over reused DTO: %v", err)
	}
	c3AssertChatLogs(t, out.StreamerSettings.ChatLogs, c3Bool(false))
}
