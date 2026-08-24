package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/config"
	"github.com/KraineOpasen/bukerov-twitch-miner-go/internal/settings"
)

func TestSettingsAndSidebarDoNotExposeRetiredRotationTiming(t *testing.T) {
	settingsTemplate := readEmbeddedTemplate(t, "templates/settings.html")
	nowWatchingTemplate := readEmbeddedTemplate(t, "templates/partials/now_watching.html")

	for _, retired := range []string{
		"rotationIntervalMinMinutes",
		"rotationIntervalMaxMinutes",
		"set.ratelimits.rotation_min_",
		"set.ratelimits.rotation_max_",
	} {
		if strings.Contains(settingsTemplate, retired) {
			t.Errorf("settings UI still exposes retired rotation timing %q", retired)
		}
	}
	for _, retired := range []string{"HasNextRotation", "NextRotationUnix", "now.next_rotation", "data-countdown-to"} {
		if strings.Contains(nowWatchingTemplate, retired) {
			t.Errorf("sidebar still projects obsolete future rotation %q", retired)
		}
	}
}

func TestSettingsAPIDoesNotExposeRetiredRotationTiming(t *testing.T) {
	cfg := config.DefaultConfig()
	srv := &Server{settingsProvider: &fakeSettingsProvider{rt: settings.BuildRuntimeSettings(&cfg)}}
	rec := httptest.NewRecorder()
	srv.handleAPISettings(rec, httptest.NewRequest(http.MethodGet, "/api/settings", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/settings = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	for _, retired := range []string{
		`"rotationInterval"`,
		`"rotationIntervalMinMinutes"`,
		`"rotationIntervalMaxMinutes"`,
	} {
		if strings.Contains(rec.Body.String(), retired) {
			t.Errorf("settings API still exposes retired field %s: %s", retired, rec.Body.String())
		}
	}
}
