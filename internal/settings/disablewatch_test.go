package settings

import (
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/models"
)

// TestDisableWatchRoundTrip verifies the DisableWatch flag survives the
// model→DTO→model conversion the settings pipeline uses, so a card quick-action
// toggling it persists correctly.
func TestDisableWatchRoundTrip(t *testing.T) {
	src := models.DefaultStreamerSettings()
	src.DisableWatch = true

	dto := StreamerSettingsToDTO(src)
	if dto.DisableWatch == nil || !*dto.DisableWatch {
		t.Fatalf("DTO should carry DisableWatch=true, got %v", dto.DisableWatch)
	}

	back := StreamerSettingsFromDTO(dto)
	if !back.DisableWatch {
		t.Fatal("round-tripped settings should keep DisableWatch=true")
	}
}

// TestApplyPartialDisableWatch verifies a partial DTO (only DisableWatch set)
// applies onto existing settings without disturbing other fields - the shape a
// quick-action produces.
func TestApplyPartialDisableWatch(t *testing.T) {
	dst := models.DefaultStreamerSettings()
	dst.MakePredictions = true

	on := true
	ApplyStreamerSettingsFromDTO(&dst, StreamerSettingsConfig{DisableWatch: &on})

	if !dst.DisableWatch {
		t.Error("DisableWatch should be applied")
	}
	if !dst.MakePredictions {
		t.Error("unrelated field MakePredictions should be preserved")
	}
}
