package settings

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
)

// TestRuntimeSettingsDTO_UsernameEditPreservesHiddenChannelID_C1C pins
// BKM-006 Corrective Pass 1 test matrix item C1-C: the RuntimeSettings DTO
// round trip (BuildRuntimeSettings -> operator edits the Username field only,
// as the Settings page's rename flow does -> ApplyToConfig) must preserve the
// persisted, hidden ChannelID untouched. Without this, every settings save —
// not just a rename — would silently erase the stored-identity anchor C1
// depends on for the NEXT cold restart's protection.
func TestRuntimeSettingsDTO_UsernameEditPreservesHiddenChannelID_C1C(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Streamers = []config.StreamerConfig{
		{Username: "oldlogin", ChannelID: "id-123"},
	}

	rt := BuildRuntimeSettings(&cfg)
	if len(rt.Streamers) != 1 || rt.Streamers[0].ChannelID != "id-123" {
		t.Fatalf("BuildRuntimeSettings did not carry ChannelID into the DTO: %+v", rt.Streamers)
	}

	// The operator edits ONLY the username field, exactly like the Settings
	// page's rename flow (gatherStreamers() never lets the user touch the
	// hidden channelId field).
	rt.Streamers[0].Username = "newlogin"

	var out config.Config
	ApplyToConfig(&out, rt)

	if len(out.Streamers) != 1 {
		t.Fatalf("ApplyToConfig produced %d streamer entries, want 1", len(out.Streamers))
	}
	if out.Streamers[0].Username != "newlogin" {
		t.Errorf("Username = %q, want newlogin", out.Streamers[0].Username)
	}
	if out.Streamers[0].ChannelID != "id-123" {
		t.Errorf("ChannelID = %q, want id-123 (must survive a Username-only edit round-trip)", out.Streamers[0].ChannelID)
	}
}

// TestRuntimeSettingsDTO_ChannelIDRoundTripsThroughStreamersFromDTO covers
// the lower-level conversion the miner's rename coordinator relies on
// directly (StreamersFromDTO — used to plan a rename BEFORE deciding how to
// persist the rest of the DTO): ChannelID must be carried through verbatim,
// same as ApplyToConfig.
func TestRuntimeSettingsDTO_ChannelIDRoundTripsThroughStreamersFromDTO(t *testing.T) {
	out := StreamersFromDTO([]StreamerConfig{
		{Username: "alpha", ChannelID: "id-alpha"},
		{Username: "beta"},
	})
	if len(out) != 2 {
		t.Fatalf("len(out) = %d, want 2", len(out))
	}
	if out[0].ChannelID != "id-alpha" {
		t.Errorf("out[0].ChannelID = %q, want id-alpha", out[0].ChannelID)
	}
	if out[1].ChannelID != "" {
		t.Errorf("out[1].ChannelID = %q, want empty (no channelId posted)", out[1].ChannelID)
	}
}

// TestBuildDefaultSettings_PreservesChannelID proves a "Reset settings"
// rebuild (BuildDefaultSettings, which intentionally drops per-streamer
// SETTINGS overrides) does not also drop the identity-metadata ChannelID —
// resetting customizations must not defeat C1's cold-restart protection.
func TestBuildDefaultSettings_PreservesChannelID(t *testing.T) {
	current := []config.StreamerConfig{{Username: "alpha", ChannelID: "id-alpha"}}
	rt := BuildDefaultSettings(current)
	if len(rt.Streamers) != 1 || rt.Streamers[0].ChannelID != "id-alpha" {
		t.Fatalf("BuildDefaultSettings dropped ChannelID: %+v", rt.Streamers)
	}
	if rt.Streamers[0].Settings != nil {
		t.Fatalf("BuildDefaultSettings must still drop per-streamer settings overrides: %+v", rt.Streamers[0].Settings)
	}
}
