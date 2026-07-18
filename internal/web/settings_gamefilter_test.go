package web

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// gameFilterServer seeds a current-settings body carrying both game-filter
// fields, so the #89 (absent=keep / null=reset / value=set / []=clear) decode
// semantics can be exercised for each independently.
func gameFilterServer(applied *[]settings.RuntimeSettings) *Server {
	current := settings.RuntimeSettings{
		DropCampaignGameIDs: []string{"game-wot"},
		DropCampaignGames:   []string{"World of Tanks"},
	}
	return &Server{
		settingsProvider: &fakeSettingsProvider{rt: current},
		onSettingsUpdate: func(rt settings.RuntimeSettings) { *applied = append(*applied, rt) },
	}
}

// T17 (absent=keep): a body touching neither field keeps both current values.
func TestGameFilterSettingsAbsentKeepsBoth(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := gameFilterServer(&applied)

	if rec := postSettings(t, srv, `{"logger":{"consoleLevel":"debug"}}`); rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	got := applied[0]
	if !reflect.DeepEqual(got.DropCampaignGameIDs, []string{"game-wot"}) ||
		!reflect.DeepEqual(got.DropCampaignGames, []string{"World of Tanks"}) {
		t.Fatalf("absent keys must keep current: ids=%v names=%v", got.DropCampaignGameIDs, got.DropCampaignGames)
	}
}

// T17 (value=set) + T18 (changing one keeps the other): posting only the IDs
// field sets it and leaves the names field untouched.
func TestGameFilterSettingsSetOneKeepsOther(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := gameFilterServer(&applied)

	if rec := postSettings(t, srv, `{"dropCampaignGameIDs":["111","222"]}`); rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	got := applied[0]
	if !reflect.DeepEqual(got.DropCampaignGameIDs, []string{"111", "222"}) {
		t.Fatalf("posted IDs not applied: %v", got.DropCampaignGameIDs)
	}
	if !reflect.DeepEqual(got.DropCampaignGames, []string{"World of Tanks"}) {
		t.Fatalf("changing one field must not zero the other: names=%v", got.DropCampaignGames)
	}
}

// T17 (null=reset): explicit null resets only that field.
func TestGameFilterSettingsNullResets(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := gameFilterServer(&applied)

	if rec := postSettings(t, srv, `{"dropCampaignGameIDs":null}`); rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	got := applied[0]
	if got.DropCampaignGameIDs != nil {
		t.Fatalf("null must reset the IDs field to nil, got %v", got.DropCampaignGameIDs)
	}
	if !reflect.DeepEqual(got.DropCampaignGames, []string{"World of Tanks"}) {
		t.Fatalf("null on one field must not disturb the other: names=%v", got.DropCampaignGames)
	}
}

// T17 ([]=clear): an explicit empty list clears that field (distinct from absent).
func TestGameFilterSettingsEmptyListClears(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := gameFilterServer(&applied)

	if rec := postSettings(t, srv, `{"dropCampaignGames":[]}`); rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	got := applied[0]
	if len(got.DropCampaignGames) != 0 {
		t.Fatalf("explicit [] must clear the names field, got %v", got.DropCampaignGames)
	}
	if !reflect.DeepEqual(got.DropCampaignGameIDs, []string{"game-wot"}) {
		t.Fatalf("clearing one field must keep the other: ids=%v", got.DropCampaignGameIDs)
	}
}

// T17 (absent=keep, real wiring): a partial POST that omits both game-filter
// fields must keep the current config values — which only holds if
// BuildRuntimeSettings seeds them into GetRuntimeSettings (the decode-onto-
// current seed). Unlike the fakeSettingsProvider tests above, this drives the
// real GetRuntimeSettings -> handler merge -> ApplyToConfig pipeline, so it
// guards the seeding step itself (regression: dropping the seed silently wipes
// the field on every settings save).
func TestGameFilterAbsentKeepThroughBuildRuntimeSettings(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.DefaultConfig()
	cfg.Username = "tester"
	cfg.DropCampaignGameIDs = []string{"game-wot"}
	cfg.DropCampaignGames = []string{"World of Tanks"}

	srv := &Server{
		settingsProvider: &funcSettingsProvider{get: func() settings.RuntimeSettings {
			return settings.BuildRuntimeSettings(&cfg)
		}},
		onSettingsUpdate: func(rt settings.RuntimeSettings) {
			settings.ApplyToConfig(&cfg, rt)
			if err := config.SaveConfig(cfgPath, &cfg); err != nil {
				t.Fatalf("SaveConfig: %v", err)
			}
		},
	}

	if rec := postSettings(t, srv, `{"logger":{"consoleLevel":"debug"}}`); rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	if !reflect.DeepEqual(cfg.DropCampaignGameIDs, []string{"game-wot"}) ||
		!reflect.DeepEqual(cfg.DropCampaignGames, []string{"World of Tanks"}) {
		t.Fatalf("absent keys must survive through BuildRuntimeSettings: ids=%v names=%v",
			cfg.DropCampaignGameIDs, cfg.DropCampaignGames)
	}
}

// T16: both fields survive the full config file round-trip (BuildRuntimeSettings
// -> ApplyToConfig -> SaveConfig -> LoadConfig), with the ID kept case-exact.
func TestGameFilterConfigRoundTrip(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	cfg := config.DefaultConfig()
	cfg.Username = "tester"
	cfg.DropCampaignGameIDs = []string{"game-WoT-Exact"}
	cfg.DropCampaignGames = []string{"World of Tanks"}

	if err := config.SaveConfig(cfgPath, &cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	loaded, err := config.LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !reflect.DeepEqual(loaded.DropCampaignGameIDs, []string{"game-WoT-Exact"}) {
		t.Fatalf("game IDs must round-trip case-exact, got %v", loaded.DropCampaignGameIDs)
	}
	if !reflect.DeepEqual(loaded.DropCampaignGames, []string{"World of Tanks"}) {
		t.Fatalf("game names must round-trip, got %v", loaded.DropCampaignGames)
	}
}

// T22: the Settings template guards both textareas with `|| []`, so an
// absent/null value from the settings JSON renders as an empty field, never the
// literal string "null".
func TestGameFilterSettingsTemplateNullGuard(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("templates", "settings.html"))
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	html := string(src)
	for _, guard := range []string{
		"(settings.dropCampaignGameIDs || [])",
		"(settings.dropCampaignGames || [])",
	} {
		if !strings.Contains(html, guard) {
			t.Fatalf("settings template missing null-guard %q (empty field would render as \"null\")", guard)
		}
	}
}
