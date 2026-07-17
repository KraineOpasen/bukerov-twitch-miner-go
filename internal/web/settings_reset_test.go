package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

// resetSettingsProvider returns the REAL production default DTO from
// settings.BuildDefaultSettings, so the reset-handler test exercises the same
// path production does (Reset settings -> GetDefaultSettings -> ApplySettings),
// not a fake echo. GetRuntimeSettings is unused by the reset handler.
type resetSettingsProvider struct{ streamers []config.StreamerConfig }

func (p resetSettingsProvider) GetRuntimeSettings() settings.RuntimeSettings {
	return settings.RuntimeSettings{}
}

func (p resetSettingsProvider) GetDefaultSettings() settings.RuntimeSettings {
	return settings.BuildDefaultSettings(p.streamers)
}

// TestSettingsResetKeepsHealthGateEnabled is the reset-blocker regression: the
// "Reset settings" endpoint rebuilds the DTO from scratch (BuildDefaultSettings,
// not decode-onto-current), so the global risk block it hands to the apply
// callback — and echoes in its JSON response — must carry the default-ON health
// gate {0, 0, true}, never the Go zero value {0, 0, false} that would silently
// disable it in runtime and the saved config.
func TestSettingsResetKeepsHealthGateEnabled(t *testing.T) {
	var applied []settings.RuntimeSettings
	srv := &Server{
		settingsProvider: resetSettingsProvider{streamers: []config.StreamerConfig{
			{Username: "alice"}, {Username: "bob"},
		}},
		onSettingsUpdate: func(rt settings.RuntimeSettings) { applied = append(applied, rt) },
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/settings/reset", nil)
	srv.handleAPISettingsReset(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	// 1. The callback that persists + applies settings must receive the ON gate.
	if len(applied) != 1 {
		t.Fatalf("apply callback invoked %d times, want 1", len(applied))
	}
	pr := applied[0].PredictionRisk
	if pr.MaxStakePercent != 0 || pr.ReservePoints != 0 || !pr.HealthGateEnabled {
		t.Errorf("reset callback risk = %+v, want {MaxStakePercent:0 ReservePoints:0 HealthGateEnabled:true}", pr)
	}

	// 2. The JSON response body must carry the same block with the ON gate.
	var resp settings.RuntimeSettings
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode reset response: %v", err)
	}
	rr := resp.PredictionRisk
	if rr.MaxStakePercent != 0 || rr.ReservePoints != 0 || !rr.HealthGateEnabled {
		t.Errorf("reset response risk = %+v, want {0 0 true}", rr)
	}
	// Format-tolerant raw check that healthGateEnabled is serialized as true.
	if body := rec.Body.String(); !strings.Contains(strings.ReplaceAll(body, " ", ""), `"healthGateEnabled":true`) {
		t.Errorf("response body must contain healthGateEnabled:true, got: %s", body)
	}
}
